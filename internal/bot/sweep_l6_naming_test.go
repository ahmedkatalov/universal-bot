package bot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Регрессии после правки «несколько сумм и наличка с тем, кто забрал, не
// называют чек»: «ФИО сумма, остаток …» перед чеком ждёт свой чек; ответ
// списком на «чей чек?» закрывает вопрос; правка текста-списка заново читает
// платежи; свайп-разбивка на чек — его подпись; обычные подписи («отдал ещё
// 15000», «взял айфон, первый взнос») — не наличка с тем, кто забрал.

// Finding 16: «ФИО сумма, остаток …» (и список «ФИО 7500 и 5000») перед чеком
// ждёт свой чек в очереди имён — следующее голое имя не забирает этот чек.
func TestL6naming_16_ListTextFirstNextNameStealsCheck(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		text, pays string
		checks     []string // суммы чеков клиента после текста
		want       float64  // сбор Исаева
	}{
		{"Исаев Харон 15000, остаток 30000", `{"name":"Исаев Харон","amount":15000,"cash":false}`, []string{"15000.00"}, 15000},
		{"Исаев Харон 7500 и 5000", `{"name":"Исаев Харон","amount":7500},{"name":"Исаев Харон","amount":5000}`, []string{"7500.00", "5000.00"}, 12500},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{tc.text: tc.pays})
		grp, emp := e2eGroup("lq"), user("79285552008")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		var cks []string
		for _, a := range tc.checks {
			cks = append(cks, e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, a, now.Format("15:04:05")), ""))
		}
		want := tc.want
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
		ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "10000.00", now.Format("15:04:05")), "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		var who []string
		for _, ck := range cks {
			who = append(who, e.db.TestReceiptByMessage(ctx, ck).Recipient)
		}
		r2 := e.db.TestReceiptByMessage(ctx, ck2)
		got := dayTotals(e, grp)
		bad := r2.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != want || got["Магомедов Ахмед"] != 10000
		for _, w := range who {
			bad = bad || w != "Исаев Харон"
		}
		if bad {
			t.Errorf("%q + чеки %v, затем «Магомедов Ахмед» + чек 10000: чеки Исаева → %q, чек 10000 → %q, сбор %v (=%v) — ожидали Исаев %v, Магомедов 10000",
				tc.text, tc.checks, who, r2.Recipient, got, sum(got), want)
		}
	}
}

// Finding 17: ответ без свайпа на «чей чек?» с несколькими суммами назвал чек —
// вопрос закрыт: следующее голое имя того же сотрудника не перезаписывает
// клиента этого чека, а называет следующий чек.
func TestL6naming_17_ListAnswerClosesReceiptAsk(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		ans, pays string
		want      float64
	}{
		{"Исаев Харон 15000 20000", `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`, 45000},
		{"Исаев Харон 15000, остаток 30000", `{"name":"Исаев Харон","amount":15000}`, 25000},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{tc.ans: tc.pays})
		grp, emp := e2eGroup("st"), user("79285552002")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		ckA := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		if e.lastWith("Чей это чек").ID == "" {
			t.Fatalf("бот не спросил «чей чек?»: %+v", e.sent())
		}
		e.sendGroup(groupText(grp, emp, "Сафаи", tc.ans)) // ответ без свайпа
		// следующий платёж того же сотрудника: имя, затем чек
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
		ckB := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "10000.00", now.Format("15:04:05")), "")
		rA, rB := e.db.TestReceiptByMessage(ctx, ckA), e.db.TestReceiptByMessage(ctx, ckB)
		got := dayTotals(e, grp)
		if rA.Recipient != "Исаев Харон" || rB.Recipient != "Магомедов Ахмед" || sum(got) != tc.want {
			t.Errorf("«чей чек?» про 15000, ответ %q, затем «Магомедов Ахмед» + чек 10000: чек 15000 → %q, чек 10000 → %q, сбор %v (=%v, ожидали %v)",
				tc.ans, rA.Recipient, rB.Recipient, got, sum(got), tc.want)
		}
	}
}

// Finding 18: правка в WhatsApp текста-списка, который назвал чек, заново
// читает его платежи (а не снимает их все и только переименовывает чек).
func TestL6naming_18_EditListTextKeepsPayments(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		orig, edited string
		pays         map[string]string
		before, want float64
	}{
		{"Исаев Харон 15000 20000", "Исаев Харон 15000 25000", map[string]string{
			"Исаев Харон 15000 20000": `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`,
			"Исаев Харон 15000 25000": `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":25000}`,
		}, 35000, 40000},
		{"Исаев Харон 15000", "Исаев Харон 15000, Магомедов Ахмед 10000", map[string]string{
			"Исаев Харон 15000":                        `{"name":"Исаев Харон","amount":15000}`,
			"Исаев Харон 15000, Магомедов Ахмед 10000": `{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`,
		}, 15000, 25000},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		names := []string{"Исаев Харон", "Магомедов Ахмед"}
		e.ai = func(c aiCall) aiReply {
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
				best := "" // самый длинный текст из таблицы, что есть в запросе
				for k := range tc.pays {
					if strings.Contains(u, k) && len(k) > len(best) {
						best = k
					}
				}
				return aiReply{Text: `{"payments":[` + tc.pays[best] + `],"clarify":""}`}
			}
			return aiReply{Text: ""}
		}
		grp, emp := e2eGroup("el"), user("79285552007")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		m := groupText(grp, emp, "Сафаи", tc.orig)
		e.sendGroup(m)
		if got := dayTotals(e, grp); sum(got) != tc.before {
			t.Fatalf("до правки %q: сбор %v, ожидали %v", tc.orig, got, tc.before)
		}
		n := len(e.sent())
		e.event(editOf(grp, emp, m.Info.ID, tc.edited))
		got := dayTotals(e, grp)
		r := e.db.TestReceiptByMessage(ctx, ck)
		if sum(got) != tc.want || r.Recipient != "Исаев Харон" {
			t.Errorf("чек 15000 и текст %q исправили на %q: сбор %v (=%v), ожидали %v; чек → %q; бот: %+v",
				tc.orig, tc.edited, got, sum(got), tc.want, r.Recipient, e.sentSince(n))
		}
	}
	l6namingEditListBeforeChecks(t)
}

// Finding 19: свайп на чек с его разбивкой («Исаев Харон 15000 + 20000» на чек
// 35000) — подпись этого чека: один платёж 35000, без вопроса.
func TestL6naming_19_SwipeBreakdownIsCaption(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон 15000 + 20000", "Исаев Харон 15000 20000"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
			text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`,
		})
		grp, emp := e2eGroup("bd"), user("79285552006")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
		e.sendGroup(groupReply(grp, emp, "Сафаи", text, ck, emp, "")) // свайп на сам чек
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("Чей это чек")
		if q.ID != "" {
			e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
		}
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if q.ID != "" || r.Recipient != "Исаев Харон" || got["Исаев Харон"] != 35000 || sum(got) != 35000 {
			t.Errorf("чек 35000, свайп на него %q: вопрос=%q, чек → %q, сбор %v — ожидали молча один платёж Исаева 35000",
				text, q.Text, r.Recipient, got)
		}
	}
}

// Finding 19 (не подпись): свайп на чек с суммами, которые НЕ дают сумму чека,
// или с разными клиентами — по-прежнему список платежей.
func TestL6naming_19_SwipeBreakdownRule(t *testing.T) {
	cases := []struct {
		text string
		amt  float64
		want string
	}{
		{"Исаев Харон 15000 + 20000", 35000, "Исаев Харон"},
		{"Исаев Харон 15000 20000", 35000, "Исаев Харон"},
		{"Исаев Харон\n15000\n20000", 35000, "Исаев Харон"},
		{"Исаев Харон 15000 + 20000", 30000, ""},
		{"Исаев Харон 15000, Магомедов Ахмед 20000", 35000, ""},
		{"Исаев Харон 15000 + 20000 нал", 35000, ""},
		{"15000 Исаев Харон 20000 Магомедов Ахмед", 35000, ""},
	}
	for _, c := range cases {
		got := ""
		if hit, ok := breakdownOfCheck(c.text, c.amt); ok {
			got = hit.name
		}
		if got != c.want {
			t.Errorf("%q на чек %v: подпись %q, ожидали %q", c.text, c.amt, got, c.want)
		}
	}
}

// Finding 20: обычные подписи «отдал ещё 15000», «отдал последние 15000»,
// «взял айфон, первый взнос 15000» — не наличка с тем, кто забрал: чек назван
// молча (и текстом после чека, и текстом перед чеком).
func TestL6naming_20_CollectorWordsNotCash(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		text   string
		before bool
	}{
		{"Исаев Харон отдал ещё 15000", false},
		{"Исаев Харон отдал последние 15000", false},
		{"Исаев Харон взял айфон, первый взнос 15000", false},
		{"Исаев Харон отдал ещё 15000", true},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{tc.text: `{"name":"Исаев Харон","amount":15000,"cash":false}`})
		grp, emp := e2eGroup("fc"), user("79285552003")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		var ck string
		if tc.before {
			e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
			ck = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		} else {
			ck = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
			e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		}
		e.age(grp.String())
		e.clarifyTick(ctx)
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if q := e.lastWith("Чей это чек"); q.ID != "" || r.Recipient != "Исаев Харон" || got["Исаев Харон"] != 15000 {
			t.Errorf("чек 15000 и %q (текст первым=%v): чек → %q (на проверке=%v), сбор %v, вопрос=%q — ожидали молча: чек Исаева, 15000",
				tc.text, tc.before, r.Recipient, r.NeedsReview, got, q.Text)
		}
	}
}

// Finding 20 (правило): «кто забрал» — только когда назван человек: имя перед
// «взял/забрал» или после «отдал/передал» (с «к»), или «мне/ему/ей».
func TestL6naming_20_CollectorCashRule(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Исаев Харон 10000 Мансур взял", true},
		{"Исаев Харон 10000 Адам забрал", true},
		{"Исаев Харон 10000 отдал Адаму", true},
		{"Исаев Харон 10000 передала к Дени", true},
		{"Исаев Харон 10000 отдал мне", true},
		{"Исаев Харон 10000 мансур взял", true},
		{"Исаев Харон отдал Адаму 10000", true},
		{"Исаев Харон\n10000\n5000\nМансур взял", true},
		{"Исаев Харон\n10000\n5000\nотдал Адаму", true},
		{"Мансур собрал", true},
		{"Исаев Харон 10000 забрал Адам", true},
		{"Исаев Харон 10000 Мансур взял себе", true},
		{"Мансур забрал деньги", true},
		{"Исаев Харон 10000\nМансур Исаев взял", true},
		{"Исаев Харон отдал ещё 15000", false},
		{"Исаев Харон отдал последние 15000", false},
		{"Исаев Харон взял айфон, первый взнос 15000", false},
		{"Исаев Харон отдал 15000", false},
		{"Исаев Харон 15000 передал через сбер", false},
		{"Исаев Харон отдал всю сумму 15000", false},
		{"Исаев Харон отдал досрочно 15000", false},
		{"Исаев Харон отдал взнос 15000", false},
		{"Исаев Харон взял телефон 15000", false},
	}
	for _, c := range cases {
		if got := collectorCash(c.text); got != c.want {
			t.Errorf("collectorCash(%q) = %v, ожидали %v", c.text, got, c.want)
		}
	}
}

// Finding 16 (правило): «, остаток 30000» / «долг 30000» после суммы — не
// второй платёж строки.
func TestL6naming_16_BalanceNotSecondPayment(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Исаев Харон 15000, остаток 30000", false},
		{"Исаев Харон 15000 осталось 30000", false},
		{"Исаев Харон 15000, долг 30000", false},
		{"Исаев Харон 15000 из 45000", false},
		{"Исаев Харон 15000 20000", true},
		{"Исаев Харон 15000, Магомедов Ахмед 10000", true},
		{"Исаев Харон 7500 и 5000", true},
	}
	for _, c := range cases {
		if got := severalAmounts(c.text); got != c.want {
			t.Errorf("severalAmounts(%q) = %v, ожидали %v", c.text, got, c.want)
		}
	}
}

// Finding 19 (без свайпа): разбивка суммы ждущего чека сразу после него —
// подпись чека; если же у отправителя ждёт чек на одну из частей — не
// угадываем: чек на 35000 не назван молча, про него спрашивают.
func TestL6naming_19_BreakdownAfterCheckNoSwipe(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000"
	for _, withPart := range []bool{false, true} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
			text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`,
		})
		grp, emp := e2eGroup("bn"), user("79285552016")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
		var part string
		if withPart {
			part = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		}
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		e.age(grp.String())
		e.clarifyTick(ctx)
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		q := e.lastWith("Чей это чек")
		if !withPart && (q.ID != "" || r.Recipient != "Исаев Харон" || sum(got) != 35000) {
			t.Errorf("чек 35000, затем %q: вопрос=%q, чек → %q, сбор %v — ожидали молча Исаев 35000", text, q.Text, r.Recipient, got)
		}
		if withPart {
			rp := e.db.TestReceiptByMessage(ctx, part)
			if r.Recipient == "Исаев Харон" || !strings.Contains(q.Text, "35000") || rp.Recipient != "Исаев Харон" || got["Исаев Харон"] != 35000 {
				t.Errorf("чеки 35000 и 15000, затем %q: чек 35000 → %q, чек 15000 → %q, вопрос=%q, сбор %v — ожидали: чек 15000 Исаева (+20000 текстом), про чек 35000 спросить",
					text, r.Recipient, rp.Recipient, q.Text, got)
			}
		}
	}
}

// Finding 18 (текст перед чеками): список назвал чеки из очереди имён; правка
// суммы второго платежа — платежи читаются заново, чек, который новый текст
// больше не называет, — без клиента (бот спросит), а не на старое имя.
func l6namingEditListBeforeChecks(t *testing.T) {
	ctx := context.Background()
	const orig, edited = "Исаев Харон 15000 20000", "Исаев Харон 15000 25000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{
		orig:   `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`,
		edited: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":25000}`,
	})
	grp, emp := e2eGroup("eb"), user("79285552017")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	m := groupText(grp, emp, "Сафаи", orig)
	e.sendGroup(m)
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "20000.00", now.Format("15:04:05")), "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 35000 {
		t.Fatalf("до правки: сбор %v, ожидали Исаев 35000", got)
	}
	e.event(editOf(grp, emp, m.Info.ID, edited))
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient == "Исаев Харон" || got["Исаев Харон"] != 40000 {
		t.Errorf("%q → %q: чек 15000 → %q, чек 20000 → %q, сбор %v — ожидали Исаев 40000 (15000 чеком + 25000), чек 20000 без клиента",
			orig, edited, r1.Recipient, r2.Recipient, got)
	}
}

// R0: наличка без названного сборщика («принёс», «занёс», «я забрал»,
// «забрал») — наличка: отдельный платёж, чек ею не называется.
func TestL6naming_R0_BroughtCashNamesCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон принёс 10000", "Исаев Харон 10000 занёс", "Исаев Харон 10000 я забрал", "Исаев Харон 10000 забрал"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":10000,"cash":true}`})
		grp, emp := e2eGroup("bc"), user("79285553007")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 10000 || r.Recipient == "Исаев Харон" {
			t.Errorf("чек 10000, затем наличка %q: чек → %q, сбор %v", text, r.Recipient, got)
		}
	}
}

// R0: та же наличка перед чеком — не ждёт чек в очереди имён.
func TestL6naming_R0_BroughtCashBeforeCheckQueued(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон занёс 10000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":10000,"cash":true}`})
	grp, emp := e2eGroup("bq"), user("79285553008")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if r.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != 10000 || got["Магомедов Ахмед"] != 10000 {
		t.Errorf("чек → %q, сбор %v", r.Recipient, got)
	}
}

// R0: та же наличка без свайпа после «чей чек?» — не ответ на вопрос.
func TestL6naming_R0_BroughtCashTakenAsAnswer(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон принёс 10000", "Исаев Харон 10000 отдал адаму"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":10000,"cash":true}`})
		grp, emp := e2eGroup("ba"), user("79285553010")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		if e.lastWith("Чей это чек").ID == "" {
			t.Fatalf("не спросил")
		}
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if strings.Contains(r.Recipient, "Исаев") || got["Исаев Харон"] != 10000 {
			t.Errorf("%q: чек → %q, сбор %v", text, r.Recipient, got)
		}
	}
}

// R1: имя сборщика с маленькой буквы («отдал адаму», «забрал мансур»,
// «передал мансуру») — наличка.
func TestL6naming_R1_LowercaseCollectorCash(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ text, coll string }{
		{"Исаев Харон 10000 отдал адаму", "Адам"},
		{"Исаев Харон 10000 забрал мансур", "Мансур"},
		{"Исаев Харон 10000 передал мансуру", "Мансур"},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{tc.text: `{"name":"Исаев Харон","amount":10000,"cash":true,"collector":"` + tc.coll + `"}`})
		grp, emp := e2eGroup("lc"), user("79285553001")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 10000 || r.Recipient == "Исаев Харон" {
			t.Errorf("чек 10000, затем %q: чек → %q, сбор %v", tc.text, r.Recipient, got)
		}
	}
}

// R2: «+ долг 5000», «и долг 5000 закрыл» — второй платёж, а не пометка остатка.
func TestL6naming_R2_PlusDebtIsPayment(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		text  string
		swipe bool
	}{
		{"Исаев Харон 15000 + долг 5000", true},
		{"Исаев Харон 15000 + долг 5000", false},
		{"Исаев Харон 15000 и долг 5000 закрыл", false},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{tc.text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":5000}`})
		grp, emp := e2eGroup("pd"), user("79285553011")
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
		if tc.swipe {
			e.sendGroup(groupReply(grp, emp, "Сафаи", tc.text, ck, emp, ""))
		} else {
			e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		}
		e.age(grp.String())
		e.clarifyTick(ctx)
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 20000 {
			t.Errorf("%q (свайп=%v): чек → %q, сбор %v", tc.text, tc.swipe, r.Recipient, got)
		}
	}
}

// R3: сводка списком после чеков с подписями не ставит их клиентов в очередь
// имён — следующий чек той же суммы получает своё голое имя.
func TestL6naming_R3_SummaryAfterCaptionedChecksStealsNext(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000\nМагомедов Ахмед 10000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед", "Алиев Руслан"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`})
	grp, emp := e2eGroup("ss"), user("79285553003")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "Исаев Харон")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "10000.00", now.Format("15:04:05")), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck3 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Алиев Руслан"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r3 := e.db.TestReceiptByMessage(ctx, ck3)
	got := dayTotals(e, grp)
	if r3.Recipient != "Алиев Руслан" || got["Исаев Харон"] != 15000 || got["Алиев Руслан"] != 15000 {
		t.Errorf("третий чек → %q, сбор %v", r3.Recipient, got)
	}
}

// R4: список «ФИО сумма остаток X» перед чеками ставит в очередь сумму
// платежа, а не остаток.
func TestL6naming_R4_ListWithBalanceBeforeChecks(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 остаток 30000\nМагомедов Ахмед 10000 остаток 5000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед", "Алиев Руслан"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`})
	grp, emp := e2eGroup("lb"), user("79285553002")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "10000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Алиев Руслан"))
	ck3 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "5000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2, r3 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2), e.db.TestReceiptByMessage(ctx, ck3)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" || r3.Recipient != "Алиев Руслан" || sum(got) != 30000 {
		t.Errorf("чеки → %q, %q, %q; сбор %v", r1.Recipient, r2.Recipient, r3.Recipient, got)
	}
}

// R5: список после двух ждущих чеков называет оба — следующее голое имя
// достаётся следующему чеку.
func TestL6naming_R5_ListAfterTwoChecksNextName(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000\nМагомедов Ахмед 10000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед", "Алиев Руслан"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Магомедов Ахмед","amount":10000}`})
	grp, emp := e2eGroup("l2"), user("79285553005")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "10000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Алиев Руслан"))
	ck3 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "20000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2, r3 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2), e.db.TestReceiptByMessage(ctx, ck3)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" || r3.Recipient != "Алиев Руслан" || sum(got) != 45000 {
		t.Errorf("чеки → %q, %q, %q; сбор %v", r1.Recipient, r2.Recipient, r3.Recipient, got)
	}
}

// R6: разбивка суммы чека, написанная ДО чека («Исаев Харон 15000 + 20000»,
// затем чек 35000) — один платёж 35000, а не 70000.
func TestL6naming_R6_BreakdownTextFirst(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`})
	grp, emp := e2eGroup("bf"), user("79285553004")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID != "" {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	}
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 35000 || q.ID != "" || r.Recipient != "Исаев Харон" {
		t.Errorf("вопрос=%q, чек → %q, сбор %v — ожидали молча: чек Исаева, 35000", q.Text, r.Recipient, got)
	}
}

// R0/R1 (правило): наличка без названного сборщика и имя сборщика с маленькой
// буквы — наличка; взяли вещь или «отдал ещё/последние» — нет.
func TestL6naming_R1_CollectorCashRule(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Исаев Харон принёс 10000", true},
		{"Исаев Харон 10000 занёс", true},
		{"Исаев Харон 10000 я забрал", true},
		{"Исаев Харон 10000 забрал", true},
		{"Исаев Харон 10000 отдал адаму", true},
		{"Исаев Харон 10000 забрал мансур", true},
		{"Исаев Харон 10000 передал мансуру", true},
		{"Исаев Харон 10000 отдал мадине", true},
		{"Исаев Харон 10000 отдал адам", true},
		{"Исаев Харон 15000 взял айфон", false},
		{"Исаев Харон отдал 15000, телефон забрал", false},
		{"Исаев Харон отдал вторую часть 15000", false},
		{"Исаев Харон отдал сразу 15000", false},
		{"Исаев Харон 15000 отдал досрочно", false},
	}
	for _, c := range cases {
		if got := collectorCash(c.text); got != c.want {
			t.Errorf("collectorCash(%q) = %v, ожидали %v", c.text, got, c.want)
		}
	}
}

// R2 (правило): «+ долг 5000», «и долг 5000» — второй платёж строки;
// «, долг 5000», «остаток 30000», «итого» — пометка.
func TestL6naming_R2_DebtPaidRule(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"Исаев Харон 15000 + долг 5000", true},
		{"Исаев Харон 15000 и долг 5000 закрыл", true},
		{"Исаев Харон 15000 плюс остаток 30000", true},
		{"Исаев Харон 15000, долг 5000", false},
		{"Исаев Харон 15000 остаток 30000", false},
		{"Исаев Харон 15000 и осталось 30000", false},
		{"Исаев Харон 15000 + 20000 итого 35000", true},
	}
	for _, c := range cases {
		if got := severalAmounts(c.text); got != c.want {
			t.Errorf("severalAmounts(%q) = %v, ожидали %v", c.text, got, c.want)
		}
	}
	// «итого 35000» — не третий платёж.
	if pays := textPays("Исаев Харон 15000 + 20000 итого 35000"); len(pays) != 2 {
		t.Errorf("«15000 + 20000 итого 35000»: платежи %+v, ожидали два", pays)
	}
}

// R4 (правило): платёж строки «ФИО сумма остаток X» — сумма, а не остаток.
func TestL6naming_R4_BalanceLineAmount(t *testing.T) {
	pays := textPays("Исаев Харон 15000 остаток 30000\nМагомедов Ахмед 10000 остаток 5000")
	var got []float64
	for _, p := range pays {
		got = append(got, p.amount)
	}
	if len(got) != 2 || got[0] != 15000 || got[1] != 10000 {
		t.Errorf("суммы платежей %v, ожидали [15000 10000]", got)
	}
	if _, _, ok := nameForCheckAmount("Исаев Харон 15000 остаток 30000", 30000); ok {
		t.Errorf("остаток 30000 назвал чек на 30000")
	}
}

// R3: одиночный «ФИО сумма» после чека с подписью того же клиента на ту же
// сумму — тот же платёж: в очередь имён не встаёт, следующий чек той же суммы
// получает своё голое имя.
func TestL6naming_R3_SingleAfterCaptionedCheck(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Алиев Руслан"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000}`})
	grp, emp := e2eGroup("sc"), user("79285553012")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "Исаев Харон")
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Алиев Руслан"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r2 := e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r2.Recipient != "Алиев Руслан" || got["Исаев Харон"] != 15000 || got["Алиев Руслан"] != 15000 {
		t.Errorf("второй чек → %q, сбор %v — ожидали Алиев, Исаев 15000 + Алиев 15000", r2.Recipient, got)
	}
}

// R6: разбивка до чека, но пришли чеки на части — каждый берёт свою часть
// (разбивка не мешает обычному пути).
func TestL6naming_R6_BreakdownTextFirstPartChecks(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`})
	grp, emp := e2eGroup("bp"), user("79285553013")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "20000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Исаев Харон" || got["Исаев Харон"] != 35000 || e.lastWith("Чей это чек").ID != "" {
		t.Errorf("чеки → %q, %q, сбор %v", r1.Recipient, r2.Recipient, got)
	}
}

// R7: «ФИО сумма, остаток X» не называет ждущий чек на сумму остатка X: чек
// другого клиента не достаётся этому клиенту, а его платёж — своему чеку.
func TestL6naming_R7_BalanceNamesOtherCheck(t *testing.T) {
	ctx := context.Background()
	for _, text := range []string{"Исаев Харон 15000, остаток 30000", "Исаев Харон 15000 из 45000", "Исаев Харон 15000 (было 20000)"} {
		other := 30000.0
		switch {
		case strings.Contains(text, "45000"):
			other = 45000
		case strings.Contains(text, "20000"):
			other = 20000
		}
		otherStr := map[float64]string{30000: "30000.00", 45000: "45000.00", 20000: "20000.00"}[other]
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000}`})
		grp, emp := e2eGroup("bo"), user("79285556005")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		ckA := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, otherStr, now.Format("15:04:05")), "")
		ckB := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
		e.age(grp.String())
		e.clarifyTick(ctx)
		rA, rB := e.db.TestReceiptByMessage(ctx, ckA), e.db.TestReceiptByMessage(ctx, ckB)
		got := dayTotals(e, grp)
		if rA.Recipient != "Магомедов Ахмед" || rB.Recipient != "Исаев Харон" || got["Исаев Харон"] != 15000 || got["Магомедов Ахмед"] != other {
			t.Errorf("%q: чек %v → %q, чек 15000 → %q, сбор %v — ожидали Магомедов %v, Исаев 15000", text, other, rA.Recipient, rB.Recipient, got, other)
		}
	}
}

// R7: ждущий чек другого клиента на сумму остатка, затем «ФИО сумма, остаток
// X», затем чек клиента: чек остатка ему не достаётся.
func TestL6naming_R7_BalanceCaptionsBalanceCheck(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000, остаток 30000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000}`})
	grp, emp := e2eGroup("bp"), user("79285556006")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	ckA := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "30000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ckB := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	rA, rB := e.db.TestReceiptByMessage(ctx, ckA), e.db.TestReceiptByMessage(ctx, ckB)
	got := dayTotals(e, grp)
	if rA.Recipient == "Исаев Харон" || rB.Recipient != "Исаев Харон" || got["Исаев Харон"] != 15000 {
		t.Errorf("чек 30000 → %q, чек 15000 → %q, сбор %v", rA.Recipient, rB.Recipient, got)
	}
}

// R7 (правило): платёж «ФИО сумма» с пометкой остатка — сумма пары, а не
// остаток («15000 из 45000», «15000 (было 20000)»).
func TestL6naming_R7_BalancePairAmountRule(t *testing.T) {
	for _, tc := range []struct {
		text  string
		other float64
	}{
		{"Исаев Харон 15000, остаток 30000", 30000},
		{"Исаев Харон 15000 из 45000", 45000},
		{"Исаев Харон 15000 (было 20000)", 20000},
	} {
		if src, _, _, ok := checkNameSource(tc.text, tc.other); ok {
			t.Errorf("%q называет чек на %v (источник %q)", tc.text, tc.other, src)
		}
		if _, _, _, ok := checkNameSource(tc.text, 15000); !ok {
			t.Errorf("%q не называет чек на 15000", tc.text)
		}
		if amountLinesMatch(tc.text, tc.other) {
			t.Errorf("%q: остаток %v читается платежом", tc.text, tc.other)
		}
		// ФИО платежа в многострочном тексте — у строки суммы, а не первая строка-имя.
		if hit, ok := amountLinePayer("Магомедов Ахмед\n" + tc.text); !ok || hit.name != "Исаев Харон" {
			t.Errorf("«Магомедов Ахмед\\n%s»: ФИО платежа %q (%v), ожидали Исаев Харон", tc.text, hit.name, ok)
		}
	}
}

// R8: итог рядом с частями («15000 + 20000 = 35000», «35000 (15000+20000)»)
// — части не ставятся в очередь имён платежами клиента: следующий чек другого
// клиента получает своё имя.
func TestL6naming_R8_TotalEqualsPhantomQueue(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		text      string
		textFirst bool
	}{
		{"Исаев Харон 15000 + 20000 = 35000", false},
		{"Исаев Харон 35000 (15000+20000)", false},
		{"Исаев Харон 15000 + 20000 = 35000", true},
		{"Исаев Харон 35000 (15000+20000)", true},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{tc.text: `{"name":"Исаев Харон","amount":35000}`})
		grp, emp := e2eGroup("pq"), user("79285556001")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		var ck1 string
		if tc.textFirst {
			e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
			ck1 = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
		} else {
			ck1 = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
			e.sendGroup(groupText(grp, emp, "Сафаи", tc.text))
		}
		ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
		e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
		e.age(grp.String())
		e.clarifyTick(ctx)
		r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
		got := dayTotals(e, grp)
		if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != 35000 || got["Магомедов Ахмед"] != 15000 {
			t.Errorf("%q (текст первым=%v): чек 35000 → %q, чек 15000 → %q, сбор %v", tc.text, tc.textFirst, r1.Recipient, r2.Recipient, got)
		}
	}
}

// R9: блок «ФИО\nсумма\nостаток X» — строка остатка не платёж: не ждёт чек
// на X в очереди имён.
func TestL6naming_R9_BlockBalancePhantomQueue(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон\n15000\nостаток 30000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000}`})
	grp, emp := e2eGroup("bb"), user("79285556003")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "30000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != 15000 || got["Магомедов Ахмед"] != 30000 {
		t.Errorf("чек 15000 → %q, чек 30000 → %q, сбор %v", r1.Recipient, r2.Recipient, got)
	}
}

// R9 (правило): строка остатка в блоке — не платёж, и не часть разбивки чека.
func TestL6naming_R9_BlockBalanceRule(t *testing.T) {
	const text = "Исаев Харон\n15000\nостаток 30000"
	pays := textPays(text)
	if len(pays) != 1 || pays[0].amount != 15000 {
		t.Errorf("платежи %+v, ожидали один на 15000", pays)
	}
	if _, ok := breakdownOfCheck(text, 45000); ok {
		t.Errorf("15000 и остаток 30000 прочитаны разбивкой чека на 45000")
	}
}

// R10: подпись чека одной суммы снимает из очереди запись клиента на ЭТУ
// сумму, а не первую по имени: чек другой суммы списка ещё получает клиента.
func TestL6naming_R10_ListQueueConsumedWrongAmount(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 7500 и 5000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":7500},{"name":"Исаев Харон","amount":5000}`})
	grp, emp := e2eGroup("lw"), user("79285556004")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ckA := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "5000.00", now.Format("15:04:05")), "Исаев Харон")
	ckB := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "7500.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	ckC := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "5000.00", now.Format("15:04:05")), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	rA, rB, rC := e.db.TestReceiptByMessage(ctx, ckA), e.db.TestReceiptByMessage(ctx, ckB), e.db.TestReceiptByMessage(ctx, ckC)
	got := dayTotals(e, grp)
	if rA.Recipient != "Исаев Харон" || rB.Recipient != "Исаев Харон" || rC.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != 12500 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("чеки → %q, %q, %q; сбор %v", rA.Recipient, rB.Recipient, rC.Recipient, got)
	}
}

// R11: свайп на чек с его разбивкой и отдельной наличкой («Исаев Харон 15000
// + 20000\n5000 нал» на чек 35000) — чек называется разбивкой, наличка
// записывается отдельно: 40000, а не 75000.
func TestL6naming_R11_SwipeBreakdownPlusCash(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000\n5000 нал"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000},{"name":"Исаев Харон","amount":5000,"cash":true}`})
	grp, emp := e2eGroup("bc"), user("79285556002")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupReply(grp, emp, "Сафаи", text, ck, emp, ""))
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID != "" {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	}
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if r.Recipient != "Исаев Харон" || got["Исаев Харон"] != 40000 {
		t.Errorf("вопрос=%q, чек → %q, сбор %v — ожидали Исаев 40000", q.Text, r.Recipient, got)
	}
}

// R12: наличка с полным именем сборщика после глагола («… 10000 забрал
// Мансур Алиев») — наличка: отдельный платёж, чек ею не называется.
func TestL6naming_R12_CollectorFullNameAfterVerb(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 10000 забрал Мансур Алиев"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":10000,"cash":true,"collector":"Мансур"}`})
	grp, emp := e2eGroup("cf"), user("79285556007")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("10000.00", "12:00:00"), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	r := e.db.TestReceiptByMessage(ctx, ck)
	got := dayTotals(e, grp)
	if r.Recipient == "Исаев Харон" || got["Исаев Харон"] != 10000 {
		t.Errorf("collectorCash=%v, чек → %q, сбор %v — ожидали наличку отдельно, чек без клиента", collectorCash(text), r.Recipient, got)
	}
	for _, s := range []string{"Исаев Харон 10000 забрал Мансур Алиев", "Исаев Харон 10000 отдал Мансуру Алиеву", "Исаев Харон 10000 взял Адам"} {
		if !collectorCash(s) {
			t.Errorf("collectorCash(%q) = false", s)
		}
	}
}

// R10: разбивка до чека («Исаев Харон 15000 + 20000»), затем чек на всю
// сумму с подписью клиента — снимает из очереди обе части (чек другого
// клиента на часть их не получает) и не считается дважды.
func TestL6naming_R10_CaptionedCheckTakesBreakdownParts(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000"
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5namingAI([]string{"Исаев Харон", "Магомедов Ахмед"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000}`})
	grp, emp := e2eGroup("cb"), user("79285556008")
	e.groupsCache[grp] = "Оплата"
	now := time.Now()
	e.sendGroup(groupText(grp, emp, "Сафаи", text))
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "Исаев Харон")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "15000.00", now.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	got := dayTotals(e, grp)
	if r1.Recipient != "Исаев Харон" || r2.Recipient != "Магомедов Ахмед" || got["Исаев Харон"] != 35000 || got["Магомедов Ахмед"] != 15000 {
		t.Errorf("чек 35000 → %q, чек 15000 → %q, сбор %v — ожидали Исаев 35000, Магомедов 15000", r1.Recipient, r2.Recipient, got)
	}
}

// R11: разбивка чека и наличка без свайпа (после чека) и до чека — чек
// называется разбивкой, наличка записывается отдельно: 40000.
func TestL6naming_R11_BreakdownPlusCashNoSwipe(t *testing.T) {
	ctx := context.Background()
	const text = "Исаев Харон 15000 + 20000\n5000 нал"
	for _, textFirst := range []bool{false, true} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5namingAI([]string{"Исаев Харон"}, map[string]string{text: `{"name":"Исаев Харон","amount":15000},{"name":"Исаев Харон","amount":20000},{"name":"Исаев Харон","amount":5000,"cash":true}`})
		grp, emp := e2eGroup("bn"), user("79285556009")
		e.groupsCache[grp] = "Оплата"
		now := time.Now()
		var ck string
		if textFirst {
			e.sendGroup(groupText(grp, emp, "Сафаи", text))
			ck = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
		} else {
			ck = e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(now, "35000.00", now.Format("15:04:05")), "")
			e.sendGroup(groupText(grp, emp, "Сафаи", text))
		}
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("Чей это чек")
		r := e.db.TestReceiptByMessage(ctx, ck)
		got := dayTotals(e, grp)
		if q.ID != "" || r.Recipient != "Исаев Харон" || got["Исаев Харон"] != 40000 {
			t.Errorf("текст первым=%v: вопрос=%q, чек → %q, сбор %v — ожидали молча Исаев 40000", textFirst, q.Text, r.Recipient, got)
		}
	}
}

// R11 (правило): разбивка чека не считает наличку частью чека.
func TestL6naming_R11_BreakdownIgnoresCashRule(t *testing.T) {
	if hit, ok := breakdownOfCheck("Исаев Харон 15000 + 20000\n5000 нал", 35000); !ok || hit.name != "Исаев Харон" {
		t.Errorf("разбивка 15000 + 20000 с наличкой 5000 не подписала чек 35000")
	}
	if _, ok := breakdownOfCheck("Исаев Харон 15000 + 20000\n5000 нал", 40000); ok {
		t.Errorf("наличка 5000 посчитана частью чека 40000")
	}
	if _, ok := breakdownOfCheck("Исаев Харон 35000\n5000 нал", 35000); ok {
		t.Errorf("одна сумма с наличкой — не разбивка")
	}
}

// R8 (правило): итог частей одного ФИО («= 35000», «35000 (15000+20000)») —
// не платёж; сумма, прибавленная союзом, и суммы разных ФИО — платежи.
func TestL6naming_R8_TotalOfPartsRule(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []float64
	}{
		{"Исаев Харон 15000 + 20000 = 35000", []float64{15000, 20000}},
		{"Исаев Харон 35000 (15000+20000)", []float64{15000, 20000}},
		{"Исаев Харон 5000 + 10000 + 15000", []float64{5000, 10000, 15000}},
		{"Исаев Харон 15000, Магомедов Ахмед 20000, Алиев Руслан 35000", []float64{15000, 20000, 35000}},
	} {
		var got []float64
		for _, p := range lineAmountPairs(tc.line, "") {
			got = append(got, p.amount)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q: платежи %v, ожидали %v", tc.line, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q: платежи %v, ожидали %v", tc.line, got, tc.want)
				break
			}
		}
	}
}
