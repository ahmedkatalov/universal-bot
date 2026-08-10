// Проактивный вопрос "чей это чек": если у чека нет подтверждённого клиента
// (никто не написал ФИО рядом) и это не массовый импорт, бот сам спрашивает
// в группе, показывая данные чека, и ждёт ответа — вместо того чтобы молча
// оставить чек непонятым.
package bot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

const (
	clarifyGrace    = 90 * time.Second // ждём ФИО рядом с чеком столько, прежде чем спросить
	clarifyMaxAsk   = 6                // если непонятых чеков больше — это массовый импорт, не спамим вопросами
	clarifyPerCycle = 2                // сколько вопросов задаём за один проход (чтобы не флудить)
)

// clarifyMap хранит соответствие "id вопроса бота" -> "id сообщения чека",
// чтобы ответ владельца на вопрос привязать к нужному чеку.
type clarifyState struct {
	mu           sync.Mutex
	askMap       map[string]string // botQuestionMsgID -> receiptWaMessageID (чей чек)
	cashAskMap   map[string]int    // botQuestionMsgID -> transactionID (у кого наличка)
	dupAskMap    map[string]int    // botQuestionMsgID -> transactionID (повтор налички?)
	askOrder     []string          // порядок вставки ключей askMap (для вытеснения старых)
	cashAskOrder []string          // порядок вставки ключей cashAskMap
	dupAskOrder  []string          // порядок вставки ключей dupAskMap
}

func newClarifyState() *clarifyState {
	return &clarifyState{
		askMap:     make(map[string]string),
		cashAskMap: make(map[string]int),
		dupAskMap:  make(map[string]int),
	}
}

// clarifyMapCap — максимум запоминаемых связей «вопрос бота -> чек/наличка».
const clarifyMapCap = 500

// registerCashAsk запоминает связь «id вопроса про наличку -> id транзакции».
// При переполнении удаляет САМЫЕ СТАРЫЕ ключи (по порядку вставки), а не весь
// map целиком — иначе ответ на любой не-самый-новый вопрос теряет привязку.
func (b *Bot) registerCashAsk(botMsgID string, txID int) {
	if botMsgID == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if _, exists := b.clarify.cashAskMap[botMsgID]; !exists {
		b.clarify.cashAskOrder = append(b.clarify.cashAskOrder, botMsgID)
	}
	b.clarify.cashAskMap[botMsgID] = txID
	for len(b.clarify.cashAskMap) > clarifyMapCap && len(b.clarify.cashAskOrder) > 0 {
		oldest := b.clarify.cashAskOrder[0]
		b.clarify.cashAskOrder = b.clarify.cashAskOrder[1:]
		delete(b.clarify.cashAskMap, oldest)
	}
}

// registerDupAsk запоминает связь «id вопроса про повтор налички -> id транзакции».
func (b *Bot) registerDupAsk(botMsgID string, txID int) {
	if botMsgID == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if _, exists := b.clarify.dupAskMap[botMsgID]; !exists {
		b.clarify.dupAskOrder = append(b.clarify.dupAskOrder, botMsgID)
	}
	b.clarify.dupAskMap[botMsgID] = txID
	for len(b.clarify.dupAskMap) > clarifyMapCap && len(b.clarify.dupAskOrder) > 0 {
		oldest := b.clarify.dupAskOrder[0]
		b.clarify.dupAskOrder = b.clarify.dupAskOrder[1:]
		delete(b.clarify.dupAskMap, oldest)
	}
}

// cashDupCheckEnabled — спрашивать ли про повторную наличку (та же сумма тому же
// клиенту за 30 дней — возможно, случайный дубль). По умолчанию ВКЛ. Отключить
// целиком: CASH_DUP_CHECK=0.
func cashDupCheckEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CASH_DUP_CHECK")))
	return v == "" || v == "1" || v == "true" || v == "yes" || v == "on"
}

// cashDupCheckOn — фактически включена ли проверка повторной налички. Требует и
// общий выключатель уточнений (askReceiptsEnabled), и свой CASH_DUP_CHECK: иначе
// придержанная наличка осталась бы без вопроса и навсегда вне сбора.
func (b *Bot) cashDupCheckOn() bool {
	return askReceiptsEnabled() && cashDupCheckEnabled()
}

// clarifyLoop раз в 45 секунд проверяет группы на чеки без клиента и задаёт
// по ним вопросы (аккуратно, не спамя).
func (b *Bot) clarifyLoop() {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		b.clarifyTick(ctx)
		cancel()
	}
}

// askReceiptsEnabled — включены ли уточняющие вопросы по чекам (чей чек, не
// разобрал сумму, у кого наличка, неполный чек). По умолчанию ВКЛ; выключить
// целиком: ASK_UNNAMED_RECEIPTS=0.
func askReceiptsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ASK_UNNAMED_RECEIPTS")))
	return !(v == "0" || v == "false" || v == "no")
}

func (b *Bot) clarifyTick(ctx context.Context) {
	if b.assistant == nil {
		return // без ИИ имена не разбираем — вопросы бессмысленны
	}
	if !askReceiptsEnabled() {
		return
	}
	before := time.Now().Add(-clarifyGrace)
	asked := 0
	for jid := range b.joinedGroups(ctx) {
		if asked >= clarifyPerCycle {
			break
		}
		if !b.isAllowedGroup(jid) {
			continue
		}
		// 1. Чеки без имени клиента — «чей это чек?».
		if n, err := b.db.CountUnconfirmed(ctx, jid.String(), before); err == nil && n > 0 && n <= clarifyMaxAsk {
			items, err := b.db.UnconfirmedReceipts(ctx, jid.String(), before, clarifyPerCycle-asked)
			if err == nil {
				for _, it := range items {
					// Прежде чем спрашивать — анализируем недавние сообщения этого
					// же отправителя: вдруг ФИО клиента написали рядом с чеком
					// (отдельной строкой/сообщением). Тогда привязываем молча.
					if b.tryResolveClientFromContext(ctx, jid, it) {
						continue
					}
					owner := it.CardOwner
					if owner == "" {
						owner = "не распознан"
					}
					text := fmt.Sprintf("🤔 Чей это чек? Получатель на чеке: %s, сумма %.0f ₽, %s. "+
						"Ответьте на это сообщение именем клиента (кому засчитать).", owner, it.Amount, it.TxDate.Format("02.01 15:04"))
					b.askClarify(ctx, jid, text, it)
					asked++
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 3. Наличка без ответственного — «у кого наличка / кто забрал?».
		if n, err := b.db.CountCashNeedingCollector(ctx, jid.String(), before); err == nil && n > 0 && n <= clarifyMaxAsk {
			items, err := b.db.CashNeedingCollector(ctx, jid.String(), before, clarifyPerCycle-asked)
			if err == nil {
				for _, it := range items {
					text := fmt.Sprintf("🤔 Наличка: %s — %.0f ₽. У кого она, кто забрал деньги? "+
						"Ответьте на это сообщение именем (например «у Дени» / «Мансур взял»).", it.Client, it.Amount)
					botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
					if botMsgID != "" {
						// Помечаем «спросили» ТОЛЬКО если вопрос реально ушёл. При
						// сбое отправки (botMsgID == "") оставляем платёж в очереди —
						// следующий цикл переспросит, иначе наличка навсегда без
						// ответственного.
						_ = b.db.MarkTxCollectorAsked(ctx, it.TxID)
						b.registerCashAsk(botMsgID, it.TxID)
						asked++
					}
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 4. Повтор налички — «У клиента уже была та же сумма, это новый платёж?».
		// Без верхней границы n<=clarifyMaxAsk (как у других веток): придержанная
		// наличка исключена из сбора, поэтому её ОБЯЗАТЕЛЬНО нужно спросить, иначе
		// при накоплении >6 повторов ветка молча выключилась бы и деньги пропали
		// из сбора навсегда. Спам ограничен общим лимитом clarifyPerCycle за цикл.
		if b.cashDupCheckOn() {
			if n, err := b.db.CountCashDupNeedingConfirm(ctx, jid.String(), before); err == nil && n > 0 {
				items, err := b.db.CashDupNeedingConfirm(ctx, jid.String(), before, clarifyPerCycle-asked)
				if err == nil {
					for _, it := range items {
						when := "недавно"
						if !it.PrevWhen.IsZero() {
							when = "была " + it.PrevWhen.Format("02.01")
						}
						text := fmt.Sprintf("🤔 У клиента %s уже %s наличка на %.0f ₽. Это НОВЫЙ платёж или тот же самый (повтор)? "+
							"Ответьте на это сообщение: «новый» — засчитаю отдельно, «тот же» — не считаю повтором.",
							it.Client, when, it.Amount)
						botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
						if botMsgID != "" {
							_ = b.db.MarkCashDupAsked(ctx, it.TxID, botMsgID)
							b.registerDupAsk(botMsgID, it.TxID)
							asked++
						}
					}
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 2. Чеки, у которых не прочиталась сумма — «не смог разобрать, что на чеке?».
		if n, err := b.db.CountUnrecognized(ctx, jid.String(), before); err == nil && n > 0 && n <= clarifyMaxAsk {
			items, err := b.db.UnrecognizedReceipts(ctx, jid.String(), before, clarifyPerCycle-asked)
			if err == nil {
				for _, it := range items {
					text := "🤔 Не смог разобрать этот чек (не прочитал сумму). Ответьте на это сообщение " +
						"суммой и ФИО клиента — например: «Ахмед Каталов 15000»."
					b.askClarify(ctx, jid, text, it)
					asked++
				}
			}
		}
	}
}

// tryResolveClientFromContext — прежде чем спросить «чей чек», анализирует
// недавние сообщения ТОГО ЖЕ отправителя: если рядом с чеком написали ФИО
// клиента (в т.ч. отдельной строкой «Магамадов Алха\n22.000₽»), привязывает
// чек к нему молча, без вопроса. Возвращает true, если удалось.
func (b *Bot) tryResolveClientFromContext(ctx context.Context, jid types.JID, it db.ClarifyReceipt) bool {
	if it.WaMessageID == "" || it.SenderJID == "" {
		return false
	}
	texts, err := b.db.RecentSenderTexts(ctx, jid.String(), it.SenderJID, time.Now().Add(-12*time.Minute), 15)
	if err != nil {
		return false
	}
	for _, t := range texts {
		name, ok := looksLikeName(t)
		if !ok {
			name, ok = firstNameLine(t)
		}
		if !ok {
			continue
		}
		canonical, _ := b.aliases.ResolveName(name)
		// Владелец карты (получатель на чеке) — это НЕ клиент, его не берём.
		if it.CardOwner != "" && strings.EqualFold(strings.TrimSpace(canonical), strings.TrimSpace(it.CardOwner)) {
			continue
		}
		var contactIDPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			contactIDPtr = &cid
		}
		if found, _, err := b.db.FillReceiptByMessage(ctx, it.WaMessageID, canonical, contactIDPtr, 0); err == nil && found {
			fmt.Printf("Чек %s привязан к клиенту %q из соседнего сообщения (без вопроса)\n", it.WaMessageID, canonical)
			return true
		}
	}
	return false
}

// applyCashCollectorReply записывает ответственного (кто забрал наличку) из
// ответа владельца на вопрос «у кого наличка?».
func (b *Bot) applyCashCollectorReply(ctx context.Context, chat types.JID, txID int, text string) bool {
	collector := extractCollectorName(text)
	if collector == "" {
		return false
	}
	if canon, ok := b.aliases.ResolveName(collector); ok {
		collector = canon
	}
	found, amount, client, err := b.db.SetTxCollector(ctx, txID, collector)
	if err != nil || !found {
		return false
	}
	b.sendText(chat, fmt.Sprintf("Записал: наличка %s %.0f ₽ — забрал %s.", client, amount, collector))
	return true
}

// cashCollectorMarkers — слова-обёртки вокруг имени в ответе «у Дени», «Мансур взял».
var cashCollectorMarkers = map[string]bool{
	"у": true, "взял": true, "взяла": true, "забрал": true, "забрала": true,
	"отдал": true, "отдала": true, "к": true, "собрал": true, "собрала": true,
	"наличка": true, "нал": true, "наличными": true, "это": true, "руки": true, "руках": true,
}

// extractCollectorName вытаскивает имя ответственного из свободного ответа
// («у Дени» -> «Дени», «Мансур взял» -> «Мансур», «отдал Адаму» -> «Адаму»).
func extractCollectorName(text string) string {
	var words []string
	for _, w := range strings.Fields(text) {
		lw := strings.ToLower(strings.Trim(w, ".,!?()«»\""))
		if cashCollectorMarkers[lw] || lw == "" {
			continue
		}
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		words = append(words, w)
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

// parseCashDupAnswer разбирает ответ на вопрос про повтор налички.
// Возвращает (resolved, isNew): resolved=false — ответ непонятен (переспросим).
func parseCashDupAnswer(text string) (bool, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	// «тот же / повтор» проверяем первым: «не новый» содержит «нов».
	sameMarkers := []string{"тот же", "та же", "то же", "тоже", "повтор", "дубл",
		"один и тот", "не новый", "не считай", "старый", "ошиб"}
	for _, m := range sameMarkers {
		if strings.Contains(t, m) {
			return true, false
		}
	}
	if t == "нет" {
		return true, false
	}
	newMarkers := []string{"нов", "отдельн", "друг", "ещё", "еще"}
	for _, m := range newMarkers {
		if strings.Contains(t, m) {
			return true, true
		}
	}
	if t == "да" {
		return true, true // «да» на «это новый?» — новый
	}
	return false, false
}

// askClarify отправляет вопрос цитатой на сам чек и запоминает связь
// «id вопроса -> id сообщения чека», чтобы привязать ответ владельца.
func (b *Bot) askClarify(ctx context.Context, jid types.JID, text string, it db.ClarifyReceipt) {
	botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
	_ = b.db.MarkReceiptAsked(ctx, it.ID)
	b.registerClarifyAsk(botMsgID, it.WaMessageID)
}

// registerClarifyAsk запоминает связь «id вопроса бота -> id сообщения чека»,
// чтобы ответ владельца (свайпом на вопрос) привязался к нужному чеку.
func (b *Bot) registerClarifyAsk(botMsgID, receiptWaID string) {
	if botMsgID == "" || receiptWaID == "" {
		return
	}
	b.clarify.mu.Lock()
	if _, exists := b.clarify.askMap[botMsgID]; !exists {
		b.clarify.askOrder = append(b.clarify.askOrder, botMsgID)
	}
	b.clarify.askMap[botMsgID] = receiptWaID
	// Вытесняем самые старые связи, а не весь map целиком — иначе ответ на любой
	// не-самый-свежий вопрос «чей чек» теряет привязку и уходит в неверную
	// FIFO-атрибуцию.
	for len(b.clarify.askMap) > clarifyMapCap && len(b.clarify.askOrder) > 0 {
		oldest := b.clarify.askOrder[0]
		b.clarify.askOrder = b.clarify.askOrder[1:]
		delete(b.clarify.askMap, oldest)
	}
	b.clarify.mu.Unlock()
}

const (
	suspiciousMultiple = 12     // во сколько раз выше медианы = подозрительно
	suspiciousFloor    = 100000 // и не ниже этого порога (мелкие суммы не трогаем)
	suspiciousMinCount = 30     // минимум чеков в группе, чтобы медиана была надёжной
)

func suspiciousAmountCheckEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUSPICIOUS_AMOUNT_CHECK")))
	return v == "" || v == "1" || v == "true" || v == "yes" || v == "on"
}

// checkSuspiciousAmount спрашивает в группе, если сумма чека аномально большая
// для этой группы (вероятно, OCR/зрение приписали лишний ноль). Чек при этом
// уже в учёте — если владелец поправит, сумма исправится; если подтвердит —
// останется. Ловит именно грубые ошибки, чтобы не спамить.
func (b *Bot) checkSuspiciousAmount(ctx context.Context, chat types.JID, waMsgID, senderJID string, amount float64) {
	if !suspiciousAmountCheckEnabled() || waMsgID == "" || amount < suspiciousFloor {
		return
	}
	median, n, err := b.db.GroupAmountMedian(ctx, chat.String(), time.Now().AddDate(0, -3, 0))
	if err != nil || n < suspiciousMinCount || median <= 0 || amount <= median*suspiciousMultiple {
		return
	}
	text := fmt.Sprintf("🤔 Проверьте сумму: %.0f ₽ по этому чеку необычно большая для этой группы "+
		"(обычно около %.0f ₽). Если это ошибка распознавания — ответьте на это сообщение верной суммой; "+
		"если всё верно — напишите «верно».", amount, median)
	b.registerClarifyAsk(b.sendReply(chat, text, waMsgID, senderJID), waMsgID)
}

// handleClarifyReply — если владелец ответил (свайп) на вопрос бота "чей чек",
// привязывает названного клиента к тому чеку. Возвращает true, если обработано.
func (b *Bot) handleClarifyReply(ctx context.Context, msg *events.Message, text string) bool {
	quotedID := extractQuotedStanzaID(msg)
	if quotedID == "" {
		return false
	}

	// Ответ на вопрос «наличка-повтор: новый или тот же?».
	b.clarify.mu.Lock()
	dupTxID, isDupAsk := b.clarify.dupAskMap[quotedID]
	b.clarify.mu.Unlock()
	if !isDupAsk {
		// Связь могла потеряться из памяти (рестарт бота) — ищем в БД по id
		// сообщения-вопроса, чтобы поздний ответ всё равно сработал.
		if id, ok, _ := b.db.DupTxByAskMsg(ctx, quotedID); ok {
			dupTxID, isDupAsk = id, true
		}
	}
	if isDupAsk {
		resolved, isNew := parseCashDupAnswer(text)
		if !resolved {
			b.sendText(msg.Info.Chat, "Ответьте, пожалуйста, «новый» (засчитать отдельно) или «тот же» (повтор, не считать).")
			return true // оставляем связь — можно ответить ещё раз на тот же вопрос
		}
		b.clarify.mu.Lock()
		delete(b.clarify.dupAskMap, quotedID)
		b.clarify.mu.Unlock()
		found, amount, client, err := b.db.ResolveCashDup(ctx, dupTxID, isNew)
		if err != nil || !found {
			return false
		}
		if isNew {
			b.sendText(msg.Info.Chat, fmt.Sprintf("Засчитал как новый платёж: наличка %s %.0f ₽.", client, amount))
		} else {
			b.sendText(msg.Info.Chat, fmt.Sprintf("Понял, это повтор — не считаю: %s %.0f ₽.", client, amount))
		}
		return true
	}

	// Ответ на вопрос «у кого наличка / кто забрал?» — записываем ответственного.
	b.clarify.mu.Lock()
	txID, isCashAsk := b.clarify.cashAskMap[quotedID]
	if isCashAsk {
		delete(b.clarify.cashAskMap, quotedID)
	}
	b.clarify.mu.Unlock()
	if isCashAsk {
		return b.applyCashCollectorReply(ctx, msg.Info.Chat, txID, text)
	}

	b.clarify.mu.Lock()
	receiptWaID, ok := b.clarify.askMap[quotedID]
	if ok {
		delete(b.clarify.askMap, quotedID)
	}
	b.clarify.mu.Unlock()
	if !ok {
		return false
	}

	// Ответ бывает трёх видов: ФИО («Ахмед Каталов»), ФИО+сумма («Ахмед 15000»),
	// или только сумма/подтверждение («50000», «да») — для правки подозрительной
	// суммы. Имя берём без цифр; сумму — отдельно.
	replyAmount := parser.ExtractAmount(text)
	name, ok := looksLikeName(text)
	if !ok && replyAmount == 0 {
		// Нет ни ФИО-из-2-слов, ни суммы: возможно, одно имя («Ахмед») —
		// но не служебное слово-подтверждение («да», «верно»).
		candidate := strings.TrimSpace(text)
		if candidate != "" && !nameStopwords[strings.ToLower(candidate)] {
			name = candidate
		}
	}
	if name == "" && replyAmount == 0 {
		return false // не смогли извлечь ни имя, ни сумму
	}
	canonical := ""
	var contactIDPtr *int
	if name != "" {
		canonical, _ = b.aliases.ResolveName(name)
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			contactIDPtr = &cid
		}
	}
	found, amount, err := b.db.FillReceiptByMessage(ctx, receiptWaID, canonical, contactIDPtr, replyAmount)
	if err != nil || !found {
		return false
	}
	switch {
	case canonical != "":
		b.sendText(msg.Info.Chat, fmt.Sprintf("Записал: чек на %.0f ₽ — клиент %s.", amount, canonical))
	default:
		b.sendText(msg.Info.Chat, fmt.Sprintf("Поправил сумму чека: %.0f ₽.", amount))
	}
	return true
}

// sendTextReturnID отправляет текст и возвращает id отправленного сообщения
// (нужно, чтобы привязать ответ владельца к конкретному вопросу).
func (b *Bot) sendTextReturnID(chat types.JID, text string) string {
	resp, err := b.client.SendMessage(context.Background(), chat, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		fmt.Println("Ошибка отправки вопроса:", err)
		return ""
	}
	b.rememberSent(chat, resp.ID)
	return resp.ID
}
