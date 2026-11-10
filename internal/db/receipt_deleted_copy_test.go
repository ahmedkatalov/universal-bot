package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func deletedReceiptCopyFixture(t *testing.T, d *DB, copies int) (string, string, []string) {
	t.Helper()
	ctx := context.Background()
	group, sender := uniqueScope("deleted-receipt-copy")
	group += "@g.us"
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	client := "Ханатаев Акраман " + group
	cid, err := d.GetOrCreateContact(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for i := 0; i <= copies; i++ {
		wa := fmt.Sprintf("%s-copy-%d", group, i)
		raw, _, err := d.SaveRawMessage(ctx, wa, group, sender, "", "Сотрудник", client, true, "", at.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		doc := "A6304225636335050B10020011881102-" + group
		if i > 0 {
			doc = "А6304225636335050B10020011\n881102-" + group
		}
		if err := d.InsertBankReceipt(ctx, BankReceiptInput{RawMessageID: raw, GroupJID: group, ContactID: &cid, RecipientRaw: client,
			Amount: 30000, DocNumber: doc, ClientConfirmed: true, IsDuplicate: i > 0,
			TxDate: at.Add(time.Duration(i) * 3 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, wa)
	}
	return group, messages[0], messages[1:]
}

func TestDeletingReceiptPromotesNormalizedDocumentCopy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, copies := deletedReceiptCopyFixture(t, d, 1)
	if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
		t.Fatal(err)
	}
	var duplicate bool
	if err := d.pool.QueryRow(ctx, `SELECT br.is_duplicate FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id WHERE rm.wa_message_id=$1`, copies[0]).Scan(&duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("live copy remains suppressed after the normalized-document original was deleted")
	}
	from := time.Now().Add(-24 * time.Hour)
	rows, err := d.SummaryForPeriod(ctx, from, from.Add(48*time.Hour), []string{group})
	if err != nil || len(rows) != 1 || rows[0].Total != 30000 || rows[0].Count != 1 {
		t.Fatalf("surviving receipt disappeared or doubled: %+v / %v", rows, err)
	}
}

func TestRepairDeletedReceiptPromotesOneCopyAndIsIdempotent(t *testing.T) {
	for _, identity := range []string{"document", "explicit_link", "image"} {
		t.Run(identity, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, _ := deletedReceiptCopyFixture(t, d, 2)
			var rawID int
			if err := d.pool.QueryRow(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1 RETURNING id`, original).Scan(&rawID); err != nil {
				t.Fatal(err)
			}
			if identity != "document" {
				if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number=NULL WHERE group_jid=$1`, group); err != nil {
					t.Fatal(err)
				}
			}
			if identity == "explicit_link" {
				if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET superseded_by_raw=$2 WHERE group_jid=$1 AND is_duplicate`, group, rawID); err != nil {
					t.Fatal(err)
				}
			}
			if identity == "image" {
				if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET media_sha=$1 WHERE wa_group_jid=$1`, group); err != nil {
					t.Fatal(err)
				}
			}
			var wg sync.WaitGroup
			results := make(chan int, 2)
			errors := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour))
					results <- n
					errors <- err
				}()
			}
			wg.Wait()
			if first, second := <-errors, <-errors; first != nil || second != nil {
				t.Fatalf("repair failed: %v / %v", first, second)
			}
			if count := <-results + <-results; count != 1 {
				t.Fatalf("concurrent repairs promoted %d copies, want one", count)
			}
			if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
				t.Fatalf("repeat repair promoted another copy: %d / %v", n, err)
			}
			if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id
			 WHERE br.group_jid=$1 AND NOT br.is_duplicate AND NOT br.ignored AND NOT rm.deleted`, group).Scan(&count); err != nil || count != 1 {
				t.Fatalf("repeat revoke promoted another copy: %d / %v", count, err)
			}
		})
	}
}

func TestDeletingDuplicateKeepsOriginalAndDeletingBothRemovesPayment(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, copies := deletedReceiptCopyFixture(t, d, 1)
	if _, _, err := d.MarkMessageDeleted(ctx, copies[0]); err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(-24 * time.Hour)
	rows, err := d.SummaryForPeriod(ctx, from, from.Add(48*time.Hour), []string{group})
	if err != nil || len(rows) != 1 || rows[0].Total != 30000 {
		t.Fatalf("deleting only the duplicate removed the original: %+v / %v", rows, err)
	}
	if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
		t.Fatal(err)
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, from); err != nil || n != 0 {
		t.Fatalf("repair revived a deleted attachment: %d / %v", n, err)
	}
	rows, err = d.SummaryForPeriod(ctx, from, from.Add(48*time.Hour), []string{group})
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted receipts remained in collection: %+v / %v", rows, err)
	}
}

func TestRepairDeletedReceiptNeverInventsEvidenceOrOverridesConflict(t *testing.T) {
	for _, change := range []string{"different_document", "ignored", "different_client", "different_amount"} {
		t.Run(change, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copies := deletedReceiptCopyFixture(t, d, 1)
			if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1`, original); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "different_document":
				_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number='UNRELATED-30000-'||id WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0])
			case "ignored":
				_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET ignored=true WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0])
			case "different_client":
				var cid int
				cid, err = d.GetOrCreateContact(ctx, "Другой клиент "+group)
				if err == nil {
					_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET contact_id=$2 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0], cid)
				}
			case "different_amount":
				_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET amount=31000 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0])
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if change == "different_client" || change == "different_amount" {
				want = 1
			}
			if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != want {
				t.Fatalf("repair count=%d want=%d: %v", n, want, err)
			}
			if want == 1 {
				var review bool
				if err := d.pool.QueryRow(ctx, `SELECT needs_review FROM bank_receipts WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0]).Scan(&review); err != nil || !review {
					t.Fatalf("conflicting receipt was automatically verified: review=%v / %v", review, err)
				}
			}
		})
	}
}

func TestRepeatedReceiptRevokeWithoutIdentifiersPromotesOnlyOneCopy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, _ := deletedReceiptCopyFixture(t, d, 2)
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number=NULL,auth_code=NULL,
	 tx_date=(SELECT br.tx_date FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id WHERE rm.wa_message_id=$2)
	 WHERE group_jid=$1`, group, original); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id
	 WHERE br.group_jid=$1 AND NOT br.is_duplicate AND NOT br.ignored AND NOT rm.deleted`, group).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated deletion promoted %d copies, want one: %v", count, err)
	}
}

func TestRepairTwoDeletedSourcesOfSameDocumentPromotesOnlyOneCopy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, copies := deletedReceiptCopyFixture(t, d, 3)
	var firstRaw, secondRaw int
	if err := d.pool.QueryRow(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1 RETURNING id`, original).Scan(&firstRaw); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1 RETURNING id`, copies[0]).Scan(&secondRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET is_duplicate=false WHERE raw_message_id=$1`, secondRaw); err != nil {
		t.Fatal(err)
	}
	for i, source := range []int{firstRaw, secondRaw} {
		if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET superseded_by_raw=$2 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[i+1], source); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("same physical receipt restored multiple times: %d / %v", n, err)
	}
}

func TestLiveReceiptRevokeKeepsContradictoryCopyPending(t *testing.T) {
	for _, change := range []string{"client", "amount", "document"} {
		t.Run(change, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copies := deletedReceiptCopyFixture(t, d, 1)
			var err error
			switch change {
			case "client":
				var cid int
				cid, err = d.GetOrCreateContact(ctx, "Другой клиент "+group)
				if err == nil {
					_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET contact_id=$2 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0], cid)
				}
			case "amount":
				_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET amount=31000 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0])
			case "document":
				_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET auth_code='024307' WHERE group_jid=$1`, group)
				if err == nil {
					_, err = d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number='CONTRADICTORY-'||id WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0])
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
				t.Fatal(err)
			}
			var duplicate, review bool
			if err := d.pool.QueryRow(ctx, `SELECT is_duplicate,needs_review FROM bank_receipts WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0]).Scan(&duplicate, &review); err != nil || duplicate || !review {
				t.Fatalf("contradictory copy not pending: duplicate=%v review=%v / %v", duplicate, review, err)
			}
		})
	}
}

func TestDeletedReceiptReliableDayDisagreementStaysPending(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical=%v", historical), func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copies := deletedReceiptCopyFixture(t, d, 1)
			if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET tx_date=tx_date+interval '1 day'
			 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0]); err != nil {
				t.Fatal(err)
			}
			if historical {
				if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1`, original); err != nil {
					t.Fatal(err)
				}
				if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 1 {
					t.Fatalf("repair failed: %d / %v", n, err)
				}
			} else if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
				t.Fatal(err)
			}
			var review bool
			if err := d.pool.QueryRow(ctx, `SELECT needs_review FROM bank_receipts WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0]).Scan(&review); err != nil || !review {
				t.Fatalf("reliable operation-day conflict was counted: review=%v / %v", review, err)
			}
		})
	}
}

func TestDeletedReceiptShortDocumentIsNotStrongIdentity(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical=%v", historical), func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copies := deletedReceiptCopyFixture(t, d, 1)
			if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number='123' WHERE group_jid=$1`, group); err != nil {
				t.Fatal(err)
			}
			if historical {
				if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1`, original); err != nil {
					t.Fatal(err)
				}
				if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
					t.Fatalf("short OCR fragment promoted an unrelated copy: %d / %v", n, err)
				}
			} else if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
				t.Fatal(err)
			}
			var duplicate bool
			if err := d.pool.QueryRow(ctx, `SELECT is_duplicate FROM bank_receipts WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0]).Scan(&duplicate); err != nil || !duplicate {
				t.Fatalf("short document was treated as strong identity: duplicate=%v / %v", duplicate, err)
			}
		})
	}
}

func TestShortDocumentOnAnotherLiveReceiptDoesNotBlockExactLinkRepair(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, copies := deletedReceiptCopyFixture(t, d, 2)
	var source int
	if err := d.pool.QueryRow(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1 RETURNING id`, original).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number='123' WHERE group_jid=$1`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET superseded_by_raw=$2 WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[0], source); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET is_duplicate=false WHERE raw_message_id=(SELECT id FROM raw_messages WHERE wa_message_id=$1)`, copies[1]); err != nil {
		t.Fatal(err)
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("unrelated short document blocked exact-link receipt recovery: %d / %v", n, err)
	}
}

func TestRepairDeletedReceiptRestoresLegacyRawOnlySHA(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _, original, _, copy := imageDuplicateFixture(t, d)
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET amount=30000 WHERE group_jid=$1`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET body='СБЕРБАНК Чек по операции 30000 ₽',caption=NULL,parsed=true,
	 media_path='/tmp/legacy-sha-copy.jpg',sender_jid='79280008888@s.whatsapp.net',sender_phone='79280008888',sender_name='Другой сотрудник' WHERE id=$1`, copy); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET deleted=true WHERE wa_message_id=$1`, original); err != nil {
		t.Fatal(err)
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("legacy SHA shortcut left surviving raw-only receipt invisible: repaired=%d / %v", n, err)
	}
	from := time.Now().Add(-24 * time.Hour)
	report, err := d.EmployeePayments(ctx, from, from.Add(48*time.Hour), EmployeeReportOptions{Groups: []string{group}, Kind: "checks"})
	if err != nil || len(report.Rows) != 1 || report.Rows[0].Amount != 30000 || report.Rows[0].RawSenderName != "Другой сотрудник" || report.Rows[0].RawSenderPhone != "79280008888" {
		t.Fatalf("restored receipt changed amount or copied the deleted source's author: %+v / %v", report, err)
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, from); err != nil || n != 0 {
		t.Fatalf("legacy SHA repair is not idempotent: %d / %v", n, err)
	}
}

func TestLiveReceiptRevokeRecoversSHAOnlyCopy(t *testing.T) {
	for _, journaled := range []bool{false, true} {
		t.Run(fmt.Sprintf("journaled=%v", journaled), func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, _, originalWA, original, copy := imageDuplicateFixture(t, d)
			if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET parsed=true,body='СБЕРБАНК Чек по операции 15000 ₽',
			 sender_name='Другой сотрудник',sender_phone='79280008888' WHERE id=$1`, copy); err != nil {
				t.Fatal(err)
			}
			now := time.Now().In(time.Local)
			operation := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
			if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number=NULL,tx_date=$2 WHERE raw_message_id=$1`, original, operation); err != nil {
				t.Fatal(err)
			}
			if journaled {
				// Simulate a duplicate saved before it acquired an explicit link.
				// Different operation times on the same day leave SHA as its only match.
				if _, err := d.pool.Exec(ctx, `INSERT INTO bank_receipts(raw_message_id,group_jid,contact_id,recipient_raw,amount,tx_date,is_duplicate,client_confirmed)
				 SELECT $1,group_jid,contact_id,recipient_raw,amount,tx_date+interval '1 hour',true,true
				 FROM bank_receipts WHERE raw_message_id=$2`, copy, original); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if _, _, err := d.MarkMessageDeleted(ctx, originalWA); err != nil {
					t.Fatal(err)
				}
				from := time.Now().Add(-24 * time.Hour)
				report, err := d.EmployeePayments(ctx, from, from.Add(48*time.Hour), EmployeeReportOptions{Groups: []string{group}, Kind: "checks"})
				if err != nil || len(report.Rows) != 1 || report.Rows[0].Amount != 15000.25 || report.Rows[0].RawSenderPhone != "79280008888" {
					t.Fatalf("revoke %d lost the surviving payment or changed its sender: %+v / %v", i, report, err)
				}
			}
		})
	}
}

func TestRepeatedLiveReceiptRevokeKeepsOneAuthorizationCodeCopy(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, _ := deletedReceiptCopyFixture(t, d, 2)
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET doc_number=NULL,auth_code='024307' WHERE group_jid=$1`, group); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := d.MarkMessageDeleted(ctx, original); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id
	 WHERE br.group_jid=$1 AND NOT br.is_duplicate AND NOT br.ignored AND NOT rm.deleted`, group).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated revoke promoted %d authorization code copies: %v", count, err)
	}
}

func legacyDeletedRawCopyFixture(t *testing.T, d *DB) (string, int, int) {
	t.Helper()
	group, _, _, original, copy := imageDuplicateFixture(t, d)
	ctx := context.Background()
	if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET parsed=true WHERE id=$1`, copy); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET deleted=true WHERE id=$1`, original); err != nil {
		t.Fatal(err)
	}
	return group, original, copy
}

func TestRepairDeletedReceiptLegacyRawCopyRejectsIneligibleRows(t *testing.T) {
	for _, change := range []struct {
		name     string
		sql      string
		original bool
		existing int
	}{
		{name: "deleted_copy", sql: `UPDATE raw_messages SET deleted=true WHERE id=$1`},
		{name: "different_group", sql: `UPDATE raw_messages SET wa_group_jid=wa_group_jid||'-other@g.us' WHERE id=$1`},
		{name: "different_sha", sql: `UPDATE raw_messages SET media_sha='other-image-same-amount' WHERE id=$1`},
		{name: "empty_sha", sql: `UPDATE raw_messages SET media_sha='' WHERE id=$1`},
		{name: "not_processed", sql: `UPDATE raw_messages SET parsed=false WHERE id=$1`},
		{name: "not_media", sql: `UPDATE raw_messages SET has_media=false WHERE id=$1`},
		{name: "handled", sql: `UPDATE raw_messages SET handled_as='not_receipt' WHERE id=$1`},
		{name: "cash", sql: `UPDATE raw_messages SET cash_hint=true WHERE id=$1`},
		{name: "forward", sql: `UPDATE raw_messages SET sender_jid='bot-forward' WHERE id=$1`},
		{name: "old_copy", sql: `UPDATE raw_messages SET received_at=now()-interval '3 days' WHERE id=$1`},
		{name: "ignored_transaction", sql: `INSERT INTO transactions(raw_message_id,raw_name,amount,tx_date,ignored) VALUES($1,'Иванов Иван',30000,current_date,true)`},
		{name: "ignored_receipt", sql: `INSERT INTO bank_receipts(raw_message_id,amount,tx_date,ignored) VALUES($1,30000,now(),true)`, existing: 1},
		{name: "source_without_amount", sql: `UPDATE bank_receipts SET amount=0 WHERE raw_message_id=$1`, original: true},
		{name: "ignored_source", sql: `UPDATE bank_receipts SET ignored=true WHERE raw_message_id=$1`, original: true},
		{name: "duplicate_source", sql: `UPDATE bank_receipts SET is_duplicate=true WHERE raw_message_id=$1`, original: true},
		{name: "different_receipt_group", sql: `UPDATE bank_receipts SET group_jid=group_jid||'-other@g.us' WHERE raw_message_id=$1`, original: true},
	} {
		t.Run(change.name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copy := legacyDeletedRawCopyFixture(t, d)
			target := copy
			if change.original {
				target = original
			}
			if _, err := d.pool.Exec(ctx, change.sql, target); err != nil {
				t.Fatal(err)
			}
			if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
				t.Fatalf("ineligible legacy copy recovered: %d / %v", n, err)
			}
			var count int
			if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts WHERE raw_message_id=$1`, copy).Scan(&count); err != nil || count != change.existing {
				t.Fatalf("repair inserted a receipt despite existing evidence or exclusion: %d / %v", count, err)
			}
		})
	}
}

func TestRepairDeletedReceiptLegacyRawCopyPreservesUncertaintyAndAttribution(t *testing.T) {
	for _, change := range []struct {
		name, caption, body          string
		status                       string
		review, provisional, unnamed bool
		wantReview                   bool
	}{
		{name: "matching_caption", caption: "  Ханатаев   АКРАМАН "},
		{name: "different_caption", caption: "Иванов Иван", wantReview: true},
		{name: "different_caption_instruction", caption: "За другого клиента", wantReview: true},
		{name: "matching_body_name", body: "Ханатаев Акраман"},
		{name: "different_body_name", body: "Иванов Иван", wantReview: true},
		{name: "printed_body", body: "СБЕРБАНК Чек по операции 30000 ₽"},
		{name: "source_review", review: true, wantReview: true},
		{name: "source_provisional", provisional: true, wantReview: true},
		{name: "source_unconfirmed", unnamed: true, wantReview: true},
		{name: "source_pending_without_review", status: "pending", wantReview: true},
		{name: "source_failed_without_review", status: "failed", wantReview: true},
	} {
		t.Run(change.name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copy := legacyDeletedRawCopyFixture(t, d)
			if change.status == "" {
				change.status = "ok"
			}
			cid, err := d.GetOrCreateContact(ctx, "Ханатаев Акраман")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET contact_id=$2,recipient_raw='Ханатаев Акраман',
			 needs_review=$3,recognition_provisional=$4,client_confirmed=$5,submitted_by='Удалённый сотрудник',recognition_status=$6
			 WHERE raw_message_id=$1`, original, cid, change.review, change.provisional, !change.unnamed, change.status); err != nil {
				t.Fatal(err)
			}
			if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET caption=$2,body=$3 WHERE id=$1`, copy, change.caption, change.body); err != nil {
				t.Fatal(err)
			}
			if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 1 {
				t.Fatalf("legacy copy repair failed: %d / %v", n, err)
			}
			var review, provisional, confirmed, sameEvidence, sourceDeleted bool
			var submitted, status string
			if err := d.pool.QueryRow(ctx, `SELECT cp.needs_review,cp.recognition_provisional,cp.client_confirmed,
			 cp.amount=orig.amount AND cp.contact_id=orig.contact_id AND cp.tx_date=orig.tx_date,
			 COALESCE(cp.submitted_by,''),orm.deleted,cp.recognition_status
			 FROM bank_receipts cp JOIN bank_receipts orig ON orig.raw_message_id=$1
			 JOIN raw_messages orm ON orm.id=orig.raw_message_id WHERE cp.raw_message_id=$2`, original, copy).
				Scan(&review, &provisional, &confirmed, &sameEvidence, &submitted, &sourceDeleted, &status); err != nil {
				t.Fatal(err)
			}
			if review != change.wantReview || provisional != change.provisional || confirmed == change.unnamed || !sameEvidence || submitted != "" || !sourceDeleted || status != change.status {
				t.Fatalf("source evidence or live attribution changed: review=%v provisional=%v confirmed=%v same=%v submitted=%q sourceDeleted=%v status=%q", review, provisional, confirmed, sameEvidence, submitted, sourceDeleted, status)
			}
		})
	}
}

func TestRepairDeletedReceiptLegacyRawOnlyPromotesOneAcrossConcurrentRepairs(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, original, copy := legacyDeletedRawCopyFixture(t, d)
	second, _, err := d.SaveRawMessage(ctx, group+"-second-copy", group, "other-sender", "", "Другой сотрудник", "", true, "", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET parsed=true,media_sha=(SELECT media_sha FROM raw_messages WHERE id=$2) WHERE id=$1`, second, copy); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	errors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour))
			results <- n
			errors <- err
		}()
	}
	wg.Wait()
	if first, second := <-errors, <-errors; first != nil || second != nil {
		t.Fatalf("concurrent legacy recovery failed: %v / %v", first, second)
	}
	if count := <-results + <-results; count != 1 {
		t.Fatalf("concurrent legacy recovery promoted %d copies, want one", count)
	}
	var count int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts WHERE raw_message_id=ANY($1)`, []int{copy, second}).Scan(&count); err != nil || count != 1 {
		t.Fatalf("raw copies produced multiple receipt rows: %d / %v", count, err)
	}
	if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("repeat recovery added another raw copy: %d / %v", n, err)
	}
	if _, _, err := d.MarkMessageDeleted(ctx, group+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM raw_messages WHERE id=$1 AND deleted`, original).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deleted original was revived: %d / %v", count, err)
	}
}

func TestRepairDeletedReceiptLegacyRawCopyRespectsExistingLivePrimary(t *testing.T) {
	for _, identity := range []string{"document", "sha"} {
		t.Run(identity, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			group, original, copy := legacyDeletedRawCopyFixture(t, d)
			live, _, err := d.SaveRawMessage(ctx, group+"-live-primary", group, "live-sender", "", "Сотрудник", "", true, "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.pool.Exec(ctx, `INSERT INTO bank_receipts(raw_message_id,group_jid,contact_id,recipient_raw,amount,tx_date,client_confirmed,doc_number)
			 SELECT $1,group_jid,contact_id,recipient_raw,amount,tx_date,client_confirmed,
			 CASE WHEN $3='document' THEN doc_number ELSE 'different-document' END FROM bank_receipts WHERE raw_message_id=$2`, live, original, identity); err != nil {
				t.Fatal(err)
			}
			if identity == "sha" {
				if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET media_sha=(SELECT media_sha FROM raw_messages WHERE id=$2) WHERE id=$1`, live, copy); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := d.RepairReceiptDuplicatesAfterDeletion(ctx, group, time.Now().Add(-24*time.Hour)); err != nil || n != 0 {
				t.Fatalf("existing live primary was counted twice: %d / %v", n, err)
			}
			var count int
			if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts WHERE raw_message_id=$1`, copy).Scan(&count); err != nil || count != 0 {
				t.Fatalf("raw-only copy inserted despite existing primary: %d / %v", count, err)
			}
		})
	}
}
