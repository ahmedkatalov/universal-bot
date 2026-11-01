package db

import (
	"context"
	"testing"
	"time"
)

func provisionalReceiptFixture(t *testing.T, d *DB) (int, int) {
	t.Helper()
	ctx := context.Background()
	group, sender := uniqueScope("provisional")
	group += "@g.us"
	at := time.Now().Truncate(time.Second)
	rawID, _, err := d.SaveRawMessage(ctx, group+"-source", group, sender, "", "Мансур", "Клиент", true, "/tmp/provisional-receipt.jpg", at)
	if err != nil {
		t.Fatal(err)
	}
	contactID, err := d.GetOrCreateContact(ctx, "Клиент "+group)
	if err != nil {
		t.Fatal(err)
	}
	var receiptID int
	if err := d.pool.QueryRow(ctx, `INSERT INTO bank_receipts(raw_message_id,group_jid,contact_id,recipient_raw,amount,needs_review,client_confirmed,tx_date)
	 VALUES($1,$2,$3,'Клиент',10000,true,true,$4) RETURNING id`, rawID, group, contactID, at).Scan(&receiptID); err != nil {
		t.Fatal(err)
	}
	return rawID, receiptID
}

func TestReceiptRetryReplacesProvisionalOCRWithConsensus(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, scenario := range []struct {
		name, kind string
		reverted   bool
		wantAmount float64
	}{
		{name: "unverified_OCR", wantAmount: 15000},
		{name: "active_manual_override", kind: "receipt", wantAmount: 13000},
		{name: "reverted_override", kind: "receipt", reverted: true, wantAmount: 15000},
		{name: "different_kind_same_id", kind: "transaction", wantAmount: 15000},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rawID, receiptID := provisionalReceiptFixture(t, d)
			if err := d.MarkReceiptForRetry(ctx, rawID, "vision verdict unavailable", time.Now()); err != nil {
				t.Fatal(err)
			}
			var provisional bool
			if err := d.pool.QueryRow(ctx, "SELECT recognition_provisional FROM bank_receipts WHERE id=$1", receiptID).Scan(&provisional); err != nil {
				t.Fatal(err)
			}
			if !provisional {
				t.Fatal("unverified OCR was not marked provisional")
			}
			if scenario.kind != "" {
				if _, err := d.pool.Exec(ctx, "UPDATE bank_receipts SET amount=13000 WHERE id=$1", receiptID); err != nil {
					t.Fatal(err)
				}
				if _, err := d.pool.Exec(ctx, `INSERT INTO amount_corrections(raw_message_id,kind,target_id,old_amount,new_amount,reverted)
				 VALUES($1,$2,$3,10000,13000,$4)`, rawID, scenario.kind, receiptID, scenario.reverted); err != nil {
					t.Fatal(err)
				}
			}
			// The original marker text is overwritten on a failed retry, but the
			// provisional provenance must survive until actual recognition.
			if err := d.BumpReceiptRetry(ctx, receiptID, "model still unavailable", time.Now().Add(time.Minute), 6); err != nil {
				t.Fatal(err)
			}
			if err := d.pool.QueryRow(ctx, "SELECT recognition_provisional FROM bank_receipts WHERE id=$1", receiptID).Scan(&provisional); err != nil {
				t.Fatal(err)
			}
			if !provisional {
				t.Fatal("retry lost provisional provenance")
			}
			if err := d.FillReceiptFromRetry(ctx, receiptID, RecognizedReceipt{Bank: "Т-Банк", Recipient: "Банковский Получатель", Amount: 15000, DocNumber: "CONFIRMED"}); err != nil {
				t.Fatal(err)
			}
			var amount float64
			var status, client string
			var review bool
			var next *time.Time
			if err := d.pool.QueryRow(ctx, "SELECT amount::float8,recognition_status,recognition_provisional,needs_review,recipient_raw,next_retry_at FROM bank_receipts WHERE id=$1", receiptID).Scan(&amount, &status, &provisional, &review, &client, &next); err != nil {
				t.Fatal(err)
			}
			if amount != scenario.wantAmount || provisional || status != "ok" || review || client != "Клиент" || next != nil {
				t.Fatalf("unsafe recognition activation: amount=%.0f want=%.0f provisional=%v status=%s review=%v client=%s next=%v", amount, scenario.wantAmount, provisional, status, review, client, next)
			}
		})
	}
}

func TestReceiptRetryOrdinaryKnownAmountStaysKnown(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, receiptID := provisionalReceiptFixture(t, d)
	if err := d.MarkReceiptForRetry(ctx, rawID, "vision unavailable", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.FillReceiptFromRetry(ctx, receiptID, RecognizedReceipt{Bank: "Т-Банк", Amount: 15000}); err != nil {
		t.Fatal(err)
	}
	var amount float64
	var provisional bool
	if err := d.pool.QueryRow(ctx, "SELECT amount::float8,recognition_provisional FROM bank_receipts WHERE id=$1", receiptID).Scan(&amount, &provisional); err != nil {
		t.Fatal(err)
	}
	if amount != 10000 || provisional {
		t.Fatalf("ordinary known amount changed: amount=%.0f provisional=%v", amount, provisional)
	}
}
