// Package bot подключается к WhatsApp через whatsmeow, слушает сообщения
// целевой группы, парсит их и сохраняет транзакции в БД. Также умеет
// отвечать на команду "/отчет" готовым PDF.
package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ocr"
	"whatsapp-bot/internal/parser"
	"whatsapp-bot/internal/report"
)

type Bot struct {
	client    *whatsmeow.Client
	db        *db.DB
	aliases   *parser.AliasMap
	ocr       ocr.Extractor
	groupJID  types.JID
	fontDir   string
	reportDir string
}

// New создаёт клиент whatsmeow. sessionDBPath — путь к SQLite-файлу сессии,
// сохраняется между рестартами, чтобы не сканировать QR каждый раз.
func New(ctx context.Context, sessionDBPath string, database *db.DB, aliases *parser.AliasMap, ocrClient ocr.Extractor, groupJIDStr, fontDir, reportDir string) (*Bot, error) {
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

	groupJID, err := types.ParseJID(groupJIDStr)
	if err != nil {
		return nil, fmt.Errorf("неверный group JID %q: %w", groupJIDStr, err)
	}

	b := &Bot{
		client:    client,
		db:        database,
		aliases:   aliases,
		ocr:       ocrClient,
		groupJID:  groupJID,
		fontDir:   fontDir,
		reportDir: reportDir,
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

func (b *Bot) handleEvent(evt interface{}) {
	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}
	if msg.Info.Chat != b.groupJID {
		return // сообщение не из целевой группы — игнорируем
	}

	ctx := context.Background()
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
