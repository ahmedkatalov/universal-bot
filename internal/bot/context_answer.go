// Понимание ответа БЕЗ свайпа: когда бот задал в группе вопрос («какая сумма?»,
// «чей чек?», «у кого наличка?»), а следом кто-то (владелец или другой) просто
// пишет ответ в чат, НЕ отвечая на сообщение бота — мы всё равно понимаем, что
// это ответ на последний открытый вопрос, и применяем его к нужному чеку/наличке.
// Берём только БАРЕ-ответ ровно на недостающее (голая сумма / голое ФИО), чтобы
// не перехватить обычный новый платёж в переписке.
package bot

import (
	"context"
	"fmt"
	"math"
	"strconv"
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
	if a.kind == "receipt" && a.receiptWaID != "" {
		q := b.clarify.receiptAsks[groupJID]
		var rest []openAsk
		for _, x := range q {
			if x.receiptWaID != a.receiptWaID {
				rest = append(rest, x)
			}
		}
		b.clarify.receiptAsks[groupJID] = append(rest, a)
	}
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
	if !ok || (a.kind != "cmf_watch" && a.kind != "receipt") || b.db == nil {
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
	return b.tryContextAnswerAs(ctx, chat, "", true, text)
}

// tryContextAnswerFrom — ответ без свайпа от конкретного участника: на вопрос о
// ЧУЖОМ чеке/наличке принимаем только от того, кто его прислал, или от владельца.
func (b *Bot) tryContextAnswerFrom(ctx context.Context, msg *events.Message, text string) bool {
	return b.tryContextAnswerAs(ctx, msg.Info.Chat, msg.Info.Sender.String(), b.isReportAdmin(msg.Info), text)
}

// shortPlainReply — короткий ответ без сумм (не новый платёж и не фраза).
func shortPlainReply(text string, maxWords int) bool {
	if parser.ExtractAmount(text) > 0 || strings.ContainsAny(text, "0123456789") {
		return false
	}
	n := len(strings.Fields(text))
	return n > 0 && n <= maxWords
}

func (b *Bot) tryContextAnswerAs(ctx context.Context, chat types.JID, sender string, admin bool, text string) bool {
	groupJID := chat.String()
	a, ok := b.recentOpenAsk(groupJID)
	if !ok {
		return false
	}
	if a.kind != "cmf_watch" && sender != "" && a.senderJID != "" && sender != a.senderJID && !admin {
		return false // вопрос о чужом чеке/наличке — без свайпа отвечает его отправитель или владелец
	}
	switch a.kind {
	case "cash_dup":
		// Без свайпа — только короткий ответ без сумм («новый», «тот же»): иначе
		// новый платёж «Усманов 5000» решил бы судьбу чужой налички.
		if !shortPlainReply(text, 4) {
			return false
		}
		resolved, isNew := parseCashDupAnswer(text)
		if !resolved {
			return false
		}
		found, amount, client, err := b.db.ResolveCashDup(ctx, a.txID, isNew)
		if err != nil || !found {
			return false
		}
		b.noteAnswer(ctx, "dup", strconv.Itoa(a.txID))
		b.clearOpenAsk(groupJID)
		if isNew {
			b.sendText(chat, "Засчитал как новый платёж: наличка "+client+".")
			_ = amount
		} else {
			b.sendText(chat, "Понял, это повтор — не считаю: "+client+".")
		}
		return true

	case "payment_who":
		// Без свайпа — только короткое ФИО (с суммой — только той же, что в вопросе).
		if !shortPlainReply(text, 4) {
			return false
		}
		b.clarify.mu.Lock()
		var pa paymentAsk
		var askID string
		for k, v := range b.clarify.paymentAsks {
			if v.rawID == a.txID {
				pa, askID = v, k
			}
		}
		b.clarify.mu.Unlock()
		if askID == "" {
			return false
		}
		return b.applyPaymentWhoAnswer(ctx, chat, askID, pa, text, false)

	case "cmf_watch":
		wantName := a.wantName && time.Since(a.at) < cmfNameWindow
		return b.applyCmfWatchAnswer(ctx, chat, a.watchID, text, modeContext, wantName)

	case "cash_collector":
		// Без свайпа — только явный ответ «у Дени» / «Мансур взял» / «отдал Адаму»
		// или известное имя; новый платёж и болтовня («Ок понял») — не ответ.
		if !shortPlainReply(text, 3) || isConfirmReply(strings.ToLower(text)) || !collectorAnswerLike(text, b) {
			return false
		}
		if b.applyCashCollectorReply(ctx, chat, a.txID, text, false) {
			b.clearOpenAsk(groupJID)
			return true
		}
		return false

	case "receipt":
		// Несколько сумм («Исаев Харон 15000 20000», «Исаев Харон 15000,
		// Магомедов Ахмед 10000», строки сумм) — это платежи, а не ответ «чей
		// чек?»: их запишет обычный разбор, а чек подпишет пара «ФИО сумма» на
		// его сумму. Наличка с суммой («… нал», «… Мансур взял», «… отдал
		// Адаму») — другой платёж, тоже не ответ про чек.
		if countAmountLines(text) > 1 || severalAmounts(text) ||
			parser.ExtractAmount(text) > 0 && textCash(text) {
			return false
		}
		// Несколько открытых вопросов «чей чек?»: ответ — к самому раннему, а если
		// в ответе сумма — к чеку на эту сумму.
		amount := parser.ExtractAmount(text)
		if q := b.pickReceiptAsk(groupJID, amount); q.receiptWaID != "" {
			a = q
		}
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
			// Нужно только ФИО — принимаем голое имя; с суммой — только если она
			// равна сумме этого чека («Хасанов Ислам 15000» на вопрос о 15000).
			if name == "" || (amount > 0 && (a.amount <= 0 || math.Abs(amount-a.amount) >= 1)) {
				return false
			}
		default:
			// Не прочитали ни сумму, ни клиента — примем ФИО и/или сумму.
			if name == "" && amount <= 0 {
				return false
			}
		}
		answer := text
		if a.needName && !a.needAmount && amount > 0 {
			answer = name // сумму уже знаем — берём только ФИО
		}
		handled, done := b.applyReceiptAnswer(ctx, chat, a.receiptWaID, answer)
		if done {
			b.dropReceiptAsk(groupJID, a.receiptWaID)
		}
		return handled
	}
	return false
}

// collectorAnswerLike — похоже на ответ «кто забрал наличку»: есть слово-маркер
// («у», «взял», «забрал», «отдал», «к») или это известное имя.
func collectorAnswerLike(text string, b *Bot) bool {
	for _, w := range strings.Fields(strings.ToLower(text)) {
		switch strings.Trim(w, ".,!?()«»\"") {
		case "у", "взял", "взяла", "забрал", "забрала", "отдал", "отдала", "к", "принял", "приняла", "в":
			return true
		}
	}
	_, known := b.aliases.ResolveName(strings.TrimSpace(text))
	return known
}

// pickReceiptAsk — какой из открытых вопросов «чей чек?» группы имеется в виду.
func (b *Bot) pickReceiptAsk(groupJID string, amount float64) openAsk {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	var fresh []openAsk
	for _, q := range b.clarify.receiptAsks[groupJID] {
		if time.Since(q.at) <= contextAnswerWindow {
			fresh = append(fresh, q)
		}
	}
	b.clarify.receiptAsks[groupJID] = fresh
	if amount > 0 {
		for _, q := range fresh {
			if q.amount > 0 && math.Abs(q.amount-amount) < 1 {
				return q
			}
		}
	}
	if len(fresh) > 0 {
		return fresh[0]
	}
	return openAsk{}
}

// dropReceiptAsk — вопрос по чеку закрыт; следующим открытым становится
// следующий по очереди вопрос «чей чек?» (если есть).
func (b *Bot) dropReceiptAsk(groupJID, receiptWaID string) {
	b.clarify.mu.Lock()
	var rest []openAsk
	for _, q := range b.clarify.receiptAsks[groupJID] {
		if q.receiptWaID != receiptWaID {
			rest = append(rest, q)
		}
	}
	b.clarify.receiptAsks[groupJID] = rest
	cur, ok := b.clarify.lastAsk[groupJID]
	if ok && cur.kind == "receipt" && cur.receiptWaID == receiptWaID {
		if len(rest) > 0 {
			b.clarify.lastAsk[groupJID] = rest[0]
		} else {
			delete(b.clarify.lastAsk, groupJID)
		}
	}
	b.clarify.mu.Unlock()
}

// clearOpenAskTx — снять открытый вопрос о наличке txID (ответили свайпом).
func (b *Bot) clearOpenAskTx(groupJID, kind string, txID int) {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if cur, ok := b.clarify.lastAsk[groupJID]; ok && cur.kind == kind && cur.txID == txID {
		delete(b.clarify.lastAsk, groupJID)
	}
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
	rid, found, amount, stillReview, err := b.db.FillReceiptByMessageID(ctx, receiptWaID, canonical, contactIDPtr, replyAmount)
	if err != nil {
		fmt.Println("Ответ про чек: не записался:", err)
		return false, false
	}
	if !found {
		// Чек за это время стал повтором (пришла подписанная копия, автоповтор
		// дочитал копию) — ответ человека всё равно не теряем.
		return b.answerForRepeatedCheck(ctx, chat, receiptWaID, text, canonical, contactIDPtr)
	}
	b.noteAnswer(ctx, "receipt", receiptWaID)
	// Чек стал полным/чьим только сейчас: тот же чек мог уже прийти и войти в
	// сбор (прислали ещё раз чётче, подписали копию) — второй раз не считаем, а
	// если не уверены — спрашиваем «тот же или другой?».
	if amount > 0 && (canonical != "" || replyAmount > 0) {
		same, err := b.db.DedupeCompletedReceipt(ctx, rid, replyAmount > 0, true)
		if err != nil {
			fmt.Println("Ответ про чек: проверка повтора не удалась:", err)
		} else if same.Found && same.VictimIsThis {
			if canonical != "" {
				_ = b.db.SetReceiptSourceByMessage(ctx, receiptWaID, answerRawFrom(ctx))
			}
			b.reportSameCheck(ctx, chat, receiptWaID, "", same, false)
			return true, true
		} else if same.Found {
			b.reportSameCheck(ctx, chat, receiptWaID, "", same, false)
		}
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
		_ = b.db.SetReceiptSourceByMessage(ctx, receiptWaID, answerRawFrom(ctx))
		b.sendText(chat, "Записал: чек — клиент "+canonical+".")
		b.syncWatchByCheckWa(ctx, chat, receiptWaID, canonical)
	} else {
		b.sendText(chat, "Поправил сумму чека.")
	}
	return true, true
}
