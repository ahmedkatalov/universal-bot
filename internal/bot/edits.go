package bot

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ledger"
	"whatsapp-bot/internal/parser"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
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
	// Платёж/ФИО сотрудника, написанный боту («Джарвис, Каталов Ахмед 15000»),
	// хранится без обращения — и правку сравниваем без него.
	if stripped, addressed := b.stripBotName(newText); addressed {
		t, ok := b.employeeAccountingText(ctx, msg, stripped)
		switch {
		case ok:
			newText = t
		case ref.HandledAs == "answer":
			newText = stripped
		default:
			return false // теперь это обращение к боту, а не запись учёта
		}
	} else if b.isMentionedText(newText) {
		newText = b.stripBotMention(newText)
	}
	if newText == "" || newText == strings.TrimSpace(ref.Body) {
		return false
	}

	undo, err := b.db.PrepareMessageEdit(ctx, ref.ID, newText)
	if err != nil {
		fmt.Println("Правка сообщения: ошибка:", err)
		return false
	}
	em := b.editedMessage(msg, ref, newText)
	em.Info.ID = origID

	// Ответ на вопрос бота — применяем новый ответ к тому же вопросу.
	if ref.HandledAs == "answer" {
		if !q.quiet() {
			b.reapplyAnswer(withAnswerRaw(ctx, ref.ID), em, ref, newText, say)
		}
		return true
	}

	// Изменили поправку «не 5000 а 6000» — ищем, что исправлять, заново (могли
	// поменять и клиента, и старую сумму).
	if len(undo.Corrections) > 0 {
		if !q.quiet() && b.applyAmountCorrection(ctx, em, newText, ref.ID) {
			return true
		}
		say("✏️ Поправку изменили — вернул прежнюю сумму.")
	}

	// Сообщение-имя, которое уже подписало чек или уточнило клиента платежа:
	// новое имя — тем же чекам/платежам. Повторно как имя «для следующего чека»
	// его не ставим — оно уже было использовано.
	if len(undo.LabeledChecks)+len(undo.LabeledTxs) > 0 {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(newText, "это "), "Это "))
		if n, ok := looksLikeName(name); ok {
			name = n
		} else if n, ok := firstNameLine(name); ok {
			name = n // «Магомедов Ахмед\n12000» — имя на первой строке
		}
		changed := 0
		canonical, ok := b.confirmClientName(ctx, name)
		if ok {
			var cid *int
			if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
				cid = &id
			}
			for _, w := range undo.LabeledChecks {
				if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, w, canonical, cid, ref.ID); err == nil && found {
					changed++
					b.reresolveWatchByCheck(ctx, chat, w, canonical)
				}
			}
			if cid != nil {
				for _, tx := range undo.LabeledTxs {
					if b.db.SetTxClientFrom(ctx, tx, *cid, canonical, ref.ID) == nil {
						changed++
					}
				}
			}
		}
		_ = b.db.MarkMessageParsed(ctx, ref.ID)
		switch {
		case changed > 0:
			fmt.Printf("Правка сообщения %s: клиент теперь %q\n", origID, canonical)
			say("✏️ Имя исправили — теперь клиент " + canonical + ".")
		case len(undo.LabeledChecks) > 0:
			say("✏️ В сообщении больше нет ФИО клиента — чек снова без клиента, спрошу, чей он.")
		}
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
		_ = b.db.CarryOverEditedTx(ctx, ref.ID, undo.OldStates)
		return true
	}
	em.Info.Timestamp = ref.ReceivedAt
	b.processText(ctx, em, newText, ref.ID, senderName, true)
	// Что узнали о старых платежах (клиент из свайпа, «новый», поправки) — новым.
	if err := b.db.CarryOverEditedTx(ctx, ref.ID, undo.OldStates); err != nil {
		fmt.Println("Правка сообщения: перенос уточнений не удался:", err)
	}

	now, _ := b.db.TxRefsByRawID(ctx, ref.ID)
	say(editNote(undo.OldTx, now))
	fmt.Printf("Правка сообщения %s: было %v, стало %v\n", origID, undo.OldTx, now)
	return true
}

// editedMessage — исправленное сообщение как обычное (тот же отправитель и
// свайп, что у исходного), чтобы разобрать его тем же путём.
func (b *Bot) editedMessage(msg *events.Message, ref db.RawMessageRef, text string) *events.Message {
	em := &events.Message{Info: msg.Info}
	if ref.QuotedID != "" {
		em.Message = &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(text), ContextInfo: &waProto.ContextInfo{StanzaID: proto.String(ref.QuotedID)}}}
	} else {
		em.Message = &waProto.Message{Conversation: proto.String(text)}
	}
	return em
}

// reapplyAnswer — изменили ответ на вопрос бота: тот же вопрос получает новый
// ответ (имя и/или сумма чека, клиент налички, «новый/тот же», кто забрал).
func (b *Bot) reapplyAnswer(ctx context.Context, em *events.Message, ref db.RawMessageRef, text string, say func(string)) {
	chat := em.Info.Chat
	target, _ := strconv.Atoi(ref.AnswerTo)
	switch ref.AnswerKind {
	case "receipt":
		if handled, _ := b.applyReceiptAnswer(ctx, chat, ref.AnswerTo, text); handled {
			return
		}
	case "who":
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "это "), "Это "))
		if n, _, defer2 := clarifyNameFromReply(name); n != "" && !defer2 {
			name = n
		}
		canonical, ok := b.confirmClientName(ctx, name)
		if !ok {
			break
		}
		cid, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			break
		}
		txs, _ := b.db.TxIDsBySource(ctx, ref.ID)
		changed := 0
		for _, tx := range txs {
			if b.db.SetTxClientFrom(ctx, tx, cid, canonical, ref.ID) == nil {
				changed++
				if b.cashDupCheckOn() {
					_ = b.db.RecheckCashRepeat(ctx, tx)
				}
			}
		}
		if changed > 0 {
			say("✏️ Ответ изменили — теперь клиент " + canonical + ".")
			return
		}
	case "dup":
		if resolved, isNew := parseCashDupAnswer(text); resolved && target > 0 {
			if amount, client, err := b.db.RedecideCashDup(ctx, target, isNew); err == nil {
				if isNew {
					say(fmt.Sprintf("✏️ Ответ изменили — засчитал как новый платёж: наличка %s %s ₽.", client, formatMoney(amount)))
				} else {
					say(fmt.Sprintf("✏️ Ответ изменили — это повтор, не считаю: %s %s ₽.", client, formatMoney(amount)))
				}
				return
			}
		}
	case "collector":
		if target > 0 && b.applyCashCollectorReply(ctx, chat, target, text, true) {
			return
		}
	case "cmf":
		if target > 0 && b.applyCmfWatchAnswerOpts(ctx, chat, target, text, answerOpts{mode: modeSwipe}) {
			return
		}
	}
	fmt.Printf("Правка ответа %s (%s): новый ответ не понял — оставил прежний\n", ref.QuotedID, ref.AnswerKind)
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

// corrGroup — одна цель поправки: записи одного платежа (как его считает
// движок подсчёта) и остальные записи этого платежа, которых среди найденных
// нет (текст-пара чека от другого сотрудника, копия чека в другой группе).
type corrGroup struct {
	ts    []db.CorrTarget
	twins []db.CorrTarget
}

// groupCorrTargets — чек и его текст-пара (тот же платёж по движку подсчёта)
// — одна цель поправки; текст, который движок считает отдельным платежом, —
// своя цель.
func (b *Bot) groupCorrTargets(ctx context.Context, ts []db.CorrTarget) []corrGroup {
	var at []time.Time
	for _, t := range ts {
		at = append(at, t.Received)
	}
	x := b.loadPayIndex(ctx, at)
	var groups []corrGroup
	var pays []*ledger.Payment
	for _, t := range ts {
		p := x.payment(t.Kind, t.ID)
		placed := false
		if p != nil {
			for gi := range groups {
				if pays[gi] == p {
					groups[gi].ts = append(groups[gi].ts, t)
					placed = true
					break
				}
			}
		}
		if !placed {
			groups = append(groups, corrGroup{ts: []db.CorrTarget{t}})
			pays = append(pays, p)
		}
	}
	for gi, p := range pays {
		if p == nil {
			continue
		}
		seen := map[string]bool{}
		for _, t := range groups[gi].ts {
			seen[evKey(t.Kind, t.ID)] = true
		}
		// Копии, пересланные ботом, повторяют сумму исходного чека сами.
		for _, c := range x.checks(p) {
			if k := evKey("receipt", c.ID); !seen[k] {
				seen[k] = true
				groups[gi].twins = append(groups[gi].twins, db.CorrTarget{Kind: "receipt", ID: c.ID, Name: c.Client, Amount: c.Amount, ContactID: c.ContactID})
			}
		}
		for _, t := range x.texts(p) {
			if k := evKey("tx", t.ID); !seen[k] {
				seen[k] = true
				groups[gi].twins = append(groups[gi].twins, db.CorrTarget{Kind: "tx", ID: t.ID, Name: t.Client, Amount: t.Amount, ContactID: t.ContactID, Cash: t.Cash})
			}
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
	groups := b.groupCorrTargets(ctx, targets)
	switch {
	case len(groups) == 0:
		say(fmt.Sprintf("Не нашёл платёж на %s ₽, который нужно исправить. Ответьте свайпом на сообщение с ним.", formatMoney(c.Old)))
		return done()
	case len(groups) > 1:
		var names []string
		for _, g := range groups {
			names = append(names, g.ts[0].Name)
		}
		amt := groups[0].ts[0].Amount
		say(fmt.Sprintf("Платежей на %s ₽ несколько (%s) — какой исправить? Ответьте свайпом на нужный: «не %s а %s».",
			formatMoney(amt), strings.Join(names, ", "), formatMoney(amt), formatMoney(c.New)))
		return done()
	}
	// Остальные записи того же платежа (текст-пара чека, чек текста) — тоже.
	g := append(groups[0].ts, groups[0].twins...)
	if math.Abs(g[0].Amount-c.New) < 0.01 {
		return done() // «правильно 5000» — подтверждение, менять нечего
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
