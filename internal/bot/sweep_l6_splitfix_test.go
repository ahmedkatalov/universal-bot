package bot

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

// Ревью R0: итог «Магомедов Ахмед 15000» (чеки 10000 + 5000) исправили в
// WhatsApp на 10000, потом чек 5000 оказался чужим (ответ на «чей чек?» или
// подпись чека — «Исаев Харон»). Итог 10000 — пара чека 10000 (обычный текст
// про чек), а не отдельный платёж: сбор 15000 (Магомедов 10000, Исаев 5000), не
// 30000.
func TestL6splitfix_R0_EditTotalDownThenOtherClientsCheck(t *testing.T) {
	ctx := context.Background()
	// (а) чек 5000 без подписи: бот спросил «чей чек?», ответили свайпом.
	s := l5splitfixSetup(t, "l6sfr0a", "")
	e := s.e
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.age(s.grp.String())
	e.clarifyTick(ctx)
	n := len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 10000"))
	reply := l4correctSent(e, n)
	t.Logf("(а) правка итога на 10000: бот: %s; сбор %v", reply, sum(dayTotals(e, s.grp)))
	if strings.Contains(reply, "Сумму пока не менял") {
		t.Errorf("(а) итог исправили на 10000 — есть чек 10000 этого клиента, вопрос лишний: %s", reply)
	}
	n = len(e.sent())
	e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "Исаев Харон", s.c2, s.emp, "чек"))
	got := dayTotals(e, s.grp)
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("(а) чек 5000 → Исаев: бот: %s; сбор %v (%v); итог %v (%q)", l4correctSent(e, n), sum(got), got, ta, why)
	if sum(got) != 15000 || got["Исаев Харон"] != 5000 {
		t.Errorf("(а) итог исправили на 10000, чек 5000 оказался Исаева: сбор %v (%v), хочу 15000 (Магомедов 10000, Исаев 5000)", sum(got), got)
	}
	if ta != 10000 || why == "" {
		t.Errorf("(а) итог %v (%q) — хочу 10000 вместе с чеком 10000", ta, why)
	}

	// (б) чек 5000 подписан тем же клиентом; подпись исправили на «Исаев Харон».
	s = l5splitfixSetup(t, "l6sfr0b", "Магомедов Ахмед")
	e = s.e
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	n = len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 10000"))
	reply = l4correctSent(e, n)
	if strings.Contains(reply, "Сумму пока не менял") {
		t.Errorf("(б) итог исправили на 10000 — есть чек 10000 этого клиента, вопрос лишний: %s", reply)
	}
	if got := sum(dayTotals(e, s.grp)); got != 15000 {
		t.Errorf("(б) после правки итога на 10000: сбор %v, хочу 15000 (оба чека в сборе)", got)
	}
	e.event(editOf(s.grp, s.emp, s.c2, "Исаев Харон"))
	got = dayTotals(e, s.grp)
	ta, why = l5splitfixText(e, s.total.Info.ID)
	t.Logf("(б) подпись чека 5000 → Исаев: сбор %v (%v); итог %v (%q)", sum(got), got, ta, why)
	if sum(got) != 15000 || got["Исаев Харон"] != 5000 {
		t.Errorf("(б) сбор %v (%v), хочу 15000 (Магомедов 10000, Исаев 5000)", sum(got), got)
	}
}

// Ревью R1: итог исправили так, что он сходится с чеками, как они есть сейчас
// — вопрос «доплата или ошибка?» лишний, итог записан как исправили.
func TestL6splitfix_R1_EditTotalMatchingCurrentChecks(t *testing.T) {
	// (а) чек 5000 прислали по ошибке и удалили, итог исправили на 10000.
	s := l5splitfixSetup(t, "l6sfr1a", "Магомедов Ахмед")
	e := s.e
	e.event(l4textsRevoke(s.grp, s.emp, s.c2))
	n := len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 10000"))
	reply := l4correctSent(e, n)
	got := sum(dayTotals(e, s.grp))
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("(а) чек 5000 удалён, итог → 10000: бот: %s; сбор %v; итог %v (%q)", reply, got, ta, why)
	if strings.Contains(reply, "Сумму пока не менял") {
		t.Errorf("(а) лишний вопрос: чек 5000 удалён, итог исправлен на 10000 — бот: %s", reply)
	}
	if got != 10000 || ta != 10000 {
		t.Errorf("(а) сбор %v, итог %v — хочу 10000 и 10000", got, ta)
	}

	// (б) доплату 1000 уже прислали чеком, итог исправили на 16000.
	s = l5splitfixSetup(t, "l6sfr1b", "Магомедов Ахмед")
	e = s.e
	e.sendReceipt(s.grp, s.emp, "Сафаи", sberCheck("1000.00", "10:05:00"), "Магомедов Ахмед")
	n = len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 16000"))
	reply = l4correctSent(e, n)
	got = sum(dayTotals(e, s.grp))
	ta, why = l5splitfixText(e, s.total.Info.ID)
	t.Logf("(б) чек 1000 прислан, итог → 16000: бот: %s; сбор %v; итог %v (%q)", reply, got, ta, why)
	if strings.Contains(reply, "доплата") || strings.Contains(reply, "Сумму пока не менял") {
		t.Errorf("(б) лишний вопрос: третий чек 1000 уже прислан, итог 16000 = 10000+5000+1000 — бот: %s", reply)
	}
	if got != 16000 || ta != 16000 || why != ledger.ReasonSplit {
		t.Errorf("(б) сбор %v, итог %v (%q) — хочу 16000, итог 16000 (итог частей)", got, ta, why)
	}
}

// Ревью R2: обе части поправили, первую поправку удалили (итог 16000), потом в
// итоге исправили только опечатку («Магомедов Ахмед, 15000» — сумма в тексте
// та же): итог остаётся 16000, без вопроса «ошибка?», сбор 16000.
func TestL6splitfix_R2_ChainUndoThenTypoEdit(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		tag := "l6sfr2"
		if quiet {
			tag = "l6sfr2q"
		}
		s := l5splitfixSetup(t, tag, "Магомедов Ахмед")
		e := s.e
		m1 := groupReply(s.grp, s.emp, "Сафаи", "не 10000 а 11000", s.c1, s.emp, "")
		e.sendGroup(m1)
		e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 5000 а 6000", s.c2, s.emp, ""))
		e.event(l4textsRevoke(s.grp, s.emp, m1.Info.ID))
		if got := sum(dayTotals(e, s.grp)); got != 16000 {
			t.Fatalf("предусловие: первая поправка удалена — сбор %v, хочу 16000", got)
		}
		n := len(e.sent())
		if quiet {
			// Импорт истории: правка применяется молча.
			budget := 10
			em := editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед, 15000")
			if !e.applyEdit(context.Background(), em, em.Message.GetProtocolMessage(), editQuiet{budget: &budget}) {
				t.Fatalf("тихая правка не применилась")
			}
		} else {
			e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед, 15000"))
		}
		reply := l4correctSent(e, n)
		got := sum(dayTotals(e, s.grp))
		ta, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("тихо=%v: правка опечатки: бот: %s; сбор %v; итог %v (%q)", quiet, reply, got, ta, why)
		if strings.Contains(reply, "Сумму пока не менял") {
			t.Errorf("тихо=%v: лишний вопрос на правку без смены суммы: %s", quiet, reply)
		}
		if got != 16000 || ta != 16000 || why != ledger.ReasonSplit {
			t.Errorf("тихо=%v: сбор %v, итог %v (%q) — хочу сбор 16000, итог 16000 (итог частей)", quiet, got, ta, why)
		}
	}

	// Та же цепочка, но в итоге написали новую сумму 16000 — как сейчас по
	// чекам 10000 + 6000: итог 16000 (а не 17000 по второй поправке, у которой
	// прежняя сумма тоже 16000), без вопроса.
	s := l5splitfixSetup(t, "l6sfr2n", "Магомедов Ахмед")
	e := s.e
	m1 := groupReply(s.grp, s.emp, "Сафаи", "не 10000 а 11000", s.c1, s.emp, "")
	e.sendGroup(m1)
	e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 5000 а 6000", s.c2, s.emp, ""))
	e.event(l4textsRevoke(s.grp, s.emp, m1.Info.ID))
	n := len(e.sent())
	e.event(editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 16000"))
	reply := l4correctSent(e, n)
	got := sum(dayTotals(e, s.grp))
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("итог исправили на 16000: бот: %s; сбор %v; итог %v (%q)", reply, got, ta, why)
	if strings.Contains(reply, "Сумму пока не менял") || got != 16000 || ta != 16000 || why != ledger.ReasonSplit {
		t.Errorf("итог исправили на 16000 (чеки 10000 + 6000): сбор %v, итог %v (%q) — хочу 16000, итог частей, без вопроса; бот: %s",
			got, ta, why, reply)
	}
}

// Ревью R3: чек 7000 и текст «Исаев Харон 7000» — один платёж; исключили его
// (чек и текст вместе), сотрудник прислал текст ещё раз (посчитан сам), потом
// «верни платёж Исаева 7000»: чек возвращается в пару к новому тексту, старый
// текст из исключения остаётся исключённым — сбор 7000, а не 14000.
func TestL6splitfix_R3_RestoreAfterTextResent(t *testing.T) {
	setup := func(t *testing.T, tag string) (*e2eBot, types.JID, string, *events.Message, *events.Message) {
		grp, emp := e2eGroup(tag), user("79001119314")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Исаев Харон")
		c := e.sendReceipt(grp, emp, "Сафаи", sberCheck("7000.00", "11:00:00"), "Исаев Харон")
		old := groupText(grp, emp, "Сафаи", "Исаев Харон 7000")
		e.sendGroup(old)
		out := e.tool("correct_operation", grp, `{"action":"exclude","amount":7000,"person":"Исаев Харон"}`)
		if got := sum(dayTotals(e, grp)); got != 0 {
			t.Fatalf("%s предусловие: исключили платёж — сбор %v (%s)", tag, got, out)
		}
		again := groupText(grp, emp, "Сафаи", "Исаев Харон 7000")
		e.sendGroup(again)
		if got := sum(dayTotals(e, grp)); got != 7000 {
			t.Fatalf("%s предусловие: текст прислали снова — сбор %v", tag, got)
		}
		return e, grp, c, old, again
	}

	e, grp, c, old, again := setup(t, "l6sfr3")
	out := e.tool("correct_operation", grp, `{"action":"restore","amount":7000,"person":"Исаев Харон"}`)
	got := sum(dayTotals(e, grp))
	_, cwhy := l5splitfixCheck(e, c)
	_, owhy := l5splitfixText(e, old.Info.ID)
	_, awhy := l5splitfixText(e, again.Info.ID)
	t.Logf("вернули: %s → сбор %v; чек %q, старый текст %q, новый текст %q", out, got, cwhy, owhy, awhy)
	if got != 7000 || cwhy != "" {
		t.Errorf("чек 7000 и его текст (текст прислали ещё раз): сбор %v, чек %q — хочу 7000, чек в сборе", got, cwhy)
	}
	if owhy != l5splitfixTextGone || awhy != ledger.ReasonTwin {
		t.Errorf("старый текст %q (хочу исключён), новый %q (хочу пара чека)", owhy, awhy)
	}
	l5splitfixWant(t, "возврат после повторного текста", out, "возвращена", "[чек]", "оставил исключённым", "kind=text")
	if strings.Contains(out, "вернул то, что было исключено") {
		t.Errorf("ответ говорит, что вернул и старый текст: %s", out)
	}

	// Владелец настаивает — «верни тот текст» (kind=text): возвращается.
	out = e.tool("correct_operation", grp, `{"action":"restore","amount":7000,"person":"Исаев Харон","kind":"text"}`)
	if got := sum(dayTotals(e, grp)); got != 14000 {
		t.Errorf("«верни текст» после предупреждения: сбор %v, хочу 14000 (владелец сказал — отдельный платёж): %s", got, out)
	}

	// Такой же текст, присланный ДО исключения, — отдельный платёж, как и был:
	// возврат его не трогает (метка исключения позже этого текста).
	t.Run("before", func(t *testing.T) { l6splitfixR3Before(t, setup) })
}

func l6splitfixR3Before(t *testing.T, setup func(*testing.T, string) (*e2eBot, types.JID, string, *events.Message, *events.Message)) {
	ctx := context.Background()
	e, grp, _, _, _ := setup(t, "l6sfr3b")
	var batch string
	ops, _ := e.db.FindOperations(ctx, 7000, "Исаев", nil, nil, "")
	for _, op := range ops {
		if op.Ignored && op.Batch != "" {
			batch = op.Batch
		}
	}
	back, err := e.db.RestoreBatch(ctx, batch)
	if err != nil || len(back) != 2 {
		t.Fatalf("возврат пачки %q: %v, %d записей", batch, err, len(back))
	}
	later := batch[:strings.LastIndexByte(batch, '@')+1] + strconv.FormatInt(time.Now().Add(time.Second).UnixNano(), 10)
	if kept := e.keepReplacedTexts(ctx, later, back); len(kept) != 0 {
		t.Errorf("текст, присланный до исключения, — отдельный платёж, а возврат оставил исключённым %v", kept)
	}
	if got := sum(dayTotals(e, grp)); got != 14000 {
		t.Errorf("такой же текст был до исключения: после возврата сбор %v, хочу 14000 (как до исключения)", got)
	}
}

// Ревью R4: итог в СБ исправили в WhatsApp, а такой же итог 15000 в основной
// группе был связан с чеками только через него — без вопроса он посчитался бы
// отдельным платежом (31000 / 30000). Итог с доплатой (чек 1000 уже прислан) и
// итог, расписанный по чекам, — тот же платёж: в учёте остаётся прежний итог,
// бот объясняет. И при импорте истории (тихо).
func TestL6splitfix_R4_EditTotalOrphansOtherGroup(t *testing.T) {
	for _, c := range []struct {
		tag, text  string
		topUp      bool
		quiet      bool
		want, main float64
	}{
		{"l6sfr4a", "Магомедов Ахмед 16000", true, false, 16000, 15000},
		{"l6sfr4b", "Магомедов Ахмед 10000\nМагомедов Ахмед 5000", false, false, 15000, 15000},
		{"l6sfr4aq", "Магомедов Ахмед 16000", true, true, 16000, 15000},
		{"l6sfr4bq", "Магомедов Ахмед 10000\nМагомедов Ахмед 5000", false, true, 15000, 15000},
	} {
		// Своя подтестовая база на случай: соединения закрываются после каждого.
		t.Run(c.tag, func(t *testing.T) {
			e, sb, mg, _, _ := l6splitfixTwoGroups(t, c.tag)
			saf := user("79001119212")
			if c.topUp {
				e.sendReceipt(sb, saf, "Сафаи", sberCheck("1000.00", "10:05:00"), "Магомедов Ахмед")
			}
			if all, _, _ := l6splitfixTotals(e, sb, mg); all != c.want {
				t.Fatalf("%s предусловие: все %v", c.tag, all)
			}
			var totalID string
			for _, x := range l5splitfixBook(e).Excluded {
				if x.Text != nil && x.Text.Group == sb.String() {
					totalID = x.Text.WaID
				}
			}
			n := len(e.sent())
			em := editOf(sb, saf, totalID, c.text)
			if c.quiet {
				budget := 10
				if !e.applyEdit(context.Background(), em, em.Message.GetProtocolMessage(), editQuiet{budget: &budget}) {
					t.Fatalf("%s: тихая правка не применилась", c.tag)
				}
			} else {
				e.event(em)
			}
			reply := l4correctSent(e, n)
			all, gsb, gmain := l6splitfixTotals(e, sb, mg)
			t.Logf("%s: бот: %s; все %v, СБ %v, основная %v", c.tag, reply, all, gsb, gmain)
			if all != c.want || gsb != c.want || gmain != c.main {
				t.Errorf("%s: итог в СБ исправили на %q, такой же итог 15000 в основной: все %v, СБ %v, основная %v — хочу %v, %v, %v",
					c.tag, c.text, all, gsb, gmain, c.want, c.want, c.main)
			}
			if c.quiet {
				if reply != "" {
					t.Errorf("%s: импорт истории — бот написал в группу: %s", c.tag, reply)
				}
				return
			}
			l5splitfixWant(t, c.tag, reply, "«Оплата клиентов»", "оставил прежний итог 15 000", "сбор не изменился")
			if strings.Contains(reply, "поправил учёт") {
				t.Errorf("%s: бот говорит «поправил учёт», а итог оставлен прежним: %s", c.tag, reply)
			}
		})
	}

	// Итог исправили на сумму одного чека («10000»: чек 5000 — не этого
	// платежа) — от части чеков отказались, прежний итог не держим (иначе,
	// когда чек 5000 назовут другим клиентом, итог 15000 посчитался бы сам):
	// бот спрашивает про итог в основной группе.
	e, sb, mg, _, c2 := l6splitfixTwoGroups(t, "l6sfr4c")
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	saf := user("79001119212")
	var totalID string
	for _, x := range l5splitfixBook(e).Excluded {
		if x.Text != nil && x.Text.Group == sb.String() {
			totalID = x.Text.WaID
		}
	}
	n := len(e.sent())
	e.event(editOf(sb, saf, totalID, "Магомедов Ахмед 10000"))
	reply := l4correctSent(e, n)
	t.Logf("итог → 10000: бот: %s", reply)
	l5splitfixWant(t, "итог → 10000", reply, "«Оплата клиентов»", "считается отдельным платежом")
	if strings.Contains(reply, "оставил прежний итог") {
		t.Errorf("итог исправили на 10000 (от чека 5000 отказались) — прежний итог держать нельзя: %s", reply)
	}
	e.event(editOf(sb, saf, c2, "Исаев Харон"))
	all, gsb, gmain := l6splitfixTotals(e, sb, mg)
	t.Logf("чек 5000 → Исаев: все %v, СБ %v, основная %v", all, gsb, gmain)
	if all > 30000 {
		t.Errorf("итог в СБ → 10000, чек 5000 → Исаев: все %v — итог 15000 в СБ посчитан сверх чеков", all)
	}
}

// Ревью R5: правка итога пришла при импорте истории (тихо, спросить нельзя):
// итог 15000 (чеки 10000 + 5000) исправлен на 16000 — как в живой правке,
// в учёте остаётся прежний итог (не сбор 31000).
func TestL6splitfix_R5_QuietEditSplitTotal(t *testing.T) {
	s := l5splitfixSetup(t, "l6sfr5", "Магомедов Ахмед")
	e := s.e
	n := len(e.sent())
	budget := 10
	em := editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 16000")
	if !e.applyEdit(context.Background(), em, em.Message.GetProtocolMessage(), editQuiet{budget: &budget}) {
		t.Fatalf("тихая правка не применилась")
	}
	got := sum(dayTotals(e, s.grp))
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("тихо: сбор %v; итог %v (%q)", got, ta, why)
	if got != 15000 || ta != 15000 || why != ledger.ReasonSplit {
		t.Errorf("импорт истории: итог 15000 (чеки 10000 + 5000) исправлен на 16000 — сбор %v, итог %v (%q); хочу 15000, итог 15000 (итог частей)",
			got, ta, why)
	}
	if reply := l4correctSent(e, n); reply != "" {
		t.Errorf("импорт истории: бот написал в группу: %s", reply)
	}

	// Тихая правка итога, которая сходится с чеками как они есть (доплата 1000
	// уже прислана чеком), — записывается как исправили.
	s = l5splitfixSetup(t, "l6sfr5b", "Магомедов Ахмед")
	e = s.e
	e.sendReceipt(s.grp, s.emp, "Сафаи", sberCheck("1000.00", "10:05:00"), "Магомедов Ахмед")
	em = editOf(s.grp, s.emp, s.total.Info.ID, "Магомедов Ахмед 16000")
	if !e.applyEdit(context.Background(), em, em.Message.GetProtocolMessage(), editQuiet{budget: &budget}) {
		t.Fatalf("тихая правка не применилась")
	}
	got = sum(dayTotals(e, s.grp))
	ta, why = l5splitfixText(e, s.total.Info.ID)
	if got != 16000 || ta != 16000 || why != ledger.ReasonSplit {
		t.Errorf("тихо, доплата чеком уже есть: сбор %v, итог %v (%q) — хочу 16000, итог 16000", got, ta, why)
	}
}
