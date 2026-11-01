package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTextReadingRetriesPersistBackoffAndStopAfterEight(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	wa := fmt.Sprintf("text-retry-%d", time.Now().UnixNano())
	group := wa + "@g.us"
	raw, _, err := d.SaveRawMessage(ctx, wa, group, "79280000996@s.whatsapp.net", "79280000996", "Мансур", "Неизвестный Клиент 10000 наличка", false, "", time.Now().AddDate(0, 0, -40))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	contains := func(items []UnparsedMessage) bool {
		for _, m := range items {
			if m.ID == raw {
				return true
			}
		}
		return false
	}
	due, err := d.DueTextReadingRetries(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(due) {
		t.Fatal("old unread text silently expired")
	}
	delays := []time.Duration{2, 5, 10, 20, 30, 30, 30}
	for attempt := 1; attempt <= MaxTextReadingAttempts; attempt++ {
		before := time.Now()
		retry, err := d.ScheduleTextReadingRetry(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if retry != (attempt < MaxTextReadingAttempts) {
			t.Fatalf("attempt %d retry=%v", attempt, retry)
		}
		var stored int
		var next *time.Time
		if err := d.pool.QueryRow(ctx, `SELECT text_attempts,text_next_at FROM raw_messages WHERE id=$1`, raw).Scan(&stored, &next); err != nil {
			t.Fatal(err)
		}
		if stored != attempt {
			t.Fatalf("attempt persisted as %d, want %d", stored, attempt)
		}
		if attempt < MaxTextReadingAttempts {
			want := delays[attempt-1] * time.Minute
			if next == nil || next.Sub(before) < want-time.Second || next.Sub(before) > want+5*time.Second {
				t.Fatalf("attempt %d backoff next=%v want=%v", attempt, next, want)
			}
		} else if next != nil {
			t.Fatal("exhausted text should not schedule another attempt")
		}
		// Another process observes the same schedule; memory loss cannot bypass it.
		due, err = d.DueTextReadingRetries(ctx, since, 500)
		if err != nil {
			t.Fatal(err)
		}
		if contains(due) {
			t.Fatalf("attempt %d ignored persistent backoff/budget", attempt)
		}
		if attempt < MaxTextReadingAttempts {
			if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET text_next_at=now()-interval '1 second' WHERE id=$1`, raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	if retry, err := d.ScheduleTextReadingRetry(ctx, raw); err != nil || retry {
		t.Fatalf("exhausted text scheduled again: %v %v", retry, err)
	}
	manual, err := d.UnparsedTextMessagesSince(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manual) {
		t.Fatal("exhausted message disappeared from manual review")
	}
	if _, err := d.PrepareMessageEdit(ctx, raw, "Исправленный Клиент 10000 наличка"); err != nil {
		t.Fatal(err)
	}
	due, err = d.DueTextReadingRetries(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(due) {
		t.Fatal("message edit did not reset exhausted retry budget")
	}
}

func TestTextReadingRetriesSkipResolvedAndDisputedButKeepPartial(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("text-retry-states-%d", time.Now().UnixNano())
	var ids []int
	for i := 0; i < 5; i++ {
		raw, _, err := d.SaveRawMessage(ctx, fmt.Sprintf("%s-%d", tag, i), tag+"@g.us", "79280000997@s.whatsapp.net", "79280000997", "Мансур", "Клиент 10000", false, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, raw)
	}
	for i, assignment := range []string{"parsed=true", "deleted=true", "ai_disputed=true", "handled_as='answer'"} {
		if _, err := d.pool.Exec(ctx, `UPDATE raw_messages SET `+assignment+` WHERE id=$1`, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := d.GetOrCreateContact(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InsertTransaction(ctx, TransactionInput{ContactID: cid, RawName: tag, Amount: 10000, RawMessageID: ids[4], TxDate: time.Now()}); err != nil {
		t.Fatal(err)
	}
	due, err := d.DueTextReadingRetries(ctx, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 500)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]bool{}
	for _, m := range due {
		found[m.ID] = true
	}
	for _, id := range ids[:4] {
		if found[id] {
			t.Fatalf("resolved/disputed message %d still due", id)
		}
		if retry, err := d.ScheduleTextReadingRetry(ctx, id); err != nil || retry {
			t.Fatalf("resolved/disputed message scheduled: %v %v", retry, err)
		}
	}
	if !found[ids[4]] {
		t.Fatal("partial existing transaction hid unfinished text")
	}
}

func TestTextReadingCashHintPersistsAndRevisionGuardsIt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	id := fmt.Sprintf("cash-hint-%d", time.Now().UnixNano())
	raw, _, err := d.SaveRawMessage(ctx, id, id+"@g.us", "79280000998@s.whatsapp.net", "79280000998", "Мансур", "Клиент 10000", false, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RememberCashHint(ctx, raw, 0); err != nil {
		t.Fatal(err)
	}
	if hint, err := d.CashHint(ctx, raw); err != nil || !hint {
		t.Fatalf("cash hint was not persisted: %v %v", hint, err)
	}
	if _, err := d.PrepareMessageEdit(ctx, raw, "Клиент 10000 перевод"); err != nil {
		t.Fatal(err)
	}
	if hint, err := d.CashHint(ctx, raw); err != nil || hint {
		t.Fatalf("cash hint survived an edit: %v %v", hint, err)
	}
	if err := d.RememberCashHint(ctx, raw, 0); err != ErrMessageRevisionChanged {
		t.Fatalf("old recognition changed the new revision: %v", err)
	}
	if hint, err := d.CashHint(ctx, raw); err != nil || hint {
		t.Fatalf("stale cash hint was restored: %v %v", hint, err)
	}
}
