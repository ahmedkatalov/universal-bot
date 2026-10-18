package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/ledger"

	"go.mau.fi/whatsmeow/types"
)

// explainCollectionTool — «из чего сложен сбор»: по каждому платежу видно,
// какими чеками/текстами он подтверждён, по каждой отброшенной записи — почему
// она не в сборе, и что выглядит подозрительно (возможный двойной счёт).
func (b *Bot) explainCollectionTool() ai.Tool {
	return ai.Tool{
		Name: "explain_collection",
		Description: "Показывает, ИЗ ЧЕГО сложен сбор за период: каждый платёж с его чеками/текстами, что не вошло и " +
			"почему, и подозрительное (возможный двойной счёт, перевод без чека). Вызывай, когда владелец говорит, что " +
			"сбор не сходится, «почему у Ахмеда 30000», «проверь подсчёт», «разбери сбор за вчера». client — если речь " +
			"про одного клиента (покажу подробно).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "YYYY-MM-DD (пусто = сегодня)"},
				"to_date":   map[string]any{"type": "string", "description": "YYYY-MM-DD (пусто = как from_date)"},
				"group":     map[string]any{"type": "string", "description": "Группа (пусто = все)"},
				"client":    map[string]any{"type": "string", "description": "Клиент (пусто = все)"},
				"date_basis": map[string]any{
					"type": "string",
					"enum": []string{"operation", "chat"},
					"description": "Как в send_finance_report: \"operation\" (по умолчанию) — по дате операции на чеке, " +
						"\"chat\" — по дате, когда прислали в чат. Разбирая сбор «по дате в чате», передай chat.",
				},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate, ToDate, Group, Client string
			}
			var raw map[string]string
			_ = json.Unmarshal(input, &raw)
			args.FromDate, args.ToDate, args.Group, args.Client = raw["from_date"], raw["to_date"], raw["group"], raw["client"]
			byChat := raw["date_basis"] == "chat"
			now := time.Now()
			from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
			if t, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local); err == nil {
				from = t
			}
			to := from.AddDate(0, 0, 1)
			if t, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local); err == nil {
				to = t.AddDate(0, 0, 1)
			}
			groupJIDs, groupLabel, err := b.resolveGroups(ctx, args.Group)
			if err != nil {
				return "", err
			}
			book, err := b.db.Book(ctx, from, to, "")
			if err != nil {
				return "", err
			}
			var gs map[string]bool
			for _, g := range groupJIDs {
				if gs == nil {
					gs = map[string]bool{}
				}
				gs[g] = true
			}
			f := ledger.Filter{From: from, To: to, Groups: gs}
			when := func(p *ledger.Payment) time.Time { return p.OpDate }
			if byChat {
				last := to.AddDate(0, 0, -1)
				f.ByChat = true
				f.MonthStart = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location())
				f.MonthEnd = time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, last.Location()).AddDate(0, 1, 0)
				when = func(p *ledger.Payment) time.Time { return p.PostedAt }
			}
			pays := book.Counted(f)
			left := book.Left(f)
			q := strings.ToLower(strings.TrimSpace(args.Client))
			match := func(name string) bool { return q == "" || strings.Contains(strings.ToLower(name), q) }
			groups := b.joinedGroups(ctx)
			gname := func(g string) string {
				if jid, err := types.ParseJID(g); err == nil && groups[jid] != "" {
					return groups[jid]
				}
				return g
			}
			sort.SliceStable(pays, func(i, j int) bool { return when(pays[i]).Before(when(pays[j])) })

			var sb strings.Builder
			period := from.Format("02.01.2006")
			if last := to.AddDate(0, 0, -1); !last.Equal(from) {
				period += " — " + last.Format("02.01.2006")
			}
			var total float64
			n := 0
			var lines []string
			for _, p := range pays {
				if !match(p.Client) {
					continue
				}
				total += p.Amount
				n++
				lines = append(lines, "• "+describePayment(p, gname))
			}
			basis := ""
			if byChat {
				basis = ", по дате в чате"
			}
			fmt.Fprintf(&sb, "Сбор за %s (%s%s): %s ₽, платежей %d.\n", period, groupLabel, basis, formatMoney(total), n)
			if q != "" || len(lines) <= 40 {
				sb.WriteString(strings.Join(lines, "\n"))
			} else {
				sb.WriteString(strings.Join(lines[:40], "\n"))
				fmt.Fprintf(&sb, "\n…и ещё %d (укажи клиента, чтобы увидеть подробно)", len(lines)-40)
			}

			// По дате чата: чеки с датой операции из другого месяца — отдельно.
			if old := book.OldChecks(f); len(old) > 0 {
				var ol []string
				var ot float64
				for _, p := range old {
					if !match(p.Client) {
						continue
					}
					ot += p.Amount
					ol = append(ol, "• "+describePayment(p, gname))
				}
				if len(ol) > 0 {
					fmt.Fprintf(&sb, "\n\nСтарые чеки (прислали в период, операция другого месяца — НЕ в сумме): %s ₽\n%s",
						formatMoney(ot), strings.Join(ol, "\n"))
				}
			}

			// Не вошло — с причиной.
			var out []string
			for _, e := range left {
				if !match(e.Client()) {
					continue
				}
				what := "текст"
				who := ""
				if e.Check != nil {
					what = "чек"
					who = e.Check.Sender
				} else {
					if e.Text.Cash {
						what = "наличка"
					}
					who = e.Text.Sender
				}
				line := fmt.Sprintf("• [%s] %s ₽ — %s, прислал %s, %s, группа «%s» — %s", what, formatMoney(e.Amount()),
					orDash(e.Client()), orDash(who), e.PostedAt().In(time.Local).Format("02.01 15:04"), gname(e.Group()), e.Reason)
				switch {
				case len(e.Parts) > 0:
					var ps []string
					for _, p := range e.Parts {
						ps = append(ps, formatMoney(p.Amount))
					}
					line += " (считаются чеки " + strings.Join(ps, " + ") + " ₽)"
				case e.Into != nil:
					line += " (считается как чек " + formatMoney(e.Into.Amount) + " ₽)"
				}
				out = append(out, line)
			}
			if len(out) > 0 {
				sb.WriteString("\n\nНЕ вошло в сбор:\n")
				if len(out) > 30 {
					out = append(out[:30], fmt.Sprintf("…и ещё %d", len(out)-30))
				}
				sb.WriteString(strings.Join(out, "\n"))
			}

			// Подозрительное.
			if sus := suspicious(pays, match); len(sus) > 0 {
				sb.WriteString("\n\n⚠️ Проверь:\n" + strings.Join(sus, "\n"))
			}
			return sb.String(), nil
		},
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// describePayment — платёж и его подтверждения одной строкой.
func describePayment(p *ledger.Payment, gname func(string) string) string {
	kind := "перевод"
	switch {
	case p.Main() != nil:
		kind = "чек"
	case p.Cash:
		kind = "наличка"
	}
	sender, _ := p.Sender()
	s := fmt.Sprintf("%s — %s ₽ [%s] %s, прислал %s, группа «%s»", orDash(p.Client), formatMoney(p.Amount), kind,
		p.OpDate.In(time.Local).Format("02.01 15:04"), orDash(sender), gname(p.HomeGroup()))
	var extra []string
	if len(p.Checks) > 1 {
		var gs []string
		for _, c := range p.Checks[1:] {
			gs = append(gs, "«"+gname(c.Group)+"»")
		}
		extra = append(extra, "тот же чек ещё в "+strings.Join(gs, ", "))
	}
	if p.Main() != nil && len(p.Texts) > 0 {
		extra = append(extra, "есть и текстом (не считается второй раз)")
	}
	for _, c := range p.Checks {
		if c.Review {
			extra = append(extra, "в «"+gname(c.Group)+"» тот же чек ждёт «чей чек?»")
		}
	}
	if len(extra) > 0 {
		s += " — " + strings.Join(extra, "; ")
	}
	return s
}

// suspicious — то, что похоже на ошибку подсчёта: два одинаковых платежа
// клиента в один день (не одно ли это?), крупный перевод текстом без чека.
func suspicious(pays []*ledger.Payment, match func(string) bool) []string {
	var out []string
	type key struct {
		client string
		amount int64
		day    string
	}
	seen := map[key][]*ledger.Payment{}
	for _, p := range pays {
		if !match(p.Client) {
			continue
		}
		k := key{ledger.ClientKey(p.Client), int64(math.Round(p.Amount)), p.OpDate.In(time.Local).Format("2006-01-02")}
		seen[k] = append(seen[k], p)
	}
	for k, ps := range seen {
		if len(ps) < 2 {
			continue
		}
		var kinds []string
		for _, p := range ps {
			switch {
			case p.Main() != nil:
				kinds = append(kinds, "чек")
			case p.Cash:
				kinds = append(kinds, "наличка")
			default:
				kinds = append(kinds, "текст")
			}
		}
		out = append(out, fmt.Sprintf("• %s: %d платежа по %s ₽ за %s (%s) — если это один платёж, исключи лишний",
			ps[0].Client, len(ps), formatMoney(float64(k.amount)), ps[0].OpDate.In(time.Local).Format("02.01"), strings.Join(kinds, ", ")))
	}
	sort.Strings(out)
	return out
}
