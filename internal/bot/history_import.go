// Импорт истории из WhatsApp (разовое восстановление данных после потери сервера).
// Включается ТОЛЬКО переменной IMPORT_HISTORY=1. При привязке устройства WhatsApp
// присылает недавнюю историю чатов (events.HistorySync) — мы МОЛЧА разбираем из неё
// чеки и текстовые платежи и пишем в базу. ВАЖНО: этот путь НИЧЕГО не отправляет в
// группы (никаких «чей чек?»/«не распознал») — иначе бот засыпал бы рабочие чаты
// сотнями старых сообщений. Ограничения: WhatsApp отдаёт лишь недавнее окно истории,
// старые медиа могут не скачаться, распознавание чеков тратит баланс ИИ-сервиса.
package bot

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

// onDemandHistoryActive — открыто ли окно приёма on-demand истории (по команде
// владельца) и кому слать итог.
func (b *Bot) onDemandHistoryActive() (bool, types.JID) {
	b.onDemandMu.Lock()
	defer b.onDemandMu.Unlock()
	if time.Now().Before(b.onDemandUntil) {
		return true, b.onDemandReplyTo
	}
	return false, types.JID{}
}

// requestGroupHistory просит ТЕЛЕФОН прислать сообщения группы, которых у
// бота нет: перед первым сообщением после самого большого перерыва за 2
// недели (бот был отключён), а если перерыва нет — перед самым свежим. Ответ
// придёт асинхронно событием HistorySync и разберётся обычным путём; итог
// (даже «ничего не пришло») уйдёт владельцу (replyTo), когда окно закроется.
func (b *Bot) requestGroupHistory(ctx context.Context, groupJID, replyTo types.JID, count int) (string, error) {
	if b.client == nil || b.client.Store.ID == nil {
		return "", fmt.Errorf("WhatsApp не подключён — историю попросить не у кого")
	}
	waID, ts, gap, ok, err := b.db.GapAnchor(ctx, groupJID.String(), 14)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("по этой группе у меня ещё нет сохранённых сообщений — не от чего отталкиваться")
	}
	anchor := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: groupJID, IsFromMe: false},
		ID:            waID,
		Timestamp:     ts,
	}
	// Окно приёма открываем ДО отправки, чтобы не упустить быстрый ответ телефона.
	b.onDemandMu.Lock()
	b.onDemandUntil = time.Now().Add(15 * time.Minute)
	b.onDemandReplyTo = replyTo
	startTimer := !b.onDemandTimer
	if startTimer {
		b.onDemandTimer = true
		b.onDemandGot = 0
	}
	b.onDemandMu.Unlock()
	if startTimer {
		go b.onDemandReport()
	}

	req := b.client.BuildHistorySyncRequest(anchor, count)
	if _, err = b.client.SendMessage(ctx, b.client.Store.ID.ToNonAD(), req, whatsmeow.SendRequestExtra{Peer: true}); err != nil {
		fmt.Println("История с телефона: запрос не ушёл:", err)
		return "", fmt.Errorf("запрос телефону не ушёл: %v", err)
	}
	if gap > 0 {
		return fmt.Sprintf("попросил телефон прислать сообщения до %s (перед перерывом %s — бот, похоже, был отключён)",
			ts.In(time.Local).Format("02.01 15:04"), gap.Round(time.Hour)), nil
	}
	return "попросил телефон прислать последние сообщения, которых у меня нет", nil
}

// onDemandReport — когда окно приёма истории закрылось, сказать владельцу итог.
func (b *Bot) onDemandReport() {
	for {
		b.onDemandMu.Lock()
		until := b.onDemandUntil
		b.onDemandMu.Unlock()
		if d := time.Until(until); d > 0 {
			time.Sleep(d)
			continue
		}
		break
	}
	b.onDemandMu.Lock()
	got, to := b.onDemandGot, b.onDemandReplyTo
	b.onDemandTimer = false
	b.onDemandMu.Unlock()
	// Пришедшие пачки уже отчитаны сразу; молчим только если не пришло ничего.
	if got == 0 && !to.IsEmpty() {
		b.sendText(to, "История с телефона так и не пришла (телефон с WhatsApp бота должен быть онлайн). Можно повторить «перечитай группу» позже.")
	}
}

func (b *Bot) importHistorySync(data *waHistorySync.HistorySync) (seenN, receiptsN, paymentsN int) {
	if data == nil {
		return 0, 0, 0
	}
	ctx := context.Background()
	names := make(map[string]string)
	for _, p := range data.GetPushnames() {
		names[p.GetID()] = p.GetPushname()
	}
	var seen, receipts, payments int
	aiBudget := 60 // столько текстов за импорт читает ИИ (дальше — только явные платежи)
	for _, conv := range data.GetConversations() {
		msgs := conv.GetMessages()
		// По времени сообщения (возрастание) — чтобы порядок совпадал с реальным.
		sort.SliceStable(msgs, func(i, j int) bool {
			return histTS(msgs[i]) < histTS(msgs[j])
		})
		for _, hm := range msgs {
			wmi := hm.GetMessage()
			if wmi == nil {
				continue
			}
			key := wmi.GetKey()
			if key == nil || key.GetFromMe() {
				continue // свои сообщения не учитываем
			}
			chat, err := types.ParseJID(key.GetRemoteJID())
			if err != nil || chat.Server != types.GroupServer {
				continue // импортируем только рабочие группы, не личку
			}
			if !b.isAllowedGroup(chat) {
				continue
			}
			content := wmi.GetMessage()
			if content == nil {
				continue
			}
			// Match whatsmeow.ParseWebMessage precedence. The top-level
			// participant is authoritative; a group itself is never an employee.
			participant := wmi.GetParticipant()
			if participant == "" {
				participant = key.GetParticipant()
			}
			sender, parseErr := types.ParseJID(participant)
			if parseErr != nil || sender.User == "" || (sender.Server != types.DefaultUserServer && sender.Server != types.HiddenUserServer) {
				sender = types.JID{}
			}
			if err := b.rememberMessageSender(ctx, types.MessageInfo{MessageSource: types.MessageSource{Sender: sender}}); err != nil {
				continue
			}
			// Protocol events must be applied even though they have no text/media.
			if prot := content.GetProtocolMessage(); prot != nil {
				switch prot.GetType() {
				case waProto.ProtocolMessage_MESSAGE_EDIT:
					em := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: true},
						ID: key.GetID(), Timestamp: time.Unix(int64(wmi.GetMessageTimestamp()), 0)}, Message: content}
					if b.applyEdit(ctx, em, prot, editQuiet{budget: &aiBudget}) {
						seen++
					}
					continue
				case waProto.ProtocolMessage_REVOKE:
					if b.importRevokeQuiet(ctx, chat, prot, time.Unix(int64(wmi.GetMessageTimestamp()), 0)) {
						seen++
					}
					continue
				}
			}
			text := extractText(content)
			img := content.GetImageMessage()
			doc := content.GetDocumentMessage()
			isPDFDoc := doc != nil && isPDFDocument(doc.GetMimetype(), doc.GetFileName())
			isImageDoc := doc != nil && strings.HasPrefix(strings.ToLower(doc.GetMimetype()), "image/")
			hasMedia := img != nil || isPDFDoc || isImageDoc
			if text == "" && !hasMedia {
				continue
			}
			seen++
			ts := time.Unix(int64(wmi.GetMessageTimestamp()), 0)
			waID := key.GetID()

			// Реальный номер — только если участник в телефонной форме (не LID).
			senderPhone := ""
			if sender.Server == types.DefaultUserServer && sender.User != "" {
				senderPhone = sender.ToNonAD().String()
			} else if actor, phone, err := b.db.ResolveSenderIdentity(ctx, sender.String()); err == nil && phone != "" {
				senderPhone = actor
			}
			senderName := wmi.GetPushName()
			if senderName == "" {
				senderName = names[sender.ToNonAD().String()]
			}
			if senderName == "" && b.client != nil && b.client.Store != nil && b.client.Store.Contacts != nil {
				if contact, err := b.client.Store.Contacts.GetContact(ctx, sender); err == nil {
					for _, name := range []string{contact.FullName, contact.FirstName, contact.PushName, contact.BusinessName} {
						if name != "" {
							senderName = name
							break
						}
					}
				}
			}
			if senderName == "" {
				senderName = sender.User
			}
			// Commit the original caption before network/AI work. A failed
			// download must remain an inspectable, retryable media message.
			rawID, existed, err := b.db.SaveRawMessage(ctx, waID, chat.String(), sender.String(), senderPhone, senderName, text, hasMedia, "", ts)
			if err != nil {
				continue
			}
			if hasMedia {
				if existed && !b.db.NeedsMediaReread(ctx, rawID) {
					continue
				}
				payload, err := proto.Marshal(content)
				if err != nil {
					continue
				}
				if err := b.db.SetMediaInfo(ctx, rawID, text, payload); err != nil {
					continue
				}
				var mediaBytes []byte
				var mediaPath string
				ext := ".jpg"
				if isPDFDoc {
					ext = ".pdf"
				}
				// Repeated history may repair recognition without downloading a
				// file again when its original local bytes already exist.
				if existed {
					if path, err := b.db.UnclearMediaPath(ctx, "message", rawID); err == nil && path != "" {
						if data, err := os.ReadFile(path); err == nil {
							mediaBytes, mediaPath = data, path
						}
					}
				}
				if mediaBytes == nil {
					var data []byte
					var err error
					if img != nil {
						data, err = b.download(ctx, img)
					} else {
						data, err = b.download(ctx, doc)
					}
					if err != nil {
						continue
					}
					mediaBytes, mediaPath = data, b.saveMediaFile(waID, data, ext)
				}
				if err := b.db.SetMediaPath(ctx, rawID, mediaPath); err != nil {
					continue
				}
				saved, complete := b.importReceiptReadSilent(ctx, chat, rawID, ts, mediaBytes, ext)
				if saved {
					receipts++
				}
				if complete {
					_ = b.db.MarkMessageParsed(ctx, rawID)
				}
				continue
			}
			if existed {
				continue
			}
			// Обращения к боту («Джарвис запиши …») и ответы на его сообщения — это
			// команды и ответы на вопросы, а не платежи: их уже выполнили вживую.
			em := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: true}, ID: waID, Timestamp: ts}, Message: content}
			// Ответ на вопрос бота, данный, пока бот был отключён, — применяем молча.
			if q := extractQuotedStanzaID(em); q != "" {
				_ = b.db.SetQuotedID(ctx, rawID, q)
				if b.applyAnswerQuiet(ctx, chat, q, b.stripBotMention(text), rawID) {
					continue
				}
			}
			// ФИО следом за чеком того же сотрудника — клиент этого чека (как вживую).
			if !sender.IsEmpty() && importNameOf(text) != "" && !strings.Contains(text, "\n") {
				if wa, ok, _ := b.db.UnnamedReceiptOfSenderBefore(ctx, chat.String(), sender.String(), ts, 15*time.Minute); ok {
					if canonical, ok := b.confirmClientName(ctx, importNameOf(text)); ok {
						var cid *int
						if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
							cid = &id
						}
						if found, _, _ := b.db.ReattributeReceiptByMessageFrom(ctx, wa, canonical, cid, rawID); found {
							_ = b.db.MarkMessageParsed(ctx, rawID)
							continue
						}
					}
				}
			}
			if stripped, addressed := b.stripBotName(text); addressed || b.isReplyToBot(em) {
				// Платёж сотрудника, написанный боту, вживую записывается как обычный —
				// и при импорте тоже. Остальное (команды, ответы) уже выполнено.
				if !addressed {
					stripped = text
				}
				if !b.employeePayment(em, stripped) {
					_ = b.db.MarkMessageParsed(ctx, rawID)
					continue
				}
				text = stripped
			}
			if text != "" {
				n, viaAI := b.importTextSilent(ctx, chat, senderName, rawID, ts, text, &aiBudget)
				payments += n
				if viaAI {
					continue
				}
			}
			_ = b.db.MarkMessageParsed(ctx, rawID)
		}
	}
	fmt.Printf("Импорт истории завершён: разобрано сообщений %d, чеков %d, текстовых платежей %d\n", seen, receipts, payments)
	return seen, receipts, payments
}

func histTS(hm *waHistorySync.HistorySyncMsg) uint64 {
	if hm == nil || hm.GetMessage() == nil {
		return 0
	}
	return hm.GetMessage().GetMessageTimestamp()
}

// importReceiptSilent распознаёт чек с изображения/PDF и пишет его в базу БЕЗ
// каких-либо сообщений в группу. Клиент — по подписи/ФИО рядом с чеком;
// без такого подтверждения needs_review, чтобы владелец привязал вручную.
func (b *Bot) importReceiptSilent(ctx context.Context, chat types.JID, rawID int, received time.Time, media []byte, ext string) bool {
	saved, _ := b.importReceiptReadSilent(ctx, chat, rawID, received, media, ext)
	return saved
}

// complete means either a saved receipt or an explicit non-receipt verdict.
// API failures, malformed extraction, and missing amounts must remain open.
func (b *Bot) importReceiptReadSilent(ctx context.Context, chat types.JID, rawID int, received time.Time, media []byte, ext string) (saved, complete bool) {
	if b.holdUnverifiedPDF(ctx, chat, "", "", rawID, media, ext, true) {
		return false, false
	}
	if b.assistant == nil {
		return false, false
	}
	rec, ok, reachable := b.aiVisionReceiptConsensus(ctx, media, ext, "")
	if reachable && (rec.Kind == "cash" || rec.Kind == "other") {
		return false, true
	}
	if !ok || rec.Kind != "receipt" {
		return false, false
	}
	var rd parser.ReceiptData
	applyAIReceiptAuthoritative(&rd, rec)
	if rd.Amount <= 0 {
		return false, false
	}
	txDate := received
	if rd.HasTxTime {
		txDate = rd.TxTime
	}
	// Клиент — ФИО рядом с чеком (подпись или сообщение сотрудника прямо перед
	// чеком), как вживую; печатный получатель — это владелец карты, не клиент.
	body, senderName, err := b.db.RawBodyByID(ctx, rawID)
	if err != nil {
		return false, false
	}
	sender, err := b.db.SenderJIDByRawID(ctx, rawID)
	if err != nil {
		return false, false
	}
	cands := []string{body}
	if sender != "" {
		before, err := b.db.SenderTextsBefore(ctx, chat.String(), sender, received, 15*time.Minute)
		if err != nil {
			return false, false
		}
		cands = append(cands, before...)
	}
	client := ""
	for _, c := range cands {
		n, _ := b.payerFromCaption(ctx, c)
		if n == "" {
			n = importNameOf(c)
		}
		if n != "" {
			if canonical, ok := b.confirmClientName(ctx, n); ok {
				client = canonical
				break
			}
		}
	}
	var contactIDPtr *int
	recipient, cardOwner := rd.Recipient, ""
	needsReview := true
	if client != "" {
		cid, err := b.db.GetOrCreateContact(ctx, client)
		if err != nil {
			return false, false
		}
		contactIDPtr = &cid
		recipient, cardOwner, needsReview = client, rd.Recipient, false
	}
	dateOnly := rd.HasTxTime && rd.TimeDefaulted
	if dateOnly {
		txDate = b.dateOnlyOpTime(ctx, 0, db.PrintedReceipt{
			Recipient: rd.Recipient, Sender: rd.Sender, Bank: rd.Bank,
			DocNumber: rd.DocNumber, AuthCode: rd.AuthCode, Amount: rd.Amount, TxDate: rd.TxTime,
		}, client, received)
	}
	isDup, _, err := b.db.FindDuplicateReceipt(ctx, chat.String(), rd.DocNumber, rd.AuthCode, contactIDPtr, recipient, rd.Amount, txDate, rd.TxTimeGuessed())
	if err != nil {
		fmt.Println("Импорт истории: ошибка проверки повтора чека:", err)
		return false, false
	}
	if err := b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
		RawMessageID: rawID, Bank: rd.Bank, RecipientRaw: recipient, RecipientBank: rd.RecipientBank, CardOwner: cardOwner,
		RecipientPhone: rd.RecipientPhone, SenderRaw: rd.Sender, SenderBank: rd.SenderBank, SenderAccount: rd.SenderAccount,
		ContactID: contactIDPtr, Amount: rd.Amount, Commission: rd.Commission, DocNumber: rd.DocNumber, AuthCode: rd.AuthCode,
		Status: rd.Status, NeedsReview: needsReview, IsDuplicate: isDup, GroupJID: chat.String(), TxDate: txDate,
		ClientConfirmed: client != "", SubmittedBy: senderName, TxTimeGuessed: !rd.HasTxTime, TxDateOnly: dateOnly,
	}); err != nil {
		fmt.Println("Импорт истории: ошибка сохранения чека:", err)
		return false, false
	}
	if client == "" {
		_ = b.db.MarkReceiptsAskedByRaw(ctx, rawID) // импорт молчит: про старые чеки не спрашиваем
	}
	return true, true
}

// importRevokeQuiet uses the same ledger deletion/undo machinery as live
// events, without announcing historical changes or opening new group questions.
func (b *Bot) importRevokeQuiet(ctx context.Context, chat types.JID, prot *waProto.ProtocolMessage, received time.Time) bool {
	waID := prot.GetKey().GetID()
	if waID == "" {
		return false
	}
	if group := prot.GetKey().GetRemoteJID(); group != "" && group != chat.String() {
		return false
	}
	if copyID, ok, err := b.db.CopyBySentID(ctx, waID); err == nil && ok {
		waID = copyID
	}
	ref, err := b.db.RawMessageByWaID(ctx, waID)
	if err != nil {
		return false
	}
	if !ref.Found {
		// History batches may arrive newest first: keep a tombstone so an
		// older batch cannot resurrect a payment already deleted in WhatsApp.
		if _, _, err := b.db.SaveRawMessage(ctx, waID, chat.String(), prot.GetKey().GetParticipant(), "", "", "", false, "", received); err != nil {
			return false
		}
		ref, err = b.db.RawMessageByWaID(ctx, waID)
	}
	if err != nil || !ref.Found || ref.Deleted || ref.GroupJID != chat.String() {
		return false
	}
	if _, _, err := b.db.MarkMessageDeleted(ctx, waID); err != nil {
		fmt.Println("Импорт истории: не удалось применить удаление:", err)
		return false
	}
	return true
}

// importNameOf — ФИО из подписи или отдельного сообщения (без сумм).
func importNameOf(text string) string {
	t := strings.TrimSpace(text)
	if t == "" || parser.ExtractAmount(t) > 0 && !strings.Contains(t, "\n") {
		return ""
	}
	if n, ok := looksLikeName(t); ok {
		return n
	}
	if n, ok := firstNameLine(t); ok {
		return n
	}
	return ""
}

// importTextSilent разбирает текстовый платёж(и) и пишет их в базу БЕЗ сообщений.
func (b *Bot) importTextSilent(ctx context.Context, chat types.JID, senderName string, rawID int, received time.Time, text string, aiBudget *int) (saved int, viaAI bool) {
	// Как вживую: платёж понимает ИИ (наличка «у Дени», планы «завтра занесёт»
	// не платёж), только молча — без вопросов в группу.
	if b.assistant != nil && *aiBudget > 0 && mightBePayment(text) {
		*aiBudget--
		before, _ := b.db.TxIDsByRawID(ctx, rawID)
		b.aiRescueUnparsed(ctx, chat, senderName, []string{text}, rawID, received, false, false)
		after, _ := b.db.TxIDsByRawID(ctx, rawID)
		return len(after) - len(before), true
	}
	// Без ИИ (или лимит исчерпан) — только явные платежи известным клиентам.
	n, _ := b.recordDeterministicPaymentsMode(ctx, text, rawID, received, false, detKnown)
	return n, false
}

// applyAnswerQuiet — ответ (свайп) на вопрос бота из истории: применяем без
// сообщений в группу. true — это был ответ на известный вопрос.
func (b *Bot) applyAnswerQuiet(ctx context.Context, chat types.JID, quoted, text string, rawID int) bool {
	done := func(kind, target string) bool {
		_ = b.db.SetAnswerTarget(ctx, rawID, kind, target)
		return true
	}
	if waID, ok, _ := b.db.ReceiptWaIDByAskMsg(ctx, quoted); ok {
		name, amount, deferToAI := clarifyNameFromReply(text)
		if deferToAI || name == "" && amount <= 0 {
			return false
		}
		canonical := ""
		var cid *int
		if name != "" {
			c, ok := b.confirmClientName(ctx, name)
			if !ok {
				return false
			}
			canonical = c
			if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
				cid = &id
			}
		}
		if found, _, _, err := b.db.FillReceiptByMessage(ctx, waID, canonical, cid, amount); err == nil && found {
			_ = b.db.SetReceiptSourceByMessage(ctx, waID, rawID)
			return done("receipt", waID)
		}
		return false
	}
	if txID, ok, _ := b.db.DupTxByAskMsg(ctx, quoted); ok {
		if resolved, isNew := parseCashDupAnswer(text); resolved {
			if found, _, _, err := b.resolveCashDup(ctx, txID, isNew); err == nil && found {
				return done("dup", strconv.Itoa(txID))
			}
		}
		return false
	}
	if txID, ok, _ := b.db.TxByCollectorAskMsg(ctx, quoted); ok {
		if c := extractCollectorName(text); c != "" {
			if found, _, _, err := b.db.SetTxCollector(ctx, txID, c, false); err == nil && found {
				return done("collector", strconv.Itoa(txID))
			}
		}
		return false
	}
	if w, ok, _ := b.db.WhoAskByMsg(ctx, quoted); ok {
		name, _, deferToAI := clarifyNameFromReply(text)
		if deferToAI || name == "" {
			return false
		}
		canonical, ok := b.confirmClientName(ctx, name)
		if !ok {
			return false
		}
		cid, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			return false
		}
		live, _ := b.db.TxRefsByRawID(ctx, w.RawID)
		var ph []db.TxRef
		for _, tx := range live {
			if whoPlaceholders[tx.Name] {
				ph = append(ph, tx)
			}
		}
		if len(ph) != 1 {
			return false
		}
		if b.db.SetTxClientFrom(ctx, ph[0].ID, cid, canonical, rawID) == nil {
			_ = b.db.ClearWhoAsk(ctx, w.RawID)
			return done("who", strconv.Itoa(w.RawID))
		}
	}
	return false
}
