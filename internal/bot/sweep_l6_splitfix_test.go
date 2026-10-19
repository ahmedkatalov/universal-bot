package bot

import (
	"strings"
	"testing"

	"whatsapp-bot/internal/ledger"

	"go.mau.fi/whatsmeow/types"
)

// Регрессии L6 (платёж частями: чеки 10000 + 5000 и итог текстом
// «Магомедов Ахмед 15000»): откат поправки части, исключение только текста-итога
// при таком же итоге в другой группе, возврат исключённого платежа целиком,
// правка итога в WhatsApp.

// l6splitfixTwoGroups — чеки 10000 и 5000 и итог в «СБ оплата», такой же итог
// от другого сотрудника в «Оплата клиентов».
func l6splitfixTwoGroups(t *testing.T, tag string) (e *e2eBot, sb, mg types.JID, c1, c2 string) {
	sb, mg = e2eGroup(tag+"sb"), e2eGroup(tag+"main")
	e = newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	saf, isl := user("79001119212"), user("79001119213")
	c1 = e.sendReceipt(sb, saf, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
	c2 = e.sendReceipt(sb, saf, "Сафаи", sberCheck("5000.00", "10:01:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(sb, saf, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendGroup(groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000"))
	return
}

// l6splitfixTotals — сбор за сегодня: все группы, СБ, основная.
func l6splitfixTotals(e *e2eBot, sb, mg types.JID) (all, gsb, gmain float64) {
	from, to := l4Today()
	return l4Total(e, nil, from, to), l4Total(e, []string{sb.String()}, from, to), l4Total(e, []string{mg.String()}, from, to)
}

// Находка 21: поправили обе части («не 10000 а 11000», «не 5000 а 6000») —
// итог 17000. Первую поправку удалили или изменили: итог следует за чеками
// (16000 / 18000), а не остаётся 17000 и не считается сверх чеков.
func TestL6splitfix_21_UndoFirstOfTwoPartCorrections(t *testing.T) {
	for _, c := range []struct {
		tag       string
		edit      bool
		want1     float64
		wantTotal float64
	}{
		{"l6sf21del", false, 10000, 16000},
		{"l6sf21edit", true, 12000, 18000},
	} {
		s := l5splitfixSetup(t, c.tag, "Магомедов Ахмед")
		e := s.e
		m1 := groupReply(s.grp, s.emp, "Сафаи", "не 10000 а 11000", s.c1, s.emp, "")
		e.sendGroup(m1)
		e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 5000 а 6000", s.c2, s.emp, ""))
		if got := sum(dayTotals(e, s.grp)); got != 17000 {
			t.Fatalf("%s предусловие: после поправок обеих частей сбор %v, хочу 17000", c.tag, got)
		}
		n := len(e.sent())
		what := "удалили"
		if c.edit {
			what = "изменили на «не 10000 а 12000»"
			e.event(editOf(s.grp, s.emp, m1.Info.ID, "не 10000 а 12000"))
		} else {
			e.event(l4textsRevoke(s.grp, s.emp, m1.Info.ID))
		}
		got := sum(dayTotals(e, s.grp))
		a1, _ := l5splitfixCheck(e, s.c1)
		a2, _ := l5splitfixCheck(e, s.c2)
		ta, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("первую поправку %s: бот: %s; сбор %v; чеки %v + %v; итог %v (%q)", what, l4correctSent(e, n), got, a1, a2, ta, why)
		if a1 != c.want1 || a2 != 6000 {
			t.Errorf("первую поправку %s: чеки %v + %v, хочу %v + 6000", what, a1, a2, c.want1)
		}
		if got != a1+a2 || ta != c.wantTotal || why != ledger.ReasonSplit {
			t.Errorf("первую поправку %s: чеки %v + %v, сбор %v, итог %v (%q) — хочу сбор %v, итог %v (итог частей)",
				what, a1, a2, got, ta, why, a1+a2, c.wantTotal)
		}
	}

	// Обе поправки удалили (в любом порядке) — чеки и итог как были.
	for _, order := range []string{"first", "second"} {
		s := l5splitfixSetup(t, "l6sf21both"+order, "Магомедов Ахмед")
		e := s.e
		m1 := groupReply(s.grp, s.emp, "Сафаи", "не 10000 а 11000", s.c1, s.emp, "")
		m2 := groupReply(s.grp, s.emp, "Сафаи", "не 5000 а 6000", s.c2, s.emp, "")
		e.sendGroup(m1)
		e.sendGroup(m2)
		first, second := m1, m2
		if order == "second" {
			first, second = m2, m1
		}
		e.event(l4textsRevoke(s.grp, s.emp, first.Info.ID))
		e.event(l4textsRevoke(s.grp, s.emp, second.Info.ID))
		got := sum(dayTotals(e, s.grp))
		a1, _ := l5splitfixCheck(e, s.c1)
		a2, _ := l5splitfixCheck(e, s.c2)
		ta, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("удалили обе поправки (%s первой): сбор %v; чеки %v + %v; итог %v (%q)", order, got, a1, a2, ta, why)
		if a1 != 10000 || a2 != 5000 || ta != 15000 || why != ledger.ReasonSplit || got != 15000 {
			t.Errorf("удалили обе поправки (%s первой): чеки %v + %v, итог %v (%q), сбор %v — хочу 10000 + 5000, итог 15000, сбор 15000",
				order, a1, a2, ta, why, got)
		}
	}

	// Поправили ту же часть дважды («не 10000 а 11000», потом «не 11000 а
	// 12000») и удалили первую: чек остаётся 12000 (вторая поправка — последняя
	// правда), итог — 17000 вместе с ним.
	s := l5splitfixSetup(t, "l6sf21same", "Магомедов Ахмед")
	e := s.e
	m1 := groupReply(s.grp, s.emp, "Сафаи", "не 10000 а 11000", s.c1, s.emp, "")
	e.sendGroup(m1)
	e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 11000 а 12000", s.c1, s.emp, ""))
	if got := sum(dayTotals(e, s.grp)); got != 17000 {
		t.Fatalf("предусловие: чек 10000 поправлен дважды до 12000 — сбор %v, хочу 17000", got)
	}
	e.event(l4textsRevoke(s.grp, s.emp, m1.Info.ID))
	got := sum(dayTotals(e, s.grp))
	a1, _ := l5splitfixCheck(e, s.c1)
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("та же часть дважды, первую удалили: сбор %v; чек %v; итог %v (%q)", got, a1, ta, why)
	if a1 != 12000 || ta != 17000 || why != ledger.ReasonSplit || got != 17000 {
		t.Errorf("та же часть дважды, первую поправку удалили: чек %v, итог %v (%q), сбор %v — хочу чек 12000, итог 17000, сбор 17000", a1, ta, why, got)
	}
}

// Находка 23: в СБ исключили только текст-итог (kind=text), а такой же итог в
// основной группе связан с чеками только через него: сбор по всем группам не
// должен вырасти (ответ обещает «сбор не изменился»).
func TestL6splitfix_23_ExcludeTotalTextOnlyKeepsOtherGroup(t *testing.T) {
	e, sb, mg, _, _ := l6splitfixTwoGroups(t, "l6sf23")
	if all, gsb, gmain := l6splitfixTotals(e, sb, mg); all != 15000 || gsb != 15000 || gmain != 15000 {
		t.Fatalf("предусловие: все %v, СБ %v, основная %v — хочу 15000 везде", all, gsb, gmain)
	}
	out := e.tool("correct_operation", sb, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед","kind":"text"}`)
	all, gsb, gmain := l6splitfixTotals(e, sb, mg)
	t.Logf("%s → все %v, СБ %v, основная %v", out, all, gsb, gmain)
	if all != 15000 || gsb != 15000 || gmain != 15000 {
		t.Errorf("в СБ исключили только текст-итог: все %v, СБ %v, основная %v — хочу 15000 везде (ответ: %s)", all, gsb, gmain, out)
	}
	if strings.Contains(out, "сбор не изменился") && strings.Contains(out, "Операция исключена") && all != 15000 {
		t.Errorf("ответ обещает «сбор не изменился», а сбор по всем группам %v", all)
	}

	// Итог только в одной группе — исключение текста работает как прежде.
	s := l5splitfixSetup(t, "l6sf23one", "Магомедов Ахмед")
	out = s.e.tool("correct_operation", s.grp, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед","kind":"text"}`)
	got := sum(dayTotals(s.e, s.grp))
	if _, why := l5splitfixText(s.e, s.total.Info.ID); why != l5splitfixTextGone || got != 15000 {
		t.Errorf("итог в одной группе, kind=text: итог %q, сбор %v — хочу исключён, сбор 15000 (%s)", why, got, out)
	}
	l5splitfixWant(t, "итог в одной группе", out, "Операция исключена", "сбор не изменился")
}

// Находка 24: весь платёж частями исключили из лички (оба чека, итоги в обеих
// группах), потом «верни платёж Магомедова 15000»: возвращается весь платёж
// как был — чеки 10000 + 5000 в сборе, итоги не считаются сверх них.
func TestL6splitfix_24_RestoreWholeSplitPayment(t *testing.T) {
	e, sb, mg, c1, c2 := l6splitfixTwoGroups(t, "l6sf24")
	out := l5splitfixDMTool(e, "correct_operation", `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	all, _, _ := l6splitfixTotals(e, sb, mg)
	t.Logf("исключили: %s → все %v", out, all)
	if all != 0 {
		t.Fatalf("предусловие: весь платёж исключён — все группы %v", all)
	}
	out = l5splitfixDMTool(e, "correct_operation", `{"action":"restore","amount":15000,"person":"Магомедов Ахмед"}`)
	t.Logf("«верни платёж Магомедова 15000»: %s", out)
	if strings.Contains(out, "каждая строка — отдельный платёж") {
		t.Errorf("два итога одного платежа показаны как отдельные платежи: %s", out)
	}
	// Модель могла понять ответ «оба» как два возврата по группам.
	for _, g := range []string{"СБ оплата", "Оплата клиентов"} {
		o := l5splitfixDMTool(e, "correct_operation", `{"action":"restore","amount":15000,"person":"Магомедов Ахмед","group":"`+g+`"}`)
		t.Logf("верни в %s: %s", g, o)
	}
	all, gsb, gmain := l6splitfixTotals(e, sb, mg)
	_, why1 := l5splitfixCheck(e, c1)
	_, why2 := l5splitfixCheck(e, c2)
	t.Logf("после возврата: все %v, СБ %v, основная %v; чек 10000 %q, чек 5000 %q", all, gsb, gmain, why1, why2)
	if all != 15000 || gsb != 15000 || gmain != 15000 {
		t.Errorf("исключили и вернули платёж 15000 (10000 + 5000): все %v, СБ %v, основная %v — хочу 15000 везде", all, gsb, gmain)
	}
	if why1 != "" || why2 != "" {
		t.Errorf("вернули платёж частями, а чеки не в сборе: 10000 %q, 5000 %q", why1, why2)
	}
	l5splitfixWant(t, "возврат платежа частями", out, "возвращена", "10 000", "5 000")

	// Возврат из группы СБ (без общего возврата) — тоже весь платёж.
	e, sb, mg, c1, _ = l6splitfixTwoGroups(t, "l6sf24g")
	l5splitfixDMTool(e, "correct_operation", `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	out = e.tool("correct_operation", sb, `{"action":"restore","amount":15000,"person":"Магомедов Ахмед"}`)
	all, gsb, gmain = l6splitfixTotals(e, sb, mg)
	_, why1 = l5splitfixCheck(e, c1)
	t.Logf("возврат в СБ: %s → все %v, СБ %v, основная %v; чек 10000 %q", out, all, gsb, gmain, why1)
	if all != 15000 || gsb != 15000 || why1 != "" {
		t.Errorf("возврат платежа частями в СБ: все %v, СБ %v, чек 10000 %q — хочу 15000, чек в сборе", all, gsb, why1)
	}

	// Обычный платёж: чек и текст про него исключены вместе — «верни чек»
	// возвращает и текст; сбор как до исключения (один платёж).
	grp, emp := e2eGroup("l6sf24one"), user("79001119214")
	e = newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Исаев Харон")
	c := e.sendReceipt(grp, emp, "Сафаи", sberCheck("7000.00", "11:00:00"), "Исаев Харон")
	txt := groupText(grp, emp, "Сафаи", "Исаев Харон 7000")
	e.sendGroup(txt)
	if got := sum(dayTotals(e, grp)); got != 7000 {
		t.Fatalf("предусловие: чек с текстом — сбор %v", got)
	}
	out = e.tool("correct_operation", grp, `{"action":"exclude","amount":7000,"person":"Исаев Харон"}`)
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("исключили платёж 7000 (чек и текст): сбор %v (%s)", got, out)
	}
	out = e.tool("correct_operation", grp, `{"action":"restore","amount":7000,"person":"Исаев Харон"}`)
	got := sum(dayTotals(e, grp))
	_, why := l5splitfixCheck(e, c)
	_, twhy := l5splitfixText(e, txt.Info.ID)
	t.Logf("вернули платёж 7000: %s → сбор %v; чек %q, текст %q", out, got, why, twhy)
	if got != 7000 || why != "" || twhy == l5splitfixTextGone {
		t.Errorf("вернули платёж 7000 (чек и текст): сбор %v (хочу 7000), чек %q, текст %q", got, why, twhy)
	}

	// Владелец назвал именно запись («верни текст») — возвращается только она,
	// и ответ говорит, что чек того же платежа остался исключённым.
	out = e.tool("correct_operation", grp, `{"action":"exclude","amount":7000,"person":"Исаев Харон"}`)
	out = e.tool("correct_operation", grp, `{"action":"restore","amount":7000,"person":"Исаев Харон","kind":"text"}`)
	_, why = l5splitfixCheck(e, c)
	t.Logf("«верни текст 7000»: %s → чек %q", out, why)
	if why != ledger.ReasonIgnored {
		t.Errorf("«верни текст» вернул и чек: %q", why)
	}
	l5splitfixWant(t, "«верни текст»", out, "Вернул только эту запись", "чек 7 000 ₽")
}

// Находка 25: итог «Магомедов Ахмед 15000» (чеки 10000 + 5000) изменили в
// WhatsApp на 16000 — как поправка «не 15000 а 16000»: бот спрашивает, доплата
// это или ошибка, сбор не растёт до 31000.
func TestL6splitfix_25_EditSplitTotalMessageAsks(t *testing.T) {
	s := l5splitfixSetup(t, "l6sf25", "Магомедов Ахмед")
	e := s.e
	n := len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 16000"))
	reply := l4correctSent(e, n)
	got := sum(dayTotals(e, s.grp))
	a1, _ := l5splitfixCheck(e, s.c1)
	a2, _ := l5splitfixCheck(e, s.c2)
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("правка итога: бот: %s; сбор %v; чеки %v + %v; итог %v (%q)", reply, got, a1, a2, ta, why)
	if got != 15000 || a1 != 10000 || a2 != 5000 || ta != 15000 || why != ledger.ReasonSplit {
		t.Errorf("итог 15000 (чеки 10000 + 5000) изменили правкой на 16000: сбор %v, чеки %v + %v, итог %v (%q) — хочу без изменений, пока сотрудник не ответит",
			got, a1, a2, ta, why)
	}
	l5splitfixWant(t, "правка итога", reply, "10 000 + 5 000 = 15 000", "доплата 1 000", "Сумму пока не менял")
	if strings.Contains(reply, "поправил учёт") {
		t.Errorf("правка итога: бот говорит «поправил учёт»: %s", reply)
	}

	// Правка, которая сумму итога не меняет (тот же клиент и сумма), — как
	// раньше: тихо, сбор прежний.
	n = len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед, 15000"))
	got = sum(dayTotals(e, s.grp))
	ta, why = l5splitfixText(e, s.total.Info.ID)
	t.Logf("правка без смены суммы: бот: %s; сбор %v; итог %v (%q)", l4correctSent(e, n), got, ta, why)
	if got != 15000 || ta != 15000 || why != ledger.ReasonSplit {
		t.Errorf("правка итога без смены суммы: сбор %v, итог %v (%q)", got, ta, why)
	}

	// Итог расписали по чекам («10000» и «5000» — суммы самих чеков): это не
	// новая сумма итога — вопроса нет, сбор прежний.
	s = l5splitfixSetup(t, "l6sf25b", "Магомедов Ахмед")
	e = s.e
	n = len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 10000\nМагомедов Ахмед 5000"))
	reply = l4correctSent(e, n)
	got = sum(dayTotals(e, s.grp))
	t.Logf("итог расписали по чекам: бот: %s; сбор %v", reply, got)
	if got != 15000 || strings.Contains(reply, "доплата") {
		t.Errorf("итог расписали по чекам 10000 + 5000: сбор %v (хочу 15000), бот: %s", got, reply)
	}
}
