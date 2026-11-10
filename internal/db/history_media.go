package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// RecordHistoryMediaEvidence restores source information before the caller
// checks whether a legacy attachment can be reread. A partial history copy
// must preserve an existing caption and download descriptor.
func (d *DB) RecordHistoryMediaEvidence(ctx context.Context, rawID int, groupJID, caption string, payload []byte, hasDownloadInfo bool) ([]byte, bool, error) {
	var stored []byte
	err := d.pool.QueryRow(ctx, `
		UPDATE raw_messages rm SET
			caption = COALESCE(NULLIF(rm.caption, ''), NULLIF($3, '')),
			media_proto = CASE WHEN $5 OR COALESCE(octet_length(rm.media_proto), 0) = 0 THEN $4 ELSE rm.media_proto END,
			media_next_at = GREATEST(rm.media_next_at, now() + interval '2 minutes')
		WHERE rm.id = $1 AND rm.wa_group_jid = $2 AND rm.has_media AND NOT rm.deleted
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
		  AND NOT EXISTS (SELECT 1 FROM transactions tx WHERE tx.raw_message_id = rm.id)
		RETURNING rm.media_proto`, rawID, groupJID, caption, payload, hasDownloadInfo).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return stored, err == nil, err
}
