// Инструменты для работы с "непонятыми" чеками и полного пересчёта:
// владелец может спросить, что бот не смог разобрать, получить сам файл
// чека в чат, продиктовать данные с чека словами — и запустить пересчёт
// всей накопленной истории.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

// dbTransactionFromParsed собирает TransactionInput из распарсенной строки.
func dbTransactionFromParsed(tr parser.Transaction, contactID, rawID int, ts time.Time) db.TransactionInput {
	return db.TransactionInput{
		ContactID:    contactID,
		RawName:      tr.RawName,
		Amount:       tr.Amount,
		Note:         tr.Note,
		CardTo:       tr.CardTo,
		IsCash:       parser.IsCash(tr.RawName + " " + tr.Note + " " + tr.CardTo),
		RawMessageID: rawID,
		TxDate:       ts,
	}
}

// unclearTool — список чеков/фото, которые бот не смог уверенно разобрать.
func (b *Bot) unclearTool() ai.Tool {
	return ai.Tool{
		Name: "list_unclear_receipts",
		Description: "Показывает чеки и фото, которые бот НЕ смог уверенно разобрать (незнакомое имя получателя, " +
			"не распозналась сумма, нечитаемое фото). Вызывай при вопросах 'какие чеки ты не понял', " +
			"'что не распозналось', 'есть ли проблемные чеки'. У каждого элемента есть код вида receipt:12 или " +
			"message:34 — используй его в send_unclear_file и fix_receipt.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы, если нужно только по ней. Пусто — по всем.",
				},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Group string `json:"group"`
			}
			_ = json.Unmarshal(input, &args)
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}
			items, err := b.db.UnclearItems(ctx, groupJID, 15)
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			if len(items) == 0 {
				return "Непонятых чеков нет — всё, что приходило, разобрано.", nil
			}

			groups := b.joinedGroups(ctx)
			var sb strings.Builder
			sb.WriteString("Непонятые чеки/фото (свежие первыми):\n")
			for _, it := range items {
				code := fmt.Sprintf("%s:%d", it.Kind, it.ID)
				groupLabel := ""
				if jid, err := types.ParseJID(it.GroupJID); err == nil {
					if name, ok := groups[jid]; ok {
						groupLabel = ", группа " + name
					}
				}
				switch {
				case it.Kind == "message":
					fmt.Fprintf(&sb, "- [%s] фото/файл без распознанной операции, от %s, %s%s\n",
						code, it.SenderName, it.TxDate.Format("02.01.2006 15:04"), groupLabel)
				case it.Amount == 0 && it.RecipientRaw == "":
					fmt.Fprintf(&sb, "- [%s] не распознались ни сумма, ни получатель, от %s, %s%s\n",
						code, it.SenderName, it.TxDate.Format("02.01.2006 15:04"), groupLabel)
				case it.Amount == 0:
					fmt.Fprintf(&sb, "- [%s] получатель %q, сумма НЕ распозналась, от %s, %s%s\n",
						code, it.RecipientRaw, it.SenderName, it.TxDate.Format("02.01.2006 15:04"), groupLabel)
				default:
					fmt.Fprintf(&sb, "- [%s] получатель %q (незнакомое имя), %.0f ₽, от %s, %s%s\n",
						code, it.RecipientRaw, it.Amount, it.SenderName, it.TxDate.Format("02.01.2006 15:04"), groupLabel)
				}
			}
			sb.WriteString("Файл любого из них можно прислать в чат (send_unclear_file), а после уточнения владельца — записать данные (fix_receipt).")
			return sb.String(), nil
		},
	}
}

// sendUnclearFileTool — присылает файл непонятого чека в текущий чат.
func (b *Bot) sendUnclearFileTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "send_unclear_file",
		Description: "Отправляет в этот чат сохранённый файл (фото или PDF) непонятого чека, чтобы владелец сам " +
			"посмотрел, что на нём. Вызывай, когда просят 'скинь чек, который ты не понял', 'покажи то фото'. " +
			"item — код из list_unclear_receipts (например receipt:12 или message:34).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item": map[string]any{
					"type":        "string",
					"description": "Код элемента из list_unclear_receipts, формат kind:id",
				},
			},
			"required": []string{"item"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Item string `json:"item"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			kind, id, err := parseUnclearCode(args.Item)
			if err != nil {
				return "", err
			}
			path, err := b.db.UnclearMediaPath(ctx, kind, id)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("файл %s не читается: %w", filepath.Base(path), err)
			}
			if strings.HasSuffix(strings.ToLower(path), ".pdf") {
				b.sendDocumentBytes(chat, data, filepath.Base(path), "application/pdf")
			} else {
				b.sendImageBytes(chat, data)
			}
			return fmt.Sprintf("Файл чека [%s] отправлен в чат. Когда владелец скажет, что на нём написано — вызови fix_receipt с этими данными.", args.Item), nil
		},
	}
}

// sendUnclearFilesTool — прислать в этот чат (обычно личку владельца) СРАЗУ ВСЕ
// файлы непонятых чеков из группы: «скинь нераспознанные чеки из этой группы».
func (b *Bot) sendUnclearFilesTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "send_unclear_files",
		Description: "Присылает в этот чат ВСЕ файлы (фото/PDF) непонятых чеков из группы — тех, что бот не смог " +
			"разобрать. Вызывай при 'скинь нераспознанные чеки из группы X', 'покажи все непонятые чеки', " +
			"'пришли мне чеки, которые не понял'. Каждый файл подписан кодом (receipt:12) и тем, что не так — " +
			"потом эти данные можно продиктовать через fix_receipt. group — по какой группе (пусто — по всем).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"group": map[string]any{"type": "string", "description": "Название группы (пусто — по всем)"},
				"limit": map[string]any{"type": "integer", "description": "Сколько файлов прислать (по умолчанию 10, максимум 30)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Group string `json:"group"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(input, &args)
			if args.Limit <= 0 || args.Limit > 30 {
				args.Limit = 10
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}
			items, err := b.db.UnclearItems(ctx, groupJID, args.Limit)
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			groups := b.joinedGroups(ctx)
			sent, noFile := 0, 0
			for _, it := range items {
				if strings.TrimSpace(it.MediaPath) == "" {
					continue // без файла (например, текстовое сообщение) — тут не шлём
				}
				data, err := os.ReadFile(it.MediaPath)
				if err != nil {
					noFile++
					continue
				}
				code := fmt.Sprintf("%s:%d", it.Kind, it.ID)
				caption := code + " — " + unclearReason(it)
				if jid, err := types.ParseJID(it.GroupJID); err == nil {
					if name, ok := groups[jid]; ok {
						caption += " (группа " + name + ")"
					}
				}
				if strings.HasSuffix(strings.ToLower(it.MediaPath), ".pdf") {
					b.sendDocumentBytes(chat, data, code+".pdf", "application/pdf")
					b.sendText(chat, caption)
				} else {
					b.sendImageWithCaption(chat, data, caption)
				}
				sent++
			}
			if sent == 0 {
				if noFile > 0 {
					return "Непонятые чеки есть, но их файлы на диске недоступны.", nil
				}
				return "Непонятых чеков с файлами не нашла — всё разобрано.", nil
			}
			return fmt.Sprintf("Отправил %d файл(ов) непонятых чеков. Под каждым — код (напр. receipt:12) и что не так. "+
				"Скажи, что на нужном чеке (например 'на receipt:12 Ахмед Каталов 15000'), и я запишу через fix_receipt.", sent), nil
		},
	}
}

// unclearReason — короткое описание, что именно бот не разобрал.
func unclearReason(it db.UnclearItem) string {
	switch {
	case it.Kind == "message":
		return "фото/файл без распознанной операции"
	case it.Amount == 0 && it.RecipientRaw == "":
		return "не распознались ни сумма, ни получатель"
	case it.Amount == 0:
		return fmt.Sprintf("получатель %q, сумма не распозналась", it.RecipientRaw)
	default:
		return fmt.Sprintf("получатель %q — незнакомое имя, %.0f ₽", it.RecipientRaw, it.Amount)
	}
}

// sendClientReceiptTool — прислать сохранённый файл чека конкретного клиента
// ("скинь чек Миланы", "покажи чеки Ахмеда за июнь").
func (b *Bot) sendClientReceiptTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "send_client_receipt",
		Description: "Присылает в этот чат сохранённый файл чека (фото или PDF) конкретного клиента/человека. " +
			"Вызывай при 'скинь чек Миланы', 'покажи чек Ахмеда Каталова', 'пришли чеки за июнь по Расулу'. " +
			"Имя обязательно; период и группа — по желанию. По умолчанию шлёт до 5 последних.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"person":    map[string]any{"type": "string", "description": "Имя клиента/человека"},
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD (необязательно)"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD (необязательно)"},
				"group":     map[string]any{"type": "string", "description": "Название группы (необязательно)"},
				"limit":     map[string]any{"type": "integer", "description": "Сколько последних чеков прислать (по умолчанию 5)"},
			},
			"required": []string{"person"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Person   string `json:"person"`
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
				Limit    int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Person) == "" {
				return "", fmt.Errorf("нужно имя клиента")
			}
			if args.Limit <= 0 || args.Limit > 20 {
				args.Limit = 5
			}
			var fromPtr, toPtr *time.Time
			if args.FromDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local); err == nil {
					fromPtr = &t
				}
			}
			if args.ToDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local); err == nil {
					next := t.AddDate(0, 0, 1)
					toPtr = &next
				}
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}
			files, err := b.db.ReceiptFilesForPerson(ctx, strings.TrimSpace(args.Person), fromPtr, toPtr, groupJID, args.Limit)
			if err != nil {
				return "", err
			}
			if len(files) == 0 {
				return fmt.Sprintf("Сохранённых файлов чеков по %q не нашла.", args.Person), nil
			}
			sent := 0
			for _, f := range files {
				data, err := os.ReadFile(f.MediaPath)
				if err != nil {
					continue
				}
				if strings.HasSuffix(strings.ToLower(f.MediaPath), ".pdf") {
					b.sendDocumentBytes(chat, data, fmt.Sprintf("%s_%s.pdf", f.Name, f.TxDate.Format("2006-01-02")), "application/pdf")
				} else {
					b.sendImageBytes(chat, data)
				}
				sent++
			}
			if sent == 0 {
				return "Файлы чеков нашлись в базе, но сами файлы на диске недоступны.", nil
			}
			return fmt.Sprintf("Отправил %d чек(ов) по %q.", sent, args.Person), nil
		},
	}
}

// receiptDetailsTool — показать все распознанные данные чека(ов) клиента.
func (b *Bot) receiptDetailsTool() ai.Tool {
	return ai.Tool{
		Name: "receipt_details",
		Description: "Показывает ВСЕ распознанные данные чека(ов) конкретного клиента: клиент (кому принадлежит), " +
			"получатель на чеке и его банк/телефон, отправитель и его банк/счёт, сумма, комиссия, дата/время, " +
			"номер документа, код авторизации, статус. Вызывай при 'покажи все данные чека Цихаева', " +
			"'что за чек у Миланы', 'детали последнего чека Ахмеда'.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"person": map[string]any{"type": "string", "description": "Имя клиента"},
				"limit":  map[string]any{"type": "integer", "description": "Сколько последних чеков показать (по умолчанию 3)"},
			},
			"required": []string{"person"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Person string `json:"person"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Person) == "" {
				return "", fmt.Errorf("нужно имя клиента")
			}
			if args.Limit <= 0 || args.Limit > 10 {
				args.Limit = 3
			}
			items, err := b.db.ReceiptDetailsForPerson(ctx, strings.TrimSpace(args.Person), args.Limit)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				return fmt.Sprintf("Чеков по %q не нашла.", args.Person), nil
			}
			var sb strings.Builder
			for i, r := range items {
				if i > 0 {
					sb.WriteString("\n")
				}
				fmt.Fprintf(&sb, "Чек %d — клиент: %s\n", i+1, r.Client)
				fmt.Fprintf(&sb, "  Сумма: %.0f ₽", r.Amount)
				if r.Commission > 0 {
					fmt.Fprintf(&sb, " (комиссия %.0f ₽)", r.Commission)
				}
				sb.WriteString("\n")
				if !r.TxDate.IsZero() {
					fmt.Fprintf(&sb, "  Дата операции: %s\n", r.TxDate.Format("02.01.2006 15:04:05"))
				}
				appendField(&sb, "  Получатель на чеке", r.CardOwner)
				appendField(&sb, "  Банк получателя", r.RecipientBank)
				appendField(&sb, "  Телефон получателя", r.RecipientPhone)
				appendField(&sb, "  Отправитель", r.Sender)
				appendField(&sb, "  Банк отправителя", r.SenderBank)
				appendField(&sb, "  Счёт отправителя", r.SenderAccount)
				appendField(&sb, "  Банк чека", r.Bank)
				appendField(&sb, "  Номер документа", r.DocNumber)
				appendField(&sb, "  Код авторизации", r.AuthCode)
				appendField(&sb, "  Статус", r.Status)
			}
			return sb.String(), nil
		},
	}
}

func appendField(sb *strings.Builder, label, value string) {
	if strings.TrimSpace(value) != "" {
		fmt.Fprintf(sb, "%s: %s\n", label, value)
	}
}

// fixReceiptTool — владелец продиктовал, что написано на чеке; записываем.
func (b *Bot) fixReceiptTool() ai.Tool {
	return ai.Tool{
		Name: "fix_receipt",
		Description: "Записывает данные непонятого чека со слов владельца: 'на том чеке Милана, 25000, 2 июля'. " +
			"item — код из list_unclear_receipts (receipt:12 или message:34). После этого чек попадает в учёт " +
			"как обычный. Указывай только то, что владелец назвал; остальное не трогается.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item": map[string]any{
					"type":        "string",
					"description": "Код элемента, формат kind:id",
				},
				"person": map[string]any{
					"type":        "string",
					"description": "Имя получателя (кому пришли деньги)",
				},
				"amount": map[string]any{
					"type":        "number",
					"description": "Сумма в рублях (0 = не менять)",
				},
				"date": map[string]any{
					"type":        "string",
					"description": "Дата операции YYYY-MM-DD или YYYY-MM-DD HH:MM (пусто = не менять)",
				},
			},
			"required": []string{"item"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Item   string  `json:"item"`
				Person string  `json:"person"`
				Amount float64 `json:"amount"`
				Date   string  `json:"date"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			kind, id, err := parseUnclearCode(args.Item)
			if err != nil {
				return "", err
			}

			var contactIDPtr *int
			canonical := strings.TrimSpace(args.Person)
			if canonical != "" {
				resolved, _ := b.aliases.ResolveName(canonical)
				canonical = resolved
				if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
					contactIDPtr = &cid
				}
			}

			var txDatePtr *time.Time
			if s := strings.TrimSpace(args.Date); s != "" {
				var t time.Time
				var err error
				for _, layout := range []string{"2006-01-02 15:04", "2006-01-02"} {
					if t, err = time.ParseInLocation(layout, s, time.Local); err == nil {
						break
					}
				}
				if err != nil {
					return "", fmt.Errorf("неверная дата %q, нужен формат YYYY-MM-DD [HH:MM]", s)
				}
				txDatePtr = &t
			}

			if kind == "message" && (args.Amount <= 0 || canonical == "") {
				return "", fmt.Errorf("для фото без распознанных данных нужны и имя получателя, и сумма")
			}

			if err := b.db.FixReceipt(ctx, kind, id, contactIDPtr, canonical, args.Amount, txDatePtr); err != nil {
				return "", fmt.Errorf("не удалось записать: %w", err)
			}

			var parts []string
			if canonical != "" {
				parts = append(parts, "получатель "+canonical)
			}
			if args.Amount > 0 {
				parts = append(parts, fmt.Sprintf("%.0f ₽", args.Amount))
			}
			if txDatePtr != nil {
				parts = append(parts, txDatePtr.Format("02.01.2006 15:04"))
			}
			return fmt.Sprintf("Записал чек [%s]: %s. Он теперь в учёте как обычный.", args.Item, strings.Join(parts, ", ")), nil
		},
	}
}

// recountTool — полный пересчёт: повторное сопоставление имён, доразбор
// нераспознанных сообщений, актуализация учёта.
func (b *Bot) recountTool() ai.Tool {
	return ai.Tool{
		Name: "recount_everything",
		Description: "Полный пересчёт учёта заново: повторно сопоставляет непонятые чеки с известными именами " +
			"(полезно после добавления алиасов или ручных правок), заново прогоняет нераспознанные текстовые " +
			"сообщения через парсер и ИИ. Вызывай, когда владелец говорит 'пересчитай', 'проанализируй всё заново', " +
			"'обнови учёт'. После пересчёта, если владелец просил цифры, вызови send_finance_report.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			return b.recountEverything(ctx)
		},
	}
}

// recountEverything — реализация пересчёта.
func (b *Bot) recountEverything(ctx context.Context) (string, error) {
	var sb strings.Builder

	// 1. Повторное сопоставление чеков с ручной проверки (алиасы могли
	// пополниться, логика — улучшиться).
	reResolved := 0
	if unresolved, err := b.db.UnresolvedReceipts(ctx); err == nil {
		for _, r := range unresolved {
			canonical, matched := b.aliases.ResolveName(r.RecipientRaw)
			if !matched {
				continue
			}
			contactID, err := b.db.GetOrCreateContact(ctx, canonical)
			if err != nil {
				continue
			}
			if err := b.db.AssignReceiptContact(ctx, r.ID, contactID); err == nil {
				reResolved++
			}
		}
	} else {
		fmt.Fprintf(&sb, "(не удалось перечитать чеки на проверке: %v)\n", err)
	}

	// 2. Повторный разбор текстовых сообщений, не давших ни одной операции.
	reParsed := 0
	aiCalls := 0
	if msgs, err := b.db.UnparsedTextMessages(ctx, 100); err == nil {
		for _, m := range msgs {
			result := parser.ParseMessage(m.Body)
			saved := 0
			for _, tr := range result.Transactions {
				canonical := b.aliases.Resolve(tr.RawName)
				contactID, err := b.db.GetOrCreateContact(ctx, canonical)
				if err != nil {
					continue
				}
				if err := b.db.InsertTransaction(ctx, dbTransactionFromParsed(tr, contactID, m.ID, m.ReceivedAt)); err == nil {
					saved++
				}
			}
			// Строки с цифрами, которые не взял парсер, — через ИИ (не больше
			// 15 обращений за пересчёт, чтобы не жечь токены на старой болтовне).
			if saved == 0 && len(result.Unparsed) > 0 && containsDigit(result.Unparsed) && b.assistant != nil && aiCalls < 15 {
				aiCalls++
				jid, err := types.ParseJID(m.GroupJID)
				if err == nil {
					b.aiRescueUnparsed(ctx, jid, m.SenderName, result.Unparsed, m.ID, m.ReceivedAt)
				}
			}
			if saved > 0 {
				reParsed += saved
			}
			_ = b.db.MarkMessageParsed(ctx, m.ID)
		}
	} else {
		fmt.Fprintf(&sb, "(не удалось перечитать сообщения: %v)\n", err)
	}

	fmt.Fprintf(&sb, "Пересчёт завершён:\n")
	fmt.Fprintf(&sb, "- чеков возвращено в учёт после повторного сопоставления имён: %d\n", reResolved)
	fmt.Fprintf(&sb, "- платежей извлечено из ранее нераспознанных сообщений: %d (плюс доразбор ИИ в фоне: %d сообщений)\n", reParsed, aiCalls)
	sb.WriteString("Все отчёты теперь строятся по обновлённым данным.")
	return sb.String(), nil
}

// parseUnclearCode разбирает код элемента вида "receipt:12" / "message:34".
func parseUnclearCode(code string) (string, int, error) {
	parts := strings.SplitN(strings.TrimSpace(code), ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("неверный код %q, ожидается формат receipt:12 или message:34", code)
	}
	kind := strings.TrimSpace(parts[0])
	if kind != "receipt" && kind != "message" {
		return "", 0, fmt.Errorf("неизвестный тип %q", kind)
	}
	var id int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &id); err != nil || id <= 0 {
		return "", 0, fmt.Errorf("неверный id в коде %q", code)
	}
	return kind, id, nil
}
