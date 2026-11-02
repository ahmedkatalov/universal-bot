package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func reliableHistoryMessage(grp, sender types.JID, id, name string, at time.Time, message *waProto.Message) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:      &waProto.MessageKey{RemoteJID: proto.String(grp.String()), Participant: proto.String(sender.String()), FromMe: proto.Bool(false), ID: proto.String(id)},
		PushName: proto.String(name), MessageTimestamp: proto.Uint64(uint64(at.Unix())), Message: message,
	}}
}

func reliableHistoryBatch(grp types.JID, messages ...*waHistorySync.HistorySyncMsg) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: messages}}}
}

func TestHistoryReliabilityDownloadFailureKeepsEveryMediaFormat(t *testing.T) {
	ctx := context.Background()
	grp, worker := e2eGroup("history-download"), user("79280000701")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ"}})
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	// No WhatsApp client: downloading fails, rather than panicking. Captions
	// of unavailable receipt photos must not turn into separate text payments.
	contents := []*waProto.Message{
		{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Магомедов Ахмед 15000")}},
		{DocumentMessage: &waProto.DocumentMessage{Mimetype: proto.String("image/png"), FileName: proto.String("receipt.png")}},
		{DocumentMessage: &waProto.DocumentMessage{Mimetype: proto.String("application/pdf"), FileName: proto.String("receipt.pdf")}},
		{EphemeralMessage: &waProto.FutureProofMessage{Message: &waProto.Message{
			ImageMessage: &waProto.ImageMessage{Caption: proto.String("Магомедов Ахмед 15000"), ContextInfo: &waProto.ContextInfo{IsForwarded: proto.Bool(true)}},
		}}},
		{DocumentWithCaptionMessage: &waProto.FutureProofMessage{Message: &waProto.Message{
			DocumentMessage: &waProto.DocumentMessage{Mimetype: proto.String("image/jpeg"), FileName: proto.String("forwarded.jpg"), Caption: proto.String("Ханатаев акраман")},
		}}},
	}
	var messages []*waHistorySync.HistorySyncMsg
	for i, content := range contents {
		messages = append(messages, reliableHistoryMessage(grp, worker, e2eID("HMEDIA"), "Мансур", time.Now().Add(-10*time.Minute+time.Duration(i)*time.Second), content))
	}
	data := reliableHistoryBatch(grp, messages...)
	seen, receipts, payments := e.importHistorySync(data)
	if seen != len(contents) || receipts != 0 || payments != 0 {
		t.Fatalf("wrong classification: %d %d %d", seen, receipts, payments)
	}
	for _, message := range messages {
		ref, err := e.db.RawMessageByWaID(ctx, message.Message.GetKey().GetID())
		if err != nil || !ref.Found || !ref.HasMedia || !e.db.NeedsMediaReread(ctx, ref.ID) {
			t.Fatalf("lost failed media: %+v / %v", ref, err)
		}
		if ids, err := e.db.TxIDsByRawID(ctx, ref.ID); err != nil || len(ids) != 0 {
			t.Fatalf("unavailable photo caption became payment: %v / %v", ids, err)
		}
	}
	items, err := e.db.GroupMediaToReread(ctx, grp.String(), time.Now().Add(-time.Hour), 10)
	if err != nil || len(items) != len(contents) {
		t.Fatalf("retry queue lost media: %+v / %v", items, err)
	}
	for _, item := range items {
		var message waProto.Message
		if len(item.Proto) == 0 || proto.Unmarshal(item.Proto, &message) != nil {
			t.Fatal("original media download descriptor was not persisted")
		}
		if message.GetImageMessage() == nil && message.GetDocumentMessage() == nil {
			t.Fatal("history wrapper was persisted without extracting its downloadable media")
		}
		if doc := message.GetDocumentMessage(); doc != nil && doc.GetFileName() == "forwarded.jpg" && item.Caption != "Ханатаев акраман" {
			t.Fatalf("document receipt lost its client caption: %+v", item)
		}
	}
	e.importHistorySync(data)
	items, err = e.db.GroupMediaToReread(ctx, grp.String(), time.Now().Add(-time.Hour), 10)
	if err != nil || len(items) != len(contents) {
		t.Fatalf("duplicate history changed failed media: %+v / %v", items, err)
	}
	if len(e.sent()) != 0 {
		t.Fatalf("history import sent messages: %+v", e.sent())
	}
}

func TestHistoryReliabilityVisionFailureThenRecovery(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "1")
	ctx := context.Background()
	grp, worker := e2eGroup("history-vision"), user("79280000702")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "СБ"}})
	e.aliases.Add("Арсаев Магомед", "Арсаев Магомед")
	waID, at := e2eID("HRECOVERY"), time.Now().Add(-10*time.Minute)
	path := filepath.Join(t.TempDir(), "receipt.jpg")
	if err := os.WriteFile(path, []byte("jpeg-local-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), worker.String(), worker.String(), "Мансур", "Арсаев Магомед 7200", true, path, at)
	if err != nil {
		t.Fatal(err)
	}
	content := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Арсаев Магомед 7200")}}
	data := reliableHistoryBatch(grp, reliableHistoryMessage(grp, worker, waID, "Мансур", at, content))
	// A malformed provider response is an absent verdict, not "not a check".
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{}`} }
	_, receipts, _ := e.importHistorySync(data)
	if receipts != 0 || !e.db.NeedsMediaReread(ctx, rawID) {
		t.Fatal("temporary extraction failure was marked parsed")
	}
	items, err := e.db.GroupMediaToReread(ctx, grp.String(), at.Add(-time.Minute), 10)
	if err != nil || len(items) != 1 || len(items[0].Proto) == 0 || items[0].MediaPath != path {
		t.Fatalf("failed extraction lost recovery evidence: %+v / %v", items, err)
	}
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: fmt.Sprintf(`{"kind":"receipt","bank":"Сбербанк","recipient":"Владелец Карты","sender":"Банковский Плательщик","amount":7200,"doc_number":"HISTORY-RECOVER-0001","datetime":"%s 10:00:00"}`, time.Now().Format("2006-01-02"))}
		}
		return aiReply{Text: `{"client":"Арсаев Магомед"}`}
	}
	_, receipts, _ = e.importHistorySync(data)
	if receipts != 1 || e.db.NeedsMediaReread(ctx, rawID) {
		t.Fatal("saved receipt did not close the pending media")
	}
	_, amount, found, err := e.db.ReceiptOfRaw(ctx, rawID)
	if err != nil || !found || amount != 7200 {
		t.Fatalf("recovered receipt: %v %v %v", amount, found, err)
	}
	day := time.Now()
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	stats, err := e.db.SenderStats(ctx, from, from.AddDate(0, 0, 1), []string{grp.String()}, "Мансур")
	if err != nil || len(stats) != 1 || stats[0].Name != "Мансур" || stats[0].Total != 7200 {
		t.Fatalf("import lost worker display name/client caption: %+v / %v", stats, err)
	}
	e.importHistorySync(data)
	ledger, err := e.db.ReceiptsLedger(ctx, from, from.AddDate(0, 0, 1), "", []string{grp.String()}, 10)
	if err != nil || len(ledger) != 1 || ledger[0].Client != "Арсаев Магомед" {
		t.Fatalf("receipt recovery duplicated or lost caption: %+v / %v", ledger, err)
	}
	if len(e.sent()) != 0 {
		t.Fatalf("recovery must remain silent: %+v", e.sent())
	}
}

func TestHistoryReliabilityExplicitOtherIsCompleted(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "1")
	grp, worker := e2eGroup("history-non-receipt"), user("79280000703")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{"kind":"other","amount":0}`} }
	path := filepath.Join(t.TempDir(), "picture.jpg")
	if err := os.WriteFile(path, []byte("non-receipt-picture-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	waID, at := e2eID("OTHER"), time.Now().Add(-10*time.Minute)
	raw, _, err := e.db.SaveRawMessage(context.Background(), waID, grp.String(), worker.String(), worker.String(), "Мансур", "", true, path, at)
	if err != nil {
		t.Fatal(err)
	}
	e.importHistorySync(reliableHistoryBatch(grp, reliableHistoryMessage(grp, worker, waID, "Мансур", at, &waProto.Message{ImageMessage: &waProto.ImageMessage{}})))
	if e.db.NeedsMediaReread(context.Background(), raw) {
		t.Fatal("explicit non-receipt was left in endless recognition retry")
	}
}

func TestHistoryReliabilityForwardedSberPhotoStaysPendingUntilRecognized(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	ctx := context.Background()
	grp, worker := e2eGroup("forwarded-sber-nov1"), user("79280000705")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
	at := time.Date(2026, time.November, 1, 14, 24, 0, 0, time.FixedZone("MSK", 3*60*60))
	waID := e2eID("FORWARDED-SBER")
	caption := "Ханатаев акраман"
	localBytes := []byte("forwarded-photo-of-sber-receipt-fixture")
	path := filepath.Join(t.TempDir(), "receipt.jpg")
	if err := os.WriteFile(path, localBytes, 0600); err != nil {
		t.Fatal(err)
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), worker.String(), worker.String(), "~M", caption, true, path, at)
	if err != nil {
		t.Fatal(err)
	}
	content := &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{Message: &waProto.Message{
		DocumentWithCaptionMessage: &waProto.FutureProofMessage{Message: &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			Mimetype: proto.String("image/jpeg"), FileName: proto.String("receipt.jpg"), Caption: proto.String(caption),
			DirectPath: proto.String("/receipt?test=1"), MediaKey: []byte("test-media-key"),
			ContextInfo: &waProto.ContextInfo{IsForwarded: proto.Bool(true)},
		}}},
	}}}
	history := reliableHistoryBatch(grp, reliableHistoryMessage(grp, worker, waID, "~M", at, content))
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{}`} }
	_, receipts, _ := e.importHistorySync(history)
	if receipts != 0 || !e.db.NeedsMediaReread(ctx, rawID) {
		t.Fatal("unreadable forwarded receipt must remain pending")
	}
	items, err := e.db.GroupMediaToReread(ctx, grp.String(), at.Add(-time.Minute), 10)
	if err != nil || len(items) != 1 || items[0].Caption != caption || len(items[0].Proto) == 0 {
		t.Fatalf("forwarded receipt lost retry evidence: %+v / %v", items, err)
	}
	// If the local copy disappears, its WhatsApp source is still available.
	// Without a connected client the download fails and the message stays open.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if e.rereadMedia(ctx, items[0]) || !e.db.NeedsMediaReread(ctx, rawID) {
		t.Fatal("failed redownload closed the pending forwarded receipt")
	}
	if err := os.WriteFile(path, localBytes, 0600); err != nil {
		t.Fatal(err)
	}
	e.ai = func(c aiCall) aiReply {
		return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Милана Нажудовна К.","sender":"Элина Зилаудиевна Г.","amount":30000,"commission":0,"doc_number":"1000000006086701079","auth_code":"024307","datetime":"2026-11-01 14:23:06"}`}
	}
	_, receipts, _ = e.importHistorySync(history)
	if receipts != 1 || e.db.NeedsMediaReread(ctx, rawID) {
		t.Fatal("recognized receipt was not recovered")
	}
	r := e.db.TestReceiptByMessage(ctx, waID)
	if !r.Found || r.Amount != 30000 || r.Recipient != "Ханатаев Акраман" || !r.Confirmed || r.NeedsReview || r.Duplicate {
		t.Fatalf("recovered receipt lost the client or money: %+v", r)
	}
	details, err := e.db.ReceiptDetailsForPerson(ctx, "Ханатаев", 10)
	if err != nil || len(details) != 1 || details[0].DocNumber != "1000000006086701079" || details[0].AuthCode != "024307" || details[0].CardOwner != "Милана Нажудовна К." {
		t.Fatalf("recovered bank identity: %+v / %v", details, err)
	}
	e.importHistorySync(history)
	if details, err = e.db.ReceiptDetailsForPerson(ctx, "Ханатаев", 10); err != nil || len(details) != 1 {
		t.Fatalf("repeated history duplicated receipt: %+v / %v", details, err)
	}
	if len(e.sent()) != 0 {
		t.Fatalf("historical recovery sent messages: %+v", e.sent())
	}
}

func TestHistoryReliabilityRevocationIsAppliedQuietlyAndScoped(t *testing.T) {
	ctx := context.Background()
	grp, other, worker := e2eGroup("history-revoke"), e2eGroup("history-foreign"), user("79280000704")
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	at, waID := time.Now().Add(-20*time.Minute), e2eID("OLDPAY")
	original := reliableHistoryMessage(grp, worker, waID, "Мансур", at, &waProto.Message{Conversation: proto.String("Каталов Ахмед 5000 наличка")})
	revoke := reliableHistoryMessage(grp, worker, e2eID("REVOKE"), "Мансур", at.Add(time.Minute), &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(waID), Participant: proto.String(worker.String())},
	}})
	e.importHistorySync(reliableHistoryBatch(grp, revoke, original)) // deliberately unsorted
	ref, err := e.db.RawMessageByWaID(ctx, waID)
	if err != nil || !ref.Deleted || sum(dayTotals(e, grp)) != 0 {
		t.Fatalf("history deletion left money in ledger: %+v / %v", ref, err)
	}
	otherID := e2eID("FOREIGN")
	e.importHistorySync(reliableHistoryBatch(other, reliableHistoryMessage(other, worker, otherID, "Мансур", at, &waProto.Message{Conversation: proto.String("Каталов Ахмед 7000 наличка")})))
	foreignRevoke := reliableHistoryMessage(grp, worker, e2eID("FOREIGN-REVOKE"), "Мансур", at.Add(2*time.Minute), &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{ID: proto.String(otherID)}}})
	e.importHistorySync(reliableHistoryBatch(grp, foreignRevoke))
	foreign, err := e.db.RawMessageByWaID(ctx, otherID)
	if err != nil || foreign.Deleted {
		t.Fatalf("one group's history revoked another group's payment: %+v / %v", foreign, err)
	}
	lateID := e2eID("LATE-ORIGINAL")
	lateRevoke := reliableHistoryMessage(grp, worker, e2eID("EARLY-REVOKE"), "Мансур", at.Add(time.Minute), &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{ID: proto.String(lateID)}}})
	e.importHistorySync(reliableHistoryBatch(grp, lateRevoke))
	lateOriginal := reliableHistoryMessage(grp, worker, lateID, "Мансур", at, &waProto.Message{Conversation: proto.String("Каталов Ахмед 9000 наличка")})
	e.importHistorySync(reliableHistoryBatch(grp, lateOriginal))
	late, err := e.db.RawMessageByWaID(ctx, lateID)
	if err != nil || !late.Deleted || sum(dayTotals(e, grp)) != 0 {
		t.Fatalf("newest-first history resurrected deleted payment: %+v / %v", late, err)
	}
	if len(e.sent()) != 0 {
		t.Fatalf("historical deletions must not send messages: %+v", e.sent())
	}
}
