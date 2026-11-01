package bot

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

func inboxRestartedBot(e *e2eBot, group types.JID) *Bot {
	return &Bot{
		db: e.db, aliases: parser.NewAliasMap(), botName: "Джарвис",
		history: map[string][]ai.Turn{}, pending: map[string][]pendingReceipt{},
		sentMsgs: map[string][]string{}, pendingCash: map[string]time.Time{},
		clarify: newClarifyState(), pairWindow: 15 * time.Minute,
		muted: map[string]bool{}, seenMsgs: map[string]time.Time{},
		selfUser: "79990000000", sendHook: e.sendHook,
		groupsCache: map[types.JID]string{group: "СБ"}, groupsFetched: time.Now().Add(24 * time.Hour),
	}
}

func inboxQueueBarrier(b *Bot, group types.JID) func() {
	blocked := make(chan struct{})
	var once sync.Once
	b.enqueueChat(group.String(), func() { <-blocked })
	return func() { once.Do(func() { close(blocked) }) }
}

func inboxPendingFor(t *testing.T, d *db.DB, group types.JID) db.GroupInboxEvent {
	t.Helper()
	items, err := d.DueGroupEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.GroupJID == group.String() {
			return item
		}
	}
	t.Fatalf("captured pending event missing for %s", group)
	return db.GroupInboxEvent{}
}

func TestGroupInboxRestartsAndRedeliversExactlyOnce(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("inbox-restart")
	e := newE2E(t, e2eOpts{})
	msg := groupText(group, user("79280000801"), "Мансур", "Каталов Ахмед 5000 наличка")
	release := inboxQueueBarrier(e.Bot, group)
	defer release()
	e.captureGroupMessage(ctx, msg)
	saved := inboxPendingFor(t, e.db, group)
	before, err := e.db.RawMessageByWaID(ctx, msg.Info.ID)
	if err != nil || before.Found {
		t.Fatalf("memory worker ran before simulated restart: %+v / %v", before, err)
	}
	restarted := inboxRestartedBot(e, group)
	restarted.recoverGroupInbox(ctx)
	restarted.drainQueues(10 * time.Second)
	ref, err := e.db.RawMessageByWaID(ctx, msg.Info.ID)
	if err != nil || !ref.Found {
		t.Fatalf("restart failed to restore captured message: %+v / %v", ref, err)
	}
	if got := sum(dayTotals(e, group)); got != 5000 {
		t.Fatalf("recovered collection=%v, want 5000", got)
	}
	// The stale pre-restart worker and later redelivery must not repeat work.
	release()
	e.drainQueues(10 * time.Second)
	e.captureGroupMessage(ctx, msg)
	restarted.recoverGroupInbox(ctx)
	e.drainQueues(10 * time.Second)
	restarted.drainQueues(10 * time.Second)
	ids, err := e.db.TxIDsByRawID(ctx, ref.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("redelivery duplicated payments: %v / %v", ids, err)
	}
	again, inserted, err := e.db.CaptureGroupEvent(ctx, saved)
	if err != nil || inserted || again.ID != saved.ID || again.State != "done" || again.Attempts != 1 {
		t.Fatalf("event dispatched twice: %+v inserted=%v / %v", again, inserted, err)
	}
}

func TestGroupInboxSameMessageIDInDifferentGroupsIsCapturedIndependently(t *testing.T) {
	ctx := context.Background()
	g1, g2 := e2eGroup("inbox-same-id-a"), e2eGroup("inbox-same-id-b")
	e := newE2E(t, e2eOpts{})
	release1, release2 := inboxQueueBarrier(e.Bot, g1), inboxQueueBarrier(e.Bot, g2)
	defer func() { release1(); release2(); e.drainQueues(10 * time.Second) }()
	m1 := groupText(g1, user("79280000802"), "Мансур", "Каталов Ахмед 5000 наличка")
	m2 := groupText(g2, user("79280000802"), "Мансур", "Каталов Ахмед 5000 наличка")
	m2.Info.ID = m1.Info.ID
	e.captureGroupMessage(ctx, m1)
	e.captureGroupMessage(ctx, m2)
	e1, e2 := inboxPendingFor(t, e.db, g1), inboxPendingFor(t, e.db, g2)
	if e1.ID == e2.ID || e1.Key == e2.Key || e1.WaID != e2.WaID || e1.GroupJID == e2.GroupJID {
		t.Fatalf("inbox identity omitted the group: %+v / %+v", e1, e2)
	}
}

func TestGroupInboxPreservesSenderAltAndQuotedContext(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("inbox-metadata")
	e := newE2E(t, e2eOpts{})
	phone := user("79280000803")
	lid := types.NewJID("999999999999999", types.HiddenUserServer)
	quotedID := e2eID("QUOTED")
	msg := groupReply(group, lid, "Мансур", "Каталов Ахмед 5000 наличка", quotedID, user("79280000804"), "Чек")
	msg.Info.SenderAlt = phone
	release := inboxQueueBarrier(e.Bot, group)
	defer release()
	e.captureGroupMessage(ctx, msg)
	saved := inboxPendingFor(t, e.db, group)
	var info types.MessageInfo
	var payload waProto.Message
	if err := json.Unmarshal(saved.Metadata, &info); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(saved.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if info.Sender != lid || info.SenderAlt != phone || info.Chat != group || info.PushName != "Мансур" || !info.Timestamp.Equal(msg.Info.Timestamp) {
		t.Fatalf("inbox metadata changed identities: %+v", info)
	}
	restored := &events.Message{Info: info, Message: &payload}
	if extractQuotedStanzaID(restored) != quotedID {
		t.Fatal("quoted context did not survive durable serialization")
	}
	release()
	e.drainQueues(10 * time.Second)
	ref, err := e.db.RawMessageByWaID(ctx, msg.Info.ID)
	if err != nil || !ref.Found || ref.QuotedID != quotedID || ref.SenderJID != lid.String() {
		t.Fatalf("restored message lost source/quote: %+v / %v", ref, err)
	}
	digits, err := e.db.SenderPhoneByMessageID(ctx, msg.Info.ID)
	if err != nil || digits != phone.User {
		t.Fatalf("LID was substituted for real phone: %q / %v", digits, err)
	}
}

func TestGroupInboxAIEditAndRevokeStayInMessageOrder(t *testing.T) {
	t.Setenv("PAYMENT_READS", "1")
	ctx := context.Background()
	group, worker := e2eGroup("inbox-order"), user("79280000805")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			if strings.Contains(c.lastUserText(), "6000") {
				return aiReply{Text: `{"payments":[{"name":"Каталов Ахмед","amount":6000,"cash":true}],"clarify":""}`}
			}
			enterOnce.Do(func() { close(entered) })
			<-release
			return aiReply{Text: `{"payments":[{"name":"Каталов Ахмед","amount":5000,"cash":true}],"clarify":""}`}
		}
		return aiReply{Text: `{"client":""}`}
	}
	original := groupText(group, worker, "Мансур", "Каталов Ахмед 5000 наличка")
	e.captureGroupMessage(ctx, original)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("original message did not reach extraction")
	}
	edit := groupText(group, worker, "Мансур", "")
	edit.Message = &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waProto.MessageKey{RemoteJID: proto.String(group.String()), ID: proto.String(original.Info.ID), Participant: proto.String(worker.String())},
		EditedMessage: &waProto.Message{Conversation: proto.String("Каталов Ахмед 6000 наличка")},
	}}
	revoke := groupText(group, worker, "Мансур", "")
	revoke.Message = &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{RemoteJID: proto.String(group.String()), ID: proto.String(original.Info.ID), Participant: proto.String(worker.String())}}}
	e.captureGroupMessage(ctx, edit)
	e.captureGroupMessage(ctx, revoke)
	// While recognition is blocked neither a later edit nor its revoke may
	// mutate the source. This probes queue ordering, not model response speed.
	time.Sleep(150 * time.Millisecond)
	before, err := e.db.RawMessageByWaID(ctx, original.Info.ID)
	if err != nil || !before.Found || before.Deleted || before.Body != "Каталов Ахмед 5000 наличка" {
		t.Errorf("later event overtook recognition: %+v / %v", before, err)
	}
	unblock()
	e.waitIdle()
	final, err := e.db.RawMessageByWaID(ctx, original.Info.ID)
	if err != nil || !final.Deleted || final.Body != "Каталов Ахмед 6000 наличка" {
		t.Fatalf("ordered edit/revoke not applied: %+v / %v", final, err)
	}
	if got := sum(dayTotals(e, group)); got != 0 {
		t.Fatalf("revoked queued payment remains counted: %v", got)
	}
	coverage, err := e.db.GroupInboxStatus(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{group.String()}, 10)
	if err != nil || coverage.Pending+coverage.Processing+coverage.Review != 0 {
		t.Fatalf("ordered events did not settle: %+v / %v", coverage, err)
	}
}
