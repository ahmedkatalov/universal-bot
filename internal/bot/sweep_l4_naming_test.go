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
