package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/parser"
)

// Регрессии общей проверки: дубли, контакты, комиссия, статус, даты чеков.

func monthTotals(e *e2eBot, groups []string, from, to time.Time) float64 {
	ss, err := e.db.SummaryForPeriod(context.Background(), from, to, groups)
	if err != nil {
		e.t.Fatal(err)
	}
	var s float64
	for _, x := range ss {
		s += x.Total
	}
	return s
}

func today() (time.Time, time.Time) {
	n := time.Now()
	d := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
	return d, d.AddDate(0, 0, 1)
}

func TestSweepTextAndCheckCaseAndOrder(t *testing.T) {
	// 3. Текст и чек одного платежа — один раз, как бы ни написали имя.
	for _, pair := range [][2]string{{"магомедов ахмед", "Магомедов Ахмед 15000"}, {"Магомедов Ахмед", "Ахмед Магомедов 15000"}} {
		e := newE2E(t, e2eOpts{})
		e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
		e.aliases.Add("Ахмед Магомедов", "Ахмед Магомедов")
		grp := e2eGroup("ord")
		e.sendReceipt(grp, user("79001110100"), "Сафаи", sberCheck("15000.00", "10:00:00"), pair[0])
		e.sendGroup(groupText(grp, user("79001110101"), "Ислам", pair[1]))
		from, to := today()
		if got := monthTotals(e, []string{grp.String()}, from, to); got != 15000 {
			t.Errorf("%v: сбор %v, ожидали 15000", pair, got)
		}
	}
}

func TestSweepDuplicateChecks(t *testing.T) {
	from, to := today()
	// 14/27. Тот же номер операции СБП — один чек, как бы ни подписали.
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("dupop")
	chk := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 11:22:33\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n10000.00 ₽\nНомер операции в СБП\nA6290222222222222G100400117911"
	e.sendReceipt(grp, user("79001110008"), "Ислам", chk, "Магомедов Ахмед")
	e.sendReceipt(grp, user("79001110008"), "Ислам", chk, "Хасанов Ислам")
	ck3 := e.sendReceipt(grp, user("79001110008"), "Ислам", chk, "")
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 10000 {
		t.Errorf("один и тот же чек посчитан %v раз(а)", got/10000)
	}
	if r := e.db.TestReceiptByMessage(context.Background(), ck3); !r.Duplicate || r.NeedsReview {
		t.Errorf("копия без подписи — должна быть дублем без вопроса: %+v", r)
	}
	// 15. Разные чеки на одну карту (разные плательщики) — не дубль.
	e2 := newE2E(t, e2eOpts{})
	g2 := e2eGroup("diff")
	d := time.Now().Format("02.01.2006")
	c1 := "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", 13:02\nИмя плательщика Зарема Б.\nСумма операции 5 000 ₽"
	c2 := "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", 13:05\nИмя плательщика Луиза Рамзановна Ю.\nСумма операции 5 000 ₽"
	e2.sendReceipt(g2, user("79001110012"), "Ислам", c1, "")
	n := len(e2.sent())
	e2.sendReceipt(g2, user("79001110012"), "Ислам", c2, "")
	for _, m := range e2.sentSince(n) {
		if strings.Contains(m.Text, "уже присылали") {
			t.Errorf("разные чеки приняты за дубль: %s", m.Text)
		}
	}
}

func TestSweepCrossGroupCounting(t *testing.T) {
	// 28/32. Чек в группе СБ и тот же платёж текстом в основной — один платёж в
	// общем сборе и в отчёте по человеку; в каждой группе отдельно — по 15000.
	from, to := today()
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	sb, main := e2eGroup("sb"), e2eGroup("main")
	e.sendReceipt(sb, user("79001110200"), "Сафаи", sberCheck("15000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(main, user("79001110201"), "Ахмед", "Магомедов Ахмед 15000"))
	if got := monthTotals(e, nil, from, to); got != 15000 {
		t.Errorf("по всем группам: %v, ожидали 15000", got)
	}
	if got := monthTotals(e, []string{main.String()}, from, to); got != 15000 {
		t.Errorf("основная группа: %v", got)
	}
	if got := monthTotals(e, []string{sb.String()}, from, to); got != 15000 {
		t.Errorf("группа СБ: %v", got)
	}
	// Тот же чек в двух группах — в отчёте по человеку один раз.
	e.sendReceipt(main, user("79001110201"), "Ахмед", sberCheck("7000.00", "11:00:00"), "Магомедов Ахмед")
	ps, err := e.db.PersonReport(context.Background(), "Магомедов", &from, &to, "")
	if err != nil || len(ps) == 0 {
		t.Fatalf("person_report: %v %v", ps, err)
	}
	total := ps[0].ReceiptTotal + ps[0].PaymentTotal
	if got := monthTotals(e, nil, from, to); got != total {
		t.Errorf("отчёт по человеку %v расходится со сбором %v", total, got)
	}
}

func TestSweepReceiptParsing(t *testing.T) {
	d := time.Now().Format("02.01.2006")
	// 12. Т-Банк: «Итого» с комиссией — в сбор сумма перевода.
	tb := "Квитанция\n" + d + " 14:22:11\nИтого 15 150 ₽\nПеревод По номеру карты\nСтатус Успешно\nСумма 15 000 ₽\nКомиссия 150 ₽\nОтправитель Ахмед Магомедов\nПолучатель Хадижат Ибрагимовна И.\nАО «ТБАНК»\nКвитанция № 1-23-456-789-012"
	rd := parser.ParseReceipt(tb)
	if rd.Amount != 15000 {
		t.Errorf("Т-Банк с комиссией: %v", rd.Amount)
	}
	if strings.Contains(rd.DocNumber, ":") || rd.DocNumber == d+" 14:22:11" {
		t.Errorf("дата принята за номер документа: %q", rd.DocNumber)
	}
	oz := "Озон Банк\nИтого 40 400 ₽\nСумма 40 000 ₽\nКомиссия 400 ₽\nДата операции " + d + " 12:00\nПолучатель Хадижат И."
	if rd := parser.ParseReceipt(oz); rd.Amount != 40000 {
		t.Errorf("Озон с комиссией: %v", rd.Amount)
	}
	// 16. Даты в разных форматах.
	for _, line := range []string{
		"Дата операции: 30.09.2026\nВремя операции: 23:40",
		"16.10.2026 в 21:15",
		"16.10.26 21:15",
		"16 окт. 2026, 21:15",
		"2026-10-16 21:15:03",
	} {
		r := parser.ParseReceipt("Сбербанк\n" + line + "\nСумма перевода 5000 ₽")
		if !r.HasTxTime {
			t.Errorf("не распознана дата: %q", line)
		}
	}
}

func TestSweepDeclinedAndOldChecks(t *testing.T) {
	// 13. Отклонённый перевод — не в сборе, с предупреждением.
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("decl")
	txt := "Квитанция\n" + time.Now().Format("02.01.2006") + " 10:05:41\nИтого 12 000 ₽\nПеревод По номеру телефона\nСтатус Отклонено\nСумма 12 000 ₽\nКомиссия 0 ₽\nПолучатель Хадижат И.\nАО «ТБАНК»\nКвитанция № 1-55-111-222-333"
	e.sendReceipt(grp, user("79001110004"), "Ислам", txt, "Магомедов Ахмед")
	from, to := today()
	if got := monthTotals(e, []string{grp.String()}, from, to); got != 0 {
		t.Errorf("отклонённый перевод в сборе: %v", got)
	}
	if !strings.Contains(e.last().Text, "не прошёл") {
		t.Errorf("нет предупреждения: %q", e.last().Text)
	}
	// 16. Чек прошлого месяца не попадает в текущий.
	now := time.Now()
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	last := ms.AddDate(0, 0, -1)
	g2 := e2eGroup("old")
	old := "Почта Банк\nКвитанция о переводе\nДата операции: " + last.Format("02.01.2006") + "\nВремя операции: 23:40\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n11 000 ₽\nНомер операции\nP123456"
	e.sendReceipt(g2, user("79001110006"), "Ислам", old, "Магомедов Ахмед")
	if got := monthTotals(e, []string{g2.String()}, ms, ms.AddDate(0, 1, 0)); got != 0 {
		t.Errorf("чек от %s посчитан в текущем месяце: %v", last.Format("02.01"), got)
	}
}
