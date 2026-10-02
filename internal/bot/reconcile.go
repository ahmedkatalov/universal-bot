// Сверка учёта с программой рассрочек «как человек»: по каждому клиенту берём
// ВСЮ картину (его чеки и наличку вокруг периода из всех групп, все его
// рассрочки и оплаты в программе) и объясняем каждый чек периода.
package bot

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

// reconContextDays — сколько дней вокруг периода смотрим, чтобы оплаты соседних
// чеков (сентябрьский чек, внесённый 2 октября) не засчитали чужой чек.
const reconContextDays = 45

// reconRec — чек после склейки копий из разных групп.
type reconRec struct {
	db.ReconReceipt
	groups map[string]bool
}

// dedupeReconReceipts склеивает копии ОДНОГО чека: пересылку «<id>-fwd-<группа>»
// с оригиналом, и одинаковые чеки (тот же номер документа и сумма; без номера —
// то же имя, сумма и минута операции). Иначе при сверке по всем группам один
// пересланный чек давал две позиции, и одна из них всегда была бы «НЕ внесён».
func dedupeReconReceipts(rs []db.ReconReceipt) []reconRec {
	var out []reconRec
	byWa := map[string]int{}
	byKey := map[string]int{}
	keyOf := func(r db.ReconReceipt) string {
		cents := strconv.FormatInt(int64(math.Round(r.Amount*100)), 10)
		if r.DocNumber != "" {
			return "doc|" + r.DocNumber + "|" + cents
		}
		return "t|" + normCyr(r.Name) + "|" + cents + "|" + r.TxDate.UTC().Truncate(time.Minute).Format(time.RFC3339)
	}
	merge := func(k int, r db.ReconReceipt) {
		out[k].groups[r.GroupJID] = true
		if out[k].NeedsReview && !r.NeedsReview { // предпочитаем копию с подтверждённым клиентом
			g := out[k].groups
			out[k].ReconReceipt = r
			out[k].groups = g
		}
	}
	add := func(r db.ReconReceipt) {
		key := keyOf(r)
		if k, ok := byKey[key]; ok {
			merge(k, r)
			if r.WaMessageID != "" {
				byWa[r.WaMessageID] = k
			}
			return
		}
		out = append(out, reconRec{ReconReceipt: r, groups: map[string]bool{r.GroupJID: true}})
		k := len(out) - 1
		byKey[key] = k
		if r.WaMessageID != "" {
			byWa[r.WaMessageID] = k
		}
	}
	var fwd []db.ReconReceipt
	for _, r := range rs {
		if strings.Contains(r.WaMessageID, "-fwd-") {
			fwd = append(fwd, r)
			continue
		}
		add(r)
	}
	for _, r := range fwd {
		orig := r.WaMessageID[:strings.Index(r.WaMessageID, "-fwd-")]
		if k, ok := byWa[orig]; ok {
			merge(k, r)
			continue
		}
		add(r)
	}
	return out
}

// sameClientName — одно ли это имя клиента (разное написание/порядок слов/
// склонение/отчество). Однословные имена — только точное совпадение, чтобы
// «Ахмед» не прилип ко всем Ахмедам.
func sameClientName(a, b string) bool {
	na, nb := normCyr(a), normCyr(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	wa, wb := normWords(a), normWords(b)
	mn := len(wa)
	if len(wb) < mn {
		mn = len(wb)
	}
	if mn < 2 {
		return false
	}
	return scoreCandidate(wa, wb) >= mn
}

type reconBucket struct {
	display string
	items   []reconItem // позиции периода (Report=true)
}

// cmfReconcile — живая сверка учёта за период [from, to) с программой.
func (b *Bot) cmfReconcile(ctx context.Context, from, to time.Time, groupJID string) (string, error) {
	ctxFrom := from.AddDate(0, 0, -reconContextDays)
	ctxTo := to.AddDate(0, 0, reconContextDays)

	rawRecs, err := b.db.ReceiptsForReconcile(ctx, ctxFrom, ctxTo)
	if err != nil {
		return "", err
	}
	cash, err := b.db.CashForReconcile(ctx, ctxFrom, ctxTo)
	if err != nil {
		return "", err
	}
	recs := dedupeReconReceipts(rawRecs)

	inPeriod := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	inGroup := func(groups map[string]bool) bool { return groupJID == "" || groups[groupJID] }

	periodLabel := from.Format("02.01.2006")
	if last := to.AddDate(0, 0, -1); last.Format("2006-01-02") != from.Format("2006-01-02") {
		periodLabel += " — " + last.Format("02.01.2006")
	}

	// Позиции ПЕРИОДА — по клиентам (нормализованное имя). Неподтверждённые чеки
	// (клиента не назвали — «имя» там владельца карты) в сверку не берём, считаем
	// отдельно: сначала надо понять, чей чек.
	order := []string{}
	buckets := map[string]*reconBucket{}
	addReported := func(name string, it reconItem) {
		key := normCyr(name)
		bk := buckets[key]
		if bk == nil {
			bk = &reconBucket{display: strings.TrimSpace(name)}
			buckets[key] = bk
			order = append(order, key)
		}
		bk.items = append(bk.items, it)
	}
	unconfirmed, unconfirmedSum := 0, 0.0
	nChecks, nCash := 0, 0
	for _, r := range recs {
		if !inPeriod(r.TxDate) || !inGroup(r.groups) {
			continue
		}
		if r.NeedsReview || strings.TrimSpace(r.Name) == "" {
			unconfirmed++
			unconfirmedSum += r.Amount
			continue
		}
		nChecks++
		addReported(r.Name, reconItem{ID: r.ID, Kind: "check", Amount: r.Amount, Date: r.TxDate, Report: true})
	}
	for _, c := range cash {
		if !inPeriod(c.TxDate) || (groupJID != "" && c.GroupJID != groupJID) {
			continue
		}
		nCash++
		addReported(c.Name, reconItem{ID: -c.ID, Kind: "cash", Amount: c.Amount, Date: c.TxDate, Report: true})
	}
	if len(order) == 0 && unconfirmed == 0 {
		return "За " + periodLabel + " чеков и налички в учёте нет.", nil
	}

	// Имя → клиент программы; один клиент под разными написаниями сводится по ID.
	type clientGroup struct {
		client  cmf.ClientInfo
		names   []string
		items   []reconItem
	}
	var attention []string
	cidOrder := []string{}
	groupsByCID := map[string]*clientGroup{}
	pinged := false
	for _, key := range order {
		bk := buckets[key]
		clients, kind, err := b.cmfLookupWithTypos(ctx, bk.display)
		switch {
		case err != nil:
			fmt.Printf("cmf: ошибка поиска клиента %q: %v\n", bk.display, err)
			if !pinged {
				pinged = true
				if perr := b.cmf.Ping(ctx); perr != nil {
					fmt.Printf("cmf: программа недоступна: %v\n", perr)
					var sum float64
					for _, k := range order {
						for _, it := range buckets[k].items {
							sum += it.Amount
						}
					}
					return fmt.Sprintf("⚠️ Сверку не сделал — %s. В учёте за %s позиций: %d на %s ₽; сверю, как только программа ответит.",
						cmf.Human(perr), periodLabel, nChecks+nCash, formatRub(sum)), nil
				}
			}
			attention = append(attention, fmt.Sprintf("%s — %s (%s)", bk.display, cmf.Human(err), itemsBrief(bk.items)))
			continue
		case kind == cmfNoMatch:
			attention = append(attention, fmt.Sprintf("%s — в программе не найден (%s)", bk.display, itemsBrief(bk.items)))
			continue
		case kind == cmfExact || kind == cmfStrong:
		case len(clients) == 1:
			attention = append(attention, fmt.Sprintf("%s — точного совпадения нет, похоже на «%s», уточни (%s)", bk.display, clients[0].FullName, itemsBrief(bk.items)))
			continue
		default:
			var names []string
			for _, c := range clients {
				names = append(names, c.FullName)
			}
			attention = append(attention, fmt.Sprintf("%s — в программе несколько клиентов (%s), уточни (%s)", bk.display, strings.Join(names, ", "), itemsBrief(bk.items)))
			continue
		}
		c := clients[0]
		g := groupsByCID[c.ID]
		if g == nil {
			g = &clientGroup{client: c}
			groupsByCID[c.ID] = g
			cidOrder = append(cidOrder, c.ID)
		}
		g.names = append(g.names, bk.display)
		g.items = append(g.items, bk.items...)
	}

	var blocks []string
	okClients, entered, notEntered, suspicious := 0, 0, 0, 0
	for _, cid := range cidOrder {
		g := groupsByCID[cid]

		// Контекст: остальные чеки/наличка ЭТОГО клиента вокруг периода и из других
		// групп — они «забирают» свои оплаты, в ответ не выводятся.
		isHis := func(name string) bool {
			if sameClientName(name, g.client.FullName) {
				return true
			}
			for _, n := range g.names {
				if sameClientName(name, n) {
					return true
				}
			}
			return false
		}
		reported := map[int]bool{}
		for _, it := range g.items {
			reported[it.ID] = true
		}
		items := append([]reconItem(nil), g.items...)
		for _, r := range recs {
			if r.NeedsReview || reported[r.ID] || !isHis(r.Name) {
				continue
			}
			items = append(items, reconItem{ID: r.ID, Kind: "check", Amount: r.Amount, Date: r.TxDate})
		}
		for _, c := range cash {
			if reported[-c.ID] || !isHis(c.Name) {
				continue
			}
			items = append(items, reconItem{ID: -c.ID, Kind: "cash", Amount: c.Amount, Date: c.TxDate})
		}

		pays, perr := b.cmf.PaymentsBetween(ctx, cid, ctxFrom.Add(-reconLateBack), ctxTo.Add(reconWinFwd))
		if perr != nil {
			fmt.Printf("cmf: ошибка платежей клиента %q: %v\n", g.client.FullName, perr)
			attention = append(attention, fmt.Sprintf("%s — не смог получить его оплаты: %s (%s)", g.client.FullName, cmf.Human(perr), itemsBrief(g.items)))
			continue
		}
		verdicts, leftover, kopecks := humanMatch(items, pays)

		// Показываем только позиции периода, по дате.
		var idx []int
		for i, it := range items {
			if it.Report {
				idx = append(idx, i)
			}
		}
		sort.SliceStable(idx, func(a, b int) bool { return items[idx[a]].Date.Before(items[idx[b]].Date) })

		allOK := true
		var lines []string
		for _, i := range idx {
			v := verdicts[i]
			switch {
			case v.entered():
				entered++
			case v.Status == stSuspicious:
				suspicious++
				allOK = false
			default:
				notEntered++
				allOK = false
			}
			lines = append(lines, "  "+describeVerdict(items, pays, verdicts, i, kopecks))
		}
		// Оплаты в программе без пары в учёте — только относящиеся к периоду.
		var extra []string
		for _, j := range leftover {
			p := pays[j]
			if p.PaidAt.IsZero() || p.PaidAt.Before(from.Add(-reconWinBack)) || !p.PaidAt.Before(to.Add(reconWinFwd)) {
				continue
			}
			extra = append(extra, fmt.Sprintf("%s ₽ от %s%s", formatRub(payRub(p, kopecks)), p.PaidAt.Format("02.01"), contractLabel(p)))
		}
		if allOK {
			okClients++
			continue
		}
		blk := g.client.FullName + ":\n" + strings.Join(lines, "\n")
		if len(extra) > 0 {
			blk += "\n  ↳ в программе есть оплата без чека в учёте: " + strings.Join(extra, "; ")
		}
		blocks = append(blocks, blk)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Сверка с программой за %s: чеков %d", periodLabel, nChecks)
	if nCash > 0 {
		fmt.Fprintf(&sb, ", наличных %d", nCash)
	}
	sb.WriteString(".\n")
	if len(blocks) > 0 {
		sb.WriteString("\n" + strings.Join(blocks, "\n\n") + "\n")
	}
	if len(attention) > 0 {
		fmt.Fprintf(&sb, "\n⚠️ Уточнить (%d):\n• %s\n", len(attention), strings.Join(attention, "\n• "))
	}
	if unconfirmed > 0 {
		fmt.Fprintf(&sb, "\nЕщё %d чек(ов) на %s ₽ без подтверждённого клиента — не знаю, чьи; ответь на «чей чек», тогда сверю.\n", unconfirmed, formatRub(unconfirmedSum))
	}
	if okClients > 0 {
		fmt.Fprintf(&sb, "\n✅ У остальных %d клиент(ов) всё внесено.", okClients)
	}
	fmt.Fprintf(&sb, "\nИтого: внесено %d, НЕ внесено %d", entered, notEntered)
	if suspicious > 0 {
		fmt.Fprintf(&sb, ", проверить сумму %d", suspicious)
	}
	sb.WriteString(".")
	return sb.String(), nil
}

// payRub — сумма оплаты программы в рублях.
func payRub(p cmf.Payment, kopecks bool) float64 {
	if kopecks {
		return float64(p.Amount) / 100
	}
	return float64(p.Amount)
}

// contractLabel — « (дог. №142)», если программа дала номер договора.
func contractLabel(p cmf.Payment) string {
	if p.ContractNumber > 0 {
		return fmt.Sprintf(" (дог. №%d)", p.ContractNumber)
	}
	return ""
}

func itemWord(it reconItem) string {
	if it.Kind == "cash" {
		return "наличка"
	}
	return "чек"
}

// itemsBrief — коротко позиции клиента для строки «уточнить».
func itemsBrief(items []reconItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s %s · %s ₽", itemWord(it), it.Date.Format("02.01"), formatRub(it.Amount)))
	}
	return strings.Join(parts, ", ")
}

// describeVerdict — строка-объяснение по позиции, как сказал бы бухгалтер.
func describeVerdict(items []reconItem, pays []cmf.Payment, vs []reconVerdict, i int, kopecks bool) string {
	it, v := items[i], vs[i]
	head := fmt.Sprintf("%s · %s ₽", it.Date.Format("02.01"), formatRub(it.Amount))
	if it.Kind == "cash" {
		head += " наличка"
	}
	payAt := func(j int) string {
		p := pays[j]
		if p.PaidAt.IsZero() {
			return "оплатой (без даты)" + contractLabel(p)
		}
		return "оплатой " + p.PaidAt.Format("02.01") + contractLabel(p)
	}
	switch v.Status {
	case stEntered:
		return "✅ " + head + " — внесён " + payAt(v.Pays[0])
	case stCombined:
		var others []string
		for _, k := range v.With {
			others = append(others, itemWord(items[k])+" "+items[k].Date.Format("02.01"))
		}
		p := pays[v.Pays[0]]
		return fmt.Sprintf("✅ %s — внесён одной оплатой %s ₽ от %s%s вместе с: %s", head,
			formatRub(payRub(p, kopecks)), p.PaidAt.Format("02.01"), contractLabel(p), strings.Join(others, ", "))
	case stSplit:
		var parts []string
		for _, j := range v.Pays {
			parts = append(parts, fmt.Sprintf("%s ₽ (%s)", formatRub(payRub(pays[j], kopecks)), pays[j].PaidAt.Format("02.01")))
		}
		return "✅ " + head + " — внесён частями: " + strings.Join(parts, " + ") + contractLabel(pays[v.Pays[0]])
	case stLate:
		p := pays[v.Pays[0]]
		days := int(math.Round(p.PaidAt.Sub(it.Date).Hours() / 24))
		if days >= 0 {
			return fmt.Sprintf("✅ %s — внесён поздно: %s (через %d дн.)", head, payAt(v.Pays[0]), days)
		}
		return fmt.Sprintf("✅ %s — внесён, но с датой раньше чека: %s (на %d дн.) — проверь дату", head, payAt(v.Pays[0]), -days)
	case stSuspicious:
		p := pays[v.Pays[0]]
		return fmt.Sprintf("⚠️ %s — в программе оплата %s ₽ от %s%s: %s — проверь сумму", head,
			formatRub(payRub(p, kopecks)), p.PaidAt.Format("02.01"), contractLabel(p), v.Note)
	default:
		s := "❌ " + head + " — НЕ внесён"
		if it.Kind == "cash" {
			s = "❌ " + head + " — НЕ внесена"
		}
		if k := v.SameAmountAs; k >= 0 {
			s += fmt.Sprintf(" (сумма как у %s %s — внесена одна из двух оплат)", itemWord(items[k]), items[k].Date.Format("02.01"))
		}
		return s
	}
}
