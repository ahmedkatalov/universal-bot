package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFindClientReceiptEvidenceSavedSberPhotoAndAllReadingStates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("client-receipt-evidence")
	other := group + "-other"
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := day.AddDate(0, 0, 1)
	client := "Ханатаев Акраман"
	addRaw := func(label, grp, body, caption string, at time.Time, media bool) int {
		t.Helper()
		raw, _, err := d.SaveRawMessage(ctx, group+"-"+label, grp, "79280000003@s.whatsapp.net", "79280000003", "~ M", body, media, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if caption != "" {
			if _, err := d.pool.Exec(ctx, "UPDATE raw_messages SET caption=$2 WHERE id=$1", raw, caption); err != nil {
				t.Fatal(err)
			}
		}
		return raw
	}
	// A phone photo was saved at 14:24, but no bank_receipts row was produced.
	photo := addRaw("sber-photo", group, "СберБанк Онлайн\n30000 ₽\nЭлина Зилаудиевна Г.\nМилана Нажудовна К.", "ХАНАТАЕВ АКРАМАН", day.Add(14*time.Hour+24*time.Minute), true)
	// Reading tags, long bodies, and deleted messages remain source evidence.
	long := addRaw("long-body", group, strings.Repeat("Банковский текст ", 3000)+"\nАкраман, Ханатаев", "", day.Add(15*time.Hour), true)
	if _, err := d.pool.Exec(ctx, "UPDATE raw_messages SET parsed=true,handled_as='assistant',ai_disputed=true WHERE id=$1", long); err != nil {
		t.Fatal(err)
	}
	deleted := addRaw("deleted", group, client, "", day.Add(16*time.Hour), true)
	if _, err := d.pool.Exec(ctx, "UPDATE raw_messages SET deleted=true WHERE id=$1", deleted); err != nil {
		t.Fatal(err)
	}
	text := addRaw("text", group, client+" 30т", "", day.Add(17*time.Hour), false)
	if _, err := d.pool.Exec(ctx, "UPDATE raw_messages SET parsed=true,handled_as='answer' WHERE id=$1", text); err != nil {
		t.Fatal(err)
	}
	addReceipt := func(label string, confirmed, review, ignored bool, named, payer string) int {
		t.Helper()
		raw := addRaw(label, group, "OCR без подписи", "", day.Add(18*time.Hour), true)
		if err := d.InsertBankReceipt(ctx, BankReceiptInput{RawMessageID: raw, GroupJID: group, Bank: "Сбер", RecipientRaw: named, SenderRaw: payer, ClientConfirmed: confirmed, NeedsReview: review, Ignored: ignored, Amount: 30000, TxDate: day}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	confirmed := addReceipt("confirmed-lost-caption", true, false, false, client, "Элина Зилаудиевна Г.")
	ignored := addReceipt("ignored-lost-caption", true, false, true, client, "")
	printedOnly := addReceipt("unconfirmed-bank-recipient", false, true, false, client, "")
	payerOnly := addReceipt("bank-payer", true, false, false, "Другой Клиент", client)
	pending := addReceipt("pending", false, true, false, "Милана Нажудовна К.", "")
	if _, err := d.pool.Exec(ctx, "UPDATE raw_messages SET caption=$2 WHERE id=$1", pending, client); err != nil {
		t.Fatal(err)
	}
	// No name-based SQL limit may prevent a late match in the selected day.
	if _, err := d.pool.Exec(ctx, `INSERT INTO raw_messages(wa_message_id,wa_group_jid,sender_jid,body,received_at)
		SELECT $1||'-noise-'||n,$1,'79280000003@s.whatsapp.net','Другой Клиент',$2::timestamptz
		FROM generate_series(1,250) n`, group, day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	addRaw("before", group, client, "", day.Add(-time.Second), true)
	addRaw("end", group, client, "", to, true)
	addRaw("other-group", other, client, "", day.Add(time.Hour), true)
	got, err := d.FindClientReceiptEvidence(ctx, day, to, []string{group}, "акраман Ханатаев")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]struct{ status, matched string }{
		photo: {"unread", "caption"}, long: {"review", "body"}, deleted: {"excluded", "body"}, text: {"review", "body"},
		confirmed: {"parsed", "client"}, ignored: {"excluded", "client"}, pending: {"review", "caption"},
	}
	if len(got) != len(want) {
		t.Fatalf("saved evidence hidden or bank payer/recipient misidentified: got %d want %d: %+v; bank-only IDs=%d,%d", len(got), len(want), got, printedOnly, payerOnly)
	}
	for i, e := range got {
		w, ok := want[e.RawID]
		if !ok || e.Status != w.status || e.MatchedBy != w.matched || e.Sender != "~ M" || e.GroupJID != group || e.WaID == "" {
			t.Errorf("wrong source status/identity: %+v, want %+v", e, w)
		}
		if i > 0 && e.PostedAt.Before(got[i-1].PostedAt) {
			t.Errorf("sources are not in posting order: %+v", got)
		}
		if e.RawID == photo && (e.HasReceipt || e.Parsed || !e.HasMedia || e.Deleted) {
			t.Errorf("unread saved photo became parsed/absent: %+v", e)
		}
	}
	// Empty groups means all groups, not an SQL NULL filter. Blank names do not
	// match every message. Complete words prevent short or mixed-name guesses.
	all, err := d.FindClientReceiptEvidence(ctx, day, to, nil, client)
	if err != nil || len(all) < len(got)+1 {
		t.Fatalf("empty group filter hid sources: %+v / %v", all, err)
	}
	for _, query := range []string{"", "...", "Хан", "Ханатаев Ислам"} {
		rows, err := d.FindClientReceiptEvidence(ctx, day, to, []string{group}, query)
		if err != nil || len(rows) != 0 {
			t.Errorf("query %q matched unrelated evidence: %+v / %v", query, rows, err)
		}
	}
	var parsed, removed bool
	var receiptCount int
	if err := d.pool.QueryRow(ctx, "SELECT parsed,deleted,(SELECT COUNT(*) FROM bank_receipts WHERE raw_message_id=$1) FROM raw_messages WHERE id=$1", photo).Scan(&parsed, &removed, &receiptCount); err != nil {
		t.Fatal(err)
	}
	if parsed || removed || receiptCount != 0 {
		t.Fatalf("read-only evidence lookup changed photo: parsed=%v deleted=%v receipts=%d", parsed, removed, receiptCount)
	}
}
