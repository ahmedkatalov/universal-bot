// Импорт истории из WhatsApp (разовое восстановление данных после потери сервера).
// Включается ТОЛЬКО переменной IMPORT_HISTORY=1. При привязке устройства WhatsApp
// присылает недавнюю историю чатов (events.HistorySync) — мы МОЛЧА разбираем из неё
// чеки и текстовые платежи и пишем в базу. ВАЖНО: этот путь НИЧЕГО не отправляет в
// группы (никаких «чей чек?»/«не распознал») — иначе бот засыпал бы рабочие чаты
// сотнями старых сообщений. Ограничения: WhatsApp отдаёт лишь недавнее окно истории,
// старые медиа могут не скачаться, распознавание чеков тратит баланс OpenRouter.
package bot

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"

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

// requestGroupHistory просит ТЕЛЕФОН прислать более старые сообщения группы
// (on-demand history sync). Ответ придёт асинхронно событием HistorySync и
// разберётся обычным путём; итог уйдёт владельцу (replyTo). Best-effort: если
// телефон офлайн или истории больше нет — просто ничего не придёт.
func (b *Bot) requestGroupHistory(ctx context.Context, groupJID, replyTo types.JID, count int) error {
	if b.client == nil || b.client.Store.ID == nil {
		return fmt.Errorf("WhatsApp не подключён")
	}
	waID, ts, ok, err := b.db.OldestMessageInfo(ctx, groupJID.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("по этой группе у меня ещё нет сохранённых сообщений — не от чего отталкиваться")
	}
	anchor := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: groupJID, IsFromMe: false},
		ID:            waID,
		Timestamp:     ts,
	}
	// Окно приёма открываем ДО отправки, чтобы не упустить быстрый ответ телефона.
	b.onDemandMu.Lock()
	b.onDemandUntil = time.Now().Add(3 * time.Minute)
	b.onDemandReplyTo = replyTo
	b.onDemandMu.Unlock()

	req := b.client.BuildHistorySyncRequest(anchor, count)
	_, err = b.client.SendMessage(ctx, b.client.Store.ID.ToNonAD(), req, whatsmeow.SendRequestExtra{Peer: true})
	return err
}

func (b *Bot) importHistorySync(data *waHistorySync.HistorySync) (seenN, receiptsN, paymentsN int) {
	if data == nil {
		return 0, 0, 0
	}
	ctx := context.Background()
	var seen, receipts, payments int
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
			sender := chat
			if p := key.GetParticipant(); p != "" {
				if s, e := types.ParseJID(p); e == nil {
					sender = s
				}
			}
			text := extractText(content)
			img := content.GetImageMessage()
			doc := content.GetDocumentMessage()
			isPDFDoc := doc != nil && isPDFDocument(doc.GetMimetype(), doc.GetFileName())
			if text == "" && img == nil && !isPDFDoc {
				continue
			}
			seen++

			ts := time.Unix(int64(wmi.GetMessageTimestamp()), 0)
			waID := key.GetID()

			// Скачиваем медиа (если ещё доступно на серверах WhatsApp).
			var mediaBytes []byte
			var mediaPath, ext string
			switch {
			case img != nil:
				ext = ".jpg"
				if data, derr := b.client.Download(ctx, img); derr == nil {
					mediaBytes, mediaPath = data, b.saveMediaFile(waID, data, ext)
				}
			case isPDFDoc:
				ext = ".pdf"
				if data, derr := b.client.Download(ctx, doc); derr == nil {
					mediaBytes, mediaPath = data, b.saveMediaFile(waID, data, ext)
				}
			}

			// Реальный номер — только если участник в телефонной форме (не LID).
			senderPhone := ""
			if sender.Server == types.DefaultUserServer && sender.User != "" {
				senderPhone = sender.ToNonAD().String()
			}
			rawID, existed, err := b.db.SaveRawMessage(ctx, waID, chat.String(), sender.String(), senderPhone, sender.User, text, img != nil || isPDFDoc, mediaPath, ts)
			if err != nil || existed {
				continue // уже было (идемпотентно) или ошибка — не дублируем
			}
			if mediaBytes != nil {
				if b.importReceiptSilent(ctx, chat, rawID, ts, mediaBytes, ext) {
					receipts++
				}
			} else if text != "" {
				payments += b.importTextSilent(ctx, chat, rawID, ts, text)
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
// каких-либо сообщений в группу. Клиент — по печатному получателю (если он
// известен), иначе needs_review, чтобы владелец потом привязал вручную.
func (b *Bot) importReceiptSilent(ctx context.Context, chat types.JID, rawID int, received time.Time, media []byte, ext string) bool {
	if b.assistant == nil {
		return false
	}
	rec, ok, _ := b.aiVisionReceiptConsensus(ctx, media, ext, "")
	if !ok || rec.Kind == "cash" || rec.Kind == "other" || rec.Kind == "" {
		return false // не чек (или наличка/не распозналось) — молча пропускаем
	}
	var rd parser.ReceiptData
	applyAIReceiptAuthoritative(&rd, rec)
	if rd.Amount <= 0 {
		return false
	}
	txDate := received
	if rd.HasTxTime {
		txDate = rd.TxTime
	}
	canonical, matched := b.aliases.ResolveName(rd.Recipient)
	var contactIDPtr *int
	needsReview := true
	if matched {
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			contactIDPtr = &cid
			needsReview = false
		}
	}
	isDup, _, _ := b.db.FindDuplicateReceipt(ctx, chat.String(), rd.DocNumber, rd.AuthCode, contactIDPtr, rd.Recipient, rd.Amount, txDate)
	if err := b.db.InsertBankReceipt(ctx, db.BankReceiptInput{
		RawMessageID: rawID, Bank: rd.Bank, RecipientRaw: rd.Recipient, RecipientBank: rd.RecipientBank,
		RecipientPhone: rd.RecipientPhone, SenderRaw: rd.Sender, SenderBank: rd.SenderBank, SenderAccount: rd.SenderAccount,
		ContactID: contactIDPtr, Amount: rd.Amount, Commission: rd.Commission, DocNumber: rd.DocNumber, AuthCode: rd.AuthCode,
		Status: rd.Status, NeedsReview: needsReview, IsDuplicate: isDup, GroupJID: chat.String(), TxDate: txDate,
	}); err != nil {
		fmt.Println("Импорт истории: ошибка сохранения чека:", err)
		return false
	}
	return true
}

// importTextSilent разбирает текстовый платёж(и) и пишет их в базу БЕЗ сообщений.
func (b *Bot) importTextSilent(ctx context.Context, chat types.JID, rawID int, received time.Time, text string) int {
	res := parser.ParseMessage(text)
	saved := 0
	for _, tr := range res.Transactions {
		canonical, _ := b.aliases.ResolveName(tr.RawName)
		cid, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			continue
		}
		if err := b.db.InsertTransaction(ctx, dbTransactionFromParsed(tr, cid, rawID, received)); err == nil {
			saved++
		}
	}
	return saved
}
