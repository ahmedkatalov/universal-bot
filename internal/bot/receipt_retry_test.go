package bot

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"whatsapp-bot/internal/db"
)

// TestRetryBackoff — пауза между попытками растёт и упирается в потолок.
func TestRetryBackoff(t *testing.T) {
	if got := retryBackoff(0); got != 2*time.Minute {
		t.Errorf("retryBackoff(0) = %v, ожидали 2m", got)
	}
	if got := retryBackoff(4); got != 30*time.Minute {
		t.Errorf("retryBackoff(4) = %v, ожидали 30m", got)
	}
	if got := retryBackoff(100); got != 30*time.Minute {
		t.Errorf("retryBackoff(100) = %v, ожидали 30m (потолок)", got)
	}
	prev := time.Duration(0)
	for i := 0; i < 6; i++ {
		if got := retryBackoff(i); got < prev {
			t.Errorf("retryBackoff не должен уменьшаться: %v < %v на i=%d", got, prev, i)
		} else {
			prev = got
		}
	}
}

func TestQueuedMediaRetrySkipsChangedSource(t *testing.T) {
	t.Setenv("RECEIPT_VISION_FIRST", "1")
	for _, change := range []string{"receipt_saved", "cash_saved", "caption_edited", "deleted"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			e := newE2E(t, e2eOpts{WithAI: true})
			var reads atomic.Int32
			e.ai = func(aiCall) aiReply {
				reads.Add(1)
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Тестовый получатель","amount":30000,"doc_number":"1000000006086701079"}`}
			}
			group, worker := e2eGroup("stale-media-"+change), user("79280000771")
			path := filepath.Join(t.TempDir(), "receipt.jpg")
			if err := os.WriteFile(path, []byte("queued test image"), 0600); err != nil {
				t.Fatal(err)
			}
			at, waID := time.Now().Add(-time.Hour), e2eID("STALE-MEDIA")
			rawID, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), worker.String(), worker.String(), "Мансур", "", true, path, at)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.db.SetMediaInfo(ctx, rawID, "Ханатаев Акраман", nil); err != nil {
				t.Fatal(err)
			}
			items, err := e.db.DueMediaRetries(ctx, 5)
			if err != nil || len(items) != 1 || items[0].RawID != rawID {
				t.Fatalf("attachment not queued: %+v / %v", items, err)
			}
			switch change {
			case "receipt_saved":
				err = e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: rawID, GroupJID: group.String(), Amount: 30000, TxDate: at})
			case "cash_saved":
				err = e.db.InsertTransaction(ctx, db.TransactionInput{RawMessageID: rawID, RawName: "Ханатаев Акраман", Amount: 30000, IsCash: true, TxDate: at})
			case "caption_edited":
				err = e.db.SetMediaInfo(ctx, rawID, "Исаев Харон", nil)
			case "deleted":
				_, _, err = e.db.MarkMessageDeleted(ctx, waID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if e.rereadCurrentMedia(ctx, items[0], true) || e.rereadMedia(ctx, items[0]) || reads.Load() != 0 {
				t.Fatalf("obsolete queued attachment was processed again (%s, model calls %d)", change, reads.Load())
			}
		})
	}
}
