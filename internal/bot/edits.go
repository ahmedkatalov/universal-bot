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
	// Текст-итог платежа частями: его новую сумму проверяем, как поправку
	// «не 15000 а 16000» (до правки — пока движок ещё видит итог с его чеками).
	// И при импорте истории: спросить нельзя, но и считать итог сверх чеков —
	// тоже (прежняя сумма остаётся, вопрос — в лог).
	var splits []editedSplit
	if ref.HandledAs == "" {
		splits = b.splitTotalsOfMessage(ctx, ref)
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
		for _, ask := range b.keepEditedSplitTotals(ctx, ref, splits) {
			fmt.Printf("Правка сообщения %s при импорте истории — вопрос не отправлен: %s\n", origID, ask)
		}
		return true
	}
	em.Info.Timestamp = ref.ReceivedAt
	b.processText(ctx, em, newText, ref.ID, senderName, true)
	// Что узнали о старых платежах (клиент из свайпа, «новый», поправки) — новым.
	if err := b.db.CarryOverEditedTx(ctx, ref.ID, undo.OldStates); err != nil {
		fmt.Println("Правка сообщения: перенос уточнений не удался:", err)
	}
	asks := b.keepEditedSplitTotals(ctx, ref, splits)

	now, _ := b.db.TxRefsByRawID(ctx, ref.ID)
	say(editNote(undo.OldTx, now))
	for _, ask := range asks {
		say(ask)
	}
	fmt.Printf("Правка сообщения %s: было %v, стало %v\n", origID, undo.OldTx, now)
	return true
}

// editedSplit — текст-итог платежа частями в сообщении до правки и такие же
// итоги в других группах, связанные с его чеками через него.
type editedSplit struct {
	st     *splitTotal
	others []*ledger.Text
}

// splitTotalsOfMessage — платежи сообщения, которые движок считает текстом-
// итогом платежа частями (сумма его чеков).
func (b *Bot) splitTotalsOfMessage(ctx context.Context, ref db.RawMessageRef) []editedSplit {
	old, err := b.db.TxRefsByRawID(ctx, ref.ID)
	if err != nil || len(old) == 0 {
		return nil
	}
	x := b.loadPayIndex(ctx, [][]time.Time{{ref.ReceivedAt}})
	var out []editedSplit
	for _, t := range old {
		if st := x.splitOf("tx", t.ID); st != nil {
			out = append(out, editedSplit{st: st, others: sameTotalsElsewhere(st, x)})
		}
	}
	return out
}

// keepEditedSplitTotals — изменили в WhatsApp текст-итог платежа частями
// («Магомедов Ахмед 15000» про чеки 10000 + 5000 → 16000). Решает движок по
// записям, как они есть сейчас (после нового разбора и переноса поправок):
// новый текст — пара своего чека («10000» про чек 10000), итог других частей
// (прислали доплату чеком, лишний чек удалили) или вовсе не в сборе — сумма
// такая, как исправили, без вопросов. Только если новый текст посчитан
// отдельным платежом без чеков (сверх своих чеков: сбор 31000), как и поправка
// «не 15000 а 16000» на итог, оставляем прежнюю сумму и спрашиваем, доплата
// это или ошибка. Такой же итог в другой группе, связанный с чеками только
// через этот, — тоже не должен посчитаться сверх чеков (keepOtherGroupTotals).
// Возвращает вопросы и пояснения (при импорте истории их не отправляют — только
// в лог, а сумма остаётся прежней так же, как в живой правке).
func (b *Bot) keepEditedSplitTotals(ctx context.Context, ref db.RawMessageRef, splits []editedSplit) []string {
	if len(splits) == 0 {
		return nil
	}
	now, err := b.db.TxRefsByRawID(ctx, ref.ID)
	if err != nil {
		return nil
	}
	type pick struct {
		st *splitTotal
		n  db.TxRef
	}
	var picks []pick
	used := map[int]bool{}
	for _, es := range splits {
		st := es.st
		// Новый платёж того же клиента в сообщении — один (несколько — это уже
		// не итог, а суммы по чекам: «10000», «5000» — пары своих чеков).
		var same []db.TxRef
		for _, n := range now {
			if !used[n.ID] && !n.Cash && sameClientMoney(n.Name, st.text.Client) {
				same = append(same, n)
			}
		}
		if len(same) != 1 {
			continue
		}
		n := same[0]
		used[n.ID] = true
		if math.Abs(n.Amount-st.text.Amount) < 0.01 {
			continue // сумма итога та же
		}
		picks = append(picks, pick{st, n})
	}
	var out []string
	held := map[*splitTotal]bool{}
	if len(picks) > 0 {
		x := b.loadPayIndex(ctx, [][]time.Time{{ref.ReceivedAt}})
		for _, pk := range picks {
			if x.splitOf("tx", pk.n.ID) != nil {
				continue // итог своих чеков (как они есть сейчас)
			}
			if p := x.payment("tx", pk.n.ID); p == nil || len(p.Checks) > 0 {
				continue // пара своего чека или не в сборе — сверх чеков не считается
			}
			if b.db.SetTxAmount(ctx, pk.n.ID, pk.st.text.Amount) == nil {
				fmt.Printf("Правка итога платежа частями %s: %v вместо %v не сходится с чеками — спрашиваю\n", pk.st.label(), pk.n.Amount, pk.st.text.Amount)
				out = append(out, splitTotalQuestion(pk.st, pk.n.Amount))
				held[pk.st] = true
			}
		}
	}
	for _, es := range splits {
		if len(es.others) == 0 || held[es.st] {
			continue // прежняя сумма итога оставлена — такие же итоги связаны, как были
		}
		if s := b.keepOtherGroupTotals(ctx, ref, es); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// keepOtherGroupTotals — правка итога es.st: такой же итог в другой группе
// (es.others) движок связывал с чеками только через этот (сравнивает итоги
// по сумме). Если после правки тот итог посчитан отдельным платежом без чеков
// (сверх чеков: сбор 31000 / 30000), а правка пересказала тот же платёж (итог
// с доплатой, уже присланной чеком; итог расписали по чекам), в учёте остаётся
// прежний итог этого сообщения: его сумма — у первой новой записи того же
// клиента, остальные записи того же клиента из нового разбора — не в учёте.
// Сбор не меняется, бот объясняет почему. Клиента в сообщении сменили или от
// части чеков отказались («15000» → «10000»: чек 5000 не этого платежа) —
// прежний итог не держим (иначе, когда чек 5000 назовут другим клиентом,
// итог 15000 посчитался бы сам), только спрашиваем про тот итог. Возвращает
// пояснение.
func (b *Bot) keepOtherGroupTotals(ctx context.Context, ref db.RawMessageRef, es editedSplit) string {
	st := es.st
	recs := append(splitRecs(st, es.others), []time.Time{ref.ReceivedAt})
	after := b.loadPayIndex(ctx, recs)
	lost := lostTotals(after, es.others)
	if len(lost) == 0 {
		return ""
	}
	where := b.totalsWhere(ctx, lost)
	warn := func() string {
		fmt.Printf("Правка итога платежа частями %s: такой же итог %s теперь считается отдельным платежом\n", st.label(), where)
		return fmt.Sprintf("❓ «%s» — итог платежа частями: чеки %s ₽. Такой же итог есть %s — с чеками он был связан только через это сообщение"+
			" и после правки считается отдельным платежом сверх чеков. Если это тот же платёж — исправьте или удалите его там"+
			" (или попросите владельца исключить его).", st.label(), st.partsLine(nil, 0), where)
	}
	now, err := b.db.TxRefsByRawID(ctx, ref.ID)
	if err != nil {
		return ""
	}
	var same []db.TxRef
	for _, n := range now {
		if !n.Cash && sameClientMoney(n.Name, st.text.Client) {
			same = append(same, n)
		}
	}
	if len(same) == 0 || !coversParts(after, st, same) {
		// Клиента сменили или от части чеков отказались («15000» → «10000»:
		// чек 5000 — не этого платежа) — прежний итог не держим: тот итог
		// в другой группе теперь спорит с этим сообщением, спрашиваем.
		return warn()
	}
	carrier := same[0]
	if b.db.SetTxAmount(ctx, carrier.ID, st.text.Amount) != nil {
		return warn()
	}
	var dropped []int
	for _, n := range same[1:] {
		if b.db.SetOperationIgnored(ctx, "transaction", n.ID, true) == nil {
			dropped = append(dropped, n.ID)
		}
	}
	if still := lostTotals(b.loadPayIndex(ctx, recs), lost); len(still) > 0 {
		// Прежний итог связь не вернул — правку не держим.
		_ = b.db.SetTxAmount(ctx, carrier.ID, carrier.Amount)
		for _, id := range dropped {
			_ = b.db.SetOperationIgnored(ctx, "transaction", id, false)
		}
		return warn()
	}
	fmt.Printf("Правка итога платежа частями %s: такой же итог %s связан с чеками только через него — оставил прежний итог\n", st.label(), where)
	return fmt.Sprintf("❓ «%s» — итог платежа частями: чеки %s ₽. Такой же итог есть %s, и с чеками он связан только через это сообщение:"+
		" после правки он посчитался бы отдельным платежом сверх чеков. Поэтому в учёте пока оставил прежний итог %s ₽ — сбор не изменился."+
		" Если итог %s неверный или лишний — пусть его исправят или удалят там.",
		st.label(), st.partsLine(nil, 0), where, formatMoney(st.text.Amount), where)
}

// coversParts — новые записи того же клиента same (книга after) по-прежнему
// про все посчитанные части итога st: каждая часть — пара одной из них или
// часть итога одной из них. Правка пересказала тот же платёж (итог с доплатой,
// итог расписали по чекам), а не отказалась от части его чеков.
func coversParts(after *payIndex, st *splitTotal, same []db.TxRef) bool {
	in := map[int]bool{}
	for _, n := range same {
		if s := after.splitOf("tx", n.ID); s != nil {
			for _, p := range s.parts {
				for _, c := range p.Checks {
					in[c.ID] = true
				}
			}
			for _, c := range s.missing {
				in[c.ID] = true
			}
			continue
		}
		if p := after.payment("tx", n.ID); p != nil {
			for _, c := range p.Checks {
				in[c.ID] = true
			}
		}
	}
	for _, p := range st.parts {
		ok := false
		for _, c := range p.Checks {
			ok = ok || in[c.ID]
		}
		if !ok {
			return false
		}
	}
	return true
}

// totalsWhere — «в группе «Оплата клиентов»» для текстов ts (по их группам).
func (b *Bot) totalsWhere(ctx context.Context, ts []*ledger.Text) string {
	groups := b.joinedGroups(ctx)
	var ws []string
	seen := map[string]bool{}
	for _, t := range ts {
		w := "в другой группе"
		if jid, err := types.ParseJID(t.Group); err == nil && groups[jid] != "" {
			w = "в группе «" + groups[jid] + "»"
		}
		if !seen[w] {
			seen[w] = true
			ws = append(ws, w)
		}
	}
	return strings.Join(ws, ", ")
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
	// split — цель — текст-итог платежа частями: его сумма — сумма чеков,
	// поправка итога на чеки не переносится.
	split *splitTotal
	// totals — итоги платежа частями, в которые входит исправляемый чек: итог
	// следует за суммой своей части (иначе он перестанет сходиться с чеками и
	// посчитается отдельным платежом сверх них).
	totals []*splitTotal
}

// groupCorrTargets — чек и его текст-пара (тот же платёж по движку подсчёта)
// — одна цель поправки; текст, который движок считает отдельным платежом, —
// своя цель. Текст-итог платежа частями — своя цель без чеков: его чеки — не
// его копии.
func (b *Bot) groupCorrTargets(ctx context.Context, ts []db.CorrTarget) []corrGroup {
	var at [][]time.Time
	for _, t := range ts {
		at = append(at, []time.Time{t.Received})
	}
	x := b.loadPayIndex(ctx, at)
	var groups []corrGroup
	var pays []*ledger.Payment
	for _, t := range ts {
		if st := x.splitOf(t.Kind, t.ID); st != nil {
			groups = append(groups, corrGroup{ts: []db.CorrTarget{t}, split: st})
			pays = append(pays, nil)
			continue
		}
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
		if groups[gi].split != nil {
			continue
		}
		// Итоги платежа частями, в которые входит этот чек (и посчитанная
		// часть, и часть, которой сейчас нет в сборе).
		ids := checkIDs(p)
		for _, t := range groups[gi].ts {
			if t.Kind == "receipt" {
				ids = append(ids, t.ID)
			}
		}
		groups[gi].totals = x.totalsOf(ids...)
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
	if st := groups[0].split; st != nil && math.Abs(st.sum()-c.New) >= 0.5 {
		// Итог платежа частями — сумма его чеков. Другой итог с чеками не
		// сойдётся: текст посчитался бы отдельным платежом сверх них, а на
		// какой-то из чеков новую сумму не перенести. Ничего не меняем — спрашиваем.
		say(splitTotalQuestion(st, c.New))
		return done()
	}
	was := g[0].Amount
	changed, onlyChecks := 0, true
	ids := map[int]bool{}
	for _, t := range g {
		if b.setCorrectedAmount(ctx, rawID, t, c.New) {
			changed++
			if t.Kind != "receipt" {
				onlyChecks = false
			} else {
				ids[t.ID] = true
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
		say(fmt.Sprintf("✏️ Исправил сумму чека%s: было %s ₽, стало %s ₽.", who, formatMoney(was), formatMoney(c.New)) +
			b.followSplitTotals(ctx, rawID, groups[0].totals, ids, c.New-was, c.New))
	} else {
		say(fmt.Sprintf("✏️ Исправил: %s — было %s ₽, стало %s ₽.", g[0].Name, formatMoney(was), formatMoney(c.New)) +
			b.followSplitTotals(ctx, rawID, groups[0].totals, ids, c.New-was, c.New))
	}
	return done()
}

// followSplitTotals — поправили сумму чека, который входит в платёж частями с
// итогом текстом: итог — сумма частей и меняется на ту же разницу (поправка
// записана — удалят её, итог сдвинется обратно на ту же разницу, даже если его
// с тех пор сдвинула поправка другой части). Иначе итог перестал бы сходиться
// с чеками и посчитался отдельным платежом сверх них. Возвращает пояснение.
func (b *Bot) followSplitTotals(ctx context.Context, rawID int, totals []*splitTotal, ids map[int]bool, delta, to float64) string {
	var out string
	for _, st := range totals {
		nt := math.Round((st.text.Amount+delta)*100) / 100
		if nt <= 0 || math.Abs(delta) < 0.01 {
			continue
		}
		if b.db.SetTxAmount(ctx, st.text.ID, nt) != nil {
			continue
		}
		if err := b.db.RecordFollowCorrection(ctx, rawID, st.text.ID, st.text.Amount, nt); err != nil {
			fmt.Println("Поправка итога платежа частями: не записалась:", err)
		}
		out += fmt.Sprintf(" Это часть платежа «%s» (итог текстом): итог — сумма чеков %s, поправил и его: было %s ₽, стало %s ₽."+
			" Если итог %s ₽ был верный — значит, ошибка в другом чеке: поправьте его сумму свайпом.",
			st.label(), st.partsLine(ids, to), formatMoney(st.text.Amount), formatMoney(nt), formatMoney(st.text.Amount))
	}
	return out
}

// splitTotalQuestion — поправили сумму текста-итога платежа частями: чеки
// дают другую сумму. Спрашиваем, что это — доплата или ошибка, и как её внести.
func splitTotalQuestion(st *splitTotal, to float64) string {
	sum := st.sum()
	who := strings.TrimSpace(st.text.Client)
	if who == "" {
		who = "<ФИО клиента>"
	}
	q := fmt.Sprintf("❓ «%s» — это итог платежа частями: чеки %s = %s ₽. Итог %s ₽ с чеками не сходится",
		st.label(), st.partsLine(nil, 0), formatMoney(sum), formatMoney(to))
	diff := to - sum
	if diff > 0 {
		plain := strconv.FormatFloat(math.Round(diff*100)/100, 'f', -1, 64) // «1000» — как пишут платёж
		q += fmt.Sprintf(" — это доплата %s ₽ или ошибка? Сумму пока не менял. Если доплата — пришлите её чек или напишите отдельно: «%s %s».",
			formatMoney(diff), who, plain)
	} else {
		q += fmt.Sprintf(" (на %s ₽ меньше) — это ошибка в сумме какого-то чека? Сумму пока не менял.", formatMoney(-diff))
	}
	return q + " Если ошибка в сумме чека — ответьте свайпом на нужный чек: «не <сумма на чеке> а <правильная>»."
}
