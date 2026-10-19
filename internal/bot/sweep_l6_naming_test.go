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
