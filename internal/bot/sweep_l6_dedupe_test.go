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
