package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestReceiptCandidateOtherLiveAndRawRetryRemainOpen(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	t.Setenv("RECEIPT_VISION_FIRST", "1")
	for _, tc := range []struct{ name, caption, ocr string }{
		{"named-empty-ocr", "Ханатаев акраман", ""},
		{"bank-text", "", "СБЕРБАНК\nЧек по операции\nСумма перевода\n30000.00 ₽"},
		{"plain-picture", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newE2E(t, e2eOpts{WithAI: true})
			e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
			e.ai = func(aiCall) aiReply { return aiReply{Text: `{"kind":"other"}`} }
			grp, worker := e2eGroup("candidate-live-"+tc.name), user("79280000707")
			path := filepath.Join(t.TempDir(), "candidate.jpg")
			if err := os.WriteFile(path, []byte("receipt-candidate-image"), 0600); err != nil {
				t.Fatal(err)
			}
			waID := l4intakePhoto(e, grp, worker, "~M", tc.ocr, tc.caption, path)
			raw, err := e.db.RawMessageByWaID(ctx, waID)
			wantPending := tc.name != "plain-picture"
			if err != nil || !raw.Found || e.db.NeedsMediaReread(ctx, raw.ID) != wantPending {
				t.Fatalf("other verdict silently closed payment candidate: %+v / %v", raw, err)
			}
			if r := e.db.TestReceiptByMessage(ctx, waID); r.Found {
				t.Fatalf("candidate was invented as a confirmed bank receipt: %+v", r)
			}
			if !wantPending {
				if len(e.sent()) != 0 {
					t.Fatalf("plain other image created a review notice: %+v", e.sent())
				}
				return
			}
			if err := e.db.SetMediaInfo(ctx, raw.ID, tc.caption, nil); err != nil {
				t.Fatal(err)
			}
			items, err := e.db.GroupMediaToReread(ctx, grp.String(), time.Now().Add(-time.Hour), 10)
			if err != nil || len(items) != 1 {
				t.Fatalf("candidate unavailable for manual reread: %+v / %v", items, err)
			}
			if e.rereadMedia(ctx, items[0]) || !e.db.NeedsMediaReread(ctx, raw.ID) {
				t.Fatal("raw-media retry closed the same contradictory candidate")
			}
			status, err := e.db.GroupReading(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{grp.String()}, 10)
			if err != nil || status.UnreadMedia != 1 || status.UnresolvedMessages != 1 || len(status.Issues) != 1 || !strings.Contains(strings.Join(status.Issues[0].Reasons, " "), "вложение на проверке") {
				t.Fatalf("candidate disappeared from reading status: %+v / %v", status, err)
			}
			if got := sum(dayTotals(e, grp)); got != 0 {
				t.Fatalf("unverified candidate added income: %v", got)
			}
			if len(e.sent()) != 1 {
				t.Fatalf("expected one review notice across original/retry: %+v", e.sent())
			}
			if tc.caption == "" {
				return
			}
			e.ai = func(aiCall) aiReply {
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Милана Нажудовна К.","sender":"Элина Зилаудиевна Г.","amount":30000,"doc_number":"1000000006086701079","auth_code":"024307","datetime":"2026-11-01 14:23:06"}`}
			}
			if !e.rereadMedia(ctx, items[0]) {
				t.Fatal("verified manual reread could not resolve the candidate")
			}
			if r := e.db.TestReceiptByMessage(ctx, waID); !r.Found || r.Amount != 30000 || r.NeedsReview || !r.Confirmed || r.Recipient != "Ханатаев Акраман" {
				t.Fatalf("verified reread lost the named receipt: %+v", r)
			}
			state, err := e.db.GroupEventRawState(ctx, waID)
			if err != nil || state.HandledAs != "" {
				t.Fatalf("verified reread retained obsolete review hold: %+v / %v", state, err)
			}
		})
	}
}

func TestReceiptCandidateOtherHistoryRemainsOpen(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	for _, tc := range []struct{ name, body, caption string }{
		{"named-photo", "Ханатаев акраман", "Ханатаев акраман"},
		{"bank-text", "СБЕРБАНК\nЧек по операции\nСумма перевода\n30000.00 ₽", ""},
		{"plain-picture", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = func(aiCall) aiReply { return aiReply{Text: `{"kind":"other"}`} }
			grp, worker := e2eGroup("candidate-history-"+tc.name), user("79280000708")
			waID, at := e2eID("CANDIDATE"), time.Now().Add(-10*time.Minute)
			path := filepath.Join(t.TempDir(), "candidate.jpg")
			if err := os.WriteFile(path, []byte("history-candidate-image"), 0600); err != nil {
				t.Fatal(err)
			}
			rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), worker.String(), worker.String(), "~M", tc.body, true, path, at)
			if err != nil {
				t.Fatal(err)
			}
			content := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(tc.caption)}}
			history := reliableHistoryBatch(grp, reliableHistoryMessage(grp, worker, waID, "~M", at, content))
			for i := 0; i < 2; i++ {
				_, receipts, payments := e.importHistorySync(history)
				if receipts != 0 || payments != 0 {
					t.Fatalf("other created financial rows: receipts=%d payments=%d", receipts, payments)
				}
			}
			wantPending := tc.name != "plain-picture"
			if got := e.db.NeedsMediaReread(ctx, rawID); got != wantPending {
				t.Fatalf("pending=%v, want %v", got, wantPending)
			}
			if r := e.db.TestReceiptByMessage(ctx, waID); r.Found {
				t.Fatalf("history created bank evidence from a candidate: %+v", r)
			}
			if len(e.sent()) != 0 {
				t.Fatalf("historical review sent a group message: %+v", e.sent())
			}
		})
	}
}
