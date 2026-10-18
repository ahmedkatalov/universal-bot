package bot

import (
	"context"
	"strings"
	"testing"
	"time"
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
