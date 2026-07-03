// ИИ-доразбор: когда обычный (регэксповый) парсер не справился с сообщением
// или чеком, текст отдаётся модели через OpenRouter, и она извлекает
// структурированные данные. Так бот "чинит сам себя" на новых форматах,
// не дожидаясь ручного обновления парсера.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
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
	Bank      string  `json:"bank"`
	Recipient string  `json:"recipient"`
	Sender    string  `json:"sender"`
	Amount    float64 `json:"amount"`
	DocNumber string  `json:"doc_number"`
	AuthCode  string  `json:"auth_code"`
	Status    string  `json:"status"`
	Datetime  string  `json:"datetime"` // "YYYY-MM-DD HH:MM:SS", "YYYY-MM-DD HH:MM" или ""
}

// aiRescueReceipt отдаёт OCR-текст чека модели, когда обычный парсер не смог
// вытащить сумму или получателя (нестандартная вёрстка, кривой OCR).
func (b *Bot) aiRescueReceipt(ctx context.Context, ocrText string) (aiReceipt, bool) {
	system := "Ты — модуль разбора банковских чеков в WhatsApp-боте учёта финансов. " +
		"Тебе дают текст, распознанный OCR со скриншота банковского перевода (текст может быть с ошибками распознавания). " +
		"Верни СТРОГО один JSON-объект вида " +
		`{"bank":"","recipient":"","sender":"","amount":0,"doc_number":"","auth_code":"","status":"","datetime":""}` + ". " +
		"recipient — ФИО получателя перевода (кому пришли деньги), sender — ФИО отправителя. " +
		"amount — сумма перевода числом в рублях (без комиссии). " +
		"datetime — дата и время операции с чека в формате YYYY-MM-DD HH:MM:SS (или YYYY-MM-DD HH:MM, если секунд нет). " +
		"Неизвестные поля оставь пустыми (amount — 0). Не выдумывай данные, которых нет в тексте."

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

	system := "Ты — модуль разбора банковских чеков. Тебе показывают ИЗОБРАЖЕНИЕ чека/скриншота банковского перевода. " +
		"Внимательно прочитай его и верни СТРОГО один JSON-объект вида " +
		`{"bank":"","recipient":"","sender":"","amount":0,"doc_number":"","auth_code":"","status":"","datetime":""}` + ". " +
		"recipient — ФИО получателя перевода (кому пришли деньги), sender — ФИО отправителя. " +
		"amount — сумма перевода числом в рублях (без комиссии). " +
		"datetime — дата и время операции с чека в формате YYYY-MM-DD HH:MM:SS (или YYYY-MM-DD HH:MM). " +
		"Неизвестные поля оставь пустыми (amount — 0). Не выдумывай данные, которых нет на изображении. " +
		"Если это вообще не банковский чек — верни все поля пустыми."

	out, err := b.assistant.CompleteWithImage(ctx, system, "Прочитай этот чек и верни JSON.", img, mime)
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
	if rec.Amount <= 0 && strings.TrimSpace(rec.Recipient) == "" {
		return aiReceipt{}, false
	}
	fmt.Printf("Вижн-разбор: Claude прочитал чек с изображения (получатель %q, сумма %.0f)\n", rec.Recipient, rec.Amount)
	return rec, true
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
	if rd.Amount == 0 {
		rd.Amount = rec.Amount
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
