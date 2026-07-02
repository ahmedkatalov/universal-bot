// Package bot подключается к WhatsApp через whatsmeow, слушает сообщения
// рабочих групп (по умолчанию — всех групп, в которых состоит номер),
// парсит их и сохраняет транзакции в БД. Умеет отвечать на команду "/отчет"
// готовым PDF в группе и отвечать в личных сообщениях через Claude,
// включая формирование отчёта за произвольный период по запросу.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	// Драйвер SQLite для хранилища сессии whatsmeow. Импорт с подчёркиванием —
	// только регистрация драйвера в database/sql, напрямую пакет не используется.
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ocr"
	"whatsapp-bot/internal/parser"
	"whatsapp-bot/internal/report"
)

// maxPrivateHistory — сколько последних реплик личного диалога держим в
// памяти как контекст для Claude. История не переживает рестарт бота —
// это не журнал операций (тот в БД), а просто продолжение разговора.
const maxPrivateHistory = 20

type Bot struct {
	client        *whatsmeow.Client
	db            *db.DB
	aliases       *parser.AliasMap
	ocr           ocr.Extractor
	assistant     *ai.Assistant
	allowedGroups map[types.JID]bool // пусто/nil => разрешены все группы, в которых состоит номер
	fontDir       string
	reportDir     string

	historyMu sync.Mutex
	history   map[string][]ai.Turn // sender JID -> личная переписка с ассистентом
}

// New создаёт клиент whatsmeow. sessionDBPath — путь к SQLite-файлу сессии,
// сохраняется между рестартами, чтобы не сканировать QR каждый раз.
// assistant может быть nil — тогда бот не отвечает в личных сообщениях
// (например, если не задан ANTHROPIC_API_KEY). groupJIDs — список JID групп,
// которые нужно учитывать; пустой список означает "все группы, в которых
// состоит номер бота".
func New(ctx context.Context, sessionDBPath string, database *db.DB, aliases *parser.AliasMap, ocrClient ocr.Extractor, assistant *ai.Assistant, groupJIDs []string, fontDir, reportDir string) (*Bot, error) {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+sessionDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("device store: %w", err)
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	// Опциональный прокси для WhatsApp-трафика (socks5://... или http://...).
	// Нужен, если сервер стоит в сети, где WhatsApp блокируется. На зарубежном
	// VPS переменную WHATSAPP_PROXY просто не задаём.
	if proxyAddr := os.Getenv("WHATSAPP_PROXY"); proxyAddr != "" {
		if err := client.SetProxyAddress(proxyAddr); err != nil {
			return nil, fmt.Errorf("не удалось установить прокси: %w", err)
		}
		fmt.Println("WhatsApp-трафик пойдёт через прокси:", proxyAddr)
	}

	var allowedGroups map[types.JID]bool
	if len(groupJIDs) > 0 {
		allowedGroups = make(map[types.JID]bool, len(groupJIDs))
		for _, s := range groupJIDs {
			jid, err := types.ParseJID(s)
			if err != nil {
				return nil, fmt.Errorf("неверный group JID %q: %w", s, err)
			}
			allowedGroups[jid] = true
		}
	}

	b := &Bot{
		client:        client,
		db:            database,
		aliases:       aliases,
		ocr:           ocrClient,
		assistant:     assistant,
		allowedGroups: allowedGroups,
		fontDir:       fontDir,
		reportDir:     reportDir,
		history:       make(map[string][]ai.Turn),
	}

	client.AddEventHandler(b.handleEvent)
	return b, nil
}

// Connect авторизуется. Если сохранённой сессии нет — печатает QR-код в консоль,
// его нужно один раз отсканировать с телефона (WhatsApp -> Связанные устройства).
func (b *Bot) Connect(ctx context.Context) error {
	if b.client.Store.ID == nil {
		qrChan, _ := b.client.GetQRChannel(ctx)
		if err := b.client.Connect(); err != nil {
			return err
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("Отсканируй этот QR в WhatsApp -> Связанные устройства:")
				// Рисуем QR прямо в терминале — не нужен сторонний сайт-генератор.
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				fmt.Println("Текст кода (если QR не читается):", evt.Code)
			} else {
				fmt.Println("Статус входа:", evt.Event)
			}
		}
		return nil
	}
	return b.client.Connect()
}

func (b *Bot) Disconnect() {
	b.client.Disconnect()
}

// isAllowedGroup решает, нужно ли учитывать сообщения из этой группы.
// Если список групп не задан при старте — учитываем ВСЕ группы, в которых
// состоит номер бота (это позволяет вести общий сбор сразу по нескольким
// чатам без перечисления JID вручную).
func (b *Bot) isAllowedGroup(jid types.JID) bool {
	if b.allowedGroups == nil {
		return true
	}
	return b.allowedGroups[jid]
}

func (b *Bot) handleEvent(evt interface{}) {
	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}
	ctx := context.Background()

	if !msg.Info.IsGroup {
		// Личный чат с номером бота — если настроен Claude, отвечаем как
		// персональный ассистент; иначе просто игнорируем.
		fmt.Printf("Личное сообщение от %s (chat=%s, fromMe=%v): %q\n",
			msg.Info.Sender, msg.Info.Chat, msg.Info.IsFromMe, extractText(msg.Message))
		if b.assistant != nil {
			b.handlePrivateMessage(ctx, msg)
		} else {
			fmt.Println("Ассистент не настроен (нет OPENROUTER_API_KEY) — сообщение проигнорировано")
		}
		return
	}

	if !b.isAllowedGroup(msg.Info.Chat) {
		return
	}

	b.handleGroupMessage(ctx, msg)
}

// handleGroupMessage разбирает сообщение из рабочей группы (сбор средств,
// чеки, команда /отчет). Работает одинаково для любой группы из
// allowedGroups (или для всех групп, если ограничение не задано).
func (b *Bot) handleGroupMessage(ctx context.Context, msg *events.Message) {
	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = msg.Info.Sender.User
	}

	imgMsg := msg.Message.GetImageMessage()
	caption := extractText(msg.Message)

	var (
		text      string
		hasMedia  bool
		mediaPath string
	)

	if imgMsg != nil {
		hasMedia = true
		imgBytes, err := b.client.Download(ctx, imgMsg)
		if err != nil {
			fmt.Println("Ошибка скачивания фото:", err)
			// сохраняем хотя бы подпись, если она есть
			text = caption
		} else {
			mediaPath = b.saveMediaFile(msg.Info.ID, imgBytes)
			ocrText, err := b.ocr.ExtractText(ctx, imgBytes)
			if err != nil {
				fmt.Println("Ошибка OCR:", err)
				text = caption
			} else {
				// Объединяем текст с чека и подпись (если человек что-то дописал вручную)
				text = ocrText
				if caption != "" {
					text = caption + "\n" + ocrText
				}
			}
		}
	} else {
		text = extractText(msg.Message)
	}

	if text == "" && !hasMedia {
		return
	}

	rawID, err := b.db.SaveRawMessage(ctx, msg.Info.ID, msg.Info.Chat.String(), msg.Info.Sender.String(),
		senderName, text, hasMedia, mediaPath, msg.Info.Timestamp)
	if err != nil {
		fmt.Println("Ошибка сохранения сообщения:", err)
		return
	}

	// Команда отчёта (актуально только для текстовых сообщений)
	if !hasMedia && isReportCommand(text) {
		b.sendMonthlyReport(ctx, msg.Info.Chat)
		return
	}

	if text == "" {
		fmt.Printf("Фото без распознанного текста (сообщение %d), нужна ручная проверка: %s\n", rawID, mediaPath)
		return
	}

	// Если фото похоже на скриншот банковского перевода — разбираем отдельным
	// парсером с полями (Получатель/Сколько/Статус), а не обычным построчным.
	if hasMedia && parser.LooksLikeBankReceipt(text) {
		b.handleBankReceipt(ctx, text, rawID, msg.Info.Timestamp)
		return
	}

	result := parser.ParseMessage(text)
	for _, tr := range result.Transactions {
		canonical := b.aliases.Resolve(tr.RawName)
		contactID, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			fmt.Println("Ошибка получения контакта:", err)
			continue
		}
		err = b.db.InsertTransaction(ctx, db.TransactionInput{
			ContactID:    contactID,
			RawName:      tr.RawName,
			Amount:       tr.Amount,
			Note:         tr.Note,
			CardTo:       tr.CardTo,
			RawMessageID: rawID,
			TxDate:       msg.Info.Timestamp,
		})
		if err != nil {
			fmt.Println("Ошибка сохранения транзакции:", err)
		}
	}

	if len(result.Unparsed) > 0 {
		fmt.Printf("Не распознано (сообщение %d): %v\n", rawID, result.Unparsed)
		// При желании можно отправлять эти строки владельцу в личку для ручной проверки.
	}

	_ = b.db.MarkMessageParsed(ctx, rawID)
}

// handlePrivateMessage отвечает на сообщение, присланное прямо номеру бота
// (не в рабочую группу), через Claude — с учётом сводки по текущему сбору
// и с доступом к инструменту "отчёт за произвольный период", чтобы владелец
// мог просто написать, например, "сколько собрали 3 и 4 июля", и получить
// ответ текстом или PDF-файлом — как попросит.
func (b *Bot) handlePrivateMessage(ctx context.Context, msg *events.Message) {
	text := extractText(msg.Message)
	if text == "" {
		fmt.Println("Личное сообщение без текста (фото/стикер/реакция?) — пропускаю")
		return
	}

	sender := msg.Info.Sender.String()
	chat := msg.Info.Chat
	fmt.Println("Спрашиваю ассистента (OpenRouter)...")

	b.historyMu.Lock()
	history := append([]ai.Turn(nil), b.history[sender]...)
	b.historyMu.Unlock()

	system := b.buildAssistantSystemPrompt(ctx)
	tools := []ai.Tool{b.reportTool(chat)}

	reply, err := b.assistant.Reply(ctx, system, tools, history, text)
	if err != nil {
		fmt.Println("Ошибка ответа Claude:", err)
		b.sendText(chat, "Не получилось ответить: "+err.Error())
		return
	}

	updated := append(history, ai.Turn{FromUser: true, Text: text}, ai.Turn{FromUser: false, Text: reply})
	if len(updated) > maxPrivateHistory {
		updated = updated[len(updated)-maxPrivateHistory:]
	}
	b.historyMu.Lock()
	b.history[sender] = updated
	b.historyMu.Unlock()

	if reply == "" {
		fmt.Println("Ассистент вернул пустой ответ — ничего не отправляю")
		return
	}
	fmt.Println("Ответ ассистента:", reply)
	b.sendText(chat, reply)
}

// buildAssistantSystemPrompt формирует системный промпт со сводкой по сбору
// за текущий месяц (по всем группам сразу) и с сегодняшней датой, чтобы
// ассистент правильно понимал относительные даты ("сегодня", "3 июля" и т.п.).
func (b *Bot) buildAssistantSystemPrompt(ctx context.Context) string {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var summaryText string
	summaries, err := b.db.SummaryForPeriod(ctx, from, from.AddDate(0, 1, 0))
	switch {
	case err != nil:
		summaryText = "(не удалось загрузить сводку: " + err.Error() + ")"
	case len(summaries) == 0:
		summaryText = "За текущий месяц данных пока нет."
	default:
		var sb strings.Builder
		var total float64
		for _, s := range summaries {
			fmt.Fprintf(&sb, "- %s: %.0f ₽ (%d платежей)\n", s.CanonicalName, s.Total, s.Count)
			total += s.Total
		}
		fmt.Fprintf(&sb, "Итого за месяц: %.0f ₽", total)
		summaryText = sb.String()
	}

	return "Ты — личный ассистент владельца WhatsApp-бота учёта финансов. Бот ведёт единый учёт " +
		"по всем WhatsApp-группам, в которых состоит его номер. Отвечай кратко и по делу, на русском языке.\n\n" +
		"Сегодняшняя дата: " + now.Format("02.01.2006") + ".\n\n" +
		"Сводка по сбору средств за текущий месяц (" + from.Format("02.01.2006") + " — " + now.Format("02.01.2006") + "):\n" +
		summaryText +
		"\n\nИспользуй эти данные для быстрых вопросов про текущий месяц. " +
		"Если владелец спрашивает про сбор за конкретные даты, диапазон дат или просит отчёт/документ за период " +
		"(например \"сколько собрали 3 и 4 июля\", \"скинь отчёт за неделю\", \"пришли пдф за июнь\") — " +
		"обязательно вызови инструмент send_finance_report с точными датами (переведи относительные даты и " +
		"названия месяцев в конкретные YYYY-MM-DD, используя сегодняшнюю дату как точку отсчёта). " +
		"Выбирай format=\"pdf\", если явно просят файл/документ/пдф/отчёт, и format=\"text\", если просто " +
		"спрашивают цифры в переписке. После вызова инструмента кратко прокомментируй результат своими словами."
}

// reportTool — инструмент Claude для отчёта за произвольный период. При
// format="pdf" сразу отправляет PDF-документ в чат и возвращает модели
// короткое подтверждение; при format="text" возвращает текстовую сводку,
// которую модель сама превратит в ответ пользователю.
func (b *Bot) reportTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "send_finance_report",
		Description: "Формирует отчёт по сбору средств за указанный период (по всем группам сразу) " +
			"и либо отправляет его как PDF-документ в чат, либо возвращает текстовую сводку для ответа. " +
			"Вызывай, когда пользователь спрашивает про суммы/сбор за конкретные даты или период, " +
			"или явно просит отчёт/документ.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"from_date": map[string]any{
					"type":        "string",
					"description": "Начало периода включительно, формат YYYY-MM-DD",
				},
				"to_date": map[string]any{
					"type":        "string",
					"description": "Конец периода включительно, формат YYYY-MM-DD (для одного дня равен from_date)",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []string{"pdf", "text"},
					"description": "\"pdf\" — прислать файл-документ; \"text\" — вернуть цифры для текстового ответа",
				},
			},
			"required": []string{"from_date", "to_date", "format"},
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
			return b.buildReportForAssistant(ctx, chat, args.FromDate, args.ToDate, args.Format)
		},
	}
}

// buildReportForAssistant — общая логика инструмента send_finance_report:
// достаёт сводку за период из БД и либо отправляет PDF, либо возвращает
// текст для финального ответа модели.
func (b *Bot) buildReportForAssistant(ctx context.Context, chat types.JID, fromStr, toStr, format string) (string, error) {
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		return "", fmt.Errorf("неверная дата начала периода %q, нужен формат YYYY-MM-DD", fromStr)
	}
	toDay, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		return "", fmt.Errorf("неверная дата конца периода %q, нужен формат YYYY-MM-DD", toStr)
	}
	to := toDay.AddDate(0, 0, 1) // конец периода включительно -> верхняя граница исключительно

	periodLabel := from.Format("02.01.2006") + " — " + toDay.Format("02.01.2006")

	summaries, err := b.db.SummaryForPeriod(ctx, from, to)
	if err != nil {
		return "", fmt.Errorf("ошибка при выборке данных: %w", err)
	}
	if len(summaries) == 0 {
		return fmt.Sprintf("За период %s данных нет.", periodLabel), nil
	}

	if format == "pdf" {
		outPath := fmt.Sprintf("%s/report_%s.pdf", b.reportDir, time.Now().Format("2006-01-02_15-04-05"))
		if err := report.Generate(summaries, periodLabel, b.fontDir, outPath); err != nil {
			return "", fmt.Errorf("ошибка генерации PDF: %w", err)
		}
		fileName := "Отчёт_" + from.Format("2006-01-02") + "_" + toDay.Format("2006-01-02") + ".pdf"
		b.sendDocument(chat, outPath, fileName)
		return "PDF-отчёт за " + periodLabel + " отправлен в чат.", nil
	}

	var sb strings.Builder
	var total float64
	for _, s := range summaries {
		fmt.Fprintf(&sb, "%s: %.0f ₽ (%d платежей)\n", s.CanonicalName, s.Total, s.Count)
		total += s.Total
	}
	fmt.Fprintf(&sb, "Итого за %s: %.0f ₽", periodLabel, total)
	return sb.String(), nil
}

// saveMediaFile сохраняет фото чека на диск для ручной проверки/аудита,
// возвращает путь к файлу или пустую строку при ошибке.
func (b *Bot) saveMediaFile(waMessageID string, data []byte) string {
	dir := filepath.Join(b.reportDir, "..", "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Println("Ошибка создания папки receipts:", err)
		return ""
	}
	path := filepath.Join(dir, waMessageID+".jpg")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Println("Ошибка сохранения фото:", err)
		return ""
	}
	return path
}

// handleBankReceipt разбирает распознанный текст скриншота банковского перевода
// и сохраняет его в bank_receipts. Если получателя не удалось уверенно
// сопоставить с известным контактом (алиас не найден и это выглядит как новое
// имя) — помечает needs_review = true, чтобы владелец мог проверить вручную.
func (b *Bot) handleBankReceipt(ctx context.Context, text string, rawID int, txDate time.Time) {
	rd := parser.ParseReceipt(text)

	if rd.Amount == 0 || rd.Recipient == "" {
		fmt.Printf("Чек (сообщение %d): не удалось распознать сумму/получателя, нужна ручная проверка\n", rawID)
		_ = b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
			RawMessageID: rawID,
			Bank:         rd.Bank,
			RecipientRaw: rd.Recipient,
			SenderRaw:    rd.Sender,
			Amount:       rd.Amount,
			Commission:   rd.Commission,
			DocNumber:    rd.DocNumber,
			AuthCode:     rd.AuthCode,
			Status:       rd.Status,
			NeedsReview:  true,
			TxDate:       txDate,
		})
		return
	}

	canonical := b.aliases.Resolve(rd.Recipient)
	needsReview := canonical == rd.Recipient // алиас не нашёлся — новое/незнакомое имя

	var contactIDPtr *int
	contactID, err := b.db.GetOrCreateContact(ctx, canonical)
	if err != nil {
		fmt.Println("Ошибка получения контакта для чека:", err)
	} else {
		contactIDPtr = &contactID
	}

	err = b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
		RawMessageID: rawID,
		Bank:         rd.Bank,
		RecipientRaw: rd.Recipient,
		SenderRaw:    rd.Sender,
		ContactID:    contactIDPtr,
		Amount:       rd.Amount,
		Commission:   rd.Commission,
		DocNumber:    rd.DocNumber,
		AuthCode:     rd.AuthCode,
		Status:       rd.Status,
		NeedsReview:  needsReview,
		TxDate:       txDate,
	})
	if err != nil {
		fmt.Println("Ошибка сохранения чека:", err)
	}
	if needsReview {
		fmt.Printf("Чек (сообщение %d): получатель %q не найден в списке известных — нужна проверка\n", rawID, rd.Recipient)
	}
}

func isReportCommand(text string) bool {
	switch text {
	case "/отчет", "/отчёт", "/report", "/итог":
		return true
	}
	return false
}

func (b *Bot) sendMonthlyReport(ctx context.Context, chat types.JID) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 1, 0)

	summaries, err := b.db.SummaryForPeriod(ctx, from, to)
	if err != nil {
		b.sendText(chat, "Ошибка при формировании отчёта: "+err.Error())
		return
	}
	if len(summaries) == 0 {
		b.sendText(chat, "За этот период пока нет данных для отчёта.")
		return
	}

	periodLabel := from.Format("02.01.2006") + " — " + now.Format("02.01.2006")
	outPath := fmt.Sprintf("%s/report_%s.pdf", b.reportDir, now.Format("2006-01-02_15-04-05"))

	if err := report.Generate(summaries, periodLabel, b.fontDir, outPath); err != nil {
		b.sendText(chat, "Ошибка генерации PDF: "+err.Error())
		return
	}

	b.sendDocument(chat, outPath, "Отчёт_"+now.Format("2006-01-02")+".pdf")
}

func (b *Bot) sendText(chat types.JID, text string) {
	_, err := b.client.SendMessage(context.Background(), chat, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		fmt.Println("Ошибка отправки сообщения:", err)
	}
}

func (b *Bot) sendDocument(chat types.JID, filePath, fileName string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Println("Ошибка чтения файла отчёта:", err)
		return
	}

	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaDocument)
	if err != nil {
		fmt.Println("Ошибка загрузки файла в WhatsApp:", err)
		return
	}

	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto.String(uploaded.URL),
			Mimetype:      proto.String("application/pdf"),
			Title:         proto.String(fileName),
			FileName:      proto.String(fileName),
			FileLength:    proto.Uint64(uploaded.FileLength),
			FileSHA256:    uploaded.FileSHA256,
			FileEncSHA256: uploaded.FileEncSHA256,
			MediaKey:      uploaded.MediaKey,
			DirectPath:    proto.String(uploaded.DirectPath),
		},
	}

	_, err = b.client.SendMessage(context.Background(), chat, msg)
	if err != nil {
		fmt.Println("Ошибка отправки документа:", err)
	}
}

// extractText вытаскивает текст из разных типов сообщений whatsmeow
// (обычный текст, "extended" текст с цитатой/форматированием, подпись к фото).
func extractText(m *waProto.Message) string {
	if m == nil {
		return ""
	}
	if m.GetConversation() != "" {
		return m.GetConversation()
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := m.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	return ""
}
