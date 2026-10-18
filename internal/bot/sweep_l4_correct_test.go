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

// R0a. Старый чек (дата операции 20 дней назад, прислан сегодня) и текст с
// опечаткой, присланный вместе с ним, — один платёж. Книга движка вокруг даты
// операции и вокруг присылки — одна: исключили чек — текст тоже.
func TestL4correct_R0_OldCheckExcludeTypoTwin(t *testing.T) {
	grp, emp := e2eGroup("l4r0a"), user("79001117021")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Магамедов Ахмед")
	old := time.Now().AddDate(0, 0, -20)
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(old, "5000.00", "09:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магамедов Ахмед 5000"))
	from, to := time.Now().AddDate(0, 0, -40), time.Now().AddDate(0, 0, 2)
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 5000 {
		t.Fatalf("старый чек + текст одного платежа: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов"}`)
	t.Logf("correct_operation: %s", out)
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 0 {
		t.Errorf("исключили старый чек (%s), а текст с опечаткой остался в сборе: %v", out, got)
	}
}

// R0b. То же с одинаковым ФИО: чек и текст — одна строка списка (без вопроса),
// исключаются вместе; деньги не переезжают со дня операции на сегодня.
func TestL4correct_R0_OldCheckExcludeSameNameText(t *testing.T) {
	grp, emp := e2eGroup("l4r0b"), user("79001117001")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	old := time.Now().AddDate(0, 0, -20)
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(old, "5000.00", "09:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 5000"))
	from, to := time.Now().AddDate(0, 0, -40), time.Now().AddDate(0, 0, 2)
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 5000 {
		t.Fatalf("старый чек + текст одного платежа: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов"}`)
	t.Logf("correct_operation: %s", out)
	if strings.Contains(out, "несколько") {
		t.Errorf("чек и его текст — один платёж, а инструмент спрашивает «какой?»: %s", out)
	}
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 0 {
		t.Errorf("исключили старый чек (%s), а в сборе %v", out, got)
	}
	// И с kind=check — тот же платёж целиком (на свежей паре).
	grp2 := e2eGroup("l4r0c")
	e.sendReceipt(grp2, emp, "Сафаи", sberCheckOn(old, "7000.00", "09:30:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp2, emp, "Сафаи", "Магомедов Ахмед 7000"))
	if got := monthTotals(e, []string{grp2.String()}, from, to); got != 7000 {
		t.Fatalf("старый чек 7000 + текст: %v", got)
	}
	out = e.tool("correct_operation", grp2, `{"action":"exclude","amount":7000,"person":"Магомедов","kind":"check"}`)
	if got := monthTotals(e, []string{grp2.String()}, from, to); got != 0 {
		t.Errorf("исключили старый чек (kind=check: %s), а его текст остался в сборе: %v", out, got)
	}
}

// R1. Тот же чек прислан вручную в две группы (один платёж). «Не считай его
// здесь» в «Оплата КЛНТ» не стирает платёж из «Оплата клиентов», которую
// владелец не называл: ответ говорит, что там чек остался.
func TestL4correct_R1_ManualCopyExcludeOnlyHere(t *testing.T) {
	src, dst := e2eGroup("l4r1s"), e2eGroup("l4r1d")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone},
		Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	ocr := sberCheck("13000.00", "14:00:00")
	e.sendReceipt(src, user("79001117002"), "Сафаи", ocr, "Магомедов Ахмед")
	e.sendReceipt(dst, user("79001117003"), "Ислам", ocr, "Магомедов Ахмед")
	from, to := today()
	if got := monthTotals(e, nil, from, to); got != 13000 {
		t.Fatalf("один чек в двух группах — один платёж: %v", got)
	}
	out := e.tool("correct_operation", dst, `{"action":"exclude","amount":13000,"person":"Магомедов"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, dst)); got != 0 {
		t.Errorf("«не считай здесь»: в «Оплата КЛНТ» осталось %v", got)
	}
	if got := sum(dayTotals(e, src)); got != 13000 {
		t.Errorf("«не считай его здесь» в «Оплата КЛНТ» стёрло платёж и в «Оплата клиентов»: %v (%s)", got, out)
	}
	if !strings.Contains(out, "Оплата клиентов") {
		t.Errorf("ответ не говорит, что чек остался в «Оплата клиентов»: %s", out)
	}
	// Владелец подтвердил «убери и там» — исключается и там.
	out = e.tool("correct_operation", dst, `{"action":"exclude","amount":13000,"person":"Магомедов","group":"Оплата клиентов"}`)
	if got := monthTotals(e, nil, from, to); got != 0 {
		t.Errorf("«убери и в Оплата клиентов» (%s): по всем группам %v", out, got)
	}
}

// R1b. Текст про чек в группе «Оплата КЛНТ», а сам чек — в «Оплата
// клиентов»: «исключи» в «Оплата КЛНТ» убирает только текст, чек в другой
// группе остаётся (и ответ об этом говорит).
func TestL4correct_R1_TextHereCheckElsewhere(t *testing.T) {
	src, dst := e2eGroup("l4r1t"), e2eGroup("l4r1u")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone},
		Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.sendReceipt(src, user("79001117004"), "Сафаи", sberCheck("12000.00", "15:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(dst, user("79001117005"), "Ислам", "Магомедов Ахмед 12000"))
	from, to := today()
	if got := monthTotals(e, nil, from, to); got != 12000 {
		t.Fatalf("чек + текст в другой группе — один платёж: %v", got)
	}
	out := e.tool("correct_operation", dst, `{"action":"exclude","amount":12000,"person":"Магомедов"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, dst)); got != 0 {
		t.Errorf("в «Оплата КЛНТ» осталось %v", got)
	}
	if got := sum(dayTotals(e, src)); got != 12000 {
		t.Errorf("исключение в «Оплата КЛНТ» стёрло чек в «Оплата клиентов»: %v (%s)", got, out)
	}
	if !strings.Contains(out, "Оплата клиентов") {
		t.Errorf("ответ не говорит, что чек остался в «Оплата клиентов»: %s", out)
	}
}

// R2. Чек + текст Сафаи (один платёж) и тот же текст от Сафаи ещё раз
// (второй платёж по движку). Строки «[текст] … прислал Сафаи» ничем не
// различить — «исключи лишний текст» исключает отдельный, сбор 5000, чек со
// своим текстом остаётся.
func TestL4correct_R2_SameSenderExtraText(t *testing.T) {
	grp := e2eGroup("l4r2")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf := user("79001117011")
	ck := e.sendReceipt(grp, saf, "Сафаи", sberCheck("5000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000"))
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000"))
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("предусловие: движок считает 2 платежа, сбор %v", got)
	}
	// Без уточнения — вопрос: чек (с текстом) или отдельный текст.
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов Ахмед"}`)
	t.Logf("1: %s", out)
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("«исключи» без уточнения (%s): сбор %v, хочу вопрос", out, got)
	}
	out = e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов Ахмед","kind":"text","sender":"Сафаи"}`)
	t.Logf("2: %s", out)
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Errorf("«исключи лишний текст» (%s): сбор %v, хочу 5000", out, got)
	}
	if !l4correctCheckCounted(e, ck) {
		t.Errorf("исключили лишний текст, а чек выпал из сбора: %s", out)
	}
	if !strings.Contains(out, "вместе с чеком") {
		t.Errorf("ответ не говорит, что такой же текст остался вместе с чеком: %s", out)
	}
}

// R2b. Два одинаковых текста от одного сотрудника без чека — два платежа.
// «Исключи лишний» исключает один (а не задаёт вопрос без ответа) и говорит,
// что такой же остался.
func TestL4correct_R2_TwoIdenticalTextsExcludeOne(t *testing.T) {
	grp := e2eGroup("l4r2b")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf := user("79001117012")
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 4000"))
	e.sendGroup(groupText(grp, saf, "Сафаи", "Магомедов Ахмед 4000"))
	if got := sum(dayTotals(e, grp)); got != 8000 {
		t.Skipf("два одинаковых текста не посчитаны двумя платежами: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":4000,"person":"Магомедов Ахмед"}`)
	t.Logf("correct_operation: %s", out)
	if got := sum(dayTotals(e, grp)); got != 4000 {
		t.Errorf("«исключи лишний» среди одинаковых текстов (%s): сбор %v, хочу 4000", out, got)
	}
	if !strings.Contains(out, "ещё 1") {
		t.Errorf("ответ не говорит, что такой же текст остался: %s", out)
	}
	// Тексты разных сотрудников различимы — там по-прежнему вопрос.
	grp2 := e2eGroup("l4r2c")
	e.sendGroup(groupText(grp2, saf, "Сафаи", "Магомедов Ахмед 3000"))
	e.sendGroup(groupText(grp2, user("79001117013"), "Ислам", "Магомедов Ахмед 3000"))
	if got := sum(dayTotals(e, grp2)); got != 6000 {
		t.Skipf("тексты разных сотрудников: %v", got)
	}
	out = e.tool("correct_operation", grp2, `{"action":"exclude","amount":3000,"person":"Магомедов Ахмед"}`)
	if got := sum(dayTotals(e, grp2)); got != 6000 || !strings.Contains(out, "Ислам") {
		t.Errorf("тексты разных сотрудников (%s): сбор %v — хочу вопрос «чей?»", out, got)
	}
}
