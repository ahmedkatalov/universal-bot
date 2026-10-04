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
	"go.mau.fi/whatsmeow/types/events"

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

// quotesCmfAsk — сообщение отвечает свайпом на сообщение бота по сверке чека.
func (b *Bot) quotesCmfAsk(ctx context.Context, msg *events.Message) bool {
	q := extractQuotedStanzaID(msg)
	if q == "" || b.db == nil {
		return false
	}
	refs, err := b.db.CmfWatchesByAsk(ctx, q)
	return err == nil && len(refs) > 0
}

// contextAnswerBlocked — открыт вопрос сверки «за кого платёж», но этот же
// человек ПОСЛЕ вопроса прислал новый чек без клиента: его имя — для нового
// чека, а не ответ на старый вопрос.
func (b *Bot) contextAnswerBlocked(ctx context.Context, msg *events.Message) bool {
	a, ok := b.recentOpenAsk(msg.Info.Chat.String())
	if !ok || a.kind != "cmf_watch" || b.db == nil {
		return false
	}
	has, err := b.db.HasUnconfirmedReceiptFrom(ctx, msg.Info.Chat.String(), msg.Info.Sender.String(), a.at)
	return err == nil && has
}

// cmfNameWindow — сколько после просьбы бота «напишите ФИО» голое ФИО без
// свайпа считается ответом (дольше — это уже, скорее, имя перед новым чеком).
const cmfNameWindow = 5 * time.Minute

// setOpenAskIfLatest — запомнить вопрос, только если в группе нет более нового
// (переспрос по старому чеку не должен перебивать свежий вопрос по другому).
func (b *Bot) setOpenAskIfLatest(groupJID string, a openAsk) {
	a.at = time.Now()
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	cur, ok := b.clarify.lastAsk[groupJID]
	if ok && time.Since(cur.at) <= contextAnswerWindow && !(cur.kind == a.kind && cur.watchID == a.watchID) {
		return
	}
	b.clarify.lastAsk[groupJID] = a
}

// clearOpenAskFor — снять открытый вопрос, только если он про это наблюдение.
func (b *Bot) clearOpenAskFor(groupJID string, watchID int) {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if cur, ok := b.clarify.lastAsk[groupJID]; ok && cur.kind == "cmf_watch" && cur.watchID == watchID {
		delete(b.clarify.lastAsk, groupJID)
	}
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

	case "cmf_watch":
		wantName := a.wantName && time.Since(a.at) < cmfNameWindow
		return b.applyCmfWatchAnswer(ctx, chat, a.watchID, text, modeContext, wantName)

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

// askAboutReceipt — переспрос по чеку (чего ещё не хватает): связываем его с
// чеком, чтобы ответ свайпом на ЭТОТ переспрос тоже применился к чеку.
func (b *Bot) askAboutReceipt(ctx context.Context, chat types.JID, receiptWaID, text string, needAmount, needName bool) {
	id := b.sendTextReturnID(chat, text)
	if id == "" {
		return
	}
	b.registerClarifyAsk(id, receiptWaID)
	_ = b.db.MarkReceiptAskedByMessage(ctx, receiptWaID, id)
	b.setOpenAsk(chat.String(), openAsk{kind: "receipt", receiptWaID: receiptWaID, needAmount: needAmount, needName: needName})
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
		b.askAboutReceipt(ctx, chat, receiptWaID, "Записал клиента "+canonical+", но сумму по этому чеку так и не знаю — напишите сумму (например «15000»), и чек войдёт в сбор.", true, false)
		return true, false
	}
	if stillReview {
		b.askAboutReceipt(ctx, chat, receiptWaID, "Поправил сумму. А чей это чек? Напишите ФИО клиента — тогда засчитаю.", false, true)
		return true, false
	}
	if canonical != "" {
		b.sendText(chat, "Записал: чек — клиент "+canonical+".")
	} else {
		b.sendText(chat, "Поправил сумму чека.")
	}
	return true, true
}
