package bot

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"whatsapp-bot/internal/ai"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии: наличка без имени, вопрос «❓ чей платёж», работа без ИИ,
// «по 7500 за 2 месяца», удалённая наличка.

func aiClarifyOnly(q string) func(c aiCall) aiReply {
	return func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[],"clarify":"` + q + `"}`}
		}
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		return aiReply{Text: ""}
	}
}

// fakeNameExtract — как настоящая модель на «выдели имя КЛИЕНТА»: ФИО — это
// 2–3 слова с заглавной буквы («Магомедов Ахмед»), а «Мансур знает» или
// «Позже скажу» — не имя.
func fakeNameExtract(c aiCall) (aiReply, bool) {
	if !strings.Contains(c.System, "выдели имя КЛИЕНТА") {
		return aiReply{}, false
	}
	ws := strings.Fields(strings.Trim(c.lastUserText(), " .,!"))
	if len(ws) < 2 || len(ws) > 3 {
		return aiReply{Text: `{"client":""}`}, true
	}
	for _, w := range ws {
		r := []rune(w)
		if !unicode.IsUpper(r[0]) {
			return aiReply{Text: `{"client":""}`}, true
		}
		for _, x := range r {
			if !unicode.IsLetter(x) && x != '-' {
				return aiReply{Text: `{"client":""}`}, true
			}
		}
	}
	return aiReply{Text: `{"client":"` + strings.Join(ws, " ") + `"}`}, true
}

// Наличка без имени клиента — не «повтор» другой такой же налички.
func TestSweepUnnamedCashNotHeldAsRepeat(t *testing.T) {
	grp := e2eGroup("nal")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	m1 := groupText(grp, user("79280000001"), "Сафаи", "Нал 27000")
	m1.Info.Timestamp = time.Now().AddDate(0, 0, -3)
	e.sendGroup(m1)
	e.sendGroup(groupText(grp, user("79280000002"), "Рустам", "Нал 27000"))
	if got := sum(dayTotals(e, grp)); got != 27000 {
		t.Errorf("сегодняшняя наличка удержана как повтор чужой: сбор=%v, хочу 27000", got)
	}
	// Одно сообщение со списком: вторая 5000 — не повтор первой.
	e.sendGroup(groupText(grp, user("79280000003"), "Мансур", "Нал\n5000\n5000\n7000"))
	if got := sum(dayTotals(e, grp)); got != 27000+17000 {
		t.Errorf("список налички: сбор=%v, хочу %v", got, 27000+17000)
	}
}

// Ответ свайпом на «❓ чей платёж» записывает платёж на названного клиента.
func TestSweepPaymentQuestionAnswered(t *testing.T) {
	grp := e2eGroup("who")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("От кого наличка 27000 — чей это платёж?")
	n := len(e.sent())
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "27000 наличка"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("бот не спросил «чей платёж»: %+v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 27000 || sum(got) != 27000 {
		t.Errorf("ответ на ❓ не записал платёж: %v", got)
	}
	// Повторный ответ не записывает второй раз.
	e.sendGroup(groupReply(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := sum(dayTotals(e, grp)); got != 27000 {
		t.Errorf("повторный ответ задвоил: %v", got)
	}
}

// Ответ без свайпа следующим сообщением того же сотрудника тоже принимается,
// а чужой новый платёж — нет.
func TestSweepPaymentQuestionPlainAnswer(t *testing.T) {
	grp := e2eGroup("who2")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "Исаев") {
				return aiReply{Text: `{"payments":[{"name":"Исаев Дени","amount":5000}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[],"clarify":"Чья это наличка 27000?"}`}
		}
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		return aiReply{Text: ""}
	}
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "27000 наличка"))
	e.sendGroup(groupText(grp, user("79280000002"), "Рустам", "Исаев Дени 5000"))
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед"))
	got := dayTotals(e, grp)
	if got["Исаев Дени"] != 5000 || got["Магомедов Ахмед"] != 27000 {
		t.Errorf("ответ/новый платёж перепутаны: %v", got)
	}
}

// Без ИИ болтовня с цифрами не становится платежом, а настоящие платежи
// известным клиентам записываются; остальное ждёт, когда ИИ вернётся.
func TestSweepAIDownFallback(t *testing.T) {
	grp := e2eGroup("down")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	for _, txt := range []string{"Позвони 9287836800", "Завтра принесет 15000", "Сколько будет 5000?", "квартира 25"} {
		e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", txt))
	}
	e.waitRetryQueue(4)
	if got := dayTotals(e, grp); len(got) != 0 {
		t.Errorf("без ИИ болтовня записана платежами: %v", got)
	}
	// ИИ вернулся — отложенный платёж дочитывается.
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "15000 Магомедов Ахмед"))
	e.waitRetryQueue(5)
	e.assistant = e.fakeAI()
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "Магомедов") {
				return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":15000}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	e.retryAIText(context.Background())
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 15000 || sum(got) != 15000 {
		t.Errorf("после возврата ИИ: %v, хочу только Магомедов Ахмед=15000", got)
	}
}

// waitRetryQueue — ждёт, пока n текстов встанут в очередь повторного разбора.
func (e *e2eBot) waitRetryQueue(n int) {
	for i := 0; i < 150; i++ {
		e.aiRetryMu.Lock()
		k := len(e.aiRetry)
		e.aiRetryMu.Unlock()
		if k >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("в очереди повторного разбора нет %d сообщений", n)
}

// Без ИИ вообще: «22т», «25 тыщ» и «сумма ФИО» читаются.
func TestSweepParserShorthand(t *testing.T) {
	grp := e2eGroup("noai")
	e := newE2E(t, e2eOpts{})
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед 22т"))
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Исаев Дени 25 тыщ"))
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Позвони 9287836800"))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 22000 || got["Исаев Дени"] != 25000 || sum(got) != 47000 {
		t.Errorf("без ИИ: %v", got)
	}
}

// «за 2 месяца по 7500» — ИИ прав (15000), страховка «одно число» не мешает.
func TestSweepMultiplierKeepsAITotal(t *testing.T) {
	grp := e2eGroup("mul")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "10.000") {
				return aiReply{Text: `{"payments":[{"name":"Исаев Дени","amount":5000}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":15000}],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед\nза 2 месяца по 7500"))
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Исаев Дени\n10.000р"))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 15000 {
		t.Errorf("«по 7500 за 2 месяца»: %v, хочу 15000", got)
	}
	if got["Исаев Дени"] != 10000 {
		t.Errorf("страховка цифр сломалась: %v, хочу Исаев Дени=10000", got)
	}
}

// Удалённая наличка не делает повтором заново отправленную.
func TestSweepDeletedCashNotRepeat(t *testing.T) {
	grp, snd := e2eGroup("delcash"), user("79001110011")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":20000,"cash":true}],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	m1 := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 20000 нал")
	e.sendGroup(m1)
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(m1.Info.ID), Participant: proto.String(snd.String())}}}})
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 20000 наличными"))
	if got := sum(dayTotals(e, grp)); got != 20000 {
		t.Errorf("переотправленная наличка: сбор=%v, хочу 20000", got)
	}
}
