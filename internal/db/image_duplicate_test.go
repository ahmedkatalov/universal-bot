package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func imageDuplicateFixture(t *testing.T, d *DB) (string, string, string, int, int) {
	t.Helper()
	ctx := context.Background()
	group, sender := uniqueScope("image-duplicate")
	group += "@g.us"
	originalWA := group + "-original"
	at := time.Now()
	client, err := d.GetOrCreateContact(ctx, "Клиент "+group)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := d.SaveRawMessage(ctx, originalWA, group, sender, "", "Мансур", "Клиент", true, "", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InsertBankReceipt(ctx, BankReceiptInput{RawMessageID: original, GroupJID: group, ContactID: &client, RecipientRaw: "Клиент " + group, Amount: 15000.25, ClientConfirmed: true, TxDate: at, DocNumber: "IMAGE-DOC-" + group}); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkMessageParsed(ctx, original); err != nil {
		t.Fatal(err)
	}
	duplicate, _, err := d.SaveRawMessage(ctx, group+"-duplicate", group, sender, "", "Мансур", "", true, "", at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(group))
	sha := hex.EncodeToString(hash[:])
	for _, id := range []int{original, duplicate} {
		if err := d.SetMediaSha(ctx, id, sha); err != nil {
			t.Fatal(err)
		}
	}
	return group, sender, originalWA, original, duplicate
}

func assertImageDuplicateState(t *testing.T, d *DB, rawID, wantRows int, wantParsed bool) {
	t.Helper()
	var rows int
	var parsed bool
	ctx := context.Background()
	if err := d.pool.QueryRow(ctx, "SELECT count(*) FROM bank_receipts WHERE raw_message_id=$1", rawID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, "SELECT parsed FROM raw_messages WHERE id=$1", rawID).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if rows != wantRows || parsed != wantParsed {
		t.Fatalf("image duplicate journal inconsistent: rows=%d parsed=%v, want rows=%d parsed=%v", rows, parsed, wantRows, wantParsed)
	}
}

func TestImageDuplicateJournalIsAtomicAndIdempotent(t *testing.T) {
	d := testDB(t)
	group, _, wa, _, duplicate := imageDuplicateFixture(t, d)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if saved, err := d.SaveImageDuplicate(ctx, duplicate, wa, group); err != nil || !saved {
			t.Fatalf("save duplicate attempt %d: saved=%v err=%v", i, saved, err)
		}
	}
	assertImageDuplicateState(t, d, duplicate, 1, true)
	var isDuplicate bool
	var amount float64
	if err := d.pool.QueryRow(ctx, "SELECT is_duplicate,amount::float8 FROM bank_receipts WHERE raw_message_id=$1", duplicate).Scan(&isDuplicate, &amount); err != nil {
		t.Fatal(err)
	}
	if !isDuplicate || amount != 15000.25 {
		t.Fatalf("original evidence was changed by duplicate journal: duplicate=%v amount=%v", isDuplicate, amount)
	}
	from := time.Now().Add(-time.Hour)
	summary, err := d.SummaryForPeriod(ctx, from, from.Add(2*time.Hour), []string{group})
	if err != nil || len(summary) != 1 || summary[0].Count != 1 || summary[0].Total != 15000.25 {
		t.Fatalf("journal duplicated collection: %+v err=%v", summary, err)
	}
}

func TestImageDuplicateRejectsUnrelatedSource(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _, wa, _, duplicate := imageDuplicateFixture(t, d)
	for _, source := range []struct{ wa, group string }{{wa + "missing", group}, {wa, group + "other"}} {
		if saved, err := d.SaveImageDuplicate(ctx, duplicate, source.wa, source.group); err != nil || saved {
			t.Fatalf("unrelated source accepted: %+v saved=%v err=%v", source, saved, err)
		}
	}
	if err := d.SetMediaSha(ctx, duplicate, "different-image"); err != nil {
		t.Fatal(err)
	}
	if saved, err := d.SaveImageDuplicate(ctx, duplicate, wa, group); err != nil || saved {
		t.Fatalf("different image accepted: saved=%v err=%v", saved, err)
	}
	assertImageDuplicateState(t, d, duplicate, 0, false)
}

func TestImageDuplicateDoesNotAcknowledgeSuppressedInsert(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _, wa, _, duplicate := imageDuplicateFixture(t, d)
	name := fmt.Sprintf("test_image_insert_%d", duplicate)
	_, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
	 BEGIN IF NEW.raw_message_id=%d THEN RETURN NULL; END IF; RETURN NEW; END $$;
	 CREATE TRIGGER %s BEFORE INSERT ON bank_receipts FOR EACH ROW EXECUTE FUNCTION %s()`, name, duplicate, name, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.pool.Exec(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON bank_receipts; DROP FUNCTION IF EXISTS %s()", name, name)); err != nil {
			t.Errorf("cleanup trigger: %v", err)
		}
	})
	if saved, err := d.SaveImageDuplicate(ctx, duplicate, wa, group); err != nil || saved {
		t.Fatalf("uncommitted duplicate was acknowledged: saved=%v err=%v", saved, err)
	}
	assertImageDuplicateState(t, d, duplicate, 0, false)
}
