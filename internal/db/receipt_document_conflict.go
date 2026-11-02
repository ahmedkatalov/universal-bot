package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ReceiptDocumentAmountConflict reports contradictory evidence without marking
// either receipt as a duplicate or choosing an OCR amount as the truth.
func (d *DB) ReceiptDocumentAmountConflict(ctx context.Context, rawID int) (bool, error) {
	var found bool
	err := d.pool.QueryRow(ctx, receiptIdentityCTE+`SELECT true
	 FROM receipt_identity current
	 JOIN raw_messages crm ON crm.id=current.raw_message_id
	 JOIN receipt_identity other ON other.document_key=current.document_key AND other.id<>current.id
	 JOIN raw_messages orm ON orm.id=other.raw_message_id
	 WHERE current.raw_message_id=$1 AND char_length(current.document_key)>=8
	 AND COALESCE(current.group_jid,crm.wa_group_jid)=COALESCE(other.group_jid,orm.wa_group_jid)
	 AND current.amount>0 AND other.amount>0 AND current.amount<>other.amount
	 AND NOT current.is_duplicate AND NOT current.ignored AND NOT crm.deleted
	 AND NOT other.is_duplicate AND NOT other.ignored AND NOT orm.deleted
	 AND NOT EXISTS(SELECT 1 FROM amount_corrections ac WHERE ac.kind='receipt'
	 AND ac.target_id IN(current.id,other.id) AND NOT ac.reverted)
	 LIMIT 1`, rawID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return found, err
}
