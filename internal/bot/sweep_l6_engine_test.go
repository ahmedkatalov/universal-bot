package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии проверки 5-го раунда (движок): итог частями без ложных склеек и
// независимо от порядка групп, «по дате в чате» на границе месяца, чек с одной датой.

// Находка 27: A text-only payment «Магомедов Ахмед 15000» is silently dropped from the сбор because the new «missing part» rule pairs it with ANOTHER employee's uncaptioned check, even a deleted one
func TestL6_27_1_TextOnlyPaymentEatenByForeignCheck(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		grp := e2eGroup("r5x1")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf, isl := user("79001115361"), user("79001115362")
		day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
		at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
		sber := func(amount, hms, payer string) string {
			return "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " " + hms +
				"\nФИО получателя\nХадижат Ибрагимовна И.\nФИО отправителя\n" + payer + "\nСумма перевода\n" + amount + " ₽\nНомер операции в СБП\n" + e2eID("OP")
		}
		checkAt := func(snd types.JID, push, ocr, caption string, when time.Time) string {
			ctx := context.Background()
			text := ocr
			if caption != "" {
				text = caption + "\n" + ocr
			}
			waID := e2eID("CHK")
			rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), snd.String(), snd.User, push, text, true, "", when)
			if err != nil {
				t.Fatal(err)
			}
			msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true},
				ID: waID, PushName: push, Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
			payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
			e.handleBankReceipt(ctx, grp, snd.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
			e.waitIdle()
			return waID
		}
		checkAt(saf, "Сафаи", sber("10000.00", "10:00:00", "Зарема Б."), "Магомедов Ахмед", at(10, 1))
		other := checkAt(isl, "Ислам", sber("5000.00", "10:39:00", "Патимат К."), "", at(10, 40))
		if deleted {
			e.event(l4textsRevoke(grp, isl, other)) // Ислам ошибся группой и удалил свой чек
		}
		n := len(e.sent())
		m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 15000")
		m.Info.Timestamp = at(11, 30)
		e.sendGroup(m)
		from, to := day, day.AddDate(0, 0, 1)
		got := l4Total(e, nil, from, to)
		ex := e.tool("explain_collection", grp, `{"from_date":"2026-10-14","to_date":"2026-10-14"}`)
		t.Logf("чужой чек удалён=%v: сбор %v; бот после текста: %+v\n%s", deleted, got, e.sentSince(n), ex)
		if got != 25000 {
			t.Errorf("чужой чек удалён=%v: Магомедов — чек 10000 + перевод текстом 15000, чек Ислама 5000 (другой плательщик, без подписи) — сбор %v, хочу 25000",
				deleted, got)
		}
	}
}

// Находка 28: A date-only check (parser fills in 12:00) plus a morning «ФИО сумма» text is ONE payment if the check is posted at 11:30 but TWO payments (30000) if posted at 13:00
func TestL6_28_2_DateOnlyCheckMorningTextDependsOnPostingHour(t *testing.T) {
	for _, post := range []time.Duration{11*time.Hour + 30*time.Minute, 13 * time.Hour} {
		grp := e2eGroup("r5x2")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf := user("79001115311")
		day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
		m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 15000")
		m.Info.Timestamp = day.Add(8*time.Hour + 40*time.Minute)
		e.sendGroup(m)
		ctx := context.Background()
		caption := "Магомедов Ахмед"
		ocr := "Почта Банк\nКвитанция о переводе\nДата операции: 14.10.2026\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n15 000 ₽\nНомер операции\nP" + strings.Trim(e2eID(""), "-")[:10]
		text := caption + "\n" + ocr
		when := day.Add(post)
		waID := e2eID("CHK")
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", when)
		if err != nil {
			t.Fatal(err)
		}
		msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
			ID: waID, PushName: "Сафаи", Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
		payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
		e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
		e.waitIdle()
		got := l4Total(e, nil, day, day.AddDate(0, 0, 1))
		t.Logf("чек прислан в %s: сбор 14.10 = %v", when.Format("15:04"), got)
		if got != 15000 {
			t.Errorf("текст «Магомедов Ахмед 15000» в 08:40 и чек этого перевода (дата без времени), присланный в %s: сбор %v, хочу 15000",
				when.Format("15:04"), got)
		}
	}
}

// Находка 29: A split payment's total is still counted twice (30000) when the main group's total is written BEFORE the SB total, or when only the main group has the total
func TestL6_29_3_SplitTotalInMainBeforeSB(t *testing.T) {
	for _, sbTotal := range []bool{true, false} {
		sb, mg := e2eGroup("r5x3sb"), e2eGroup("r5x3main")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf, isl := user("79001115301"), user("79001115302")
		day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
		at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
		checkAt := func(ocr string, when time.Time) {
			ctx := context.Background()
			caption := "Магомедов Ахмед"
			text := caption + "\n" + ocr
			waID := e2eID("CHK")
			rawID, _, err := e.db.SaveRawMessage(ctx, waID, sb.String(), saf.String(), saf.User, "Сафаи", text, true, "", when)
			if err != nil {
				t.Fatal(err)
			}
			msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: sb, Sender: saf, IsGroup: true},
				ID: waID, PushName: "Сафаи", Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
			payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
			e.handleBankReceipt(ctx, sb, saf.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
			e.waitIdle()
		}
		textAt := func(grp, snd types.JID, push string, when time.Time) {
			m := groupText(grp, snd, push, "Магомедов Ахмед 15000")
			m.Info.Timestamp = when
			e.sendGroup(m)
		}
		checkAt(sberCheckOn(day, "10000.00", "10:00:00"), at(10, 1))
		checkAt(sberCheckOn(day, "5000.00", "10:01:00"), at(10, 2))
		textAt(mg, isl, "Ислам", at(10, 5))
		if sbTotal {
			textAt(sb, saf, "Сафаи", at(10, 7))
		}
		from, to := day, day.AddDate(0, 0, 1)
		all := l4Total(e, nil, from, to)
		gsb := l4Total(e, []string{sb.String()}, from, to)
		gmain := l4Total(e, []string{mg.String()}, from, to)
		t.Logf("итог и в СБ=%v: все группы %v, СБ %v, основная %v", sbTotal, all, gsb, gmain)
		if all != 15000 || gsb != 15000 || gmain != 15000 {
			t.Errorf("итог и в СБ=%v: 15000 частями (10000+5000), итог в основной написан первым: все группы %v, СБ %v, основная %v — хочу 15000",
				sbTotal, all, gsb, gmain)
		}
	}
}

// Находка 30: «По дате в чате»: a text posted 30.09 whose check arrives on 01.10 (even at 00:03) is in neither month's sum, and September claims the text is «уже в сумме»
func TestL6_30_4_ByChatTextSeptCheckPostedOct(t *testing.T) {
	cases := []struct{ text, op, posted time.Time }{
		{time.Date(2026, 9, 30, 23, 58, 0, 0, time.Local), time.Date(2026, 9, 30, 23, 57, 0, 0, time.Local), time.Date(2026, 10, 1, 0, 3, 0, 0, time.Local)},
		{time.Date(2026, 9, 30, 10, 5, 0, 0, time.Local), time.Date(2026, 9, 30, 10, 0, 0, 0, time.Local), time.Date(2026, 10, 2, 11, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		grp := e2eGroup("r5x4")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf := user("79001115321")
		m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 15000")
		m.Info.Timestamp = c.text
		e.sendGroup(m)
		ctx := context.Background()
		caption := "Магомедов Ахмед"
		text := caption + "\n" + sberCheckOn(c.op, "15000.00", c.op.Format("15:04:05"))
		waID := e2eID("CHK")
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", c.posted)
		if err != nil {
			t.Fatal(err)
		}
		msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
			ID: waID, PushName: "Сафаи", Timestamp: c.posted}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
		payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
		e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, c.posted, nil, ".jpg", payer, amt)
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
		sepC, octC := chat(sepF, octF), chat(octF, novF)
		repS := e.tool("send_finance_report", grp, `{"from_date":"2026-09-01","to_date":"2026-09-30","format":"text","date_basis":"chat"}`)
		repO := e.tool("send_finance_report", grp, `{"from_date":"2026-10-01","to_date":"2026-10-31","format":"text","date_basis":"chat"}`)
		t.Logf("текст %s, чек прислан %s: по дате операции сентябрь %v; по дате в чате сентябрь %v, октябрь %v\nСЕНТЯБРЬ:\n%s\nОКТЯБРЬ:\n%s",
			c.text.Format("02.01 15:04"), c.posted.Format("02.01 15:04"), l4Total(e, nil, sepF, octF), sepC, octC, repS, repO)
		if sepC+octC != 15000 {
			t.Errorf("текст %s, чек прислан %s: по дате в чате сентябрь %v + октябрь %v — платёж 15000 не в сумме ни одного месяца",
				c.text.Format("02.01 15:04"), c.posted.Format("02.01 15:04"), sepC, octC)
		}
	}
}

// Находка 31: A split payment whose second check is captioned with the surname only («Магомедов») is counted twice: 30000, including a phantom client «Магомедов»
func TestL6_31_5_SplitSecondPartShortCaption(t *testing.T) {
	grp := e2eGroup("r5x5")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	pay := aiNamesAndPayments("Магомедов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			// Как настоящая модель: имя — как написано.
			u := strings.TrimSpace(c.lastUserText())
			switch {
			case strings.Contains(u, "Магомедов Ахмед"):
				return aiReply{Text: `{"client":"Магомедов Ахмед"}`}
			case u == "Магомедов":
				return aiReply{Text: `{"client":"Магомедов"}`}
			}
			return aiReply{Text: `{"client":""}`}
		}
		return pay(c)
	}
	saf := user("79001115371")
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	checkAt := func(ocr, caption string, when time.Time) {
		ctx := context.Background()
		text := caption + "\n" + ocr
		waID := e2eID("CHK")
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", when)
		if err != nil {
			t.Fatal(err)
		}
		msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
			ID: waID, PushName: "Сафаи", Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
		payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
		e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
		e.waitIdle()
	}
	checkAt(sberCheckOn(day, "10000.00", "10:00:00"), "Магомедов Ахмед", at(10, 1))
	checkAt(sberCheckOn(day, "5000.00", "10:01:00"), "Магомедов", at(10, 2))
	m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 15000")
	m.Info.Timestamp = at(10, 4)
	e.sendGroup(m)
	from, to := day, day.AddDate(0, 0, 1)
	got := l4Total(e, nil, from, to)
	ss, _ := e.db.SummaryForPeriod(context.Background(), from, to, nil)
	t.Logf("сбор %v %+v", got, ss)
	if got != 15000 {
		t.Errorf("15000 частями: чек 10000 «Магомедов Ахмед», чек 5000 «Магомедов», итог «Магомедов Ахмед 15000» — сбор %v, хочу 15000", got)
	}
}

// Находка 32: Split total with a waiting part breaks again in the SB→main workflow: when both checks are in main (reposted manually or by the bot's forwarding rule) and the total is written in main, the сбор is 25000
func TestL6_32_6_WaitingPartRepostedTotalInMain(t *testing.T) {
	for _, botForward := range []bool{false, true} {
		ctx := context.Background()
		sb, mg := e2eGroup("r5x6sb"), e2eGroup("r5x6main")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		if botForward {
			_ = e.db.SetForwardRule(ctx, sb.String(), "СБ оплата", mg.String(), "Оплата клиентов")
		}
		saf, isl := user("79001115331"), user("79001115332")
		day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
		at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
		checkAt := func(grp, snd types.JID, push, ocr, caption string, when time.Time) {
			text := ocr
			if caption != "" {
				text = caption + "\n" + ocr
			}
			waID := e2eID("CHK")
			rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), snd.String(), snd.User, push, text, true, "", when)
			if err != nil {
				t.Fatal(err)
			}
			msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true},
				ID: waID, PushName: push, Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
			payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
			e.handleBankReceipt(ctx, grp, snd.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
			e.waitIdle()
			if botForward {
				e.applyForwardRules(ctx, grp.String(), push, waID, []byte("jpeg"), ".jpg", text, when)
				e.waitIdle()
			}
		}
		c1, c2 := sberCheckOn(day, "10000.00", "10:00:00"), sberCheckOn(day, "5000.00", "10:01:00")
		checkAt(sb, saf, "Сафаи", c1, "Магомедов Ахмед", at(10, 1))
		checkAt(sb, saf, "Сафаи", c2, "", at(10, 2))
		if !botForward {
			checkAt(mg, isl, "Ислам", c1, "Магомедов Ахмед", at(10, 10))
			checkAt(mg, isl, "Ислам", c2, "", at(10, 11))
		}
		m := groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000")
		m.Info.Timestamp = at(10, 12)
		e.sendGroup(m)
		from, to := day, day.AddDate(0, 0, 1)
		all := l4Total(e, nil, from, to)
		gmain := l4Total(e, []string{mg.String()}, from, to)
		ex, _ := e.db.ReportExclusions(ctx, from, to, nil)
		t.Logf("пересылка ботом=%v: все группы %v, основная %v; НЕ вошло %d шт на %v", botForward, all, gmain, ex.NeedsReviewCount, ex.NeedsReviewSum)
		if all > 15000 || gmain > 15000 {
			t.Errorf("пересылка ботом=%v: 15000 частями (10000 + 5000 ждёт имени) в СБ и основной, итог в основной: все группы %v, основная %v — больше 15000",
				botForward, all, gmain)
		}
	}
}

// Находка 22: A split total in a group without its checks counts as a separate payment (30000) unless the same total reached the checks' group first
func TestL6_22_SplitTotalInGroupWithoutChecks(t *testing.T) {
	for _, withSB := range []bool{false, true} {
		sb, mg := e2eGroup("zzr5jsb"), e2eGroup("zzr5jmain")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf, isl := user("79001119192"), user("79001119193")
		e.sendReceipt(sb, saf, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
		e.sendReceipt(sb, saf, "Сафаи", sberCheck("5000.00", "10:01:00"), "Магомедов Ахмед")
		e.sendGroup(groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000"))
		if withSB {
			e.sendGroup(groupText(sb, saf, "Сафаи", "Магомедов Ахмед 15000"))
		}
		from, to := l4Today()
		all := l4Total(e, nil, from, to)
		gsb := l4Total(e, []string{sb.String()}, from, to)
		gm := l4Total(e, []string{mg.String()}, from, to)
		t.Logf("итог в СБ тоже (позже): %v → все %v, СБ %v, основная %v", withSB, all, gsb, gm)
		if all != 15000 {
			t.Errorf("чеки 10000 + 5000 в СБ, итог 15000 в основной (и в СБ позже: %v): все группы %v, хочу 15000", withSB, all)
		}
	}
}
