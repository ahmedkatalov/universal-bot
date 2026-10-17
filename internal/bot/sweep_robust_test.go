package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

// Регрессии надёжности: «молчи» глушит и вопросы по чекам, пересланный чек
// сохраняет клиента, импорт истории не пересчитывает команды и болтовню.

func TestSweepMuteSilencesCheckQuestions(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("mute")
	e := newE2E(t, e2eOpts{})
	_ = e.setMuted(ctx, grp.String(), true)
	n := len(e.sent())
	e.sendReceipt(grp, user("79001110020"), "Сафаи", "Альфа-Банк\n\nПолучатель\nХадижат Имрановна С.\n\nДата операции "+
		time.Now().Format("02.01.2006 15:04")+"\n\nНомер документа\n"+e2eID("D"), "")
	ocr := sberCheck("7000", "13:00:00")
	e.sendReceipt(grp, user("79001110020"), "Сафаи", ocr, "Магомедов Ахмед")
	e.sendReceipt(grp, user("79001110020"), "Сафаи", ocr, "Магомедов Ахмед")
	if got := e.sentSince(n); len(got) != 0 {
		t.Errorf("в заглушённой группе бот писал: %+v", got)
	}

	g2 := e2eGroup("muteall")
	e2 := newE2E(t, e2eOpts{Groups: map[string]string{g2.String(): "Отче"}})
	tool := e2.muteGroupTool(g2)
	_, _ = tool.Handle(ctx, json.RawMessage(`{"silence":true,"group":"везде"}`))
	r, _ := tool.Handle(ctx, json.RawMessage(`{"silence":false}`))
	if e2.groupSilent(g2) {
		t.Errorf("ответил %q, но в группе всё ещё молчит", r)
	}
	if !e2.groupSilent(e2eGroup("other")) {
		t.Error("остальные группы перестали молчать")
	}
	_, _ = tool.Handle(ctx, json.RawMessage(`{"silence":true,"group":"везде"}`))
	if !e2.groupSilent(g2) {
		t.Error("«молчи везде» не действует на группу с прежним исключением")
	}
}

func TestSweepForwardedCheckKeepsClient(t *testing.T) {
	ctx := context.Background()
	src, dst := e2eGroup("fs"), e2eGroup("fd")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{src.String(): "Оплата клиентов", dst.String(): "Оплата КЛНТ"}})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	_ = e.db.SetForwardRule(ctx, src.String(), "Оплата клиентов", dst.String(), "Оплата КЛНТ")
	ocr := sberCheck("13000", "14:00:00")
	waID := e.sendReceipt(src, user("79001110050"), "Сафаи", ocr, "Магомедов Ахмед")
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Магомедов Ахмед\n"+ocr, time.Now())
	e.applyForwardRules(ctx, src.String(), "Сафаи", waID, []byte("jpeg"), ".jpg", "Магомедов Ахмед\n"+ocr, time.Now()) // повторная доставка
	e.waitIdle()
	if got := dayTotals(e, dst); got["Магомедов Ахмед"] != 13000 || sum(got) != 13000 {
		t.Errorf("сбор целевой группы: %v", got)
	}
	from, to := today()
	if got := monthTotals(e, nil, from, to); got != 13000 {
		t.Errorf("по всем группам чек посчитан не один раз: %v", got)
	}
	// Чек без имени, имя написали позже — копия тоже получает клиента; про копию не спрашиваем.
	ocr2 := sberCheck("8000", "15:00:00")
	w2 := e.sendReceipt(src, user("79001110050"), "Сафаи", ocr2, "")
	e.applyForwardRules(ctx, src.String(), "Сафаи", w2, []byte("jpeg"), ".jpg", ocr2, time.Now())
	e.sendGroup(groupText(src, user("79001110050"), "Сафаи", "Исаев Харон"))
	e.age(dst.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	for _, m := range e.sentSince(n) {
		if m.Chat == dst.String() {
			t.Errorf("бот спросил про копию в целевой группе: %q", m.Text)
		}
	}
	if got := dayTotals(e, dst); got["Исаев Харон"] != 8000 {
		t.Errorf("копия не получила позже названного клиента: %v", got)
	}
}

func TestSweepHistoryImportSkipsCommands(t *testing.T) {
	grp, owner := e2eGroup("hd"), user("79001110040")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	pay := aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.lastUserText(), "Завтра") {
			return aiReply{Text: `{"payments":[],"clarify":""}`}
		}
		return pay(c)
	}
	cmd := groupText(grp, owner, "Ахмед", "Джарвис запиши наличку Магомедов Ахмед 20000")
	plan := groupText(grp, owner, "Ахмед", "Завтра Магомедов Ахмед 5000 занесёт")
	real := groupText(grp, owner, "Ахмед", "Исаев Харон 10000 у Дени")
	var hm []*waHistorySync.HistorySyncMsg
	for _, m := range []*waProto.Message{cmd.Message, plan.Message, real.Message} {
		id := e2eID("H")
		hm = append(hm, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:     &waProto.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(id), Participant: proto.String(owner.String())},
			Message: m, MessageTimestamp: proto.Uint64(uint64(time.Now().Unix()))}})
	}
	hs := &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: hm}}}
	e.importHistorySync(hs)
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 10000 || sum(got) != 10000 {
		t.Errorf("импорт истории: %v, хочу только Исаев Харон=10000", got)
	}
}
