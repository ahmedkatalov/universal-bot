package bot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func legacyParsedSberSource(t *testing.T, e *e2eBot, group, worker, path, waID string) (int, time.Time) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, time.November, 1, 14, 24, 0, 0, time.FixedZone("MSK", 3*60*60))
	raw, _, err := e.db.SaveRawMessage(ctx, waID, group, worker, worker, "~M", "нечитаемый OCR", true, path, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetMediaInfo(ctx, raw, "Ханатаев акраман", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
		t.Fatal(err)
	}
	return raw, at
}

func TestLegacyParsedPhotoRecoveryOnlySavesVerifiedReceipt(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	for _, kind := range []string{"receipt", "cash", "other"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			e := newE2E(t, e2eOpts{WithAI: true})
			e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
			e.ai = func(aiCall) aiReply {
				if kind == "receipt" {
					return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Милана Нажудовна К.","sender":"Элина Зилаудиевна Г.","amount":30000,"doc_number":"1000000006086701079","auth_code":"024307","datetime":"2026-11-01 14:23:06"}`}
				}
				if kind == "cash" {
					return aiReply{Text: `{"kind":"cash","amount":30000}`}
				}
				return aiReply{Text: `{"kind":"other"}`}
			}
			grp, worker := e2eGroup("legacy-cash-safe-"+kind), user("79280000709")
			path, waID := filepath.Join(t.TempDir(), "legacy.jpg"), e2eID("LEGACY")
			if err := os.WriteFile(path, []byte("legacy-photo-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			raw, at := legacyParsedSberSource(t, e, grp.String(), worker.String(), path, waID)
			items, err := e.db.GroupMediaToRereadForClient(ctx, grp.String(), at.Add(-time.Minute), "Ханатаев Акраман", 1)
			if err != nil || len(items) != 1 || !items[0].RecoverParsed {
				t.Fatalf("legacy source not selected: %+v / %v", items, err)
			}
			if saved := e.rereadMedia(ctx, items[0]); saved != (kind == "receipt") {
				t.Fatalf("saved=%v for %s", saved, kind)
			}
			txs, err := e.db.TxIDsByRawID(ctx, raw)
			if err != nil || len(txs) != 0 {
				t.Fatalf("legacy recovery replayed cash effects: %+v / %v", txs, err)
			}
			r := e.db.TestReceiptByMessage(ctx, waID)
			if kind == "receipt" {
				if !r.Found || r.Amount != 30000 || r.Recipient != "Ханатаев Акраман" || r.NeedsReview || !r.Confirmed {
					t.Fatalf("verified receipt lost saved caption or amount: %+v", r)
				}
			} else if r.Found {
				t.Fatalf("nonreceipt became bank evidence: %+v", r)
			}
			if kind == "other" && !e.db.NeedsMediaReread(ctx, raw) {
				t.Fatal("other silently reclosed the named legacy candidate")
			}
			if kind == "cash" && e.db.NeedsMediaReread(ctx, raw) {
				t.Fatal("known cash photo remained a legacy receipt candidate")
			}
			if len(e.sent()) != 0 {
				t.Fatalf("legacy recovery sent a group message: %+v", e.sent())
			}
		})
	}
}

func TestHistoryReliabilityLegacyParsedNamedPhotoCanRecover(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
	e.ai = func(aiCall) aiReply {
		return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Милана Нажудовна К.","sender":"Элина Зилаудиевна Г.","amount":30000,"doc_number":"1000000006086701079","auth_code":"024307","datetime":"2026-11-01 14:23:06"}`}
	}
	grp, worker := e2eGroup("legacy-history-repair"), user("79280000710")
	path, waID := filepath.Join(t.TempDir(), "legacy.jpg"), e2eID("LEGACY-HISTORY")
	if err := os.WriteFile(path, []byte("legacy-photo-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	_, at := legacyParsedSberSource(t, e, grp.String(), worker.String(), path, waID)
	content := &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("Ханатаев акраман")}}
	history := reliableHistoryBatch(grp, reliableHistoryMessage(grp, worker, waID, "~M", at, content))
	_, receipts, payments := e.importHistorySync(history)
	if receipts != 1 || payments != 0 {
		t.Fatalf("repeated history skipped legacy source: receipts=%d payments=%d", receipts, payments)
	}
	_, receipts, _ = e.importHistorySync(history)
	if receipts != 0 {
		t.Fatal("already recovered receipt was recognized a second time")
	}
	details, err := e.db.ReceiptDetailsForPerson(ctx, "Ханатаев", 10)
	if err != nil || len(details) != 1 || details[0].Amount != 30000 || details[0].DocNumber != "1000000006086701079" {
		t.Fatalf("wrong recovered receipt: %+v / %v", details, err)
	}
}
