package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии второго прогона (проверка свежих исправлений).

// 1. «Магомедов не 5000 а 6000» corrects the wrong client's payment

func TestR2_01_EditsCorrectionNamedClientHitsOtherClient(t *testing.T) {
	grp, snd := e2eGroup("zzname"), user("79001119002")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
	e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон 5000"))
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов не 5000 а 6000"))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 6000 || got["Исаев Харон"] != 5000 {
		var bot []string
		for _, m := range e.sentSince(n) {
			bot = append(bot, m.Text)
		}
		t.Errorf("«Магомедов не 5000 а 6000»: сбор %v, хочу Магомедов Ахмед=6000, Исаев Харон=5000; бот: %q", got, bot)
	}
}

// 2. «не X а Y» or «правильно N» anywhere in a sentence takes over ordinary messages: plans change recorded money, real payments get dropped

func TestR2_02_EditsCorrectionHijacksOrdinaryMessages(t *testing.T) {
	// (a) Разговор о плане.
	{
		grp, snd := e2eGroup("zzplan"), user("79001119001")
		e := newE2E(t, e2eOpts{WithAI: true})
		base := aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(c.lastUserText(), "сказал") {
				return aiReply{Text: `{"payments":[],"clarify":""}`} // план, а не платёж
			}
			return base(c)
		}
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев сказал в следующий раз не 5000 а 6000 принесёт"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || sum(got) != 5000 {
			t.Errorf("(a) разговор о плане поправил записанный платёж: %v; бот: %q", got, e.last().Text)
		}
	}
	// (b) «Правильно…» в предложении (свайп владельца на платёж).
	{
		grp, snd, owner := e2eGroup("zzprav"), user("79001119016"), user("79990001111")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		m := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000")
		e.sendGroup(m)
		e.sendGroup(groupReply(grp, owner, "Ахмед", "Правильно записал, он ещё 3000 должен", m.Info.ID, snd, "Магомедов Ахмед 5000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
			t.Errorf("(b) «Правильно записал, он ещё 3000 должен» поменяло сумму: %v; бот: %q", got, e.last().Text)
		}
	}
	// (c) Платёж, написанный через «не X а Y», когда исправлять нечего.
	{
		grp, snd := e2eGroup("zzlost"), user("79001119003")
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Харон принёс не 10000 а 15000"))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 15000 {
			t.Errorf("(c) платёж «принёс не 10000 а 15000» не записан: %v; бот: %q", got, e.last().Text)
		}
	}
}

// 3. An edit that arrives while the AI is still reading the original text counts both the old and the new amount

func TestR2_03_EditsEditWhileAIStillParsingDoubles(t *testing.T) {
	grp, snd := e2eGroup("zzrace"), user("79001119008")
	e := newE2E(t, e2eOpts{WithAI: true})
	base := aiNamesAndPayments("Магомедов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(c.lastUserText(), "50000") {
			time.Sleep(1500 * time.Millisecond) // ИИ отвечает медленно
		}
		return base(c)
	}
	m := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 50000")
	e.handleGroupMessage(context.Background(), m) // разбор ИИ идёт в фоне
	time.Sleep(200 * time.Millisecond)
	e.event(editOf(grp, snd, m.Info.ID, "Магомедов Ахмед 5000")) // опечатку сразу исправили
	time.Sleep(2 * time.Second)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || sum(got) != 5000 {
		t.Errorf("правка во время разбора ИИ: сбор %v, хочу только Магомедов Ахмед=5000", got)
	}
}

// 4. Editing a payment while the AI is failing loses it for good: the retry skips the message

func TestR2_04_EditsEditDuringAIOutageLosesPayment(t *testing.T) {
	grp, snd := e2eGroup("zzdown"), user("79001119017")
	e := newE2E(t, e2eOpts{WithAI: true})
	up := aiNamesAndPayments("Сугаипов Иса")
	e.ai = up
	m := groupText(grp, snd, "Сафаи", "Сугаипов Иса 50000")
	e.sendGroup(m)
	if got := dayTotals(e, grp); got["Сугаипов Иса"] != 50000 {
		t.Fatalf("исходный платёж: %v", got)
	}
	e.mu.Lock()
	e.ai = func(c aiCall) aiReply { return aiReply{Text: "503 Service Unavailable"} } // ИИ сбоит
	e.mu.Unlock()
	n := len(e.sent())
	e.event(editOf(grp, snd, m.Info.ID, "Сугаипов Иса 5000"))
	e.waitRetryQueue(1)
	e.mu.Lock()
	e.ai = up // ИИ вернулся
	e.mu.Unlock()
	e.retryAIText(context.Background())
	if got := dayTotals(e, grp); got["Сугаипов Иса"] != 5000 {
		var bot []string
		for _, s := range e.sentSince(n) {
			bot = append(bot, s.Text)
		}
		t.Errorf("правка во время сбоя ИИ: после возврата ИИ сбор %v, хочу Сугаипов Иса=5000; бот: %q", got, bot)
	}
}

// 5. Editing your answer to «Чей это чек?» is ignored, so the check stays on the wrong client

func TestR2_05_EditsEditedClarifyAnswerIgnored(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiNamesAndPayments("Исаев Харон", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	grp := e2eGroup("zzcl")
	e.groupsCache[grp] = "Оплата"
	a := user("79001119009")
	ck := e.sendReceipt(grp, a, "Сафаи", sberCheck("5000.00", "10:00:00"), "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей чек»: %+v", e.sent())
	}
	ans := groupReply(grp, a, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text)
	e.sendGroup(ans)
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient != "Магомедов Ахмед" {
		t.Fatalf("ответ не применён: %+v", r)
	}
	e.event(editOf(grp, a, ans.Info.ID, "Исаев Харон"))
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient != "Исаев Харон" {
		t.Errorf("правка ответа на «Чей это чек?» не применена: чек %+v, хочу клиента Исаев Харон", r)
	}
}

// 6. Editing a name that clarified a text payment leaves the payment on the old name and labels the next unrelated check

func TestR2_06_EditsEditedSwipeNameQueuedForNextCheck(t *testing.T) {
	ctx := context.Background()
	grp, snd := e2eGroup("zztxtname"), user("79001119006")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "Ахмед 5000") {
				return aiReply{Text: `{"payments":[{"name":"Ахмед","amount":5000}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	p := groupText(grp, snd, "Сафаи", "Ахмед 5000")
	e.sendGroup(p)
	nm := groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", p.Info.ID, snd, "Ахмед 5000")
	e.sendGroup(nm)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
		t.Fatalf("свайп-имя не уточнило платёж: %v", got)
	}
	e.event(editOf(grp, snd, nm.Info.ID, "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("исправленное имя не дошло до платежа: %v, хочу Исаев Харон=5000", got)
	}
	ck := e.sendReceipt(grp, snd, "Сафаи", sberCheck("20000.00", "12:00:00"), "")
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient == "Исаев Харон" {
		t.Errorf("правка имени встала в очередь и подписала чужой чек на 20000: %+v", r)
	}
}

// 7. Deleting or editing a «не 5000 а 6000» correction doesn't revert or update the amount it changed

func TestR2_07_EditsCorrectionMessageDeleteOrEditIgnored(t *testing.T) {
	// (a) Удаление поправки.
	{
		grp, snd := e2eGroup("zzdelcorr"), user("79001119011")
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		c := groupText(grp, snd, "Сафаи", "не 5000 а 6000")
		e.sendGroup(c)
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 {
			t.Fatalf("поправка не применилась: %v", got)
		}
		if _, _, err := e.db.MarkMessageDeleted(context.Background(), c.Info.ID); err != nil {
			t.Fatal(err)
		}
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
			t.Errorf("(a) поправку удалили, а в учёте осталось %v, хочу Магомедов Ахмед=5000", got)
		}
	}
	// (b) Правка поправки.
	{
		grp, snd := e2eGroup("zzeditcorr"), user("79001119010")
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		e.sendGroup(groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000"))
		c := groupText(grp, snd, "Сафаи", "не 5000 а 6000")
		e.sendGroup(c)
		e.event(editOf(grp, snd, c.Info.ID, "не 5000 а 7000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 7000 || sum(got) != 7000 {
			t.Errorf("(b) поправку изменили на «не 5000 а 7000», а в учёте %v, хочу 7000", got)
		}
	}
}

// 8. A held cash repeat corrected to a different amount stays held as a «повтор»

func TestR2_08_EditsCorrectedHeldCashStaysHeld(t *testing.T) {
	grp, snd := e2eGroup("zzheld"), user("79001119004")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	m1 := groupText(grp, snd, "Сафаи", "Магомедов Ахмед нал 5000")
	m1.Info.Timestamp = time.Now().AddDate(0, 0, -3)
	e.sendGroup(m1)
	m2 := groupText(grp, snd, "Сафаи", "Магомедов Ахмед нал 5000")
	e.sendGroup(m2)
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("повтор налички не придержан: %v", dayTotals(e, grp))
	}
	e.sendGroup(groupReply(grp, snd, "Сафаи", "не 5000 а 6000", m2.Info.ID, snd, "Магомедов Ахмед нал 5000"))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 {
		t.Errorf("наличку исправили на 6000 (уже не повтор 5000), а она всё ещё вне сбора: %v; бот: %q", got, e.last().Text)
	}
}

// 9. Owner's «Джарвис, …» swipe on a bot question is now fed to the clarify parsers: a command becomes the check's client, or is swallowed by a re-ask

func TestR2_09_OwnerAddressedSwipeOnQuestion(t *testing.T) {
	ctx := context.Background()
	owner := user("79287836800")
	t.Run("чей чек", func(t *testing.T) {
		grp, a := e2eGroup("own"), user("79001110041")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: `{"payments":[],"clarify":""}`}
			}
			if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				return aiReply{Text: `{"client":""}`}
			}
			return aiReply{Text: "ОТВЕТ-АССИСТЕНТА"}
		}
		e.groupsCache[grp] = "Оплата"
		ck := e.sendReceipt(grp, a, "Сафаи", sberCheck("15000.00", "12:00:00"), "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("Чей это чек")
		if q.ID == "" {
			t.Fatalf("нет вопроса: %+v", e.sent())
		}
		n := len(e.sent())
		e.sendGroup(groupReply(grp, owner, "Ахмед", "Джарвис, отправь этот чек Мансуру", q.ID, e.botJID(), q.Text))
		if r := e.db.TestReceiptByMessage(ctx, ck); strings.Contains(strings.ToLower(r.Recipient), "отправь") {
			t.Errorf("команда ассистенту стала клиентом чека: %+v; бот: %+v; сбор: %v", r, e.sentSince(n), dayTotals(e, grp))
		}
	})
	t.Run("новый или тот же", func(t *testing.T) {
		grp, emp := e2eGroup("odup"), user("79001110002")
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
		pay := aiNamesAndPayments("Ислам")
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "ПОВТОР налички") {
				return aiReply{Text: `{"decision":"unclear"}`}
			}
			if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				return pay(c)
			}
			return aiReply{Text: "ОТВЕТ-АССИСТЕНТА"}
		}
		e.groupsCache[grp] = "Оплата"
		e.sendGroup(groupText(grp, emp, "Мансур", "Ислам 27000\nналичка у Дени"))
		e.sendGroup(groupText(grp, emp, "Мансур", "Ислам 27000\nналичка у Дени"))
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("НОВЫЙ платёж")
		if q.ID == "" {
			t.Fatalf("бот не спросил про повтор: %+v", e.sent())
		}
		n := len(e.sent())
		e.sendGroup(groupReply(grp, owner, "Ахмед", "Джарвис, покажи оба платежа Ислама", q.ID, e.botJID(), q.Text))
		ok := false
		for _, m := range e.sentSince(n) {
			if strings.Contains(m.Text, "ОТВЕТ-АССИСТЕНТА") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("команда владельца ассистенту проглочена: %+v", e.sentSince(n))
		}
	})
}

// 10. An employee's payment that @-mentions the bot is lost: it goes to the assistant, which has no tools for employees

// needs imports: "go.mau.fi/whatsmeow", "go.mau.fi/whatsmeow/store", waLog "go.mau.fi/whatsmeow/util/log", waProto "go.mau.fi/whatsmeow/binary/proto", "go.mau.fi/whatsmeow/types", "go.mau.fi/whatsmeow/types/events", "google.golang.org/protobuf/proto"
func TestR2_10_EmployeeMentionPaymentLost(t *testing.T) {
	grp, emp := e2eGroup("ment"), user("79001112290")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	botJID := e.botJID()
	e.client = whatsmeow.NewClient(&store.Device{ID: &botJID}, waLog.Noop)
	pay := aiNamesAndPayments("Каталов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			return pay(c)
		}
		return aiReply{Text: "Записывать платежи может только владелец 🙂"}
	}
	msg := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true},
			ID:            e2eID("M"), PushName: "Сафаи", Timestamp: time.Now(),
		},
		Message: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String("@" + botJID.User + " Каталов Ахмед 15000 наличка у Дени"),
			ContextInfo: &waProto.ContextInfo{MentionedJID: []string{botJID.String()}},
		}},
	}
	e.sendGroup(msg)
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 15000 {
		t.Errorf("платёж сотрудника с @-упоминанием бота потерян: %v; бот: %+v", got, e.sent())
	}
}

// 11. «Да нет» / «да не, всё правильно» / «да, это повтор» on the duplicate-check warning moves the check to the other client

func TestR2_11_DupFixDaNetMovesCheck(t *testing.T) {
	for _, ans := range []string{"да нет", "Да нет, не надо", "да не, всё правильно", "да, это повтор"} {
		t.Run(ans, func(t *testing.T) {
			grp, emp := e2eGroup("dfx"), user("79001112234")
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
			ocr := sberCheck("9000", "11:00:00")
			e.sendReceipt(grp, emp, "Сафаи", ocr, "Магомедов Ахмед")
			e.sendReceipt(grp, emp, "Сафаи", ocr, "Исаев Харон")
			w := e.lastWith("уже присылали")
			if w.ID == "" {
				t.Fatalf("нет предупреждения о повторе: %+v", e.sent())
			}
			e.sendGroup(groupReply(grp, emp, "Сафаи", ans, w.ID, e.botJID(), w.Text))
			if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 9000 {
				t.Errorf("ответ «%s» перенёс чек на другого клиента: %v; бот: %q", ans, got, e.last().Text)
			}
		})
	}
}

// 12. Excluding a check leaves its text twin from another group counting, so the all-groups total does not change

func TestR2_12_ExcludeCheckCrossGroupTwin(t *testing.T) {
	from, to := today()
	sb, main := e2eGroup("sbx"), e2eGroup("mainx")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"},
		Groups: map[string]string{sb.String(): "СБ Оплата", main.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	e.sendReceipt(sb, user("79001110200"), "Сафаи", sberCheck("15000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(main, user("79001110201"), "Ахмед", "Магомедов Ахмед 15000"))
	if got := monthTotals(e, nil, from, to); got != 15000 {
		t.Fatalf("до исключения по всем группам: %v", got)
	}
	out := e.tool("correct_operation", sb, `{"action":"exclude","amount":15000,"person":"Магомедов"}`)
	if got := monthTotals(e, nil, from, to); got != 0 {
		t.Errorf("исключили чек (тот же платёж, что текст в основной), а общий сбор остался %v: %s", got, out)
	}
}

// 13. Excluding a forwarded check leaves its copy in the target group counting, so the total does not change

func TestR2_13_ExcludeForwardedOriginal(t *testing.T) {
	ctx := context.Background()
	src, dst := e2eGroup("fxs"), e2eGroup("fxd")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"},
		Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	ocr := sberCheck("13000", "14:00:00")
	waID := e.sendReceipt(src, user("79001110050"), "Сафаи", ocr, "Магомедов Ахмед")
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Магомедов Ахмед\n"+ocr, time.Now())
	e.waitIdle()
	from, to := today()
	if got := monthTotals(e, nil, from, to); got != 13000 {
		t.Fatalf("до исключения: %v", got)
	}
	out := e.tool("correct_operation", src, `{"action":"exclude","amount":13000,"person":"Магомедов"}`)
	if got := monthTotals(e, nil, from, to); got != 0 {
		t.Errorf("исключили чек, а его пересланная копия всплыла в общем сборе: %v (%s)", got, out)
	}
}

// 14. An employee's «Джарвис, это <ФИО>» right after their check is lost; the bot says «Понял» and then still asks «Чей это чек?»

func TestR2_14_EmployeeAddressedNameAfterCheck(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("anc"), user("79001112291")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
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
	e.sendGroup(groupText(grp, emp, "Сафаи", "Джарвис, это Каталов Ахмед"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Recipient != "Каталов Ахмед" {
		t.Errorf("имя клиента, написанное боту сразу после чека, потеряно: %+v; бот: %+v", r, e.sent())
	}
}

// 15. AI down: a list where one line is a known client saves that line and drops every other line for good (no retry)

// imports: context, strings, testing, time, "whatsapp-bot/internal/ai"
func TestR2_15_AIDownListKnownLineDropsRest(t *testing.T) {
	grp := e2eGroup("downlist")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")       // известный клиент (алиас/слияние владельца)
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1") // ИИ недоступен
	e.sendGroup(groupText(grp, user("79280000001"), "Сафаи", "Магомедов Ахмед 15000\nИсаев Харон 10000"))
	for i := 0; i < 100; i++ { // ждём фоновый разбор (откат к парсеру)
		e.aiRetryMu.Lock()
		k := len(e.aiRetry)
		e.aiRetryMu.Unlock()
		if k > 0 || sum(dayTotals(e, grp)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	down := dayTotals(e, grp)
	e.assistant = e.fakeAI() // ИИ снова доступен
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":15000},{"name":"Исаев Харон","amount":10000}],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	e.retryAIText(context.Background())
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 10000 || sum(got) != 25000 {
		t.Errorf("пока ИИ лежал: %v; после возврата ИИ: %v — хочу Магомедов Ахмед=15000 и Исаев Харон=10000", down, got)
	}
}

// 16. «Нал»/«Наличка» counts as a 'known client' in the fallback: a phantom cash line is written against the AI's verdict, and with the AI down the real client is lost

// imports: context, strings, testing, time, "whatsapp-bot/internal/ai"
func TestR2_16_NalLineCountedAsKnownClient(t *testing.T) {
	// (a) ИИ доступен и вернул «платежей нет».
	g1 := e2eGroup("summary")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	e.sendGroup(groupText(g1, user("79280000001"), "Сафаи", "Итог за сегодня:\nПереводы 120000\nНаличка 50000\nВсего 170000"))
	if got := dayTotals(e, g1); sum(got) != 0 {
		t.Errorf("(a) итог дня (ИИ: не платёж) записан как наличка: %v", got)
	}
	// (b) ИИ недоступен.
	g2 := e2eGroup("downnal")
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	e.sendGroup(groupText(g2, user("79280000001"), "Сафаи", "Магомедов Ахмед\nНал 27000"))
	for i := 0; i < 100; i++ {
		e.aiRetryMu.Lock()
		k := len(e.aiRetry)
		e.aiRetryMu.Unlock()
		if k > 0 || sum(dayTotals(e, g2)) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	down := dayTotals(e, g2)
	e.assistant = e.fakeAI()
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":27000,"cash":true}],"clarify":""}`}
		}
		return aiReply{Text: ""}
	}
	e.retryAIText(context.Background())
	if got := dayTotals(e, g2); got["Магомедов Ахмед"] != 27000 || sum(got) != 27000 {
		t.Errorf("(b) пока ИИ лежал: %v; после возврата ИИ: %v — хочу Магомедов Ахмед=27000", down, got)
	}
}

// 17. Answering ❓ on a «Нал» list with «ФИО сумма» adds a third payment on top of the two placeholders (double count)

// imports: strings, testing
func TestR2_17_NalListAnswerAddsThirdPayment(t *testing.T) {
	grp := e2eGroup("nallist")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("Чья это наличка 20000 и 7000?")
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Нал 20000\nНал 7000"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
	}
	before := dayTotals(e, grp)
	n = len(e.sent())
	e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед 20000", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 27000 {
		t.Errorf("до ответа %v; после «Магомедов Ахмед 20000» сбор %v (хочу 27000): %v; бот: %+v", before, sum(got), got, e.sentSince(n))
	}
}

// 18. An open ❓ with no known amount takes the same employee's next payment as its 'answer', records it as cash, and double-counts it with its check

// imports: strings, testing
func TestR2_18_PaymentWhoHijacksNextTransfer(t *testing.T) {
	grp := e2eGroup("hij")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			if strings.Contains(u, "Исаев") {
				return aiReply{Text: `{"client":"Исаев Дени"}`}
			}
			return aiReply{Text: `{"client":""}`}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(u, "Исаев") {
				return aiReply{Text: `{"payments":[{"name":"Исаев Дени","amount":5000,"cash":false}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[],"clarify":"Чья это наличка 27000?"}`}
		}
		return aiReply{Text: ""}
	}
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "20000 и 7000 наличка"))
	asked := false
	for _, m := range e.sentSince(n) {
		asked = asked || strings.HasPrefix(m.Text, "❓")
	}
	if !asked {
		t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
	}
	n = len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Исаев Дени 5000"))
	e.sendReceipt(grp, snd, "Сафаи", sberCheck("5000.00", "13:10:00"), "Исаев Дени")
	if got := dayTotals(e, grp); got["Исаев Дени"] != 5000 {
		t.Errorf("«Исаев Дени 5000» + его чек должны быть ОДНИМ платежом: %v; бот: %+v", got, e.sentSince(n))
	}
}

// 19. Replies to ❓ that are not names («у Дени», «Мансур знает», «Позже скажу», «Спрошу у Мансура») are recorded as the client

// imports: strings, testing
func TestR2_19_PaymentWhoNonNameAnswerBecomesClient(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	snd, other := user("79280000001"), user("79280000002")
	cases := []struct {
		swipe  bool
		byMate bool
		answer string
	}{
		{true, true, "у Дени"},
		{true, true, "Мансур знает"},
		{false, false, "Позже скажу"},
		{false, false, "Спрошу у Мансура"},
	}
	for _, c := range cases {
		grp := e2eGroup("whonn")
		n := len(e.sent())
		e.sendGroup(groupText(grp, snd, "Сафаи", "27000 наличка"))
		var q e2eSent
		for _, m := range e.sentSince(n) {
			if strings.HasPrefix(m.Text, "❓") {
				q = m
			}
		}
		if q.ID == "" {
			t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
		}
		who, push := snd, "Сафаи"
		if c.byMate {
			who, push = other, "Рустам"
		}
		n = len(e.sent())
		if c.swipe {
			e.sendGroup(groupReply(grp, who, push, c.answer, q.ID, e.botJID(), q.Text))
		} else {
			e.sendGroup(groupText(grp, who, push, c.answer))
		}
		got := dayTotals(e, grp)
		if _, bad := got[c.answer]; bad {
			t.Errorf("ответ %q (свайп=%v) записан КЛИЕНТОМ: %v; бот: %+v", c.answer, c.swipe, got, e.sentSince(n))
		}
	}
}

// 20. Naming the unnamed «Наличка» row through ❓ skips the 30-day same-client/same-amount cash hold

// imports: strings, testing, time
func TestR2_20_PlaceholderNamedSkipsCashRepeat(t *testing.T) {
	grp := e2eGroup("phdup")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "Магомедов") {
				return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":27000,"cash":true}],"clarify":""}`}
			}
			return aiReply{Text: `{"payments":[],"clarify":"Чья это наличка 27000?"}`}
		}
		return aiReply{Text: ""}
	}
	snd := user("79280000001")
	m1 := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 27000 нал")
	m1.Info.Timestamp = time.Now().AddDate(0, 0, -5)
	e.sendGroup(m1)
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "Нал 27000"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 0 {
		t.Errorf("повтор налички Магомедов Ахмед 27000 (такая же 5 дней назад) не придержан: сегодня %v", got)
	}
}

// 21. ❓ about a message with two amounts can never be answered with a name: the payment is never recorded

// imports: strings, testing
func TestR2_21_PaymentWhoTwoAmountsNameIgnored(t *testing.T) {
	grp := e2eGroup("two")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	ask := aiClarifyOnly("Чья это наличка 27000?")
	e.ai = func(c aiCall) aiReply {
		// Как настоящая модель: узнав клиента, читает обе суммы.
		if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(c.lastUserText(), "Магомедов Ахмед") {
			return aiReply{Text: `{"payments":[{"name":"Магомедов Ахмед","amount":20000,"cash":true},{"name":"Магомедов Ахмед","amount":7000,"cash":true}],"clarify":""}`}
		}
		return ask(c)
	}
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "20000 и 7000 наличка"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
	}
	n = len(e.sent())
	e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 27000 {
		t.Errorf("ответ «Магомедов Ахмед» на ❓ ничего не записал: %v; бот: %+v", got, e.sentSince(n))
	}
}

// 22. The ❓ question-to-message link lives only in memory: after a bot restart the swipe answer is lost and the cash never enters the сбор

// imports: strings, testing
func TestR2_22_PaymentWhoAnswerLostAfterRestart(t *testing.T) {
	grp := e2eGroup("rst")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}})
	e.ai = aiClarifyOnly("Чья это наличка 27000?")
	snd := user("79280000001")
	n := len(e.sent())
	e.sendGroup(groupText(grp, snd, "Сафаи", "27000 наличка"))
	var q e2eSent
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			q = m
		}
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса ❓: %+v", e.sentSince(n))
	}
	e.clarify = newClarifyState() // перезапуск бота: память вопросов пуста, база та же
	n = len(e.sent())
	e.sendGroup(groupReply(grp, snd, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 {
		t.Errorf("после перезапуска ответ на ❓ потерян: %v; бот: %+v", got, e.sentSince(n))
	}
}

// 23. Answering «за кого этот платёж?» counts one payment twice: «ФИО + сумма» text and its check both go into the сбор

func TestR2_23_BindAnswerDoublesTextTwin(t *testing.T) {
	grp, emp := e2eGroup("zzf1"), user("79281119101")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: []cmf.ClientInfo{
		{ID: "11", FullName: "Магомедов Ахмед Русланович"}, {ID: "12", FullName: "Магомедов Ахмед Исаевич"}}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 15000"))
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "10:00:00"), "")
	time.Sleep(300 * time.Millisecond)
	if got := sum(dayTotals(e, grp)); got != 15000 {
		t.Fatalf("до ответа сбор %v, ожидали 15000", got)
	}
	q := e.lastWith("За кого этот платёж")
	if q.ID == "" {
		t.Fatalf("нет вопроса «за кого платёж»: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "1", q.ID, e.botJID(), q.Text))
	time.Sleep(300 * time.Millisecond)
	if got := dayTotals(e, grp); sum(got) != 15000 {
		t.Errorf("после ответа «1» один платёж посчитан дважды: %v", got)
	}
}

// 24. cmf_add_payment without a date takes the date of an old 'reminded' check, writes the payment on the wrong day and closes the wrong check

func TestR2_24_StaleRemindedWatchStealsCheckDate(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzf2"), user("79281119102")
	sep := time.Now().AddDate(0, 0, -33)
	oct := time.Now().AddDate(0, 0, -2)
	pays := map[string][]map[string]any{}
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov, CMFPays: pays})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(sep, "15000.00", "11:00:00"), "Каталов Ахмед")
	if !hasReminder(e.remindNow()) {
		t.Fatal("сентябрьский чек не напомнили")
	}
	pays["1"] = append(pays["1"], map[string]any{"amount": 15000, "paid_at": sep.Format("2006-01-02")})
	ckOct := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(oct, "15000.00", "12:00:00"), "Каталов Ахмед")
	out := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":15000,"confirm_duplicate":true}`)
	e.posts.mu.Lock()
	paid := ""
	if len(e.posts.list) > 0 {
		paid = fmt.Sprint(e.posts.list[len(e.posts.list)-1]["paid_at"])
	}
	e.posts.mu.Unlock()
	if paid != oct.Format("2006-01-02") {
		t.Errorf("внёс с датой %q, а невнесённый чек — от %s: %s", paid, oct.Format("2006-01-02"), out)
	}
	if st, _, _ := e.db.TestCmfWatchState(ctx, ckOct); st != "found" {
		t.Errorf("октябрьский чек после внесения всё ещё ждёт: %s", st)
	}
}

// 25. «Каталову не внесли» in reply to a reminder unbinds the client and the check drops out of reminders

func TestR2_25_NegatedReminderReplyUnbindsClient(t *testing.T) {
	for _, reply := range []string{"Каталову не внесли", "Каталову пока не внесли, вечером внесём"} {
		t.Run(reply, func(t *testing.T) {
			ctx := context.Background()
			grp, emp := e2eGroup("zzf3"), user("79281119103")
			e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
			e.ai = aiNamesAndPayments("Каталов Ахмед")
			ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
			rem := e.remindNow()
			if !hasReminder(rem) {
				t.Fatalf("нет напоминания: %+v", rem)
			}
			r := rem[len(rem)-1]
			n := len(e.sent())
			e.sendGroup(groupReply(grp, emp, "Сафаи", reply, r.ID, e.botJID(), r.Text))
			time.Sleep(200 * time.Millisecond)
			for _, m := range e.sentSince(n) {
				if strings.Contains(m.Text, "такого клиента") || strings.Contains(m.Text, "Платёж за него") {
					t.Errorf("«%s» понят как другое имя: %q", reply, m.Text)
				}
			}
			if st, cid, _ := e.db.TestCmfWatchState(ctx, ck); cid != "1" {
				t.Errorf("«%s» отвязал клиента от чека: status=%s client=%q", reply, st, cid)
			}
		})
	}
}

// 26. Name message deleted: the check goes back to «чей чек?», but the reminder still tells the group to enter it for that client

func TestR2_26_DeletedNameStillRemindedAsClient(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzf4"), user("79281119104")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	m := groupText(grp, emp, "Сафаи", "Каталов Ахмед")
	e.sendGroup(m)
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "")
	if _, cid, _ := e.db.TestCmfWatchState(ctx, ck); cid != "1" {
		t.Fatalf("имя перед чеком не дошло до сверки")
	}
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(m.Info.ID), Participant: proto.String(emp.String())}}}})
	if r := e.db.TestReceiptByMessage(ctx, ck); !r.NeedsReview {
		t.Fatalf("после удаления имени чек не вернулся в «чей чек?»: %+v", r)
	}
	for _, msg := range e.remindNow() {
		if strings.Contains(msg.Text, "⏰") && strings.Contains(msg.Text, "Каталов") {
			t.Errorf("имя удалили, а сверка напоминает внести чек тому же клиенту: %q", msg.Text)
		}
	}
}

// 27. An excluded («скинули по ошибке») check gives its date to cmf_add_payment and is closed instead of the live check

func TestR2_27_ExcludedCheckGivesDateToProgramEntry(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzf5"), user("79281119105")
	d1, d2 := time.Now().AddDate(0, 0, -6), time.Now().AddDate(0, 0, -1)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(d1, "14000.00", "10:00:00"), "Каталов Ахмед")
	if out := e.tool("correct_operation", grp, `{"action":"exclude","amount":14000,"person":"Каталов"}`); !strings.Contains(out, "исключена") {
		t.Fatalf("исключение: %s", out)
	}
	live := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(d2, "14000.00", "11:00:00"), "Каталов Ахмед")
	out := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":14000}`)
	if !strings.Contains(out, d2.Format("02.01.2006")) {
		t.Errorf("внёс не с датой живого чека %s: %s", d2.Format("02.01.2006"), out)
	}
	if st, _, _ := e.db.TestCmfWatchState(ctx, live); st != "found" {
		t.Errorf("живой чек после внесения всё ещё ждёт: %s", st)
	}
}

// 28. Checks that only become countable later are never reconciled (ФИО given by answer on a check with no recipient; «это не дубль»)

func TestR2_28_LaterCompletedCheckNeverReconciled(t *testing.T) {
	day := time.Now().AddDate(0, 0, -1)
	t.Run("ФИО ответом", func(t *testing.T) {
		ctx := context.Background()
		grp, emp := e2eGroup("zzf6a"), user("79281119106")
		e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
		e.ai = aiNamesAndPayments("Каталов Ахмед")
		ocr := "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " 10:00:00\nСумма перевода\n14000.00 ₽\nНомер операции в СБП\n" + e2eID("OP")
		ck := e.sendReceipt(grp, emp, "Сафаи", ocr, "")
		q := e.lastWith("ЧЕЙ он")
		if q.ID == "" {
			t.Fatalf("бот не спросил, чей чек: %+v", e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Каталов Ахмед", q.ID, e.botJID(), q.Text))
		time.Sleep(300 * time.Millisecond)
		if r := e.db.TestReceiptByMessage(ctx, ck); r.NeedsReview || r.Recipient != "Каталов Ахмед" {
			t.Fatalf("клиент не записан: %+v", r)
		}
		if st, cid, _ := e.db.TestCmfWatchState(ctx, ck); cid != "1" {
			t.Errorf("чек засчитан за Каталова, но сверка его не ведёт: status=%q client=%q", st, cid)
		}
		if !hasReminder(e.remindNow()) {
			t.Error("невнесённый чек не напомнили")
		}
	})
	t.Run("не дубль", func(t *testing.T) {
		ctx := context.Background()
		grp, emp := e2eGroup("zzf6b"), user("79281119116")
		e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
		e.ai = aiNamesAndPayments("Каталов Ахмед")
		mk := func(hms string) string {
			return "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " " + hms +
				"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽"
		}
		e.sendReceipt(grp, emp, "Сафаи", mk("10:00:00"), "Каталов Ахмед")
		ck2 := e.sendReceipt(grp, emp, "Сафаи", mk("10:02:00"), "Каталов Ахмед")
		if out := e.tool("correct_operation", grp, `{"action":"not_duplicate","amount":5000,"person":"Каталов"}`); !strings.Contains(out, "учитывается") {
			t.Fatalf("снятие дубля: %s", out)
		}
		if st, cid, _ := e.db.TestCmfWatchState(ctx, ck2); cid != "1" {
			t.Errorf("второй платёж засчитан, но сверка его не ведёт: status=%q client=%q", st, cid)
		}
	})
}

// 29. The same check posted in the СБ group and the main group gets a «⏰ Занесите» reminder in both groups

func TestR2_29_SameCheckTwoGroupsRemindedTwice(t *testing.T) {
	sb, mainG := e2eGroup("zzf7sb"), e2eGroup("zzf7main")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	ocr := sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00")
	e.sendReceipt(sb, user("79281119107"), "Сафаи", ocr, "Каталов Ахмед")
	e.sendReceipt(mainG, user("79281119108"), "Мадина", ocr, "Каталов Ахмед")
	var rems []string
	for _, m := range e.remindNow() {
		if strings.Contains(m.Text, "⏰") {
			rems = append(rems, m.Chat+": "+m.Text)
		}
	}
	if len(rems) != 1 {
		t.Errorf("один чек (копия в двух группах) напомнили %d раз(а): %v", len(rems), rems)
	}
}

// 30. Kopeck check: cmf_add_payment misses the payment already in the program and writes 15001 ₽ while reporting 15000 ₽

func TestR2_30_KopecksCheckWrittenTwice(t *testing.T) {
	grp, emp := e2eGroup("zzf8"), user("79281119109")
	day := time.Now().AddDate(0, 0, -1)
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov,
		CMFPays: map[string][]map[string]any{"1": {{"amount": 15000, "paid_at": day.Format("2006-01-02")}}}})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, "15000.50", "10:00:00"), "Каталов Ахмед")
	if hasReminder(e.remindNow()) {
		t.Fatal("сверка не сочла чек внесённым")
	}
	out := e.tool("cmf_add_payment", grp, `{"client_name":"Каталов Ахмед Нажудович","amount":15000.5}`)
	if e.posts.n() != 0 {
		e.posts.mu.Lock()
		last := e.posts.list[len(e.posts.list)-1]
		e.posts.mu.Unlock()
		t.Errorf("в программе уже есть 15000 за этот чек — бот внёс ещё раз (POST amount=%v): %s", last["amount"], out)
	}
}

// 31. Unreadable checks are never asked about after a mute: CountUnrecognized/CountUnconfirmed lost their amount filter inside an SQL comment

func TestR2_31_UnreadableCheckAskedAfterUnmute(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("unm"), user("79001112001")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	amounts := []string{"10000", "11000", "12000", "13000", "14000", "15000", "16000"}
	for i, a := range amounts {
		e.sendReceipt(grp, emp, "Сафаи", sberCheck(a, "09:0"+string(rune('0'+i))+":00"), "Магомедов Ахмед")
	}
	_ = e.setMuted(ctx, grp.String(), true) // «молчи в этой группе»
	unreadable := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:10:00" +
		"\nНомер операции в СБП\n" + e2eID("OP")
	e.sendReceipt(grp, emp, "Сафаи", unreadable, "")
	_ = e.setMuted(ctx, grp.String(), false) // «можешь снова писать»
	e.age(grp.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "Не смог разобрать") {
			return
		}
	}
	t.Errorf("после снятия молчания бот так и не спросил про нераспознанный чек (его нет ни в сумме, ни в «НЕ вошло»); отправил: %+v", e.sentSince(n))
}

// 32. Collapsed screen plus full check: the source group replaces it, but its forwarded copy is never replaced, so the payment counts twice

func TestR2_32_CollapsedCopyDoubleCounted(t *testing.T) {
	ctx := context.Background()
	src, dst, emp := e2eGroup("cs2"), e2eGroup("cd2"), user("79001112017")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	sendWithMedia := func(ocr string) string {
		waID := e2eID("CHK")
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, src.String(), emp.String(), emp.User, "Сафаи", ocr, true, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.handleBankReceipt(ctx, src, emp.String(), waID, ocr, rawID, time.Now(), []byte("jpeg"), ".jpg", "", 0)
		e.waitIdle()
		e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", ocr, time.Now())
		e.waitIdle()
		return waID
	}
	sendWithMedia("Т-Банк\nПеревод выполнен\nИтого\n5 000 ₽")
	q := e.lastWith("НЕПОЛНЫМ")
	if q.ID == "" {
		t.Fatalf("бот не попросил полный чек: %+v", e.sent())
	}
	e.sendGroup(groupReply(src, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	e.clarifyTick(ctx)
	sendWithMedia(sberCheck("5000", l5collapsedPastHMS()))
	e.clarifyTick(ctx)
	if got := dayTotals(e, src); sum(got) != 5000 {
		t.Fatalf("исходная группа: %v", got)
	}
	if got := dayTotals(e, dst); sum(got) != 5000 {
		t.Errorf("«Оплата КЛНТ»: один платёж 5000 посчитан как %v", got)
	}
	from, to := today()
	if got := monthTotals(e, nil, from, to); got != 5000 {
		t.Errorf("по всем группам один платёж 5000 посчитан как %v", got)
	}
}

// 33. Deleting or excluding a forwarded source check leaves its copy counted, while the bot says the reports no longer include it

func TestR2_33_ForwardCopySurvivesSourceRemoval(t *testing.T) {
	ctx := context.Background()
	setup := func(tag string) (*e2eBot, types.JID, types.JID, types.JID, string) {
		src, dst, emp := e2eGroup(tag+"s"), e2eGroup(tag+"d"), user("79001112003")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
		ocr := sberCheck("13000", "14:00:00")
		waID := e.sendReceipt(src, emp, "Сафаи", ocr, "Магомедов Ахмед")
		e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Магомедов Ахмед\n"+ocr, time.Now())
		e.waitIdle()
		return e, src, dst, emp, waID
	}
	from, to := today()
	e, src, dst, emp, waID := setup("del")
	n := len(e.sent())
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: src, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(src.String()), ID: proto.String(waID), Participant: proto.String(emp.String())}}}})
	if got := monthTotals(e, nil, from, to); got != 0 {
		t.Errorf("удаление: бот написал %+v, а по всем группам в сборе %v", e.sentSince(n), got)
	}
	if got := dayTotals(e, dst); sum(got) != 0 {
		t.Errorf("удаление: копия в «Оплата КЛНТ» всё ещё в сборе: %v", got)
	}
	e, src, _, _, _ = setup("exc")
	out := e.tool("correct_operation", src, `{"action":"exclude","amount":13000,"person":"Магомедов Ахмед"}`)
	if got := monthTotals(e, nil, from, to); got != 0 {
		t.Errorf("исключение: инструмент ответил %q, а по всем группам в сборе всё ещё %v", out, got)
	}
}

// 34. Forwarded copy of an unreadable check gets the client but keeps amount 0, so the target group loses the payment

func TestR2_34_ForwardCopyOfUnreadableGetsAmount(t *testing.T) {
	ctx := context.Background()
	src, dst, emp, owner := e2eGroup("us"), e2eGroup("ud"), user("79001112002"), user("79990001111")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	ocr := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 12:10:00" +
		"\nНомер операции в СБП\n" + e2eID("OP")
	waID := e.sendReceipt(src, emp, "Сафаи", ocr, "")
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", ocr, time.Now())
	q := e.lastWith("Не смог разобрать")
	if q.ID == "" {
		t.Fatalf("бот не спросил про нераспознанный чек: %+v", e.sent())
	}
	e.sendGroup(groupReply(src, owner, "Ахмед", "Магомедов Ахмед 15000", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, src); got["Магомедов Ахмед"] != 15000 {
		t.Fatalf("исходная группа: %v", got)
	}
	e.age(dst.String())
	e.clarifyTick(ctx)
	if got := dayTotals(e, dst); got["Магомедов Ахмед"] != 15000 {
		d := time.Now().Format("2006-01-02")
		rep := e.tool("send_finance_report", dst, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","group":"Оплата КЛНТ"}`)
		t.Errorf("копия в «Оплата КЛНТ» после ответа: %v, хочу Магомедов Ахмед=15000; отчёт группы:\n%s", got, rep)
	}
}

// 35. Checks forwarded from the owner's DM into a group are never asked «чей это чек?», so they stay out of the total forever

func TestR2_35_DMForwardedCopyNeverAsked(t *testing.T) {
	ctx := context.Background()
	dst := e2eGroup("dm")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	_ = e.db.SetForwardRule(ctx, "dm", "личка", dst.String(), "Оплата КЛНТ")
	e.applyForwardRules(ctx, "dm", "Ахмед", e2eID("P"), []byte("jpeg"), ".jpg", sberCheck("9000", "12:30:00"), time.Now())
	e.waitIdle()
	e.age(dst.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	for _, m := range e.sentSince(n) {
		if m.Chat == dst.String() && strings.Contains(m.Text, "Чей это чек") {
			return
		}
	}
	d := time.Now().Format("2006-01-02")
	rep := e.tool("send_finance_report", dst, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","group":"Оплата КЛНТ"}`)
	t.Errorf("чек из лички на 9000 лежит в «Оплата КЛНТ» без клиента, а вопроса нет; отчёт:\n%s", rep)
}

// 36. «Можешь снова писать везде» says the bot is back, but a group muted on its own earlier stays silent

func TestR2_36_UnmuteEverywhereKeepsGroupMute(t *testing.T) {
	ctx := context.Background()
	g := e2eGroup("ot")
	e := newE2E(t, e2eOpts{Groups: map[string]string{g.String(): "Отче"}})
	tool := e.muteGroupTool(g)
	_, _ = tool.Handle(ctx, json.RawMessage(`{"silence":true}`))
	_, _ = tool.Handle(ctx, json.RawMessage(`{"silence":true,"group":"везде"}`))
	r, _ := tool.Handle(ctx, json.RawMessage(`{"silence":false,"group":"везде"}`))
	if e.groupSilent(g) {
		t.Errorf("ответ %q, но в «Отче» бот всё ещё молчит", r)
	}
}

// 37. History import differs from live handling: edits are ignored and employees' payments written to the bot are dropped

func TestR2_37_HistoryImportDivergesFromLive(t *testing.T) {
	hist := func(grp, snd types.JID, id string, ts time.Time, m *waProto.Message) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:     &waProto.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(id), Participant: proto.String(snd.String())},
			Message: m, MessageTimestamp: proto.Uint64(uint64(ts.Unix()))}}
	}
	total := func(e *e2eBot, grp types.JID, name string) float64 {
		ss, err := e.db.SummaryForPeriod(context.Background(), time.Now().AddDate(0, 0, -3), time.Now().AddDate(0, 0, 1), []string{grp.String()})
		if err != nil {
			t.Fatal(err)
		}
		var s float64
		for _, x := range ss {
			if x.CanonicalName == name {
				s += x.Total
			}
		}
		return s
	}
	grp, emp := e2eGroup("he"), user("79001112010")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Исаев Харон")
	origID, at := e2eID("H"), time.Now().Add(-10*time.Minute)
	e.importHistorySync(&waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: []*waHistorySync.HistorySyncMsg{
		hist(grp, emp, origID, at, &waProto.Message{Conversation: proto.String("Исаев Харон 5000")}),
		hist(grp, emp, e2eID("H"), at.Add(time.Minute), &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(origID), Participant: proto.String(emp.String())},
			EditedMessage: &waProto.Message{Conversation: proto.String("Исаев Харон 6000")}}}),
	}}}})
	if got := total(e, grp, "Исаев Харон"); got != 6000 {
		t.Errorf("импорт истории не учёл правку суммы: Исаев Харон=%v, хочу 6000", got)
	}
	grp2, emp2 := e2eGroup("hi"), user("79001112009")
	e = newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79990001111"}, Groups: map[string]string{grp2.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Исаев Харон")
	e.importHistorySync(&waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp2.String()), Messages: []*waHistorySync.HistorySyncMsg{
		hist(grp2, emp2, e2eID("H"), time.Now().Add(-5*time.Minute), &waProto.Message{Conversation: proto.String("Джарвис, Исаев Харон 10000 у Дени")}),
	}}}})
	imported := total(e, grp2, "Исаев Харон")
	grp3 := e2eGroup("hl")
	e.groupsCache[grp3] = "Оплата КЛНТ 2"
	e.sendGroup(groupText(grp3, emp2, "Мансур", "Джарвис, Исаев Харон 10000 у Дени"))
	if live := total(e, grp3, "Исаев Харон"); imported != live {
		t.Errorf("тот же платёж сотрудника боту: вживую %v, из истории %v", live, imported)
	}
}
