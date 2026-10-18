package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
)

// Регрессии L4 (разбор): дата операции на PDF-чеке и сумма в ответе на вопрос.

// TestL4parse_18_FormationDateBeatsOperationDate — PDF скачали утром: «Дата
// формирования» (сегодня) не должна стать датой чека вместо «Даты операции».
func TestL4parse_18_FormationDateBeatsOperationDate(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("form")
	now := time.Now()
	yest := now.AddDate(0, 0, -1)
	pdf := "Почта Банк\nКвитанция о переводе\nДата формирования: " + now.Format("02.01.2006") + " 09:15:02\nДата операции: " + yest.Format("02.01.2006") +
		"\nВремя операции: 23:40\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n11 000 ₽\nНомер операции\nP123456789"
	ck := e.sendReceipt(grp, user("79001112009"), "Сафаи", pdf, "Исаев Харон")
	r := e.db.TestReceiptByMessage(ctx, ck)
	want := time.Date(yest.Year(), yest.Month(), yest.Day(), 23, 40, 0, 0, time.Local)
	if !r.TxDate.Equal(want) {
		t.Errorf("дата чека %v, ожидали дату операции %v", r.TxDate, want)
	}
	if got := dayTotals(e, grp); len(got) != 0 {
		t.Errorf("вчерашний платёж попал в сегодняшний сбор: %v", got)
	}
}

// TestL4parse_21_AnswerAmountMisread — ответ на «не разобрал сумму»: «15тр»,
// «15 т.р.», «15 тыс.руб» — это 15 000, а дата после суммы («15000 18.10») —
// не часть суммы. Клиент — «Исаев Харон», без «т.р».
func TestL4parse_21_AnswerAmountMisread(t *testing.T) {
	ctx := context.Background()
	for _, ans := range []string{"Исаев Харон 15тр", "Исаев Харон 15 т.р.", "Исаев Харон 15 тыс.руб", "Исаев Харон 15 тыс. р.", "Исаев Харон 15000 18.10"} {
		e := newE2E(t, e2eOpts{})
		grp, emp := e2eGroup("an"), user("79285550008")
		ocr := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:00:00" +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nНомер операции в СБП\n" + e2eID("OP") // сумма не прочитана
		ck := e.sendReceipt(grp, emp, "Сафаи", ocr, "")
		q := e.lastWith("не разобрал")
		if q.ID == "" {
			t.Fatalf("нет вопроса по чеку: %+v", e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", ans, q.ID, e.botJID(), q.Text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 15000 || sum(got) != 15000 {
			t.Errorf("ответ %q: чек записан на %v ₽ (клиент %q), сбор %v — ожидали Исаев Харон 15000", ans, r.Amount, r.Recipient, got)
		}
	}
}

// TestL4parse_21_AmbiguousAnswerAsksAgain — в ответе две разные суммы: бот не
// склеивает их и не выбирает сам, а переспрашивает сумму.
func TestL4parse_21_AmbiguousAnswerAsksAgain(t *testing.T) {
	ctx := context.Background()
	for _, ans := range []string{"Исаев Харон 15000 20000", "Исаев Харон 15000\n20000"} {
		e := newE2E(t, e2eOpts{})
		grp, emp := e2eGroup("amb"), user("79285550009")
		ocr := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:00:00" +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nНомер операции в СБП\n" + e2eID("OP")
		ck := e.sendReceipt(grp, emp, "Сафаи", ocr, "")
		q := e.lastWith("не разобрал")
		if q.ID == "" {
			t.Fatalf("нет вопроса по чеку: %+v", e.sent())
		}
		n := len(e.sent())
		e.sendGroup(groupReply(grp, emp, "Сафаи", ans, q.ID, e.botJID(), q.Text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		if r.Amount != 0 || sum(dayTotals(e, grp)) != 0 {
			t.Errorf("ответ %q: бот сам выбрал сумму %v ₽ (сбор %v) — должен переспросить", ans, r.Amount, dayTotals(e, grp))
		}
		asked := false
		for _, m := range e.sentSince(n) {
			if strings.Contains(strings.ToLower(m.Text), "сумм") {
				asked = true
			}
		}
		if !asked {
			t.Errorf("ответ %q: бот не переспросил сумму: %+v", ans, e.sentSince(n))
		}
	}
}

// TestL4parse_21_NameWithoutMoneyTail — хвост суммы («т.р.», «тыс. руб.») не
// попадает в ФИО ни в подписи к чеку, ни в ответе на вопрос.
func TestL4parse_21_NameWithoutMoneyTail(t *testing.T) {
	for _, in := range []string{"Исаев Харон 15 т.р.", "Исаев Харон 15 тыс.руб", "Исаев Харон 15 тыс. руб.", "Исаев Харон 15000 руб."} {
		if n, ok := looksLikeName(in); !ok || n != "Исаев Харон" {
			t.Errorf("looksLikeName(%q) = %q, %v — ожидали «Исаев Харон»", in, n, ok)
		}
		name, amount, deferToAI := clarifyNameFromReply(in)
		if name != "Исаев Харон" || deferToAI || amount != 15000 {
			t.Errorf("clarifyNameFromReply(%q) = %q, %v, defer=%v — ожидали «Исаев Харон», 15000", in, name, amount, deferToAI)
		}
	}
	// Инициал — не хвост суммы.
	if n, _ := looksLikeName("Исаев Харон К."); n != "Исаев Харон К" {
		t.Errorf("looksLikeName(«Исаев Харон К.») = %q — инициал потерян", n)
	}
}

// TestL4parse_21_MoneyHintsNotJoined — подсказка ИИ с суммами: «15000 200» —
// два числа, «15 т.р.» — 15000.
func TestL4parse_21_MoneyHintsNotJoined(t *testing.T) {
	cases := map[string][]float64{
		"Исаев Харон 15000 200":    {15000, 200},
		"Исаев Харон 15000\n20000": {15000, 20000},
		"Исаев Харон 15 т.р.":      {15000},
		"Исаев Харон 15тр":         {15000},
		"Исаев Харон 15 000 ₽":     {15000},
	}
	for in, want := range cases {
		got := moneyTokens(in)
		if len(got) != len(want) {
			t.Errorf("moneyTokens(%q) = %v, ожидали %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("moneyTokens(%q) = %v, ожидали %v", in, got, want)
				break
			}
		}
	}
}

// l4AIEcho — ИИ, который возвращает имя клиента ровно как его написал работник.
func l4AIEcho(c aiCall) aiReply {
	if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
		return aiReply{Text: `{"client":"` + strings.TrimSpace(c.lastUserText()) + `"}`}
	}
	if strings.Contains(c.System, "модуль разбора платежей") {
		return aiReply{Text: `{"payments":[],"clarify":""}`}
	}
	return aiReply{Text: ""}
}

// TestL4parse_R0_InitialsMergeTwoClients — инициалы из букв Т/Р/К («Р.К.»,
// «Т.К.») — не хвост суммы «т.р»: «Магомедов Р.К.» и «Магомедов Т.К.» — два
// разных клиента, а не один «Магомедов».
func TestL4parse_R0_InitialsMergeTwoClients(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = l4AIEcho
	grp := e2eGroup("ini7")
	e.groupsCache[grp] = "Оплата"
	emp := user("79285550107")
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q1 := e.lastWith("Чей это чек")
	if q1.ID == "" {
		t.Fatalf("нет вопроса 1: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Р.К.", q1.ID, e.botJID(), q1.Text))
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("20000.00", "10:05:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q2 := e.lastWith("Чей это чек")
	if q2.ID == "" || q2.ID == q1.ID {
		t.Fatalf("нет вопроса 2: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Т.К.", q2.ID, e.botJID(), q2.Text))
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r1.Recipient == r2.Recipient || len(got) != 2 || sum(got) != 35000 {
		t.Errorf("два разных клиента слиты: чек1=%q чек2=%q, сбор %v", r1.Recipient, r2.Recipient, got)
	}
}

// TestL4parse_R0_InitialsNameAfterCheck — «Магомедов Р.К.» / «Исаев Т.Р.»
// отдельным сообщением после чека (с ИИ) или подписью к чеку (без ИИ) — это
// клиент чека.
func TestL4parse_R0_InitialsNameAfterCheck(t *testing.T) {
	ctx := context.Background()
	for _, nm := range []string{"Магомедов Р.К.", "Исаев Т.Р."} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l4AIEcho
		grp := e2eGroup("ini5")
		emp := user("79285550106")
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", nm))
		if r := e.db.TestReceiptByMessage(ctx, ck); !r.Confirmed || sum(dayTotals(e, grp)) != 15000 {
			t.Errorf("%q после чека: клиент %q confirmed=%v, сбор %v", nm, r.Recipient, r.Confirmed, dayTotals(e, grp))
		}
	}
	// Подпись к чеку с инициалами — без ИИ.
	for _, nm := range []string{"Магомедов Р.К.", "Исаев Т.Р."} {
		e := newE2E(t, e2eOpts{})
		grp := e2eGroup("ini6")
		ck := e.sendReceipt(grp, user("79285550109"), "Сафаи", sberCheck("15000.00", "10:00:00"), nm)
		r := e.db.TestReceiptByMessage(ctx, ck)
		want := strings.TrimSuffix(nm, ".")
		if got := dayTotals(e, grp); got[want] != 15000 {
			t.Errorf("подпись %q: клиент %q, сбор %v — ожидали %q 15000", nm, r.Recipient, got, want)
		}
	}
}

// TestL4parse_R0_CmfAnswerInitials — ответ «Магомедов Р.К.» не выбирает
// уверенно кандидата с другими инициалами (Ахмед Курбанович).
func TestL4parse_R0_CmfAnswerInitials(t *testing.T) {
	cands := []cmf.ClientInfo{{ID: "1", FullName: "Магомедов Ахмед Курбанович"}, {ID: "2", FullName: "Исаев Харон Русланович"}}
	a, confident := parseCmfAnswer("Магомедов Р.К.", "", cands)
	if a.Kind == ansPick && confident {
		t.Errorf("parseCmfAnswer(«Магомедов Р.К.») уверенно выбрал %d (%s)", a.Pick, cands[a.Pick].FullName)
	}
	for _, in := range []string{"Магомедов Р.К.", "Исаев Т.Р.", "Алиев К.Т.", "Алиев Т.К."} {
		n, ok := looksLikeName(in)
		if !ok || len(strings.Fields(n)) != 2 {
			t.Errorf("looksLikeName(%q) = %q, %v — инициалы потеряны", in, n, ok)
		}
		if name, _, _ := clarifyNameFromReply(in); len(strings.Fields(name)) != 2 {
			t.Errorf("clarifyNameFromReply(%q) = %q — инициалы потеряны", in, name)
		}
	}
}

// TestL4parse_R1_AnswerWithContractNo — номер договора («дог.345», «д.345») в
// ответе на «не разобрал сумму» — не вторая сумма.
func TestL4parse_R1_AnswerWithContractNo(t *testing.T) {
	ctx := context.Background()
	for _, ans := range []string{"Исаев Харон 15000 дог.345", "Исаев Харон 15000 д.345"} {
		e := newE2E(t, e2eOpts{})
		grp, emp := e2eGroup("dog"), user("79285550108")
		ocr := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:00:00" +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nНомер операции в СБП\n" + e2eID("OP")
		ck := e.sendReceipt(grp, emp, "Сафаи", ocr, "")
		q := e.lastWith("не разобрал")
		if q.ID == "" {
			t.Fatalf("нет вопроса по чеку: %+v", e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", ans, q.ID, e.botJID(), q.Text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		if got := dayTotals(e, grp); got["Исаев Харон"] != 15000 {
			t.Errorf("ответ %q: чек %v ₽ (клиент %q), сбор %v — ожидали Исаев Харон 15000", ans, r.Amount, r.Recipient, got)
		}
	}
}

// TestL4parse_R2_DebitDateBeatsOperationDate — «Дата списания» со временем не
// перебивает явные «Дата операции» + «Время операции».
func TestL4parse_R2_DebitDateBeatsOperationDate(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("deb")
	now := time.Now()
	yest := now.AddDate(0, 0, -1)
	pdf := "Квитанция о переводе\nДата операции: " + yest.Format("02.01.2006") + "\nВремя операции: 23:40\nДата списания: " + now.Format("02.01.2006") +
		" 00:15\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n11 000 ₽\nНомер операции\n" + e2eID("P")
	ck := e.sendReceipt(grp, user("79001112010"), "Сафаи", pdf, "Исаев Харон")
	r := e.db.TestReceiptByMessage(ctx, ck)
	want := time.Date(yest.Year(), yest.Month(), yest.Day(), 23, 40, 0, 0, time.Local)
	if !r.TxDate.Equal(want) || len(dayTotals(e, grp)) != 0 {
		t.Errorf("дата чека %v (ожидали %v), сегодняшний сбор %v", r.TxDate, want, dayTotals(e, grp))
	}
}
