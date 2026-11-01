package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestGroupReadingCountersDoNotTreatUnknownMediaAsReceipt(t *testing.T) {
	var s GroupReadingStatus
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.add(groupReadingMessage{RawID: 1, ReceivedAt: at, HasMedia: true}, 1)
	s.add(groupReadingMessage{RawID: 2, ReceivedAt: at.Add(time.Minute), Parsed: true, HasMedia: true, ReceiptRows: 1, ReviewReceipts: 1, UnknownAmounts: 1}, 1)
	s.add(groupReadingMessage{RawID: 3, ReceivedAt: at.Add(2 * time.Minute), Parsed: true}, 1)
	s.add(groupReadingMessage{RawID: 4, ReceivedAt: at.Add(3 * time.Minute), Deleted: true, HasMedia: true, ReviewReceipts: 1, UnknownAmounts: 1}, 1)
	if s.SavedMessages != 4 || s.DeletedMessages != 1 || s.SettledMessages != 1 || s.UnresolvedMessages != 2 || s.UnreadMedia != 1 || s.ReviewReceipts != 1 || s.UnknownReceiptAmounts != 1 {
		t.Fatalf("wrong coverage: %+v", s)
	}
	if !s.Truncated || len(s.Issues) != 1 || !strings.Contains(strings.Join(s.Issues[0].Reasons, " "), "неизвестно, является ли оно чеком") {
		t.Fatalf("unknown photo claimed to be receipt or limit lost: %+v", s)
	}
	if !s.FirstSavedAt.Equal(at) || !s.LastSavedAt.Equal(at.Add(3*time.Minute)) {
		t.Fatal("bad saved window")
	}
}

func TestGroupReadingSnapshotDoesNotLoseEventDuringRawCapture(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, sender := uniqueScope("reading-snapshot")
	group += "@g.us"
	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)
	waID := group + "-media"
	_, _, err := d.CaptureGroupEvent(ctx, GroupInboxEvent{Key: waID, WaID: waID, GroupJID: group, Kind: "message", Metadata: []byte(`{}`), Payload: []byte("test"), ReceivedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	reading, err := groupReading(ctx, tx, from, to, []string{group}, 30)
	if err != nil || reading.SavedMessages != 0 {
		t.Fatalf("initial snapshot: %+v / %v", reading, err)
	}
	// Worker commits a raw row while the status snapshot is being read.
	_, _, err = d.SaveRawMessage(ctx, waID, group, sender, "", "Мансур", "", true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := groupInboxStatus(ctx, tx, from, to, []string{group}, 30)
	if err != nil || inbox.Pending != 1 {
		t.Fatalf("event vanished between snapshots: %+v / %v", inbox, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	reading, inbox, err = d.GroupReadingSnapshot(ctx, from, to, []string{group}, 30)
	if err != nil || reading.UnreadMedia != 1 || inbox.Pending != 0 {
		t.Fatalf("new snapshot must show raw once: %+v / %+v / %v", reading, inbox, err)
	}
}

func TestGroupReadingPartialTextRemainsUnresolved(t *testing.T) {
	var s GroupReadingStatus
	s.add(groupReadingMessage{RawID: 1, Body: "первый клиент 1000\nвторой клиент 2000", Disputed: true}, 30)
	if s.UnresolvedMessages != 1 || s.UnreadTexts != 1 || s.DisputedMessages != 1 || len(s.Issues) != 1 {
		t.Fatalf("multiple reasons must count one message: %+v", s)
	}
	s.add(groupReadingMessage{RawID: 2, Body: "Джарвис покажи отчёт", Handled: "command"}, 30)
	if s.UnresolvedMessages != 1 {
		t.Fatal("handled command incorrectly requires financial parsing")
	}
}

func TestGroupReadingDatabaseScopeAndUnknownAmounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, sender := uniqueScope("reading")
	group += "@g.us"
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	to := base.Add(10 * time.Minute)
	add := func(n int, media bool, body string) int {
		id, _, err := d.SaveRawMessage(ctx, group+"-"+string(rune('a'+n)), group, sender, "", "Мансур", body, media, "", base.Add(time.Duration(n)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	settled := add(0, false, "Понял")
	must(d.MarkMessageParsed(ctx, settled))
	deleted := add(1, true, "")
	_, err := d.pool.Exec(ctx, "UPDATE raw_messages SET deleted=true WHERE id=$1", deleted)
	must(err)
	add(2, true, "") // no receipt row: unknown attachment, not a zero-valued receipt
	partial := add(3, false, "Клиент Один 1000\nКлиент Два 2000")
	_, err = d.pool.Exec(ctx, "INSERT INTO transactions(raw_name,amount,raw_message_id,tx_date) VALUES('Клиент Один',1000,$1,$2)", partial, base)
	must(err)
	disputed := add(4, false, "спорный платёж")
	_, err = d.pool.Exec(ctx, "UPDATE raw_messages SET parsed=true,ai_disputed=true WHERE id=$1", disputed)
	must(err)
	zero := add(5, true, "")
	must(d.MarkMessageParsed(ctx, zero))
	_, err = d.pool.Exec(ctx, "INSERT INTO bank_receipts(raw_message_id,amount,tx_date,group_jid,needs_review,recognition_status) VALUES($1,0,$2,$3,true,'failed')", zero, base, group)
	must(err)
	held := add(6, false, "Нал 5000")
	must(d.MarkMessageParsed(ctx, held))
	_, err = d.pool.Exec(ctx, "INSERT INTO transactions(raw_name,amount,raw_message_id,tx_date,dup_pending,is_cash) VALUES('Клиент Три',5000,$1,$2,true,true)", held, base)
	must(err)
	ignored := add(7, true, "")
	must(d.MarkMessageParsed(ctx, ignored))
	_, err = d.pool.Exec(ctx, "INSERT INTO bank_receipts(raw_message_id,amount,tx_date,group_jid,needs_review,ignored) VALUES($1,9000,$2,$3,true,true)", ignored, base, group)
	must(err)
	duplicate := add(8, true, "")
	must(d.MarkMessageParsed(ctx, duplicate))
	_, err = d.pool.Exec(ctx, "INSERT INTO bank_receipts(raw_message_id,amount,tx_date,group_jid,needs_review,is_duplicate) VALUES($1,11000,$2,$3,true,true)", duplicate, base, group)
	must(err)
	_, _, err = d.SaveRawMessage(ctx, group+"boundary", group, sender, "", "Мансур", "outside", false, "", to)
	must(err)
	_, _, err = d.SaveRawMessage(ctx, group+"other", group+"-other@g.us", sender, "", "Умар", "outside", false, "", base)
	must(err)
	_, _, err = d.SaveRawMessage(ctx, group+"private", "79280000001@s.whatsapp.net", sender, "", "Мансур", "private", false, "", base)
	must(err)
	_, _, err = d.SaveRawMessage(ctx, group+"forward", group, "bot-forward", "", "Мансур", "mirror", true, "", base)
	must(err)
	s, err := d.GroupReading(ctx, base, to, []string{group}, 2)
	must(err)
	if s.SavedMessages != 9 || s.DeletedMessages != 1 || s.SettledMessages != 3 || s.UnresolvedMessages != 5 || s.UnreadMedia != 1 || s.UnreadTexts != 1 || s.DisputedMessages != 1 || s.ReviewReceipts != 1 || s.UnknownReceiptAmounts != 1 || s.RecognitionPending != 1 || s.HeldPayments != 1 {
		t.Fatalf("wrong database status: %+v", s)
	}
	if !s.Truncated || len(s.Issues) != 2 {
		t.Fatalf("counters/list must have different limits: %+v", s)
	}
	all, err := d.GroupReading(ctx, base, to, []string{group}, 200)
	must(err)
	for _, issue := range all.Issues {
		if issue.RawID == held && issue.KnownPendingAmount != 5000 {
			t.Fatalf("held sum lost: %+v", issue)
		}
	}
	if _, err := d.GroupReading(ctx, to, base, []string{group}, 30); err == nil {
		t.Fatal("invalid range accepted")
	}
}
