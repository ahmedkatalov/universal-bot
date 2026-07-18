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
	mu     sync.Mutex
	askMap map[string]string // botQuestionMsgID -> receiptWaMessageID
}

func newClarifyState() *clarifyState {
	return &clarifyState{askMap: make(map[string]string)}
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

func (b *Bot) clarifyTick(ctx context.Context) {
	if b.assistant == nil {
		return // без ИИ имена не разбираем — вопросы бессмысленны
	}
	// Можно выключить проактивные вопросы: ASK_UNNAMED_RECEIPTS=0.
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ASK_UNNAMED_RECEIPTS"))); v == "0" || v == "false" || v == "no" {
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
	b.clarify.askMap[botMsgID] = receiptWaID
	if len(b.clarify.askMap) > 500 {
		b.clarify.askMap = map[string]string{botMsgID: receiptWaID}
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
