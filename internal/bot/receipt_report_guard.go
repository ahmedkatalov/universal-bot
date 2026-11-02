package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
	"whatsapp-bot/internal/ai"
)

const receiptReportInstructions = `

ПОЛНОТА СПИСКА ЧЕКОВ.
Сотрудник (sender) — автор сообщения WhatsApp. Клиент (client) — ФИО из подписи к чеку, банковский плательщик — отдельное поле bank_payer. На уточнение «а как насчёт <ФИО>» после отчёта по сотрудникам проверяй client в том же периоде и группе, очистив sender, если человек явно не назван сотрудником. Не заменяй отсутствующего клиента другим человеком или платежом на похожую сумму. Отсутствие строки в одном отчёте не доказывает, что чек не присылали: проверь сохранённые исходники и незавершённое чтение. Нельзя говорить «всё учтено» при непрочитанных вложениях или неполной истории. Для списка с ФИО вызывай senders_report с view=payments. Данные, итоги, предупреждения и отметка неполной страницы должны сохраняться полностью; не сокращай список при пересказе.
`

type savedReceiptReport struct {
	Args senderReportArgs `json:"args"`
	At   time.Time        `json:"at"`
}

func (b *Bot) receiptReportContextKey(ctx context.Context, info types.MessageInfo) string {
	actor := b.senderActorKey(ctx, info)
	if pn, known := senderPhoneJID(info); known {
		actor = pn.String()
	}
	chat := info.Chat.String()
	if !info.IsGroup {
		chat = "private"
	}
	return "receipt_report_context|" + chat + "|" + actor
}

func reportWantsDetails(query string) bool {
	q := strings.ToLower(query)
	if strings.Contains(q, "без фио") || strings.Contains(q, "без списка") || strings.Contains(q, "только итог") {
		return false
	}
	return strings.Contains(q, "фио") || strings.Contains(q, "плательщик") || strings.Contains(q, "список") || strings.Contains(q, "таблиц") || strings.Contains(q, "подробн")
}

// The model can explain a financial report, but cannot safely rewrite its
// rows: a shortened table must never replace the authoritative tool result.
type receiptReportReply struct {
	mu         sync.Mutex
	reportKeys map[string]int
	reports    []string
	warnings   []string
	other      []string
}

func (r *receiptReportReply) available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reports) > 0
}

func (r *receiptReportReply) apply(reply string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) > 0 {
		reply = strings.Join(r.reports, "\n\n")
		if len(r.other) > 0 {
			reply += "\n\nДополнительные результаты:\n" + strings.Join(r.other, "\n\n")
		}
	}
	for _, warning := range r.warnings {
		if !strings.Contains(reply, warning) {
			reply += "\n" + warning
		}
	}
	return strings.TrimSpace(reply)
}

func (b *Bot) trackReceiptReports(ctx context.Context, info types.MessageInfo, query string, original []ai.Tool) ([]ai.Tool, *string, *receiptReportReply) {
	state := &receiptReportReply{reportKeys: make(map[string]int)}
	wrapped := make([]ai.Tool, len(original))
	for i, tool := range original {
		tool := tool
		handle := tool.Handle
		tool.Handle = func(callCtx context.Context, input json.RawMessage) (string, error) {
			var args senderReportArgs
			if tool.Name == "senders_report" && json.Unmarshal(input, &args) == nil {
				if reportWantsDetails(query) {
					args.View = "payments"
					if len(args.Columns) == 0 {
						args.Columns = []string{"employee", "client", "bank_payer", "amount", "date", "status"}
					} else {
						for _, required := range []string{"client", "amount"} {
							found := false
							for _, c := range args.Columns {
								found = found || c == required
							}
							if !found {
								args.Columns = append(args.Columns, required)
							}
						}
					}
					input, _ = json.Marshal(args)
				}
			}
			out, err := handle(callCtx, input)
			if err != nil || strings.TrimSpace(out) == "" {
				return out, err
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if tool.Name == "senders_report" {
				if _, _, err := args.validate(); err == nil && b.db != nil {
					stored, _ := json.Marshal(savedReceiptReport{Args: args, At: time.Now()})
					if err := b.db.SettingSet(callCtx, b.receiptReportContextKey(ctx, info), string(stored)); err != nil {
						fmt.Println("Не удалось сохранить параметры отчёта по чекам:", err)
					}
				}
				// Keep separate selections and pages: a request may compare two
				// groups, employees or periods. Refresh only the identical page.
				key, _ := json.Marshal(args)
				if index, exists := state.reportKeys[string(key)]; exists {
					state.reports[index] = out
				} else {
					state.reportKeys[string(key)] = len(state.reports)
					state.reports = append(state.reports, out)
				}
			} else {
				state.other = append(state.other, out)
				switch tool.Name {
				case "send_finance_report", "cards_report", "receipts_ledger", "explain_collection", "person_report", "group_reading_status":
					for _, line := range strings.Split(out, "\n") {
						if strings.Contains(line, "⚠️") {
							state.warnings = append(state.warnings, line)
						}
					}
				}
			}
			return out, nil
		}
		wrapped[i] = tool
	}
	tools, last := trackTools(wrapped)
	return tools, last, state
}

func receiptFollowupClient(query string) (string, bool) {
	q := strings.TrimSpace(query)
	low := strings.ReplaceAll(strings.ToLower(q), "ё", "е")
	for _, prefix := range []string{"а как насчет ", "как насчет ", "а что насчет ", "что насчет "} {
		if strings.HasPrefix(low, prefix) {
			name := strings.Trim(strings.TrimSpace(q[len(prefix):]), " ?!.…\n\r\t")
			return looksLikeName(name)
		}
	}
	return "", false
}

func (b *Bot) receiptClientFollowup(ctx context.Context, info types.MessageInfo, query string, isAdmin bool) (string, bool) {
	if !isAdmin || b.db == nil {
		return "", false
	}
	if stripped, addressed := b.stripBotName(query); addressed {
		query = stripped
	}
	name, ok := receiptFollowupClient(query)
	if !ok {
		return "", false
	}
	key := b.receiptReportContextKey(ctx, info)
	raw, err := b.db.SettingGet(ctx, key)
	if err != nil || raw == "" {
		return "", false
	}
	var saved savedReceiptReport
	if json.Unmarshal([]byte(raw), &saved) != nil || time.Since(saved.At) > 24*time.Hour {
		return "", false
	}
	if saved.Args.View != "payments" && saved.Args.Client == "" {
		return "", false // a worker-only summary does not establish a client follow-up
	}
	args := saved.Args
	args.Sender, args.Client = "", name
	args.Format, args.View, args.Offset = "text", "payments", 0
	args.Columns = []string{"employee", "client", "bank_payer", "amount", "date", "status"}
	from, to, err := args.validate()
	if err != nil {
		return "", false
	}
	input, _ := json.Marshal(args)
	out, err := b.sendersTool(ctx, info.Chat).Handle(ctx, input)
	if err != nil {
		return "Не получилось проверить чек клиента: " + err.Error(), true
	}
	groups, _, err := b.resolveGroups(ctx, args.Group)
	if err == nil && args.DateBasis != "operation" {
		evidence, lookupErr := b.db.FindClientReceiptEvidence(ctx, from, to.AddDate(0, 0, 1), groups, name)
		if lookupErr != nil {
			out += "\n⚠️ Проверить сохранённые исходные сообщения не удалось; отсутствие чека не подтверждено."
		} else if len(evidence) == 0 {
			out += "\nВ сохранённых исходниках этой выборки совпадений по такому написанию ФИО нет. Это не доказывает, что чек не присылали в WhatsApp: мог не загрузиться исходник или история. Нужен исходный чек либо импорт истории этой группы."
		} else {
			out += "\nПроверка исходных сообщений (не дополнительные платежи):"
			for _, e := range evidence {
				kind := "сообщение"
				if e.HasMedia {
					kind = "вложение"
				}
				status := map[string]string{"unread": "разбор не завершён", "review": "требует уточнения или перечитывания", "parsed": "исходник разобран; его наличие само по себе не означает включение в сбор", "excluded": "удалено, исключено или отмечено повтором"}[e.Status]
				if status == "" {
					status = "статус требует проверки"
				}
				if !e.HasMedia && !e.HasReceipt && !e.Deleted {
					status = "текст с этим ФИО сохранён; это не подтверждает наличие банковского чека"
				}
				out += fmt.Sprintf("\n• %s от %s, %s: %s.", kind, orDash(e.Sender), e.PostedAt.In(time.Local).Format("02.01.2006 15:04"), status)
			}
		}
	}
	encoded, _ := json.Marshal(savedReceiptReport{Args: args, At: time.Now()})
	_ = b.db.SettingSet(ctx, key, string(encoded))
	return out, true
}
