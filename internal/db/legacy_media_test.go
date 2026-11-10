package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestLegacyParsedNamedPhotoWithoutReceiptCanBeReread(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	waID := fmt.Sprintf("legacy-sber-%d", time.Now().UnixNano())
	group := waID + "@g.us"
	at := time.Date(2026, time.November, 1, 14, 24, 0, 0, time.FixedZone("MSK", 3*60*60))
	rawID, _, err := d.SaveRawMessage(ctx, waID, group, "79280000003@s.whatsapp.net", "79280000003", "~M", "нечитаемый OCR", true, "/tmp/legacy-sber.jpg", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetMediaInfo(ctx, rawID, "Ханатаев акраман", []byte("saved-download-descriptor")); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkMessageParsed(ctx, rawID); err != nil {
		t.Fatal(err)
	}
	var oldPredicate bool
	if err := d.pool.QueryRow(ctx, `SELECT has_media AND NOT parsed AND NOT EXISTS
		(SELECT 1 FROM bank_receipts br WHERE br.raw_message_id=rm.id) FROM raw_messages rm WHERE id=$1`, rawID).Scan(&oldPredicate); err != nil || oldPredicate {
		t.Fatalf("old predicate did not reproduce the hidden-photo gap: %v / %v", oldPredicate, err)
	}
	items, err := d.GroupMediaToReread(ctx, group, at.Add(-time.Minute), 10)
	if err != nil || len(items) != 1 || items[0].RawID != rawID || !items[0].RecoverParsed {
		t.Fatalf("legacy parsed photo is invisible to explicit reread: %+v / %v", items, err)
	}
	if !d.NeedsMediaReread(ctx, rawID) {
		t.Fatal("repeated history still skips the saved named photo without a receipt")
	}
}

func TestLegacyParsedMediaRecoveryRespectsExistingDecisions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, state := range []string{"cash", "other", "answer", "review", "deleted", "cash-hint", "cash-transaction", "ignored-transaction", "counted-receipt", "ignored-receipt", "duplicate-receipt", "no-source", "plain-other"} {
		t.Run(state, func(t *testing.T) {
			waID := fmt.Sprintf("legacy-%s-%d", state, time.Now().UnixNano())
			group, at := waID+"@g.us", time.Now()
			path := "/tmp/legacy.jpg"
			if state == "no-source" {
				path = ""
			}
			body := "Ханатаев акраман"
			if state == "plain-other" {
				body = "Красивый закат"
			}
			raw, _, err := d.SaveRawMessage(ctx, waID, group, "79280000003@s.whatsapp.net", "", "~M", body, true, path, at)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.MarkMessageParsed(ctx, raw); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "cash", "other", "answer", "review":
				err = d.SetHandledAs(ctx, raw, state)
			case "cash-hint":
				_, err = d.pool.Exec(ctx, "UPDATE raw_messages SET cash_hint=true WHERE id=$1", raw)
			case "deleted":
				_, _, err = d.MarkMessageDeleted(ctx, waID)
			case "cash-transaction", "ignored-transaction":
				var contactID int
				contactID, err = d.GetOrCreateContact(ctx, body)
				if err == nil {
					err = d.InsertTransaction(ctx, TransactionInput{ContactID: contactID, RawMessageID: raw, RawName: body, Amount: 15000, IsCash: true, TxDate: at})
				}
				if err == nil && state == "ignored-transaction" {
					_, err = d.pool.Exec(ctx, "UPDATE transactions SET ignored=true WHERE raw_message_id=$1", raw)
				}
			case "counted-receipt", "ignored-receipt", "duplicate-receipt":
				err = d.InsertBankReceipt(ctx, BankReceiptInput{RawMessageID: raw, GroupJID: group, Amount: 30000, RecipientRaw: body, ClientConfirmed: true, TxDate: at, Ignored: state == "ignored-receipt", IsDuplicate: state == "duplicate-receipt"})
			}
			if err != nil {
				t.Fatal(err)
			}
			items, err := d.GroupMediaToReread(ctx, group, at.Add(-time.Minute), 10)
			if err != nil || len(items) != 0 || d.NeedsMediaReread(ctx, raw) {
				t.Fatalf("existing %s decision reopened: %+v / %v", state, items, err)
			}
		})
	}
}

func TestLegacyParsedMediaClientMatchAndEligibilityPrecedeLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group := fmt.Sprintf("legacy-limit-%d@g.us", time.Now().UnixNano())
	at := time.Now().Add(-time.Hour)
	add := func(label, body string, offset int) int {
		t.Helper()
		raw, _, err := d.SaveRawMessage(ctx, fmt.Sprintf("%s-%s-%d", group, label, offset), group, "79280000003@s.whatsapp.net", "", "~M", body, true, "/tmp/legacy.jpg", at.Add(time.Duration(offset)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.MarkMessageParsed(ctx, raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for i := 0; i < 50; i++ {
		add("plain", "Красивый закат", i)
	}
	target := add("target", "Ханатаев акраман", 100)
	items, err := d.GroupMediaToReread(ctx, group, at.Add(-time.Minute), 1)
	if err != nil || len(items) != 1 || items[0].RawID != target {
		t.Fatalf("SQL limit hid the first eligible candidate: %+v / %v", items, err)
	}
	for i := 0; i < 50; i++ {
		add("another-client", "Исаев Харон", i)
	}
	items, err = d.GroupMediaToRereadForClient(ctx, group, at.Add(-time.Minute), "акраман Ханатаев", 1)
	if err != nil || len(items) != 1 || items[0].RawID != target {
		t.Fatalf("batch limit hid a later requested client: %+v / %v", items, err)
	}
}
