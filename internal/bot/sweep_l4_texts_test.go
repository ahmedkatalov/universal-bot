package bot

import (
	"context"
	"strings"
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
	e.retryAIText(ctx)
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
