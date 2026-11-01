package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const MaxTextReadingAttempts = 8

// CashHint is evidence from an adjacent cash photo. It survives process-local
// retry context loss and must not silently become false on a database error.
func (d *DB) CashHint(ctx context.Context, rawID int) (bool, error) {
	var hint bool
	err := d.pool.QueryRow(ctx, `SELECT cash_hint FROM raw_messages WHERE id=$1`, rawID).Scan(&hint)
	return hint, err
}

// A supplied revision prevents an old extraction from annotating an edited
// message. Without one, deterministic processing annotates its current row.
func (d *DB) RememberCashHint(ctx context.Context, rawID int, revision ...int) error {
	expected := -1
	if len(revision) > 0 {
		expected = revision[0]
	}
	var id int
	err := d.pool.QueryRow(ctx, `UPDATE raw_messages SET cash_hint=true
		WHERE id=$1 AND NOT deleted AND ($2<0 OR edit_count=$2) RETURNING id`, rawID, expected).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMessageRevisionChanged
	}
	return err
}

// Text retries belong to the raw message, not a process-local queue. Exhausted
// messages remain unparsed and visible for manual review; they do not expire.
func (d *DB) DueTextReadingRetries(ctx context.Context, since time.Time, limit int) ([]UnparsedMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, COALESCE(rm.wa_group_jid,''), COALESCE(rm.sender_name,''), COALESCE(rm.body,''), rm.received_at, rm.ai_disputed
		FROM raw_messages rm
		WHERE NOT rm.parsed AND NOT rm.has_media AND NOT rm.deleted AND NOT rm.ai_disputed
		  AND rm.handled_as IS NULL AND btrim(COALESCE(rm.body,'')) <> ''
		  AND COALESCE(rm.wa_group_jid,'') LIKE '%@g.us' AND rm.received_at >= $1
		  AND rm.text_attempts < $2 AND (rm.text_next_at IS NULL OR rm.text_next_at <= now())
		ORDER BY COALESCE(rm.text_next_at,rm.received_at),rm.id
		LIMIT $3`, since, MaxTextReadingAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnparsedMessage
	for rows.Next() {
		var m UnparsedMessage
		if err := rows.Scan(&m.ID, &m.GroupJID, &m.SenderName, &m.Body, &m.ReceivedAt, &m.Disputed); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Reload after entering the chat queue: an edit may have changed the body,
// retry schedule or review state since the scheduler selected its snapshot.
func (d *DB) DueTextReadingRetryByID(ctx context.Context, rawID int) (UnparsedMessage, bool, error) {
	var m UnparsedMessage
	err := d.pool.QueryRow(ctx, `
		SELECT rm.id, COALESCE(rm.wa_group_jid,''), COALESCE(rm.sender_name,''), COALESCE(rm.body,''), rm.received_at, rm.ai_disputed
		FROM raw_messages rm WHERE rm.id=$1
		  AND NOT rm.parsed AND NOT rm.has_media AND NOT rm.deleted AND NOT rm.ai_disputed
		  AND rm.handled_as IS NULL AND btrim(COALESCE(rm.body,'')) <> ''
		  AND COALESCE(rm.wa_group_jid,'') LIKE '%@g.us'
		  AND rm.text_attempts < $2 AND (rm.text_next_at IS NULL OR rm.text_next_at <= now())`, rawID, MaxTextReadingAttempts).
		Scan(&m.ID, &m.GroupJID, &m.SenderName, &m.Body, &m.ReceivedAt, &m.Disputed)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// ScheduleTextReadingRetry records one failed reading and its durable backoff.
// false means no automatic attempts remain or the raw message was resolved.
// The caller must leave an exhausted message visible rather than mark parsed.
func (d *DB) ScheduleTextReadingRetry(ctx context.Context, rawID int) (bool, error) {
	var attempts int
	err := d.pool.QueryRow(ctx, `
		UPDATE raw_messages SET text_attempts=text_attempts+1,
			text_next_at=CASE WHEN text_attempts+1 >= $2 THEN NULL ELSE now()+
				(CASE text_attempts WHEN 0 THEN interval '2 minutes' WHEN 1 THEN interval '5 minutes'
				WHEN 2 THEN interval '10 minutes' WHEN 3 THEN interval '20 minutes' ELSE interval '30 minutes' END) END
		WHERE id=$1 AND NOT parsed AND NOT deleted AND NOT ai_disputed AND handled_as IS NULL
		  AND NOT has_media AND text_attempts < $2
		RETURNING text_attempts`, rawID, MaxTextReadingAttempts).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return attempts < MaxTextReadingAttempts, nil
}
