package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReceiptRetryRejectsStaleSourceAndReceiptState(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, change := range []string{"unchanged", "edited", "revision_only", "resolved", "deleted"} {
		t.Run(change, func(t *testing.T) {
			waID := fmt.Sprintf("retry-source-%s-%d", change, time.Now().UnixNano())
			rawID, _, err := d.SaveRawMessage(ctx, waID, waID+"@g.us", "worker@s.whatsapp.net", "", "Работник", "Ханатаев Акраман", true, "/tmp/missing.jpg", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := d.InsertBankReceipt(ctx, BankReceiptInput{RawMessageID: rawID, Amount: 0, NeedsReview: true, GroupJID: waID + "@g.us", TxDate: time.Now()}); err != nil {
				t.Fatal(err)
			}
			rid, _, ok, err := d.ReceiptOfRaw(ctx, rawID)
			if err != nil || !ok {
				t.Fatalf("receipt fixture missing: %v / %v", ok, err)
			}
			if err := d.MarkReceiptForRetry(ctx, rawID, "vision unavailable", time.Now()); err != nil {
				t.Fatal(err)
			}
			it, err := d.ReceiptRetryByID(ctx, rid)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "edited":
				err = d.SetMessageBody(ctx, rawID, "изменённый источник")
			case "revision_only":
				_, err = d.pool.Exec(ctx, "UPDATE raw_messages SET edit_count=edit_count+1 WHERE id=$1", rawID)
			case "resolved":
				err = d.ResolveReceiptRetry(ctx, rid, "ok", false)
			case "deleted":
				_, _, err = d.MarkMessageDeleted(ctx, waID)
			}
			if err != nil {
				t.Fatal(err)
			}
			applied, err := d.FillReceiptFromRetryIfCurrent(ctx, it, RecognizedReceipt{Amount: 30000, Bank: "Сбербанк", DocNumber: "1000000006086701079"})
			if err != nil || applied != (change == "unchanged") {
				t.Fatalf("stale result applied=%v for %s: %v", applied, change, err)
			}
			if change == "unchanged" {
				return
			}
			if err := d.BumpReceiptRetryIfCurrent(ctx, it, "late network failure", time.Now(), 6); err != nil {
				t.Fatal(err)
			}
			var amount float64
			var attempts int
			var state string
			if err := d.pool.QueryRow(ctx, `SELECT amount::float8,recognition_attempts,recognition_status FROM bank_receipts WHERE id=$1`, rid).Scan(&amount, &attempts, &state); err != nil {
				t.Fatal(err)
			}
			if amount != 0 || attempts != 0 || (change == "resolved" && state != "ok") {
				t.Fatalf("stale worker overwrote current state: amount=%v attempts=%d state=%s", amount, attempts, state)
			}
		})
	}
}
