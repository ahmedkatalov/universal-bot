package bot

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии 5-го раунда проверки движка подсчёта (2026-10-18): правила склейки
// не должны сливать разные платежи и считать итог частями сверх своих чеков.

// Находка 0: A split payment (10000+5000 checks plus a «15000» total) reported in SB and in the main group is counted twice: 30000
func TestL5_00_SplitPaymentReportedInTwoGroups(t *testing.T) {
	for _, repostChecks := range []bool{true, false} {
		sb, mg := e2eGroup("zzFsb"), e2eGroup("zzFmain")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf, isl := user("79001110041"), user("79001110042")
		c1, c2 := sberCheck("10000.00", "10:00:00"), sberCheck("5000.00", "10:01:00")
		e.sendReceipt(sb, saf, "Сафаи", c1, "Магомедов Ахмед")
		e.sendReceipt(sb, saf, "Сафаи", c2, "Магомедов Ахмед")
		e.sendGroup(groupText(sb, saf, "Сафаи", "Магомедов Ахмед 15000"))
		if repostChecks {
			e.sendReceipt(mg, isl, "Ислам", c1, "Магомедов Ахмед")
			e.sendReceipt(mg, isl, "Ислам", c2, "Магомедов Ахмед")
		}
		e.sendGroup(groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000"))
		from, to := l4Today()
		all := l4Total(e, nil, from, to)
		gsb := l4Total(e, []string{sb.String()}, from, to)
		gmain := l4Total(e, []string{mg.String()}, from, to)
		d := time.Now().Format("2006-01-02")
		rep := e.tool("send_finance_report", mg, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text"}`)
		t.Logf("чеки и в основной=%v: все группы %v, СБ %v, основная %v\nотчёт:\n%s", repostChecks, all, gsb, gmain, rep)
		if all != 15000 || gmain != 15000 || gsb != 15000 {
			t.Errorf("чеки и в основной=%v: 15000 частями (10000+5000) с итогом в двух группах: все группы %v, СБ %v, основная %v — хочу 15000",
				repostChecks, all, gsb, gmain)
		}
	}
}

// Находка 1: The same VTB check (no doc number) posted in two groups is counted twice when the second group's caption is shorter («Ахмед» / «Магомедов»)
func TestL5_01_SameVtbCheckShortCaptionInSecondGroup(t *testing.T) {
	for _, short := range []string{"Ахмед", "Магомедов"} {
		sb, mg := e2eGroup("zzEsb"), e2eGroup("zzEmain")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				u := strings.TrimSpace(c.lastUserText())
				if strings.Contains(u, "Магомедов Ахмед") {
					return aiReply{Text: `{"client":"Магомедов Ахмед"}`}
				}
				if u == short {
					return aiReply{Text: `{"client":"` + short + `"}`}
				}
				return aiReply{Text: `{"client":""}`}
			}
			if strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: `{"payments":[],"clarify":""}`}
			}
			return aiReply{Text: ""}
		}
		d := time.Now().Format("02.01.2006")
		vtb := "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", 10:02\nИмя плательщика Зарема Б.\nСумма операции 10 000 ₽"
		e.sendReceipt(sb, user("79001110021"), "Сафаи", vtb, "Магомедов Ахмед")
		e.sendReceipt(mg, user("79001110022"), "Ислам", vtb, short)
		from, to := l4Today()
		all := l4Total(e, nil, from, to)
		ss, _ := e.db.SummaryForPeriod(context.Background(), from, to, nil)
		t.Logf("подпись во второй группе %q: сбор по всем группам %v %+v; бот: %q", short, all, ss, e.last().Text)
		if all != 10000 {
			t.Errorf("один чек ВТБ 10000 в двух группах (во второй подписан %q): сбор по всем группам %v", short, all)
		}
	}
}

// Находка 2: A text-only transfer posted the evening before absorbs the next morning's caption-named check; by chat date the check is in neither month's sum
func TestL5_02_EveningTextTakesNextMorningCaptionCheck(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("zzA")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf := user("79001110001")
	sep30 := time.Date(2026, 9, 30, 18, 0, 0, 0, time.Local)
	oct1 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 5000")
	m.Info.Timestamp = sep30
	e.sendGroup(m)
	caption := "Магомедов Ахмед"
	text := caption + "\n" + sberCheckOn(oct1, "5000.00", "08:58:00")
	waID := e2eID("CHK")
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", oct1)
	if err != nil {
		t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
		ID: waID, PushName: "Сафаи", Timestamp: oct1}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, oct1, nil, ".jpg", payer, payerAmount)
	e.waitIdle()
	sepF, octF, novF := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	chat := func(from, to time.Time) float64 {
		ss, err := e.db.SummaryForPeriodByChat(ctx, from, to, from, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		var s float64
		for _, x := range ss {
			s += x.Total
		}
		return s
	}
	sep, oct := l4Total(e, nil, sepF, octF), l4Total(e, nil, octF, novF)
	sepC, octC := chat(sepF, octF), chat(octF, novF)
	repS := e.tool("send_finance_report", grp, `{"from_date":"2026-09-01","to_date":"2026-09-30","format":"text","date_basis":"chat"}`)
	repO := e.tool("send_finance_report", grp, `{"from_date":"2026-10-01","to_date":"2026-10-31","format":"text","date_basis":"chat"}`)
	t.Logf("по дате операции: сентябрь %v, октябрь %v; по дате в чате: сентябрь %v, октябрь %v", sep, oct, sepC, octC)
	t.Logf("сентябрь по дате в чате:\n%s\n\nоктябрь по дате в чате:\n%s", repS, repO)
	if sep != 5000 || oct != 5000 {
		t.Errorf("по дате операции: сентябрь %v (хочу 5000 — перевод 30.09), октябрь %v (хочу 5000 — чек 01.10)", sep, oct)
	}
	if octC == 0 && sepC == 0 {
		t.Errorf("по дате в чате чек 01.10 (прислан 01.10) не в сумме ни сентября, ни октября")
	}
}

// Находка 3: A full-name text 35 hours later takes the check from a short-name text written 2 minutes after it: day 1 doubled under a phantom client «Ахмед», day 2 zero
func TestL5_03_FullNameNextDayBeatsShortNameNow(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("zzC")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	names := []string{"Магомедов Ахмед", "Ахмед"}
	has := func(s, n string) bool {
		for _, w := range strings.Fields(n) {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	}
	e.ai = func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			for _, n := range names {
				if has(u, n) {
					return aiReply{Text: `{"client":"` + n + `"}`}
				}
			}
			return aiReply{Text: `{"client":""}`}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			if i := strings.Index(u, "Строки:\n"); i >= 0 {
				u = u[i+len("Строки:\n"):]
			}
			if i := strings.Index(u, "\n\nЧисла-суммы"); i >= 0 {
				u = u[:i]
			}
			var ps []string
			for _, ln := range strings.Split(u, "\n") {
				for _, n := range names {
					if !has(ln, n) {
						continue
					}
					var amt float64
					for _, f := range strings.Fields(ln) {
						if v := parseAmountWord(f); v > 0 {
							amt = v
						}
					}
					if amt > 0 {
						ps = append(ps, fmt.Sprintf(`{"name":%q,"amount":%v,"cash":false}`, n, amt))
					}
					break
				}
			}
			return aiReply{Text: `{"payments":[` + strings.Join(ps, ",") + `],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	saf := user("79001110011")
	d1 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 10, 15, 21, 0, 0, 0, time.Local)
	caption := "Магомедов Ахмед"
	text := caption + "\n" + sberCheckOn(d1, "10000.00", "09:59:00")
	waID := e2eID("CHK")
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", d1)
	if err != nil {
		t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
		ID: waID, PushName: "Сафаи", Timestamp: d1}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, d1, nil, ".jpg", payer, payerAmount)
	e.waitIdle()
	m1 := groupText(grp, saf, "Сафаи", "Ахмед 10000")
	m1.Info.Timestamp = d1.Add(2 * time.Minute)
	e.sendGroup(m1)
	m2 := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 10000")
	m2.Info.Timestamp = d2
	e.sendGroup(m2)
	day1 := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
	day2 := day1.AddDate(0, 0, 1)
	t1, t2 := l4Total(e, nil, day1, day2), l4Total(e, nil, day2, day2.AddDate(0, 0, 1))
	ss, _ := e.db.SummaryForPeriod(ctx, day1, day2, nil)
	t.Logf("14.10: %v %+v; 15.10: %v", t1, ss, t2)
	if t1 != 10000 || t2 != 10000 {
		t.Errorf("14.10 = %v (хочу 10000: чек и «Ахмед 10000» о нём), 15.10 = %v (хочу 10000: новый перевод)", t1, t2)
	}
}

// Находка 4: An unnamed check of another client (1 minute later, other group) is attached to a counted check: card totals undercount, «НЕ вошло» hides it, and the other group's сбор shows a foreign payment
func TestL5_04_UnnamedCheckJoinsOtherClientsCheck(t *testing.T) {
	ctx := context.Background()
	ga, gb := e2eGroup("zzDa"), e2eGroup("zzDb")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{ga.String(): "Магазин 1", gb.String(): "Магазин 2"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	d := time.Now().Format("02.01.2006")
	vtb := func(hm string) string {
		return "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", " + hm +
			"\nИмя плательщика Зарема Б.\nСумма операции 5 000 ₽"
	}
	e.sendReceipt(ga, user("79001110071"), "Сафаи", vtb("13:02"), "Магомедов Ахмед")
	e.sendReceipt(gb, user("79001110072"), "Ислам", vtb("13:03"), "")
	from, to := l4Today()
	cards, _ := e.db.CardTotals(ctx, from, to, nil)
	var ct float64
	for _, c := range cards {
		ct += c.Total
	}
	exB, _ := e.db.ReportExclusions(ctx, from, to, []string{gb.String()})
	b := l4Total(e, []string{gb.String()}, from, to)
	t.Logf("по картам %v; магазин 2: сбор %v, НЕ вошло %d шт на %v", ct, b, exB.NeedsReviewCount, exB.NeedsReviewSum)
	if ct != 10000 || exB.NeedsReviewSum != 5000 || b != 0 {
		t.Errorf("два разных чека (13:02 и 13:03): по картам %v (хочу 10000); магазин 2: сбор %v (хочу 0), ждёт имени %v (хочу 5000)", ct, b, exB.NeedsReviewSum)
	}
}

// Находка 5: A payment split into two checks counts its total text again when one part is missing: the сбор shows 25000 instead of 15000, and an exclusion or deletion makes it grow
func TestL5_05_SplitTotalCountedWhenPartMissing(t *testing.T) {
	ctx := context.Background()
	setup := func(tag, caption2 string) (*e2eBot, types.JID, types.JID, string) {
		grp, emp := e2eGroup(tag), user("79001119101")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "10:01:00"), caption2)
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000"))
		return e, grp, emp, ck
	}
	e, grp, _, _ := setup("zz5s1", "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	d := time.Now().Format("2006-01-02")
	rep := e.tool("send_finance_report", grp, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text"}`)
	t.Logf("(а) второй чек ждёт имени:\n%s", rep)
	if got := sum(dayTotals(e, grp)); got > 15000 {
		t.Errorf("(а) клиент заплатил 15000 (10000+5000, итог текстом), второй чек ждёт имени — сбор %v", got)
	}
	e, grp, _, _ = setup("zz5s2", "Магомедов Ахмед")
	if got := sum(dayTotals(e, grp)); got != 15000 {
		t.Fatalf("предусловие: сплит = 15000, сбор %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов Ахмед","kind":"check"}`)
	got := sum(dayTotals(e, grp))
	t.Logf("(б) correct_operation: %s → сбор %v", out, got)
	if got > 15000 {
		t.Errorf("(б) исключили чек 5000 из платежа 10000+5000 — сбор вырос с 15000 до %v", got)
	}
	e, grp, emp, ck := setup("zz5s3", "Магомедов Ахмед")
	e.event(l4textsRevoke(grp, emp, ck))
	got = sum(dayTotals(e, grp))
	t.Logf("(в) после удаления чека 5000: сбор %v; бот: %q", got, e.last().Text)
	if got > 15000 {
		t.Errorf("(в) удалили чек 5000 из платежа 10000+5000 — сбор вырос с 15000 до %v", got)
	}
}

// Находка 7: «По дате в чате»: a transfer text plus its check posted days later is counted on both days, while the report for the whole period counts it once
func TestL5_07_ByChatLateCheckCountedTwice(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zz5lc"), user("79001119103")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	d0 := time.Date(2026, 9, 10, 10, 5, 0, 0, time.Local)
	m := groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000")
	m.Info.Timestamp = d0
	e.sendGroup(m)
	late := time.Date(2026, 9, 16, 9, 0, 0, 0, time.Local)
	text := "Магомедов Ахмед\n" + sberCheckOn(d0, "15000.00", "10:00:00")
	wa := e2eID("CHK")
	raw, _, err := e.db.SaveRawMessage(ctx, wa, grp.String(), emp.String(), emp.User, "Сафаи", text, true, "", late)
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, grp, emp.String(), wa, text, raw, late, nil, ".jpg", "Магомедов Ахмед", 0)
	e.waitIdle()
	total := func(from, to string) string {
		out := e.tool("send_finance_report", grp, `{"from_date":"`+from+`","to_date":"`+to+`","format":"text","date_basis":"chat"}`)
		m := regexp.MustCompile(`Итого за [^:]*: (\d+) ₽`).FindStringSubmatch(out)
		if m == nil {
			return "0"
		}
		return m[1]
	}
	day10, day16, period := total("2026-09-10", "2026-09-10"), total("2026-09-16", "2026-09-16"), total("2026-09-10", "2026-09-16")
	t.Logf("по дате в чате: 10.09 = %s, 16.09 = %s, 10.09–16.09 = %s; по дате операции сентябрь = %v", day10, day16, period,
		l4Total(e, nil, time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)))
	if period != "15000" || (day10 != "0" && day16 != "0") {
		t.Errorf("один платёж 15000: по дате в чате 10.09 = %s и 16.09 = %s (оба дня считают его), а за период = %s", day10, day16, period)
	}
}

// Находка 8: «По дате в чате» at a month boundary: a text from 30.09 plus its check posted on 01.10 is counted in neither September nor October, and September lists it as an old check posted in September
func TestL5_08_ByChatMonthEdgeLost(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zz5me"), user("79001119104")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	m := groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000")
	m.Info.Timestamp = time.Date(2026, 9, 30, 21, 0, 0, 0, time.Local)
	e.sendGroup(m)
	oct1 := time.Date(2026, 10, 1, 9, 59, 0, 0, time.Local)
	text := "Магомедов Ахмед\n" + sberCheckOn(oct1, "15000.00", "09:58:00")
	wa := e2eID("CHK")
	raw, _, err := e.db.SaveRawMessage(ctx, wa, grp.String(), emp.String(), emp.User, "Сафаи", text, true, "", oct1)
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, grp, emp.String(), wa, text, raw, oct1, nil, ".jpg", "Магомедов Ахмед", 0)
	e.waitIdle()
	sep := e.tool("send_finance_report", grp, `{"from_date":"2026-09-01","to_date":"2026-09-30","format":"text","date_basis":"chat"}`)
	oct := e.tool("send_finance_report", grp, `{"from_date":"2026-10-01","to_date":"2026-10-31","format":"text","date_basis":"chat"}`)
	opOct := l4Total(e, nil, time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local))
	t.Logf("по дате операции октябрь = %v\nСЕНТЯБРЬ по дате в чате:\n%s\n\nОКТЯБРЬ по дате в чате:\n%s", opOct, sep, oct)
	if !strings.Contains(sep, "Итого за") && !strings.Contains(oct, "Итого за") {
		t.Errorf("платёж 15000 (текст 30.09, чек прислан 01.10) не вошёл в сбор по дате в чате ни за сентябрь, ни за октябрь")
	}
}

// Находка 9: Forwarding rule: a check copied by the bot and still waiting for «чей чек?» is missing from the target group's сбор, «НЕ вошло» and cards_report, but explain_collection for the same group lists it
func TestL5_09_ForwardedWaitingCopyHiddenInTarget(t *testing.T) {
	ctx := context.Background()
	src, dst := e2eGroup("zz5fs"), e2eGroup("zz5fd")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	ocr := sberCheck("8000", "15:00:00")
	w := e.sendReceipt(src, user("79001119105"), "Сафаи", ocr, "")
	e.applyForwardRules(ctx, src.String(), "Сафаи", w, []byte("jpeg"), ".jpg", ocr, time.Now())
	e.waitIdle()
	d := time.Now().Format("2006-01-02")
	rep := e.tool("send_finance_report", dst, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","group":"Оплата КЛНТ"}`)
	cards := e.tool("cards_report", dst, `{"from_date":"`+d+`","to_date":"`+d+`","group":"Оплата КЛНТ"}`)
	ex := e.tool("explain_collection", dst, `{"from_date":"`+d+`","to_date":"`+d+`","group":"Оплата КЛНТ"}`)
	t.Logf("сбор «Оплата КЛНТ»:\n%s\n\nкарты:\n%s\n\nexplain:\n%s", rep, cards, ex)
	if !strings.Contains(rep, "8000") || !strings.Contains(cards, "8000") {
		t.Errorf("в «Оплата КЛНТ» лежит копия чека на 8000 (ждёт имени), explain её показывает, а сбор и карты группы молчат")
	}
}
