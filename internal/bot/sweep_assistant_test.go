package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии ассистента: платёж сотрудника боту, повтор через record_payment,
// исключение чека с текстовой копией, сбор за день по расписанию.

const ownerPhone = "79287836800"

func TestSweepEmployeePaymentAddressedToBot(t *testing.T) {
	grp, emp := e2eGroup("addr"), user("79001112233")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	pay := aiNamesAndPayments("Каталов Ахмед", "Исаев Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") || strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			return pay(c)
		}
		return aiReply{Text: "Записывать платежи может только владелец 🙂"}
	}
	e.sendGroup(groupText(grp, emp, "Сафаи", "Джарвис, Каталов Ахмед 15000 наличка у Дени"))
	if got := dayTotals(e, grp); got["Каталов Ахмед"] != 15000 {
		t.Errorf("платёж «Джарвис, …» потерян: %v", got)
	}
	id := e.sendTextReturnID(grp, "Записал: наличка Каталов Ахмед 15000 ₽ — забрал Дени.")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Ахмед 7000 нал", id, e.botJID(), "Записал: …"))
	if got := dayTotals(e, grp); got["Исаев Ахмед"] != 7000 {
		t.Errorf("платёж свайпом на сообщение бота потерян: %v", got)
	}
	for _, m := range e.sent() {
		if strings.Contains(m.Text, "только владелец") {
			t.Errorf("сотруднику ответили «только владелец»: %q", m.Text)
		}
	}
}

func TestSweepDuplicateCheckClientFix(t *testing.T) {
	grp, emp := e2eGroup("dfix"), user("79001112234")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	ocr := sberCheck("9000", "11:00:00")
	e.sendReceipt(grp, emp, "Сафаи", ocr, "Магомедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", ocr, "Исаев Харон")
	w := e.lastWith("уже присылали")
	if w.ID == "" || !strings.Contains(w.Text, "ответьте на это сообщение «да»") {
		t.Fatalf("нет предупреждения о повторе с подсказкой: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "да", w.ID, e.botJID(), w.Text))
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 9000 || sum(got) != 9000 {
		t.Errorf("«да» на предупреждение не перенёс чек: %v", got)
	}
}

func TestSweepRecordPaymentDuplicateGuard(t *testing.T) {
	grp, emp := e2eGroup("rp"), user("79001112235")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{}})
	e.groupsCache[grp] = "Оплата КЛНТ"
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	ctx := context.Background()
	_ = e.db.SettingSet(ctx, settingWorkingGroup, grp.String())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Каталов Ахмед 10000 наличка у Дени"))
	out := e.tool("record_payment", grp, `{"client":"Каталов Ахмед","amount":10000,"kind":"cash","collector":"Дени"}`)
	if !strings.Contains(out, "ПОХОЖЕ НА ПОВТОР") {
		t.Errorf("повтор налички записан молча: %s", out)
	}
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Errorf("сбор после повтора: %v", got)
	}
	out = e.tool("record_payment", grp, `{"client":"Каталов Ахмед","amount":10000,"kind":"cash","collector":"Дени","confirm_duplicate":true}`)
	if !strings.HasPrefix(out, "Записал") || sum(dayTotals(e, grp)) != 20000 {
		t.Errorf("подтверждённый отдельный платёж: %s / %v", out, dayTotals(e, grp))
	}
	// «у Дени» без слова «наличка» — наличка, а не перевод, который съел бы чек.
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("7000", "12:00:00"), "Каталов Ахмед")
	out = e.tool("record_payment", grp, `{"client":"Каталов Ахмед","amount":7000,"kind":"transfer","collector":"Дени"}`)
	if !strings.Contains(out, "наличка") || sum(dayTotals(e, grp)) != 34000 { // 20000 + чек 7000 + наличка 7000
		t.Errorf("«у Дени» записан не наличкой: %s / %v", out, dayTotals(e, grp))
	}
}

func TestSweepForwardedPaymentMarked(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = func(c aiCall) aiReply { return aiReply{Text: "ок"} }
	fwd := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: user(ownerPhone), Sender: user(ownerPhone)}, ID: e2eID("P"), Timestamp: time.Now()},
		Message: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("Каталов Ахмед 10000 наличка у Дени"),
			ContextInfo: &waProto.ContextInfo{IsForwarded: proto.Bool(true)}}}}
	e.handlePrivateMessage(context.Background(), fwd)
	found := false
	for _, c := range e.calls {
		if strings.Contains(c.lastUserText(), "Переслано из другого чата") {
			found = true
		}
	}
	if !found {
		t.Error("ассистент не знает, что платёж переслан")
	}
}

func TestSweepExcludeCheckWithTextTwin(t *testing.T) {
	grp, emp := e2eGroup("excl"), user("79001112236")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000", "09:00:00"), "Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед\n5000"))
	if got := sum(dayTotals(e, grp)); got != 5000 {
		t.Fatalf("чек+текст: %v", got)
	}
	out := e.tool("correct_operation", grp, `{"action":"exclude","amount":5000,"person":"Магомедов"}`)
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Errorf("исключили чек, а сумма осталась %v: %s", got, out)
	}
}

func TestSweepScheduledDayCollection(t *testing.T) {
	grp, emp := e2eGroup("sched"), user("79001112237")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.groupsCache[grp] = "Оплата"
	e.ai = aiNamesAndPayments("Магомедов Ахмед")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед 12000"))
	out := e.tool("schedule_reminder", grp, `{"kind":"daily","time":"21:00","target":"here","message":"Сбор за день","report":"day_collection"}`)
	if !strings.Contains(out, "сбор за день с цифрами") {
		t.Fatalf("напоминание: %s", out)
	}
	n := len(e.sent())
	e.runDueJobsAt(time.Now().Add(25 * time.Hour))
	got := e.sentSince(n)
	if len(got) != 1 || !strings.Contains(got[0].Text, "12000") {
		t.Errorf("по расписанию пришёл не сбор с цифрами: %+v", got)
	}
}
