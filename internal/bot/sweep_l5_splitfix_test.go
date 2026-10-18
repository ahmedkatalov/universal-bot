package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Регрессии: текст-итог платежа частями («Магомедов Ахмед 15000» про чеки
// 10000 + 5000) — один платёж со ВСЕМИ своими чеками. Исключение итога
// исключает весь платёж (или только текст с kind=text), поправка суммы итога
// не переписывает банковский чек, поправка одной части не переписывает итог
// суммой этой части.

// l5splitfix — платёж частями: чеки 10000 и 5000 (второй с подписью caption2)
// и итог текстом «Магомедов Ахмед 15000».
type l5splitfix struct {
	e        *e2eBot
	grp, emp types.JID
	c1, c2   string          // сообщения с чеками 10000 и 5000
	total    *events.Message // итог текстом
}

func l5splitfixSetup(t *testing.T, tag, caption2 string) l5splitfix {
	grp, emp := e2eGroup(tag), user("79001119121")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	s := l5splitfix{e: e, grp: grp, emp: emp}
	s.c1 = e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
	s.c2 = e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "10:01:00"), caption2)
	s.total = groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000")
	e.sendGroup(s.total)
	return s
}

// l5splitfixBook — книга движка за сегодня.
func l5splitfixBook(e *e2eBot) *ledger.Book {
	n := time.Now()
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	book, err := e.db.Book(context.Background(), from, from.AddDate(0, 0, 1), "")
	if err != nil {
		e.t.Fatal(err)
	}
	return book
}

// l5splitfixTextGone — текста нет в книге: исключённые тексты движок не
// показывает вовсе.
const l5splitfixTextGone ledger.Reason = "нет в книге (исключён из учёта)"

// l5splitfixText — текст сообщения waID в книге: сумма и почему он не в сборе
// («» — посчитан как свой платёж).
func l5splitfixText(e *e2eBot, waID string) (float64, ledger.Reason) {
	b := l5splitfixBook(e)
	for _, x := range b.Excluded {
		if x.Text != nil && x.Text.WaID == waID {
			return x.Text.Amount, x.Reason
		}
	}
	for _, p := range b.Payments {
		for _, t := range p.Texts {
			if t.WaID == waID {
				return t.Amount, ""
			}
		}
	}
	return 0, l5splitfixTextGone
}

// l5splitfixCheck — чек сообщения waID в книге: сумма и почему он не в сборе
// («» — в сборе).
func l5splitfixCheck(e *e2eBot, waID string) (float64, ledger.Reason) {
	b := l5splitfixBook(e)
	for _, p := range b.Payments {
		for _, c := range p.Checks {
			if c.WaID == waID {
				return c.Amount, ""
			}
		}
	}
	for _, x := range b.Excluded {
		if x.Check != nil && x.Check.WaID == waID {
			return x.Check.Amount, x.Reason
		}
	}
	e.t.Fatalf("чек %s не найден в книге", waID)
	return 0, ""
}

// l5splitfixDMTool — инструмент ассистента в личке владельца (без группы по умолчанию).
func l5splitfixDMTool(e *e2eBot, name, args string) string {
	e.t.Helper()
	for _, tl := range e.assistantTools(context.Background(), user(ownerPhone), user(ownerPhone), true, false) {
		if tl.Name == name {
			out, err := tl.Handle(context.Background(), json.RawMessage(args))
			if err != nil {
				return "ERR: " + err.Error()
			}
			return out
		}
	}
	e.t.Fatalf("нет инструмента %s", name)
	return ""
}

func l5splitfixWant(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Errorf("%s: нет %q в ответе: %s", what, s, got)
		}
	}
}

// Находка 6 (а): «убери платёж Магомедова на 15000» — исключает весь платёж
// (оба чека), а не только первый чек; с kind=text — только текст, сбор прежний.
func TestL5splitfix_1_ExcludeSplitTotal(t *testing.T) {
	s := l5splitfixSetup(t, "l5sf1a", "Магомедов Ахмед")
	e := s.e
	if got := sum(dayTotals(e, s.grp)); got != 15000 {
		t.Fatalf("предусловие: сплит = 15000, сбор %v", got)
	}
	out := e.tool("correct_operation", s.grp, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	got := sum(dayTotals(e, s.grp))
	t.Logf("(а) %s → сбор %v", out, got)
	if got != 0 {
		t.Errorf("(а) исключили платёж «Магомедов Ахмед 15000» (10000 + 5000): в сборе осталось %v", got)
	}
	for _, c := range []string{s.c1, s.c2} {
		if amt, why := l5splitfixCheck(e, c); why != ledger.ReasonIgnored {
			t.Errorf("(а) чек %v после исключения всего платежа: %q", amt, why)
		}
	}
	l5splitfixWant(t, "(а)", out, "весь платёж", "10 000 ₽ + 5 000 ₽")
	if strings.Contains(out, "исключил и чек (1)") {
		t.Errorf("(а) ответ говорит про один чек: %s", out)
	}

	s = l5splitfixSetup(t, "l5sf1b", "Магомедов Ахмед")
	e = s.e
	out = e.tool("correct_operation", s.grp, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед","kind":"text"}`)
	got = sum(dayTotals(e, s.grp))
	t.Logf("(б) %s → сбор %v", out, got)
	if got != 15000 {
		t.Errorf("(б) исключили только текст-итог: сбор %v, хочу 15000 (чеки остаются)", got)
	}
	if _, why := l5splitfixText(e, s.total.Info.ID); why != l5splitfixTextGone {
		t.Errorf("(б) текст-итог не исключён: %q", why)
	}
	l5splitfixWant(t, "(б)", out, "только эта текстовая запись", "10 000 + 5 000", "сбор не изменился")

	// (в) Назван только сотрудник («убери платёж Сафаи на 15000»), «текст» не
	// сказан — это весь платёж.
	s = l5splitfixSetup(t, "l5sf1c", "Магомедов Ахмед")
	e = s.e
	out = e.tool("correct_operation", s.grp, `{"action":"exclude","amount":15000,"sender":"Сафаи"}`)
	got = sum(dayTotals(e, s.grp))
	t.Logf("(в) %s → сбор %v", out, got)
	if got != 0 {
		t.Errorf("(в) «убери платёж Сафаи на 15000» (10000 + 5000): в сборе осталось %v", got)
	}
	l5splitfixWant(t, "(в)", out, "весь платёж")
}

// Находка 6 (б): «не 15000 а 16000» свайпом на итог — банковский чек 10000
// остаётся 10000, итог не меняется молча и не считается сверх чеков: бот
// спрашивает, доплата это или ошибка.
func TestL5splitfix_2_CorrectSplitTotalAsks(t *testing.T) {
	for _, c := range []struct {
		tag, text string
		swipe     bool
		want      []string
	}{
		{"l5sf2a", "не 15000 а 16000", true, []string{"10 000 + 5 000 = 15 000", "доплата 1 000", "Сумму пока не менял", "Магомедов Ахмед 1000"}},
		{"l5sf2b", "Магомедов не 15000 а 16000", false, []string{"доплата 1 000", "Сумму пока не менял"}},
		{"l5sf2c", "не 15000 а 14000", true, []string{"на 1 000 ₽ меньше", "Сумму пока не менял"}},
	} {
		s := l5splitfixSetup(t, c.tag, "Магомедов Ахмед")
		e := s.e
		n := len(e.sent())
		if c.swipe {
			e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", c.text, s.total.Info.ID, s.emp, "Магомедов Ахмед 15000"))
		} else {
			e.sendGroup(groupText(s.grp, s.emp, "Сафаи", c.text))
		}
		reply := l4correctSent(e, n)
		got := sum(dayTotals(e, s.grp))
		a1, _ := l5splitfixCheck(e, s.c1)
		a2, _ := l5splitfixCheck(e, s.c2)
		ta, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("%q: бот: %s; сбор %v; чеки %v + %v; итог %v (%s)", c.text, reply, got, a1, a2, ta, why)
		if a1 != 10000 || a2 != 5000 {
			t.Errorf("%q на итог текстом: банковские чеки стали %v и %v (были 10000 и 5000)", c.text, a1, a2)
		}
		if got != 15000 || ta != 15000 || why != ledger.ReasonSplit {
			t.Errorf("%q на итог текстом: сбор %v, итог %v (%s) — хочу без изменений, пока сотрудник не ответит", c.text, got, ta, why)
		}
		l5splitfixWant(t, c.text, reply, c.want...)
	}
}

// Находка 6 (б): поправка одной части («не 5000 а 6000» / «не 10000 а 11000»
// свайпом на чек) не переписывает итог суммой части и не оставляет итог
// посчитанным сверх чеков: итог следует за частью (15000 → 16000).
func TestL5splitfix_3_CorrectPartCheckKeepsTotal(t *testing.T) {
	for _, c := range []struct {
		tag, old, new string
		first         bool
		want1, want2  float64
	}{
		{"l5sf3a", "5000", "6000", false, 10000, 6000},
		{"l5sf3b", "10000", "11000", true, 11000, 5000},
	} {
		s := l5splitfixSetup(t, c.tag, "Магомедов Ахмед")
		e := s.e
		target := s.c2
		if c.first {
			target = s.c1
		}
		n := len(e.sent())
		e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не "+c.old+" а "+c.new, target, s.emp, ""))
		reply := l4correctSent(e, n)
		got := sum(dayTotals(e, s.grp))
		a1, _ := l5splitfixCheck(e, s.c1)
		a2, _ := l5splitfixCheck(e, s.c2)
		ta, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("«не %s а %s» на чек: бот: %s; сбор %v; чеки %v + %v; итог %v (%s)", c.old, c.new, reply, got, a1, a2, ta, why)
		if a1 != c.want1 || a2 != c.want2 {
			t.Errorf("«не %s а %s»: чеки %v + %v, хочу %v + %v", c.old, c.new, a1, a2, c.want1, c.want2)
		}
		if ta != 16000 || why != ledger.ReasonSplit {
			t.Errorf("«не %s а %s» на часть: итог текстом %v (%s) — хочу 16000, итог частей (не сумма одной части)", c.old, c.new, ta, why)
		}
		if got != 16000 {
			t.Errorf("«не %s а %s» на часть платежа 10000 + 5000: сбор %v, хочу 16000", c.old, c.new, got)
		}
		l5splitfixWant(t, "поправка части", reply, "часть платежа", "стало 16 000 ₽")
	}
}

// Находка 6: итог, у которого часть сейчас не в сборе (чек без имени ждёт
// «чей чек?»): исключение итога исключает посчитанные части и прямо говорит,
// что чек без клиента не тронут.
func TestL5splitfix_4_ExcludeSplitTotalWithMissingPart(t *testing.T) {
	ctx := context.Background()
	s := l5splitfixSetup(t, "l5sf4", "")
	e := s.e
	e.age(s.grp.String())
	e.clarifyTick(ctx)
	if _, why := l5splitfixText(e, s.total.Info.ID); why != ledger.ReasonSplit {
		t.Fatalf("предусловие: итог с частью без имени — %q", why)
	}
	if _, why := l5splitfixCheck(e, s.c2); why != ledger.ReasonReview {
		t.Fatalf("предусловие: чек 5000 ждёт имени — %q", why)
	}
	out := e.tool("correct_operation", s.grp, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	got := sum(dayTotals(e, s.grp))
	_, why1 := l5splitfixCheck(e, s.c1)
	_, why2 := l5splitfixCheck(e, s.c2)
	t.Logf("%s → сбор %v; чек 10000: %q, чек 5000: %q", out, got, why1, why2)
	if got != 0 || why1 != ledger.ReasonIgnored {
		t.Errorf("исключили итог 15000: сбор %v, чек 10000 %q", got, why1)
	}
	if why2 != ledger.ReasonReview {
		t.Errorf("чек 5000 без названного клиента (может быть чужим) исключён молча: %q", why2)
	}
	l5splitfixWant(t, "итог с частью без имени", out, "весь платёж", "10 000 ₽", "5 000 ₽ без названного клиента")
}

// Находка 6: исключение ОДНОЙ части не выдаётся за «текст того же платежа»:
// итог остаётся итогом остальных частей; когда исключена последняя
// посчитанная часть, итог исключается тоже (иначе он посчитался бы сам).
func TestL5splitfix_5_ExcludeOnePart(t *testing.T) {
	for _, first := range []string{"10000", "5000"} {
		s := l5splitfixSetup(t, "l5sf5"+first, "Магомедов Ахмед")
		e := s.e
		second, left := "5000", 5000.0
		if first == "5000" {
			second, left = "10000", 10000
		}
		out := e.tool("correct_operation", s.grp, `{"action":"exclude","amount":`+first+`,"person":"Магомедов Ахмед","kind":"check"}`)
		got := sum(dayTotals(e, s.grp))
		_, why := l5splitfixText(e, s.total.Info.ID)
		t.Logf("исключили чек %s: %s → сбор %v, итог %q", first, out, got, why)
		if got != left || why != ledger.ReasonSplit {
			t.Errorf("исключили чек %s из 10000 + 5000: сбор %v (хочу %v), итог %q (хочу «%s»)", first, got, left, why, ledger.ReasonSplit)
		}
		if strings.Contains(out, "исключил его текстовую запись") {
			t.Errorf("исключили чек %s: ответ выдаёт итог 15000 за текст этого чека: %s", first, out)
		}
		l5splitfixWant(t, "чек "+first, out, "часть платежа", "остальные части остаются в сборе")

		out = e.tool("correct_operation", s.grp, `{"action":"exclude","amount":`+second+`,"person":"Магомедов Ахмед","kind":"check"}`)
		got = sum(dayTotals(e, s.grp))
		_, why = l5splitfixText(e, s.total.Info.ID)
		t.Logf("исключили и чек %s: %s → сбор %v, итог %q", second, out, got, why)
		if got != 0 || why != l5splitfixTextGone {
			t.Errorf("исключили оба чека 10000 + 5000: сбор %v, итог %q — итог не должен посчитаться сам", got, why)
		}
		l5splitfixWant(t, "последняя часть", out, "Итог текстом", "исключил тоже")
	}
}

// Находка 6: итог в двух группах (чеки только в СБ). В основной группе
// «убери 15000» убирает платёж только из основной; из лички с group=СБ —
// весь платёж везде (оба чека и оба итога).
func TestL5splitfix_6_ExcludeSplitTotalTwoGroups(t *testing.T) {
	setup := func(tag string) (*e2eBot, types.JID, types.JID) {
		sb, mg := e2eGroup(tag+"sb"), e2eGroup(tag+"main")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf, isl := user("79001119122"), user("79001119123")
		e.sendReceipt(sb, saf, "Сафаи", sberCheck("10000.00", "10:00:00"), "Магомедов Ахмед")
		e.sendReceipt(sb, saf, "Сафаи", sberCheck("5000.00", "10:01:00"), "Магомедов Ахмед")
		e.sendGroup(groupText(sb, saf, "Сафаи", "Магомедов Ахмед 15000"))
		e.sendGroup(groupText(mg, isl, "Ислам", "Магомедов Ахмед 15000"))
		return e, sb, mg
	}
	totals := func(e *e2eBot, sb, mg types.JID) (all, gsb, gmain float64) {
		from, to := l4Today()
		return l4Total(e, nil, from, to), l4Total(e, []string{sb.String()}, from, to), l4Total(e, []string{mg.String()}, from, to)
	}

	e, sb, mg := setup("l5sf6a")
	if all, gsb, gmain := totals(e, sb, mg); all != 15000 || gsb != 15000 || gmain != 15000 {
		t.Fatalf("предусловие: все %v, СБ %v, основная %v", all, gsb, gmain)
	}
	out := l5splitfixDMTool(e, "correct_operation", `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед","group":"СБ оплата"}`)
	all, gsb, gmain := totals(e, sb, mg)
	t.Logf("(а) из лички, group=СБ: %s → все %v, СБ %v, основная %v", out, all, gsb, gmain)
	if all != 0 || gsb != 0 || gmain != 0 {
		t.Errorf("(а) исключили весь платёж 15000 (10000 + 5000) в СБ: все %v, СБ %v, основная %v — хочу 0", all, gsb, gmain)
	}
	l5splitfixWant(t, "(а)", out, "весь платёж", "10 000 ₽ + 5 000 ₽")

	e, sb, mg = setup("l5sf6b")
	out = e.tool("correct_operation", mg, `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	all, gsb, gmain = totals(e, sb, mg)
	t.Logf("(б) в основной группе: %s → все %v, СБ %v, основная %v", out, all, gsb, gmain)
	if all != 15000 || gsb != 15000 || gmain != 0 {
		t.Errorf("(б) «убери 15000» в основной группе: все %v (хочу 15000), СБ %v (хочу 15000), основная %v (хочу 0)", all, gsb, gmain)
	}
	l5splitfixWant(t, "(б)", out, "«СБ оплата»", "остаются в сборе")

	// (в) Из лички без группы: оба итога — один платёж (не список «какой?»),
	// исключается весь платёж везде.
	e, sb, mg = setup("l5sf6c")
	out = l5splitfixDMTool(e, "correct_operation", `{"action":"exclude","amount":15000,"person":"Магомедов Ахмед"}`)
	all, gsb, gmain = totals(e, sb, mg)
	t.Logf("(в) из лички без группы: %s → все %v, СБ %v, основная %v", out, all, gsb, gmain)
	if all != 0 || gsb != 0 || gmain != 0 {
		t.Errorf("(в) «убери платёж Магомедова на 15000» из лички: все %v, СБ %v, основная %v — хочу 0", all, gsb, gmain)
	}
	l5splitfixWant(t, "(в)", out, "весь платёж", "10 000 ₽ + 5 000 ₽")
}

// Находка 6: итог, у которого часть сейчас не в сборе (чек без имени ждёт
// «чей чек?»). Поправка итога — вопрос, без изменений; поправка этой части —
// итог следует за ней (иначе 15000 перестал бы сходиться с 10000 + 6000 и
// посчитался сверх чеков).
func TestL5splitfix_7_CorrectSplitWithMissingPart(t *testing.T) {
	ctx := context.Background()
	s := l5splitfixSetup(t, "l5sf7", "")
	e := s.e
	e.age(s.grp.String())
	e.clarifyTick(ctx)
	if got := sum(dayTotals(e, s.grp)); got != 10000 {
		t.Fatalf("предусловие: 10000 в сборе, 5000 ждёт имени — сбор %v", got)
	}
	n := len(e.sent())
	e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 15000 а 16000", s.total.Info.ID, s.emp, "Магомедов Ахмед 15000"))
	reply := l4correctSent(e, n)
	got := sum(dayTotals(e, s.grp))
	ta, why := l5splitfixText(e, s.total.Info.ID)
	t.Logf("итог: бот: %s; сбор %v; итог %v (%s)", reply, got, ta, why)
	if got != 10000 || ta != 15000 || why != ledger.ReasonSplit {
		t.Errorf("«не 15000 а 16000» на итог с частью без имени: сбор %v, итог %v (%s) — хочу без изменений", got, ta, why)
	}
	l5splitfixWant(t, "итог с частью без имени", reply, "10 000 + 5 000 (пока не в сборе) = 15 000", "доплата 1 000")

	n = len(e.sent())
	e.sendGroup(groupReply(s.grp, s.emp, "Сафаи", "не 5000 а 6000", s.c2, s.emp, ""))
	reply = l4correctSent(e, n)
	got = sum(dayTotals(e, s.grp))
	a2, why2 := l5splitfixCheck(e, s.c2)
	ta, why = l5splitfixText(e, s.total.Info.ID)
	t.Logf("часть: бот: %s; сбор %v; чек %v (%s); итог %v (%s)", reply, got, a2, why2, ta, why)
	if a2 != 6000 || ta != 16000 || why != ledger.ReasonSplit || got != 10000 {
		t.Errorf("«не 5000 а 6000» на часть без имени: чек %v, итог %v (%s), сбор %v — хочу 6000, 16000 (итог частей), сбор 10000", a2, ta, why, got)
	}
}
