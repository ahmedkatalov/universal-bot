package bot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Регрессии: ручные корректировки (correct_operation, «не 5000 а 6000»)
// решают, какой текст — тот же платёж, что и чек, так же, как движок подсчёта.

// l4correctSent — тексты ответов бота с n-го.
func l4correctSent(e *e2eBot, n int) string {
	var out []string
	for _, m := range e.sentSince(n) {
		out = append(out, m.Text)
	}
	return strings.Join(out, " | ")
}

// l4correctCheckCounted — считает ли движок чек сообщения waID в сборе за сегодня.
func l4correctCheckCounted(e *e2eBot, waID string) bool {
	ctx := context.Background()
	id, ok, err := e.db.ReceiptIDByMessage(ctx, waID)
	if err != nil || !ok {
		e.t.Fatalf("чек %s не найден: %v", waID, err)
	}
	n := time.Now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	book, err := e.db.Book(ctx, from, from.AddDate(0, 0, 1), "")
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range book.Payments {
		for _, c := range p.Checks {
			if c.ID == id {
				return true
			}
		}
	}
	return false
}

// 3. Чек «Магомедов Ахмед» и текст с опечаткой «Магамедов Ахмед» (другой
// контакт) — движок считает их одним платежом. Исключили чек — текст тоже.
func TestL4correct_3_ExcludeCheckTypoTextTwin(t *testing.T) {
	grp, emp := e2eGroup("l4ct"), user("79001115531")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Магамедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "09:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Fatalf("чек + текст одного платежа: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов"}`)
	t.Logf("correct_operation: %s", out)
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Errorf("исключили чек (%s), а в сборе осталось %v: %v", out, sum(got), got)
	}
}

// 3b. То же, но владелец назвал клиента как в тексте с опечаткой: нашёлся
// только текст, а он — тот же платёж, что чек. Исключается весь платёж.
func TestL4correct_3_ExcludeByTypoTextNameDropsPayment(t *testing.T) {
	grp, emp := e2eGroup("l4cn"), user("79001115532")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Магамедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "09:05:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Fatalf("чек + текст одного платежа: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магамедов"}`)
	t.Logf("correct_operation: %s", out)
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Errorf("исключили платёж (%s), а в сборе осталось %v: %v", out, sum(got), got)
	}
}

// 3c. Поправка суммы чека («не 5000 а 6000» свайпом на чек) двигает и его
// текст с опечаткой: иначе чек 6000 и текст 5000 посчитаются двумя платежами.
func TestL4correct_3_AmountCorrectionMovesTypoTwin(t *testing.T) {
	grp, emp := e2eGroup("l4ca"), user("79001115533")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Магамедов Ахмед")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "09:10:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Fatalf("чек + текст одного платежа: %v", got)
	}
	n := len(e.sent())
	e.sendGroup(groupReply(grp, emp, "Сафаи", "не 5000 а 6000", ck, emp, "чек"))
	if got := dayTotals(e, grp); sum(got) != 6000 {
		t.Errorf("свайп «не 5000 а 6000» на чек: сбор %v (%v), хочу 6000; бот: %s", sum(got), got, l4correctSent(e, n))
	}
	// Без свайпа: чек и его текст — один платёж, а не «несколько платежей».
	n = len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "не 6000 а 7000"))
	if got := dayTotals(e, grp); sum(got) != 7000 {
		t.Errorf("«не 6000 а 7000»: сбор %v (%v), хочу 7000; бот: %s", sum(got), got, l4correctSent(e, n))
	}
}

// 10. Текст Сафаи + чек (один платёж) и такой же текст Ислама (второй платёж
// по движку). «Исключи» без уточнения не стирает весь платёж, а спрашивает,
// какой; «исключи текст Ислама» исключает только его.
func TestL4correct_10_ExcludeExtraKeepsPayment(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("l4cx")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf, isl := user("79001115534"), user("79001115535")
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000"))
	ck := e.sendReceipt(grp, saf, "Сафаи", sberCheck("5000.00", "10:00:00"), "")
	e.sendGroup(groupText(grp, isl, "Ислам", "Магомедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("предусловие: движок считает 2 платежа, сбор %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов Ахмед"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("«исключи» без уточнения (%s): сбор %v — платёж стёрт целиком, хочу вопрос «какой?»", out, got)
	}
	if !strings.Contains(out, "Ислам") || !strings.Contains(out, "[чек]") || !strings.Contains(out, "[текст]") {
		t.Errorf("в списке нет отдельного текста Ислама и чека: %s", out)
	}
	out = e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов Ахмед","kind":"text","sender":"Ислама"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Errorf("«исключи текст Ислама» (%s): сбор %v, хочу 5000", out, got)
	}
	if !l4correctCheckCounted(e, ck) {
		t.Errorf("исключили текст Ислама, а чек выпал из сбора: %s", out)
	}
	n := time.Now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	st, err := e.db.SenderStats(ctx, from, from.AddDate(0, 0, 1), []string{grp.String()}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if strings.Contains(s.Name, "Ислам") {
			t.Errorf("исключённый текст Ислама всё ещё в сборе по сотрудникам: %+v", st)
		}
	}
}

// 10b. Поправка суммы чека («не 5000 а 6000» свайпом) меняет только его
// текст-пару, а отдельный такой же текст Ислама (другой платёж) не трогает.
func TestL4correct_10_AmountCorrectionOnlyPairedText(t *testing.T) {
	grp := e2eGroup("l4cy")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf, isl := user("79001115536"), user("79001115537")
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000"))
	ck := e.sendReceipt(grp, saf, "Сафаи", sberCheck("5000.00", "10:30:00"), "")
	e.sendGroup(groupText(grp, isl, "Ислам", "Магомедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("предусловие: движок считает 2 платежа, сбор %v", got)
	}
	n := len(e.sent())
	e.sendGroup(groupReply(grp, saf, "Сафаи", "не 5000 а 6000", ck, saf, "чек"))
	if got := sum(dayTotals(e, grp)); got != 11000 {
		t.Errorf("свайп «не 5000 а 6000» на чек: сбор %v, хочу 11000 (6000 чек с текстом Сафаи + 5000 текст Ислама); бот: %s",
			got, l4correctSent(e, n))
	}
}

// 10c. «Исключи текст Сафаи» (текст-пара чека): исключается только этот
// текст, чек остаётся в сборе (тот же платёж), отдельный текст Ислама — тоже.
func TestL4correct_10_ExcludeNamedTwinTextKeepsCheck(t *testing.T) {
	grp := e2eGroup("l4cz")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf, isl := user("79001115538"), user("79001115539")
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000"))
	ck := e.sendReceipt(grp, saf, "Сафаи", sberCheck("5000.00", "11:00:00"), "")
	e.sendGroup(groupText(grp, isl, "Ислам", "Магомедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("предусловие: движок считает 2 платежа, сбор %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов","kind":"text","sender":"Сафаи"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Errorf("«исключи текст Сафаи» (%s): сбор %v, хочу 5000", out, got)
	}
	if !l4correctCheckCounted(e, ck) {
		t.Errorf("исключили текст Сафаи, а чек выпал из сбора: %s", out)
	}
}
