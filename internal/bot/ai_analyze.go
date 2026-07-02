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

	"whatsapp-bot/internal/db"
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
// его в ```json ... ``` или добавлять пояснения до/после.
func extractJSONBlock(s string) string {
	if start := strings.Index(s, "["); start >= 0 {
		if end := strings.LastIndex(s, "]"); end > start {
			return s[start : end+1]
		}
	}
	if start := strings.Index(s, "{"); start >= 0 {
		if end := strings.LastIndex(s, "}"); end > start {
			return s[start : end+1]
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
// не тормозить обработку остальных сообщений.
func (b *Bot) aiRescueUnparsed(ctx context.Context, senderName string, lines []string, rawID int, txDate time.Time) {
	system := "Ты — модуль разбора платежей в WhatsApp-боте учёта финансов. " +
		"Тебе дают строки из сообщения рабочей группы, которые не смог разобрать обычный парсер. " +
		"Известные люди: " + strings.Join(b.aliases.Canonicals(), ", ") + ".\n" +
		"Верни СТРОГО JSON-массив объектов вида " +
		`[{"name":"Имя","amount":12345,"note":"аванс|премия|долг|","card":"втб|сбер|наличные|"}]` + ". " +
		"Включай ТОЛЬКО реальные платежи (кто-то сдал/перевёл/принёс деньги). Сумма — числом в рублях. " +
		"name — имя из списка известных, если уверенно совпадает; иначе как написано в сообщении. " +
		"Если платежей в строках нет (болтовня, время встречи, номер телефона и т.п.) — верни []."

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
	var payments []aiPayment
	if err := json.Unmarshal([]byte(block), &payments); err != nil {
		fmt.Printf("ИИ-доразбор: не удалось разобрать JSON (%v): %s\n", err, block)
		return
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
