package bot

import (
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии: правка сообщения в WhatsApp и поправка «не 5000 а 6000».

func editOf(grp, snd types.JID, origID, newText string) *events.Message {
	return &events.Message{IsEdit: true,
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: snd, IsGroup: true},
			ID: e2eID("EDIT"), Timestamp: time.Now(), Edit: types.EditAttributeMessageEdit},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(origID), Participant: proto.String(snd.String())},
			EditedMessage: &waProto.Message{Conversation: proto.String(newText)}}}}
}

func TestSweepEditedPaymentMessage(t *testing.T) {
	grp, snd := e2eGroup("edit"), user("79001110004")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	m := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000")
	e.sendGroup(m)
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 {
		t.Fatalf("исходный платёж: %v", got)
	}
	n := len(e.sent())
	e.event(editOf(grp, snd, m.Info.ID, "Магомедов Ахмед 15000"))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 15000 || sum(got) != 15000 {
		t.Errorf("правка суммы: %v, хочу Магомедов Ахмед=15000", got)
	}
	if out := e.sentSince(n); len(out) != 1 || !strings.Contains(out[0].Text, "стало Магомедов Ахмед 15 000") {
		t.Errorf("ответ на правку: %+v", out)
	}
	// Повторная доставка той же правки ничего не меняет.
	ed := editOf(grp, snd, m.Info.ID, "Исаев Харон 15000")
	e.event(ed)
	e.event(ed)
	got = dayTotals(e, grp)
	if got["Исаев Харон"] != 15000 || sum(got) != 15000 {
		t.Errorf("правка клиента: %v, хочу Исаев Харон=15000", got)
	}
}

func TestSweepEditedCheckCaptionAndName(t *testing.T) {
	grp, snd := e2eGroup("editc"), user("79001110005")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
	// Имя перед чеком, потом чек; имя исправили — чек переходит к новому клиенту.
	nm := groupText(grp, snd, "Сафаи", "Магомедов Ахмед")
	e.sendGroup(nm)
	e.sendReceipt(grp, snd, "Сафаи", sberCheck("12000", "10:00:00"), "")
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 12000 {
		t.Fatalf("чек по имени: %v", got)
	}
	e.event(editOf(grp, snd, nm.Info.ID, "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 12000 || sum(got) != 12000 {
		t.Errorf("исправленное имя перед чеком: %v, хочу Исаев Харон=12000", got)
	}
}

func TestSweepAmountCorrection(t *testing.T) {
	for _, withAI := range []bool{false, true} {
		grp, snd := e2eGroup("corr"), user("79001110006")
		e := newE2E(t, e2eOpts{WithAI: withAI})
		if withAI {
			e.ai = aiNamesAndPayments("Магомедов Ахмед")
		} else {
			e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
		}
		m := groupText(grp, snd, "Сафаи", "Магомедов Ахмед 5000")
		e.sendGroup(m)
		// Без свайпа — свой свежий платёж на 5000.
		e.sendGroup(groupText(grp, snd, "Сафаи", "не 5000 а 6000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 6000 || sum(got) != 6000 {
			t.Errorf("ai=%v «не 5000 а 6000»: %v", withAI, got)
		}
		// Свайпом на сам платёж.
		e.sendGroup(groupReply(grp, snd, "Сафаи", "не 6000 а 7000", m.Info.ID, snd, "Магомедов Ахмед 5000"))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 7000 || sum(got) != 7000 {
			t.Errorf("ai=%v свайп «не 6000 а 7000»: %v", withAI, got)
		}
		// Поправка без подходящего платежа — не становится новым платежом.
		e.sendGroup(groupText(grp, snd, "Сафаи", "не 3000 а 4000"))
		if got := sum(dayTotals(e, grp)); got != 7000 {
			t.Errorf("ai=%v лишняя поправка записалась платежом: %v", withAI, got)
		}
	}
}
