package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"whatsapp-bot/internal/parser"
)

// Регрессии: имя клиента для чека без подписи берётся только из строки с той
// же суммой (или без суммы); «Имя\nсумма\nсумма» после чека — это платежи.

// Finding 19: «чей чек?» не берёт имя из платежа на другую сумму.
func TestL4naming_19_ContextNameIgnoresAmount(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	grp, emp := e2eGroup("oo"), user("79285550006")
	e.groupsCache[grp] = "Оплата"
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("20000.00", "12:01:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 20000"))
	e.age(grp.String())
	e.clarifyTick(ctx) // прошло 90 с: бот разбирает чеки без имени
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон 15000"))
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if sum(got) != 35000 || got["Магомедов Ахмед"] > 20000 {
		t.Errorf("чеки 15000 и 20000, затем «Магомедов Ахмед 20000» и «Исаев Харон 15000»: сбор %v, ожидали 35000 (%v); чек 15000 → %q, чек 20000 → %q",
			sum(got), got, r1.Recipient, r2.Recipient)
	}
	// Текст на ту же сумму по-прежнему называет чек молча, второй чек — по ответу.
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" {
		t.Errorf("чек 15000 → %q (ожидали «Исаев Харон»), чек 20000 → %q (ожидали «Магомедов Ахмед»)", r1.Recipient, r2.Recipient)
	}
}

// Finding 19 (наличка): «ФИО сумма нал» не называет чек другого клиента.
func TestL4naming_19_CashTextDoesNotNameCheck(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	grp, emp := e2eGroup("oc"), user("79285550016")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000 нал"))
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("30000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r := e.db.TestReceiptByMessage(ctx, ck)
	if r.Recipient == "Магомедов Ахмед" {
		t.Errorf("чек 30000 без подписи молча отдан «Магомедов Ахмед» из «Магомедов Ахмед 15000 нал»: %v", dayTotals(e, grp))
	}
	if q := e.lastWith("Чей это чек"); q.ID == "" {
		t.Errorf("бот не спросил «чей чек?» про чек 30000: %+v", e.sent())
	}
}

// l4namingAI — фейковый ИИ, как настоящая модель: имя клиента из подписи, а
// платежи читает и в формате блоков «ФИО\nсумма\nсумма» (каждая сумма — платёж
// этого ФИО, «нал» в строке — наличка).
func l4namingAI(names ...string) func(c aiCall) aiReply {
	base := aiNamesAndPayments(names...)
	return func(c aiCall) aiReply {
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return base(c)
		}
		u := c.lastUserText()
		if i := strings.Index(u, "Строки:\n"); i >= 0 {
			u = u[i+len("Строки:\n"):]
		}
		if i := strings.Index(u, "\n\nЧисла-суммы"); i >= 0 {
			u = u[:i]
		}
		var ps []string
		for _, tr := range parser.ParseMessage(u).Transactions {
			for _, n := range names {
				if strings.Contains(tr.RawName, strings.Fields(n)[0]) {
					cash := tr.IsCash || parser.IsCash(tr.Line)
					ps = append(ps, fmt.Sprintf(`{"name":%q,"amount":%v,"cash":%v}`, n, tr.Amount, cash))
				}
			}
		}
		return aiReply{Text: `{"payments":[` + strings.Join(ps, ",") + `],"clarify":""}`}
	}
}

// Finding 22: «Имя\nсумма\nсумма» после чека без подписи — платежи, а не
// только имя: лишняя сумма не теряется, чек подписан строкой на его сумму.
func TestL4naming_22_NameAndAmountLinesNextToCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон\n10000\n5000 нал", "Исаев Харон\n10000\n5000"} {
		for _, swipe := range []bool{false, true} {
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = l4namingAI("Исаев Харон")
			grp, emp := e2eGroup("nl"), user("79285550007")
			ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
			if swipe {
				e.sendGroup(groupReply(grp, emp, "Сафаи", text, ck, emp, ""))
			} else {
				e.sendGroup(groupText(grp, emp, "Сафаи", text))
			}
			r := e.db.TestReceiptByMessage(ctx, ck)
			if got := dayTotals(e, grp); sum(got) != 15000 || got["Исаев Харон"] != 15000 {
				t.Errorf("чек 10000 без подписи, затем %q (свайп=%v): сбор %v, ожидали 15000 у Исаева (%v); чек → %q", text, swipe, sum(got), got, r.Recipient)
			}
			if r.Recipient != "Исаев Харон" {
				t.Errorf("чек 10000, затем %q (свайп=%v): чек → %q, ожидали «Исаев Харон»", text, swipe, r.Recipient)
			}
		}
	}
}

// Finding 22: в списке нет строки на сумму чека — чек не подписываем чужой
// строкой, но все платежи списка записаны.
func TestL4naming_22_ListWithoutCheckAmountKeepsCheckOpen(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l4namingAI("Исаев Харон")
	grp, emp := e2eGroup("nk"), user("79285550017")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("7000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон\n10000\n5000"))
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Исаев Харон" {
		t.Errorf("чек 7000 подписан «Исаев Харон» из списка без строки 7000")
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 15000 {
		t.Errorf("список «Исаев Харон\n10000\n5000»: у Исаева %v, ожидали 15000 (%v)", got["Исаев Харон"], got)
	}
}

// Finding 22: в списке сумма чека у ДВУХ разных клиентов — чек не подписываем
// первым попавшимся (не угадываем), оба платежа записаны.
func TestL4naming_22_ListTwoClientsSameAmountNotGuessed(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l4namingAI("Иванов Иван", "Петров Петр")
	grp, emp := e2eGroup("n2"), user("79285550027")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Иванов Иван 10000\nПетров Петр 10000"))
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Иванов Иван" || r.Recipient == "Петров Петр" {
		t.Errorf("чек 10000 молча подписан %q из списка, где 10000 у двух клиентов", r.Recipient)
	}
	if got := dayTotals(e, grp); got["Иванов Иван"] != 10000 || got["Петров Петр"] != 10000 {
		t.Errorf("список двух платежей по 10000: %v", got)
	}
}

// Finding 14 (часть бота): текст на полную сумму не называет молча чек-часть
// на другую сумму — бот спрашивает «чей чек?».
func TestL4naming_14_SplitCheckNotSilentlyNamedFromTotalText(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	grp := e2eGroup("tot")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112012")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон 100000"))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("50000.00", "13:02:10"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("50000.00", "13:03:40"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	for _, ck := range []string{ck1, ck2} {
		if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Исаев Харон" {
			t.Errorf("чек 50000 молча назван «Исаев Харон» из текста «Исаев Харон 100000»")
		}
	}
	asked := 0
	for _, s := range e.sent() {
		if strings.Contains(s.Text, "Чей это чек") {
			asked++
		}
	}
	if asked == 0 {
		t.Errorf("бот не спросил «чей чек?» про чеки 50000: %+v", e.sent())
	}
}

// revCashBlockAI — фейковый ИИ, как настоящая модель: пометка налички в
// заголовке («Наличка»), в строке ФИО («… у Дени») или последней строкой
// («наличка») относится ко всему блоку.
func revCashBlockAI(names ...string) func(c aiCall) aiReply {
	base := aiNamesAndPayments(names...)
	return func(c aiCall) aiReply {
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return base(c)
		}
		u := c.lastUserText()
		if i := strings.Index(u, "Строки:\n"); i >= 0 {
			u = u[i+len("Строки:\n"):]
		}
		if i := strings.Index(u, "\n\nЧисла-суммы"); i >= 0 {
			u = u[:i]
		}
		blockCash := parser.IsCash(u)
		var ps []string
		for _, tr := range parser.ParseMessage(u).Transactions {
			for _, n := range names {
				if strings.Contains(tr.RawName, strings.Fields(n)[0]) {
					ps = append(ps, fmt.Sprintf(`{"name":%q,"amount":%v,"cash":%v}`, n, tr.Amount, blockCash))
				}
			}
		}
		return aiReply{Text: `{"payments":[` + strings.Join(ps, ",") + `],"clarify":""}`}
	}
}

// R0: наличный блок («Наличка» сверху, «… у Дени» в строке ФИО, «наличка»
// последней строкой) не называет чек — это другой платёж; иначе чек 10000
// считался бы второй раз рядом с наличкой.
func TestL4naming_R0_CashBlockDoesNotNameCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Наличка\nИсаев Харон\n10000\n5000", "Исаев Харон\n10000\n5000\nналичка", "Исаев Харон у Дени\n10000\n5000"} {
		for _, textFirst := range []bool{false, true} {
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = revCashBlockAI("Исаев Харон")
			e.aliases.Add("Исаев Харон", "Исаев Харон")
			grp, emp := e2eGroup("cb"), user("79285550101")
			e.groupsCache[grp] = "Оплата"
			var ck string
			if textFirst {
				e.sendGroup(groupText(grp, emp, "Сафаи", text))
				ck = e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
			} else {
				ck = e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
				e.sendGroup(groupText(grp, emp, "Сафаи", text))
			}
			e.age(grp.String())
			e.clarifyTick(ctx)
			r := e.db.TestReceiptByMessage(ctx, ck)
			got := dayTotals(e, grp)
			if r.Recipient == "Исаев Харон" || got["Исаев Харон"] != 15000 {
				t.Errorf("чек 10000 без подписи + наличный блок %q (текст первым=%v): чек → %q, сбор %v (ожидали: наличка Исаева 15000, про чек — вопрос)", text, textFirst, r.Recipient, got)
			}
			if q := e.lastWith("Чей это чек"); q.ID == "" {
				t.Errorf("наличный блок %q (текст первым=%v): про чек 10000 не спросили «чей чек?»", text, textFirst)
			}
		}
	}
}

// R0 (граница): пометка «Наличка:» относится к строкам ПОД ней — безналичный
// платёж выше по-прежнему называет чек на свою сумму.
func TestL4naming_R0_CashHeaderBelowKeepsCardLine(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l4namingAI("Магомедов Ахмед", "Исаев Дени")
	grp, emp := e2eGroup("ch"), user("79285550111")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000\nНаличка:\nИсаев Дени 20000"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient != "Магомедов Ахмед" {
		t.Errorf("чек 15000, затем «Магомедов Ахмед 15000\\nНаличка:\\nИсаев Дени 20000»: чек → %q, ожидали «Магомедов Ахмед» (%v)", r.Recipient, dayTotals(e, grp))
	}
}

// R1: в «ФИО\nФИО сумма» чек на эту сумму получает ФИО со строки суммы, а не
// первую строку-имя.
func TestL4naming_R1_SingleAmountLineOtherNameLine(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	grp, emp := e2eGroup("sl"), user("79285550103")
	e.groupsCache[grp] = "Оплата"
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("20000.00", "12:00:00"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:01:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед\nИсаев Харон 15000"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r2.Recipient == "Магомедов Ахмед" || got["Исаев Харон"] > 15000 {
		t.Errorf("чеки 20000 и 15000, затем «Магомедов Ахмед\\nИсаев Харон 15000»: чек 15000 → %q, чек 20000 → %q, сбор %v", r2.Recipient, r1.Recipient, got)
	}
}

// R2: заголовок блока, который не ФИО клиента («Карта Хадижат», «Карта Сбер»),
// чек не называет — бот спрашивает «чей чек?».
func TestL4naming_R2_NonNameHeaderDoesNotLabelCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Карта Хадижат\n10000\n5000", "Карта Сбер\n10000\n5000"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l4namingAI("Исаев Харон")
		grp, emp := e2eGroup("nh"), user("79285550102")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		e.age(grp.String())
		e.clarifyTick(ctx)
		r := e.db.TestReceiptByMessage(ctx, ck)
		if e.lastWith("Чей это чек").ID == "" || strings.HasPrefix(r.Recipient, "Карта") {
			t.Errorf("чек 10000 без подписи, затем %q: чек молча → %q, «чей чек?» не спросили (сбор %v)", text, r.Recipient, dayTotals(e, grp))
		}
	}
}

// R3: «ФИО сумма» с лишним числом в строке («за 2 месяца», «остаток 60т»)
// называет чек на ту же сумму без вопроса.
func TestL4naming_R3_ExtraNumberInLineNamesCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон за 2 месяца 30000", "Исаев Харон 30000 остаток 60т"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		e.aliases.Add("Исаев Харон", "Исаев Харон")
		grp, emp := e2eGroup("xn"), user("79285550104")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("30000.00", "12:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		e.age(grp.String())
		e.clarifyTick(ctx)
		r := e.db.TestReceiptByMessage(ctx, ck)
		if q := e.lastWith("Чей это чек"); q.ID != "" || r.Recipient != "Исаев Харон" {
			t.Errorf("чек 30000, затем %q: чек → %q, вопрос=%q, сбор %v", text, r.Recipient, q.Text, dayTotals(e, grp))
		}
	}
}

// R4: два чека одного клиента и «ФИО\nсумма\nсумма» — оба чека подписаны
// строками на их суммы, без лишнего «чей чек?».
func TestL4naming_R4_TwoChecksOneClientBlock(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l4namingAI("Исаев Харон")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	grp, emp := e2eGroup("tc"), user("79285550105")
	e.groupsCache[grp] = "Оплата"
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "12:01:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон\n10000\n5000"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	q := e.lastWith("Чей это чек")
	if got := dayTotals(e, grp); q.ID != "" || r1.Recipient != "Исаев Харон" || r2.Recipient != "Исаев Харон" || got["Исаев Харон"] != 15000 {
		t.Errorf("два чека 10000+5000 и «Исаев Харон\\n10000\\n5000»: чек1 → %q, чек2 → %q, вопрос=%q, сбор %v", r1.Recipient, r2.Recipient, q.Text, got)
	}
}

// R4 (граница): одна строка «ФИО сумма» и ДВА чека на эту сумму — второй чек
// этим ФИО не называем (строка одна), про него спрашиваем.
func TestL4naming_R4_OneLineTwoSameChecksAsks(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	grp, emp := e2eGroup("t2"), user("79285550115")
	e.groupsCache[grp] = "Оплата"
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "12:00:00"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "12:01:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон 5000"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	if r1.Recipient == "Исаев Харон" && r2.Recipient == "Исаев Харон" {
		t.Errorf("одна строка «Исаев Харон 5000» назвала оба чека по 5000 (сбор %v)", dayTotals(e, grp))
	}
	if e.lastWith("Чей это чек").ID == "" {
		t.Errorf("про второй чек 5000 не спросили «чей чек?»")
	}
}

// R0 (правило): какой платёж списка может назвать чек — пометка налички
// отдельной строкой или в заголовке блока делает платёж наличным; граница
// неясна — наличка (чек не называем, спросим).
func TestL4naming_R0_CashScopeOfMarkers(t *testing.T) {
	cases := []struct {
		text string
		amt  float64
		want string // "" — чек этим текстом не называем
	}{
		{"Наличка\nИсаев Харон\n10000\n5000", 10000, ""},
		{"Исаев Харон\n10000\n5000\nналичка", 10000, ""},
		{"Исаев Харон у Дени\n10000\n5000", 10000, ""},
		{"Исаев Харон\n10000\n5000\nДеньги у Дени", 10000, ""},
		{"Магомедов Ахмед 15000\nналичка\nИсаев Дени 20000", 15000, ""},
		{"Исаев Харон\n10000\n5000 нал", 10000, "Исаев Харон"},
		{"Магомедов Ахмед 15000\nНаличка:\nИсаев Дени 20000", 15000, "Магомедов Ахмед"},
		{"Магомедов Ахмед 15000\nИсаев Дени 20000 нал", 15000, "Магомедов Ахмед"},
		{"Иванов Иван у Дени\n5000\nПетров Петр\n10000", 10000, "Петров Петр"},
	}
	for _, c := range cases {
		hit, _, ok := nameForCheckAmount(c.text, c.amt)
		got := ""
		if ok {
			got = hit.name
		}
		if got != c.want {
			t.Errorf("%q, чек %v: ФИО %q, ожидали %q", c.text, c.amt, got, c.want)
		}
	}
}

// R1 (очередь имён): «ФИО\nФИО сумма» рядом с чеком на другую сумму — ФИО
// для будущего чека на эту сумму берётся со строки суммы, а не с первой строки.
func TestL4naming_R1_QueuedNameFromAmountLine(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	grp, emp := e2eGroup("sq"), user("79285550113")
	e.groupsCache[grp] = "Оплата"
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("20000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед\nИсаев Харон 15000"))
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:02:00"), "")
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Магомедов Ахмед" {
		t.Errorf("чек 15000 после «Магомедов Ахмед\\nИсаев Харон 15000» → %q (сбор %v)", r.Recipient, dayTotals(e, grp))
	}
}
