package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The inbox commits the original event before media downloads and AI calls.
// Its state describes dispatch, not successful recognition or collection.
const groupInboxSchema = `
CREATE TABLE IF NOT EXISTS group_inbox (
	id BIGSERIAL PRIMARY KEY,
	event_key TEXT NOT NULL UNIQUE,
	wa_message_id TEXT NOT NULL,
	group_jid TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('message','edit','revoke')),
	command BOOLEAN NOT NULL DEFAULT false,
	metadata JSONB NOT NULL,
	payload BYTEA NOT NULL,
	received_at TIMESTAMPTZ NOT NULL,
	state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processing','done','review')),
	attempts INT NOT NULL DEFAULT 0,
	lease_until TIMESTAMPTZ,
	note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_group_inbox_work ON group_inbox (state, id) WHERE state IN ('pending','processing');
CREATE INDEX IF NOT EXISTS idx_group_inbox_chat ON group_inbox (group_jid, id);
CREATE INDEX IF NOT EXISTS idx_group_inbox_period ON group_inbox (group_jid, received_at) WHERE state <> 'done';
`

type GroupInboxEvent struct {
	ID                        int64
	Key, WaID, GroupJID, Kind string
	Command                   bool
	Metadata, Payload         []byte
	ReceivedAt                time.Time
	State, Note               string
	Attempts                  int
}

const inboxCols = `id, event_key, wa_message_id, group_jid, kind, command,
	metadata, payload, received_at, state, note, attempts`

func scanGroupEvent(row pgx.Row) (GroupInboxEvent, error) {
	var e GroupInboxEvent
	err := row.Scan(&e.ID, &e.Key, &e.WaID, &e.GroupJID, &e.Kind, &e.Command,
		&e.Metadata, &e.Payload, &e.ReceivedAt, &e.State, &e.Note, &e.Attempts)
	return e, err
}

func (d *DB) CaptureGroupEvent(ctx context.Context, e GroupInboxEvent) (GroupInboxEvent, bool, error) {
	row := d.pool.QueryRow(ctx, `INSERT INTO group_inbox
		(event_key, wa_message_id, group_jid, kind, command, metadata, payload, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (event_key) DO NOTHING
		RETURNING `+inboxCols, e.Key, e.WaID, e.GroupJID, e.Kind, e.Command, e.Metadata, e.Payload, e.ReceivedAt)
	saved, err := scanGroupEvent(row)
	if !errors.Is(err, pgx.ErrNoRows) {
		return saved, err == nil, err
	}
	saved, err = scanGroupEvent(d.pool.QueryRow(ctx, `SELECT `+inboxCols+` FROM group_inbox WHERE event_key=$1`, e.Key))
	return saved, false, err
}

// Claim uses an attempt token and a lease; an older ordered event blocks later
// ones in the same chat even when new messages arrive during startup recovery.
func (d *DB) ClaimGroupEvent(ctx context.Context, id int64, lease time.Duration) (GroupInboxEvent, bool, error) {
	e, err := scanGroupEvent(d.pool.QueryRow(ctx, `UPDATE group_inbox i
		SET state='processing', attempts=attempts+1, lease_until=now()+($2::double precision * interval '1 second'), updated_at=now()
		WHERE i.id=$1
		  AND (i.state='pending' OR (i.state='processing' AND i.lease_until < now()))
		  AND NOT (i.command AND i.attempts > 0)
		  AND (i.command OR NOT EXISTS (
			SELECT 1 FROM group_inbox prev WHERE prev.group_jid=i.group_jid AND prev.id<i.id
			AND NOT prev.command AND prev.state IN ('pending','processing')
		  )) RETURNING `+inboxCols, id, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupInboxEvent{}, false, nil
	}
	return e, err == nil, err
}

func (d *DB) FinishGroupEvent(ctx context.Context, e GroupInboxEvent, state, note string) error {
	if state != "done" && state != "pending" && state != "review" {
		return errors.New("invalid inbox completion state")
	}
	_, err := d.pool.Exec(ctx, `UPDATE group_inbox SET state=$3, note=$4, lease_until=NULL, updated_at=now()
		WHERE id=$1 AND attempts=$2 AND state='processing'`, e.ID, e.Attempts, state, note)
	return err
}

// Interrupted commands may already have sent money-related writes/messages.
// Keep them visible for review instead of repeating an external action.
func (d *DB) DueGroupEvents(ctx context.Context, limit int) ([]GroupInboxEvent, error) {
	// Recovered recognition can settle a previously interrupted event without
	// re-running the original command or applying the payment twice.
	if _, err := d.pool.Exec(ctx, `UPDATE group_inbox i SET state='done', note='recovered_by_recognition', updated_at=now()
		WHERE i.state='review' AND i.kind='message' AND i.note='interrupted_recognition' AND EXISTS (
			SELECT 1 FROM raw_messages rm WHERE rm.wa_message_id=i.wa_message_id AND rm.wa_group_jid=i.group_jid
			AND (rm.parsed OR rm.deleted OR rm.handled_as IS NOT NULL))`); err != nil {
		return nil, err
	}
	if _, err := d.pool.Exec(ctx, `UPDATE group_inbox SET state='review', note='interrupted_command', lease_until=NULL, updated_at=now()
		WHERE command AND attempts>0 AND (state='pending' OR (state='processing' AND lease_until<now()))`); err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `SELECT `+inboxCols+` FROM group_inbox i
		WHERE (i.state='pending' OR (i.state='processing' AND i.lease_until<now()))
		ORDER BY i.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupInboxEvent
	for rows.Next() {
		e, err := scanGroupEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GroupEventRawState helps resume only work which has not already changed the
// ledger. Existing raw-message retry queues own unfinished recognition.
type GroupEventRawState struct {
	Exists, Parsed, Deleted, HasMedia, HasPayments, HasReceipt bool
	GroupJID                                                   string
	HandledAs                                                  string
}

func (d *DB) GroupEventRawState(ctx context.Context, waID string) (GroupEventRawState, error) {
	var s GroupEventRawState
	err := d.pool.QueryRow(ctx, `SELECT parsed, deleted, has_media, COALESCE(handled_as,''),wa_group_jid,
		EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id=rm.id),
		EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id=rm.id)
		FROM raw_messages rm WHERE wa_message_id=$1`, waID).
		Scan(&s.Parsed, &s.Deleted, &s.HasMedia, &s.HandledAs, &s.GroupJID, &s.HasPayments, &s.HasReceipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	s.Exists = err == nil
	return s, err
}

type GroupInboxIssue struct {
	WaID, GroupJID, Kind, State, Note string
	ReceivedAt                        time.Time
}
type GroupInboxCoverage struct {
	Pending, Processing, Review int
	Issues                      []GroupInboxIssue
}

func (d *DB) GroupInboxStatus(ctx context.Context, from, to time.Time, groups []string, limit int) (GroupInboxCoverage, error) {
	return groupInboxStatus(ctx, d.pool, from, to, groups, limit)
}

func groupInboxStatus(ctx context.Context, q groupReadingQuerier, from, to time.Time, groups []string, limit int) (GroupInboxCoverage, error) {
	var result GroupInboxCoverage
	if !to.After(from) {
		return result, errors.New("конец периода должен быть позже начала")
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	// A raw message is diagnosed by the recognition report, not counted twice.
	const condition = `i.state <> 'done' AND i.received_at >= $1 AND i.received_at < $2
  AND ($3::text[] IS NULL OR i.group_jid=ANY($3))
		AND NOT EXISTS (SELECT 1 FROM raw_messages rm WHERE rm.wa_message_id=i.wa_message_id AND rm.wa_group_jid=i.group_jid AND i.kind='message' AND i.state <> 'review')`
	err := q.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE state='pending'),COUNT(*) FILTER (WHERE state='processing'),COUNT(*) FILTER (WHERE state='review') FROM group_inbox i WHERE `+condition, from, to, groupSliceArg(groups)).Scan(&result.Pending, &result.Processing, &result.Review)
	if err != nil {
		return result, err
	}
	rows, err := q.Query(ctx, `SELECT wa_message_id,group_jid,kind,state,note,received_at FROM group_inbox i WHERE `+condition+` ORDER BY id LIMIT $4`, from, to, groupSliceArg(groups), limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var issue GroupInboxIssue
		if err := rows.Scan(&issue.WaID, &issue.GroupJID, &issue.Kind, &issue.State, &issue.Note, &issue.ReceivedAt); err != nil {
			return result, err
		}
		result.Issues = append(result.Issues, issue)
	}
	return result, rows.Err()
}
