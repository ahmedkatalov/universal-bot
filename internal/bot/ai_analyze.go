// ИИ-доразбор: когда обычный (регэксповый) парсер не справился с сообщением
// или чеком, текст отдаётся модели через OpenRouter, и она извлекает
// структурированные данные. Так бот "чинит сам себя" на новых форматах,
// не дожидаясь ручного обновления парсера.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

// containsDigit — быстрый фильтр: строки без цифр (шутки, болтовня в группе)
// не гоняем через ИИ, там точно нет платежей.
func containsDigit(lines []string) bool {
	for _, l := range lines {
		for _, r := range l {
			if unicode.IsDigit(r) {
				return true
			}
		}
	}
	return false
}

// extractJSONBlock вырезает JSON из ответа модели — модели любят оборачивать
// его в ```json ... ``` или добавлять пояснения до/после. Берём ту скобку
// (массив или объект), которая встречается раньше: объект может содержать
// массивы внутри и наоборот.
func extractJSONBlock(s string) string {
	iArr := strings.Index(s, "[")
	iObj := strings.Index(s, "{")
	if iObj >= 0 && (iArr < 0 || iObj < iArr) {
		if end := strings.LastIndex(s, "}"); end > iObj {
			return s[iObj : end+1]
		}
	}
	if iArr >= 0 {
		if end := strings.LastIndex(s, "]"); end > iArr {
			return s[iArr : end+1]
		}
	}
	return ""
}

type aiPayment struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
	Card   string  `json:"card"`
}

// aiRescueUnparsed отдаёт нераспознанные строки группового сообщения модели
// и сохраняет платежи, которые она смогла извлечь. Вызывается в фоне, чтобы
// не тормозить обработку остальных сообщений. Модель отличает реальные
// операции от обсуждения денег; если сомневается — бот задаёт уточняющий
// вопрос прямо в группе, а не записывает наугад.
func (b *Bot) aiRescueUnparsed(ctx context.Context, chat types.JID, senderName string, lines []string, rawID int, txDate time.Time) {
	system := "Ты — модуль разбора платежей в WhatsApp-боте учёта финансов. " +
		"Тебе дают строки из сообщения рабочей группы, которые не смог разобрать обычный парсер. " +
		"Известные люди: " + strings.Join(b.aliases.Canonicals(), ", ") + ".\n\n" +
		"Верни СТРОГО один JSON-объект вида " +
		`{"payments":[{"name":"Имя","amount":12345,"note":"аванс|премия|долг|","card":"втб|сбер|наличные|"}],"clarify":""}` + ".\n\n" +
		"В payments включай ТОЛЬКО РЕАЛЬНО СОВЕРШЁННЫЕ операции — деньги фактически сдали/перевели/принесли: " +
		"«Ахмед скинул 5000», «пришло 25 тыщ от Миланы», «оплатил 10000 за аренду». " +
		"НЕ включай обсуждения, планы, вопросы и пересказы: «сказал взять 5000», «может нужно 10000», " +
		"«я ему говорил про 7к», «сколько будет 5000?», «надо собрать лям» — это НЕ операции, верни для них пустой payments. " +
		"Понимай сленг и разговорные суммы: 5к = 5000, 25 тыщ = 25000, лям = 1000000, полтора ляма = 1500000, " +
		"«косарь» = 1000. Сумма — числом в рублях. " +
		"name — имя из списка известных, если уверенно совпадает; иначе как написано в сообщении.\n\n" +
		"clarify — уточняющий вопрос ОДНОЙ короткой фразой, ТОЛЬКО если строка похожа на реальную операцию, " +
		"но непонятно ключевое (был ли платёж на самом деле, чьи это деньги, какая сумма). " +
		"ВАЖНО: строка либо в payments, либо в clarify — никогда одновременно. Если непонятно, от кого деньги, " +
		"НЕ записывай их на отправителя сообщения — задай clarify и оставь payments пустым. " +
		"Для болтовни и явных обсуждений clarify оставь пустым — не переспрашивай по пустякам."

	user := "Отправитель сообщения: " + senderName + "\nСтроки:\n" + strings.Join(lines, "\n")

	out, err := b.assistant.Complete(ctx, system, user)
	if err != nil {
		fmt.Println("ИИ-доразбор сообщения не удался:", err)
		return
	}
	block := extractJSONBlock(out)
	if block == "" {
		return
	}
	var parsed struct {
		Payments []aiPayment `json:"payments"`
		Clarify  string      `json:"clarify"`
	}
	if err := json.Unmarshal([]byte(block), &parsed); err != nil {
		fmt.Printf("ИИ-доразбор: не удалось разобрать JSON (%v): %s\n", err, block)
		return
	}
	payments := parsed.Payments

	if q := strings.TrimSpace(parsed.Clarify); q != "" {
		b.sendText(chat, "❓ "+q)
	}

	saved := 0
	for _, p := range payments {
		name := strings.TrimSpace(p.Name)
		if p.Amount <= 0 || name == "" {
			continue
		}
		canonical := b.aliases.Resolve(name)
		contactID, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			fmt.Println("ИИ-доразбор: ошибка контакта:", err)
			continue
		}
		err = b.db.InsertTransaction(ctx, db.TransactionInput{
			ContactID:    contactID,
			RawName:      name,
			Amount:       p.Amount,
			Note:         p.Note,
			CardTo:       p.Card,
			IsCash:       parser.IsCash(strings.Join(lines, " ") + " " + p.Note + " " + p.Card),
			RawMessageID: rawID,
			TxDate:       txDate,
		})
		if err != nil {
			fmt.Println("ИИ-доразбор: ошибка сохранения транзакции:", err)
			continue
		}
		saved++
	}
	if saved > 0 {
		fmt.Printf("ИИ-доразбор: сообщение %d — извлечено и сохранено %d платеж(ей), которые не понял обычный парсер\n", rawID, saved)
	}
}

type aiReceipt struct {
	Kind           string  `json:"kind"`            // "receipt" (чек), "cash" (фото наличных), "other" (не чек)
	Bank           string  `json:"bank"`            // банк чека (сторона получателя)
	Recipient      string  `json:"recipient"`       // ФИО получателя
	RecipientBank  string  `json:"recipient_bank"`  // банк получателя, если указан отдельно
	RecipientPhone string  `json:"recipient_phone"` // телефон получателя
	Sender         string  `json:"sender"`          // ФИО отправителя (плательщик, напечатан на чеке)
	SenderBank     string  `json:"sender_bank"`     // банк отправителя
	SenderAccount  string  `json:"sender_account"`  // счёт/карта отправителя
	Amount         float64 `json:"amount"`
	Commission     float64 `json:"commission"`
	DocNumber      string  `json:"doc_number"`
	AuthCode       string  `json:"auth_code"`
	Status         string  `json:"status"`
	Datetime       string  `json:"datetime"` // "YYYY-MM-DD HH:MM:SS", "YYYY-MM-DD HH:MM" или ""
}

// receiptSchemaJSON — форма JSON для ИИ-разбора чека (полный набор полей).
const receiptSchemaJSON = `{"kind":"receipt","bank":"","recipient":"","recipient_bank":"","recipient_phone":"","sender":"","sender_bank":"","sender_account":"","amount":0,"commission":0,"doc_number":"","auth_code":"","status":"","datetime":""}`

// receiptExtractRules — общие правила извлечения полей чека для ИИ.
const receiptExtractRules = "kind — что на изображении: 'receipt' (банковский чек/квитанция перевода), 'cash' (ФОТО НАЛИЧНЫХ ДЕНЕГ — купюры в руке/на столе, а не чек), 'other' (что-то иное). " +
	"recipient — ФИО ПОЛУЧАТЕЛЯ перевода (кому/на чью карту пришли деньги — владелец карты). " +
	"sender — ФИО отправителя/плательщика, КАК НАПЕЧАТАНО НА ЧЕКЕ. ВАЖНО: напечатанный на чеке отправитель — это НЕ обязательно клиент рассрочки (часто платят с чужой карты). Клиента определяет бот по подписи рядом, а не ты — просто верни, что напечатано. " +
	"recipient_bank/sender_bank — банки сторон, если указаны (например 'Банк получателя: Т-Банк'). " +
	"recipient_phone — телефон получателя, sender_account — счёт/карта отправителя (последние цифры). " +
	"amount — ГЛАВНАЯ сумма перевода числом в рублях, БЕЗ комиссии. Это крупное число вверху у слов 'Итого', " +
	"'Сумма перевода', 'Сумма операции', 'Сумма'. НЕ путай с остатком/балансом, комиссией или номером карты. " +
	"Если чисел несколько — бери именно сумму ПЕРЕВОДА (сколько ушло получателю). commission — комиссия числом. " +
	"datetime — дата и время операции с чека в формате YYYY-MM-DD HH:MM:SS (или YYYY-MM-DD HH:MM). " +
	"doc_number — номер документа/операции, auth_code — код авторизации, status — статус ('Выполнено' и т.п.). " +
	"Читай ВНИМАТЕЛЬНО, даже если фото размытое, под углом, тёмное или это скан — разбери, что можешь. " +
	"Кириллицу и цифры не путай (0/О, 3/З, 6/б). Заполняй ВСЕ поля, которые видишь; чего не видно — оставь пустым " +
	"(числа 0). Лучше оставить поле пустым, чем выдумать. Не придумывай данные, которых нет на изображении."

// aiRescueReceipt отдаёт OCR-текст чека модели, когда обычный парсер не смог
// вытащить сумму или получателя (нестандартная вёрстка, кривой OCR).
func (b *Bot) aiRescueReceipt(ctx context.Context, ocrText string) (aiReceipt, bool) {
	system := "Ты — модуль разбора банковских чеков в WhatsApp-боте учёта финансов. " +
		"Тебе дают текст, распознанный OCR со скриншота банковского перевода (текст может быть с ошибками распознавания). " +
		"Верни СТРОГО один JSON-объект вида " + receiptSchemaJSON + ". " + receiptExtractRules

	out, err := b.assistant.Complete(ctx, system, ocrText)
	if err != nil {
		fmt.Println("ИИ-доразбор чека не удался:", err)
		return aiReceipt{}, false
	}
	block := extractJSONBlock(out)
	if block == "" {
		return aiReceipt{}, false
	}
	var rec aiReceipt
	if err := json.Unmarshal([]byte(block), &rec); err != nil {
		fmt.Printf("ИИ-доразбор чека: не удалось разобрать JSON (%v): %s\n", err, block)
		return aiReceipt{}, false
	}
	if rec.Amount <= 0 && strings.TrimSpace(rec.Recipient) == "" {
		return aiReceipt{}, false // модель тоже ничего не нашла
	}
	return rec, true
}

// aiVisionReceipt показывает файл чека (фото или PDF) модели "глазами" —
// последний рубеж распознавания, когда OCR выдал кашу или вообще ничего.
// Claude читает чек прямо с изображения: банк, получатель, сумма, дата.
func (b *Bot) aiVisionReceipt(ctx context.Context, media []byte, ext string) (aiReceipt, bool) {
	if b.assistant == nil || len(media) == 0 {
		return aiReceipt{}, false
	}

	img := media
	mime := "image/jpeg"
	if ext == ".pdf" {
		rendered, err := renderPDFFirstPage(ctx, media)
		if err != nil {
			fmt.Println("Вижн-разбор: не удалось отрендерить PDF:", err)
			return aiReceipt{}, false
		}
		img, mime = rendered, "image/png"
	} else if len(img) >= 8 && string(img[:4]) == "\x89PNG" {
		mime = "image/png"
	}

	system := "Ты — модуль разбора изображений в боте учёта финансов. Тебе показывают ИЗОБРАЖЕНИЕ: обычно это чек/скриншот " +
		"банковского перевода, но иногда — ФОТО НАЛИЧНЫХ ДЕНЕГ (пачка купюр в руке/на столе). " +
		"Внимательно посмотри и верни СТРОГО один JSON-объект вида " + receiptSchemaJSON + ". " + receiptExtractRules +
		" Если это фото наличных денег — kind='cash' (сумму заполни, только если она явно видна/подписана, иначе 0). " +
		"Если это банковский чек — kind='receipt'. Если ни то ни другое — kind='other' и остальные поля пустыми."

	out, err := b.assistant.CompleteWithImage(ctx, system, "Что на изображении? Верни JSON.", img, mime)
	if err != nil {
		fmt.Println("Вижн-разбор чека не удался:", err)
		return aiReceipt{}, false
	}
	block := extractJSONBlock(out)
	if block == "" {
		return aiReceipt{}, false
	}
	var rec aiReceipt
	if err := json.Unmarshal([]byte(block), &rec); err != nil {
		fmt.Printf("Вижн-разбор: не удалось разобрать JSON (%v): %s\n", err, block)
		return aiReceipt{}, false
	}
	// Фото наличных — это валидный результат (наличка), даже без суммы/получателя.
	if rec.Kind == "cash" {
		fmt.Printf("Вижн-разбор: на фото НАЛИЧНЫЕ деньги (сумма с фото: %.0f)\n", rec.Amount)
		return rec, true
	}
	if rec.Amount <= 0 && strings.TrimSpace(rec.Recipient) == "" {
		return aiReceipt{}, false
	}
	fmt.Printf("Вижн-разбор: Claude прочитал чек с изображения (получатель %q, сумма %.0f)\n", rec.Recipient, rec.Amount)
	return rec, true
}

// receiptVisionFirst — читать фото-чеки СРАЗУ глазами Claude, не полагаясь на
// Tesseract-OCR (тот на кириллице часто выдаёт «правдоподобный мусор» с неверной
// суммой, и тогда зрение как фолбэк не включалось). По умолчанию ВКЛЮЧЕНО —
// это заметно точнее. Отключить: RECEIPT_VISION_FIRST=0.
func receiptVisionFirst() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("RECEIPT_VISION_FIRST")))
	if v == "" {
		return true
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// applyAIReceiptAuthoritative заполняет ReceiptData из ответа ИИ КАК ОСНОВНОЙ
// источник (перезаписывает поля) — когда чек прочитан вижном первым.
func applyAIReceiptAuthoritative(rd *parser.ReceiptData, rec aiReceipt) {
	if v := strings.TrimSpace(rec.Bank); v != "" {
		rd.Bank = v
	}
	if v := strings.TrimSpace(rec.Recipient); v != "" {
		rd.Recipient = v
	}
	if v := strings.TrimSpace(rec.RecipientBank); v != "" {
		rd.RecipientBank = v
	}
	if v := strings.TrimSpace(rec.RecipientPhone); v != "" {
		rd.RecipientPhone = v
	}
	if v := strings.TrimSpace(rec.Sender); v != "" {
		rd.Sender = v
	}
	if v := strings.TrimSpace(rec.SenderBank); v != "" {
		rd.SenderBank = v
	}
	if v := strings.TrimSpace(rec.SenderAccount); v != "" {
		rd.SenderAccount = v
	}
	if rec.Amount > 0 {
		rd.Amount = rec.Amount
	}
	if rec.Commission > 0 {
		rd.Commission = rec.Commission
	}
	if v := strings.TrimSpace(rec.DocNumber); v != "" {
		rd.DocNumber = v
	}
	if v := strings.TrimSpace(rec.AuthCode); v != "" {
		rd.AuthCode = v
	}
	if v := strings.TrimSpace(rec.Status); v != "" {
		rd.Status = v
	}
	if t, ok := parseAIDatetime(rec.Datetime); ok {
		rd.TxTime = t
		rd.HasTxTime = true
	}
}

// mergeAIReceipt дополняет ReceiptData недостающими полями из ответа ИИ.
func mergeAIReceipt(rd *parser.ReceiptData, rec aiReceipt) {
	if rd.Bank == "" {
		rd.Bank = rec.Bank
	}
	if rd.Recipient == "" {
		rd.Recipient = strings.TrimSpace(rec.Recipient)
	}
	if rd.Sender == "" {
		rd.Sender = strings.TrimSpace(rec.Sender)
	}
	if rd.RecipientBank == "" {
		rd.RecipientBank = strings.TrimSpace(rec.RecipientBank)
	}
	if rd.RecipientPhone == "" {
		rd.RecipientPhone = strings.TrimSpace(rec.RecipientPhone)
	}
	if rd.SenderBank == "" {
		rd.SenderBank = strings.TrimSpace(rec.SenderBank)
	}
	if rd.SenderAccount == "" {
		rd.SenderAccount = strings.TrimSpace(rec.SenderAccount)
	}
	if rd.Amount == 0 {
		rd.Amount = rec.Amount
	}
	if rd.Commission == 0 {
		rd.Commission = rec.Commission
	}
	if rd.DocNumber == "" {
		rd.DocNumber = rec.DocNumber
	}
	if rd.AuthCode == "" {
		rd.AuthCode = rec.AuthCode
	}
	if rd.Status == "" {
		rd.Status = rec.Status
	}
	if !rd.HasTxTime {
		if t, ok := parseAIDatetime(rec.Datetime); ok {
			rd.TxTime = t
			rd.HasTxTime = true
		}
	}
}

// parseAIDatetime разбирает дату/время из ответа модели.
func parseAIDatetime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
