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
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Правки: сообщение изменили в WhatsApp («изменить») или написали поправку
// «не 5000 а 6000». Учёт должен показать исправленную сумму/клиента.

// editQuiet — как применять правку. budget != nil — импорт истории: молча, ИИ
// читает не больше budget сообщений.
type editQuiet struct{ budget *int }

func (q editQuiet) quiet() bool { return q.budget != nil }

// handleMessageEdit — сотрудник изменил своё сообщение.
func (b *Bot) handleMessageEdit(ctx context.Context, msg *events.Message, prot *waProto.ProtocolMessage) {
	b.applyEdit(ctx, msg, prot, editQuiet{})
}

// applyEdit — правка сообщения: платежи из старого текста снимаем и разбираем
// новый текст под тем же сообщением; изменённое имя (подпись к чеку, имя
// перед/после чека, ответ на «чей чек?», свайп с ФИО на платёж) переносим на
// тот же чек/платёж; изменённую поправку «не X а Y» применяем заново.
// Возвращает, применилась ли правка к сообщению из учёта.
func (b *Bot) applyEdit(ctx context.Context, msg *events.Message, prot *waProto.ProtocolMessage, q editQuiet) bool {
	origID := prot.GetKey().GetID()
	newMsg := prot.GetEditedMessage()
	if origID == "" || newMsg == nil {
		return false
	}
	ref, err := b.db.RawMessageByWaID(ctx, origID)
	if err != nil || !ref.Found || ref.Deleted || ref.GroupJID != msg.Info.Chat.String() {
		return false // сообщения нет в учёте (болтовня) — правка ничего не меняет
	}
	if ref.HandledAs == "assistant" {
		return false // обращение к ассистенту: команду заново не выполняем
	}
	chat := msg.Info.Chat
	say := func(s string) {
		if s != "" && !q.quiet() && !b.groupSilent(chat) {
			b.sendText(chat, s)
		}
	}
	newText := strings.TrimSpace(extractText(newMsg))
	if ref.HasMedia {
		b.applyCaptionEdit(ctx, chat, origID, ref, newText, say)
		return true
	}
	if newText == "" || newText == strings.TrimSpace(ref.Body) {
		return false
	}
	if _, addressed := b.stripBotName(newText); addressed && ref.HandledAs == "" {
		return false // теперь это обращение к боту, а не платёж
	}

	undo, err := b.db.PrepareMessageEdit(ctx, ref.ID, newText)
	if err != nil {
		fmt.Println("Правка сообщения: ошибка:", err)
		return false
	}

	// Изменили поправку «не 5000 а 6000» — те же платежи получают новую сумму.
	if len(undo.Corrections) > 0 {
		if c, ok := parser.ParseCorrection(newText); ok {
			var done []string
			for _, co := range undo.Corrections {
				if b.setCorrectedAmount(ctx, ref.ID, db.CorrTarget{Kind: co.Kind, ID: co.TargetID, Amount: co.Old}, c.New) {
					done = append(done, formatMoney(c.New))
				}
			}
			_ = b.db.MarkMessageParsed(ctx, ref.ID)
			if len(done) > 0 {
				say(fmt.Sprintf("✏️ Поправку изменили — теперь %s ₽.", formatMoney(c.New)))
			}
			return true
		}
		say("✏️ Поправку изменили — вернул прежнюю сумму.")
	}

	// Сообщение-имя, которое уже подписало чек или уточнило клиента платежа
	// (или ответ на «чей чек?»): новое имя — тем же чекам/платежам. Повторно как
	// имя «для следующего чека» его не ставим — оно уже было использовано.
	if len(undo.LabeledChecks)+len(undo.LabeledTxs) > 0 || ref.HandledAs == "answer" {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(newText, "это "), "Это "))
		if canonical, ok := b.confirmClientName(ctx, name); ok {
			var cid *int
			if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
				cid = &id
			}
			for _, w := range undo.LabeledChecks {
				if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, w, canonical, cid, ref.ID); err == nil && found {
					b.reresolveWatchByCheck(ctx, chat, w, canonical)
				}
			}
			if cid != nil {
				for _, tx := range undo.LabeledTxs {
					_ = b.db.SetTxClientFrom(ctx, tx, *cid, canonical, ref.ID)
				}
			}
			fmt.Printf("Правка сообщения %s: клиент теперь %q\n", origID, canonical)
			say("✏️ Имя исправили — теперь клиент " + canonical + ".")
		}
		_ = b.db.MarkMessageParsed(ctx, ref.ID)
		return true
	}

	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = ref.SenderName
	}
	if q.quiet() {
		if _, viaAI := b.importTextSilent(ctx, chat, senderName, ref.ID, ref.ReceivedAt, newText, q.budget); !viaAI {
			_ = b.db.MarkMessageParsed(ctx, ref.ID)
		}
		return true
	}
	em := &events.Message{Info: msg.Info, Message: newMsg}
	em.Info.ID = origID
	em.Info.Timestamp = ref.ReceivedAt
	b.processText(ctx, em, newText, ref.ID, senderName, true)

	now, _ := b.db.TxRefsByRawID(ctx, ref.ID)
	say(editNote(undo.OldTx, now))
	fmt.Printf("Правка сообщения %s: было %v, стало %v\n", origID, undo.OldTx, now)
	return true
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
func (b *Bot) applyCaptionEdit(ctx context.Context, chat types.JID, origID string, ref db.RawMessageRef, caption string, say func(string)) {
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
	b.reresolveWatchByCheck(ctx, chat, origID, canonical)
	fmt.Printf("Правка подписи к чеку %s: клиент %q\n", origID, canonical)
	say("✏️ Подпись к чеку изменили — чек теперь на клиента " + canonical + ".")
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

// corrNameMatches — имя из поправки («Магомедов», «Магомедову Ахмеду») — это
// клиент платежа.
func corrNameMatches(query, name string) bool {
	qw, nw := normWords(query), normWords(name)
	if len(qw) == 0 {
		return true
	}
	for _, q := range qw {
		hit := false
		for _, n := range nw {
			if nameWordSame(q, n) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// groupCorrTargets — текст и чек одного платежа (тот же клиент, перевод,
// присланы в пределах двух суток) — одна цель поправки.
func groupCorrTargets(ts []db.CorrTarget) [][]db.CorrTarget {
	var groups [][]db.CorrTarget
	for _, t := range ts {
		placed := false
		for gi, g := range groups {
			for _, o := range g {
				d := t.Received.Sub(o.Received)
				if d < 0 {
					d = -d
				}
				if t.Kind != o.Kind && t.ContactID != 0 && t.ContactID == o.ContactID && !t.Cash && !o.Cash && d <= 48*time.Hour {
					groups[gi] = append(groups[gi], t)
					placed = true
					break
				}
			}
			if placed {
				break
			}
		}
		if !placed {
			groups = append(groups, []db.CorrTarget{t})
		}
	}
	return groups
}

// setCorrectedAmount — поставить платежу/чеку новую сумму и запомнить поправку
// (удалят поправку — вернём). Наличка заново проверяется на «повтор».
func (b *Bot) setCorrectedAmount(ctx context.Context, srcRawID int, t db.CorrTarget, amount float64) bool {
	var err error
	switch t.Kind {
	case "tx":
		if err = b.db.SetTxAmount(ctx, t.ID, amount); err == nil && b.cashDupCheckOn() {
			_ = b.db.RecheckCashRepeat(ctx, t.ID)
		}
	case "receipt":
		err = b.db.SetReceiptAmount(ctx, t.ID, amount)
	default:
		return false
	}
	if err != nil {
		return false
	}
	_ = b.db.RecordCorrection(ctx, srcRawID, t.Kind, t.ID, t.Amount, amount)
	return true
}

// applyAmountCorrection — «не 5000 а 6000», «Магомедов не 5000 а 6000» или
// свайпом «правильно 6000»: меняет сумму уже записанного платежа (и его копии
// — текста или чека того же платежа), а не добавляет новый. Не поправка или
// разговор («сказал не 5000 а 6000 принесёт») — false, сообщение разбирается
// как обычно.
func (b *Bot) applyAmountCorrection(ctx context.Context, msg *events.Message, text string, rawID int) bool {
	c, ok := parser.ParseCorrection(text)
	if !ok {
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
	var targets []db.CorrTarget
	if q := extractQuotedStanzaID(msg); q != "" {
		ts, _ := b.db.CorrTargetsOfMessage(ctx, q)
		for _, t := range ts {
			if c.Old == 0 || math.Abs(t.Amount-c.Old) < 1 {
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		if c.Old == 0 {
			return false // «правильно 6000» без свайпа на платёж — непонятно, что исправлять
		}
		ts, _ := b.db.CorrTargetsFromSender(ctx, chat.String(), msg.Info.Sender.String(), c.Old, time.Now().Add(-48*time.Hour))
		for _, t := range ts {
			if c.Name == "" || corrNameMatches(c.Name, t.Name) {
				targets = append(targets, t)
			}
		}
	}
	groups := groupCorrTargets(targets)
	switch {
	case len(groups) == 0:
		say(fmt.Sprintf("Не нашёл платёж на %s ₽, который нужно исправить. Ответьте свайпом на сообщение с ним.", formatMoney(c.Old)))
		return done()
	case len(groups) > 1:
		var names []string
		for _, g := range groups {
			names = append(names, g[0].Name)
		}
		say(fmt.Sprintf("Платежей на %s ₽ несколько (%s) — какой исправить? Ответьте свайпом на нужный: «не %s а %s».",
			formatMoney(groups[0][0].Amount), strings.Join(names, ", "), formatMoney(groups[0][0].Amount), formatMoney(c.New)))
		return done()
	}
	g := groups[0]
	if math.Abs(g[0].Amount-c.New) < 0.01 {
		return done() // «правильно 5000» — подтверждение, менять нечего
	}
	// Копия того же платежа, присланная отдельно (текст ↔ чек), — тоже.
	seen := map[string]bool{}
	for _, t := range g {
		seen[t.Kind+fmt.Sprint(t.ID)] = true
	}
	for _, t := range append([]db.CorrTarget(nil), g...) {
		var twins []int
		kind := ""
		if t.Kind == "tx" && !t.Cash {
			twins, _ = b.db.TxReceiptTwins(ctx, t.ID)
			kind = "receipt"
		} else if t.Kind == "receipt" {
			twins, _ = b.db.ReceiptTextTwins(ctx, t.ID)
			kind = "tx"
		}
		for _, id := range twins {
			if !seen[kind+fmt.Sprint(id)] {
				seen[kind+fmt.Sprint(id)] = true
				g = append(g, db.CorrTarget{Kind: kind, ID: id, Amount: t.Amount, Name: t.Name})
			}
		}
	}
	was := g[0].Amount
	changed, onlyChecks := 0, true
	for _, t := range g {
		if b.setCorrectedAmount(ctx, rawID, t, c.New) {
			changed++
			if t.Kind != "receipt" {
				onlyChecks = false
			}
		}
	}
	if changed == 0 {
		return done()
	}
	if onlyChecks {
		who := ""
		if g[0].Name != "" {
			who = " (" + g[0].Name + ")"
		}
		say(fmt.Sprintf("✏️ Исправил сумму чека%s: было %s ₽, стало %s ₽.", who, formatMoney(was), formatMoney(c.New)))
	} else {
		say(fmt.Sprintf("✏️ Исправил: %s — было %s ₽, стало %s ₽.", g[0].Name, formatMoney(was), formatMoney(c.New)))
	}
	return done()
}
