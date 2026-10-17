package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
)

// Регрессии сверки с программой: клиента назвали ответом на «чей чек?»,
// напоминание судит как сверка, «Каталова внесли», исключённый чек,
// поправка суммы, внесение в программу без дублей и с датой чека.

func sberCheckOn(day time.Time, amount, hms string) string {
	return "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " " + hms +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n" + amount + " ₽\nНомер операции в СБП\n" + e2eID("OP")
}

var katalov = []cmf.ClientInfo{{ID: "1", FullName: "Каталов Ахмед Нажудович"}}

func (e *e2eBot) remindNow() []e2eSent {
	ctx := context.Background()
	if err := e.db.TestAgeCmfWatches(ctx, 30*time.Hour); err != nil {
		e.t.Fatal(err)
	}
	n := len(e.sent())
	e.cmfCheckDue(ctx)
	e.waitIdle()
	return e.sentSince(n)
}

func hasReminder(ms []e2eSent) bool {
	for _, m := range ms {
		if strings.Contains(m.Text, "⏰") {
			return true
		}
	}
	return false
}

func TestSweepCmfClientNamedByAnswer(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("cmf1"), user("79281110001")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей чек»: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Каталов Ахмед", q.ID, e.botJID(), q.Text))
	time.Sleep(300 * time.Millisecond)
	if st, cid, _ := e.db.TestCmfWatchState(ctx, ck); cid != "1" {
		t.Errorf("сверка не узнала клиента после ответа: status=%s client=%q", st, cid)
	}
	if !hasReminder(e.remindNow()) {
		t.Error("невнесённый чек не напомнили")
	}
}

func TestSweepCmfReminderLikeReconcile(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("cmf2"), user("79281110002")
	d1, d2 := time.Now().AddDate(0, 0, -5), time.Now().AddDate(0, 0, -3)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov,
		CMFPays: map[string][]map[string]any{"1": {{"amount": 15000, "paid_at": d1.Format("2006-01-02")}}}})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(d1, "15000.00", "11:00:00"), "Каталов Ахмед")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(d2, "15000.00", "16:30:00"), "Каталов Ахмед")
	got := e.remindNow()
	if !hasReminder(got) || !strings.Contains(got[len(got)-1].Text, d2.Format("02.01")) {
		t.Errorf("одна оплата закрыла два чека — второй не напомнили: %+v", got)
	}
	if st, _, _ := e.db.TestCmfWatchState(ctx, ck2); st != "reminded" {
		t.Errorf("второй чек: status=%s", st)
	}
}

func TestSweepCmfEnteredReplyKeepsClient(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("cmf3"), user("79281110003")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
	rem := e.remindNow()
	if !hasReminder(rem) {
		t.Fatalf("нет напоминания: %+v", rem)
	}
	r := rem[len(rem)-1]
	for _, reply := range []string{"Каталова внесли", "Внесли Каталову", "занесла"} {
		n := len(e.sent())
		e.sendGroup(groupReply(grp, emp, "Сафаи", reply, r.ID, e.botJID(), r.Text))
		for _, m := range e.sentSince(n) {
			if strings.Contains(m.Text, "Платёж за него") || strings.Contains(m.Text, "такого клиента") {
				t.Errorf("«%s» понят как новое имя: %q", reply, m.Text)
			}
		}
		if _, cid, _ := e.db.TestCmfWatchState(ctx, ck); cid != "1" {
			t.Errorf("«%s» сбросил клиента", reply)
		}
	}
}

func TestSweepCmfExcludedCheckNotReminded(t *testing.T) {
	grp, emp := e2eGroup("cmf4"), user("79281110004")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":14000,"person":"Каталов"}`)
	if !strings.Contains(out, "исключена") {
		t.Fatalf("исключение: %s", out)
	}
	if got := e.remindNow(); hasReminder(got) {
		t.Errorf("исключённый чек напомнили: %+v", got)
	}
}

func TestSweepCmfAmountCorrectionReachesWatch(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("cmf5"), user("79281110005")
	day := time.Now().AddDate(0, 0, -1)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov,
		CMFPays: map[string][]map[string]any{"1": {{"amount": 14000, "paid_at": day.Format("2006-01-02")}}}})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "1400.00", "10:00:00"), "Каталов Ахмед")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "не 1400 а 14000", ck, emp, "чек"))
	if _, _, amt := e.db.TestCmfWatchState(ctx, ck); amt != 14000 {
		t.Errorf("сверка ждёт старую сумму: %v", amt)
	}
	if got := e.remindNow(); hasReminder(got) {
		t.Errorf("ложное «не отмечен» после поправки: %+v", got)
	}
}

func TestSweepCmfAddPaymentNoDuplicateAndCheckDate(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("cmf6"), user("79281110006")
	day := time.Now().AddDate(0, 0, -7)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: append(katalov, cmf.ClientInfo{ID: "5", FullName: "Саралиева Милана Руслановна"}),
		CMFPays: map[string][]map[string]any{"5": {{"amount": 25000, "paid_at": time.Now().AddDate(0, 0, -1).Format("2006-01-02")}}}})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	// Уже внесено вручную — второй раз не пишем.
	out := e.tool("cmf_add_payment", grp, `{"client_name":"Саралиева Милана Руслановна","amount":25000,"date":"`+time.Now().AddDate(0, 0, -1).Format("2006-01-02")+`"}`)
	if !strings.Contains(out, "ПОХОЖЕ НА ПОВТОР") || e.posts.n() != 0 {
		t.Errorf("дубль в программу: %s (POST %d)", out, e.posts.n())
	}
	// «Внеси чек Каталова» без даты — дата с чека, повторная просьба не пишет.
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "14000.00", "12:00:00"), "Каталов Ахмед")
	out = e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":14000}`)
	if !strings.Contains(out, day.Format("02.01.2006")) || e.posts.n() != 1 {
		t.Errorf("дата не с чека: %s (POST %d)", out, e.posts.n())
	}
	out = e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":14000}`)
	if !strings.Contains(out, "ПОХОЖЕ НА ПОВТОР") || e.posts.n() != 1 {
		t.Errorf("повторная просьба записала второй раз: %s (POST %d)", out, e.posts.n())
	}
	if st, _, _ := e.db.TestCmfWatchState(ctx, ck); st != "found" {
		t.Errorf("после внесения чек всё ещё ждёт: %s", st)
	}
	if got := e.remindNow(); hasReminder(got) {
		t.Errorf("напомнили про только что внесённый: %+v", got)
	}
}
