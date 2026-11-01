package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии L4 (тексты): наличка, переотправленная с исправленной опечаткой в
// имени, и 3 чтения ИИ, разошедшиеся в порядке слов имени.

// l4textsRevoke — сотрудник удалил своё сообщение в WhatsApp.
func l4textsRevoke(grp, snd types.JID, origID string) *events.Message {
	return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(origID), Participant: proto.String(snd.String())}}}}
}

// l4textsHeld — сколько налички группы придержано как возможный повтор.
func l4textsHeld(e *e2eBot, g types.JID) int {
	n, err := e.db.CountCashDupNeedingConfirm(context.Background(), g.String(), time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// 24. Наличку переотправили с исправленной опечаткой в фамилии — это тот же
// клиент и та же сумма за 30 дней: придержать и спросить «новый или тот же?»,
// как при одинаковом написании. Разные клиенты (Омаров ≠ Умаров) — не повтор.
func TestL4texts_24_TypoResendCashHeld(t *testing.T) {
	ctx := context.Background()
	for _, answer := range []string{"тот же", "новый"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Магамедов Ахмед", "Магомедов Ахмед")
		grp, emp := e2eGroup("l4ty"), user("79285550004")
		e.groupsCache[grp] = "Оплата"
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед нал 15000"))
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000"))
		if got := dayTotals(e, grp); sum(got) != 15000 {
			t.Fatalf("та же наличка, вторая — с исправленной опечаткой: сбор %v (%v), повтор не придержан", sum(got), got)
		}
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("НОВЫЙ платёж")
		if q.ID == "" {
			t.Fatalf("нет вопроса «новый или тот же?»: %+v", e.sent())
		}
		if !strings.Contains(q.Text, "Магомедов Ахмед") || !strings.Contains(q.Text, "Магамедов Ахмед") ||
			!strings.Contains(q.Text, time.Now().Format("02.01")) {
			t.Errorf("вопрос должен назвать прежнюю запись («Магамедов Ахмед», дата): %q", q.Text)
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", answer, q.ID, e.botJID(), q.Text))
		want := 15000.0
		if answer == "новый" {
			want = 30000
		}
		if got := dayTotals(e, grp); sum(got) != want {
			t.Errorf("ответ «%s»: сбор %v (%v), хочу %v", answer, sum(got), got, want)
		}
	}

	// Омаров и Умаров — разные клиенты: одинаковая сумма не делает их повтором.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Омаров Ахмед", "Умаров Ахмед")
	grp, emp := e2eGroup("l4ou"), user("79285550005")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Омаров Ахмед нал 15000"))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Умаров Ахмед нал 15000"))
	if got := dayTotals(e, grp); sum(got) != 30000 || l4textsHeld(e, grp) != 0 {
		t.Errorf("Омаров и Умаров — разные клиенты: сбор %v (%v), придержано %d", sum(got), got, l4textsHeld(e, grp))
	}
}

// 24b. Сообщение с опечаткой удалили — повтора больше нет: придержанная
// исправленная наличка идёт в сбор (без вопроса, ответ на который «тот же»
// потерял бы деньги).
func TestL4texts_24_TypoOriginalDeletedReleasesHold(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магамедов Ахмед", "Магомедов Ахмед")
	grp, emp := e2eGroup("l4td"), user("79285550006")
	m1 := groupText(grp, emp, "Сафаи", "Магамедов Ахмед нал 15000")
	e.sendGroup(m1)
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000"))
	if h := l4textsHeld(e, grp); h != 1 {
		t.Fatalf("исправленная наличка не придержана как возможный повтор: придержано %d, сбор %v", h, dayTotals(e, grp))
	}
	e.event(l4textsRevoke(grp, emp, m1.Info.ID))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 15000 || sum(got) != 15000 || l4textsHeld(e, grp) != 0 {
		t.Errorf("сообщение с опечаткой удалили: сбор %v, придержано %d — хочу Магомедов Ахмед 15000 и ничего не придержано", got, l4textsHeld(e, grp))
	}
}

// 24c. Наличку записали без имени («Нал 15000»), клиента назвали ответом на
// «❓» с иным написанием — это повтор недавней налички того же клиента: придержать.
func TestL4texts_24_AnswerNamesTypoClientRechecked(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		if strings.Contains(c.lastUserText(), "Магамедов") {
			return aiReply{Text: `{"payments":[{"name":"Магамедов Ахмед","amount":15000,"cash":true}],"clarify":""}`}
		}
		return aiReply{Text: `{"payments":[],"clarify":"Чья это наличка 15000?"}`}
	}
	grp, emp := e2eGroup("l4ta"), user("79285550007")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед нал 15000"))
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Нал 15000"))
	var q e2eSent
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса «❓ чья наличка?»: %+v", e.sentSince(n0))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 15000 || l4textsHeld(e, grp) != 1 {
		t.Errorf("клиента наличке назвали с иным написанием — повтор не придержан: сбор %v (%v), придержано %d", sum(got), got, l4textsHeld(e, grp))
	}
}

// 25. Одно из 3 чтений ИИ сорвалось, два других записали имя в разном порядке
// слов («Ахмед Магомедов» / «Магомедов Ахмед») — это один платёж, а не потеря.
func TestL4texts_25_ConsensusNameWordOrder(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	reads := make(chan string, 2)
	reads <- `{"payments":[{"name":"Ахмед Магомедов","amount":15000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Магомедов Ахмед","amount":15000,"cash":false}],"clarify":""}`
	e.ai = func(c aiCall) aiReply {
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		select {
		case r := <-reads:
			return aiReply{Text: r}
		default:
			return aiReply{Text: "Извините, не могу обработать запрос."} // чтение сорвалось
		}
	}
	grp, emp := e2eGroup("l4cs"), user("79285550009")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Ахмед Магомедов 15000"))
	if got := dayTotals(e, grp); sum(got) != 15000 {
		t.Errorf("«Ахмед Магомедов 15000»: сбор %v (%v) — платёж потерян", sum(got), got)
	}
}

// 25 (без базы). Сведение чтений: порядок слов, регистр и падеж — один клиент;
// Омаров и Умаров — разные (строгое сравнение для денег), их голоса не
// складываются; два похожих имени в одном чтении — два клиента.
func TestL4texts_25_ConsensusSpellings(t *testing.T) {
	got := consensusPayments([][]aiPayment{
		{{Name: "Ахмед Магомедов", Amount: 15000}},
		{{Name: "магомедов ахмед", Amount: 15000}},
		{{Name: "Магомедову Ахмеду", Amount: 15000}},
	}, 3)
	if len(got) != 1 || got[0].Amount != 15000 {
		t.Errorf("три написания одного клиента — один платёж 15000, получили %+v", got)
	}
	if got := consensusPayments([][]aiPayment{{{Name: "Омаров Ахмед", Amount: 15000}}, {{Name: "Умаров Ахмед", Amount: 15000}}}, 2); len(got) != 0 {
		t.Errorf("Омаров и Умаров склеены в один платёж: %+v", got)
	}
	// Два похожих имени в ОДНОМ чтении — два разных клиента, каждый со своим именем.
	two := []aiPayment{{Name: "Магомедов Ахмед", Amount: 15000}, {Name: "Магомедов Ахмад", Amount: 15000}}
	got = consensusPayments([][]aiPayment{two, {two[1], two[0]}, two}, 3)
	names := map[string]bool{}
	for _, p := range got {
		names[p.Name] = true
	}
	if len(got) != 2 || !names["Магомедов Ахмед"] || !names["Магомедов Ахмад"] {
		t.Errorf("Ахмед и Ахмад из одного сообщения: %+v", got)
	}
}

// 25b. Чтения разошлись (одно сорвалось, два назвали разных клиентов) —
// вердикта нет: сообщение не закрывается молча, а перечитывается позже.
func TestL4texts_25_DisputedReadsRetried(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	reads := make(chan string, 2)
	reads <- `{"payments":[{"name":"Магомедов Ахмед","amount":15000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Исаев Харон","amount":15000,"cash":false}],"clarify":""}`
	consistent := false
	e.ai = func(c aiCall) aiReply {
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		<-mu
		ok := consistent
		mu <- struct{}{}
		if ok {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":15000,"cash":false}],"clarify":""}`}
		}
		select {
		case r := <-reads:
			return aiReply{Text: r}
		default:
			return aiReply{Text: "Извините, не могу обработать запрос."}
		}
	}
	grp, emp := e2eGroup("l4dr"), user("79285550010")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000 перевел"))
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Fatalf("чтения разошлись, а платёж записан наугад: %v", got)
	}
	ms, err := e.db.UnparsedTextMessagesSince(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("чтения разошлись, а сообщение закрыто как разобранное (неразобранных %d) — платёж потерян", len(ms))
	}
	<-mu
	consistent = true
	mu <- struct{}{}
	e.retryAITextAfterBackoff(ctx)
	e.waitIdle()
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 || sum(got) != 15000 {
		t.Errorf("повторное чтение не записало платёж: %v", got)
	}
}

// 25c. Все 3 чтения удались, но назвали трёх разных клиентов — вердикта нет:
// спросить сотрудника «❓», а не потерять платёж молча.
func TestL4texts_25_DisputedReadsAsk(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	names := make(chan string, 3)
	names <- "Магомедов Ахмед"
	names <- "Исаев Харон"
	names <- "Хасанов Ислам"
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		n := "Магомедов Ахмед"
		select {
		case n = <-names:
		default:
		}
		return aiReply{Text: `{"payments":[{"name":"` + n + `","amount":15000,"cash":false}],"clarify":""}`}
	}
	grp, emp := e2eGroup("l4da"), user("79285550011")
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Ахмед Магомедов перевел 15000"))
	var q e2eSent
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("чтения назвали трёх разных клиентов — бот молчит, сбор %v: %+v", dayTotals(e, grp), e.sentSince(n0))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед 15000", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 15000 {
		t.Errorf("после ответа «Магомедов Ахмед 15000»: сбор %v (%v), хочу 15000", sum(got), got)
	}
}

// R0. «Тот же», а потом удалили сообщение с опечаткой — наличка не пропадает:
// повтор возвращается в сбор (повторять больше нечего). То же при одинаковом
// написании: удалили исходное — остаётся признанный повтором.
func TestL4texts_R0_SameThenDeleteOriginalRestores(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ first, second string }{
		{"Магамедов Ахмед", "Магомедов Ахмед"}, // переотправили с исправленной опечаткой
		{"Магомедов Ахмед", "Магомедов Ахмед"}, // просто прислали дважды
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		if tc.first == tc.second {
			e.ai = aiNamesAndPayments(tc.first)
		} else {
			e.ai = aiNamesAndPayments(tc.first, tc.second)
		}
		grp, emp := e2eGroup("l4r0"), user("79285550104")
		e.groupsCache[grp] = "Оплата"
		m1 := groupText(grp, emp, "Сафаи", tc.first+" нал 15000")
		e.sendGroup(m1)
		e.sendGroup(groupText(grp, emp, "Сафаи", tc.second+" нал 15000"))
		var q e2eSent
		for i := 0; i < 4 && q.ID == ""; i++ {
			e.age(grp.String())
			e.clarifyTick(ctx)
			q = e.lastWith("НОВЫЙ платёж")
		}
		if q.ID == "" {
			t.Fatalf("%s / %s: нет вопроса «новый или тот же?»: %+v", tc.first, tc.second, e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "тот же", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); sum(got) != 15000 {
			t.Fatalf("%s / %s: после «тот же» сбор %v (%v), хочу 15000", tc.first, tc.second, sum(got), got)
		}
		e.event(l4textsRevoke(grp, emp, m1.Info.ID)) // удалили первое сообщение
		got := dayTotals(e, grp)
		if sum(got) != 15000 || got[tc.second] != 15000 || l4textsHeld(e, grp) != 0 {
			t.Errorf("%s / %s: после «тот же» удалили первое сообщение — сбор %v (%v), придержано %d; хочу %s 15000",
				tc.first, tc.second, sum(got), got, l4textsHeld(e, grp), tc.second)
		}
	}
}

// R0b. Повтор вернулся, но у клиента есть ещё одна такая же наличка за 30 дней
// (не та, о которой спрашивали): вернувшийся снова придержан и бот спросит
// заново — молча ни засчитать дважды, ни потерять.
func TestL4texts_R0_RestoredRepeatAskedAgainIfAnotherMatch(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магамедов Ахмед", "Магомедов Ахмед")
	grp, emp := e2eGroup("l4r0b"), user("79285550105")
	e.groupsCache[grp] = "Оплата"
	m0 := groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000")
	m0.Info.Timestamp = time.Now().AddDate(0, 0, -20) // прошлый взнос, 20 дней назад
	e.sendGroup(m0)
	m1 := groupText(grp, emp, "Сафаи", "Магамедов Ахмед нал 15000")
	e.sendGroup(m1)
	for i := 0; i < 4; i++ { // опечатка придержана как повтор взноса 20-дневной давности
		e.age(grp.String())
		e.clarifyTick(ctx)
	}
	if q := e.lastWith("НОВЫЙ платёж"); q.ID != "" {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "новый", q.ID, e.botJID(), q.Text))
	}
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000"))
	var q e2eSent
	for i := 0; i < 4 && q.ID == ""; i++ {
		e.age(grp.String())
		e.clarifyTick(ctx)
		for _, m := range e.sentSince(n0) {
			if strings.Contains(m.Text, "НОВЫЙ платёж") {
				q = m
			}
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса о переотправленной наличке: %+v", e.sentSince(n0))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "тот же", q.ID, e.botJID(), q.Text))
	n1 := len(e.sent())
	e.event(l4textsRevoke(grp, emp, m1.Info.ID))
	if h := l4textsHeld(e, grp); h != 1 {
		t.Fatalf("вернувшийся повтор совпадает с наличкой 20-дневной давности — должен быть придержан, придержано %d, сбор %v", h, dayTotals(e, grp))
	}
	e.age(grp.String())
	e.clarifyTick(ctx)
	var q2 e2eSent
	for _, m := range e.sentSince(n1) {
		if strings.Contains(m.Text, "НОВЫЙ платёж") {
			q2 = m
		}
	}
	if q2.ID == "" || !strings.Contains(q2.Text, m0.Info.Timestamp.Format("02.01")) {
		t.Fatalf("бот должен спросить заново, сравнив с наличкой %s: %+v", m0.Info.Timestamp.Format("02.01"), e.sentSince(n1))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "новый", q2.ID, e.botJID(), q2.Text))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 || sum(got) != 15000 {
		t.Errorf("после «новый» сегодняшний взнос в сборе: %v", got)
	}
}

// R1. Придержали из-за сегодняшней записи с опечаткой — вопрос называет её
// (дата и написание), а не прошломесячный взнос того же клиента.
func TestL4texts_R1_QuestionCitesTheHoldingRecord(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магамедов Ахмед", "Магомедов Ахмед")
	grp, emp := e2eGroup("l4r1"), user("79285550108")
	e.groupsCache[grp] = "Оплата"
	m0 := groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000")
	m0.Info.Timestamp = time.Now().AddDate(0, 0, -35) // прошлый месяц
	e.sendGroup(m0)
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед нал 15000"))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 15000"))
	var q e2eSent
	for i := 0; i < 4 && q.ID == ""; i++ {
		e.age(grp.String())
		e.clarifyTick(ctx)
		q = e.lastWith("НОВЫЙ платёж")
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса: %+v", e.sent())
	}
	if !strings.Contains(q.Text, time.Now().Format("02.01")) || !strings.Contains(q.Text, "Магамедов Ахмед") ||
		strings.Contains(q.Text, m0.Info.Timestamp.Format("02.01")) {
		t.Errorf("придержали из-за сегодняшней «Магамедов Ахмед», а вопрос называет другое: %q", q.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "тот же", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 15000 {
		t.Errorf("«тот же» про сегодняшнюю запись: сбор %v (%v), хочу 15000", sum(got), got)
	}
}

// R2. «Магомедов А.» подходит и к «Магомедов Ахмед», и к «Магомедов Алихан» —
// это не мост между ними: голоса разных клиентов не складываются, и платёж не
// достаётся тому, чьё чтение пришло первым.
func TestL4texts_R2_ConsensusInitialsNoChain(t *testing.T) {
	ah := aiPayment{Name: "Магомедов Ахмед", Amount: 15000}
	al := aiPayment{Name: "Магомедов Алихан", Amount: 15000}
	in := aiPayment{Name: "Магомедов А.", Amount: 15000}
	orders := [][]aiPayment{{ah, in, al}, {ah, al, in}, {al, ah, in}, {al, in, ah}, {in, ah, al}, {in, al, ah}}
	for _, o := range orders {
		got := consensusPayments([][]aiPayment{{o[0]}, {o[1]}, {o[2]}}, 3)
		if len(got) != 0 {
			t.Errorf("чтения %s / %s / %s: платёж %+v без большинства", o[0].Name, o[1].Name, o[2].Name, got)
		}
		if _, rest := consensusSplit([][]aiPayment{{o[0]}, {o[1]}, {o[2]}}, 3); !readsUndecided(rest, 3) {
			t.Errorf("чтения %s / %s / %s: вердикта нет — надо спросить", o[0].Name, o[1].Name, o[2].Name)
		}
	}
	// Инициалы к одному клиенту — сливаются, как раньше.
	for _, o := range [][]aiPayment{{ah, in, ah}, {in, ah, in}, {in, in, al}} {
		got := consensusPayments([][]aiPayment{{o[0]}, {o[1]}, {o[2]}}, 3)
		if len(got) != 1 || got[0].Amount != 15000 {
			t.Errorf("чтения %s / %s / %s — один клиент, хочу один платёж: %+v", o[0].Name, o[1].Name, o[2].Name, got)
		}
	}
}

// R2 (e2e). Оба клиента известны, три чтения: «Магомедов Алихан», «Магомедов
// А.», «Магомедов Ахмед» (в любом порядке) — бот спрашивает «❓», а не
// записывает платёж на одного из них молча.
func TestL4texts_R2_ConsensusInitialsAsks(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Магомедов Алихан", "Магомедов Алихан")
	names := make(chan string, 3)
	names <- "Магомедов Алихан"
	names <- "Магомедов А."
	names <- "Магомедов Ахмед"
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		n := "Магомедов А."
		select {
		case n = <-names:
		default:
		}
		return aiReply{Text: `{"payments":[{"name":"` + n + `","amount":15000,"cash":false}],"clarify":""}`}
	}
	grp, emp := e2eGroup("l4r2"), user("79285550112")
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов А. 15000"))
	asked := false
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") {
			asked = true
		}
	}
	got := dayTotals(e, grp)
	if !asked {
		t.Errorf("чтения назвали двух разных клиентов и инициалы — бот не спросил, сбор %v", got)
	}
	if got["Магомедов Ахмед"] > 0 || got["Магомедов Алихан"] > 0 {
		t.Errorf("платёж записан на одного из двух клиентов наугад: %v", got)
	}
}

// R3. Импорт истории: чтения разошлись, спросить нельзя — сообщение ждёт
// «пересчитай». После перезапуска бота фоновый повтор не спрашивает о нём в
// группе задним числом (отметка в базе, а не только в памяти).
func TestL4texts_R3_DisputedImportNotAskedAfterRestart(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	var mu sync.Mutex
	k := 0
	names := []string{"Магомедов Ахмед", "Исаев Харон", "Хасанов Ислам"}
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		mu.Lock()
		n := names[k%3]
		k++
		mu.Unlock()
		return aiReply{Text: `{"payments":[{"name":"` + n + `","amount":15000,"cash":false}],"clarify":""}`}
	}
	grp, emp := e2eGroup("l4r3"), user("79285550109")
	ts := time.Now().AddDate(0, 0, -2)
	text := "Ахмед Магомедов 15000"
	rawID, _, err := e.db.SaveRawMessage(ctx, e2eID("H"), grp.String(), emp.String(), emp.User, "Сафаи", text, false, "", ts)
	if err != nil {
		t.Fatal(err)
	}
	budget := 10
	e.importTextSilent(ctx, grp, "Сафаи", rawID, ts, text, &budget)
	e.waitIdle()
	n0 := len(e.sent())
	// перезапуск: память бота пуста
	e.aiRetryMu.Lock()
	e.aiDisputed, e.aiRetry = nil, nil
	e.aiRetryMu.Unlock()
	e.startedAt = time.Now()
	e.retryAITextAfterBackoff(ctx)
	e.waitIdle()
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") {
			t.Errorf("после перезапуска бот спросил группу о сообщении двухдневной давности из импорта: %q (цитата %q)", m.Text, m.Quoted)
		}
	}
	ms, err := e.db.UnparsedTextMessages(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range ms {
		found = found || m.ID == rawID
	}
	if !found {
		t.Errorf("спорное сообщение из импорта должно остаться неразобранным для «пересчитай»")
	}
}

// R3b. Давнее сообщение, перечитанное после сбоя ИИ, спорно — вопрос «❓»
// цитирует его: иначе в группе не понять, о каком платеже речь.
func TestL4texts_R3_LateDisputedQuestionQuotes(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	var mu sync.Mutex
	down := true
	k := 0
	names := []string{"Магомедов Ахмед", "Исаев Харон", "Хасанов Ислам"}
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		mu.Lock()
		defer mu.Unlock()
		if down {
			return aiReply{Text: "Извините, не могу обработать запрос."}
		}
		n := names[k%3]
		k++
		return aiReply{Text: `{"payments":[{"name":"` + n + `","amount":15000,"cash":false}],"clarify":""}`}
	}
	grp, emp := e2eGroup("l4r3b"), user("79285550113")
	m := groupText(grp, emp, "Сафаи", "Ахмед Магомедов перевел 15000")
	m.Info.Timestamp = time.Now().Add(-time.Hour)
	e.sendGroup(m)
	mu.Lock()
	down = false
	mu.Unlock()
	n0 := len(e.sent())
	e.retryAITextAfterBackoff(ctx)
	e.waitIdle()
	var q e2eSent
	for _, s := range e.sentSince(n0) {
		if strings.HasPrefix(s.Text, "❓") {
			q = s
		}
	}
	if q.ID == "" {
		t.Fatalf("чтения разошлись — бот должен спросить: %+v", e.sentSince(n0))
	}
	if q.Quoted != m.Info.ID {
		t.Errorf("вопрос о сообщении часовой давности без цитаты (%q): %q", q.Quoted, q.Text)
	}
}

// R4. В сообщении два платежа: первый все чтения прочли одинаково, второй —
// с разными клиентами. Второй не пропадает молча: деньги в сборе без клиента,
// бот спрашивает «чей платёж на 5 000 ₽?», ответ даёт клиента.
func TestL4texts_R4_PartialDisputeAsked(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	reads := make(chan string, 3)
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов","amount":5000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов Ахмед","amount":5000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов Алихан","amount":5000,"cash":false}],"clarify":""}`
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		select {
		case r := <-reads:
			return aiReply{Text: r}
		default:
			return aiReply{Text: "Извините, не могу обработать запрос."}
		}
	}
	grp, emp := e2eGroup("l4r4"), user("79285550107")
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон 20000\nМагомедов 5000"))
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 20000 || sum(got) != 25000 {
		t.Errorf("согласованный платёж и спорный (без клиента) — в сборе: %v, хочу 25000", got)
	}
	var q e2eSent
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" || !strings.Contains(q.Text, "5 000") {
		t.Fatalf("спорный платёж 5000 — бот должен спросить, чей он: %+v", e.sentSince(n0))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	got = dayTotals(e, grp)
	if got["Исаев Харон"] != 20000 || got["Магомедов Ахмед"] != 5000 || sum(got) != 25000 {
		t.Errorf("после ответа «Магомедов Ахмед»: %v", got)
	}
}

// R4b. То же, но одно из чтений сорвалось: сообщение не закрывается молча с
// одним Исаевым — перечитывается, и при том же расхождении бот спрашивает.
func TestL4texts_R4_PartialDisputeRetriedThenAsked(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	reads := make(chan string, 5)
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов","amount":5000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов Ахмед","amount":5000,"cash":false}],"clarify":""}`
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		select {
		case r := <-reads:
			return aiReply{Text: r}
		default:
			return aiReply{Text: "Извините, не могу обработать запрос."}
		}
	}
	grp, emp := e2eGroup("l4r4b"), user("79285550114")
	n0 := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон 20000\nМагомедов 5000"))
	ms, _ := e.db.UnparsedTextMessagesSince(ctx, time.Now().Add(-time.Hour), 50)
	if sum(dayTotals(e, grp)) != 0 || len(ms) != 1 {
		t.Fatalf("чтения разошлись (одно сорвалось) — сообщение ждёт перечитывания: сбор %v, неразобранных %d", dayTotals(e, grp), len(ms))
	}
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов","amount":5000,"cash":false}],"clarify":""}`
	reads <- `{"payments":[{"name":"Исаев Харон","amount":20000,"cash":false},{"name":"Магомедов Ахмед","amount":5000,"cash":false}],"clarify":""}`
	e.retryAITextAfterBackoff(ctx)
	e.waitIdle()
	asked := false
	for _, m := range e.sentSince(n0) {
		if strings.HasPrefix(m.Text, "❓") && strings.Contains(m.Text, "5 000") {
			asked = true
		}
	}
	if got := dayTotals(e, grp); sum(got) != 25000 || !asked {
		t.Errorf("после перечитывания: сбор %v (хочу 25000), спросили про 5 000: %v", got, asked)
	}
}
