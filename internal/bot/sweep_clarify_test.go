package bot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Регрессии: вопросы бота («у кого наличка?», «новый или тот же?», «чей чек?»)
// не перехватывают чужие сообщения и новые платежи.

func (e *e2eBot) age(grp string) {
	if err := e.db.TestAgeGroup(context.Background(), grp, 3*time.Minute); err != nil {
		e.t.Fatal(err)
	}
}

// lastQuestion — последнее сообщение бота, содержащее подстроку.
func (e *e2eBot) lastWith(sub string) e2eSent {
	out := e.sent()
	for k := len(out) - 1; k >= 0; k-- {
		if strings.Contains(out[k].Text, sub) {
			return out[k]
		}
	}
	return e2eSent{}
}

func TestSweepCollectorQuestionNotHijacked(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Ислам", "Магомед", "Дени")
	grp := e2eGroup("col")
	e.groupsCache[grp] = "Оплата"
	B, A, owner := user("79001110002"), user("79001110001"), user("79990001111")
	e.sendGroup(groupText(grp, B, "Мансур", "Ислам 27000 наличка"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("У кого она")
	if q.ID == "" {
		t.Fatalf("бот не спросил «у кого наличка»: %+v", e.sent())
	}
	// Новый платёж другого сотрудника — не ответ.
	e.sendGroup(groupText(grp, A, "Сафаи", "Магомед 5000"))
	if got := dayTotals(e, grp); got["Магомед"] != 5000 {
		t.Errorf("новый платёж съеден вопросом: %v", got)
	}
	// Ответ владельца свайпом, потом болтовня — сборщик не меняется.
	e.sendGroup(groupReply(grp, owner, "Ахмед", "у Дени", q.ID, e.botJID(), q.Text))
	n := len(e.sent())
	e.sendGroup(groupText(grp, B, "Мансур", "Ок понял"))
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "Ок понял") {
			t.Errorf("«Ок понял» записан как сборщик: %q", m.Text)
		}
	}
}

func TestSweepCashDupQuestionNotHijacked(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Ислам", "Усманов")
	grp := e2eGroup("dup")
	e.groupsCache[grp] = "Оплата"
	B, A, owner := user("79001110002"), user("79001110001"), user("79990001111")
	e.sendGroup(groupText(grp, B, "Мансур", "Ислам 27000\nналичка у Дени"))
	e.sendGroup(groupText(grp, B, "Мансур", "Ислам 27000\nналичка у Дени"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("НОВЫЙ платёж")
	if q.ID == "" {
		t.Fatalf("бот не спросил про повтор: %+v", e.sent())
	}
	e.sendGroup(groupText(grp, A, "Сафаи", "Усманов 5000"))
	if got := dayTotals(e, grp); got["Усманов"] != 5000 || got["Ислам"] != 27000 {
		t.Errorf("«Усманов 5000» решило повтор: %v", got)
	}
	e.sendGroup(groupReply(grp, owner, "Ахмед", "новый", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Ислам"] != 54000 {
		t.Fatalf("«новый» не засчитан: %v", got)
	}
	e.sendGroup(groupText(grp, owner, "Ахмед", "нет"))
	if got := dayTotals(e, grp); got["Ислам"] != 54000 {
		t.Errorf("случайное «нет» сняло подтверждённую наличку: %v", got)
	}
	if r, n := parseCashDupAnswer("Усманов"); r || n {
		t.Error("«Усманов» прочитан как «новый»")
	}
}

func TestSweepReceiptAnswerGoesToRightCheck(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Исаев Харон", "Гайрбеков Дени")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	e.aliases.Add("Гайрбеков Дени", "Гайрбеков Дени")
	grp := e2eGroup("rcp")
	e.groupsCache[grp] = "Оплата"
	a, b := user("79001110030"), user("79001110031")
	// 50. Имя от ДРУГОГО сотрудника перед его чеком — не ответ про чужой чек.
	ckA := e.sendReceipt(grp, a, "Сафаи", sberCheck("5000.00", "10:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	if !strings.Contains(e.last().Text, "Чей это чек") {
		t.Fatalf("нет вопроса «чей чек»: %q", e.last().Text)
	}
	e.sendGroup(groupText(grp, b, "Мансур", "Исаев Харон"))
	ckB := e.sendReceipt(grp, b, "Мансур", sberCheck("9000.00", "10:02:00"), "")
	if r := e.db.TestReceiptByMessage(ctx, ckA); r.Recipient == "Исаев Харон" {
		t.Errorf("имя сотрудника B ушло на чек A")
	}
	if r := e.db.TestReceiptByMessage(ctx, ckB); r.Recipient != "Исаев Харон" {
		t.Errorf("чек B не получил имя: %+v", r)
	}
	// 19. Свайп с именем на ДРУГОЙ чек — к нему, а не к открытому вопросу.
	g2 := e2eGroup("rcp2")
	ckX := e.sendReceipt(g2, a, "Сафаи", sberCheck("15000.00", "11:00:00"), "")
	ckZ := e.sendReceipt(g2, a, "Сафаи", sberCheck("8000.00", "11:01:00"), "")
	e.sendGroup(groupReply(g2, user("79990001111"), "Ахмед", "Гайрбеков Дени", ckZ, a, "чек"))
	if r := e.db.TestReceiptByMessage(ctx, ckZ); r.Recipient != "Гайрбеков Дени" {
		t.Errorf("свайп на чек Z не применён к Z: %+v", r)
	}
	if r := e.db.TestReceiptByMessage(ctx, ckX); r.Recipient == "Гайрбеков Дени" {
		t.Errorf("свайп на Z ушёл на X")
	}
}

func TestSweepContextClientNeedsRealName(t *testing.T) {
	// 21. Приветствие перед чеком — не клиент; бот спрашивает.
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments() // ИИ не признаёт приветствие именем
	grp := e2eGroup("greet")
	e.groupsCache[grp] = "Оплата"
	a := user("79001110040")
	e.sendGroup(groupText(grp, a, "Сафаи", "Ассаламу алейкум брат"))
	ck := e.sendReceipt(grp, a, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Ассаламу алейкум брат" || r.Confirmed {
		t.Errorf("приветствие стало клиентом: %+v", r)
	}
}
