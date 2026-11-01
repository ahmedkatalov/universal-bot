package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
)

func (b *Bot) groupReadingTool() ai.Tool {
	return ai.Tool{
		Name: "group_reading_status",
		Description: "Проверяет ПОЛНОТУ ЧТЕНИЯ сохранённых сообщений группы за период: неразобранные фото/PDF и тексты, " +
			"чеки без суммы/клиента, спорный разбор, отложенные повторы. Используй на «всё ли прочитал», «ничего не пропустил», " +
			"«почему сумма неполная», «что ещё не разобрано». Непрочитанное фото не считается автоматически чеком. " +
			"Это НЕ сверка с CMF и НЕ доказательство получения всей прошлой истории WhatsApp. Возвращает полный счётчик проблем и ограниченный список raw_id/wa_message_id для дальнейшего поиска.",
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD включительно; по дате присылки в чат"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD включительно"},
				"group":     map[string]any{"type": "string", "description": "Группа или несколько через запятую; пусто — все рабочие группы"},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Сколько нерешённых сообщений показать (30 по умолчанию). Итоги всегда полные"},
			}, "required": []string{"from_date", "to_date"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
				Limit    int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать параметры: %w", err)
			}
			from, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local)
			if err != nil {
				return "", fmt.Errorf("неверная дата начала %q, нужен YYYY-MM-DD", args.FromDate)
			}
			to, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local)
			if err != nil {
				return "", fmt.Errorf("неверная дата конца %q, нужен YYYY-MM-DD", args.ToDate)
			}
			if to.Before(from) {
				return "", fmt.Errorf("дата конца раньше начала")
			}
			if args.Limit <= 0 {
				args.Limit = 30
			}
			if args.Limit > 200 {
				args.Limit = 200
			}
			groups, label, err := b.resolveGroups(ctx, args.Group)
			if err != nil {
				return "", err
			}
			spool, spoolErr := b.LocalGroupSpoolStatus(from, to.AddDate(0, 0, 1), groups)
			status, inbox, err := b.db.GroupReadingSnapshot(ctx, from, to.AddDate(0, 0, 1), groups, args.Limit)
			if err != nil {
				return "", fmt.Errorf("не удалось проверить чтение группы: %w", err)
			}
			return formatGroupReadingStatus(status, from, to, label, inbox) + groupSpoolReadingNote(spool, spoolErr) + b.ledgerConflictNote(ctx, from, to.AddDate(0, 0, 1), groups), nil
		},
	}
}

func formatGroupReadingStatus(s db.GroupReadingStatus, from, to time.Time, groupLabel string, queued ...db.GroupInboxCoverage) string {
	var inbox db.GroupInboxCoverage
	if len(queued) > 0 {
		inbox = queued[0]
	}
	queueCount := inbox.Pending + inbox.Processing + inbox.Review
	var out strings.Builder
	fmt.Fprintf(&out, "Чтение группы: %s — %s (%s; по дате присылки в чат).\n", from.Format("02.01.2006"), to.Format("02.01.2006"), groupLabel)
	fmt.Fprintf(&out, "Сохранено сообщений: %d; удалено в WhatsApp: %d; без нерешённых вопросов: %d; требуют внимания: %d.\n", s.SavedMessages, s.DeletedMessages, s.SettledMessages, s.UnresolvedMessages)
	if s.SavedMessages == 0 {
		out.WriteString("В базе нет сообщений за эту выборку. Это не означает, что в WhatsApp группа была пуста.\n")
	}
	if s.UnresolvedMessages > 0 {
		fmt.Fprintf(&out, "Неразобранные вложения: %d; тексты: %d; спорный разбор: %d.\n", s.UnreadMedia, s.UnreadTexts, s.DisputedMessages)
		fmt.Fprintf(&out, "Чеков на проверке: %d; с неизвестной суммой: %d; распознавание не завершено: %d; платежей ждут решения о повторе: %d.\n", s.ReviewReceipts, s.UnknownReceiptAmounts, s.RecognitionPending, s.HeldPayments)
		out.WriteString("Непрочитанное вложение может оказаться чеком или другим изображением. Неизвестные суммы нельзя считать нулём или прибавлять к сбору.\n")
		for _, i := range s.Issues {
			who := i.Sender
			if who == "" {
				who = "неизвестный отправитель"
			}
			fmt.Fprintf(&out, "• raw_id=%d; wa_message_id=%s; %s; %s; группа=%s: %s", i.RawID, i.WaID, i.ReceivedAt.In(time.Local).Format("02.01.2006 15:04"), who, i.GroupJID, strings.Join(i.Reasons, "; "))
			if i.KnownPendingAmount > 0 {
				fmt.Fprintf(&out, "; известные суммы нерешённых записей: %s ₽ (не итог сбора)", employeeMoney(i.KnownPendingAmount))
			}
			if i.MediaAttempts > 0 {
				fmt.Fprintf(&out, "; попыток чтения вложения: %d", i.MediaAttempts)
			}
			out.WriteByte('\n')
		}
		if s.Truncated {
			fmt.Fprintf(&out, "Показано %d из %d нерешённых сообщений; счётчики выше относятся ко всему периоду.\n", len(s.Issues), s.UnresolvedMessages)
		}
	} else if s.SavedMessages > 0 && queueCount == 0 {
		out.WriteString("В сохранённых сообщениях этой выборки нерешённых вопросов чтения не найдено. Это не подтверждает полноту всей истории WhatsApp.\n")
	}
	if queueCount > 0 {
		fmt.Fprintf(&out, "В очереди приёма: ждут обработки %d; обрабатываются %d; требуют проверки %d. Это события сообщений/правок/удалений, а не дополнительные чеки или суммы.\n", inbox.Pending, inbox.Processing, inbox.Review)
		for _, i := range inbox.Issues {
			note := groupInboxIssueDescription(i.Note)
			if note == "" {
				note = "обработка ещё не завершена"
			}
			fmt.Fprintf(&out, "• очередь: wa_message_id=%s; группа=%s; %s; вид=%s; состояние=%s; %s\n", i.WaID, i.GroupJID, i.ReceivedAt.In(time.Local).Format("02.01.2006 15:04"), i.Kind, i.State, note)
		}
		if len(inbox.Issues) < queueCount {
			fmt.Fprintf(&out, "Показано %d из %d незавершённых событий очереди.\n", len(inbox.Issues), queueCount)
		}
	}
	out.WriteString("Статус относится только к сообщениям в базе. Сверка внесения платежей в программу CMF выполняется отдельно.")
	return out.String()
}

func groupInboxIssueDescription(code string) string {
	switch code {
	case "unsupported_content":
		return "этот формат сообщения ещё не расшифрован; его содержание не учтено автоматически"
	case "interrupted_command":
		return "выполнение команды было прервано; автоматический повтор отключён, чтобы не повторить действие"
	case "interrupted_recognition":
		return "разбор сообщения был прерван; исходник сохранён и требует дочитывания"
	case "message_identity_conflict":
		return "идентификатор сообщения совпал с записью другой группы; нужна проверка"
	case "interrupted_edit", "interrupted_revoke":
		return "применение изменения сообщения было прервано"
	case "group_not_available":
		return "группа сейчас недоступна для обработки"
	case "invalid_saved_event":
		return "сохранённое событие не удалось прочитать"
	case "processing_failed":
		return "автоматические попытки исчерпаны; требуется проверка"
	case "revoke_cleanup_not_confirmed":
		return "сообщение удалено, но откат связанных записей не подтверждён; нужна проверка"
	case "database_unavailable", "raw_message_not_saved":
		return "сохранение в базе ещё не завершено"
	default:
		return code
	}
}

// groupReadingNote is added to money reports without changing their totals.
// Unknown attachments cannot be assigned to a payer/employee reliably yet.
func (b *Bot) groupReadingNote(ctx context.Context, from, to time.Time, groups []string) string {
	spool, spoolErr := b.LocalGroupSpoolStatus(from, to, groups)
	spoolNote := groupSpoolReadingNote(spool, spoolErr)
	s, inbox, err := b.db.GroupReadingSnapshot(ctx, from, to, groups, 1)
	if err != nil {
		return "\n⚠️ Не удалось проверить полноту чтения сохранённых сообщений; итог нельзя считать подтверждением, что все сообщения разобраны." + spoolNote
	}
	return groupReadingIncompleteNote(s, inbox) + spoolNote + b.ledgerConflictNote(ctx, from, to, groups)
}

// A parsed source can still contradict another copy. Raw-message coverage
// alone cannot establish that client, amount and operation date agree.
func (b *Bot) ledgerConflictNote(ctx context.Context, from, to time.Time, groups []string) string {
	book, err := b.db.LoadBook(ctx, from, to, "")
	if err != nil {
		return "\n⚠️ Проверить согласованность копий чеков не удалось; отчёт предварительный."
	}
	selected := make(map[string]bool, len(groups))
	for _, g := range groups {
		selected[g] = true
	}
	conflicts := 0
	for _, p := range book.Payments {
		if !p.ReviewRequired() {
			continue
		}
		for _, c := range p.Checks {
			if len(selected) > 0 && !selected[c.Group] {
				continue
			}
			posted := !c.PostedAt.Before(from) && c.PostedAt.Before(to)
			operation := !c.OpDate.Before(from) && c.OpDate.Before(to)
			if posted || operation {
				conflicts++
				break
			}
		}
	}
	if conflicts == 0 {
		return ""
	}
	return fmt.Sprintf("\n⚠️ Отчёт предварительный: у %d платежей копии чеков противоречат друг другу по клиенту, сумме или дате. Эти платежи не включены в сбор до уточнения. Подробности доступны в разборе сбора.", conflicts)
}

func groupSpoolReadingNote(s GroupSpoolStatus, err error) string {
	if err != nil {
		return "\n⚠️ Отчёт предварительный: локальный журнал входящих событий проверить не удалось."
	}
	if s.Pending+s.Unreadable == 0 {
		return ""
	}
	return fmt.Sprintf("\n⚠️ Отчёт предварительный: локальный журнал до передачи в базу содержит %d событий; повреждённых записей для проверки: %d. Это состояние дискового буфера, а не дополнительные чеки или известные суммы.", s.Pending, s.Unreadable)
}

func groupReadingIncompleteNote(s db.GroupReadingStatus, inbox db.GroupInboxCoverage) string {
	queueCount := inbox.Pending + inbox.Processing + inbox.Review
	if s.UnreadMedia+s.UnreadTexts+s.DisputedMessages+s.UnknownReceiptAmounts+s.RecognitionPending+queueCount == 0 {
		return ""
	}
	var parts []string
	if s.UnreadMedia > 0 {
		parts = append(parts, fmt.Sprintf("не разобраны вложения: %d (не все обязательно чеки)", s.UnreadMedia))
	}
	if s.UnreadTexts > 0 {
		parts = append(parts, fmt.Sprintf("не дочитаны тексты: %d", s.UnreadTexts))
	}
	if s.DisputedMessages > 0 {
		parts = append(parts, fmt.Sprintf("спорных сообщений: %d", s.DisputedMessages))
	}
	if s.UnknownReceiptAmounts > 0 {
		parts = append(parts, fmt.Sprintf("чеков с неизвестной суммой: %d", s.UnknownReceiptAmounts))
	}
	if s.RecognitionPending > 0 {
		parts = append(parts, fmt.Sprintf("распознавание чеков не завершено: %d", s.RecognitionPending))
	}
	if queueCount > 0 {
		parts = append(parts, fmt.Sprintf("не завершена обработка %d событий сообщений/правок/удалений", queueCount))
	}
	return "\n⚠️ Отчёт предварительный: в выбранных группах за период " + strings.Join(parts, "; ") + ". Неизвестные суммы не равны нулю. Можно запросить список того, что ещё не разобрано; сверка с CMF выполняется отдельно."
}
