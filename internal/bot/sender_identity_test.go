package bot

import (
	"context"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func TestSenderIdentityOwnerGateRejectsLIDNumericCoincidence(t *testing.T) {
	phone := "79280000777"
	info := types.MessageInfo{MessageSource: types.MessageSource{Sender: types.NewJID(phone, types.HiddenUserServer)}}
	set := map[string]bool{phone: true}
	if senderInSet(set, info) {
		t.Fatal("LID numeric coincidence authorized a different identity")
	}
	b := &Bot{secretRecipients: set, selfUser: phone}
	if b.isSecretRecipient(info) {
		t.Fatal("LID numeric coincidence authorized secret file")
	}
	msg := groupReply(e2eGroup("identity-reply"), user("79280000666"), "Работник", "текст", "quoted", types.NewJID(phone, types.HiddenUserServer), "цитата")
	if b.isReplyToBot(msg) {
		t.Fatal("foreign LID with equal digits was treated as the bot")
	}
	info.SenderAlt = user(phone)
	if !senderInSet(set, info) {
		t.Fatal("verified SenderAlt lost authorization")
	}
}

func TestSenderIdentityQuoteUsesStoredAuthorAndGroup(t *testing.T) {
	ctx := context.Background()
	group, other := e2eGroup("identity-quote"), e2eGroup("identity-quote-other")
	e := newE2E(t, e2eOpts{})
	source := e2eID("SOURCE")
	if _, _, err := e.db.SaveRawMessage(ctx, source, group.String(), user("79280000661").String(), user("79280000661").String(), "Мансур", "исходный чек", false, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	msg := groupReply(group, user("79280000662"), "Владелец", "запомни этот номер", source, user("79280000663"), "цитата")
	note := e.quotedSenderPhoneNote(ctx, msg)
	if !strings.Contains(note, "79280000661") || strings.Contains(note, "79280000663") {
		t.Fatalf("quoted participant overrode the saved author: %s", note)
	}
	msg.Info.Chat = other
	note = e.quotedSenderPhoneNote(ctx, msg)
	if strings.Contains(note, "номер отправителя") || strings.Contains(note, "id сообщения") {
		t.Fatalf("global waID leaked an author from another group: %s", note)
	}
}

func TestSenderIdentityHistoryUsesTopLevelParticipant(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("identity-history")
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	message := reliableHistoryMessage(group, user("79280000664"), e2eID("IDENTITY-HISTORY"), "Мансур", time.Now(), &waProto.Message{Conversation: proto.String("Каталов Ахмед 5000 наличка")})
	message.Message.Key.Participant = nil
	message.Message.Participant = proto.String(user("79280000664").String())
	e.importHistorySync(reliableHistoryBatch(group, message))
	ref, err := e.db.RawMessageByWaID(ctx, message.Message.Key.GetID())
	if err != nil || !ref.Found || ref.SenderJID != user("79280000664").String() {
		t.Fatalf("history attributed worker to group: %+v %v", ref, err)
	}
	if sum(dayTotals(e, group)) != 5000 {
		t.Fatal("top-level participant payment was lost")
	}
}

func TestSenderIdentityPNToLIDDoesNotStealAnotherEmployeesReceipt(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("identity-name-pairing")
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	lid := types.NewJID("999000000000001", types.HiddenUserServer)
	phone := user("79280000665")
	check := e2eID("IDENTITY-CHECK")
	raw, _, err := e.db.SaveRawMessage(ctx, check, group.String(), lid.String(), "", "Мансур", sberCheck("15000", "11:00:00"), true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, group, lid.String(), check, sberCheck("15000", "11:00:00"), raw, time.Now(), nil, ".jpg", "", 0)
	e.sendGroup(groupText(group, user("79280000666"), "Мансур", "Исаев Харон"))
	if name, confirmed, err := e.db.ReceiptClientByMessage(ctx, check); err != nil || confirmed || name == "Исаев Харон" {
		t.Fatalf("same display name stole a different sender's receipt: %q %v %v", name, confirmed, err)
	}
	name := groupText(group, phone, "Мансур", "Каталов Ахмед")
	name.Info.SenderAlt = lid
	e.sendGroup(name)
	client, confirmed, err := e.db.ReceiptClientByMessage(ctx, check)
	if err != nil || !confirmed || client != "Каталов Ахмед" {
		t.Fatalf("observed PN/LID failed to link same employee: %q %v %v", client, confirmed, err)
	}
}

func TestSenderIdentityCashHintFollowsObservedAliasOnly(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("identity-cash-photo")
	e := newE2E(t, e2eOpts{})
	lid := types.NewJID("999000000000002", types.HiddenUserServer)
	phone := user("79280000667")
	e.markPendingCash(group, lid.String())
	if e.consumePendingCash(group, user("79280000668").String()) {
		t.Fatal("cash photo moved to another employee")
	}
	if err := e.db.RememberSenderIdentity(ctx, lid.String(), phone.String()); err != nil {
		t.Fatal(err)
	}
	if !e.consumePendingCash(group, phone.String()) {
		t.Fatal("confirmed PN/LID lost the cash-photo context")
	}
	if e.consumePendingCash(group, lid.String()) {
		t.Fatal("same cash-photo context was consumed twice")
	}
}
