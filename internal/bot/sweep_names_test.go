package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии общей проверки бота: «ФИО + сумма», очередь имён, удаления.

// dayTotals — сбор за сегодня по группе: клиент → сумма.
func dayTotals(e *e2eBot, g types.JID) map[string]float64 {
	n := time.Now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
	ss, err := e.db.SummaryForPeriod(context.Background(), from, from.AddDate(0, 0, 1), []string{g.String()})
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]float64{}
	for _, x := range ss {
		out[x.CanonicalName] += x.Total
	}
	return out
}

func sum(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

// sberCheck — распознанный текст чека Сбера на сумму и время (уникальный номер операции).
func sberCheck(amount string, hms string) string {
	return "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " " + hms +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n" + amount + " ₽\nНомер операции в СБП\n" + e2eID("OP")
}

// aiNamesAndPayments — фейковый ИИ, как настоящий: выделяет имя из подписи и
// читает платежи из текста (ФИО + сумма, наличка, «у X»).
func aiNamesAndPayments(names ...string) func(c aiCall) aiReply {
	return func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			for _, n := range names {
				if strings.Contains(strings.ToLower(u), strings.ToLower(strings.Fields(n)[0])) {
					return aiReply{Text: `{"client":"` + n + `"}`}
				}
			}
			return aiReply{Text: `{"client":""}`}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			var ps []string
			for _, ln := range strings.Split(u, "\n") {
				for _, n := range names {
					if strings.Contains(ln, strings.Fields(n)[0]) {
						var amt float64
						for _, f := range strings.Fields(ln) {
							if v := parseAmountWord(f); v > 0 {
								amt = v
							}
						}
						if amt > 0 {
							cash := strings.Contains(ln, "нал") || strings.Contains(ln, " у ")
							ps = append(ps, fmt.Sprintf(`{"name":%q,"amount":%v,"cash":%v}`, n, amt, cash))
						}
					}
				}
			}
			return aiReply{Text: `{"payments":[` + strings.Join(ps, ",") + `],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
}

func parseAmountWord(f string) float64 {
	f = strings.Trim(f, ".,₽р")
	var v float64
	if _, err := fmt.Sscanf(f, "%f", &v); err == nil && v >= 100 {
		return v
	}
	return 0
}

func TestSweepNamePlusAmountIsPayment(t *testing.T) {
	for _, withAI := range []bool{true, false} {
		e := newE2E(t, e2eOpts{WithAI: withAI})
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Дени")
		e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
		grp, snd := e2eGroup("np"), user("79280000001")
		// (a) «ФИО + сумма» без чека — это платёж.
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 15000"))
		if got := sum(dayTotals(e, grp)); got != 15000 {
			t.Fatalf("AI=%v (a): сбор %v, ожидали 15000 (%v)", withAI, got, dayTotals(e, grp))
		}
		// (b) следующий чек ДРУГОГО клиента без подписи не получает это имя.
		ck := e.sendReceipt(grp, snd, "Сафаи", sberCheck("30000.00", "11:30:00"), "")
		if r := e.db.TestReceiptByMessage(context.Background(), ck); r.Recipient == "Магомедов Ахмед" {
			t.Errorf("AI=%v (b): чек на 30000 отдан «Магомедов Ахмед»", withAI)
		}
		// (c) чек на ТУ ЖЕ сумму получает имя, а текст+чек считаются ОДИН раз.
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 7000"))
		ck2 := e.sendReceipt(grp, snd, "Сафаи", sberCheck("7000.00", "11:40:00"), "")
		if r := e.db.TestReceiptByMessage(context.Background(), ck2); r.Recipient != "Магомедов Ахмед" {
			t.Errorf("AI=%v (c): чек на 7000 не получил имя: %+v", withAI, r)
		}
		if got := dayTotals(e, grp)["Магомедов Ахмед"]; got != 22000 {
			t.Errorf("AI=%v (c): Магомедов %v, ожидали 22000 (%v)", withAI, got, dayTotals(e, grp))
		}
	}
}

func TestSweepCashLineIsPayment(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Дени")
	grp, snd := e2eGroup("cash"), user("79280000002")
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед наличка 15000"))
	if got := sum(dayTotals(e, grp)); got != 15000 {
		t.Fatalf("наличка «ФИО нал сумма» потеряна: %v", dayTotals(e, grp))
	}
	// Чек без подписи, затем список «чек + наличка»: чек подписан строкой с его суммой, наличка записана.
	e2 := newE2E(t, e2eOpts{WithAI: true})
	e2.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Дени")
	g2 := e2eGroup("list")
	ck := e2.sendReceipt(g2, snd, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e2.sendGroup(groupText(g2, snd, "Сафаи", "Магомедов Ахмед 15000\nИсаев Дени 20000 нал"))
	if r := e2.db.TestReceiptByMessage(context.Background(), ck); r.Recipient != "Магомедов Ахмед" || r.NeedsReview {
		t.Errorf("чек не подписан строкой списка: %+v", r)
	}
	if got := sum(dayTotals(e2, g2)); got != 35000 {
		t.Errorf("список «чек + наличка»: сбор %v, ожидали 35000 (%v)", got, dayTotals(e2, g2))
	}
}

func TestSweepQueuedNameUsedByCaption(t *testing.T) {
	// 10/22. Имя перед чеком, у чека та же подпись — имя использовано, следующему чеку не достаётся.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, snd := e2eGroup("cap"), user("79280000003")
	e.sendGroup(groupText(grp, snd, "Ислам", "Магомедов Ахмед"))
	e.sendReceipt(grp, snd, "Ислам", sberCheck("15000.00", "19:00:01"), "Магомедов Ахмед")
	id2 := e.sendReceipt(grp, snd, "Ислам", sberCheck("6000.00", "19:04:01"), "")
	if r := e.db.TestReceiptByMessage(context.Background(), id2); r.Recipient == "Магомедов Ахмед" {
		t.Errorf("чужой чек на 6000 записан на «Магомедов Ахмед»")
	}
}

func TestSweepCheckRepliedToName(t *testing.T) {
	// 11. Чек ответом (свайпом) на сообщение с именем — клиент из цитаты.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Саралиева Милана", "Магомедов Ахмед")
	ctx := context.Background()
	grp, snd := e2eGroup("rep"), user("79280000004")
	e.sendGroup(groupText(grp, snd, "Ислам", "Магомедов Ахмед"))
	m2 := groupText(grp, snd, "Ислам", "Саралиева Милана")
	e.sendGroup(m2)
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true}, ID: e2eID("CHK"), Timestamp: time.Now()},
		Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{ContextInfo: &waProto.ContextInfo{StanzaID: proto.String(m2.Info.ID),
			Participant: proto.String(snd.String()), QuotedMessage: &waProto.Message{Conversation: proto.String("Саралиева Милана")}}}}}
	if payer, _ := e.resolveReceiptPayer(ctx, msg, ""); payer != "Саралиева Милана" {
		t.Errorf("чек-ответ на «Саралиева Милана» отдан %q", payer)
	}
	// Документ (PDF) ответом — тоже.
	doc := &events.Message{Info: msg.Info, Message: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{ContextInfo: msg.Message.GetImageMessage().GetContextInfo()}}}
	if extractQuotedText(doc) != "Саралиева Милана" {
		t.Error("цитата у PDF-ответа не читается")
	}
}

func TestSweepDeletedNamesAndChecks(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Хасанов Ислам", "Хасанов Ислам")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	grp, a := e2eGroup("del"), user("79280000005")
	// (a) Удалённый чек не забирает имя у живого.
	cX := e.sendReceipt(grp, a, "Сафаи", sberCheck("15000.00", "09:00:00"), "")
	_, _, _ = e.db.MarkMessageDeleted(ctx, cX)
	cY := e.sendReceipt(grp, a, "Сафаи", sberCheck("15500.00", "09:05:00"), "")
	e.sendGroup(groupText(grp, a, "Сафаи", "Хасанов Ислам"))
	if r := e.db.TestReceiptByMessage(ctx, cY); r.Recipient != "Хасанов Ислам" || r.NeedsReview {
		t.Errorf("(a) живой чек не получил имя: %+v", r)
	}
	// (b) Удалённое имя не подписывает следующий чек.
	g2 := e2eGroup("del2")
	m := groupText(g2, a, "Сафаи", "Магомедов Ахмед")
	e.sendGroup(m)
	_, _, _ = e.db.MarkMessageDeleted(ctx, m.Info.ID)
	ck := e.sendReceipt(g2, a, "Сафаи", sberCheck("6500.00", "09:10:00"), "")
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Магомедов Ахмед" {
		t.Errorf("(b) удалённое имя подписало чек")
	}
	// (c) Имя после чека, затем имя удалили — чек снова ждёт клиента.
	g3 := e2eGroup("del3")
	ck3 := e.sendReceipt(g3, a, "Сафаи", sberCheck("6600.00", "09:20:00"), "")
	m3 := groupText(g3, a, "Сафаи", "Магомедов Ахмед")
	e.sendGroup(m3)
	if r := e.db.TestReceiptByMessage(ctx, ck3); r.Recipient != "Магомедов Ахмед" {
		t.Fatalf("(c) имя не подписало чек: %+v", r)
	}
	_, _, _ = e.db.MarkMessageDeleted(ctx, m3.Info.ID)
	if r := e.db.TestReceiptByMessage(ctx, ck3); !r.NeedsReview || r.Confirmed {
		t.Errorf("(c) после удаления имени чек всё ещё засчитан: %+v", r)
	}
}

func TestSweepRecountNoDouble(t *testing.T) {
	// 49. «ФИО + сумма» перед чеком: пересчёт не добавляет второй платёж.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, snd := e2eGroup("rc"), user("79280000006")
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendReceipt(grp, snd, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
	before := dayTotals(e, grp)
	if _, err := e.recountEverything(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := dayTotals(e, grp)
	if sum(before) != 15000 || sum(after) != 15000 {
		t.Errorf("до %v, после пересчёта %v — ожидали 15000", before, after)
	}
}
