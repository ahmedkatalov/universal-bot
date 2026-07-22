// Инструмент записи платежа со слов владельца: когда он диктует/пересылает
// платёж боту (в личку или обращаясь к нему), а не бот сам поймал его из
// группы. Без этого ассистент мог только «говорить», что записал, но реально
// ничего не сохранял — отсюда бесконечные переспросы и «пересчёт даёт то же».
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/report"
)

const settingWorkingGroup = "working_group"

// recordPaymentTool записывает наличный/безналичный платёж, продиктованный
// владельцем, прямо в учёт (в рабочую группу).
func (b *Bot) recordPaymentTool() ai.Tool {
	return ai.Tool{
		Name: "record_payment",
		Description: "ЗАПИСАТЬ платёж в учёт со слов владельца — когда он диктует или пересылает платёж, а не бот сам " +
			"поймал его из группы. Вызывай на сообщения-платежи владельца: 'Джабраилов Мовсар оплатил наличными 22400', " +
			"'Асхабов Ибрагим наличка 35000 отдал Адаму', 'Умхадижиев Рахман 170т', 'запиши перевод Милана 25000'. " +
			"Если владелец прислал СРАЗУ НЕСКОЛЬКО платежей — вызови инструмент по КАЖДОМУ. client и amount обязательны. " +
			"kind: 'cash' если явно наличка/наличными/нал, иначе 'transfer'. collector — кто ЗАБРАЛ наличку (ответственный, " +
			"'отдал Адаму'/'Мансур взял'). group — в какую группу; если не указана, берётся ЗАПОМНЕННАЯ рабочая группа. " +
			"НЕ переспрашивай группу на каждый платёж — спроси один раз, запомни и применяй ко всем. date — YYYY-MM-DD, " +
			"по умолчанию сегодня.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client":    map[string]any{"type": "string", "description": "ФИО клиента (кто оплатил)"},
				"amount":    map[string]any{"type": "number", "description": "Сумма в рублях"},
				"kind":      map[string]any{"type": "string", "enum": []string{"cash", "transfer"}, "description": "cash — наличка; transfer — перевод/чек"},
				"group":     map[string]any{"type": "string", "description": "Группа учёта (пусто — рабочая группа из памяти)"},
				"collector": map[string]any{"type": "string", "description": "Кто забрал наличку (ответственный), если указан"},
				"date":      map[string]any{"type": "string", "description": "Дата платежа YYYY-MM-DD (по умолчанию сегодня)"},
			},
			"required": []string{"client", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Client    string  `json:"client"`
				Amount    float64 `json:"amount"`
				Kind      string  `json:"kind"`
				Group     string  `json:"group"`
				Collector string  `json:"collector"`
				Date      string  `json:"date"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			client := strings.TrimSpace(args.Client)
			if client == "" || args.Amount <= 0 {
				return "", fmt.Errorf("нужны и клиент, и сумма (>0)")
			}

			// Группа: указана -> используем и ЗАПОМИНАЕМ как рабочую; иначе берём
			// запомненную. Если нет ни там, ни там — просим назвать один раз.
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
				_ = b.db.SettingSet(ctx, settingWorkingGroup, groupJID)
			} else {
				groupJID, _ = b.db.SettingGet(ctx, settingWorkingGroup)
				if groupJID == "" {
					return "Не знаю, в какую группу записывать платежи. Назови рабочую группу один раз " +
						"(например: «работаем с Оплата КЛНТ»), дальше буду использовать её автоматически.", nil
				}
			}

			txDate := time.Now()
			if s := strings.TrimSpace(args.Date); s != "" {
				if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
					txDate = t
				}
			}

			isCash := strings.EqualFold(args.Kind, "cash")
			collector := strings.TrimSpace(args.Collector)
			senderName := collector
			if senderName == "" {
				senderName = "владелец"
			}

			// Синтетическое сырое сообщение, чтобы платёж лёг в нужную группу и
			// «кто собрал» засчитался ответственному (по sender_name).
			waMsgID := fmt.Sprintf("dictated-%d", time.Now().UnixNano())
			body := fmt.Sprintf("%s %.0f", client, args.Amount)
			if isCash {
				body += " наличка"
			}
			if collector != "" {
				body += " (собрал " + collector + ")"
			}
			rawID, err := b.db.SaveRawMessage(ctx, waMsgID, groupJID, "dictated@c.us", senderName, body, false, "", txDate)
			if err != nil {
				return "", fmt.Errorf("не удалось сохранить: %w", err)
			}

			canonical := client
			if resolved, ok := b.aliases.ResolveName(client); ok {
				canonical = resolved
			}
			contactID, err := b.db.GetOrCreateContact(ctx, canonical)
			if err != nil {
				return "", fmt.Errorf("не удалось создать контакт: %w", err)
			}
			cardTo := ""
			if isCash {
				cardTo = "наличные"
			}
			if err := b.db.InsertTransaction(ctx, db.TransactionInput{
				ContactID:    contactID,
				RawName:      canonical,
				Amount:       args.Amount,
				CardTo:       cardTo,
				IsCash:       isCash,
				Collector:    collector,
				RawMessageID: rawID,
				TxDate:       txDate,
			}); err != nil {
				return "", fmt.Errorf("не удалось записать платёж: %w", err)
			}

			kindLabel := "перевод"
			if isCash {
				kindLabel = "наличка"
			}
			groupName := groupJID
			if jid, err := types.ParseJID(groupJID); err == nil {
				if name, ok := b.joinedGroups(ctx)[jid]; ok {
					groupName = name
				}
			}
			out := fmt.Sprintf("Записал: %s — %.0f ₽ (%s), группа %s, %s.", canonical, args.Amount, kindLabel, groupName, txDate.Format("02.01.2006"))
			if collector != "" {
				out += " Собрал: " + collector + "."
			}
			return out, nil
		},
	}
}

// recordExpenseTool — записать расход со слов владельца («сделал расход 5000 на бензин»).
func (b *Bot) recordExpenseTool() ai.Tool {
	return ai.Tool{
		Name: "record_expense",
		Description: "ЗАПИСАТЬ РАСХОД (трату) со слов владельца. Вызывай, когда он пишет про свой расход: " +
			"'сделал расход 5000 на бензин', 'потратил 12000 на аренду', 'расход 3000 обед'. amount обязателен; " +
			"note — на что потрачено (бензин/аренда/еда и т.п.). date — YYYY-MM-DD (по умолчанию сегодня). " +
			"Расходы отдельно от сбора — в сбор не идут. Несколько трат в одном сообщении — вызови по каждой.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"amount": map[string]any{"type": "number", "description": "Сумма расхода в рублях"},
				"note":   map[string]any{"type": "string", "description": "На что потрачено"},
				"date":   map[string]any{"type": "string", "description": "Дата расхода YYYY-MM-DD (по умолчанию сегодня)"},
			},
			"required": []string{"amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Amount float64 `json:"amount"`
				Note   string  `json:"note"`
				Date   string  `json:"date"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			if args.Amount <= 0 {
				return "", fmt.Errorf("нужна сумма расхода (>0)")
			}
			spentAt := time.Now()
			if s := strings.TrimSpace(args.Date); s != "" {
				if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
					spentAt = t
				}
			}
			if _, err := b.db.InsertExpense(ctx, args.Amount, strings.TrimSpace(args.Note), "владелец", spentAt); err != nil {
				return "", fmt.Errorf("не удалось записать расход: %w", err)
			}
			note := strings.TrimSpace(args.Note)
			if note != "" {
				note = " на " + note
			}
			return fmt.Sprintf("Записал расход: %.0f ₽%s (%s).", args.Amount, note, spentAt.Format("02.01.2006")), nil
		},
	}
}

// expensesReportTool — отчёт по расходам за период (текст или PDF).
func (b *Bot) expensesReportTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "expenses_report",
		Description: "Отчёт по РАСХОДАМ (тратам) за период — список и итог. Вызывай при 'скинь мои расходы', " +
			"'сколько потратил за июль', 'расходы за неделю'. format='pdf' — прислать документом, 'text' — текстом.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD"},
				"format":    map[string]any{"type": "string", "enum": []string{"pdf", "text"}, "description": "pdf — документ; text — цифры в ответ"},
			},
			"required": []string{"from_date", "to_date"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Format   string `json:"format"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			from, err := time.Parse("2006-01-02", args.FromDate)
			if err != nil {
				return "", fmt.Errorf("неверная дата начала %q (нужен YYYY-MM-DD)", args.FromDate)
			}
			toDay, err := time.Parse("2006-01-02", args.ToDate)
			if err != nil {
				return "", fmt.Errorf("неверная дата конца %q (нужен YYYY-MM-DD)", args.ToDate)
			}
			items, total, err := b.db.ExpensesForPeriod(ctx, from, toDay.AddDate(0, 0, 1))
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			periodLabel := from.Format("02.01.2006") + " — " + toDay.Format("02.01.2006")
			if len(items) == 0 {
				return fmt.Sprintf("Расходов за %s не найдено.", periodLabel), nil
			}

			if strings.EqualFold(args.Format, "pdf") {
				rows := make([][]string, 0, len(items))
				for _, e := range items {
					what := e.Note
					if what == "" {
						what = "—"
					}
					rows = append(rows, []string{e.SpentAt.Format("02.01.2006"), what, formatRub(e.Amount) + " ₽"})
				}
				section := report.Section{
					Columns:  []string{"Дата", "На что", "Сумма"},
					Rows:     rows,
					TotalRow: []string{"ИТОГО", "", formatRub(total) + " ₽"},
				}
				outPath := fmt.Sprintf("%s/expenses_%s.pdf", b.reportDir, time.Now().Format("2006-01-02_15-04-05"))
				if err := report.GenerateCustom("Мои расходы", "Период: "+periodLabel, []report.Section{section}, b.fontDir, outPath); err != nil {
					return "", fmt.Errorf("ошибка генерации PDF: %w", err)
				}
				b.sendDocument(chat, outPath, "Расходы_"+from.Format("2006-01-02")+"_"+toDay.Format("2006-01-02")+".pdf")
				return fmt.Sprintf("Отправил PDF с расходами за %s. Итого: %.0f ₽.", periodLabel, total), nil
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "Расходы за %s:\n", periodLabel)
			for _, e := range items {
				what := e.Note
				if what != "" {
					what = " — " + what
				}
				fmt.Fprintf(&sb, "• %s: %.0f ₽%s\n", e.SpentAt.Format("02.01.2006"), e.Amount, what)
			}
			fmt.Fprintf(&sb, "Итого: %.0f ₽.", total)
			return sb.String(), nil
		},
	}
}
