// Фоновый повтор распознавания чеков: если в момент прихода чека модель зрения
// была недоступна (сбой API/сети), чек не теряется (сохранён как needs_review),
// а этот воркер позже перечитывает его — когда ИИ вернётся — и заполняет
// сумму/получателя, не заставляя работника пересылать чек заново.
package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// maxReceiptRetries — сколько раз перечитываем чек, прежде чем сдаться и оставить
// его на ручную проверку. Настраивается RECEIPT_RETRY_MAX (по умолчанию 6).
func maxReceiptRetries() int {
	if v := strings.TrimSpace(os.Getenv("RECEIPT_RETRY_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			return n
		}
	}
	return 6
}

// retryBackoff — растущая пауза между попытками перечитать чек.
func retryBackoff(attempt int) time.Duration {
	steps := []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(steps) {
		attempt = len(steps) - 1
	}
	return steps[attempt]
}

func (b *Bot) receiptRetryLoop() {
	if b.assistant == nil {
		return // без модели зрения перечитывать нечем
	}
	time.Sleep(45 * time.Second) // дать клиенту/ИИ подняться после старта
	b.retryDueReceipts()
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for range t.C {
		b.retryDueReceipts()
	}
}

func (b *Bot) retryDueReceipts() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	b.retryDueMedia(ctx)
	items, err := b.db.DueReceiptRetries(ctx, time.Now(), maxReceiptRetries(), 10)
	if err != nil {
		fmt.Println("Повтор чеков: ошибка выборки:", err)
		return
	}
	for _, it := range items {
		b.retryOneReceipt(ctx, it)
	}
}

func (b *Bot) retryOneReceipt(ctx context.Context, it db.ReceiptRetry) {
	max := maxReceiptRetries()
	if it.ReceiptVersion == "" {
		current, err := b.db.ReceiptRetryByID(ctx, it.ID)
		if err != nil {
			return
		}
		it = current
	}
	ext := filepath.Ext(it.MediaPath)
	data, err := os.ReadFile(it.MediaPath)
	if err != nil {
		if len(it.Proto) == 0 {
			_ = b.db.ResolveReceiptRetryIfCurrent(ctx, it, "failed", false)
			fmt.Printf("Повтор чека #%d: файл недоступен (%v), исходника для загрузки нет — оставляю на ручную проверку\n", it.ID, err)
			return
		}
		var m waProto.Message
		if err := proto.Unmarshal(it.Proto, &m); err != nil {
			_ = b.db.ResolveReceiptRetryIfCurrent(ctx, it, "failed", false)
			return
		}
		// A pending receipt must retain the same recovery path as raw media:
		// losing its local file does not mean the WhatsApp source is gone.
		switch {
		case m.GetImageMessage() != nil:
			ext = ".jpg"
			data, err = b.download(ctx, m.GetImageMessage())
		case m.GetDocumentMessage() != nil:
			doc := m.GetDocumentMessage()
			ext = ".jpg"
			if isPDFDocument(doc.GetMimetype(), doc.GetFileName()) {
				ext = ".pdf"
			}
			data, err = b.download(ctx, doc)
		default:
			_ = b.db.ResolveReceiptRetryIfCurrent(ctx, it, "failed", false)
			return
		}
		if err != nil {
			_ = b.db.BumpReceiptRetryIfCurrent(ctx, it, "файл недоступен, повтор загрузки не удался", time.Now().Add(retryBackoff(it.Attempts)), max)
			return
		}
		if path := b.saveMediaFile(it.WaID, data, ext); path != "" {
			if err := b.db.SetMediaPath(ctx, it.RawID, path); err != nil {
				fmt.Printf("Повтор чека #%d: не удалось сохранить путь к файлу: %v\n", it.ID, err)
			}
		}
	}
	rec, ok, reachable := b.aiVisionReceiptConsensus(ctx, data, ext, "")
	if !reachable {
		next := time.Now().Add(retryBackoff(it.Attempts))
		_ = b.db.BumpReceiptRetryIfCurrent(ctx, it, "зрение всё ещё недоступно", next, max)
		return
	}
	// ВАЖНО: ok=false у vision означает «не смог прочитать поля», а НЕ «это точно
	// не чек». Никогда не прячем/не удаляем чек по такому сигналу — иначе можно
	// потерять настоящий, но размытый чек. Оставляем needs_review, пробуем ещё;
	// после лимита -> 'failed', но чек ОСТАЁТСЯ видимым для ручной проверки.
	var rd parser.ReceiptData
	if ok {
		applyAIReceiptAuthoritative(&rd, rec)
	}
	// Банк перевод отклонил — денег нет: в сбор не идёт, как и при приёме чека
	// (даже если клиента назвали в подписи и сумма прочиталась).
	declined := ok && rec.Kind != "cash" && receiptDeclined(rd.Status)
	if !declined && (!ok || rec.Kind == "cash" || rd.Amount <= 0) {
		next := time.Now().Add(retryBackoff(it.Attempts))
		_ = b.db.BumpReceiptRetryIfCurrent(ctx, it, "перечитал, но чек не распознался", next, max)
		return
	}
	applied, err := b.db.FillReceiptFromRetryIfCurrent(ctx, it, db.RecognizedReceipt{
		Bank: rd.Bank, Recipient: rd.Recipient, Sender: rd.Sender,
		DocNumber: rd.DocNumber, AuthCode: rd.AuthCode, Status: rd.Status,
		Amount: rd.Amount, Commission: rd.Commission,
		TxDate: rd.TxTime, HasTxDate: rd.HasTxTime, TimeDefaulted: rd.TimeDefaulted, Declined: declined,
	})
	if err != nil {
		fmt.Printf("Повтор чека #%d: ошибка сохранения: %v\n", it.ID, err)
		return
	}
	if !applied {
		return // the receipt/source changed while the file was downloaded/read
	}
	if rd.HasTxTime && rd.TimeDefaulted {
		b.anchorStoredDateOnly(ctx, it.ID) // на чеке только дата — время по копии/тексту платежа
	}
	waID, senderJID, groupJID, _ := b.db.ReceiptReplyRef(ctx, it.ID)
	chat, jerr := types.ParseJID(groupJID)
	if declined {
		fmt.Printf("Повтор чека #%d: статус «%s» — перевод не прошёл, в сбор не идёт\n", it.ID, rd.Status)
		if jerr == nil && groupJID != "" {
			b.dropReceiptAsk(groupJID, waID) // про сумму этого чека больше не спрашиваем
			if !b.groupSilent(chat) {
				b.sendReply(chat, fmt.Sprintf("⚠️ Перечитал чек: на нём статус «%s» — перевод не прошёл, в сбор не засчитываю.", rd.Status), waID, senderJID)
			}
		}
		return
	}
	fmt.Printf("Повтор чека #%d: распознан (сумма %.0f ₽, получатель %q)\n", it.ID, rd.Amount, rd.Recipient)
	// Чек стал полным только сейчас — при приёме проверка «уже присылали» его
	// не поймала (не было суммы, номера). Тот же чек, уже засчитанный, — повтор.
	// Слабое совпадение («похоже, тот же чек») — с вопросом «тот же или
	// другой?»; в группе на молчании чек ждёт в «НЕ вошло», спросим потом.
	weak := db.WeakOff
	if jerr == nil && groupJID != "" {
		weak = b.weakSameMode(chat)
	}
	same, err := b.db.DedupeCompletedReceiptWeak(ctx, it.ID, true, weak)
	if err != nil {
		fmt.Printf("Повтор чека #%d: проверка повтора не удалась: %v\n", it.ID, err)
		return
	}
	if same.Found && jerr == nil && groupJID != "" {
		b.reportSameCheck(ctx, chat, waID, senderJID, same, true)
	}
	// Дочитали номер операции у чека с клиентом: тот же чек в другой группе
	// может быть записан на другого клиента — спросим «чей он?».
	if jerr == nil && groupJID != "" && !(same.Found && same.VictimIsThis) {
		b.crossCheckNamed(ctx, chat, waID, 0)
	}
}

// retryDueMedia — фото/PDF чеков, которые так и не стали чеком (не скачались
// или ИИ не смог их прочитать), пробуем снова — по очереди своей группы.
func (b *Bot) retryDueMedia(ctx context.Context) {
	if b.assistant != nil && b.assistant.Degraded() {
		return // ИИ всё ещё не отвечает — подождём
	}
	items, err := b.db.DueMediaRetries(ctx, 5)
	if err != nil {
		fmt.Println("Повтор фото: ошибка выборки:", err)
		return
	}
	for _, it := range items {
		it := it
		done := make(chan struct{})
		b.enqueueChat(it.GroupJID, func() {
			defer close(done)
			b.rereadMedia(ctx, it)
		})
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// rereadMedia — заново скачать (если нужно) и разобрать фото чека так же, как
// при получении. Возвращает, стало ли фото чеком.
func (b *Bot) rereadMedia(ctx context.Context, it db.MediaRetry) bool {
	chat, err := types.ParseJID(it.GroupJID)
	if err != nil {
		return false
	}
	text, ext := it.Body, filepath.Ext(it.MediaPath)
	var data []byte
	if it.MediaPath != "" {
		data, err = os.ReadFile(it.MediaPath)
	}
	if it.MediaPath == "" || err != nil {
		if len(it.Proto) == 0 {
			_ = b.db.BumpMediaRetry(ctx, it.RawID, time.Now().Add(retryBackoff(it.Attempts)))
			return false
		}
		var m waProto.Message
		if uerr := proto.Unmarshal(it.Proto, &m); uerr != nil {
			_ = b.db.BumpMediaRetry(ctx, it.RawID, time.Now().Add(24*time.Hour))
			return false
		}
		mr := b.readMedia(ctx, &m, it.WaID, it.Caption)
		if mr.dlErr != nil || mr.bytes == nil {
			_ = b.db.BumpMediaRetry(ctx, it.RawID, time.Now().Add(retryBackoff(it.Attempts)))
			return false
		}
		_ = b.db.SetMediaPath(ctx, it.RawID, mr.path)
		data, text, ext = mr.bytes, mr.text, mr.ext
		_ = b.db.SetMessageBody(ctx, it.RawID, text)
	}
	if ext == "" {
		ext = ".jpg"
	}
	payer, payerAmount := b.payerFromCaption(ctx, it.Caption)
	b.handleBankReceipt(ctx, chat, it.SenderJID, it.WaID, text, it.RawID, it.ReceivedAt, data, ext, payer, payerAmount)
	if _, _, ok, _ := b.db.ReceiptOfRaw(ctx, it.RawID); ok {
		return true
	}
	_ = b.db.BumpMediaRetry(ctx, it.RawID, time.Now().Add(retryBackoff(it.Attempts)))
	return false
}
