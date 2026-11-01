package bot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Регрессии l6 (повторы чеков): вопрос «Чей он?» про тот же чек в другой
// группе, экран перевода перед его полным чеком.

// Находка 06: ответ на «Чей он?» переносит чек другой группы — и текст «ФИО
// сумма», который там шёл с этим чеком: один перевод остаётся одним платежом.
func TestL6dedupe_06_CrossAnswerMovesOtherGroupText(t *testing.T) {
	sb, mg := e2eGroup("l6ta"), e2eGroup("l6tb")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	a, emp := user("79001119128"), user("79001119129")
	e.sendGroup(groupText(sb, a, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendReceipt(sb, a, "Сафаи", ocr, "")
	if got := sum(dayTotals(e, sb)); got != 15000 {
		t.Fatalf("до вопроса: сбор СБ %v", dayTotals(e, sb))
	}
	n := len(e.sent())
	e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	q := e.lastWith("Чей он")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sentSince(n))
	}
	e.sendGroup(groupReply(mg, emp, "Ислам", "Исаев Харон", q.ID, e.botJID(), q.Text))
	from, to := l4Today()
	s, m := dayTotals(e, sb), dayTotals(e, mg)
	all := l4Total(e, nil, from, to)
	if sum(s) != 15000 || sum(m) != 15000 || all != 15000 {
		t.Errorf("один перевод 15000 (текст+чек в СБ, тот же чек в основной), ответ «Исаев Харон»: сбор СБ %v, основная %v, все группы %v; бот: %q", s, m, all, e.last().Text)
	}
}

// Находка 07: экран «Перевод выполнен» прислан раньше полного чека того же
// перевода — в учёте остаётся полный чек (номер операции, напечатанное время).
func TestL6dedupe_07_ScreenThenFullCheckKeepsFull(t *testing.T) {
	screen := "СберБанк\nПеревод выполнен\nХадижат Ибрагимовна И.\n5 000 ₽\nСохранить чек"
	fullCheck := func(op time.Time, doc string) string {
		return "СберБанк\nЧек по операции\nДата операции " + op.Format("02.01.2006 15:04:05") +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер операции в СБП\n" + doc
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	t.Run("две группы", func(t *testing.T) {
		at := today.AddDate(0, 0, -1).Add(12 * time.Hour)
		op := at.Add(-4 * time.Minute)
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		sb, mg := e2eGroup("l6ka"), e2eGroup("l6kb")
		full := fullCheck(op, "A6290101010101010G10040011791155")
		l5collapsedPhotoAt(e, sb, user("79001115941"), screen, "Исаев Харон", at)
		l5collapsedPhotoAt(e, sb, user("79001115941"), full, "Исаев Харон", at.Add(30*time.Second))
		l5collapsedPhotoAt(e, mg, user("79001115942"), full, "Исаев Харон", at.Add(time.Minute))
		day := today.AddDate(0, 0, -1)
		all := l4Total(e, nil, day, today)
		s := l4Total(e, []string{sb.String()}, day, today)
		m := l4Total(e, []string{mg.String()}, day, today)
		if all != 5000 || s != 5000 || m != 5000 {
			t.Errorf("один перевод 5000 (экран+чек в СБ, тот же чек в основной): все группы %v, СБ %v, основная %v", all, s, m)
		}
	})
	t.Run("после полуночи", func(t *testing.T) {
		d0 := today.AddDate(0, 0, -1)
		op := d0.Add(-90 * time.Second)
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup("l6mid"), user("79001115951")
		l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", d0.Add(90*time.Second))
		l5collapsedPhotoAt(e, grp, emp, fullCheck(op, "A6290101010101010G10040011791166"), "Исаев Харон", d0.Add(150*time.Second))
		prev := l4Total(e, []string{grp.String()}, d0.AddDate(0, 0, -1), d0)
		next := l4Total(e, []string{grp.String()}, d0, d0.AddDate(0, 0, 1))
		if prev != 5000 || next != 0 {
			t.Errorf("перевод %s (напечатано на чеке), экран прислан %s: сбор за %s = %v, за %s = %v; ожидали 5000 в день операции; бот: %q",
				op.Format("02.01 15:04:05"), d0.Add(90*time.Second).Format("02.01 15:04"), op.Format("02.01"), prev, d0.Format("02.01"), next, e.last().Text)
		}
	})
}

// Находка 08: подписанный экран успеха, а за ним полный чек того же перевода,
// сделанного больше чем за 5 минут до экрана, — один платёж или вопрос «другой?».
// Экран, присланный РАНЬШЕ операции полного чека, — другой перевод.
func TestL6dedupe_08_ScreenThenEarlierFullCheck(t *testing.T) {
	now := time.Now()
	at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -1)
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.Local)
	screen := "СберБанк\nПеревод выполнен\nХадижат Ибрагимовна И.\n5 000 ₽\nСохранить чек"
	full := func(op time.Time, doc string) string {
		return "СберБанк\nЧек по операции\nДата операции " + op.Format("02.01.2006 15:04:05") +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер операции в СБП\n" + doc
	}
	t.Run("операция раньше экрана", func(t *testing.T) {
		op := at.Add(-40 * time.Minute)
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup("l6scr"), user("79001115931")
		l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", at)
		n := len(e.sent())
		l5collapsedPhotoAt(e, grp, emp, full(op, "A6290101010101010G10040011791199"), "Исаев Харон", at.Add(time.Minute))
		if got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1)); got != 5000 {
			asked := false
			for _, m := range e.sentSince(n) {
				asked = asked || strings.Contains(m.Text, "другой")
			}
			t.Errorf("экран (прислан %s) + полный чек того же перевода (операция %s), оба «Исаев Харон»: сбор %v, ожидали 5000; спросил «другой?» = %v, бот: %v",
				at.Format("15:04"), op.Format("15:04:05"), got, asked, e.sentSince(n))
		}
		// Номера у экрана нет — не наверняка: ответ «другой» возвращает экран
		// отдельным платежом.
		q := e.lastWith("ДРУГОЙ")
		if q.ID == "" {
			t.Fatalf("бот не спросил, тот же ли это перевод: %v", e.sentSince(n))
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
		if got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1)); got != 10000 {
			t.Errorf("после ответа «другой» экран не засчитан отдельно: сбор %v (бот: %q)", got, e.last().Text)
		}
	})
	t.Run("экран раньше операции — другой перевод", func(t *testing.T) {
		op := at.Add(40 * time.Minute)
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup("l6scr2"), user("79001115932")
		l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", at)
		l5collapsedPhotoAt(e, grp, emp, full(op, "A6290101010101010G10040011791198"), "Исаев Харон", op.Add(time.Minute))
		if got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1)); got != 10000 {
			t.Errorf("экран прислан в %s, полный чек — операция %s (позже экрана): два перевода, сбор %v, ожидали 10000 (бот: %q)",
				at.Format("15:04"), op.Format("15:04"), got, e.last().Text)
		}
	})
}

// Находка 09: ФИО в ответ на «Чей он?» без свайпа — ответ на этот вопрос, а не
// подпись следующего чека.
func TestL6dedupe_09_CrossQuestionAnsweredWithoutSwipe(t *testing.T) {
	sb, mg := e2eGroup("l6na"), e2eGroup("l6nb")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон", "Алиев Ислам")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001119138"), "Сафаи", ocr, "Магомедов Ахмед")
	emp := user("79001119139")
	e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	if q := e.lastWith("Чей он"); q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	e.sendGroup(groupText(mg, emp, "Ислам", "Исаев Харон"))
	ck := e.sendReceipt(mg, emp, "Ислам", sberCheck("7000.00", l5collapsedPastHMS()), "")
	e.sendGroup(groupText(mg, emp, "Ислам", "Алиев Ислам"))
	r := e.db.TestReceiptByMessage(context.Background(), ck)
	if got := dayTotals(e, mg); got["Алиев Ислам"] != 7000 || got["Исаев Харон"] == 7000 {
		t.Errorf("чек 7000, подписанный следующим сообщением «Алиев Ислам», записан на %q (ответ «Исаев Харон» на вопрос про другой чек): сбор %v", r.Recipient, got)
	}
	if s, m := dayTotals(e, sb), dayTotals(e, mg); s["Исаев Харон"] != 15000 || m["Исаев Харон"] != 15000 {
		t.Errorf("ответ «Исаев Харон» без свайпа на «Чей он?» не применён: СБ %v, основная %v", s, m)
	}
}

// Находка 09: вопрос «Чей он?» переживает перезапуск бота — ответ свайпом
// после перезапуска применяется к обеим копиям.
func TestL6dedupe_09_CrossAnswerAfterRestart(t *testing.T) {
	sb, mg := e2eGroup("l6ra"), e2eGroup("l6rb")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001119148"), "Сафаи", ocr, "Магомедов Ахмед")
	emp := user("79001119149")
	e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	q := e.lastWith("Чей он")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	e.clarify = newClarifyState() // перезапуск: память о вопросах пуста
	e.sendGroup(groupReply(mg, emp, "Ислам", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if s, m := dayTotals(e, sb), dayTotals(e, mg); s["Исаев Харон"] != 15000 || m["Исаев Харон"] != 15000 {
		t.Errorf("ответ свайпом «Исаев Харон» после перезапуска не применён: СБ %v, основная %v (бот: %q)", s, m, e.last().Text)
	}
}

// Находка 10: тот же чек в основной группе назван следующим сообщением (не
// подписью) другим клиентом, чем в СБ, — тоже спрашиваем «Чей он?».
func TestL6dedupe_10_CrossGroupCheckThenNameAsks(t *testing.T) {
	sb, mg := e2eGroup("l6ca"), e2eGroup("l6cb")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001119118"), "Сафаи", ocr, "Магомедов Ахмед")
	n := len(e.sent())
	emp := user("79001119119")
	e.sendReceipt(mg, emp, "Ислам", ocr, "")
	e.sendGroup(groupText(mg, emp, "Ислам", "Исаев Харон"))
	asked := false
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "Исаев") && strings.Contains(m.Text, "Магомедов") {
			asked = true
		}
	}
	if main := dayTotals(e, mg); !asked && main["Исаев Харон"] == 0 {
		t.Errorf("в основной группе чек назван «Исаев Харон» следующим сообщением, а сбор молча %v и вопроса «чей он?» нет (в СБ тот же чек — «Магомедов Ахмед»)", main)
	}
}

// Повторная проверка l6 (R<n> — находка #n): экран «Перевод выполнен» и его
// полный чек, вопрос «Чей он?» рядом с другими открытыми вопросами.
const l6rScreen = "СберБанк\nПеревод выполнен\nХадижат Ибрагимовна И.\n5 000 ₽\nСохранить чек"

func l6rFull(op time.Time, doc string) string {
	return "СберБанк\nЧек по операции\nДата операции " + op.Format("02.01.2006 15:04:05") +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер операции в СБП\n" + doc
}

// R0: экран в обеих группах (СБ и основная), полный чек — только в СБ: полный
// чек заменил экран в СБ, а экран основной группы — тот же перевод, что и он.
func TestL6dedupe_R0_ScreenInBothGroupsFullOnlyInSB(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	for _, gap := range []time.Duration{3 * time.Minute, 4 * time.Minute, 40 * time.Minute} {
		at := today.AddDate(0, 0, -1).Add(12 * time.Hour)
		op := at.Add(-gap)
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		sb, mg := e2eGroup("l6r0a"), e2eGroup("l6r0b")
		l5collapsedPhotoAt(e, sb, user("79001116041"), l6rScreen, "Исаев Харон", at)
		l5collapsedPhotoAt(e, mg, user("79001116042"), l6rScreen, "Исаев Харон", at.Add(30*time.Second))
		day := today.AddDate(0, 0, -1)
		before := l4Total(e, nil, day, today)
		n := len(e.sent())
		l5collapsedPhotoAt(e, sb, user("79001116041"), l6rFull(op, "A6290101010101010G100400117711"), "Исаев Харон", at.Add(time.Minute))
		all := l4Total(e, nil, day, today)
		s := l4Total(e, []string{sb.String()}, day, today)
		m := l4Total(e, []string{mg.String()}, day, today)
		if all != 5000 || s != 5000 || m != 5000 {
			t.Errorf("операция за %v до экрана; экран в СБ и в основной, полный чек только в СБ: до полного чека все группы %v, после — %v (СБ %v, основная %v); бот: %v", gap, before, all, s, m, e.sentSince(n))
		}
	}
}

// R1: «Чей он?» к одному работнику не занимает место открытого вопроса «Чей
// это чек?» к другому: его ФИО без свайпа — ответ про его чек, а не подпись к
// следующему.
func TestL6dedupe_R1_OtherEmployeeReceiptAnswerAfterCrossAsk(t *testing.T) {
	ctx := context.Background()
	sb, mg := e2eGroup("l6r1a"), e2eGroup("l6r1b")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон", "Алиев Ислам")
	empA, empB := user("79001116761"), user("79001116762")
	ckB := e.sendReceipt(mg, empB, "Мурад", sberCheck("7000.00", l5collapsedPastHMS()), "")
	e.age(mg.String())
	e.clarifyTick(ctx)
	if q := e.lastWith("Чей это чек"); q.ID == "" {
		t.Fatalf("нет вопроса «чей это чек?»: %+v", e.sent())
	}
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001116763"), "Сафаи", ocr, "Магомедов Ахмед")
	e.sendReceipt(mg, empA, "Ислам", ocr, "Исаев Харон")
	if q := e.lastWith("Чей он"); q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	if err := e.db.TestAgeGroup(ctx, mg.String(), 13*time.Minute); err != nil {
		t.Fatal(err)
	}
	n := len(e.sent())
	e.sendGroup(groupText(mg, empB, "Мурад", "Алиев Ислам"))
	nextB := e.sendReceipt(mg, empB, "Мурад", sberCheck("9000.00", l5collapsedPastHMS()), "")
	r1 := e.db.TestReceiptByMessage(ctx, ckB)
	r2 := e.db.TestReceiptByMessage(ctx, nextB)
	if got := dayTotals(e, mg); got["Алиев Ислам"] != 7000 {
		t.Errorf("ответ «Алиев Ислам» без свайпа на «Чей это чек?» про чек 7000: чек 7000 → %q, следующий чек 9000 (без подписи) → %q; сбор основной %v; бот: %v", r1.Recipient, r2.Recipient, got, e.sentSince(n))
	}
}

// R1: то же с открытым вопросом «новый или тот же?» про наличку.
func TestL6dedupe_R1_CashDupAnswerAfterCrossAsk(t *testing.T) {
	ctx := context.Background()
	sb, mg := e2eGroup("l6r1c"), e2eGroup("l6r1d")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон", "Алиев Ислам")
	emp := user("79001116771")
	e.sendGroup(groupText(mg, emp, "Ислам", "Алиев Ислам нал 5000"))
	e.sendGroup(groupText(mg, emp, "Ислам", "Алиев Ислам нал 5000"))
	e.age(mg.String())
	e.clarifyTick(ctx)
	if q := e.lastWith("НОВЫЙ платёж"); q.ID == "" {
		t.Fatalf("нет вопроса «новый или тот же?»: %+v", e.sent())
	}
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001116772"), "Сафаи", ocr, "Магомедов Ахмед")
	e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	if q := e.lastWith("Чей он"); q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	n := len(e.sent())
	e.sendGroup(groupText(mg, emp, "Ислам", "новый"))
	if got := dayTotals(e, mg); got["Алиев Ислам"] != 10000 {
		t.Errorf("ответ «новый» без свайпа: сбор %v, ожидали Алиев Ислам 10000; бот: %v", got, e.sentSince(n))
	}
}

// R2: на «Чей он?» ответили свайпом — открытый раньше «Чей это чек?» по-прежнему
// принимает ответ без свайпа (и от другого работника, и от того же).
func TestL6dedupe_R2_ReceiptAnswerAfterCrossAnswered(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "другой-работник", true: "тот-же-работник"}[same], func(t *testing.T) {
			ctx := context.Background()
			sb, mg := e2eGroup("l6r2a"), e2eGroup("l6r2b")
			e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
			e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон", "Алиев Ислам")
			empA, empB := user("79001116811"), user("79001116812")
			if same {
				empB = empA
			}
			ckB := e.sendReceipt(mg, empB, "Мурад", sberCheck("7000.00", l5collapsedPastHMS()), "")
			e.age(mg.String())
			e.clarifyTick(ctx)
			if q := e.lastWith("Чей это чек"); q.ID == "" {
				t.Fatalf("нет вопроса «чей это чек?»: %+v", e.sent())
			}
			ocr := sberCheck("15000.00", l5collapsedPastHMS())
			e.sendReceipt(sb, user("79001116813"), "Сафаи", ocr, "Магомедов Ахмед")
			e.sendReceipt(mg, empA, "Ислам", ocr, "Исаев Харон")
			q := e.lastWith("Чей он")
			if q.ID == "" {
				t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
			}
			e.sendGroup(groupReply(mg, empA, "Ислам", "Исаев Харон", q.ID, e.botJID(), q.Text))
			if err := e.db.TestAgeGroup(ctx, mg.String(), 13*time.Minute); err != nil {
				t.Fatal(err)
			}
			n := len(e.sent())
			e.sendGroup(groupText(mg, empB, "Мурад", "Алиев Ислам"))
			nextB := e.sendReceipt(mg, empB, "Мурад", sberCheck("9000.00", l5collapsedPastHMS()), "")
			r1 := e.db.TestReceiptByMessage(ctx, ckB)
			r2 := e.db.TestReceiptByMessage(ctx, nextB)
			if got := dayTotals(e, mg); got["Алиев Ислам"] != 7000 {
				t.Errorf("«Чей он?» уже отвечен; «Алиев Ислам» без свайпа на «Чей это чек?» про 7000: чек 7000 → %q, следующий 9000 → %q; сбор %v; бот: %v", r1.Recipient, r2.Recipient, got, e.sentSince(n))
			}
		})
	}
}

// R3: после «Чей он?» работник пишет ФИО и следом присылает чек этого клиента
// без подписи — ФИО к этому чеку (как всегда «имя, потом чек»), а копии чека
// из вопроса никто не перекладывает.
func TestL6dedupe_R3_NameBeforeNextCheckAfterCrossAsk(t *testing.T) {
	for _, name := range []string{"Исаев Харон", "Магомедов Ахмед"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sb, mg := e2eGroup("l6r3a"), e2eGroup("l6r3b")
			e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
			e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
			ocr := sberCheck("15000.00", l5collapsedPastHMS())
			sbCk := e.sendReceipt(sb, user("79001116821"), "Сафаи", ocr, "Магомедов Ахмед")
			emp := user("79001116822")
			mgCk := e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
			if q := e.lastWith("Чей он"); q.ID == "" {
				t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
			}
			n := len(e.sent())
			e.sendGroup(groupText(mg, emp, "Ислам", name))
			ck := e.sendReceipt(mg, emp, "Ислам", sberCheck("7000.00", l5collapsedPastHMS()), "")
			r := e.db.TestReceiptByMessage(ctx, ck)
			rs, rm := e.db.TestReceiptByMessage(ctx, sbCk), e.db.TestReceiptByMessage(ctx, mgCk)
			if !r.Confirmed || r.Recipient != name || rs.Recipient != "Магомедов Ахмед" || rm.Recipient != "Исаев Харон" {
				t.Errorf("«%s» перед чеком 7000: чек 7000 → %q (подтверждён=%v); чек 15000: СБ → %q, основная → %q (никто не ответил на «Чей он?»); сбор СБ %v, основная %v; бот: %v",
					name, r.Recipient, r.Confirmed, rs.Recipient, rm.Recipient, dayTotals(e, sb), dayTotals(e, mg), e.sentSince(n))
			}
		})
	}
}

// R4: подписанный экран, затем полный чек того же перевода без подписи, ФИО —
// следующим сообщением или свайпом: один платёж (или вопрос «другой?»).
func TestL6dedupe_R4_ScreenThenUncaptionedFullThenName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gap   time.Duration
		swipe bool
	}{
		{"4мин-имя-следом", 4 * time.Minute, false},
		{"40мин-имя-следом", 40 * time.Minute, false},
		{"4мин-свайп", 4 * time.Minute, true},
		{"40мин-свайп", 40 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -1)
			op := at.Add(-tc.gap)
			day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.Local)
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = l5collapsedVision(op)
			grp, emp := e2eGroup("l6r4"), user("79001116751")
			l5collapsedPhotoAt(e, grp, emp, l6rScreen, "Исаев Харон", at)
			n := len(e.sent())
			ck := l5collapsedPhotoAt(e, grp, emp, l6rFull(op, "A6290101010101010G100400117722"), "", at.Add(30*time.Second))
			if tc.swipe {
				e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", ck, emp, ""))
			} else {
				e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
			}
			got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1))
			asked := false
			for _, m := range e.sentSince(n) {
				asked = asked || strings.Contains(m.Text, "другой") || strings.Contains(m.Text, "ДРУГОЙ")
			}
			if got != 5000 && !asked {
				r := e.db.TestReceiptByMessage(context.Background(), ck)
				t.Errorf("экран + полный чек без подписи (операция за %v до экрана), ФИО потом: сбор %v, ожидали 5000 или «другой?»; чек → %q; бот: %v", tc.gap, got, r.Recipient, e.sentSince(n))
			}
		})
	}
}

// R5: ответ на «Чей он?» про чек, операция которого была за неделю до того,
// как его прислали: текст «ФИО сумма», присланный с ним, переносится вместе с
// чеком (один платёж).
func TestL6dedupe_R5_CrossAnswerOldCheckText(t *testing.T) {
	for _, ago := range []int{3, 8} {
		sb, mg := e2eGroup("l6r5a"), e2eGroup("l6r5b")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		now := time.Now()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		opDay := today.AddDate(0, 0, -ago)
		ocr := sberCheckOn(opDay, "15000.00", "10:00:00")
		a, emp := user("79001116781"), user("79001116782")
		e.sendGroup(groupText(sb, a, "Сафаи", "Магомедов Ахмед 15000"))
		e.sendReceipt(sb, a, "Сафаи", ocr, "")
		from, to := opDay.AddDate(0, 0, -1), today.AddDate(0, 0, 1)
		before := l4Total(e, nil, from, to)
		n := len(e.sent())
		e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
		q := e.lastWith("Чей он")
		if q.ID == "" {
			t.Fatalf("нет вопроса «чей он?»: %v", e.sentSince(n))
		}
		e.sendGroup(groupReply(mg, emp, "Ислам", "Исаев Харон", q.ID, e.botJID(), q.Text))
		all := l4Total(e, nil, from, to)
		s := l4Total(e, []string{sb.String()}, from, to)
		if all != 15000 || s != 15000 {
			t.Errorf("чек от %s (%d дн. назад) + текст «Магомедов Ахмед 15000» в СБ, ответ «Исаев Харон»: до ответа все группы %v; после — все группы %v, СБ %v; бот: %q", opDay.Format("02.01"), ago, before, all, s, e.last().Text)
		}
	}
}

// R0: ответ «другой» на «похоже, это полный чек того же перевода» — экран СБ
// снова отдельный перевод, а его копия в основной группе снова с ним (время —
// её время сообщения), а не с полным чеком.
func TestL6dedupe_R0_DifferentRestoresScreenCopy(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	at := today.AddDate(0, 0, -1).Add(12 * time.Hour)
	op := at.Add(-4 * time.Minute)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	sb, mg := e2eGroup("l6r0c"), e2eGroup("l6r0d")
	emp := user("79001116043")
	l5collapsedPhotoAt(e, sb, emp, l6rScreen, "Исаев Харон", at)
	mgScreen := l5collapsedPhotoAt(e, mg, user("79001116044"), l6rScreen, "Исаев Харон", at.Add(30*time.Second))
	l5collapsedPhotoAt(e, sb, emp, l6rFull(op, "A6290101010101010G100400117733"), "Исаев Харон", at.Add(time.Minute))
	day := today.AddDate(0, 0, -1)
	if got := e.db.TestReceiptByMessage(ctx, mgScreen).TxDate; !got.Equal(op) {
		t.Errorf("копия экрана в основной: время %s, ожидали время операции полного чека %s", got.Format("15:04:05"), op.Format("15:04:05"))
	}
	q := e.lastWith("ДРУГОЙ")
	if q.ID == "" {
		t.Fatalf("нет вопроса «другой?»: %v", e.sent())
	}
	e.sendGroup(groupReply(sb, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	all := l4Total(e, nil, day, today)
	s := l4Total(e, []string{sb.String()}, day, today)
	m := l4Total(e, []string{mg.String()}, day, today)
	if all != 10000 || s != 10000 || m != 5000 {
		t.Errorf("после «другой»: все группы %v, СБ %v, основная %v; ожидали 10000/10000/5000 (бот: %q)", all, s, m, e.last().Text)
	}
	if got := e.db.TestReceiptByMessage(ctx, mgScreen).TxDate; !got.Equal(at.Add(30 * time.Second)) {
		t.Errorf("после «другой» копия экрана в основной: время %s, ожидали её время сообщения %s", got.Format("15:04:05"), at.Add(30*time.Second).Format("15:04:05"))
	}
}

// R3: ФИО без свайпа после «Чей он?», чек следом не пришёл (окно очереди
// прошло) — это был ответ на вопрос: обе копии на этого клиента.
func TestL6dedupe_R3_NameWithoutCheckAnswersCross(t *testing.T) {
	ctx := context.Background()
	sb, mg := e2eGroup("l6r3c"), e2eGroup("l6r3d")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	first := e.sendReceipt(sb, user("79001116831"), "Сафаи", ocr, "Магомедов Ахмед")
	emp := user("79001116832")
	second := e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	if q := e.lastWith("Чей он"); q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	e.sendGroup(groupText(mg, emp, "Ислам", "Исаев Харон"))
	// Until that ambiguous name is resolved, neither contradictory caption
	// may win just because it was posted first. Preserve both original labels
	// as evidence and keep the physical receipt pending in both groups.
	if s, m := dayTotals(e, sb), dayTotals(e, mg); sum(s) != 0 || sum(m) != 0 {
		t.Fatalf("спорный клиент попал в подтверждённый сбор до разрешения ответа: СБ %v, основная %v", s, m)
	}
	if a, b := e.db.TestReceiptByMessage(ctx, first), e.db.TestReceiptByMessage(ctx, second); a.Recipient != "Магомедов Ахмед" || b.Recipient != "Исаев Харон" {
		t.Fatalf("ФИО без свайпа сразу переписало исходные подписи (а это могло быть имя перед следующим чеком): %q / %q", a.Recipient, b.Recipient)
	}
	from := time.Now().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	book, err := e.db.LoadBook(ctx, from, from.Add(3*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Payments) != 1 || !book.Payments[0].ReviewRequired() || len(book.Payments[0].Conflicts) == 0 {
		t.Fatalf("чек должен сохраниться единой спорной уликой, а не пропасть: %+v", book.Payments)
	}
	// Окно очереди «имя перед чеком» прошло, чека не было.
	e.clarify.mu.Lock()
	for k, c := range e.clarify.crossCands {
		c.at = c.at.Add(-e.pairWindow - time.Minute)
		e.clarify.crossCands[k] = c
	}
	e.clarify.mu.Unlock()
	e.clarifyTick(ctx)
	if s, m := dayTotals(e, sb), dayTotals(e, mg); s["Исаев Харон"] != 15000 || m["Исаев Харон"] != 15000 {
		t.Errorf("«Исаев Харон» после «Чей он?» без чека следом: СБ %v, основная %v; ожидали обе копии на Исаева (бот: %q)", s, m, e.last().Text)
	}
	// Имя снято из очереди — следующий чек без подписи его не возьмёт.
	ck := e.sendReceipt(mg, emp, "Ислам", sberCheck("7000.00", l5collapsedPastHMS()), "")
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Confirmed {
		t.Errorf("ответ на «Чей он?» достался и следующему чеку: %q", r.Recipient)
	}
}

// R3: ФИО без свайпа после «Чей он?», чек без подписи его взял, а потом чек
// назвали другим ФИО свайпом — первое ФИО было ответом на «Чей он?».
func TestL6dedupe_R3_NameCheckSwipeOtherName(t *testing.T) {
	ctx := context.Background()
	sb, mg := e2eGroup("l6r3e"), e2eGroup("l6r3f")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон", "Алиев Ислам")
	ocr := sberCheck("15000.00", l5collapsedPastHMS())
	e.sendReceipt(sb, user("79001116841"), "Сафаи", ocr, "Магомедов Ахмед")
	emp := user("79001116842")
	e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
	if q := e.lastWith("Чей он"); q.ID == "" {
		t.Fatalf("нет вопроса «чей он?»: %v", e.sent())
	}
	e.sendGroup(groupText(mg, emp, "Ислам", "Исаев Харон"))
	ck := e.sendReceipt(mg, emp, "Ислам", sberCheck("7000.00", l5collapsedPastHMS()), "")
	e.sendGroup(groupReply(mg, emp, "Ислам", "Алиев Ислам", ck, emp, ""))
	r := e.db.TestReceiptByMessage(ctx, ck)
	s, m := dayTotals(e, sb), dayTotals(e, mg)
	if r.Recipient != "Алиев Ислам" || s["Исаев Харон"] != 15000 || m["Исаев Харон"] != 15000 {
		t.Errorf("«Исаев Харон», чек 7000, свайп «Алиев Ислам» на чек: чек 7000 → %q; СБ %v, основная %v", r.Recipient, s, m)
	}
}
