package bot

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии проверки нового движка подсчёта (4-й раунд, 2026-10-18): сбор
// считается одинаково во всех отчётах, текст и чек одного платежа — один платёж.

func l4Today() (time.Time, time.Time) {
	n := time.Now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	return from, from.AddDate(0, 0, 1)
}

// l4Total — сбор за [from, to) по группам (nil — все) по дате операции.
func l4Total(e *e2eBot, groups []string, from, to time.Time) float64 {
	e.waitIdle()
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

// Находка 0: Same check plus its text posted in two groups is counted twice: «one text per check» blocks the second group's text
func TestL4_00_SameCheckAndTextInTwoGroups(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	sb, main := e2eGroup("sb"), e2eGroup("main")
	emp, acc := user("79001115501"), user("79001115502")
	ocr := sberCheck("15000.00", "10:00:00")
	e.sendReceipt(sb, emp, "Сафаи", ocr, "Магомедов Ахмед")
	e.sendGroup(groupText(sb, emp, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendGroup(groupText(main, acc, "Ислам", "Магомедов Ахмед 15000"))
	e.sendReceipt(main, acc, "Ислам", ocr, "Магомедов Ахмед")
	from, to := l4Today()
	all := l4Total(e, nil, from, to)
	m := l4Total(e, []string{main.String()}, from, to)
	s := l4Total(e, []string{sb.String()}, from, to)
	t.Logf("все группы=%v основная=%v СБ=%v", all, m, s)
	if all != 15000 || m != 15000 || s != 15000 {
		t.Errorf("один платёж 15000 (тот же чек и текст в двух группах): все группы %v, основная %v, СБ %v", all, m, s)
	}
}

// Находка 1: Fuzzy SameName merges different clients (Омаров Магомед / Умаров Магомед): one client's payment disappears
func TestL4_01_SimilarSurnamesAreDifferentClients(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Омаров Магомед", "Умаров Магомед")
	grp, emp := e2eGroup("om"), user("79001115522")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "10:00:00"), "Омаров Магомед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Умаров Магомед 5000"))
	got := dayTotals(e, grp)
	from, to := l4Today()
	pu, _ := e.db.PersonReport(ctx, "Умаров", &from, &to, "")
	t.Logf("(а) сбор: %v; отчёт по Умарову: %+v", got, pu)
	if got["Умаров Магомед"] != 5000 || sum(got) != 10000 {
		t.Errorf("(а) Омаров 5000 чеком + Умаров 5000 текстом, а сбор %v: %v", sum(got), got)
	}

	e2 := newE2E(t, e2eOpts{WithAI: true})
	e2.ai = aiNamesAndPayments("Омаров Магомед", "Умаров Магомед")
	ga, gb := e2eGroup("fa"), e2eGroup("fb")
	d := time.Now().Format("02.01.2006")
	c1 := "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", 13:02\nИмя плательщика Зарема Б.\nСумма операции 5 000 ₽"
	c2 := "ВТБ\nИсходящий перевод СБП\nМурад Исаевич К.\nСтатус Выполнено\nДата операции " + d + ", 13:03\nИмя плательщика Луиза Рамзановна Ю.\nСумма операции 5 000 ₽"
	e2.sendReceipt(ga, user("79001115571"), "Сафаи", c1, "Омаров Магомед")
	e2.sendReceipt(gb, user("79001115572"), "Ислам", c2, "Умаров Магомед")
	if all := l4Total(e2, nil, from, to); all != 10000 {
		t.Errorf("(б) два разных чека (разные карты и плательщики) Омарова и Умарова по 5000, а сбор по всем группам %v", all)
	}
}

// Находка 2: Split payment: two checks (10000 + 5000) plus the total text «Магомедов Ахмед 15000» is counted twice
func TestL4_02_TwoChecksAndTotalText(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, emp := e2eGroup("split"), user("79001115591")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "10:01:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000"))
	got := dayTotals(e, grp)
	t.Logf("сбор: %v", got)
	if sum(got) != 15000 {
		t.Errorf("15000 двумя чеками (10000+5000) + итог текстом = 15000, а сбор %v", sum(got))
	}
}

// Находка 4: Greedy text-to-check pairing: an earlier text-only transfer takes the next day's check, so one day or month loses 5000 and the next counts it twice
func TestL4_04_EarlierTextStealsNextDayCheck(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, emp := e2eGroup("greedy"), user("79001115511")
	now := time.Now()
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, -1, 0)
	d1 := ms.AddDate(0, 0, -1).Add(18 * time.Hour)
	d2 := ms.Add(9 * time.Hour)
	m1 := groupText(grp, emp, "Сафаи", "Магомедов Ахмед 5000")
	m1.Info.Timestamp = d1
	e.sendGroup(m1)
	ocr := sberCheckOn(d2, "5000.00", "08:58:00")
	wa := e2eID("CHK")
	raw, _, err := e.db.SaveRawMessage(ctx, wa, grp.String(), emp.String(), emp.User, "Сафаи", "Магомедов Ахмед\n"+ocr, true, "", d2)
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, grp, emp.String(), wa, "Магомедов Ахмед\n"+ocr, raw, d2, nil, ".jpg", "Магомедов Ахмед", 0)
	e.waitIdle()
	m2 := groupText(grp, emp, "Сафаи", "Магомедов Ахмед 5000")
	m2.Info.Timestamp = d2.Add(time.Minute)
	e.sendGroup(m2)
	prev := l4Total(e, nil, ms.AddDate(0, -1, 0), ms)
	cur := l4Total(e, nil, ms, ms.AddDate(0, 1, 0))
	t.Logf("%s: %v, %s: %v", d1.Format("01.2006"), prev, d2.Format("01.2006"), cur)
	if prev != 5000 || cur != 5000 {
		t.Errorf("сбор за %s = %v (хочу 5000 — перевод %s), за %s = %v (хочу 5000 — чек и его текст)",
			d1.Format("01.2006"), prev, d1.Format("02.01"), d2.Format("01.2006"), cur)
	}
}

// Находка 5: Same check in two groups, unnamed in one: cards_report counts it twice, and group B's report shows it neither in the sum nor in «НЕ вошло»
func TestL4_05_SameCheckReviewInOtherGroup(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	ga, gb := e2eGroup("ca"), e2eGroup("cb")
	ocr := sberCheck("12000.00", "11:00:00")
	e.sendReceipt(ga, user("79001115541"), "Сафаи", ocr, "Исаев Харон")
	e.sendReceipt(gb, user("79001115542"), "Ислам", ocr, "")
	from, to := l4Today()
	cards, _ := e.db.CardTotals(ctx, from, to, nil)
	var ct float64
	for _, c := range cards {
		ct += c.Total
	}
	b := l4Total(e, []string{gb.String()}, from, to)
	ex, _ := e.db.ReportExclusions(ctx, from, to, []string{gb.String()})
	t.Logf("сбор=%v, по картам=%v (%+v); группа B: сбор=%v, НЕ вошло=%d шт на %v", l4Total(e, nil, from, to), ct, cards, b, ex.NeedsReviewCount, ex.NeedsReviewSum)
	if ct != 12000 {
		t.Errorf("на карты пришёл один чек 12000, а отчёт по картам %v", ct)
	}
	if b+ex.NeedsReviewSum != 12000 {
		t.Errorf("группа B: чек не в сборе (%v) и не в «НЕ вошло» (%v)", b, ex.NeedsReviewSum)
	}
}

// Находка 6: Same op number, amount corrected in one group: the engine keeps two payments (28000 instead of 13000)
func TestL4_06_SameDocAmountCorrectedInOneGroup(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	sb, main := e2eGroup("csb"), e2eGroup("cmain")
	emp, acc := user("79001115561"), user("79001115562")
	ocr := sberCheck("15000.00", "12:00:00")
	ck := e.sendReceipt(sb, emp, "Сафаи", ocr, "Исаев Харон")
	e.sendReceipt(main, acc, "Ислам", ocr, "Исаев Харон")
	e.sendGroup(groupReply(sb, emp, "Сафаи", "не 15000 а 13000", ck, emp, ""))
	from, to := l4Today()
	all := l4Total(e, nil, from, to)
	t.Logf("все группы=%v СБ=%v основная=%v; бот: %q", all, l4Total(e, []string{sb.String()}, from, to),
		l4Total(e, []string{main.String()}, from, to), e.last().Text)
	if all != 13000 {
		t.Errorf("один чек (тот же номер операции) на 13000, а сбор по всем группам %v", all)
	}
}

// Находка 7: «По дате в чате»: a check reposted in another group 10 days later is counted on both days, because merging depends on the ±4-day LoadBook window
func TestL4_07_ByChatRepostAfterTenDays(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	ga, gb := e2eGroup("ra"), e2eGroup("rb")
	now := time.Now()
	d1 := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local).AddDate(0, 0, -10)
	d2 := now.Add(-time.Minute)
	ocr := sberCheckOn(d1, "12000.00", "09:58:00")
	for i, p := range []struct {
		grp string
		at  time.Time
	}{{ga.String(), d1}, {gb.String(), d2}} {
		wa := e2eID("CHK")
		snd := user("7900111558" + string(rune('1'+i)))
		raw, _, err := e.db.SaveRawMessage(ctx, wa, p.grp, snd.String(), snd.User, "Сафаи", "Исаев Харон\n"+ocr, true, "", p.at)
		if err != nil {
			t.Fatal(err)
		}
		g := ga
		if i == 1 {
			g = gb
		}
		e.handleBankReceipt(ctx, g, snd.String(), wa, "Исаев Харон\n"+ocr, raw, p.at, nil, ".jpg", "Исаев Харон", 0)
		e.waitIdle()
	}
	ms := time.Date(d1.Year(), d1.Month(), 1, 0, 0, 0, 0, time.Local)
	me := time.Date(d2.Year(), d2.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, 1, 0)
	tot := func(from, to time.Time) float64 {
		ss, err := e.db.SummaryForPeriodByChat(ctx, from, to, ms, me, nil)
		if err != nil {
			t.Fatal(err)
		}
		var s float64
		for _, x := range ss {
			s += x.Total
		}
		return s
	}
	first := time.Date(d1.Year(), d1.Month(), d1.Day(), 0, 0, 0, 0, time.Local)
	last := time.Date(d2.Year(), d2.Month(), d2.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	var byDays float64
	for d := first; d.Before(last); d = d.AddDate(0, 0, 1) {
		if v := tot(d, d.AddDate(0, 0, 1)); v != 0 {
			t.Logf("%s: %v", d.Format("02.01"), v)
			byDays += v
		}
	}
	whole := tot(first, last)
	t.Logf("по дням=%v, одним периодом=%v", byDays, whole)
	if byDays != whole || whole != 12000 {
		t.Errorf("один чек 12000: по дням в сумме %v, за весь период %v", byDays, whole)
	}
}

// Находка 8: The same payment is counted twice in the all-groups сбор when it is reported as text in both groups but has one check
func TestL4_08_TextInSecondGroupDoubleCounted(t *testing.T) {
	sb, mg := e2eGroup("sb"), e2eGroup("main")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf, isl := user("79001110001"), user("79001110002")
	e.sendGroup(groupText(sb, saf, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendReceipt(sb, saf, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
	e.sendGroup(groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000"))
	from, to := l4Today()
	all := l4Total(e, nil, from, to)
	gsb := l4Total(e, []string{sb.String()}, from, to)
	gmain := l4Total(e, []string{mg.String()}, from, to)
	legacy, _ := e.db.LegacyTotal(context.Background(), from, to, nil)
	t.Logf("все группы %v, СБ %v, основная %v; старый SQL по всем группам %v", all, gsb, gmain, legacy)
	if all != 15000 {
		t.Errorf("один платёж (текст+чек в СБ, тот же текст в основной) в сборе по всем группам = %v, хочу 15000", all)
	}
}

// Находка 9: cards_report double-counts a check posted in two groups when a copy is still waiting for a name (70000 shown, 35000 arrived)
func TestL4_09_CardsDoubleCountSameCheckTwoGroups(t *testing.T) {
	sb, mg := e2eGroup("sb"), e2eGroup("main")
	e := newE2E(t, e2eOpts{Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	saf, isl := user("79001110001"), user("79001110002")
	a := sberCheck("15000.00", "10:00:00")
	e.sendReceipt(sb, saf, "Сафаи", a, "Магомедов Ахмед")
	e.sendReceipt(mg, isl, "Ислам", a, "")
	b := sberCheck("20000.00", "11:00:00")
	e.sendReceipt(sb, saf, "Сафаи", b, "")
	e.sendReceipt(mg, isl, "Ислам", b, "")
	d := time.Now().Format("2006-01-02")
	t.Logf("cards_report:\n%s", e.tool("cards_report", sb, `{"from_date":"`+d+`","to_date":"`+d+`"}`))
	from, to := l4Today()
	cards, err := e.db.CardTotals(context.Background(), from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, c := range cards {
		total += c.Total
	}
	if total != 35000 {
		t.Errorf("на карты за день пришло 35000 (два перевода), cards_report показывает %v", total)
	}
}

// Находка 11: An earlier text-only transfer takes the next day's check, moving 5000 from September to October
func TestL4_11_TextStealsNextDaysCheck(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("parts")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf := user("79001110001")
	sep30 := time.Date(2026, 9, 30, 21, 0, 0, 0, time.Local)
	oct1 := time.Date(2026, 10, 1, 9, 59, 0, 0, time.Local)
	m1 := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000")
	m1.Info.Timestamp = sep30
	e.sendGroup(m1)
	m2 := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000")
	m2.Info.Timestamp = oct1
	e.sendGroup(m2)
	at := oct1.Add(time.Minute)
	caption := "Магомедов Ахмед"
	text := caption + "\n" + sberCheckOn(oct1, "5000.00", "09:58:00")
	waID := e2eID("CHK")
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", at)
	if err != nil {
		t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
		ID: waID, PushName: "Сафаи", Timestamp: at}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, at, nil, ".jpg", payer, payerAmount)
	e.waitIdle()
	sep := l4Total(e, nil, time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local))
	oct := l4Total(e, nil, time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local))
	if sep != 5000 || oct != 5000 {
		t.Errorf("сентябрь %v (хочу 5000), октябрь %v (хочу 5000): вечерний перевод 30.09 «съел» чек 01.10", sep, oct)
	}
}

// Находка 12: explain_collection cannot break down a «по дате в чате» сбор: it always uses the operation date and shows a different total
func TestL4_12_ExplainChatDate(t *testing.T) {
	grp := e2eGroup("chat")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	isl := user("79001110002")
	e.sendReceipt(grp, isl, "Ислам", sberCheck("15000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendReceipt(grp, isl, "Ислам", sberCheckOn(time.Now().AddDate(0, 0, -1), "10000.00", "23:50:00"), "Исаев Харон")
	d := time.Now().Format("2006-01-02")
	rep := e.tool("send_finance_report", grp, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","date_basis":"chat"}`)
	ex := e.tool("explain_collection", grp, `{"from_date":"`+d+`","to_date":"`+d+`","date_basis":"chat"}`)
	num := func(re, s string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(s)
		if m == nil {
			return ""
		}
		return strings.ReplaceAll(m[1], " ", "")
	}
	r := num(`Итого за [^:]*: ([\d ]+) ₽`, rep)
	x := num(`Сбор за [^:]*: ([\d ]+) ₽`, ex)
	if r == "" || r != x {
		t.Errorf("сбор по дате в чате %q ₽, а разбор этого сбора показывает %q ₽\n%s\n%s", r, x, rep, ex)
	}
}

// Находка 13: The group report hides a check that is waiting for a name in that group when the same check is counted in another group (explain lists it)
func TestL4_13_GroupReportHidesWaitingCheck(t *testing.T) {
	sb, mg := e2eGroup("sb"), e2eGroup("main")
	e := newE2E(t, e2eOpts{Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	a := sberCheck("15000.00", "10:00:00")
	e.sendReceipt(sb, user("79001110001"), "Сафаи", a, "Магомедов Ахмед")
	e.sendReceipt(mg, user("79001110002"), "Ислам", a, "")
	d := time.Now().Format("2006-01-02")
	rep := e.tool("send_finance_report", sb, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","group":"Оплата клиентов"}`)
	ex := e.tool("explain_collection", sb, `{"from_date":"`+d+`","to_date":"`+d+`","group":"Оплата клиентов"}`)
	t.Logf("сбор основной группы:\n%s\n\nexplain основной группы:\n%s", rep, ex)
	if !strings.Contains(rep, "15000") {
		t.Errorf("в основной группе лежит чек на 15000, ждёт имени (explain его показывает), а отчёт группы молчит: %s", rep)
	}
}

// Находка 20: A text with only a first name («Ахмед 10000») plus the check for the same payment is counted twice
func TestL4_20_FirstNameTextPlusCheck(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Ахмед")
	grp, emp := e2eGroup("fn"), user("79285550001")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Ахмед 10000"))
	got := dayTotals(e, grp)
	if sum(got) != 10000 {
		t.Errorf("чек «Магомедов Ахмед» 10000 + текст «Ахмед 10000» о нём же: сбор %v, ожидали 10000 (%v)", sum(got), got)
	}
}

// Находка 23: One text with the total for a payment split across two checks is counted on top of the checks
func TestL4_23_TotalTextForTwoChecks(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, emp := e2eGroup("tt"), user("79285550005")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "Магомедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "12:01:00"), "Магомедов Ахмед")
	if got := dayTotals(e, grp); sum(got) != 15000 {
		t.Errorf("текст 15000 + два чека 10000 и 5000 того же платежа: сбор %v, ожидали 15000 (%v)", sum(got), got)
	}
}

// Находка 26: The check for a text payment, posted more than 48 hours later (for example after a weekend), is counted on top of the text
func TestL4_26_CheckPostedAfterWeekend(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, emp := e2eGroup("lt"), user("79285550010")
	n := time.Now()
	fri := time.Date(n.Year(), n.Month(), n.Day(), 10, 5, 0, 0, n.Location()).AddDate(0, 0, -3)
	m := groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000")
	m.Info.Timestamp = fri
	e.sendGroup(m)
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(fri, "15000.00", "10:00:00"), "Магомедов Ахмед")
	from := time.Date(fri.Year(), fri.Month(), fri.Day(), 0, 0, 0, 0, fri.Location())
	if got := l4Total(e, []string{grp.String()}, from, from.AddDate(0, 0, 1)); got != 15000 {
		t.Errorf("текст пятницы + чек того же платежа, присланный через 3 дня: сбор за пятницу %v, ожидали 15000", got)
	}
}
