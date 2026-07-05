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
	"sort"
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
	"whatsapp-bot/internal/cmf"
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
	cmf           *cmf.Client        // nil => сверка с программой рассрочек выключена
	allowedGroups map[types.JID]bool // пусто/nil => разрешены все группы, в которых состоит номер
	ownerNumbers  map[string]bool    // пусто/nil => ассистент в личке отвечает всем; иначе только этим номерам
	reportAdmins  map[string]bool    // номера, которым доступны отчёты/суммы; остальным — вежливый отказ
	botName       string             // обращение в группах: "Джарвис скинь отчет"
	fontDir       string
	reportDir     string

	historyMu sync.Mutex
	history   map[string][]ai.Turn // sender JID -> личная переписка с ассистентом

	// Чеки, присланные в личку и ещё не сохранённые в учёт: владелец сначала
	// кидает фото, потом говорит "запомни их для группы такой-то" (дозагрузка
	// пропущенных дней, когда бота ещё не было в группе).
	pendingMu sync.Mutex
	pending   map[string][]pendingReceipt // chat JID -> распознанные чеки

	// Кэш списка групп (JID -> название), чтобы ассистент мог работать
	// с группами по-человечески ("группа сб оплата клиентов").
	groupsMu      sync.Mutex
	groupsCache   map[types.JID]string
	groupsFetched time.Time

	// ID последних сообщений, отправленных самим ботом в каждый чат —
	// чтобы по просьбе "удали своё сообщение" бот мог их отозвать.
	sentMu   sync.Mutex
	sentMsgs map[string][]string // chat JID -> последние отправленные IDs

	// Очередь "имён без чека" (FIFO): ФИО клиентов, написанные ПЕРЕД чеками.
	// Ключ groupJID|senderJID. Очередь нужна, когда имена и чеки идут пачками
	// в любом чередовании — пары строятся по порядку сообщений.
	pendingNameMu sync.Mutex
	pendingNames  map[string][]pendingName

	// Проактивные вопросы "чей это чек" и привязка ответов к чекам.
	clarify *clarifyState
}

// pendingName — ФИО, написанное отправителем, пока без чека.
type pendingName struct {
	name string
	at   time.Time
}

// pendingReceipt — распознанный чек из лички, ожидающий команды "запомнить".
type pendingReceipt struct {
	rawID      int
	rd         parser.ReceiptData
	receivedAt time.Time
}

// New создаёт клиент whatsmeow. sessionDBPath — путь к SQLite-файлу сессии,
// сохраняется между рестартами, чтобы не сканировать QR каждый раз.
// assistant может быть nil — тогда бот не отвечает в личных сообщениях
// (например, если не задан OPENROUTER_API_KEY). groupJIDs — список JID групп,
// которые нужно учитывать; пустой список означает "все группы, в которых
// состоит номер бота". botName — имя, по которому к боту обращаются в группах.
// ownerNumbers — номера (только цифры), которым разрешена личка с ассистентом;
// пустой список = отвечаем всем. reportAdmins — номера, которым доступна
// отчётность (суммы, сборы, отчёты); остальным по этим вопросам — вежливый
// отказ, но обычное общение и поиск конкретного чека доступны.
func New(ctx context.Context, sessionDBPath string, database *db.DB, aliases *parser.AliasMap, ocrClient ocr.Extractor, assistant *ai.Assistant, cmfClient *cmf.Client, groupJIDs []string, ownerNumbers, reportAdmins []string, botName, fontDir, reportDir string) (*Bot, error) {
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

	var owners map[string]bool
	if len(ownerNumbers) > 0 {
		owners = make(map[string]bool, len(ownerNumbers))
		for _, n := range ownerNumbers {
			owners[normalizePhone(n)] = true
		}
	}
	admins := make(map[string]bool, len(reportAdmins))
	for _, n := range reportAdmins {
		admins[normalizePhone(n)] = true
	}

	b := &Bot{
		client:        client,
		db:            database,
		aliases:       aliases,
		ocr:           ocrClient,
		assistant:     assistant,
		cmf:           cmfClient,
		allowedGroups: allowedGroups,
		ownerNumbers:  owners,
		reportAdmins:  admins,
		botName:       botName,
		fontDir:       fontDir,
		reportDir:     reportDir,
		history:       make(map[string][]ai.Turn),
		pending:       make(map[string][]pendingReceipt),
		sentMsgs:      make(map[string][]string),
		pendingNames:  make(map[string][]pendingName),
		clarify:       newClarifyState(),
	}

	client.AddEventHandler(b.handleEvent)
	go b.cmfWatcherLoop() // сверка чеков с программой рассрочек (no-op, если cmf == nil)
	go b.clarifyLoop()    // проактивные вопросы "чей это чек"
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
	if _, ok := evt.(*events.Connected); ok {
		// Отмечаемся "онлайн" — без этого WhatsApp не показывает собеседникам
		// ни статус "печатает…", ни имя бота (pushname).
		if err := b.client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
			fmt.Println("Не удалось отправить presence:", err)
		}
		// Печатаем группы с JID — удобно для настройки WHATSAPP_GROUP_JIDS.
		go func() {
			for jid, name := range b.joinedGroups(context.Background()) {
				fmt.Printf("Группа: %q — %s\n", name, jid)
			}
		}()
		return
	}

	msg, ok := evt.(*events.Message)
	if !ok {
		return
	}
	ctx := context.Background()

	// Собственные сообщения бота (в т.ч. пересланные им чеки) не обрабатываем —
	// они записываются в учёт напрямую при пересылке; иначе возможны петли.
	if msg.Info.IsFromMe {
		return
	}

	// Статусы (сторис) и каналы — не чаты. Чужие статусы приходят как
	// сообщения из status@broadcast; если на них "ответить", ответ
	// ПУБЛИКУЕТСЯ КАК СТАТУС БОТА — поэтому игнорируем их полностью.
	if msg.Info.Chat.Server == types.BroadcastServer || msg.Info.Chat.Server == types.NewsletterServer {
		return
	}

	// Пользователь удалил сообщение ("Удалить у всех") — если к нему были
	// привязаны платежи/чеки, они выпадают из учёта, а бот подтверждает
	// это в чате, чтобы было видно, что отчёт обновился.
	if prot := msg.Message.GetProtocolMessage(); prot != nil && prot.GetType() == waProto.ProtocolMessage_REVOKE {
		revokedID := prot.GetKey().GetID()
		txN, rcN, err := b.db.MarkMessageDeleted(ctx, revokedID)
		if err != nil {
			fmt.Println("Ошибка обработки удалённого сообщения:", err)
			return
		}
		if txN+rcN > 0 {
			fmt.Printf("Сообщение %s удалено — из учёта убрано платежей: %d, чеков: %d\n", revokedID, txN, rcN)
			b.sendText(msg.Info.Chat, fmt.Sprintf(
				"🗑 Сообщение удалено — убрал из учёта связанные записи (платежей: %d, чеков: %d). Отчёты уже без них.", txN, rcN))
		}
		return
	}

	if !msg.Info.IsGroup {
		// Личный чат с номером бота — если настроен ассистент, отвечаем.
		// В отдельной горутине: ответ ИИ может занимать десятки секунд,
		// и он не должен блокировать разбор сообщений из групп.
		fmt.Printf("Личное сообщение от %s (chat=%s, fromMe=%v): %q\n",
			msg.Info.Sender, msg.Info.Chat, msg.Info.IsFromMe, extractText(msg.Message))
		if b.ownerNumbers != nil && !b.ownerNumbers[msg.Info.Sender.User] {
			fmt.Println("Отправитель не в списке OWNER_NUMBERS — личка проигнорирована")
			return
		}
		if b.assistant != nil {
			go b.handlePrivateMessage(ctx, msg)
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

	caption := extractText(msg.Message)

	var (
		text       string
		hasMedia   bool
		mediaPath  string
		mediaBytes []byte
		mediaExt   string
	)

	// Чеки приходят фотографиями, PDF-документами (Сбер/ВТБ/РСХБ шлют PDF)
	// и фото, отправленными "как файл" (без сжатия) — все типы скачиваем
	// и распознаём в текст.
	imgMsg := msg.Message.GetImageMessage()
	docMsg := msg.Message.GetDocumentMessage()
	isPDF := docMsg != nil && isPDFDocument(docMsg.GetMimetype(), docMsg.GetFileName())
	isImgDoc := docMsg != nil && !isPDF && strings.HasPrefix(strings.ToLower(docMsg.GetMimetype()), "image/")

	switch {
	case isImgDoc:
		hasMedia = true
		mediaExt = ".jpg"
		imgBytes, err := b.client.Download(ctx, docMsg)
		if err != nil {
			fmt.Println("Ошибка скачивания фото-файла:", err)
			text = caption
		} else {
			mediaBytes = imgBytes
			mediaPath = b.saveMediaFile(msg.Info.ID, imgBytes, mediaExt)
			ocrText, err := b.ocr.ExtractText(ctx, imgBytes)
			if err != nil {
				fmt.Println("Ошибка OCR фото-файла:", err)
				text = caption
			} else {
				text = ocrText
				if caption != "" {
					text = caption + "\n" + ocrText
				}
			}
		}
	case imgMsg != nil:
		hasMedia = true
		mediaExt = ".jpg"
		imgBytes, err := b.client.Download(ctx, imgMsg)
		if err != nil {
			fmt.Println("Ошибка скачивания фото:", err)
			text = caption // сохраняем хотя бы подпись, если она есть
		} else {
			mediaBytes = imgBytes
			mediaPath = b.saveMediaFile(msg.Info.ID, imgBytes, mediaExt)
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
	case isPDF:
		hasMedia = true
		mediaExt = ".pdf"
		pdfBytes, err := b.client.Download(ctx, docMsg)
		if err != nil {
			fmt.Println("Ошибка скачивания PDF:", err)
			text = caption
		} else {
			mediaBytes = pdfBytes
			mediaPath = b.saveMediaFile(msg.Info.ID, pdfBytes, mediaExt)
			pdfText, err := b.extractPDFText(ctx, pdfBytes)
			if err != nil {
				fmt.Println("Ошибка извлечения текста из PDF:", err)
				text = caption
			} else {
				text = pdfText
				if caption != "" {
					text = caption + "\n" + pdfText
				}
			}
		}
	default:
		text = extractText(msg.Message)
	}

	if text == "" && !hasMedia {
		return
	}

	// Пересылка чеков между чатами по активным правилам ("все чеки из группы
	// оплата клиентов скидывай в оплата клнт") — в фоне, не мешая учёту.
	if hasMedia && mediaBytes != nil {
		go b.applyForwardRules(context.Background(), msg.Info.Chat.String(), senderName, msg.Info.ID, mediaBytes, mediaExt, text, msg.Info.Timestamp)
	}

	// Обращение к боту по имени ("Джарвис скинь отчет") или ответом (реплаем)
	// на его сообщение — отвечаем ассистентом прямо в группе, в учёт такое
	// сообщение не идёт. В фоне, чтобы не блокировать разбор остальных.
	if !hasMedia && b.assistant != nil {
		if query, ok := b.stripBotName(text); ok {
			go b.handleGroupAssistant(context.Background(), msg, query)
			return
		}
		if b.isReplyToBot(msg) {
			go b.handleGroupAssistant(context.Background(), msg, text)
			return
		}
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
		// OCR не дал текста вообще. Последний шанс — Claude смотрит на само
		// изображение: handleBankReceipt с пустым текстом сразу уйдёт в
		// вижн-фолбэк. В фоне, т.к. вижн-запрос занимает секунды.
		if mediaBytes != nil && b.assistant != nil {
			mb, me, chatJID, ts := mediaBytes, mediaExt, msg.Info.Chat, msg.Info.Timestamp
			sender := msg.Info.Sender.String()
			waMsgID := msg.Info.ID
			payer := b.resolveReceiptPayer(ctx, msg, caption)
			go b.handleBankReceipt(context.Background(), chatJID, sender, waMsgID, "", rawID, ts, mb, me, payer)
			return
		}
		fmt.Printf("Фото без распознанного текста (сообщение %d), нужна ручная проверка: %s\n", rawID, mediaPath)
		return
	}

	// Любое медиа (фото/PDF/фото-как-файл) в рабочей группе трактуем как чек
	// и отдаём в каскад разбора: парсер полей -> ИИ по тексту -> Claude
	// смотрит на само изображение. Раньше сюда попадали только те, где OCR
	// выдал знакомые слова ("получатель", "сумма перевода"); если Tesseract
	// на кириллице выдавал кашу, чёткий чек проваливался в разбор текста и
	// терялся. Теперь распознаётся даже при плохом OCR — за счёт вижна.
	if hasMedia {
		// Чей это чек — берём из ФИО, которое написали РЯДОМ с чеком:
		// подпись к фото, текст ответом (свайп) или отдельное сообщение
		// с именем прямо перед чеком. Это важнее, чем получатель на самом
		// чеке (там может быть владелец карты, а не клиент).
		payer := b.resolveReceiptPayer(ctx, msg, caption)
		b.handleBankReceipt(ctx, msg.Info.Chat, msg.Info.Sender.String(), msg.Info.ID, text, rawID, msg.Info.Timestamp, mediaBytes, mediaExt, payer)
		return
	}

	// Ответ (свайп) на вопрос бота "чей это чек" — привязываем клиента к
	// тому чеку и дальше не разбираем это сообщение.
	if b.handleClarifyReply(ctx, msg, text) {
		return
	}

	// Сообщение-имя (ФИО без чека): по порядку сообщений привязываем к чеку
	// (до или после), либо ставим в очередь для будущего чека. Если это имя —
	// в учёт как платёж оно не идёт.
	if b.handleNameMessage(ctx, msg, text) {
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
		// Строки с цифрами (возможные платежи в незнакомом формате) отдаём
		// на доразбор ИИ — в фоне, чтобы не тормозить остальные сообщения.
		if b.assistant != nil && containsDigit(result.Unparsed) {
			unparsed := append([]string(nil), result.Unparsed...)
			go b.aiRescueUnparsed(context.Background(), msg.Info.Chat, senderName, unparsed, rawID, msg.Info.Timestamp)
		}
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
	imgMsg := msg.Message.GetImageMessage()
	docMsg := msg.Message.GetDocumentMessage()
	isPDF := docMsg != nil && isPDFDocument(docMsg.GetMimetype(), docMsg.GetFileName())
	isImgDoc := docMsg != nil && !isPDF && strings.HasPrefix(strings.ToLower(docMsg.GetMimetype()), "image/")

	if text == "" && imgMsg == nil && !isPDF && !isImgDoc {
		fmt.Println("Личное сообщение без текста и вложений (стикер/реакция?) — пропускаю")
		return
	}

	chat := msg.Info.Chat

	// Показываем "печатает…", пока распознаём вложение и ждём ответ ИИ —
	// чтобы было видно, что бот работает, а не завис.
	stopTyping := b.startTyping(chat)
	defer stopTyping()

	// Вложение в личке (фото или PDF-чек): распознаём и отдаём ассистенту
	// как контекст. Сразу в учёт НЕ добавляем (источник учёта — рабочие
	// группы), но запоминаем как "ожидающий" чек: если владелец скажет
	// "запомни их для группы такой-то", ассистент сохранит их инструментом
	// save_pending_receipts.
	if imgMsg != nil || isPDF || isImgDoc {
		var (
			mediaBytes []byte
			mediaText  string
			mediaExt   string
			err        error
		)
		switch {
		case imgMsg != nil:
			mediaExt = ".jpg"
			mediaBytes, err = b.client.Download(ctx, imgMsg)
			if err == nil {
				mediaText, err = b.ocr.ExtractText(ctx, mediaBytes)
			}
		case isImgDoc:
			// Фото, отправленное "как файл" (без сжатия).
			mediaExt = ".jpg"
			mediaBytes, err = b.client.Download(ctx, docMsg)
			if err == nil {
				mediaText, err = b.ocr.ExtractText(ctx, mediaBytes)
			}
		default:
			mediaExt = ".pdf"
			mediaBytes, err = b.client.Download(ctx, docMsg)
			if err == nil {
				mediaText, err = b.extractPDFText(ctx, mediaBytes)
			}
		}
		if err != nil {
			fmt.Println("Ошибка обработки вложения из лички:", err)
		}

		// Пересылка чеков из лички по правилу source='dm' ("все чеки, что
		// мне присылают, скидывай в группу такую-то").
		if mediaBytes != nil && strings.TrimSpace(mediaText) != "" {
			senderName := msg.Info.PushName
			if senderName == "" {
				senderName = msg.Info.Sender.User
			}
			go b.applyForwardRules(context.Background(), "dm", senderName, msg.Info.ID, mediaBytes, mediaExt, mediaText, msg.Info.Timestamp)
		}

		receiptCtx, summary := "", ""
		if mediaBytes != nil {
			receiptCtx, summary = b.describePrivateMedia(ctx, msg, mediaBytes, mediaText, mediaExt)
		}

		// Вложение без подписи — тихий приём: короткое подтверждение без
		// вызова ИИ. Так можно массово переслать десятки/сотни старых чеков
		// с другого телефона, не получая развёрнутый ответ на каждый.
		if text == "" {
			if summary != "" {
				b.pendingMu.Lock()
				waiting := len(b.pending[chat.String()])
				b.pendingMu.Unlock()
				b.sendText(chat, fmt.Sprintf("📥 Чек распознан: %s. Ожидают сохранения: %d. Когда пришлёшь все — напиши, для какой группы их запомнить.", summary, waiting))
			} else {
				b.sendText(chat, "Не смог распознать чек в этом вложении — попробуй прислать более чёткий файл или фото.")
			}
			return
		}

		if receiptCtx != "" {
			text += "\n\n" + receiptCtx
		}
	}

	if text == "" {
		return
	}

	sender := msg.Info.Sender.String()
	fmt.Println("Спрашиваю ассистента (OpenRouter)...")

	b.historyMu.Lock()
	history := append([]ai.Turn(nil), b.history[sender]...)
	b.historyMu.Unlock()

	isAdmin := b.isReportAdmin(msg.Info)
	system := b.buildAssistantSystemPromptFor(ctx, isAdmin)
	tools := b.assistantTools(ctx, chat, isAdmin, false)

	// Если это ответ (свайп) на чужое сообщение — подскажем номер его
	// отправителя, чтобы сработали команды памяти ("запомни этот номер").
	text += quotedSenderPhoneNote(msg)

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

// normalizePhone приводит номер к формату WhatsApp: без плюса и пробелов,
// российский "8XXXXXXXXXX" превращается в "7XXXXXXXXXX".
func normalizePhone(n string) string {
	n = strings.TrimPrefix(strings.TrimSpace(n), "+")
	n = strings.ReplaceAll(n, " ", "")
	n = strings.ReplaceAll(n, "-", "")
	if len(n) == 11 && strings.HasPrefix(n, "8") {
		n = "7" + n[1:]
	}
	return n
}

// isReportAdmin проверяет, входит ли отправитель в список тех, кому доступна
// отчётность. Сверяем и основной JID, и альтернативный (WhatsApp может
// подставлять скрытый LID вместо номера телефона).
func (b *Bot) isReportAdmin(info types.MessageInfo) bool {
	if len(b.reportAdmins) == 0 {
		return true // список не задан — ограничение выключено
	}
	return b.reportAdmins[info.Sender.User] || b.reportAdmins[info.SenderAlt.User]
}

// assistantTools собирает набор инструментов ассистента с учётом прав:
// финансовые инструменты доступны только админам отчётности.
// groupDefault — JID группы для корректировок (в личке пусто),
// inGroup — вызов из группы (там нет дозагрузки чеков).
func (b *Bot) assistantTools(ctx context.Context, chat types.JID, isAdmin, inGroup bool) []ai.Tool {
	if !isAdmin {
		// Не-админам — только поиск по конкретному человеку (проверить,
		// прошёл ли чек) и никакой сводной отчётности.
		return []ai.Tool{b.personTool()}
	}
	groupDefault := ""
	if inGroup {
		groupDefault = chat.String()
	}
	tools := []ai.Tool{
		b.reportTool(chat),
		b.sendersTool(ctx, chat),
		b.correctionTool(groupDefault),
		b.personTool(),
		b.customPDFTool(chat),
		b.deleteMessagesTool(chat),
		b.forwardingTool(),
		b.unclearTool(),
		b.sendUnclearFileTool(chat),
		b.sendClientReceiptTool(chat),
		b.receiptDetailsTool(),
		b.fixReceiptTool(),
		b.recountTool(),
		b.phoneMemoryTool(chat),
		b.assignReceiptTool(chat),
	}
	if b.cmf != nil {
		tools = append(tools, b.cmfStatusTool(), b.cmfAddPaymentTool(), b.cmfResolveTool(), b.cmfBranchTool())
	}
	if !inGroup {
		tools = append(tools, b.savePendingTool(chat))
	}
	return tools
}

// isReplyToBot определяет, является ли сообщение ответом (реплаем/цитатой)
// на сообщение самого бота — такой ответ считается обращением к боту,
// имя писать не обязательно.
func (b *Bot) isReplyToBot(msg *events.Message) bool {
	ext := msg.Message.GetExtendedTextMessage()
	if ext == nil {
		return false
	}
	ci := ext.GetContextInfo()
	if ci == nil || ci.GetParticipant() == "" {
		return false
	}
	quoted, err := types.ParseJID(ci.GetParticipant())
	if err != nil {
		return false
	}
	if own := b.client.Store.ID; own != nil && quoted.User == own.User {
		return true
	}
	if lid := b.client.Store.LID; !lid.IsEmpty() && quoted.User == lid.User {
		return true
	}
	return false
}

// stripBotName проверяет, начинается ли сообщение с имени бота
// ("Джарвис скинь отчет", "джарвис, какой сбор?"), и возвращает текст
// без обращения. Сравнение по рунам без учёта регистра.
func (b *Bot) stripBotName(text string) (string, bool) {
	if b.botName == "" {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	name := []rune(b.botName)
	if len(runes) < len(name) {
		return "", false
	}
	if !strings.EqualFold(string(runes[:len(name)]), b.botName) {
		return "", false
	}
	rest := strings.TrimLeft(string(runes[len(name):]), " \t,:;!?-—.")
	// "Джарвисом" и т.п. — не обращение: после имени должна идти граница слова.
	if len(runes) > len(name) {
		next := runes[len(name)]
		if next != ' ' && next != ',' && next != ':' && next != ';' && next != '!' && next != '?' && next != '-' && next != '—' && next != '.' && next != '\t' {
			return "", false
		}
	}
	if rest == "" {
		rest = "Привет!"
	}
	return rest, true
}

// handleGroupAssistant отвечает на обращение к боту по имени прямо в группе.
// История такого диалога общая на группу (ключ — JID группы).
func (b *Bot) handleGroupAssistant(ctx context.Context, msg *events.Message, query string) {
	chat := msg.Info.Chat

	stopTyping := b.startTyping(chat)
	defer stopTyping()

	key := chat.String()
	b.historyMu.Lock()
	history := append([]ai.Turn(nil), b.history[key]...)
	b.historyMu.Unlock()

	isAdmin := b.isReportAdmin(msg.Info)
	curGroupName := chat.String()
	if groups := b.joinedGroups(ctx); groups[chat] != "" {
		curGroupName = groups[chat]
	}
	system := b.buildAssistantSystemPromptFor(ctx, isAdmin) +
		"\n\nСейчас к тебе обратились ПРЯМО В РАБОЧЕЙ ГРУППЕ «" + curGroupName + "» — твой ответ увидят все участники. " +
		"Отвечай коротко и по делу. Если владелец говорит 'здесь', 'в этой группе', 'в эту группу', 'вот тут' — " +
		"это про ЭТУ группу («" + curGroupName + "»), сразу используй её как group в инструментах, НЕ переспрашивай."

	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = msg.Info.Sender.User
	}
	userText := senderName + ": " + query + quotedSenderPhoneNote(msg)

	tools := b.assistantTools(ctx, chat, isAdmin, true)

	reply, err := b.assistant.Reply(ctx, system, tools, history, userText)
	if err != nil {
		fmt.Println("Ошибка ответа ассистента в группе:", err)
		b.sendText(chat, "Не получилось ответить: "+err.Error())
		return
	}

	updated := append(history, ai.Turn{FromUser: true, Text: userText}, ai.Turn{FromUser: false, Text: reply})
	if len(updated) > maxPrivateHistory {
		updated = updated[len(updated)-maxPrivateHistory:]
	}
	b.historyMu.Lock()
	b.history[key] = updated
	b.historyMu.Unlock()

	if reply != "" {
		b.sendText(chat, reply)
	}
}

// joinedGroups возвращает список групп бота (JID -> название) с кэшем на
// 5 минут, чтобы не дёргать WhatsApp на каждое сообщение.
func (b *Bot) joinedGroups(ctx context.Context) map[types.JID]string {
	b.groupsMu.Lock()
	defer b.groupsMu.Unlock()

	if b.groupsCache != nil && time.Since(b.groupsFetched) < 5*time.Minute {
		return b.groupsCache
	}
	groups, err := b.client.GetJoinedGroups(ctx)
	if err != nil {
		fmt.Println("Не удалось получить список групп:", err)
		if b.groupsCache != nil {
			return b.groupsCache // отдаём устаревший кэш, это лучше, чем ничего
		}
		return map[types.JID]string{}
	}
	cache := make(map[types.JID]string, len(groups))
	for _, g := range groups {
		cache[g.JID] = g.Name
	}
	b.groupsCache = cache
	b.groupsFetched = time.Now()
	return cache
}

// resolveGroup ищет группу по названию (без учёта регистра, по подстроке).
// Возвращает JID и точное название. Если совпадений нет или их несколько —
// ошибка с пояснением, чтобы ассистент мог переспросить владельца.
func (b *Bot) resolveGroup(ctx context.Context, name string) (types.JID, string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return types.JID{}, "", fmt.Errorf("название группы пустое")
	}
	var (
		foundJID  types.JID
		foundName string
		matches   []string
	)
	for jid, gname := range b.joinedGroups(ctx) {
		if strings.Contains(strings.ToLower(gname), name) {
			foundJID, foundName = jid, gname
			matches = append(matches, gname)
		}
	}
	switch len(matches) {
	case 0:
		return types.JID{}, "", fmt.Errorf("группа %q не найдена среди групп бота", name)
	case 1:
		return foundJID, foundName, nil
	default:
		return types.JID{}, "", fmt.Errorf("под %q подходит несколько групп: %s — уточни название", name, strings.Join(matches, "; "))
	}
}

// resolveGroups разбирает описание из ОДНОЙ или НЕСКОЛЬКИХ групп (через
// запятую / "и" / ";") и возвращает их JID-ы и общий ярлык. Пусто -> nil
// (значит все группы). Для запросов "по двум группам сразу".
func (b *Bot) resolveGroups(ctx context.Context, spec string) (jids []string, label string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, "все группы", nil
	}
	repl := strings.NewReplacer(";", ",", " и ", ",", " плюс ", ",", "+", ",")
	parts := strings.Split(repl.Replace(spec), ",")
	var names []string
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		jid, name, e := b.resolveGroup(ctx, p)
		if e != nil {
			return nil, "", e
		}
		if !seen[jid.String()] {
			seen[jid.String()] = true
			jids = append(jids, jid.String())
			names = append(names, name)
		}
	}
	if len(jids) == 0 {
		return nil, "все группы", nil
	}
	return jids, strings.Join(names, " + "), nil
}

// startTyping включает в чате статус "печатает…" и обновляет его каждые
// несколько секунд (WhatsApp сам скрывает статус по таймауту ~10 секунд,
// а ответ ИИ может занимать заметно дольше). Возвращает функцию остановки,
// которая убирает статус — вызывать через defer.
func (b *Bot) startTyping(chat types.JID) func() {
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.client.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
		fmt.Println("Не удалось включить статус 'печатает…':", err)
	}
	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = b.client.SendChatPresence(context.Background(), chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
			}
		}
	}()
	return func() {
		cancel()
		_ = b.client.SendChatPresence(context.Background(), chat, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}
}

// describePrivateMedia разбирает уже скачанный файл из личного сообщения
// (фото или PDF) с уже извлечённым текстом и, если это банковский чек,
// возвращает разобранную информацию (плюс результат проверки на дубль)
// в виде текстового блока для контекста ассистента. Распознанный чек
// дополнительно запоминается как "ожидающий" (pending) — на случай, если
// владелец попросит добавить его в учёт.
// Второе возвращаемое значение — короткая сводка ("Милана, 29 400 ₽ от
// 02.07.2026") для тихого подтверждения при массовой пересылке; пустая
// строка, если чек не распознался.
func (b *Bot) describePrivateMedia(ctx context.Context, msg *events.Message, mediaBytes []byte, ocrText, ext string) (string, string) {
	ocrText = strings.TrimSpace(ocrText)
	if ocrText == "" {
		return "[Пользователь прислал файл без распознаваемого текста.]", ""
	}

	if !parser.LooksLikeBankReceipt(ocrText) {
		return "[Пользователь прислал файл. Распознанный текст]:\n" + ocrText, ""
	}

	rd := parser.ParseReceipt(ocrText)

	// Парсер не справился — пробуем доразобрать через ИИ по тексту,
	// а если и это не помогло — Claude смотрит на само изображение.
	if rd.Amount == 0 || rd.Recipient == "" {
		if rec, ok := b.aiRescueReceipt(ctx, ocrText); ok {
			mergeAIReceipt(&rd, rec)
		}
	}
	if rd.Amount == 0 || rd.Recipient == "" {
		if rec, ok := b.aiVisionReceipt(ctx, mediaBytes, ext); ok {
			mergeAIReceipt(&rd, rec)
		}
	}

	// Сохраняем сообщение в БД (нужен id для возможной дозагрузки в учёт)
	// и файл на диск, затем запоминаем чек как ожидающий команду владельца.
	if rd.Amount > 0 && rd.Recipient != "" {
		mediaPath := b.saveMediaFile(msg.Info.ID, mediaBytes, ext)
		senderName := msg.Info.PushName
		if senderName == "" {
			senderName = msg.Info.Sender.User
		}
		rawID, err := b.db.SaveRawMessage(ctx, msg.Info.ID, msg.Info.Chat.String(), msg.Info.Sender.String(),
			senderName, ocrText, true, mediaPath, msg.Info.Timestamp)
		if err != nil {
			fmt.Println("Ошибка сохранения фото-сообщения из лички:", err)
		} else {
			chatKey := msg.Info.Chat.String()
			b.pendingMu.Lock()
			b.pending[chatKey] = append(b.pending[chatKey], pendingReceipt{
				rawID:      rawID,
				rd:         rd,
				receivedAt: msg.Info.Timestamp,
			})
			// Защита от бесконечного накопления. Лимит с запасом на массовую
			// пересылку истории чеков с другого телефона.
			if len(b.pending[chatKey]) > 300 {
				b.pending[chatKey] = b.pending[chatKey][len(b.pending[chatKey])-300:]
			}
			b.pendingMu.Unlock()
		}
	}
	var sb strings.Builder
	sb.WriteString("[Пользователь прислал фото банковского чека. Распознанные данные]:\n")
	if rd.Bank != "" {
		fmt.Fprintf(&sb, "Банк: %s\n", rd.Bank)
	}
	if rd.Recipient != "" {
		canonical, matched := b.aliases.ResolveName(rd.Recipient)
		if matched {
			fmt.Fprintf(&sb, "Получатель: %s (это %s)\n", rd.Recipient, canonical)
		} else {
			fmt.Fprintf(&sb, "Получатель: %s\n", rd.Recipient)
		}
	}
	if rd.Sender != "" {
		fmt.Fprintf(&sb, "Отправитель: %s\n", rd.Sender)
	}
	if rd.Amount > 0 {
		fmt.Fprintf(&sb, "Сумма: %.2f ₽\n", rd.Amount)
	}
	if rd.HasTxTime {
		fmt.Fprintf(&sb, "Дата и время операции: %s\n", rd.TxTime.Format("02.01.2006 15:04:05"))
	}
	if rd.DocNumber != "" {
		fmt.Fprintf(&sb, "Номер документа: %s\n", rd.DocNumber)
	}

	// Проверяем, есть ли такой чек уже в учёте (из рабочих групп).
	if rd.Amount > 0 && rd.Recipient != "" {
		txTime := rd.TxTime
		if !rd.HasTxTime {
			txTime = time.Now()
		}
		var contactIDPtr *int
		if canonical, matched := b.aliases.ResolveName(rd.Recipient); matched {
			if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
				contactIDPtr = &id
			}
		}
		// Информационная проверка — по всем группам сразу (groupJID пустой).
		isDup, dupTime, err := b.db.FindDuplicateReceipt(ctx, "", rd.DocNumber, rd.AuthCode, contactIDPtr, rd.Recipient, rd.Amount, txTime)
		switch {
		case err != nil:
			fmt.Println("Ошибка проверки дубля чека из лички:", err)
		case isDup:
			fmt.Fprintf(&sb, "Статус в учёте: этот чек УЖЕ УЧТЁН в базе (запись от %s).\n", dupTime.Format("02.01.2006 15:04"))
		default:
			sb.WriteString("Статус в учёте: такого чека в базе НЕТ — он не проходил через рабочие группы.\n")
		}
	}
	sb.WriteString("(Чек запомнен как ожидающий. В учёт он попадёт, только если владелец попросит сохранить — " +
		"тогда вызови инструмент save_pending_receipts с названием группы, к которой относятся чеки.)")

	summary := ""
	if rd.Amount > 0 && rd.Recipient != "" {
		name, _ := b.aliases.ResolveName(rd.Recipient)
		summary = fmt.Sprintf("%s, %.0f ₽", name, rd.Amount)
		if rd.HasTxTime {
			summary += ", операция " + rd.TxTime.Format("02.01.2006 15:04")
		}
	}
	return sb.String(), summary
}

// accessNote — вставка в системный промпт о правах текущего собеседника.
func accessNote(isAdmin bool) string {
	if isAdmin {
		return "\n\nСейчас с тобой говорит ВЛАДЕЛЕЦ (админ отчётности) — ему доступно всё: отчёты, суммы, статистика, корректировки."
	}
	return "\n\nВАЖНО: сейчас с тобой говорит НЕ владелец. Финансовая отчётность конфиденциальна. " +
		"На вопросы про общий сбор, суммы, отчёты, статистику отправителей, PDF-отчёты — вежливо отказывай: " +
		"«Извини, не могу с этим помочь — это конфиденциальная информация». " +
		"Что МОЖНО: свободно общаться на общие темы, помогать с вопросами не про деньги, " +
		"и проверить чеки конкретного человека через person_report (например, человек хочет убедиться, что его " +
		"чек прошёл). Не выполняй по просьбе не-владельца корректировки, удаления и настройку пересылки — " +
		"таких инструментов у тебя сейчас и нет."
}

// buildAssistantSystemPromptFor — промпт с учётом прав собеседника: для
// не-админов сводка по деньгам в контекст вообще не кладётся.
func (b *Bot) buildAssistantSystemPromptFor(ctx context.Context, isAdmin bool) string {
	if isAdmin {
		return b.buildAssistantSystemPrompt(ctx) + accessNote(true)
	}

	now := time.Now()
	peopleList := "(не удалось загрузить)"
	if contacts, err := b.db.ListContacts(ctx); err == nil && len(contacts) > 0 {
		peopleList = strings.Join(contacts, ", ")
	}
	return "Ты — ассистент WhatsApp-бота учёта финансов по имени " + b.botName + ". " +
		"Общайся дружелюбно и по-человечески, на русском.\n\n" +
		"Сегодняшняя дата: " + now.Format("2006-01-02") + " (" + now.Format("02.01.2006") + ").\n\n" +
		"Известные люди в учёте: " + peopleList + ". Пользователь может писать имена с опечатками — " +
		"сопоставь с ближайшим известным именем.\n\n" +
		"Твой единственный инструмент — person_report: проверить чеки/платежи конкретного человека." +
		accessNote(false)
}

// buildAssistantSystemPrompt формирует системный промпт со сводкой по сбору
// за текущий месяц (по всем группам сразу) и с сегодняшней датой, чтобы
// ассистент правильно понимал относительные даты ("сегодня", "3 июля" и т.п.).
func (b *Bot) buildAssistantSystemPrompt(ctx context.Context) string {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var summaryText string
	summaries, err := b.db.SummaryForPeriod(ctx, from, from.AddDate(0, 1, 0), nil)
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

	groupsList := "(не удалось получить список групп)"
	if groups := b.joinedGroups(ctx); len(groups) > 0 {
		var names []string
		for _, name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		groupsList = strings.Join(names, "; ")
	}

	peopleList := "(не удалось загрузить)"
	if contacts, err := b.db.ListContacts(ctx); err == nil && len(contacts) > 0 {
		peopleList = strings.Join(contacts, ", ")
	}

	forwardingList := "нет"
	if rules, err := b.db.ListForwardRules(ctx); err == nil && len(rules) > 0 {
		var parts []string
		for _, r := range rules {
			parts = append(parts, fmt.Sprintf("из %q в %q", r.SourceName, r.TargetName))
		}
		forwardingList = strings.Join(parts, "; ")
	}

	return "Ты — универсальный личный ассистент владельца WhatsApp-бота учёта финансов. Тебя зовут " + b.botName + " — " +
		"в группах к тебе обращаются по этому имени. " +
		"Общайся свободно и по-человечески на любые темы: можешь поболтать, ответить на общие вопросы, " +
		"помочь советом, пошутить — ты не ограничен только финансами. Отвечай на русском, живо и без канцелярита, " +
		"но без лишней воды.\n\n" +
		"При этом ты «мозг» этого бота и знаешь, как он устроен: бот собирает платежи из рабочих WhatsApp-групп " +
		"(суммы из текстовых сообщений и банковские чеки с фото через OCR), помнит, из какой группы и от кого " +
		"пришёл каждый чек, замечает дубли и умеет строить отчёты. Если владелец спрашивает, как что-то работает, " +
		"почему чек не распознался или что-то посчиталось не так — объясняй и подсказывай, как поправить.\n\n" +
		"Сегодняшняя дата: " + now.Format("2006-01-02") + " (" + now.Format("02.01.2006") + ").\n\n" +
		"Группы, в которых состоит бот: " + groupsList + ".\n\n" +
		"Известные люди в учёте: " + peopleList + ". " +
		"Пользователь может писать имена с опечатками или в другой форме ('ахмет каталов', 'миланка') — " +
		"сам сопоставь с ближайшим известным именем и используй точное имя из списка при вызовах инструментов; " +
		"если сомневаешься, к кому относится запрос, — переспроси.\n\n" +
		"Сводка по сбору средств за текущий месяц по всем группам (" + from.Format("02.01.2006") + " — " + now.Format("02.01.2006") + "):\n" +
		summaryText +
		"\n\nТвои инструменты:\n" +
		"1. send_finance_report — отчёт по сбору за период (текстом или PDF). Вызывай при вопросах про суммы " +
		"за даты/период или просьбах прислать отчёт. Переводи относительные даты и названия месяцев в YYYY-MM-DD " +
		"от сегодняшней даты. format=\"pdf\" если просят файл/документ/пдф, иначе \"text\". " +
		"В group можно передать ОДНУ группу, НЕСКОЛЬКО через запятую/'и' ('Оплата КЛНТ и Отче' — посчитает по обеим " +
		"сразу), либо оставить пустым (по всем группам). В ОТВЕТЕ всегда явно пиши, за какой период и по какой " +
		"группе/группам цифры. НЕ переспрашивай группу без нужды: если тебя вызвали В группе и не назвали другую — " +
		"считай по ЭТОЙ группе; если пишут в личке без указания группы — считай по всем и так и напиши.\n" +
		"2. senders_report — СБОР по ответственному (кто собрал/забрал деньги). Считает ВСЁ, что человек собрал: " +
		"и чеки, и наличку/текстовые платежи. Вызывай при 'сбор Расула за июнь', 'сколько собрал Расул', " +
		"'какой сотрудник сколько собрал'. Фильтр по группе и по человеку (имя или номер). ВАЖНО: 'сбор X' — это " +
		"обычно про того, кто СОБРАЛ (senders_report), а не про клиента; если из контекста ясно, что X — клиент " +
		"(его платежи по рассрочке), тогда person_report.\n" +
		"3. save_pending_receipts — дозагрузка чеков в учёт. Когда бота не было в группе (добавили с опозданием), " +
		"владелец присылает пропущенные чеки сюда в личку и просит их запомнить, указывая группу. Каждое фото чека " +
		"уже распознано и ждёт; вызывай инструмент ТОЛЬКО после явной просьбы сохранить/запомнить/учесть, " +
		"с названием группы (переспроси, если владелец её не назвал). Дата операции берётся с самого чека " +
		"(включая год), так что чеки лягут на свои реальные даты. Дубли пропускаются автоматически.\n" +
		"4. correct_operation — исключить ошибочную операцию из учёта или вернуть обратно. Вызывай, когда говорят " +
		"'тот чек на 5000 был по ошибке, не считай', 'убери платёж Миланы на 3000', 'верни тот чек'. " +
		"Если инструмент вернул несколько кандидатов — покажи их и уточни, какую операцию имели в виду.\n" +
		"5. person_report — вся статистика по одному человеку: сколько чеков на его имя и платежей, суммы, " +
		"первая/последняя операции. Вызывай при 'сколько чеков у Ахмеда', 'что там по Милане'. Период необязателен.\n" +
		"6. make_custom_pdf — произвольный PDF-документ по описанию пользователя ('сделай чек/отчёт с такими данными', " +
		"'оформи это в пдф'). Ты задаёшь заголовок, колонки и строки. Данные бери из результатов других инструментов " +
		"или из того, что пользователь явно назвал — НЕ выдумывай цифры. Если для отчёта нужны данные — сначала " +
		"вызови нужный инструмент (senders_report/person_report/send_finance_report), потом оформи в make_custom_pdf. " +
		"Учти: senders_report и send_finance_report сами умеют присылать PDF (format=\"pdf\") — для стандартных " +
		"отчётов проще использовать их, а make_custom_pdf нужен для нестандартной таблицы, которую попросил владелец.\n" +
		"7. delete_my_messages — удалить (отозвать 'у всех') последние собственные сообщения бота в этом чате. " +
		"Вызывай, когда просят 'удали своё сообщение', 'убери это предупреждение'. Ты УМЕЕШЬ это делать — " +
		"не отвечай, что не можешь удалять сообщения.\n" +
		"8. manage_receipt_forwarding — автоматическая пересылка чеков между чатами. 'Давай все чеки из группы " +
		"оплата клиентов скидывай в оплата клнт' -> action=set; 'пересылай чеки из лички в ...' -> from='личка'; " +
		"'хватит пересылать' -> action=stop. Пересланные чеки сразу записываются в учёт целевой группы. " +
		"Сейчас активные правила пересылки: " + forwardingList + ".\n" +
		"9. list_unclear_receipts — список чеков/фото, которые бот не смог уверенно разобрать. " +
		"'Какие чеки ты не понял?', 'что не распозналось?'.\n" +
		"10. send_unclear_file — прислать файл непонятого чека в чат ('скинь мне тот чек, который не понял'), " +
		"чтобы владелец сам посмотрел и продиктовал данные.\n" +
		"10b. send_client_receipt — прислать сохранённый файл чека КОНКРЕТНОГО клиента ('скинь чек Миланы', " +
		"'покажи чеки Ахмеда за июнь'). Не путать с send_unclear_file (тот — только про непонятые).\n" +
		"10c. receipt_details — показать ВСЕ данные чека клиента (получатель и его банк/телефон, отправитель и его " +
		"банк/счёт, сумма, комиссия, дата, номер документа, код) — 'покажи все данные чека Цихаева'.\n" +
		"11. fix_receipt — записать данные чека со слов владельца ('на том чеке Милана, 25000, 2 июля'). " +
		"Обычный сценарий: list_unclear_receipts -> send_unclear_file -> владелец диктует -> fix_receipt.\n" +
		"12. recount_everything — полный пересчёт учёта заново ('пересчитай', 'проанализируй всё заново'): " +
		"повторное сопоставление имён и доразбор нераспознанных сообщений. После него данные во всех отчётах свежие.\n\n" +
		"Сверка с программой рассрочек (если настроена): бот сверяет чеки из групп с программой — внесли ли платёж " +
		"клиенту. Инструменты:\n" +
		"- cmf_check_receipts — ЖИВАЯ сверка: какие чеки внесены, какие НЕ внесены, по каким клиент не найден. " +
		"ВСЕГДА вызывай его при любом вопросе про сверку ('какие чеки не внесены', 'какие сегодняшние добавлены', " +
		"'чеки каких клиентов внесены', 'точно проверил?'). НИКОГДА не отвечай про сверку по памяти и не говори " +
		"'всё чисто' без вызова инструмента. По умолчанию проверяет сегодня; можно задать период и группу.\n" +
		"- cmf_add_payment — внести платёж в программу по чеку ('внеси чек Миланы на 25000 в программу'). " +
		"Это запись в программу — только по явной просьбе. Если у клиента несколько договоров, инструмент вернёт " +
		"список — переспроси, по какому вносить.\n" +
		"- cmf_resolve — указать, чей чек, когда клиентов несколько похожих.\n" +
		"- cmf_set_unmatched_branch — точка для чеков, клиента которых НЕТ в программе " +
		"('запомни: чек клиента, которого нет в программе, относится к точке Нойбер'). Для клиентов, которые ЕСТЬ " +
		"в программе, точка (филиал — Грозный/Главная и т.п.) берётся автоматически из их договора, задавать не нужно.\n" +
		"Если платёж не внесли за сутки, бот сам напоминает в группе.\n\n" +
		"Память номеров (кто 'забрал' деньги): у каждого чека есть отправитель (номер WhatsApp). Если номер " +
		"привязан к человеку в памяти — этот человек считается ответственным, который забрал деньги, и так он " +
		"показывается в отчёте по ответственным (senders_report). Инструмент phone_memory: 'открой память номеров' " +
		"(list), 'запомни этот номер — Расул' / 'этот номер теперь Майр-Эли' (set, сохраняется навсегда), убрать " +
		"(remove). Номер бери из слов владельца; если он написал 'этот номер' ответом (свайпом) на чек — в тексте " +
		"будет [Контекст ответа: номер отправителя +...], используй его.\n" +
		"assign_receipt_collector — привязать конкретный чек к тому, кто забрал деньги ('запиши этот чек на " +
		"Майр-Эли'). Если это ответ на чек, в тексте будет [Контекст ответа: ... id сообщения ...] — передай id.\n\n" +
		"Принципы работы:\n" +
		"- Подстраивайся под живые ситуации: владелец может дать команду в любой формулировке — пойми смысл " +
		"и выбери подходящий инструмент или их цепочку (например 'пересчитай и скинь итог' = recount_everything, " +
		"затем send_finance_report). Не проси переформулировать, если смысл ясен.\n" +
		"- Отвечай коротко и по делу: 1–3 предложения плюс, если нужно, список. Конкретные цифры и факты из " +
		"инструментов. Без воды, без длинных вступлений, без списков советов, которых не просили.\n" +
		"- Проверяй фактами, а не памятью. На вопросы про данные (сверка, суммы, чеки) СНАЧАЛА вызови инструмент, " +
		"потом отвечай. Никогда не говори 'всё внесено/всё чисто/всё сверено', не вызвав инструмент проверки — " +
		"это критично, за такие ответы владелец справедливо ругается.\n" +
		"- Не оправдывайся и не сыпь отмазками ('я не идеальный', 'OCR спотыкается', смайлы про зарплату). " +
		"Если что-то не смог — коротко: что именно и что нужно от владельца, одним предложением.\n" +
		"- Если просьба неоднозначна и от толкования зависит результат (какая группа, какой период, какой из " +
		"нескольких чеков) — задай ОДИН короткий уточняющий вопрос, а не гадай.\n" +
		"- Помни контекст диалога: если владелец говорит 'да', 'этот', 'запомни их' — это про то, что обсуждали " +
		"в предыдущих репликах.\n\n" +
		"Также знай: если сообщение с платежом/чеком УДАЛИЛИ в WhatsApp, бот автоматически убирает его из учёта " +
		"и пишет об этом в чат — отчёты всегда отражают актуальное состояние.\n\n" +
		"Если пользователь присылает фото чека, ты получишь его распознанное содержимое в квадратных скобках — " +
		"расскажи, что на чеке (от кого, сумма, дата операции, учтён ли уже). В учёт чек попадает только " +
		"через save_pending_receipts по явной просьбе, либо когда его присылают в рабочую группу."
}

// reportTool — инструмент Claude для отчёта за произвольный период. При
// format="pdf" сразу отправляет PDF-документ в чат и возвращает модели
// короткое подтверждение; при format="text" возвращает текстовую сводку,
// которую модель сама превратит в ответ пользователю.
func (b *Bot) reportTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "send_finance_report",
		Description: "Формирует отчёт по сбору средств за указанный период " +
			"и либо отправляет его как PDF-документ в чат, либо возвращает текстовую сводку для ответа. " +
			"Вызывай, когда пользователь спрашивает про суммы/сбор за конкретные даты или период, " +
			"или явно просит отчёт/документ. По умолчанию считает по всем группам сразу; " +
			"если пользователь назвал конкретную группу — передай её название в параметре group.",
		InputSchema: map[string]any{
			"type": "object",
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
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы; можно указать ДВЕ и более через запятую или \"и\" (напр. \"Оплата КЛНТ и Отче\"). Пусто — по всем группам.",
				},
			},
			"required": []string{"from_date", "to_date", "format"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Format   string `json:"format"`
				Group    string `json:"group"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			groupJIDs, groupLabel, err := b.resolveGroups(ctx, args.Group)
			if err != nil {
				return "", err
			}
			return b.buildReportForAssistant(ctx, chat, args.FromDate, args.ToDate, args.Format, groupJIDs, groupLabel)
		},
	}
}

// sendersTool — статистика "кто сколько чеков прислал": для групп, где
// работники кидают чеки (их сбор считается по отправленным чекам).
func (b *Bot) sendersTool(ctx context.Context, chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "senders_report",
		Description: "Показывает, кто сколько чеков отправил и на какую сумму за период — по отправителям " +
			"сообщений с чеками. Используй, когда спрашивают 'какой сотрудник сколько чеков скинул', " +
			"'сколько чеков и какой сбор сделал Расул', 'проверь по группе сб оплата клиентов кто сколько отправил' " +
			"и т.п. Если у отправителя не сохранено имя в WhatsApp, он будет показан по номеру телефона. " +
			"Может вернуть результат текстом или прислать PDF-документ (format=\"pdf\", если просят файл/пдф/документ).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{
					"type":        "string",
					"description": "Начало периода включительно, формат YYYY-MM-DD",
				},
				"to_date": map[string]any{
					"type":        "string",
					"description": "Конец периода включительно, формат YYYY-MM-DD",
				},
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы; можно несколько через запятую или \"и\". Пусто — по всем группам.",
				},
				"sender": map[string]any{
					"type":        "string",
					"description": "Имя или номер телефона конкретного человека, если спрашивают про одного (например 'расул' или '7937...'). Пусто — по всем отправителям.",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []string{"pdf", "text"},
					"description": "\"pdf\" — прислать документ; \"text\" (по умолчанию) — вернуть цифры текстом",
				},
			},
			"required": []string{"from_date", "to_date"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
				Sender   string `json:"sender"`
				Format   string `json:"format"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			from, err := time.Parse("2006-01-02", args.FromDate)
			if err != nil {
				return "", fmt.Errorf("неверная дата начала периода %q, нужен формат YYYY-MM-DD", args.FromDate)
			}
			toDay, err := time.Parse("2006-01-02", args.ToDate)
			if err != nil {
				return "", fmt.Errorf("неверная дата конца периода %q, нужен формат YYYY-MM-DD", args.ToDate)
			}
			groupJIDs, groupLabel, err := b.resolveGroups(ctx, args.Group)
			if err != nil {
				return "", err
			}

			stats, err := b.db.SenderStats(ctx, from, toDay.AddDate(0, 0, 1), groupJIDs, strings.TrimSpace(args.Sender))
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			periodLabel := from.Format("02.01.2006") + " — " + toDay.Format("02.01.2006")
			if len(stats) == 0 {
				return fmt.Sprintf("За период %s (%s) чеков не найдено.", periodLabel, groupLabel), nil
			}

			var totalCount int
			var totalSum float64
			for _, s := range stats {
				totalCount += s.Count
				totalSum += s.Total
			}

			if args.Format == "pdf" {
				rows := make([][]string, 0, len(stats))
				for _, s := range stats {
					label := s.Name
					if label == "" {
						label = "+" + s.Phone
					} else if s.Phone != "" {
						label += " (+" + s.Phone + ")"
					}
					rows = append(rows, []string{label, fmt.Sprintf("%d", s.Count), formatRub(s.Total)})
				}
				section := report.Section{
					Title:    "Чеки по отправителям",
					Columns:  []string{"Отправитель", "Кол-во чеков", "Сумма, ₽"},
					Rows:     rows,
					TotalRow: []string{"ИТОГО", fmt.Sprintf("%d", totalCount), formatRub(totalSum)},
				}
				outPath := fmt.Sprintf("%s/senders_%s.pdf", b.reportDir, time.Now().Format("2006-01-02_15-04-05"))
				subtitle := "Период: " + periodLabel + " | " + groupLabel
				if err := report.GenerateCustom("Отчёт: кто сколько чеков скинул", subtitle, []report.Section{section}, b.fontDir, outPath); err != nil {
					return "", fmt.Errorf("ошибка генерации PDF: %w", err)
				}
				b.sendDocument(chat, outPath, "Чеки_по_отправителям_"+from.Format("2006-01-02")+".pdf")
				return fmt.Sprintf("PDF с разбивкой по отправителям за %s (%s) отправлен: %d чек(ов) на %.0f ₽.", periodLabel, groupLabel, totalCount, totalSum), nil
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "Чеки по отправителям за %s (%s):\n", periodLabel, groupLabel)
			for _, s := range stats {
				label := s.Name
				if label == "" {
					label = "+" + s.Phone
				} else if s.Phone != "" {
					label += " (+" + s.Phone + ")"
				}
				fmt.Fprintf(&sb, "- %s: %d чек(ов), %.0f ₽\n", label, s.Count, s.Total)
			}
			fmt.Fprintf(&sb, "Итого: %d чек(ов) на %.0f ₽", totalCount, totalSum)
			return sb.String(), nil
		},
	}
}

// customPDFTool — произвольный PDF по запросу владельца: он описывает, какие
// колонки и строки нужны, а бот рисует таблицу и присылает документ.
// Данные бот НЕ придумывает — модель заполняет их из результатов других
// инструментов или из того, что явно продиктовал владелец.
func (b *Bot) customPDFTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "make_custom_pdf",
		Description: "Создаёт и присылает произвольный PDF-документ с таблицей(ами) по описанию пользователя. " +
			"Используй, когда просят 'сделай чек/отчёт/документ с такими данными', 'оформи в пдф вот это', " +
			"или когда нужно красиво оформить результат другого инструмента в файл. " +
			"ВАЖНО: бери данные только из результатов инструментов или из того, что пользователь явно назвал — " +
			"НЕ выдумывай цифры. Первая колонка выравнивается влево, остальные вправо (для чисел).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Заголовок документа",
				},
				"subtitle": map[string]any{
					"type":        "string",
					"description": "Подзаголовок: период, группа, автор и т.п. (необязательно)",
				},
				"sections": map[string]any{
					"type":        "array",
					"description": "Один или несколько блоков-таблиц",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":   map[string]any{"type": "string", "description": "Заголовок блока (необязательно)"},
							"columns": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Названия колонок"},
							"rows": map[string]any{
								"type":        "array",
								"description": "Строки таблицы; каждая — массив ячеек по числу колонок",
								"items":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							},
							"total_row": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Необязательная итоговая строка (по числу колонок)",
							},
						},
						"required": []string{"columns", "rows"},
					},
				},
			},
			"required": []string{"title", "sections"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Title    string `json:"title"`
				Subtitle string `json:"subtitle"`
				Sections []struct {
					Title    string     `json:"title"`
					Columns  []string   `json:"columns"`
					Rows     [][]string `json:"rows"`
					TotalRow []string   `json:"total_row"`
				} `json:"sections"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			if strings.TrimSpace(args.Title) == "" || len(args.Sections) == 0 {
				return "", fmt.Errorf("нужен заголовок и хотя бы одна таблица")
			}
			sections := make([]report.Section, 0, len(args.Sections))
			for _, s := range args.Sections {
				if len(s.Columns) == 0 {
					continue
				}
				sections = append(sections, report.Section{
					Title:    s.Title,
					Columns:  s.Columns,
					Rows:     s.Rows,
					TotalRow: s.TotalRow,
				})
			}
			if len(sections) == 0 {
				return "", fmt.Errorf("не задано ни одной таблицы с колонками")
			}
			outPath := fmt.Sprintf("%s/custom_%s.pdf", b.reportDir, time.Now().Format("2006-01-02_15-04-05"))
			if err := report.GenerateCustom(args.Title, args.Subtitle, sections, b.fontDir, outPath); err != nil {
				return "", fmt.Errorf("ошибка генерации PDF: %w", err)
			}
			b.sendDocument(chat, outPath, sanitizeFileName(args.Title)+".pdf")
			return "Готовый PDF «" + args.Title + "» отправлен в чат.", nil
		},
	}
}

// formatRub форматирует сумму с разбивкой по разрядам для PDF-таблиц.
func formatRub(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// sanitizeFileName делает из заголовка безопасное имя файла.
func sanitizeFileName(s string) string {
	s = strings.TrimSpace(s)
	repl := func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\t':
			return '_'
		}
		return r
	}
	s = strings.Map(repl, s)
	if len([]rune(s)) > 60 {
		s = string([]rune(s)[:60])
	}
	if s == "" {
		s = "Отчёт"
	}
	return s
}

// forwardingTool — управление правилами пересылки чеков между чатами:
// "все чеки из группы оплата клиентов скидывай в оплата клнт" / "хватит
// пересылать". Правила хранятся в БД и переживают рестарт бота.
func (b *Bot) forwardingTool() ai.Tool {
	return ai.Tool{
		Name: "manage_receipt_forwarding",
		Description: "Управляет автоматической пересылкой чеков между чатами. " +
			"action=set: все чеки (фото и PDF), приходящие в чат from, бот будет автоматически пересылать " +
			"в группу to и сразу записывать их в учёт целевой группы. from — название группы или 'личка' " +
			"(чеки, присланные боту в личные сообщения). " +
			"action=stop: выключить пересылку из from (или ВСЕ правила, если from не указан). " +
			"action=list: показать активные правила. " +
			"Вызывай при командах вроде 'давай все чеки из группы оплата клиентов скидывай в оплата клнт', " +
			"'пересылай чеки из лички в основную группу', 'хватит пересылать чеки', 'останови пересылку из ...'.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"set", "stop", "list"},
					"description": "set — включить пересылку; stop — выключить; list — показать правила",
				},
				"from": map[string]any{
					"type":        "string",
					"description": "Источник: название группы или 'личка'. Для stop можно не указывать (выключит все)",
				},
				"to": map[string]any{
					"type":        "string",
					"description": "Название целевой группы (обязательно для set)",
				},
			},
			"required": []string{"action"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Action string `json:"action"`
				From   string `json:"from"`
				To     string `json:"to"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}

			switch args.Action {
			case "list":
				rules, err := b.db.ListForwardRules(ctx)
				if err != nil {
					return "", err
				}
				if len(rules) == 0 {
					return "Активных правил пересылки нет.", nil
				}
				var sb strings.Builder
				sb.WriteString("Активные правила пересылки чеков:\n")
				for _, r := range rules {
					fmt.Fprintf(&sb, "- из %q -> в %q\n", r.SourceName, r.TargetName)
				}
				return sb.String(), nil

			case "stop":
				source := ""
				label := "все правила"
				if s := strings.TrimSpace(args.From); s != "" {
					src, srcName, err := b.resolveForwardSource(ctx, s)
					if err != nil {
						return "", err
					}
					source, label = src, "пересылку из "+srcName
				}
				n, err := b.db.DeleteForwardRule(ctx, source)
				if err != nil {
					return "", err
				}
				if n == 0 {
					return "Подходящих правил пересылки не было — ничего не выключал.", nil
				}
				return fmt.Sprintf("Выключил %s (удалено правил: %d).", label, n), nil

			case "set":
				if strings.TrimSpace(args.From) == "" || strings.TrimSpace(args.To) == "" {
					return "", fmt.Errorf("для включения пересылки нужны и источник (from), и целевая группа (to)")
				}
				source, sourceName, err := b.resolveForwardSource(ctx, args.From)
				if err != nil {
					return "", err
				}
				targetJID, targetName, err := b.resolveGroup(ctx, args.To)
				if err != nil {
					return "", err
				}
				if source == targetJID.String() {
					return "", fmt.Errorf("источник и целевая группа совпадают")
				}
				if err := b.db.SetForwardRule(ctx, source, sourceName, targetJID.String(), targetName); err != nil {
					return "", err
				}
				return fmt.Sprintf("Включил: все чеки из %q теперь пересылаются в %q и записываются в её учёт. Скажи 'останови пересылку', когда надо будет выключить.", sourceName, targetName), nil

			default:
				return "", fmt.Errorf("неизвестное действие %q", args.Action)
			}
		},
	}
}

// resolveForwardSource переводит человекочитаемый источник пересылки в ключ
// правила: 'личка'/'лс'/'dm' -> "dm", иначе — JID группы по названию.
func (b *Bot) resolveForwardSource(ctx context.Context, from string) (source, name string, err error) {
	switch strings.ToLower(strings.TrimSpace(from)) {
	case "личка", "лс", "dm", "личные", "личные чаты", "личные сообщения":
		return "dm", "личных сообщений", nil
	}
	jid, gname, err := b.resolveGroup(ctx, from)
	if err != nil {
		return "", "", err
	}
	return jid.String(), gname, nil
}

// deleteMessagesTool — отзыв собственных сообщений бота по просьбе владельца
// ("удали своё последнее сообщение", "убери свои предупреждения из чата").
func (b *Bot) deleteMessagesTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "delete_my_messages",
		Description: "Удаляет (отзывает 'у всех') последние сообщения, которые отправил сам бот. " +
			"Вызывай, когда владелец просит удалить твоё сообщение/сообщения, убрать предупреждение и т.п. " +
			"По умолчанию удаляет в текущем чате; если владелец указал группу ('удали своё сообщение в группе " +
			"оплата клнт') — передай её название в group. count — сколько последних сообщений (по умолчанию 1). " +
			"Чужие сообщения удалять нельзя.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{
					"type":        "integer",
					"description": "Сколько последних сообщений бота удалить (по умолчанию 1)",
				},
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы, если удалять надо в ней, а не в текущем чате",
				},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Count int    `json:"count"`
				Group string `json:"group"`
			}
			_ = json.Unmarshal(input, &args)
			if args.Count <= 0 {
				args.Count = 1
			}
			target := chat
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				target = jid
			}
			deleted := b.deleteOwnMessages(ctx, target, args.Count)
			if deleted == 0 {
				return "Нечего удалять — я не находил своих недавних сообщений там.", nil
			}
			return fmt.Sprintf("Удалил свои последние сообщения: %d.", deleted), nil
		},
	}
}

// personTool — сводка по конкретному человеку: сколько чеков на его имя
// и текстовых платежей, за всё время или за период.
func (b *Bot) personTool() ai.Tool {
	return ai.Tool{
		Name: "person_report",
		Description: "Показывает всю статистику по конкретному человеку: сколько чеков на его имя (переводы на его " +
			"карту), сколько текстовых платежей, общие суммы, первая и последняя операции. " +
			"Вызывай при вопросах вроде 'сколько чеков у Ахмеда Каталова', 'что там по Милане', 'сводка по Хадижат'. " +
			"Период необязателен — без него считает за всё время. " +
			"Имя пиши как в списке известных людей (исправь опечатки пользователя сам).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"person": map[string]any{
					"type":        "string",
					"description": "Имя человека (из списка известных людей в учёте)",
				},
				"from_date": map[string]any{
					"type":        "string",
					"description": "Начало периода YYYY-MM-DD; пусто = за всё время",
				},
				"to_date": map[string]any{
					"type":        "string",
					"description": "Конец периода включительно YYYY-MM-DD; пусто = до сегодня",
				},
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы, если нужна статистика только по ней",
				},
			},
			"required": []string{"person"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Person   string `json:"person"`
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			if strings.TrimSpace(args.Person) == "" {
				return "", fmt.Errorf("нужно имя человека")
			}

			var fromPtr, toPtr *time.Time
			if args.FromDate != "" {
				t, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local)
				if err != nil {
					return "", fmt.Errorf("неверная дата %q", args.FromDate)
				}
				fromPtr = &t
			}
			if args.ToDate != "" {
				t, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local)
				if err != nil {
					return "", fmt.Errorf("неверная дата %q", args.ToDate)
				}
				next := t.AddDate(0, 0, 1)
				toPtr = &next
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}

			stats, err := b.db.PersonReport(ctx, strings.TrimSpace(args.Person), fromPtr, toPtr, groupJID)
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			if len(stats) == 0 {
				return fmt.Sprintf("По имени %q операций не нашёл. Проверь имя — известные люди перечислены в твоём контексте.", args.Person), nil
			}
			var sb strings.Builder
			for _, p := range stats {
				fmt.Fprintf(&sb, "%s:\n", p.Name)
				fmt.Fprintf(&sb, "- чеков: %d на %.0f ₽\n", p.ReceiptCount, p.ReceiptTotal)
				fmt.Fprintf(&sb, "- текстовых платежей: %d на %.0f ₽\n", p.PaymentCount, p.PaymentTotal)
				fmt.Fprintf(&sb, "- всего: %.0f ₽\n", p.ReceiptTotal+p.PaymentTotal)
				if p.FirstOp != nil && p.LastOp != nil {
					fmt.Fprintf(&sb, "- операции с %s по %s\n", p.FirstOp.Format("02.01.2006"), p.LastOp.Format("02.01.2006"))
				}
			}
			return sb.String(), nil
		},
	}
}

// correctionTool — ручные корректировки учёта: "тот чек на 5000 был по ошибке,
// не считай его" / "верни обратно платёж на 3000". Ищет операцию по сумме
// с уточнениями и помечает её исключённой (или снимает пометку).
// defaultGroupJID — группа по умолчанию для поиска (в группе — она сама,
// в личке — пусто, т.е. все группы).
func (b *Bot) correctionTool(defaultGroupJID string) ai.Tool {
	return ai.Tool{
		Name: "correct_operation",
		Description: "Исключает операцию (платёж или чек) из учёта, возвращает её обратно, или снимает ошибочную " +
			"пометку 'дубль'. Вызывай, когда пользователь говорит: запись ошибочная/не должна считаться " +
			"('тот чек на 5000 скинули по ошибке, убери его') → action=exclude; вернуть исключённую " +
			"('верни тот чек на 5000') → action=restore; чек ошибочно посчитан дублем и его надо засчитать " +
			"('это не дубль, засчитай чек Миланы на 25000') → action=not_duplicate. " +
			"Если под описание подходит несколько операций, инструмент вернёт список — уточни дату или имя.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"exclude", "restore", "not_duplicate"},
					"description": "exclude — исключить из учёта; restore — вернуть исключённую; not_duplicate — снять ошибочную пометку дубля и засчитать",
				},
				"amount": map[string]any{
					"type":        "number",
					"description": "Сумма операции в рублях (точная)",
				},
				"person": map[string]any{
					"type":        "string",
					"description": "Имя человека, если известно (для уточнения поиска)",
				},
				"date": map[string]any{
					"type":        "string",
					"description": "Дата операции YYYY-MM-DD, если известна (для уточнения поиска)",
				},
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы, если пользователь её указал",
				},
			},
			"required": []string{"action", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Action string  `json:"action"`
				Amount float64 `json:"amount"`
				Person string  `json:"person"`
				Date   string  `json:"date"`
				Group  string  `json:"group"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			if args.Amount <= 0 {
				return "", fmt.Errorf("нужна точная сумма операции")
			}

			groupJID := defaultGroupJID
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}

			// "Это не дубль" — снимаем пометку дубля с последнего такого чека.
			if args.Action == "not_duplicate" {
				found, name, txDate, err := b.db.ClearDuplicateFlag(ctx, args.Amount, strings.TrimSpace(args.Person), groupJID)
				if err != nil {
					return "", fmt.Errorf("не удалось снять пометку дубля: %w", err)
				}
				if !found {
					return fmt.Sprintf("Чека на %.0f ₽, помеченного дублем, не нашёл — возможно, он уже засчитан.", args.Amount), nil
				}
				return fmt.Sprintf("Снял пометку дубля: %s, %.0f ₽ от %s — теперь чек учитывается в сборе.",
					name, args.Amount, txDate.Format("02.01.2006 15:04")), nil
			}

			var fromPtr, toPtr *time.Time
			if strings.TrimSpace(args.Date) != "" {
				day, err := time.ParseInLocation("2006-01-02", args.Date, time.Local)
				if err != nil {
					return "", fmt.Errorf("неверная дата %q, нужен формат YYYY-MM-DD", args.Date)
				}
				next := day.AddDate(0, 0, 1)
				fromPtr, toPtr = &day, &next
			}

			ops, err := b.db.FindOperations(ctx, args.Amount, strings.TrimSpace(args.Person), fromPtr, toPtr, groupJID)
			if err != nil {
				return "", fmt.Errorf("ошибка поиска операции: %w", err)
			}

			// Для exclude интересны ещё не исключённые, для restore — исключённые.
			wantIgnored := args.Action == "restore"
			var candidates []db.OperationRef
			for _, op := range ops {
				if op.Ignored == wantIgnored {
					candidates = append(candidates, op)
				}
			}

			switch len(candidates) {
			case 0:
				if args.Action == "restore" {
					return fmt.Sprintf("Исключённых операций на %.0f ₽ не нашёл.", args.Amount), nil
				}
				return fmt.Sprintf("Операций на %.0f ₽ в учёте не нашёл — возможно, она уже исключена или сумма другая.", args.Amount), nil
			case 1:
				op := candidates[0]
				if err := b.db.SetOperationIgnored(ctx, op.Kind, op.ID, args.Action == "exclude"); err != nil {
					return "", fmt.Errorf("не удалось обновить операцию: %w", err)
				}
				verb := "исключена из учёта"
				if args.Action == "restore" {
					verb = "возвращена в учёт"
				}
				return fmt.Sprintf("Операция %s: %s, %.0f ₽ от %s. Отчёты уже учитывают это изменение.",
					verb, op.Name, op.Amount, op.TxDate.Format("02.01.2006 15:04")), nil
			default:
				var sb strings.Builder
				sb.WriteString("Нашёл несколько подходящих операций, уточни дату или имя:\n")
				for _, op := range candidates {
					fmt.Fprintf(&sb, "- %s, %.0f ₽, %s\n", op.Name, op.Amount, op.TxDate.Format("02.01.2006 15:04"))
				}
				return sb.String(), nil
			}
		},
	}
}

// savePendingTool сохраняет в учёт чеки, присланные владельцем в личку —
// дозагрузка пропущенных дней, когда бота ещё не было в группе. Дата каждого
// чека берётся с самого чека (включая год), так что "июльские" чеки лягут
// на июль, даже если их прислали позже.
func (b *Bot) savePendingTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "save_pending_receipts",
		Description: "Сохраняет в учёт чеки, присланные фото в этот личный чат и ещё не сохранённые. " +
			"Вызывай ТОЛЬКО когда владелец явно просит их запомнить/добавить/учесть " +
			"(например 'запомни эти чеки, это для группы оплата клиентов', 'тебя не было в группе 1-2 июля, вот пропущенные чеки'). " +
			"Обязательно нужна группа, к которой отнести чеки. Дата операции берётся с самого чека. " +
			"Дубли уже учтённых чеков автоматически пропускаются.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"group": map[string]any{
					"type":        "string",
					"description": "Название группы, к которой относятся чеки (например 'оплата клиентов')",
				},
				"sender": map[string]any{
					"type":        "string",
					"description": "Кто изначально прислал/собрал эти чеки, если владелец указал (например 'Расул'). Пусто, если не указано.",
				},
			},
			"required": []string{"group"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Group  string `json:"group"`
				Sender string `json:"sender"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			return b.savePendingReceipts(ctx, chat, args.Group, strings.TrimSpace(args.Sender))
		},
	}
}

// savePendingReceipts — реализация save_pending_receipts: переносит ожидающие
// чеки из лички в учёт с привязкой к группе, проверяя каждый на дубль.
func (b *Bot) savePendingReceipts(ctx context.Context, chat types.JID, groupName, sender string) (string, error) {
	groupJID, groupLabel, err := b.resolveGroup(ctx, groupName)
	if err != nil {
		return "", err
	}

	chatKey := chat.String()
	b.pendingMu.Lock()
	receipts := b.pending[chatKey]
	delete(b.pending, chatKey)
	b.pendingMu.Unlock()

	if len(receipts) == 0 {
		return "Ожидающих чеков нет — сначала пришли фото чеков в этот чат.", nil
	}

	var sb strings.Builder
	saved, skipped := 0, 0
	for _, pr := range receipts {
		rd := pr.rd
		txDate := pr.receivedAt
		if rd.HasTxTime {
			txDate = rd.TxTime
		}

		canonical, matched := b.aliases.ResolveName(rd.Recipient)
		var contactIDPtr *int
		if contactID, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			contactIDPtr = &contactID
		}

		// Дубли ищем только внутри целевой группы: тот же чек в ДРУГОЙ группе
		// (например, в группе СБ, откуда его переслали) — это не дубль.
		isDup, dupTime, err := b.db.FindDuplicateReceipt(ctx, groupJID.String(), rd.DocNumber, rd.AuthCode, contactIDPtr, rd.Recipient, rd.Amount, txDate)
		if err != nil {
			fmt.Println("Ошибка проверки дубля при дозагрузке:", err)
		}
		if isDup {
			skipped++
			fmt.Fprintf(&sb, "- ПРОПУЩЕН (дубль, уже учтён %s): %s, %.0f ₽\n", dupTime.Format("02.01.2006 15:04"), canonical, rd.Amount)
			continue
		}

		err = b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
			RawMessageID: pr.rawID,
			Bank:         rd.Bank,
			RecipientRaw: rd.Recipient,
			SenderRaw:    rd.Sender,
			ContactID:    contactIDPtr,
			Amount:       rd.Amount,
			Commission:   rd.Commission,
			DocNumber:    rd.DocNumber,
			AuthCode:     rd.AuthCode,
			Status:       rd.Status,
			NeedsReview:  !matched,
			GroupJID:     groupJID.String(),
			SubmittedBy:  sender,
			TxDate:       txDate,
		})
		if err != nil {
			fmt.Fprintf(&sb, "- ОШИБКА сохранения: %s, %.0f ₽ (%v)\n", canonical, rd.Amount, err)
			continue
		}
		saved++
		dateLabel := txDate.Format("02.01.2006 15:04")
		if !rd.HasTxTime {
			dateLabel += " (дата с чека не распозналась — взято время получения)"
		}
		fmt.Fprintf(&sb, "- Сохранён: %s, %.0f ₽, операция %s\n", canonical, rd.Amount, dateLabel)
	}

	header := fmt.Sprintf("Группа: %s. Сохранено %d, пропущено дублей %d.\n", groupLabel, saved, skipped)
	return header + sb.String(), nil
}

// buildReportForAssistant — общая логика инструмента send_finance_report:
// достаёт сводку за период из БД и либо отправляет PDF, либо возвращает
// текст для финального ответа модели.
func (b *Bot) buildReportForAssistant(ctx context.Context, chat types.JID, fromStr, toStr, format string, groupJIDs []string, groupLabel string) (string, error) {
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
	if groupLabel != "" {
		periodLabel += " (" + groupLabel + ")"
	}

	summaries, err := b.db.SummaryForPeriod(ctx, from, to, groupJIDs)
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

// saveMediaFile сохраняет файл чека (фото или PDF) на диск для ручной
// проверки/аудита, возвращает путь к файлу или пустую строку при ошибке.
func (b *Bot) saveMediaFile(waMessageID string, data []byte, ext string) string {
	dir := filepath.Join(b.reportDir, "..", "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Println("Ошибка создания папки receipts:", err)
		return ""
	}
	path := filepath.Join(dir, waMessageID+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Println("Ошибка сохранения файла чека:", err)
		return ""
	}
	return path
}

// applyForwardRules пересылает файл чека (фото/PDF) в целевые группы по
// активным правилам и сразу записывает его в учёт целевой группы —
// дальше он там живёт как обычный чек (с проверкой дублей внутри группы).
func (b *Bot) applyForwardRules(ctx context.Context, sourceKey, senderName, origMsgID string, media []byte, ext, text string, ts time.Time) {
	rules, err := b.db.ListForwardRules(ctx)
	if err != nil {
		fmt.Println("Ошибка чтения правил пересылки:", err)
		return
	}
	for _, rule := range rules {
		if rule.Source != sourceKey {
			continue
		}
		if rule.TargetJID == sourceKey {
			continue // защита от пересылки группы в саму себя
		}
		targetJID, err := types.ParseJID(rule.TargetJID)
		if err != nil {
			fmt.Println("Правило пересылки с кривым JID:", rule.TargetJID, err)
			continue
		}

		if ext == ".pdf" {
			b.sendDocumentBytes(targetJID, media, "чек_"+ts.Format("2006-01-02_15-04")+".pdf", "application/pdf")
		} else {
			b.sendImageBytes(targetJID, media)
		}
		fmt.Printf("Чек из %s переслан в %s (%s)\n", sourceKey, rule.TargetName, rule.TargetJID)

		// Записываем чек в учёт целевой группы. Синтетический ID сообщения,
		// чтобы не конфликтовать с исходным по уникальности.
		if parser.LooksLikeBankReceipt(text) {
			rawID, err := b.db.SaveRawMessage(ctx, origMsgID+"-fwd-"+targetJID.User, rule.TargetJID,
				"bot-forward", senderName, text, true, "", ts)
			if err != nil {
				fmt.Println("Ошибка сохранения пересланного чека:", err)
				continue
			}
			b.handleBankReceipt(ctx, targetJID, "", "", text, rawID, ts, media, ext, "")
		}
	}
}

// handleBankReceipt разбирает распознанный текст скриншота банковского перевода
// и сохраняет его в bank_receipts. Если получателя не удалось уверенно
// сопоставить с известным контактом (алиас не найден и это выглядит как новое
// имя) — помечает needs_review = true, чтобы владелец мог проверить вручную.
// receivedAt — время получения сообщения в WhatsApp, используется как
// запасной вариант, если на самом чеке не удалось распознать дату/время операции.
// media/mediaExt — исходный файл чека (может быть nil): если и парсер, и
// текстовый ИИ-доразбор не справились, Claude смотрит на изображение чека
// напрямую (последний рубеж распознавания).
// senderJID — кто прислал чек (для сверки с программой). payerOverride —
// ФИО клиента, написанное рядом с чеком; если задано, платёж относится
// именно к нему (а не к получателю на чеке — там часто владелец карты).
func (b *Bot) handleBankReceipt(ctx context.Context, chat types.JID, senderJID, waMsgID, text string, rawID int, receivedAt time.Time, media []byte, mediaExt, payerOverride string) {
	rd := parser.ParseReceipt(text)

	// RECEIPT_VISION_FIRST=1 — читать чек-фото сразу глазами Claude (макс.
	// точность на «неразборчивых» фото, дороже по токенам). Для PDF не нужно —
	// там надёжный текстовый слой.
	visionFirst := receiptVisionFirst() && media != nil && mediaExt != ".pdf" && b.assistant != nil
	if visionFirst {
		if rec, ok := b.aiVisionReceipt(ctx, media, mediaExt); ok {
			applyAIReceiptAuthoritative(&rd, rec)
			fmt.Printf("Чек (сообщение %d): прочитан Claude с изображения (получатель %q, сумма %.0f ₽)\n", rawID, rd.Recipient, rd.Amount)
		}
	}

	// Обычный парсер не справился (нестандартная вёрстка чека, кривой OCR) —
	// пробуем доразобрать через ИИ по тексту, дополняя недостающие поля.
	if (rd.Amount == 0 || rd.Recipient == "") && b.assistant != nil && strings.TrimSpace(text) != "" {
		if rec, ok := b.aiRescueReceipt(ctx, text); ok {
			mergeAIReceipt(&rd, rec)
			fmt.Printf("Чек (сообщение %d): обычный парсер не справился, ИИ дораспознал по тексту (получатель %q, сумма %.0f ₽)\n",
				rawID, rd.Recipient, rd.Amount)
		}
	}

	// Слабый OCR: нет ключевых полей ИЛИ не прочитались ни номер документа,
	// ни время операции (значит структура чека не распозналась) — показываем
	// Claude само изображение. Пропускаем, если вижн уже отработал первым.
	weakOCR := rd.Amount == 0 || rd.Recipient == "" || (rd.DocNumber == "" && !rd.HasTxTime)
	if !visionFirst && weakOCR && media != nil {
		if rec, ok := b.aiVisionReceipt(ctx, media, mediaExt); ok {
			mergeAIReceipt(&rd, rec)
		}
	}

	// ФИО, написанное рядом с чеком, важнее получателя на чеке — платёж
	// относим к клиенту, которого назвал владелец. Получателя с чека
	// (владельца карты) сохраняем отдельно в cardOwner для истории.
	cardOwner := ""
	if payerOverride != "" {
		cardOwner = rd.Recipient
		rd.Recipient = payerOverride
	}

	txDate := receivedAt
	if rd.HasTxTime {
		txDate = rd.TxTime
	}

	if rd.Amount == 0 || rd.Recipient == "" {
		// Ни парсер, ни ИИ по тексту, ни вижн ничего не вытащили. Если это
		// не выглядело чеком вообще (случайная картинка без денежных полей и
		// без частичных данных) — не засоряем "непонятые", просто выходим.
		looksReceipt := parser.LooksLikeBankReceipt(text) || rd.Amount > 0 || rd.Recipient != "" || rd.DocNumber != ""
		if !looksReceipt {
			fmt.Printf("Медиа (сообщение %d) не распознано как чек — пропускаю (вероятно, не чек)\n", rawID)
			return
		}
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
			GroupJID:     chat.String(),
			TxDate:       txDate,
		})
		return
	}

	// ResolveName умеет сопоставлять полные ФИО с чеков ("Милана Нажудовна К.")
	// с короткими алиасами ("Милана") — по словам, а не только точным совпадением.
	canonical, matched := b.aliases.ResolveName(rd.Recipient)
	// Не нашли уверенного совпадения — на ручную проверку. Но если клиента
	// назвал сам владелец (payerOverride), имени доверяем — проверка не нужна.
	needsReview := !matched && payerOverride == ""

	var contactIDPtr *int
	contactID, err := b.db.GetOrCreateContact(ctx, canonical)
	if err != nil {
		fmt.Println("Ошибка получения контакта для чека:", err)
	} else {
		contactIDPtr = &contactID
	}

	// Проверяем, не тот же самый чек уже присылали В ЭТУ ЖЕ ГРУППУ — по
	// совокупности параметров (получатель + сумма + время операции + номер
	// документа, см. FindDuplicateReceipt) — до вставки, иначе новая запись
	// найдёт сама себя. Тот же чек в другой группе дублем не считается: это
	// рабочий процесс (чеки из группы СБ пересылают в основную группу).
	isDuplicate, dupTxDate, err := b.db.FindDuplicateReceipt(ctx, chat.String(), rd.DocNumber, rd.AuthCode, contactIDPtr, rd.Recipient, rd.Amount, txDate)
	if err != nil {
		fmt.Println("Ошибка проверки дубля чека:", err)
	}

	err = b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
		RawMessageID:    rawID,
		Bank:            rd.Bank,
		RecipientRaw:    rd.Recipient,
		RecipientBank:   rd.RecipientBank,
		RecipientPhone:  rd.RecipientPhone,
		SenderRaw:       rd.Sender,
		SenderBank:      rd.SenderBank,
		SenderAccount:   rd.SenderAccount,
		CardOwner:       cardOwner,
		ContactID:       contactIDPtr,
		Amount:          rd.Amount,
		Commission:      rd.Commission,
		DocNumber:       rd.DocNumber,
		AuthCode:        rd.AuthCode,
		Status:          rd.Status,
		NeedsReview:     needsReview,
		IsDuplicate:     isDuplicate,
		ClientConfirmed: payerOverride != "", // клиента назвал владелец — точно
		GroupJID:        chat.String(),
		TxDate:          txDate,
	})
	if err != nil {
		fmt.Println("Ошибка сохранения чека:", err)
	}
	if needsReview {
		fmt.Printf("Чек (сообщение %d): получатель %q не найден в списке известных — нужна проверка\n", rawID, rd.Recipient)
	}
	if isDuplicate {
		fmt.Printf("Чек (сообщение %d): похоже на повтор чека от %s на %.0f ₽ (первый раз был %s) — не учитываю в сумме\n",
			rawID, canonical, rd.Amount, dupTxDate.Format("02.01.2006 15:04"))
		b.sendReply(chat, fmt.Sprintf(
			"⚠️ Похоже, этот чек уже присылали в эту группу: %s, %.0f ₽, чек от %s. Второй раз не учитываю в сумме сбора. "+
				"Если это ошибка — ответь на это сообщение или напиши «%s, это не дубль, засчитай чек %s на %.0f», и я его учту.",
			canonical, rd.Amount, dupTxDate.Format("02.01.2006 15:04"), b.botName, canonical, rd.Amount),
			waMsgID, senderJID)
	}

	// Сверка с программой рассрочек: заводим наблюдение за этим чеком (с уже
	// разобранной суммой/датой, включая вижн). Клиент — ФИО рядом с чеком,
	// иначе получатель. Дубли не сверяем повторно.
	if b.cmf != nil && !isDuplicate {
		clientText := payerOverride
		if clientText == "" {
			clientText = rd.Recipient
		}
		go b.cmfWatchReceipt(context.Background(), chat, senderJID, clientText, rd.Amount, txDate, rawID)
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
	stopTyping := b.startTyping(chat)
	defer stopTyping()

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 1, 0)

	// "/отчет" в группе показывает общий сбор по всем группам (единый учёт);
	// отчёт по конкретной группе можно спросить у ассистента в личке.
	summaries, err := b.db.SummaryForPeriod(ctx, from, to, nil)
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
	resp, err := b.client.SendMessage(context.Background(), chat, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		fmt.Println("Ошибка отправки сообщения:", err)
		return
	}
	b.rememberSent(chat, resp.ID)
}

// rememberSent запоминает ID отправленного ботом сообщения (до 20 последних
// на чат), чтобы по просьбе владельца бот мог их отозвать.
func (b *Bot) rememberSent(chat types.JID, id string) {
	if id == "" {
		return
	}
	key := chat.String()
	b.sentMu.Lock()
	b.sentMsgs[key] = append(b.sentMsgs[key], id)
	if len(b.sentMsgs[key]) > 20 {
		b.sentMsgs[key] = b.sentMsgs[key][len(b.sentMsgs[key])-20:]
	}
	b.sentMu.Unlock()
}

// deleteOwnMessages отзывает ("удаляет у всех") последние n сообщений бота
// в чате. Возвращает, сколько удалил.
func (b *Bot) deleteOwnMessages(ctx context.Context, chat types.JID, n int) int {
	key := chat.String()
	b.sentMu.Lock()
	ids := b.sentMsgs[key]
	if n > len(ids) {
		n = len(ids)
	}
	toDelete := append([]string(nil), ids[len(ids)-n:]...)
	b.sentMsgs[key] = ids[:len(ids)-n]
	b.sentMu.Unlock()

	deleted := 0
	for i := len(toDelete) - 1; i >= 0; i-- {
		if _, err := b.client.SendMessage(ctx, chat, b.client.BuildRevoke(chat, types.EmptyJID, toDelete[i])); err != nil {
			fmt.Println("Ошибка удаления своего сообщения:", err)
			continue
		}
		deleted++
	}
	return deleted
}

// sendImageBytes отправляет фото (например, пересланный чек) в чат.
func (b *Bot) sendImageBytes(chat types.JID, data []byte) {
	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		fmt.Println("Ошибка загрузки фото в WhatsApp:", err)
		return
	}
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(uploaded.URL),
			Mimetype:      proto.String("image/jpeg"),
			FileLength:    proto.Uint64(uploaded.FileLength),
			FileSHA256:    uploaded.FileSHA256,
			FileEncSHA256: uploaded.FileEncSHA256,
			MediaKey:      uploaded.MediaKey,
			DirectPath:    proto.String(uploaded.DirectPath),
		},
	}
	resp, err := b.client.SendMessage(context.Background(), chat, msg)
	if err != nil {
		fmt.Println("Ошибка отправки фото:", err)
		return
	}
	b.rememberSent(chat, resp.ID)
}

// sendDocumentBytes отправляет документ (PDF-чек) из памяти.
func (b *Bot) sendDocumentBytes(chat types.JID, data []byte, fileName, mimetype string) {
	uploaded, err := b.client.Upload(context.Background(), data, whatsmeow.MediaDocument)
	if err != nil {
		fmt.Println("Ошибка загрузки документа в WhatsApp:", err)
		return
	}
	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto.String(uploaded.URL),
			Mimetype:      proto.String(mimetype),
			Title:         proto.String(fileName),
			FileName:      proto.String(fileName),
			FileLength:    proto.Uint64(uploaded.FileLength),
			FileSHA256:    uploaded.FileSHA256,
			FileEncSHA256: uploaded.FileEncSHA256,
			MediaKey:      uploaded.MediaKey,
			DirectPath:    proto.String(uploaded.DirectPath),
		},
	}
	resp, err := b.client.SendMessage(context.Background(), chat, msg)
	if err != nil {
		fmt.Println("Ошибка отправки документа:", err)
		return
	}
	b.rememberSent(chat, resp.ID)
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
