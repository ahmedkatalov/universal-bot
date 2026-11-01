package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func inboxFixture(t *testing.T, d *DB, group, key string, command bool) GroupInboxEvent {
	t.Helper()
	e, created, err := d.CaptureGroupEvent(context.Background(), GroupInboxEvent{Key: key, WaID: key, GroupJID: group, Kind: "message", Command: command, Metadata: []byte(`{"saved":true}`), Payload: []byte("original-payload"), ReceivedAt: time.Now().AddDate(0, 0, -30)})
	if err != nil || !created {
		t.Fatalf("capture: %+v %v %v", e, created, err)
	}
	return e
}

func TestGroupInboxOrderingDedupAndIndependentChats(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("inbox-order")
	a := inboxFixture(t, d, group, fmt.Sprintf("inbox-a-%d", time.Now().UnixNano()), false)
	b := inboxFixture(t, d, group, a.WaID+"-b", false)
	other := inboxFixture(t, d, group+"-other", a.WaID+"-other", false)
	command := inboxFixture(t, d, group, a.WaID+"-cmd", true)
	if _, ok, err := d.ClaimGroupEvent(ctx, b.ID, time.Minute); err != nil || ok {
		t.Fatalf("overtook older event: %v %v", ok, err)
	}
	first, ok, err := d.ClaimGroupEvent(ctx, a.ID, time.Minute)
	if err != nil || !ok {
		t.Fatal("first not claimed", err)
	}
	if _, ok, err := d.ClaimGroupEvent(ctx, b.ID, time.Minute); err != nil || ok {
		t.Fatal("overtook running predecessor", err)
	}
	if _, ok, err := d.ClaimGroupEvent(ctx, other.ID, time.Minute); err != nil || !ok {
		t.Fatal("blocked other group", err)
	}
	if _, ok, err := d.ClaimGroupEvent(ctx, command.ID, time.Minute); err != nil || !ok {
		t.Fatal("owner command blocked", err)
	}
	duplicate, created, err := d.CaptureGroupEvent(ctx, a)
	if err != nil || created || duplicate.ID != a.ID {
		t.Fatal("duplicate created", err)
	}
	if err := d.FinishGroupEvent(ctx, first, "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.ClaimGroupEvent(ctx, b.ID, time.Minute); err != nil || !ok {
		t.Fatal("next event not released", err)
	}
}

func TestGroupInboxExpiredLeaseUsesAttemptToken(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("inbox-lease")
	e := inboxFixture(t, d, group, fmt.Sprintf("lease-%d", time.Now().UnixNano()), false)
	old, ok, err := d.ClaimGroupEvent(ctx, e.ID, time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE group_inbox SET lease_until=now()-interval '1 second' WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	newer, ok, err := d.ClaimGroupEvent(ctx, e.ID, time.Minute)
	if err != nil || !ok || newer.Attempts != 2 {
		t.Fatalf("lease recovery: %+v %v", newer, err)
	}
	if err := d.FinishGroupEvent(ctx, old, "done", ""); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = d.pool.QueryRow(ctx, `SELECT state FROM group_inbox WHERE id=$1`, e.ID).Scan(&state)
	if state != "processing" {
		t.Fatal("old worker completed a new lease")
	}
	if err := d.FinishGroupEvent(ctx, newer, "done", ""); err != nil {
		t.Fatal(err)
	}
}

func TestGroupInboxInterruptedCommandNotReplayed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("inbox-command")
	for _, finish := range []string{"pending", "expired"} {
		e := inboxFixture(t, d, group, fmt.Sprintf("cmd-%s-%d", finish, time.Now().UnixNano()), true)
		claimed, ok, err := d.ClaimGroupEvent(ctx, e.ID, time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if finish == "pending" {
			if err := d.FinishGroupEvent(ctx, claimed, "pending", "database_unavailable"); err != nil {
				t.Fatal(err)
			}
		} else {
			_, err = d.pool.Exec(ctx, `UPDATE group_inbox SET lease_until=now()-interval '1 second' WHERE id=$1`, e.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.DueGroupEvents(ctx, 100); err != nil {
			t.Fatal(err)
		}
		var state string
		_ = d.pool.QueryRow(ctx, `SELECT state FROM group_inbox WHERE id=$1`, e.ID).Scan(&state)
		if state != "review" {
			t.Fatalf("interrupted command stranded in %s", state)
		}
		if _, ok, _ := d.ClaimGroupEvent(ctx, e.ID, time.Minute); ok {
			t.Fatal("command repeated")
		}
	}
}

func TestGroupInboxRecoveryLoadsOrderedBacklogAsBatch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("inbox-backlog")
	var events []GroupInboxEvent
	for i := 0; i < 3; i++ {
		events = append(events, inboxFixture(t, d, group, fmt.Sprintf("backlog-%d-%d", time.Now().UnixNano(), i), false))
	}
	due, err := d.DueGroupEvents(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var found []int64
	for _, e := range due {
		if e.GroupJID == group {
			found = append(found, e.ID)
		}
	}
	if len(found) != 3 || found[0] != events[0].ID || found[2] != events[2].ID {
		t.Fatalf("backlog throttled to one event per tick: %v", found)
	}
	for _, e := range events {
		claimed, ok, err := d.ClaimGroupEvent(ctx, e.ID, time.Minute)
		if err != nil || !ok {
			t.Fatalf("ordered batch could not advance: %+v %v", e, err)
		}
		if err := d.FinishGroupEvent(ctx, claimed, "done", ""); err != nil {
			t.Fatal(err)
		}
	}
}
