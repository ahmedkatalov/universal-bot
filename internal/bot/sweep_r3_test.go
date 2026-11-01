package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Регрессии третьего прогона.

// 1. An edited answer is only re-read as a client name: a fixed amount or a switch from «тот же» to «новый» is dropped, and the bot still says «Имя исправили»

// imports: context, strings, testing, time
func TestR3_01_F1_EditedAnswerAmountOrDecisionIgnored(t *testing.T) {
	ctx := context.Background()
	clean := func(e *e2eBot) { // TestResetAll не чистит amount_corrections
		for id := 1; id <= 300; id++ {
			_, _ = e.db.RevertCorrections(ctx, id)
		}
	}
	botSince := func(e *e2eBot, n int) []string {
		var out []string
		for _, m := range e.sentSince(n) {
			out = append(out, m.Text)
		}
		return out
	}
	{
		grp, emp := e2eGroup("zzf1a"), user("79001119401")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		clean(e)
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		unreadable := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:10:00" +
			"\nНомер операции в СБП\n" + e2eID("OP")
		ck := e.sendReceipt(grp, emp, "Сафаи", unreadable, "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("Не разобрал чек")
		if q.ID == "" || q.Quoted != ck || !strings.Contains(q.Text, "ФИО клиента и сумму") {
			t.Fatalf("(a) бот не спросил про нечитаемый чек: %+v", e.sent())
		}
		ans := groupReply(grp, emp, "Сафаи", "Магомедов Ахмед 150000", q.ID, e.botJID(), q.Text)
		e.sendGroup(ans)
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 150000 {
			t.Fatalf("(a) ответ не применён: %v", got)
		}
		n := len(e.sent())
		e.event(editOf(grp, emp, ans.Info.ID, "Магомедов Ахмед 15000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 {
			t.Errorf("(a) сумму в ответе исправили 150000→15000, а в сборе %v; бот: %q", got, botSince(e, n))
		}
	}
	{
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
		clean(e)
		e.ai = aiNamesAndPayments("Ислам Хасанов")
		grp := e2eGroup("zzf1b")
		e.groupsCache[grp] = "Оплата"
		emp, owner := user("79001119402"), user("79990001111")
		e.sendGroup(groupText(grp, emp, "Мансур", "Ислам Хасанов 27000\nналичка у Дени"))
		e.sendGroup(groupText(grp, emp, "Мансур", "Ислам Хасанов 27000\nналичка у Дени"))
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("НОВЫЙ платёж")
		if q.ID == "" {
			t.Fatalf("(b) нет вопроса про повтор: %+v", e.sent())
		}
		ans := groupReply(grp, owner, "Ахмед", "тот же", q.ID, e.botJID(), q.Text)
		e.sendGroup(ans)
		if got := dayTotals(e, grp); got["Ислам Хасанов"] != 27000 {
			t.Fatalf("(b) «тот же»: %v", got)
		}
		n := len(e.sent())
		e.event(editOf(grp, owner, ans.Info.ID, "новый"))
		if got := dayTotals(e, grp); got["Ислам Хасанов"] != 54000 {
			t.Errorf("(b) ответ «тот же» исправили на «новый», а в сборе %v (хочу 54000); бот: %q", got, botSince(e, n))
		}
	}
}

// 2. «не 5000р а 6000р» is not recognised as a correction: it is silently lost, or recorded as a second payment when a surname is given

// imports: testing
func TestR3_02_F2_RubleSuffixCorrectionNotRecognized(t *testing.T) {
	for _, txt := range []string{"не 5000р а 6000р", "Магомедов не 5000р а 6000р"} {
		grp, snd := e2eGroup("zzf2"), user("79001119403")
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		n := len(e.sent())
		e.sendGroup(groupText(grp, snd, "Сафаи", txt))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 || sum(got) != 6000 {
			var bot []string
			for _, m := range e.sentSince(n) {
				bot = append(bot, m.Text)
			}
			t.Errorf("%q: сбор %v, хочу Магомедов Ахмед=6000; бот: %q", txt, got, bot)
		}
	}
}

// 3. An employee's «Джарвис, ФИО сумма» payment can't be fixed by editing it: the edit is ignored

// imports: strings, testing
func TestR3_03_F3_EditOfAddressedEmployeePaymentIgnored(t *testing.T) {
	grp, emp := e2eGroup("zzf3"), user("79001119404")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	pay := aiNamesAndPayments("Каталов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			return pay(c)
		}
		return aiReply{Text: "Записывать платежи может только владелец 🙂"}
	}
	m := groupText(grp, emp, "Сафаи", "Джарвис, Каталов Ахмед 150000")
	e.sendGroup(m)
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 150000 {
		t.Fatalf("платёж «Джарвис, …» не записан: %v", got)
	}
	n := len(e.sent())
	e.event(editOf(grp, emp, m.Info.ID, "Джарвис, Каталов Ахмед 15000"))
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 15000 || sum(got) != 15000 {
		var bot []string
		for _, s := range e.sentSince(n) {
			bot = append(bot, s.Text)
		}
		t.Errorf("сотрудник исправил 150000→15000 правкой, а в сборе %v; бот: %q", got, bot)
	}
}

// 4. Editing the answer to «❓ чей платёж?»: the bot says «Имя исправили — теперь клиент Исаев Харон», but the payment stays on the old client

// imports: testing
func TestR3_04_F4_EditedPaymentWhoAnswerFalseSuccess(t *testing.T) {
	grp, snd := e2eGroup("zzf4"), user("79001119405")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	e.sendGroup(groupText(grp, snd, "Сафаи", "27000 наличка"))
	q := e.lastWith("❓")
	if q.ID == "" {
		t.Fatalf("нет ❓: %+v", e.sent())
	}
	ans := groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 {
		t.Fatalf("ответ не записан: %v", got)
	}
	n := len(e.sent())
	e.event(editOf(grp, snd, ans.Info.ID, "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 27000 || sum(got) != 27000 {
		var bot []string
		for _, s := range e.sentSince(n) {
			bot = append(bot, s.Text)
		}
		t.Errorf("правка ответа на ❓: сбор %v, хочу Исаев Харон=27000; бот: %q", got, bot)
	}
}

// 5. An edited «не X а Y» correction goes back to the payments the old text hit: a new client name or a new old amount is ignored, so the wrong payment gets changed

// imports: context, testing
func TestR3_05_F5_EditedCorrectionKeepsOldTargets(t *testing.T) {
	ctx := context.Background()
	clean := func(e *e2eBot) { // TestResetAll не чистит amount_corrections
		for id := 1; id <= 300; id++ {
			_, _ = e.db.RevertCorrections(ctx, id)
		}
	}
	{
		grp, snd := e2eGroup("zzf5a"), user("79001119406")
		e := newE2E(t, e2eOpts{WithAI: true})
		clean(e)
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон 5000"))
		c := groupText(grp, snd, "Сафаи", "Магомедов не 5000 а 6000")
		e.sendGroup(c)
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 {
			t.Fatalf("(a) поправка не применилась: %v", got)
		}
		e.event(editOf(grp, snd, c.Info.ID, "Исаев не 5000 а 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || got["Исаев Харон"] != 6000 {
			t.Errorf("(a) поправку исправили на «Исаев не 5000 а 6000»: сбор %v, хочу Магомедов=5000, Исаев=6000; бот: %q", got, e.last().Text)
		}
	}
	{
		grp, snd := e2eGroup("zzf5b"), user("79001119407")
		e := newE2E(t, e2eOpts{WithAI: true})
		clean(e)
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон 7000"))
		c := groupText(grp, snd, "Сафаи", "не 5000 а 8000")
		e.sendGroup(c)
		e.event(editOf(grp, snd, c.Info.ID, "не 7000 а 8000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || got["Исаев Харон"] != 8000 {
			t.Errorf("(b) поправку исправили на «не 7000 а 8000»: сбор %v, хочу Магомедов=5000, Исаев=8000; бот: %q", got, e.last().Text)
		}
	}
}

// 6. How ParseCorrection reads names: a lowercase surname kills the correction silently, and a capitalised non-name word («Нал», «Перевод», «Сегодня») is taken as the client

// imports: strings, testing
func TestR3_06_F6_CorrectionNameHeuristics(t *testing.T) {
	{
		grp, snd := e2eGroup("zzf6a"), user("79001119408")
		e := newE2E(t, e2eOpts{WithAI: true})
		base := aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(c.lastUserText(), "не 5000 а 6000") {
				return aiReply{Text: `{"payments":[],"clarify":""}`} // это поправка, а не новый платёж
			}
			return base(c)
		}
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон 5000"))
		n := len(e.sent())
		e.sendGroup(groupText(grp, snd, "Сафаи", "магомедов не 5000 а 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 || got["Исаев Харон"] != 5000 {
			var bot []string
			for _, m := range e.sentSince(n) {
				bot = append(bot, m.Text)
			}
			t.Errorf("(a) «магомедов не 5000 а 6000»: сбор %v; бот: %q", got, bot)
		}
	}
	{
		grp, snd := e2eGroup("zzf6b"), user("79001119409")
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед нал 5000"))
		e.sendGroup(groupText(grp, snd, "Сафаи", "Нал не 5000 а 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 {
			t.Errorf("(b) «Нал не 5000 а 6000»: сбор %v; бот: %q", got, e.last().Text)
		}
	}
}

// 7. Adding the amount on a second line of the name message («Магомедов Ахмед\n12000») drops its check out of the сбор without a word

// imports: context, testing
func TestR3_07_F7_NameEditWithAmountLineDropsCheck(t *testing.T) {
	ctx := context.Background()
	grp, snd := e2eGroup("zzf7"), user("79001119410")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	nm := groupText(grp, snd, "Сафаи", "Магомедов Ахмед")
	e.sendGroup(nm)
	ck := e.sendReceipt(grp, snd, "Сафаи", sberCheck("12000", "10:00:00"), "")
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 12000 {
		t.Fatalf("чек по имени: %v", got)
	}
	n := len(e.sent())
	e.event(editOf(grp, snd, nm.Info.ID, "Магомедов Ахмед\n12000"))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 12000 || sum(got) != 12000 {
		t.Errorf("имя дополнили суммой: сбор %v, чек %+v; бот: %d сообщений", got, e.db.TestReceiptByMessage(ctx, ck), len(e.sentSince(n)))
	}
}

// 8. Editing the original payment message rebuilds its payments from scratch: a later «не X а Y» correction, a swipe-clarified ФИО and the owner's «новый» decision are all lost

// imports: context, strings, testing
func TestR3_08_F8_EditOfOriginalDropsLaterFixes(t *testing.T) {
	ctx := context.Background()
	clean := func(e *e2eBot) { // TestResetAll не чистит amount_corrections
		for id := 1; id <= 300; id++ {
			_, _ = e.db.RevertCorrections(ctx, id)
		}
	}
	{
		grp, snd := e2eGroup("zzf8a"), user("79001119411")
		e := newE2E(t, e2eOpts{WithAI: true})
		clean(e)
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		m := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000\nИсаев Харон 7000")
		e.sendGroup(m)
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов не 5000 а 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 {
			t.Fatalf("(a) поправка: %v", got)
		}
		e.event(editOf(grp, snd, m.Info.ID, "Магомедов Ахмед 5000\nИсаев Харон 8000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 || got["Исаев Харон"] != 8000 {
			t.Errorf("(a) правка строки Исаева сняла поправку Магомедова: %v; бот: %q", got, e.last().Text)
		}
	}
	{
		grp, snd := e2eGroup("zzf8b"), user("79001119412")
		e := newE2E(t, e2eOpts{WithAI: true})
		clean(e)
		e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") {
				u := c.lastUserText()
				if strings.Contains(u, "Ахмед 5000") {
					return aiReply{Text: `{"payments":[{"name":"Ахмед","amount":5000}],"clarify":""}`}
				}
				if strings.Contains(u, "Ахмед 6000") {
					return aiReply{Text: `{"payments":[{"name":"Ахмед","amount":6000}],"clarify":""}`}
				}
				return aiReply{Text: `{"payments":[],"clarify":""}`}
			}
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
			return aiReply{Text: ""}
		}
		p := groupText(grp, snd, "Сафаи", "Ахмед 5000")
		e.sendGroup(p)
		e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", p.Info.ID, snd, "Ахмед 5000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
			t.Fatalf("(b) свайп-имя: %v", got)
		}
		e.event(editOf(grp, snd, p.Info.ID, "Ахмед 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 || sum(got) != 6000 {
			t.Errorf("(b) сумму поправили, а уточнённый клиент потерян: %v; бот: %q", got, e.last().Text)
		}
	}
	{
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
		clean(e)
		e.ai = aiNamesAndPayments("Ислам Хасанов")
		grp := e2eGroup("zzf8c")
		e.groupsCache[grp] = "Оплата"
		emp, owner := user("79001119413"), user("79990001111")
		e.sendGroup(groupText(grp, emp, "Мансур", "Ислам Хасанов 27000\nналичка у Дени"))
		m2 := groupText(grp, emp, "Мансур", "Ислам Хасанов 27000\nналичка у Дени")
		e.sendGroup(m2)
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("НОВЫЙ платёж")
		if q.ID == "" {
			t.Fatalf("(c) нет вопроса про повтор: %+v", e.sent())
		}
		e.sendGroup(groupReply(grp, owner, "Ахмед", "новый", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Ислам Хасанов"] != 54000 {
			t.Fatalf("(c) «новый»: %v", got)
		}
		n := len(e.sent())
		e.event(editOf(grp, emp, m2.Info.ID, "Ислам Хасанов 27000\nналичка у Мансура"))
		if got := dayTotals(e, grp); got["Ислам Хасанов"] != 54000 {
			t.Errorf("(c) правка «у Дени»→«у Мансура» снова придержала подтверждённую наличку: %v; бот: %d сообщений", got, len(e.sentSince(n)))
		}
	}
}

// 9. Owner's (or, with REPORT_ADMINS empty, anyone's) «Джарвис, новый» swipe on «НОВЫЙ платёж или тот же?» goes to the assistant, which has no tool to settle it, so the held cash never enters the сбор

// needs imports: "context", "strings", "testing"
func TestR3_09_OwnerAddressedAnswerToCashDup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		admins []string
		who    string
	}{
		{"владелец", []string{"79287836800"}, "79287836800"},
		{"REPORT_ADMINS пуст — сотрудник", nil, "79001119309"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newE2E(t, e2eOpts{WithAI: true, Admins: tc.admins})
			pay := aiNamesAndPayments("Ислам")
			e.ai = func(c aiCall) aiReply {
				if strings.Contains(c.System, "ПОВТОР налички") {
					return aiReply{Text: `{"decision":"new"}`}
				}
				if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
					return pay(c)
				}
				// Ассистент: инструмента «новый/тот же» нет — отвечает словами.
				return aiReply{Text: "Понял, это новый платёж 👍"}
			}
			grp := e2eGroup("zzowndup")
			e.groupsCache[grp] = "Оплата"
			emp := user("79001110002")
			e.sendGroup(groupText(grp, emp, "Мансур", "Ислам 27000\nналичка у Дени"))
			e.sendGroup(groupText(grp, emp, "Мансур", "Ислам 27000\nналичка у Дени"))
			e.age(grp.String())
			e.clarifyTick(ctx)
			q := e.lastWith("НОВЫЙ платёж")
			if q.ID == "" {
				t.Fatalf("бот не спросил про повтор: %+v", e.sent())
			}
			n := len(e.sent())
			e.sendGroup(groupReply(grp, user(tc.who), "Ахмед", "Джарвис, новый", q.ID, e.botJID(), q.Text))
			e.age(grp.String())
			e.clarifyTick(ctx)
			if got := dayTotals(e, grp); got["Ислам"] != 54000 {
				t.Errorf("ответ «Джарвис, новый» на вопрос о повторе не засчитан: сбор %v; бот: %+v", got, e.sentSince(n))
			}
		})
	}
}

// 10. Deleting the answer to «Чей это чек?» silently drops the check from the сбор, and the bot never asks about it again

// needs imports: "context", "strings", "testing", "time", waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_10_DeletedAnswerCheckLostSilently(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Исаев Харон", "Магомедов Ахмед")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	grp := e2eGroup("zzdelans")
	e.groupsCache[grp] = "Оплата"
	a := user("79001119301")
	ck := e.sendReceipt(grp, a, "Сафаи", sberCheck("5000.00", "10:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей чек»: %+v", e.sent())
	}
	ans := groupReply(grp, a, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
		t.Fatalf("ответ не применён: %v", got)
	}
	n := len(e.sent())
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: a, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(ans.Info.ID), Participant: proto.String(a.String())}}}})
	r := e.db.TestReceiptByMessage(ctx, ck)
	t.Logf("после удаления ответа: чек %+v, сбор %v, бот: %+v", r, dayTotals(e, grp), e.sentSince(n))
	e.age(grp.String())
	e.clarifyTick(ctx)
	asked := false
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "Чей это чек") || strings.Contains(m.Text, "чей") {
			asked = true
		}
	}
	if sum(dayTotals(e, grp)) == 0 && !asked {
		t.Errorf("ответ «Магомедов Ахмед» удалили: чек на 5000 молча выпал из сбора и бот больше не спрашивает, чей он; бот: %+v", e.sentSince(n))
	}
}

// 11. Editing the answer to «❓ чей это платёж?»: the bot says «✏️ Имя исправили — теперь клиент Исаев Харон», but the payment stays on the old client

// needs imports: "strings", "testing", "time"
func TestR3_11_EditPaymentWhoAnswer(t *testing.T) {
	grp := e2eGroup("zzwhoedit")
	emp := user("79280009305")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("От кого наличка 27000 — чей это платёж?")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "27000 наличка"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("бот не спросил «чей платёж»: %+v", e.sentSince(n))
	}
	ans := groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 {
		t.Fatalf("ответ не записан: %v", got)
	}
	n = len(e.sent())
	e.event(editOf(grp, emp, ans.Info.ID, "Исаев Харон"))
	time.Sleep(500 * time.Millisecond)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 27000 || sum(got) != 27000 {
		t.Errorf("ответ на ❓ исправили на «Исаев Харон», а в сборе %v; бот: %+v", got, e.sentSince(n))
	}
}

// 12. Edits to an employee's «Джарвис, …» payment or client name are ignored, so the wrong amount or client stays in the сбор

// needs imports: "testing", "time"
func TestR3_12_EditAddressedEmployeePayment(t *testing.T) {
	grp, emp := e2eGroup("zzeditaddr"), user("79001119302")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	m := groupText(grp, emp, "Сафаи", "Джарвис, Каталов Ахмед 15000")
	e.sendGroup(m)
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 15000 {
		t.Fatalf("платёж сотрудника боту не записан: %v; бот: %+v", got, e.sent())
	}
	n := len(e.sent())
	e.event(editOf(grp, emp, m.Info.ID, "Джарвис, Каталов Ахмед 16000"))
	time.Sleep(500 * time.Millisecond)
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 16000 || sum(got) != 16000 {
		t.Errorf("сотрудник исправил «Джарвис, Каталов Ахмед 15000» на 16000, а в сборе %v; бот: %+v", got, e.sentSince(n))
	}
}

// 13. Edits to answers that are not names are ignored: an amount fix stays at the old sum while the bot claims «Имя исправили», and «новый» edited to «тот же» keeps the cash counted twice

// needs imports: "context", "testing", "time"
func TestR3_13_EditAnswerAmount(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzeditansamt"), user("79001119304")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.ai = aiNamesAndPayments("Хасанов Ислам")
	e.aliases.Add("Хасанов Ислам", "Хасанов Ислам")
	e.groupsCache[grp] = "Оплата"
	unreadable := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:10:00" +
		"\nНомер операции в СБП\n" + e2eID("OP")
	ck := e.sendReceipt(grp, emp, "Сафаи", unreadable, "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Не разобрал чек")
	if q.ID == "" || q.Quoted != ck || !strings.Contains(q.Text, "ФИО клиента и сумму") {
		t.Fatalf("нет вопроса: %+v", e.sent())
	}
	ans := groupReply(grp, emp, "Сафаи", "Хасанов Ислам 15000", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if got := dayTotals(e, grp); got["Хасанов Ислам"] != 15000 {
		t.Fatalf("ответ не применён: %v; чек %+v; бот %+v", got, e.db.TestReceiptByMessage(ctx, ck), e.sent())
	}
	n := len(e.sent())
	e.event(editOf(grp, emp, ans.Info.ID, "Хасанов Ислам 16000"))
	time.Sleep(500 * time.Millisecond)
	if got := dayTotals(e, grp); got["Хасанов Ислам"] != 16000 {
		t.Errorf("ответ исправили на «Хасанов Ислам 16000», а в сборе %v; чек %+v; бот: %+v", got, e.db.TestReceiptByMessage(ctx, ck), e.sentSince(n))
	}
}

// 14. An answer that @-mentions the bot is dropped: the mention's phone digits are read as the amount (7.999e10), FillReceiptByMessage hits a NUMERIC(12,2) overflow, and the check stays out of the сбор

// needs imports: "context", "strings", "testing", "time", "go.mau.fi/whatsmeow", "go.mau.fi/whatsmeow/store", waLog "go.mau.fi/whatsmeow/util/log", waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_14_MentionSwipeAnswerOnWhoseCheck(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzmentans"), user("79001119307")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	botJID := e.botJID()
	e.client = whatsmeow.NewClient(&store.Device{ID: &botJID}, waLog.Noop)
	pay := aiNamesAndPayments("Каталов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			return pay(c)
		}
		return aiReply{Text: "Понял 👍"}
	}
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("нет вопроса: %+v", e.sent())
	}
	n := len(e.sent())
	msg := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true},
			ID:            e2eID("M"), PushName: "Сафаи", Timestamp: time.Now(),
		},
		Message: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("@" + botJID.User + " Каталов Ахмед"),
			ContextInfo: &waProto.ContextInfo{MentionedJID: []string{botJID.String()},
				StanzaID: proto.String(q.ID), Participant: proto.String(botJID.String()),
				QuotedMessage: &waProto.Message{Conversation: proto.String(q.Text)}},
		}},
	}
	e.sendGroup(msg)
	r := e.db.TestReceiptByMessage(ctx, ck)
	if r.Recipient != "Каталов Ахмед" || r.NeedsReview || r.Amount != 15000 {
		t.Errorf("ответ «@Джарвис Каталов Ахмед» на «Чей это чек?» не применён: чек %+v; сбор %v; бот: %+v", r, dayTotals(e, grp), e.sentSince(n))
	}
}

// 15. Since 5a6a002, an employee's correction «Джарвис, это не Каталов Ахмед, а Исаев Харон» becomes a queued name and labels the NEXT, unrelated check

// needs imports: "context", "strings", "testing"
func TestR3_15_EmployeeAddressedCorrectionLabelsNextCheck(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzaddrcorr"), user("79001119310")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.ai = func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			// Как настоящая модель: «не X, а Y» — плательщик Y.
			if i := strings.Index(u, ", а "); i >= 0 {
				return aiReply{Text: `{"client":"` + strings.TrimSpace(u[i+len(", а "):]) + `"}`}
			}
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return aiReply{Text: "Понял 👍"}
	}
	e.groupsCache[grp] = "Оплата"
	ck1 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "12:00:00"), "Каталов Ахмед")
	if r := e.db.TestReceiptByMessage(ctx, ck1); r.Recipient != "Каталов Ахмед" {
		t.Fatalf("подпись не применена: %+v", r)
	}
	e.sendGroup(groupText(grp, emp, "Сафаи", "Джарвис, это не Каталов Ахмед, а Исаев Харон"))
	ck2 := e.sendReceipt(grp, emp, "Сафаи", sberCheck("8000.00", "12:05:00"), "")
	r1, r2 := e.db.TestReceiptByMessage(ctx, ck1), e.db.TestReceiptByMessage(ctx, ck2)
	if r2.Recipient == "Исаев Харон" {
		t.Errorf("поправка «это не Каталов, а Исаев Харон» подписала СЛЕДУЮЩИЙ чек на 8000: чек1 %+v, чек2 %+v; сбор %v", r1, r2, dayTotals(e, grp))
	}
}

// 16. «Да, правильно» on «если клиент «Y», ответьте «да»» now keeps the old client, because the R2_11 fix treats any «правильно» as «keep as it was»

// needs imports: "strings", "testing"
func TestR3_16_DupFixYesCorrect(t *testing.T) {
	for _, ans := range []string{"Да, правильно", "да правильно, Исаев Харон", "Да, всё верно, правильно"} {
		t.Run(ans, func(t *testing.T) {
			grp, emp := e2eGroup("zzdfx"), user("79001119312")
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
			ocr := sberCheck("9000", "11:00:00")
			e.sendReceipt(grp, emp, "Сафаи", ocr, "Магомедов Ахмед")
			e.sendReceipt(grp, emp, "Сафаи", ocr, "Исаев Харон")
			w := e.lastWith("уже присылали")
			if w.ID == "" || !strings.Contains(w.Text, "ответьте на это сообщение «да»") {
				t.Fatalf("нет предупреждения о повторе: %+v", e.sent())
			}
			e.sendGroup(groupReply(grp, emp, "Сафаи", ans, w.ID, e.botJID(), w.Text))
			if got := dayTotals(e, grp); got["Исаев Харон"] != 9000 || sum(got) != 9000 {
				t.Errorf("ответ «%s» на «если клиент «Исаев Харон», ответьте «да»» не перенёс чек: %v; бот: %q", ans, got, e.last().Text)
			}
		})
	}
}

// 17. A list with one unnamed «Нал» line loses money whichever way the AI answers: the cash line, or the named lines

// imports: strings, testing
func TestR3_17_UnnamedCashLineInListLost(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	snd := user("79280000001")
	list := "Магомедов Ахмед 15000\nИсаев Харон 10000\nНал 27000"

	// (a) ИИ: две строки в payments, третья — в clarify («строка либо в payments, либо в clarify»).
	g1 := e2eGroup("mixa")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":15000,"cash":false},{"name":"Исаев Харон","amount":10000,"cash":false}],"clarify":"Чья наличка 27000?"}`}
		}
		return aiReply{Text: ""}
	}
	n := len(e.sent())
	e.sendGroup(groupText(g1, snd, "Сафаи", list))
	asked := false
	for _, m := range e.sentSince(n) {
		asked = asked || strings.HasPrefix(m.Text, "❓")
	}
	if got := dayTotals(e, g1); sum(got) != 52000 && !asked {
		t.Errorf("(a) наличка 27000 без имени пропала молча: сбор %v (%v), вопроса ❓ нет", sum(got), got)
	}

	// (b) ИИ: «задай clarify и оставь payments пустым».
	g2 := e2eGroup("mixb")
	e.ai = aiClarifyOnly("Чья наличка 27000?")
	n = len(e.sent())
	e.sendGroup(groupText(g2, snd, "Сафаи", list))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("(b) нет ❓: %+v", e.sentSince(n))
	}
	e.sendGroup(groupReply(g2, snd, "Сафаи", "Гайрбеков Дени", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, g2); got["Магомедов Ахмед"] != 15000 || got["Исаев Харон"] != 10000 || sum(got) != 52000 {
		t.Errorf("(b) после ответа на ❓ названные строки списка пропали: %v, хочу Магомедов 15000 + Исаев 10000 + Гайрбеков 27000", got)
	}
}

// 18. ❓ about a «Нал 20000 / Нал 7000» list: answering per sum or as a list counts the cash twice

// imports: strings, testing
func TestR3_18_NalListAnswersDoubleCount(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	ask := aiClarifyOnly("Чья это наличка 20000 и 7000?")
	names := aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.ai = func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "модуль разбора платежей") && !strings.Contains(u, "Магомедов") && !strings.Contains(u, "Исаев") {
			return ask(c)
		}
		return names(c)
	}
	snd := user("79280000001")
	whoQ := func(n int) e2eSent {
		for _, m := range e.sentSince(n) {
			if strings.HasPrefix(m.Text, "❓") {
				return m
			}
		}
		return e2eSent{}
	}

	g1 := e2eGroup("nal2a")
	n := len(e.sent())
	e.sendGroup(groupText(g1, snd, "Сафаи", "Нал 20000\nНал 7000"))
	q := whoQ(n)
	if q.ID == "" {
		t.Fatalf("(a) нет ❓: %+v", e.sentSince(n))
	}
	n = len(e.sent())
	e.sendGroup(groupReply(g1, snd, "Сафаи", "Магомедов Ахмед 20000", q.ID, e.botJID(), q.Text))
	e.sendGroup(groupReply(g1, snd, "Сафаи", "Исаев Харон 7000", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, g1); sum(got) != 27000 || got["Исаев Харон"] != 7000 {
		t.Errorf("(a) сбор %v, хочу 27000 (Магомедов 20000 + Исаев 7000): %v; бот: %+v", sum(got), got, e.sentSince(n))
	}

	g2 := e2eGroup("nal2b")
	n = len(e.sent())
	e.sendGroup(groupText(g2, snd, "Сафаи", "Нал 20000\nНал 7000"))
	q = whoQ(n)
	if q.ID == "" {
		t.Fatalf("(b) нет ❓: %+v", e.sentSince(n))
	}
	n = len(e.sent())
	e.sendGroup(groupReply(g2, snd, "Сафаи", "Магомедов Ахмед 20000\nИсаев Харон 7000", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, g2); sum(got) != 27000 {
		t.Errorf("(b) ответ списком: сбор %v, хочу 27000: %v; бот: %+v", sum(got), got, e.sentSince(n))
	}
}

// 19. Without an AI key, a cash «повтор» is held out of the total forever: «новый или тот же?» is never asked

// imports: context, strings, testing, time
func TestR3_19_NoAICashRepeatNeverAsked(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("noaidup")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Оплата"}}) // ключа ИИ нет
	snd := user("79280000001")
	m1 := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 15000\nналичка")
	m1.Info.Timestamp = time.Now().AddDate(0, 0, -27) // прошлый месяц по графику
	e.sendGroup(m1)
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 15000\nналичка"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	e.clarifyTick(ctx)
	asked := false
	for _, m := range e.sentSince(n) {
		asked = asked || strings.Contains(m.Text, "НОВЫЙ")
	}
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 && !asked {
		t.Errorf("без ИИ сегодняшняя наличка 15000 придержана (сбор %v), а вопроса «новый или тот же?» нет: %+v", got, e.sentSince(n))
	}
}

// 20. Without an AI key, «ФИО сумма нал» / «ФИО сумма у Дени» lose the client and «27000 наличка» is dropped entirely

// imports: testing
func TestR3_20_NoAICashLinesLoseClient(t *testing.T) {
	grp := e2eGroup("noainame")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Оплата"}}) // ключа ИИ нет
	snd := user("79280000001")
	e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон 10000 у Дени"))
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 15000 нал"))
	e.sendGroup(groupText(grp, snd, "Сафаи", "27000 наличка"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 || got["Магомедов Ахмед"] != 15000 || sum(got) != 52000 {
		t.Errorf("без ИИ: %v — хочу Исаев Харон=10000, Магомедов Ахмед=15000 и наличку 27000 в сборе (всего 52000)", got)
	}
}

// 21. AI outage plus a restart (or an outage longer than 24 h): deferred payments are never re-read

// imports: context, testing, time, "whatsapp-bot/internal/ai"
func TestR3_21_AIDownDeferredLostOnRestart(t *testing.T) {
	grp := e2eGroup("downrst")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1") // ИИ недоступен
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Исаев Харон 10000"))
	for i := 0; i < 150; i++ { // ждём, пока сообщение отложат до возврата ИИ
		e.aiRetryMu.Lock()
		k := len(e.aiRetry)
		e.aiRetryMu.Unlock()
		if k > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Перезапуск: память пуста, база та же; ИИ снова работает.
	e.aiRetryMu.Lock()
	e.aiRetry = nil
	e.aiRetryMu.Unlock()
	e.assistant = e.fakeAI()
	e.ai = aiNamesAndPayments("Исаев Харон")
	e.retryAITextAfterBackoff(context.Background())
	e.clarifyTick(context.Background())
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("после перезапуска и возврата ИИ «Исаев Харон 10000» так и не записан: %v", got)
	}
}

// 22. Answering ❓ after the owner excluded that cash brings the excluded money back into the сбор

// imports: strings, testing
func TestR3_22_WhoAnswerResurrectsExcludedCash(t *testing.T) {
	grp := e2eGroup("whoexcl")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.groupsCache[grp] = "Оплата"
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Нал 27000"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет ❓: %+v", e.sentSince(n))
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":27000}`)
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("исключение не сработало: %v; %s", got, out)
	}
	e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Errorf("исключённая владельцем наличка вернулась в сбор после ответа на ❓: %v", got)
	}
}

// 23. Editing the answer to ❓: the bot says «Имя исправили — теперь клиент Исаев Харон» but the cash stays on the old client

// imports: strings, testing
func TestR3_23_EditedWhoAnswerNotApplied(t *testing.T) {
	grp := e2eGroup("whoedit")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Нал 27000"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет ❓: %+v", e.sentSince(n))
	}
	ans := groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 {
		t.Fatalf("ответ не применён: %v", got)
	}
	n = len(e.sent())
	e.event(editOf(grp, snd, ans.Info.ID, "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 27000 {
		t.Errorf("правка ответа на ❓: %v (хочу Исаев Харон=27000); бот: %+v", got, e.sentSince(n))
	}
}

// 24. The «у кого наличка?» question about the same placeholder pushes ❓ out, so a plain ФИО answer is lost

// imports: context, strings, testing
func TestR3_24_CollectorAskStealsPlainWhoAnswer(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("colwho")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Нал 27000"))
	asked := false
	for _, m := range e.sentSince(n) {
		asked = asked || strings.HasPrefix(m.Text, "❓")
	}
	if !asked {
		t.Fatalf("нет ❓: %+v", e.sentSince(n))
	}
	e.age(grp.String()) // прошло больше clarifyGrace (90 с)
	e.clarifyTick(ctx)  // «🤔 Наличка: без имени клиента — 27000 ₽. У кого она…?»
	n = len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед"))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 {
		t.Errorf("ответ «Магомедов Ахмед» на ❓ (без свайпа) после вопроса «у кого наличка?» потерян: %v; бот: %+v", got, e.sentSince(n))
	}
}

// 25. Correcting a «за кого платёж?» answer («нет, не он» / «нет в программе») moves only the check back; the text copy stays with the old client and one payment is counted twice

func TestR3_25_01_UnbindLeavesTextTwinBehind(t *testing.T) {
	for _, fix := range []string{"нет, не он", "нет в программе"} {
		t.Run(fix, func(t *testing.T) {
			grp, emp := e2eGroup("zz01"), user("79281119212")
			e := newE2E(t, e2eOpts{WithAI: true, CMFClients: []cmf.ClientInfo{
				{ID: "11", FullName: "Магомедов Ахмед Русланович"}, {ID: "12", FullName: "Магомедов Ахмед Исаевич"}}})
			e.ai = aiNamesAndPayments("Магомедов Ахмед")
			e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000"))
			e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
			time.Sleep(300 * time.Millisecond)
			q := e.lastWith("За кого этот платёж")
			if q.ID == "" {
				t.Fatalf("нет вопроса «за кого платёж»: %+v", e.sent())
			}
			e.sendGroup(groupReply(grp, emp, "Сафаи", "1", q.ID, e.botJID(), q.Text))
			time.Sleep(300 * time.Millisecond)
			if got := sum(dayTotals(e, grp)); got != 15000 {
				t.Fatalf("после «1» сбор %v (%v)", got, dayTotals(e, grp))
			}
			ok := e.lastWith("Понял")
			if ok.ID == "" {
				ok = q
			}
			e.sendGroup(groupReply(grp, emp, "Сафаи", fix, ok.ID, e.botJID(), ok.Text))
			time.Sleep(300 * time.Millisecond)
			if got := dayTotals(e, grp); sum(got) != 15000 {
				t.Errorf("после «%s» один платёж (текст + его чек) посчитан дважды: %v", fix, got)
			}
			if q2 := e.lastWith("За кого тогда"); fix == "нет, не он" && q2.ID != "" {
				e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед Русланович", q2.ID, e.botJID(), q2.Text))
				time.Sleep(300 * time.Millisecond)
				if got := dayTotals(e, grp); sum(got) != 15000 {
					t.Errorf("и после ответа «Магомедов Ахмед Русланович» сбор %v", got)
				}
			}
		})
	}
}

// 26. cmf_add_payment writes the same payment into the program a second time (dated today) when the check's payment is already there and older than 5 days

// needs imports: encoding/json, fmt, net/http, net/http/httptest, strings, sync, testing, time, whatsapp-bot/internal/cmf
func TestR3_26_02_AddPaymentDuplicatesEnteredCheck(t *testing.T) {
	grp, emp := e2eGroup("zz02"), user("79281119203")
	day := time.Now().AddDate(0, 0, -10)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	var mu sync.Mutex
	pays := []map[string]any{}
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			pays = append(pays, map[string]any{"amount": body["amount"], "paid_at": body["paid_at"]})
			posts++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1"})
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			out := []map[string]string{}
			if strings.Contains("каталов ахмед нажудович", strings.ToLower(r.URL.Query().Get("full_name"))) {
				out = append(out, map[string]string{"id": "1", "full_name": "Каталов Ахмед Нажудович"})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-1", "branch_id": "b1", "number": 100, "product_name": "товар"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			from, to := r.URL.Query().Get("date_from"), r.URL.Query().Get("date_to")
			out := []map[string]any{}
			for _, p := range pays {
				if d := fmt.Sprint(p["paid_at"]); d >= from && d <= to {
					out = append(out, p)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("CMF_API_URL", srv.URL)
	e.cmf = cmf.NewFromEnv()

	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "15000.00", "11:00:00"), "Каталов Ахмед")
	if !hasReminder(e.remindNow()) {
		t.Fatal("невнесённый чек не напомнили")
	}
	mu.Lock() // сотрудник внёс оплату вручную, датой чека
	pays = append(pays, map[string]any{"amount": 15000, "paid_at": day.Format("2006-01-02")})
	mu.Unlock()
	out := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":15000}`)
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 0 {
		t.Errorf("оплата за чек от %s уже есть в программе — бот внёс её второй раз: %s", day.Format("02.01"), out)
	}
}

// 27. A check whose client is given by a swipe on the check itself, a caption edit, a name after the check in a muted group, or by deleting the uncaptioned first copy is counted but never reconciled (no cmf_watch)

// needs imports: context, testing, time, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_27_06_LaterCompletedCheckNoWatch(t *testing.T) {
	day := time.Now().AddDate(0, 0, -1)
	noRecipient := func() string {
		return "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " 10:00:00\nСумма перевода\n14000.00 ₽\nНомер операции в СБП\n" + e2eID("OP")
	}
	cases := []struct {
		name string
		run  func(e *e2eBot, grp, emp types.JID) string
	}{
		{"свайп с ФИО на чек", func(e *e2eBot, grp, emp types.JID) string {
			ck := e.sendReceipt(grp, emp, "Сафаи", noRecipient(), "")
			e.sendGroup(groupReply(grp, emp, "Сафаи", "Каталов Ахмед", ck, emp, ""))
			return ck
		}},
		{"правка подписи к чеку", func(e *e2eBot, grp, emp types.JID) string {
			ck := e.sendReceipt(grp, emp, "Сафаи", noRecipient(), "")
			e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("E"), Timestamp: time.Now()},
				Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
					Key:           &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(ck), Participant: proto.String(emp.String())},
					EditedMessage: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Каталов Ахмед")}}}}})
			return ck
		}},
		{"ФИО следом, бот молчит в группе", func(e *e2eBot, grp, emp types.JID) string {
			_ = e.setMuted(context.Background(), grp.String(), true)
			ck := e.sendReceipt(grp, emp, "Сафаи", noRecipient(), "")
			e.sendGroup(groupText(grp, emp, "Сафаи", "Каталов Ахмед"))
			_ = e.setMuted(context.Background(), grp.String(), false)
			return ck
		}},
		{"первую копию (без ФИО) удалили", func(e *e2eBot, grp, emp types.JID) string {
			ocr := sberCheckOn(day, "14000.00", "10:00:00")
			first := e.sendReceipt(grp, emp, "Сафаи", ocr, "")
			second := e.sendReceipt(grp, emp, "Сафаи", ocr, "Каталов Ахмед")
			e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
				Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
					Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(first), Participant: proto.String(emp.String())}}}})
			return second
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			grp, emp := e2eGroup("zz06"), user("79281119211")
			e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
			e.ai = aiNamesAndPayments("Каталов Ахмед")
			ck := c.run(e, grp, emp)
			time.Sleep(300 * time.Millisecond)
			r := e.db.TestReceiptByMessage(ctx, ck)
			if r.NeedsReview || r.Duplicate || r.Recipient != "Каталов Ахмед" {
				t.Fatalf("чек не засчитан за Каталова: %+v", r)
			}
			st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
			if cid != "1" || !hasReminder(e.remindNow()) {
				t.Errorf("чек засчитан за Каталова (14000 в сборе), но сверка его не ведёт и не напоминает: status=%q client=%q", st, cid)
			}
		})
	}
}

// 28. The same check in a muted group (СБ) and in the main group: the «one reminder per item» rule picks the muted copy, so the reminder goes nowhere

func TestR3_28_04_MutedPrimaryGroupSwallowsReminder(t *testing.T) {
	ocr := sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00")
	reminders := func(e *e2eBot) []string {
		var rems []string
		for _, m := range e.remindNow() {
			if strings.Contains(m.Text, "⏰") {
				rems = append(rems, m.Chat+": "+m.Text)
			}
		}
		return rems
	}
	t.Run("чек выложили в обе группы", func(t *testing.T) {
		ctx := context.Background()
		sb, mainG := e2eGroup("zz04sb"), e2eGroup("zz04main")
		e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
		e.ai = aiNamesAndPayments("Каталов Ахмед")
		_ = e.setMuted(ctx, sb.String(), true) // «молчи в группе СБ»
		e.sendReceipt(sb, user("79281119201"), "Сафаи", ocr, "Каталов Ахмед")
		e.sendReceipt(mainG, user("79281119202"), "Мадина", ocr, "Каталов Ахмед")
		if rems := reminders(e); len(rems) != 1 || !strings.HasPrefix(rems[0], mainG.String()) {
			t.Errorf("невнесённый чек: в основную группу напоминание не пришло: %v", rems)
		}
	})
	t.Run("правило пересылки СБ → основная", func(t *testing.T) {
		ctx := context.Background()
		sb, mainG, emp := e2eGroup("zz04fs"), e2eGroup("zz04fd"), user("79281119210")
		e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov, Groups: map[string]string{sb.String(): "СБ оплата", mainG.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Каталов Ахмед")
		_ = e.setMuted(ctx, sb.String(), true)
		_ = e.db.SetForwardRule(ctx, sb.String(), "СБ оплата", mainG.String(), "Оплата клиентов")
		waID := e.sendReceipt(sb, emp, "Сафаи", ocr, "Каталов Ахмед")
		e.applyForwardRules(ctx, sb.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Каталов Ахмед\n"+ocr, time.Now())
		e.waitIdle()
		if rems := reminders(e); len(rems) != 1 {
			t.Errorf("невнесённый чек (копия в «Оплата клиентов»): напоминаний %d: %v", len(rems), rems)
		}
	})
}

// 29. Two different checks of one client (same amount, a minute apart, no operation number) are treated as copies of one check, so only one gets a reminder

func TestR3_29_05_TwoChecksMinuteApartOneReminder(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zz05"), user("79281119213")
	day := time.Now().AddDate(0, 0, -1)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	mk := func(hms string) string {
		return "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " " + hms +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽"
	}
	ck1 := e.sendReceipt(grp, emp, "Сафаи", mk("10:00:00"), "Каталов Ахмед")
	ck2 := e.sendReceipt(grp, emp, "Сафаи", mk("10:01:00"), "Каталов Ахмед")
	if r := e.db.TestReceiptByMessage(ctx, ck2); r.Duplicate {
		if out := e.tool("correct_operation", grp, `{"action":"not_duplicate","amount":5000,"person":"Каталов"}`); !strings.Contains(out, "учитывается") {
			t.Fatalf("снятие дубля: %s", out)
		}
	}
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	if got := monthTotals(e, []string{grp.String()}, from, from.AddDate(0, 0, 1)); got != 10000 {
		t.Fatalf("сбор %v, ожидали 10000", got)
	}
	lines := 0
	var texts []string
	for _, m := range e.remindNow() {
		if strings.Contains(m.Text, "⏰") {
			texts = append(texts, m.Text)
			lines += strings.Count(m.Text, "5000 ₽")
		}
	}
	s1, _, _ := e.db.TestCmfWatchState(ctx, ck1)
	s2, _, _ := e.db.TestCmfWatchState(ctx, ck2)
	if lines != 2 {
		t.Errorf("два невнесённых чека по 5000 (в сборе 10000), а напомнили про %d: %q (сверка: %s, %s)", lines, texts, s1, s2)
	}
}

// 30. notYetReply silently swallows client corrections on a reminder («Это не он, пока не внесли», «Не Каталову внесли, а Каталовой»)

func TestR3_30_07_NotYetReplySwallowsCorrection(t *testing.T) {
	clients := []cmf.ClientInfo{{ID: "1", FullName: "Каталов Ахмед Нажудович"}, {ID: "2", FullName: "Каталова Марьям Исаевна"}}
	for _, reply := range []string{"Это не он, пока не внесли", "Не Каталову внесли, а Каталовой"} {
		t.Run(reply, func(t *testing.T) {
			ctx := context.Background()
			grp, emp := e2eGroup("zz07"), user("79281119207")
			e := newE2E(t, e2eOpts{WithAI: true, CMFClients: clients})
			e.ai = aiNamesAndPayments("Каталов Ахмед")
			ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
			rem := e.remindNow()
			if !hasReminder(rem) {
				t.Fatalf("нет напоминания: %+v", rem)
			}
			r := rem[len(rem)-1]
			n := len(e.sent())
			e.sendGroup(groupReply(grp, emp, "Сафаи", reply, r.ID, e.botJID(), r.Text))
			time.Sleep(300 * time.Millisecond)
			st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
			if cid == "1" && len(e.sentSince(n)) == 0 {
				t.Errorf("«%s»: бот промолчал, чек по-прежнему за Каталовым Ахмедом (status=%s)", reply, st)
			}
		})
	}
}

// 31. cmf_add_payment for the second of two same-day checks closes that check from the first payment and writes the second payment dated today, not the check date

func TestR3_31_08_AddPaymentSecondSameDayCheckGetsToday(t *testing.T) {
	grp, emp := e2eGroup("zz08"), user("79281119209")
	day := time.Now().AddDate(0, 0, -3)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "5000.00", "10:00:00"), "Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "5000.00", "10:03:00"), "Каталов Ахмед")
	out1 := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":5000}`)
	out2 := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":5000}`)
	out3 := ""
	if strings.Contains(out2, "ПОХОЖЕ НА ПОВТОР") {
		out3 = e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":5000,"confirm_duplicate":true}`)
	}
	e.posts.mu.Lock()
	var dates []string
	for _, p := range e.posts.list {
		dates = append(dates, fmt.Sprint(p["paid_at"]))
	}
	e.posts.mu.Unlock()
	want := day.Format("2006-01-02")
	if len(dates) != 2 || dates[0] != want || dates[1] != want {
		t.Errorf("два чека от %s — в программу ушли даты %v\n1: %s\n2: %s\n3: %s", want, dates, out1, out2, out3)
	}
}

// 32. The 45-second forward-copy sync undoes the owner's fix or exclusion of a forwarded copy in the target group

// imports: context, regexp, time, testing, types "go.mau.fi/whatsmeow/types"
func TestR3_32_MirrorUndoesOwnerFixOfCopy(t *testing.T) {
	ctx := context.Background()
	setup := func(tag, caption string) (*e2eBot, types.JID) {
		src, dst, emp := e2eGroup(tag+"s"), e2eGroup(tag+"d"), user("79001113001")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"},
			Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
		ocr := sberCheck("13000", "14:00:00")
		waID := e.sendReceipt(src, emp, "Сафаи", ocr, caption)
		text := ocr
		if caption != "" {
			text = caption + "\n" + ocr
		}
		e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", text, time.Now())
		e.waitIdle()
		return e, dst
	}

	// (a) Чек без ФИО. Владелец: «какие чеки не понял в Оплата КЛНТ?» → «это Магомедов Ахмед».
	e, dst := setup("fx", "")
	list := e.tool("list_unclear_receipts", dst, `{"group":"Оплата КЛНТ"}`)
	code := regexp.MustCompile(`receipt:\d+`).FindString(list)
	if code == "" {
		t.Fatalf("копии нет в списке непонятых чеков «Оплата КЛНТ»: %s", list)
	}
	out := e.tool("fix_receipt", dst, `{"item":"`+code+`","person":"Магомедов Ахмед"}`)
	if got := dayTotals(e, dst); got["Магомедов Ахмед"] != 13000 {
		t.Fatalf("сразу после fix_receipt (%s): %v", out, got)
	}
	e.clarifyTick(ctx) // обычный фоновый цикл, раз в 45 секунд
	if got := dayTotals(e, dst); got["Магомедов Ахмед"] != 13000 {
		t.Errorf("бот ответил %q, а через один цикл уточнений чек снова выпал из сбора «Оплата КЛНТ»: %v", out, got)
	}

	// (b) Чек с ФИО. Владелец в «Оплата КЛНТ»: «этот чек не наш, не считай его здесь».
	e, dst = setup("ex", "Магомедов Ахмед")
	out = e.tool("correct_operation", dst, `{"action":"exclude","amount":13000,"person":"Магомедов"}`)
	if got := dayTotals(e, dst); sum(got) != 0 {
		t.Fatalf("сразу после исключения (%s): %v", out, got)
	}
	e.clarifyTick(ctx)
	if got := dayTotals(e, dst); sum(got) != 0 {
		t.Errorf("бот ответил %q, а через один цикл уточнений чек снова в сборе «Оплата КЛНТ»: %v", out, got)
	}
}

// 33. A check read only by vision is counted in the source group, but its forwarded copy is never recorded in the target group

// imports: context, strings, time, testing, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_33_VisionReadCheckCopyNotRecorded(t *testing.T) {
	ctx := context.Background()
	src, dst, emp := e2eGroup("vcs"), e2eGroup("vcd"), user("79001113009")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	names := aiNamesAndPayments("Магомедов Ахмед")
	day := time.Now().Format("2006-01-02")
	op := e2eID("OP")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Т-Банк","recipient":"Хадижат И.","amount":15000,"doc_number":"` + op + `","status":"Успешно","datetime":"` + day + ` 10:05:00"}`}
		}
		if strings.Contains(c.System, "модуль разбора банковских чеков") {
			return aiReply{Text: `{"kind":"receipt","amount":0,"recipient":""}`} // из пустого OCR нечего достать
		}
		return names(c)
	}
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	// Tesseract ничего не прочитал: текст сообщения — только подпись (как в handleGroupMessage).
	text := "Магомедов Ахмед\n"
	waID := e2eID("CHK")
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, src.String(), emp.String(), emp.User, "Сафаи", text, true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: src, Sender: emp, IsGroup: true}, ID: waID, PushName: "Сафаи", Timestamp: time.Now()},
		Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Магомедов Ахмед")}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, "Магомедов Ахмед")
	e.handleBankReceipt(ctx, src, emp.String(), waID, text, rawID, time.Now(), []byte("jpeg"), ".jpg", payer, payerAmount)
	n := len(e.sent())
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", text, time.Now()) // как startForward
	e.waitIdle()
	e.clarifyTick(ctx)
	if got := dayTotals(e, src); got["Магомедов Ахмед"] != 15000 {
		t.Fatalf("исходная группа: %v", got)
	}
	if got := dayTotals(e, dst); got["Магомедов Ахмед"] != 15000 {
		t.Errorf("фото чека переслано в «Оплата КЛНТ» (%+v), а в сборе группы его нет: %v", e.sentSince(n), got)
	}
}

// 34. Editing an employee's payment written to the bot («Джарвис, Исаев Харон 10000» → 12000) is ignored

// imports: time, testing, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_34_EditOfAddressedEmployeePaymentIgnored(t *testing.T) {
	grp, emp := e2eGroup("eap"), user("79001113006")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Исаев Харон")
	m := groupText(grp, emp, "Мансур", "Джарвис, Исаев Харон 10000")
	e.sendGroup(m)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Fatalf("платёж сотрудника боту: %v", got)
	}
	e.event(&events.Message{IsEdit: true,
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true},
			ID: e2eID("EDIT"), Timestamp: time.Now(), Edit: types.EditAttributeMessageEdit},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(m.Info.ID), Participant: proto.String(emp.String())},
			EditedMessage: &waProto.Message{Conversation: proto.String("Джарвис, Исаев Харон 12000")}}}})
	if got := dayTotals(e, grp); got["Исаев Харон"] != 12000 || sum(got) != 12000 {
		t.Errorf("сотрудник исправил сумму в своём сообщении боту на 12000, а в сборе %v", got)
	}
}

// 35. History import ignores the client written next to the check, then the bot asks «Чей это чек?» about old imported checks

// imports: context, strings, time, testing, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/proto/waHistorySync", "go.mau.fi/whatsmeow/proto/waWeb", "google.golang.org/protobuf/proto"
func TestR3_35_HistoryImportIgnoresClientNextToCheck(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("hic"), user("79001113008")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	names := aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	day := time.Now().Format("2006-01-02")
	visionSays := func(amount string) func(c aiCall) aiReply {
		return func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора изображений") {
				// Как настоящая модель: печатный получатель — владелец карты-сборщика.
				return aiReply{Text: `{"kind":"receipt","bank":"СберБанк","recipient":"Хадижат Ибрагимовна И.","amount":` + amount + `,"doc_number":"OP-` + amount + `","status":"Выполнено","datetime":"` + day + ` 11:20:00"}`}
			}
			return names(c)
		}
	}
	e.mu.Lock()
	e.ai = visionSays("15000")
	e.mu.Unlock()
	// Как importHistorySync после скачивания медиа: подпись к фото — текст сообщения.
	ts := time.Now().Add(-40 * time.Minute)
	rawA, _, err := e.db.SaveRawMessage(ctx, e2eID("H"), grp.String(), emp.String(), emp.String(), emp.User, "Магомедов Ахмед", true, "", ts)
	if err != nil {
		t.Fatal(err)
	}
	if !e.importReceiptSilent(ctx, grp, rawA, ts, []byte("jpeg"), ".jpg") {
		t.Fatal("чек A из истории не записан")
	}
	_ = e.db.MarkMessageParsed(ctx, rawA)
	// Чек без подписи, ФИО — следующим сообщением (как вживую «чек, потом имя»).
	e.mu.Lock()
	e.ai = visionSays("8000")
	e.mu.Unlock()
	tsB := time.Now().Add(-30 * time.Minute)
	rawB, _, err := e.db.SaveRawMessage(ctx, e2eID("H"), grp.String(), emp.String(), emp.String(), emp.User, "", true, "", tsB)
	if err != nil {
		t.Fatal(err)
	}
	if !e.importReceiptSilent(ctx, grp, rawB, tsB, []byte("jpeg"), ".jpg") {
		t.Fatal("чек B из истории не записан")
	}
	_ = e.db.MarkMessageParsed(ctx, rawB)
	e.importHistorySync(&waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: []*waHistorySync.HistorySyncMsg{{
		Message: &waWeb.WebMessageInfo{
			Key:     &waProto.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(e2eID("H")), Participant: proto.String(emp.String())},
			Message: &waProto.Message{Conversation: proto.String("Исаев Харон")}, MessageTimestamp: proto.Uint64(uint64(tsB.Add(20 * time.Second).Unix()))}}}}}})
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 15000 || got["Исаев Харон"] != 8000 {
		e.age(grp.String())
		n := len(e.sent())
		e.clarifyTick(ctx)
		t.Errorf("импорт истории: чек с подписью «Магомедов Ахмед» и чек + «Исаев Харон» следом — сбор %v (хочу Магомедов Ахмед=15000, Исаев Харон=8000); затем бот написал в группу: %+v", got, e.sentSince(n))
	}
}

// 36. History import throws away the answer to «Чей это чек?» given while the bot was offline; the check is never counted and never asked again

// imports: context, time, testing, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/proto/waHistorySync", "go.mau.fi/whatsmeow/proto/waWeb", "google.golang.org/protobuf/proto"
func TestR3_36_HistoryImportDropsAnswerToCheckQuestion(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("hia"), user("79001113011")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000", "09:40:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("бот не спросил: %+v", e.sent())
	}
	reply := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("Магомедов Ахмед"),
		ContextInfo: &waProto.ContextInfo{StanzaID: proto.String(q.ID), Participant: proto.String(e.botJID().String()),
			QuotedMessage: &waProto.Message{Conversation: proto.String(q.Text)}}}}
	e.importHistorySync(&waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: []*waHistorySync.HistorySyncMsg{{
		Message: &waWeb.WebMessageInfo{
			Key:     &waProto.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(e2eID("H")), Participant: proto.String(emp.String())},
			Message: reply, MessageTimestamp: proto.Uint64(uint64(time.Now().Unix()))}}}}}})
	e.waitIdle()
	e.clarifyTick(ctx)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 {
		t.Errorf("ответ «Магомедов Ахмед» на «Чей это чек?» пришёл через импорт истории — чек не засчитан и больше не спрашивается: %v", got)
	}
}

// 37. The owner deletes the bot's forwarded check photo in the target group; the copy keeps counting there

// imports: context, strings, time, testing, waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR3_37_DeleteForwardedPhotoInTargetKeepsCopy(t *testing.T) {
	ctx := context.Background()
	src, dst, emp, owner := e2eGroup("dfs"), e2eGroup("dfd"), user("79001113007"), user("79990001111")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"},
		Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	ocr := sberCheck("13000", "14:00:00")
	waID := e.sendReceipt(src, emp, "Сафаи", ocr, "Магомедов Ахмед")
	n := len(e.sent())
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Магомедов Ахмед\n"+ocr, time.Now())
	e.waitIdle()
	var fwd e2eSent
	for _, s := range e.sentSince(n) {
		if s.Chat == dst.String() && strings.HasPrefix(s.Text, "[фото]") {
			fwd = s
		}
	}
	if fwd.ID == "" {
		t.Fatalf("бот не переслал фото: %+v", e.sentSince(n))
	}
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: dst, Sender: owner, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(dst.String()), ID: proto.String(fwd.ID), FromMe: proto.Bool(true), Participant: proto.String(e.botJID().String())}}}})
	if got := dayTotals(e, dst); sum(got) != 0 {
		t.Errorf("владелец удалил пересланный чек в «Оплата КЛНТ», а он всё ещё в сборе группы: %v", got)
	}
}
