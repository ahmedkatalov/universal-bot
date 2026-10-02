// Понимание ответа БЕЗ свайпа: когда бот задал в группе вопрос («какая сумма?»,
// «чей чек?», «у кого наличка?»), а следом кто-то (владелец или другой) просто
// пишет ответ в чат, НЕ отвечая на сообщение бота — мы всё равно понимаем, что
// это ответ на последний открытый вопрос, и применяем его к нужному чеку/наличке.
// Берём только БАРЕ-ответ ровно на недостающее (голая сумма / голое ФИО), чтобы
// не перехватить обычный новый платёж в переписке.
package bot

import (
	"context"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/parser"
)

// contextAnswerWindow — сколько ответ без свайпа считается ответом на последний
// вопрос бота в группе. Короткое окно, чтобы не привязать случайное сообщение.
const contextAnswerWindow = 20 * time.Minute

// setOpenAsk запоминает последний вопрос бота в группе (для ответа без свайпа).
func (b *Bot) setOpenAsk(groupJID string, a openAsk) {
	a.at = time.Now()
	b.clarify.mu.Lock()
	b.clarify.lastAsk[groupJID] = a
	b.clarify.mu.Unlock()
}

// recentOpenAsk возвращает свежий открытый вопрос группы (или ok=false).
func (b *Bot) recentOpenAsk(groupJID string) (openAsk, bool) {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	a, ok := b.clarify.lastAsk[groupJID]
	if !ok || time.Since(a.at) > contextAnswerWindow {
		return openAsk{}, false
	}
	return a, true
}

func (b *Bot) clearOpenAsk(groupJID string) {
	b.clarify.mu.Lock()
	delete(b.clarify.lastAsk, groupJID)
	b.clarify.mu.Unlock()
}

// tryContextAnswer пытается понять сообщение как ответ БЕЗ свайпа на последний
// вопрос бота в группе. Возвращает true, если сообщение потреблено как ответ.
func (b *Bot) tryContextAnswer(ctx context.Context, chat types.JID, text string) bool {
	groupJID := chat.String()
	a, ok := b.recentOpenAsk(groupJID)
	if !ok {
		return false
	}
	switch a.kind {
	case "cash_dup":
		resolved, isNew := parseCashDupAnswer(text)
		if !resolved && b.assistant != nil {
			if r, n := b.aiCashDupDecision(ctx, text); r {
				resolved, isNew = r, n
			}
		}
		if !resolved {
			return false
		}
		found, amount, client, err := b.db.ResolveCashDup(ctx, a.txID, isNew)
		if err != nil || !found {
			return false
		}
		b.clearOpenAsk(groupJID)
		if isNew {
			b.sendText(chat, "Засчитал как новый платёж: наличка "+client+".")
			_ = amount
		} else {
			b.sendText(chat, "Понял, это повтор — не считаю: "+client+".")
		}
		return true

	case "cash_collector":
		if b.applyCashCollectorReply(ctx, chat, a.txID, text) {
			b.clearOpenAsk(groupJID)
			return true
		}
		return false

	case "receipt":
		// Берём ТОЛЬКО бесспорный бэр-ответ ровно на недостающее поле, чтобы не
		// перехватить обычный новый платёж («Магомед 5000») в переписке.
		amount := parser.ExtractAmount(text)
		name, _, deferToAI := clarifyNameFromReply(text)
		if deferToAI {
			return false
		}
		switch {
		case a.needAmount && !a.needName:
			// Нужна только сумма — принимаем, если это ПО СУТИ одна сумма (без ФИО).
			if amount <= 0 || name != "" {
				return false
			}
		case a.needName && !a.needAmount:
			// Нужно только ФИО — принимаем голое имя без суммы.
			if name == "" || amount > 0 {
				return false
			}
		default:
			// Не прочитали ни сумму, ни клиента — примем ФИО и/или сумму.
			if name == "" && amount <= 0 {
				return false
			}
		}
		handled, done := b.applyReceiptAnswer(ctx, chat, a.receiptWaID, text)
		if done {
			b.clearOpenAsk(groupJID)
		}
		return handled
	}
	return false
}

// applyReceiptAnswer применяет ответ владельца (ФИО и/или сумма) к чеку
// receiptWaID. handled — ответ распознан и что-то записано/поправлено; done —
// вопрос закрыт (всё заполнено). handled && !done — чего-то ещё не хватает
// (дали сумму без ФИО или наоборот), вопрос оставляем открытым. Используется и
// при ответе свайпом (handleClarifyReply), и при ответе без свайпа.
func (b *Bot) applyReceiptAnswer(ctx context.Context, chat types.JID, receiptWaID, text string) (handled, done bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if isConfirmReply(lower) {
		b.sendText(chat, "Понял, оставляю как есть.")
		return true, true
	}
	if isUnknownReply(lower) {
		b.sendText(chat, "Хорошо, оставлю этот чек в нераспознанных — вернёмся к нему позже.")
		return true, true
	}
	name, replyAmount, deferToAI := clarifyNameFromReply(text)
	if deferToAI {
		return false, false
	}
	if name == "" && replyAmount == 0 {
		return false, false
	}
	canonical := ""
	var contactIDPtr *int
	if name != "" {
		canonical, _ = b.aliases.ResolveName(name)
		cid, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			return false, false
		}
		contactIDPtr = &cid
	}
	found, amount, stillReview, err := b.db.FillReceiptByMessage(ctx, receiptWaID, canonical, contactIDPtr, replyAmount)
	if err != nil || !found {
		return false, false
	}
	if amount <= 0 {
		b.sendText(chat, "Записал клиента "+canonical+", но сумму по этому чеку так и не знаю — напишите сумму (например «15000»), и чек войдёт в сбор.")
		return true, false
	}
	if stillReview {
		b.sendText(chat, "Поправил сумму. А чей это чек? Напишите ФИО клиента — тогда засчитаю.")
		return true, false
	}
	if canonical != "" {
		b.sendText(chat, "Записал: чек — клиент "+canonical+".")
	} else {
		b.sendText(chat, "Поправил сумму чека.")
	}
	return true, true
}
