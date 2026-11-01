package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
	"whatsapp-bot/internal/db"
)

func reopenSpoolDatabase(t *testing.T, e *e2eBot) *db.DB {
	t.Helper()
	d, err := db.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	e.db = d
	return d
}

func TestGroupSpoolRetainsOutageAndRestartsInCaptureOrder(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("spool-outage")
	e := newE2E(t, e2eOpts{})
	e.inboxSpoolDir = t.TempDir()
	e.db.Close() // PostgreSQL client unavailable; filesystem remains durable.
	m1 := groupText(group, user("79280000901"), "Мансур", "Каталов Ахмед 5000 наличка")
	m2 := groupText(group, user("79280000901"), "Мансур", "Исаев Харон 7000 наличка")
	e.captureGroupMessage(ctx, m1)
	e.captureGroupMessage(ctx, m2)
	files, err := e.groupSpoolFiles()
	if err != nil || len(files) != 2 {
		t.Fatalf("outage lost local events: %v / %v", files, err)
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("spool permissions: %v / %v", info, err)
		}
	}
	status, err := e.LocalGroupSpoolStatus(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{group.String()})
	if err != nil || status.Pending != 2 || status.Unreadable != 0 {
		t.Fatalf("local coverage: %+v / %v", status, err)
	}
	reopenSpoolDatabase(t, e)
	restarted := inboxRestartedBot(e, group)
	restarted.inboxSpoolDir = e.inboxSpoolDir
	release := inboxQueueBarrier(restarted, group)
	defer release()
	// New capture must first commit the older spooled records, even if the
	// periodic startup recovery has not run yet.
	m3 := groupText(group, user("79280000901"), "Мансур", "Хасанов Ислам 9000 наличка")
	restarted.captureGroupMessage(ctx, m3)
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT wa_message_id FROM group_inbox WHERE group_jid=$1 ORDER BY id`, group.String())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	rows.Close()
	if len(got) != 3 || got[0] != m1.Info.ID || got[1] != m2.Info.ID || got[2] != m3.Info.ID {
		t.Fatalf("new capture overtook recovered events: %v", got)
	}
	files, err = restarted.groupSpoolFiles()
	if err != nil || len(files) != 0 {
		t.Fatalf("committed local records not removed: %v / %v", files, err)
	}
	release()
	restarted.drainQueues(10 * time.Second)
	if total := sum(dayTotals(e, group)); total != 21000 {
		t.Fatalf("recovered collection=%v, want 21000", total)
	}
	restarted.recoverGroupInbox(ctx)
	restarted.drainQueues(10 * time.Second)
	if total := sum(dayTotals(e, group)); total != 21000 {
		t.Fatalf("second recovery duplicated collection=%v", total)
	}
}

func TestGroupSpoolCommitBeforeRemovalDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("spool-commit-window")
	e := newE2E(t, e2eOpts{})
	e.inboxSpoolDir = t.TempDir()
	e.db.Close()
	msg := groupText(group, user("79280000902"), "Мансур", "Каталов Ахмед 5000 наличка")
	e.captureGroupMessage(ctx, msg)
	files, err := e.groupSpoolFiles()
	if err != nil || len(files) != 1 {
		t.Fatal("local event not persisted")
	}
	event, err := readGroupSpool(files[0])
	if err != nil {
		t.Fatal(err)
	}
	d := reopenSpoolDatabase(t, e)
	if _, inserted, err := d.CaptureGroupEvent(ctx, event); err != nil || !inserted {
		t.Fatalf("first DB commit: inserted=%v / %v", inserted, err)
	}
	// Simulate death between PostgreSQL commit and local file unlink.
	restarted := inboxRestartedBot(e, group)
	restarted.inboxSpoolDir = e.inboxSpoolDir
	restarted.recoverGroupInbox(ctx)
	restarted.drainQueues(10 * time.Second)
	restarted.recoverGroupInbox(ctx)
	restarted.drainQueues(10 * time.Second)
	ref, err := d.RawMessageByWaID(ctx, msg.Info.ID)
	if err != nil || !ref.Found {
		t.Fatalf("recovery missing: %+v / %v", ref, err)
	}
	ids, err := d.TxIDsByRawID(ctx, ref.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("commit/remove crash duplicated payment: %v / %v", ids, err)
	}
	files, err = restarted.groupSpoolFiles()
	if err != nil || len(files) != 0 {
		t.Fatalf("local commit not settled: %v / %v", files, err)
	}
}

func TestGroupSpoolCorruptionRemainsVisibleWithoutBlockingValidEvents(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("spool-corrupt")
	e := newE2E(t, e2eOpts{})
	e.inboxSpoolDir = t.TempDir()
	bad := filepath.Join(e.inboxSpoolDir, "00000000000000000001-corrupted.json")
	if err := os.WriteFile(bad, []byte(`{"unfinished`), 0600); err != nil {
		t.Fatal(err)
	}
	msg := groupText(group, user("79280000903"), "Мансур", "Каталов Ахмед 5000 наличка")
	e.captureGroupMessage(ctx, msg)
	e.waitIdle()
	status, err := e.LocalGroupSpoolStatus(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), nil)
	if err != nil || status.Unreadable != 1 || status.Pending != 0 {
		t.Fatalf("corrupt coverage: %+v / %v", status, err)
	}
	ref, err := e.db.RawMessageByWaID(ctx, msg.Info.ID)
	if err != nil || !ref.Found || sum(dayTotals(e, group)) != 5000 {
		t.Fatalf("corrupt record blocked a valid event: %+v / %v", ref, err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("corrupt original was discarded")
	}
}

func TestGroupInboxProtocolDatabaseFailureIsNotAcknowledged(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"edit", "revoke"} {
		t.Run(kind, func(t *testing.T) {
			e := newE2E(t, e2eOpts{})
			group, worker := e2eGroup("protocol-db-failure"), user("79280000904")
			original := groupText(group, worker, "Мансур", "Каталов Ахмед 5000 наличка")
			e.sendGroup(original)
			pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			predicate := "NEW.edit_count>OLD.edit_count"
			if kind == "revoke" {
				predicate = "NEW.deleted AND NOT OLD.deleted"
			}
			// Isolated test database, trigger removed before leaving this subtest.
			ddl := `CREATE OR REPLACE FUNCTION fail_protocol_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF ` + predicate + ` THEN RAISE EXCEPTION 'test protocol storage failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_protocol_test_trigger BEFORE UPDATE ON raw_messages FOR EACH ROW EXECUTE FUNCTION fail_protocol_test()`
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			cleanup := func() {
				_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS fail_protocol_test_trigger ON raw_messages; DROP FUNCTION IF EXISTS fail_protocol_test()`)
			}
			defer cleanup()
			protocol := groupText(group, worker, "Мансур", "")
			p := &waProto.ProtocolMessage{Key: &waProto.MessageKey{ID: proto.String(original.Info.ID), RemoteJID: proto.String(group.String()), Participant: proto.String(worker.String())}}
			if kind == "edit" {
				p.Type = waProto.ProtocolMessage_MESSAGE_EDIT.Enum()
				p.EditedMessage = &waProto.Message{Conversation: proto.String("Каталов Ахмед 6000 наличка")}
			} else {
				p.Type = waProto.ProtocolMessage_REVOKE.Enum()
			}
			protocol.Message = &waProto.Message{ProtocolMessage: p}
			e.captureGroupMessage(ctx, protocol)
			e.waitIdle()
			var state string
			if err := pool.QueryRow(ctx, `SELECT state FROM group_inbox WHERE wa_message_id=$1 AND kind=$2`, protocol.Info.ID, kind).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == "done" {
				t.Fatal("failed protocol operation was acknowledged as done")
			}
			cleanup()
			e.recoverGroupInbox(ctx)
			e.waitIdle()
			if err := pool.QueryRow(ctx, `SELECT state FROM group_inbox WHERE wa_message_id=$1 AND kind=$2`, protocol.Info.ID, kind).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "done" {
				t.Fatalf("protocol did not recover: %s", state)
			}
			ref, err := e.db.RawMessageByWaID(ctx, original.Info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "revoke" && !ref.Deleted {
				t.Fatal("revoke did not recover")
			}
			if kind == "edit" && !strings.Contains(ref.Body, "6000") {
				t.Fatalf("edit did not recover: %+v", ref)
			}
		})
	}
}

func TestGroupSpoolCorruptedSameKeyDoesNotLoseRedelivery(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("spool-corrupt-redelivery")
	e := newE2E(t, e2eOpts{})
	e.inboxSpoolDir = t.TempDir()
	e.db.Close()
	msg := groupText(group, user("79280000905"), "Мансур", "Каталов Ахмед 5000 наличка")
	e.captureGroupMessage(ctx, msg)
	files, err := e.groupSpoolFiles()
	if err != nil || len(files) != 1 {
		t.Fatal("local capture missing")
	}
	corrupt := files[0]
	if err := os.WriteFile(corrupt, []byte("broken payload"), 0600); err != nil {
		t.Fatal(err)
	}
	e.captureGroupMessage(ctx, msg)
	status, err := e.LocalGroupSpoolStatus(time.Time{}, time.Time{}, nil)
	if err != nil || status.Unreadable != 1 || status.Pending != 1 {
		t.Fatalf("redelivery was discarded against corrupt key: %+v / %v", status, err)
	}
	reopenSpoolDatabase(t, e)
	restarted := inboxRestartedBot(e, group)
	restarted.inboxSpoolDir = e.inboxSpoolDir
	restarted.recoverGroupInbox(ctx)
	restarted.drainQueues(10 * time.Second)
	if total := sum(dayTotals(e, group)); total != 5000 {
		t.Fatalf("valid redelivery did not recover: %v", total)
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatal("corrupt evidence was removed")
	}
}

func TestGroupInboxPartialRevokeCleanupRequiresReview(t *testing.T) {
	ctx := context.Background()
	group, worker := e2eGroup("revoke-partial-cleanup"), user("79280000906")
	e := newE2E(t, e2eOpts{})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	name := groupText(group, worker, "Мансур", "Каталов Ахмед")
	e.sendGroup(name)
	check := e.sendReceipt(group, worker, "Мансур", sberCheck("15000", "11:00:00"), "Каталов Ахмед")
	ref, err := e.db.RawMessageByWaID(ctx, name.Info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetReceiptSourceByMessage(ctx, check, ref.ID); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `CREATE FUNCTION fail_delete_cleanup_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.client_source_raw_id IS NOT NULL AND NEW.client_source_raw_id IS NULL THEN RAISE EXCEPTION 'test deletion link cleanup failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_delete_cleanup_test_trigger BEFORE UPDATE ON bank_receipts FOR EACH ROW EXECUTE FUNCTION fail_delete_cleanup_test()`)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS fail_delete_cleanup_test_trigger ON bank_receipts; DROP FUNCTION IF EXISTS fail_delete_cleanup_test()`)
	}
	defer cleanup()
	revoke := groupText(group, worker, "Мансур", "")
	revoke.Message = &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{ID: proto.String(name.Info.ID), RemoteJID: proto.String(group.String())}}}
	notifications := len(e.sent())
	e.captureGroupMessage(ctx, revoke)
	e.waitIdle()
	if len(e.sentSince(notifications)) != 0 {
		t.Fatal("partial cleanup sent a false success notification")
	}
	var state, note string
	if err := pool.QueryRow(ctx, `SELECT state,note FROM group_inbox WHERE wa_message_id=$1 AND kind='revoke'`, revoke.Info.ID).Scan(&state, &note); err != nil {
		t.Fatal(err)
	}
	if state != "review" || note != "revoke_cleanup_not_confirmed" {
		t.Fatalf("partial deletion falsely acknowledged: %s / %s", state, note)
	}
	after, err := e.db.RawMessageByWaID(ctx, name.Info.ID)
	if err != nil || !after.Deleted {
		t.Fatal("fixture did not reach partial-deletion window")
	}
	cleanup()
	e.recoverGroupInbox(ctx)
	e.waitIdle()
	if err := pool.QueryRow(ctx, `SELECT state FROM group_inbox WHERE wa_message_id=$1 AND kind='revoke'`, revoke.Info.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "review" {
		t.Fatal("partial revoke was blindly rerun/resolved")
	}
}
