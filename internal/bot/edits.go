package bot

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
)

// Правки: сообщение изменили в WhatsApp («изменить») или написали поправку
// «не 5000 а 6000». Учёт должен показать исправленную сумму/клиента.

// handleMessageEdit — сотрудник изменил своё сообщение. Платежи из старого
// текста снимаем и разбираем новый текст под тем же сообщением; изменённую
// подпись к чеку применяем к этому чеку.
func (b *Bot) handleMessageEdit(ctx context.Context, msg *events.Message, prot *waProto.ProtocolMessage) {
	origID := prot.GetKey().GetID()
	newMsg := prot.GetEditedMessage()
	if origID == "" || newMsg == nil {
		return
	}
	ref, err := b.db.RawMessageByWaID(ctx, origID)
	if err != nil || !ref.Found || ref.Deleted || ref.GroupJID != msg.Info.Chat.String() {
		// Сообщения нет в учёте (болтовня, обращение к боту, ответ на вопрос) —
		// правка ничего не меняет.
		return
	}
	newText := strings.TrimSpace(extractText(newMsg))
	if ref.HasMedia {
		b.applyCaptionEdit(ctx, msg, origID, ref, newText)
		return
	}
	if newText == "" || newText == strings.TrimSpace(ref.Body) {
		return
	}
	if _, addressed := b.stripBotName(newText); addressed {
		return // теперь это обращение к боту, а не платёж — команду заново не выполняем
	}

	undo, err := b.db.PrepareMessageEdit(ctx, ref.ID, newText)
	if err != nil {
		fmt.Println("Правка сообщения: ошибка:", err)
		return
	}

	// Сообщение-имя, которое уже подписало чек: новое имя — тому же чеку.
	if len(undo.LabeledChecks) > 0 && parser.ExtractAmount(newText) == 0 {
		if canonical, ok := b.confirmClientName(ctx, newText); ok {
			var cid *int
			if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
				cid = &id
			}
			for _, w := range undo.LabeledChecks {
				if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, w, canonical, cid, ref.ID); err == nil && found {
					b.reresolveWatchByCheck(ctx, msg.Info.Chat, w, canonical)
				}
			}
			_ = b.db.MarkMessageParsed(ctx, ref.ID)
			fmt.Printf("Правка сообщения %s: чек теперь на клиента %q\n", origID, canonical)
			return
		}
	}

	em := &events.Message{Info: msg.Info, Message: newMsg}
	em.Info.ID = origID
	em.Info.Timestamp = ref.ReceivedAt
	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = ref.SenderName
	}
	b.processText(ctx, em, newText, ref.ID, senderName, true)

	now, _ := b.db.TxRefsByRawID(ctx, ref.ID)
	if note := editNote(undo.OldTx, now); note != "" && !b.groupSilent(msg.Info.Chat) {
		b.sendText(msg.Info.Chat, note)
	}
	fmt.Printf("Правка сообщения %s: было %v, стало %v\n", origID, undo.OldTx, now)
}

// editNote — короткий ответ о том, что поменялось в учёте после правки.
func editNote(was, now []db.TxRef) string {
	list := func(ts []db.TxRef) string {
		var parts []string
		for _, t := range ts {
			parts = append(parts, fmt.Sprintf("%s %s ₽", t.Name, formatMoney(t.Amount)))
		}
		return strings.Join(parts, ", ")
	}
	a, c := list(was), list(now)
	switch {
	case a == c:
		return ""
	case a == "":
		return "✏️ Сообщение изменили — записал: " + c + "."
	case c == "":
		return "✏️ Сообщение изменили — убрал из учёта: " + a + "."
	}
	return "✏️ Сообщение изменили — поправил учёт: было " + a + ", стало " + c + "."
}

// applyCaptionEdit — изменили подпись к чеку: ФИО из новой подписи — клиент
// этого чека.
func (b *Bot) applyCaptionEdit(ctx context.Context, msg *events.Message, origID string, ref db.RawMessageRef, caption string) {
	if caption == "" {
		return
	}
	name := ""
	if n, ok := looksLikeName(caption); ok {
		name = n
	} else if n, ok := firstNameLine(caption); ok {
		name = n
	}
	if name == "" {
		return
	}
	canonical, ok := b.confirmClientName(ctx, name)
	if !ok {
		return
	}
	var cid *int
	if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
		cid = &id
	}
	found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, origID, canonical, cid, ref.ID)
	if err != nil || !found {
		return
	}
	b.reresolveWatchByCheck(ctx, msg.Info.Chat, origID, canonical)
	fmt.Printf("Правка подписи к чеку %s: клиент %q\n", origID, canonical)
	if !b.groupSilent(msg.Info.Chat) {
		b.sendText(msg.Info.Chat, "✏️ Подпись к чеку изменили — чек теперь на клиента "+canonical+".")
	}
}

// confirmClientName — это действительно ФИО клиента (алиас или ИИ подтвердил)?
func (b *Bot) confirmClientName(ctx context.Context, text string) (string, bool) {
	name, ok := looksLikeName(text)
	if !ok {
		return "", false
	}
	if _, known := b.aliases.ResolveName(name); !known {
		confirmed := ""
		if b.assistant != nil {
			confirmed = b.cmfExtractClientName(ctx, text)
		}
		if confirmed == "" {
			return "", false
		}
		name = confirmed
	}
	canonical, _ := b.aliases.ResolveName(name)
	return canonical, true
}

// applyAmountCorrection — «не 5000 а 6000» (или «исправь на 6000» свайпом):
// меняет сумму уже записанного платежа, а не добавляет новый.
func (b *Bot) applyAmountCorrection(ctx context.Context, msg *events.Message, text string, rawID int) bool {
	old, amount, ok := parser.ParseAmountCorrection(text)
	if !ok {
		return false
	}
	// Поправка — короткая фраза. Длинное сообщение со списком платежей — не она.
	if strings.Count(strings.TrimSpace(text), "\n") > 1 || len([]rune(text)) > 80 || strings.Contains(text, "?") {
		return false
	}
	chat := msg.Info.Chat
	say := func(s string) {
		if !b.groupSilent(chat) {
			b.sendText(chat, s)
		}
	}
	done := func() bool {
		_ = b.db.MarkMessageParsed(ctx, rawID)
		return true
	}
	if q := extractQuotedStanzaID(msg); q != "" {
		if tx, err := b.db.TextTxOfMessage(ctx, q); err == nil && tx.Found && (old == 0 || math.Abs(tx.Amount-old) < 1) {
			if math.Abs(tx.Amount-amount) < 1 {
				return done() // «правильно 5000» — подтверждение, менять нечего
			}
			if err := b.db.SetTxAmount(ctx, tx.ID, amount); err == nil {
				say(fmt.Sprintf("✏️ Исправил: %s — было %s ₽, стало %s ₽.", tx.Name, formatMoney(tx.Amount), formatMoney(amount)))
				return done()
			}
		}
		if found, name, was, err := b.db.CorrectReceiptAmountByMessage(ctx, q, old, amount); err == nil && found {
			who := ""
			if name != "" {
				who = " (" + name + ")"
			}
			say(fmt.Sprintf("✏️ Исправил сумму чека%s: было %s ₽, стало %s ₽.", who, formatMoney(was), formatMoney(amount)))
			return done()
		}
		if old == 0 {
			return false
		}
	} else if old == 0 {
		return false // «исправь на 6000» без свайпа — непонятно, что исправлять
	}
	// Без свайпа: свежий платёж этого же сотрудника на старую сумму.
	tx, err := b.db.LatestTxFromSenderWithAmount(ctx, chat.String(), msg.Info.Sender.String(), old, time.Now().Add(-48*time.Hour))
	if err == nil && tx.Found {
		if err := b.db.SetTxAmount(ctx, tx.ID, amount); err == nil {
			say(fmt.Sprintf("✏️ Исправил: %s — было %s ₽, стало %s ₽.", tx.Name, formatMoney(tx.Amount), formatMoney(amount)))
			return done()
		}
	}
	if w, err := b.db.LatestReceiptFromSenderWithAmount(ctx, chat.String(), msg.Info.Sender.String(), old, time.Now().Add(-48*time.Hour)); err == nil && w != "" {
		if found, name, was, err := b.db.CorrectReceiptAmountByMessage(ctx, w, old, amount); err == nil && found {
			who := ""
			if name != "" {
				who = " (" + name + ")"
			}
			say(fmt.Sprintf("✏️ Исправил сумму чека%s: было %s ₽, стало %s ₽.", who, formatMoney(was), formatMoney(amount)))
			return done()
		}
	}
	say(fmt.Sprintf("Не нашёл платёж на %s ₽, который нужно исправить. Ответьте свайпом на сообщение с ним.", formatMoney(old)))
	return done()
}
