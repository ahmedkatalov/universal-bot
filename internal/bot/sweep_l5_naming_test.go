package bot

import (
	"context"
	"strings"
	"testing"
)

// Регрессии: текст с несколькими суммами (в одной строке или в нескольких) —
// это платежи, а не подпись к чеку; наличка с «кто забрал» («Мансур взял»,
// «отдал Адаму») — тоже платёж, а не подпись.

// Finding 21: одна строка с несколькими суммами после чека без подписи — все
// платежи записаны, чек подписан только парой ФИО+сумма на его сумму.
func TestL5naming_21_OneLineSeveralAmountsAfterCheck(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		checks   []string
		text     string
		client   string
		payments string
		want     float64
	}{
		{[]string{"15000.00"}, "Исаев Харон 15000 20000", "Исаев Харон",
			`{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`, 35000},
		{[]string{"15000.00"}, "Исаев Харон 15000 + 20000", "Исаев Харон",
			`{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`, 35000},
		{[]string{"15000.00"}, "Исаев Харон 15000, Магомедов Ахмед 10000", "Исаев Харон",
			`{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`, 25000},
		{[]string{"7500.00", "5000.00"}, "Магомедов Ахмед 7500 и 5000", "Магомедов Ахмед",
			`{"name":"Магомедов Ахмед","amount":7500},{"name":"Магомедов Ахмед","amount":5000}`, 12500},
	}
	for _, tc := range cases {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				return aiReply{Text: `{"client":"` + tc.client + `"}`}
			}
			if strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: `{"payments":[` + tc.payments + `],"clarify":""}`}
			}
			return aiReply{Text: ""}
		}
		grp, emp := e2eGroup("one"), user("79285551001")
		e.groupsCache[grp] = "Оплата"
		var cks []string
		for k, a := range tc.checks {
			cks = append(cks, e.sendReceipt(grp, emp, "Сафаи", sberCheck(a, "12:0"+string(rune('0'+k))+":00"), ""))
		}
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		got := dayTotals(e, grp)
		// честный вопрос про это сообщение тоже допустим вместо молчаливой потери
		if sum(got) != tc.want && len(e.sentSince(n)) == 0 {
			var who []string
			for _, ck := range cks {
				who = append(who, e.db.TestReceiptByMessage(ctx, ck).Recipient)
			}
			t.Errorf("чеки %v без подписи, затем %q: сбор %v (%v), ожидали %v; чеки → %q; бот написал %d сообщ.",
				tc.checks, tc.text, sum(got), got, tc.want, who, len(e.sentSince(n)))
		}
	}
}

// Finding 22: наличка с тем, кто её забрал («Мансур взял», «отдал Адаму»,
// «Адам забрал»), без слова «нал» — отдельный платёж, а не подпись к чеку на
// ту же сумму.
func TestL5naming_22_CollectorCashDoesNotNameCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон 10000 Мансур взял", "Исаев Харон 10000 отдал Адаму", "Исаев Харон 10000 Адам забрал"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			u := c.lastUserText()
			if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				if strings.Contains(u, "Исаев") {
					return aiReply{Text: `{"client":"Исаев Харон"}`}
				}
				return aiReply{Text: `{"client":""}`}
			}
			if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(u, "Исаев") {
				coll := "Мансур"
				if strings.Contains(u, "Адам") {
					coll = "Адам"
				}
				return aiReply{Text: `{"payments":[{"name":"Исаев Харон","amount":10000,"cash":true,"collector":"` + coll + `"}],"clarify":""}`}
			}
			return aiReply{Text: ""}
		}
		grp, emp := e2eGroup("col"), user("79285551004")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 10000 || r.Recipient == "Исаев Харон" {
			t.Errorf("чек 10000 без подписи, затем наличка %q: чек отдан %q, у Исаева %v (%v) — ожидали наличку Исаева 10000 отдельно, чек — без клиента («чей чек?»)",
				text, r.Recipient, got["Исаев Харон"], got)
		}
	}
}

// l5namingAI — фейковый ИИ, как настоящая модель: имя клиента — первое из
// names, встреченное в тексте; платежи — по таблице «текст → платежи» (как их
// прочитала бы модель), прочий текст — без платежей.
func l5namingAI(names []string, payments map[string]string) func(c aiCall) aiReply {
	return func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			for _, n := range names {
				if strings.Contains(u, strings.Fields(n)[0]) {
					return aiReply{Text: `{"client":"` + n + `"}`}
				}
			}
			return aiReply{Text: `{"client":""}`}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			for text, ps := range payments {
				if strings.Contains(u, text) {
					return aiReply{Text: `{"payments":[` + ps + `],"clarify":""}`}
				}
			}
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
}

// Finding 21 (свайп на чек): одна строка с двумя клиентами — чек подписан
// парой на его сумму, второй платёж записан.
func TestL5naming_21_SwipeOneLineList(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000, Магомедов Ахмед 10000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{
		text: `{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`,
	})
	grp, emp := e2eGroup("sw"), user("79285551011")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e.sendGroup(groupReply(grp, emp, "Сафаи", text, ck, emp, ""))
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if r.Recipient != "Исаев Харон" || got["Исаев Харон"] != 15000 || got["Магомедов Ахмед"] != 10000 {
		t.Errorf("чек 15000, свайп %q: чек → %q, сбор %v (ожидали Исаев 15000 чеком, Магомедов 10000)", text, r.Recipient, got)
	}
}

// Finding 21 (текст перед чеками): «ФИО 7500 и 5000», затем два чека — оба
// чека молча подписаны своими парами, без «чей чек?», в сборе 12500.
func TestL5naming_21_TextBeforeChecksNamedByPairs(t *testing.T) {
	ctx := context.Background()
	const text = "Магомедов Ахмед 7500 и 5000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Магомедов Ахмед"}, map[string]string{
		text: `{"name":"Магомедов Ахмед","amount":7500},{"name":"Магомедов Ахмед","amount":5000}`,
	})
	grp, emp := e2eGroup("tb"), user("79285551012")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("7500.00", "12:00:00"), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000.00", "12:01:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if q := e.lastWith("Чей это чек"); q.ID != "" || r1.Recipient != "Магомедов Ахмед" || r2.Recipient != "Магомедов Ахмед" || sum(got) != 12500 {
		t.Errorf("%q, затем чеки 7500 и 5000: чек 7500 → %q, чек 5000 → %q, вопрос=%q, сбор %v (ожидали 12500)", text, r1.Recipient, r2.Recipient, q.Text, got)
	}
}

// Finding 21 (ответ без свайпа на «чей чек?»): строка с двумя суммами — это
// платежи, а не ответ: обе суммы в сборе, чек подписан парой на его сумму.
func TestL5naming_21_ContextAnswerSeveralAmounts(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 20000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
		text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`,
	})
	grp, emp := e2eGroup("ca"), user("79285551013")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	if e.lastWith("Чей это чек").ID == "" {
		t.Fatalf("бот не спросил «чей чек?»: %+v", e.sent())
	}
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	r := e.db.TestReceiptByMessage(ctx, ck)
	if got := dayTotals(e, grp); sum(got) != 35000 || r.Recipient != "Исаев Харон" {
		t.Errorf("«чей чек?» про 15000, ответ %q без свайпа: чек → %q, сбор %v (ожидали 35000)", text, r.Recipient, got)
	}
}

// Finding 22 (текст перед чеком): наличка «… Мансур взял» не ждёт чек в
// очереди имён — чек на ту же сумму не назван, про него спрашивают.
func TestL5naming_22_CollectorCashBeforeCheckNotQueued(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 10000 Мансур взял"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
		text: `{"name":"Исаев Харон","amount":10000,"cash":true,"collector":"Мансур"}`,
	})
	grp, emp := e2eGroup("cq"), user("79285551014")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if r.Recipient == "Исаев Харон" || got["Исаев Харон"] != 10000 {
		t.Errorf("наличка %q, затем чек 10000: чек → %q, сбор %v (ожидали наличку Исаева 10000 отдельно)", text, r.Recipient, got)
	}
	if e.lastWith("Чей это чек").ID == "" {
		t.Errorf("про чек 10000 не спросили «чей чек?»")
	}
}

// Finding 22 (ответ без свайпа на «чей чек?»): наличка с тем, кто забрал, — не
// ответ про чек, а отдельный платёж.
func TestL5naming_22_ContextAnswerCollectorCash(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 10000 Мансур взял"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
		text: `{"name":"Исаев Харон","amount":10000,"cash":true,"collector":"Мансур"}`,
	})
	grp, emp := e2eGroup("cc"), user("79285551015")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	if e.lastWith("Чей это чек").ID == "" {
		t.Fatalf("бот не спросил «чей чек?»: %+v", e.sent())
	}
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if strings.Contains(r.Recipient, "Исаев") || got["Исаев Харон"] != 10000 {
		t.Errorf("«чей чек?» про 10000, затем %q без свайпа: чек → %q, сбор %v (ожидали наличку Исаева 10000, чек без клиента)", text, r.Recipient, got)
	}
}

// Правило (21, 22): какое ФИО соседнего текста называет чек на сумму.
// Несколько сумм в строке — только ФИО перед суммой чека; наличка с тем, кто
// забрал («Мансур взял», «отдал Адаму»), — не называет; неясно — "" (спросим).
func TestL5naming_CheckNameFromText(t *testing.T) {
	cases := []struct {
		text string
		amt  float64
		want string
	}{
		{"Исаев Харон 15000 20000", 20000, "Исаев Харон"},
		{"Исаев Харон 15000 + 20000", 20000, "Исаев Харон"},
		{"Исаев Харон 15000, Магомедов Ахмед 10000", 10000, "Магомедов Ахмед"},
		{"Исаев Харон 15000, Магомедов Ахмед 10000", 15000, "Исаев Харон"},
		{"Магомедов Ахмед 7500 и 5000", 5000, "Магомедов Ахмед"},
		{"Исаев Харон\n15000 + 20000", 20000, "Исаев Харон"},
		{"15000 Исаев Харон 20000 Магомедов Ахмед", 20000, ""},
		{"Исаев Харон 15000 20000 нал", 15000, ""},
		{"Исаев Харон 15000 нал, Магомедов Ахмед 10000", 10000, ""},
		{"Наличка\nИсаев Харон 15000 20000", 15000, ""},
		{"Исаев Харон\n10000\n5000\nМансур взял", 10000, ""},
		{"Исаев Харон\n10000\n5000\nотдал Адаму", 10000, ""},
		{"Исаев Харон отдал Адаму 10000", 10000, ""},
		{"Исаев Харон 10000 Мансур взял", 10000, ""},
		{"Исаев Харон 10000 отдал Адаму", 10000, ""},
		{"Исаев Харон 10000 Адам забрал", 10000, ""},
		// не наличка и не список — называет чек, как раньше
		{"Исаев Харон 15000 (15т)", 15000, "Исаев Харон"},
		{"Исаев Харон отдал 15000", 15000, "Исаев Харон"},
		{"Исаев Харон 15000 передал через сбер", 15000, "Исаев Харон"},
		{"Исаев Харон за 2 месяца 30000", 30000, "Исаев Харон"},
	}
	for _, c := range cases {
		// ФИО из текста-источника (дальше его подтверждает ИИ/список клиентов).
		got := ""
		if src, _, _, ok := checkNameSource(c.text, c.amt); ok {
			got, _ = looksLikeName(src)
		}
		if c.want == "" && got != "" || c.want != "" && !strings.HasPrefix(got, c.want) {
			t.Errorf("%q, чек %v: ФИО %q, ожидали %q", c.text, c.amt, got, c.want)
		}
	}
}
