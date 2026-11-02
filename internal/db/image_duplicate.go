package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SaveImageDuplicate preserves the second message in the receipt journal.
// The SHA shortcut must not leave a parsed attachment without any receipt,
// and must recheck the source/group/hash before copying its evidence.
func (d *DB) SaveImageDuplicate(ctx context.Context, rawID int, originalWaID, group string) (bool, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT deleted FROM raw_messages WHERE id=$1 FOR UPDATE`, rawID).Scan(&deleted); err != nil {
		return false, err
	}
	if deleted {
		return false, nil
	}
	var source int
	err = tx.QueryRow(ctx, `SELECT orm.id FROM raw_messages current
	 JOIN raw_messages orm ON orm.media_sha=current.media_sha AND orm.id<>current.id
	 JOIN bank_receipts br ON br.raw_message_id=orm.id
	 WHERE current.id=$1 AND current.wa_group_jid=$3 AND orm.wa_group_jid=$3
	 AND orm.wa_message_id=$2 AND COALESCE(current.media_sha,'')<>''
	 AND NOT orm.deleted AND NOT br.is_duplicate AND NOT br.ignored AND br.amount>0
	 ORDER BY br.id LIMIT 1 FOR SHARE OF orm,br`, rawID, originalWaID, group).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var exists, alreadyCopied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bank_receipts WHERE raw_message_id=$1),
	 EXISTS(SELECT 1 FROM bank_receipts WHERE raw_message_id=$1 AND is_duplicate AND superseded_by_raw=$2)`, rawID, source).Scan(&exists, &alreadyCopied); err != nil {
		return false, err
	}
	if exists && !alreadyCopied {
		return false, nil
	}
	if !exists {
		tag, err := tx.Exec(ctx, `INSERT INTO bank_receipts
		 (raw_message_id,bank,recipient_raw,recipient_bank,recipient_phone,sender_raw,sender_bank,sender_account,
		 card_owner,contact_id,amount,commission,doc_number,auth_code,status,needs_review,is_duplicate,
		 client_confirmed,group_jid,tx_date,collapsed,ignored,client_source_raw_id,tx_time_guessed,
		 superseded_by_raw,tx_date_only,clarify_asked,recognition_provisional)
		 SELECT $1,bank,recipient_raw,recipient_bank,recipient_phone,sender_raw,sender_bank,sender_account,
		 card_owner,contact_id,amount,commission,doc_number,auth_code,status,needs_review,true,
		 client_confirmed,$3,tx_date,collapsed,false,client_source_raw_id,tx_time_guessed,
		 $2,tx_date_only,true,recognition_provisional
		 FROM bank_receipts WHERE raw_message_id=$2 AND NOT is_duplicate AND NOT ignored
		 ORDER BY id LIMIT 1`, rawID, source, group)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() != 1 {
			return false, nil
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE raw_messages SET parsed=true WHERE id=$1`, rawID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
