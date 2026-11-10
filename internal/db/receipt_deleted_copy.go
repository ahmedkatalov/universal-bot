package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// deletedReceiptIdentitySQL requires the complete operation number, an exact
// recorded replacement link, or identical source bytes. A shared amount/name
// alone never proves that a surviving attachment replaces a deleted receipt.
func deletedReceiptIdentitySQL(copy, original string) string {
	doc, originalDoc := receiptDocumentSQL(copy+".doc_number"), receiptDocumentSQL(original+".doc_number")
	return `(` + copy + `.superseded_by_raw = ` + original + `.raw_message_id OR (` + copy + `.superseded_by_raw IS NULL AND (
	 (char_length(` + originalDoc + `) >= 8 AND ` + doc + ` = ` + originalDoc + `) OR EXISTS (
	 SELECT 1 FROM raw_messages cm JOIN raw_messages om ON om.id=` + original + `.raw_message_id
	 WHERE cm.id=` + copy + `.raw_message_id AND COALESCE(cm.media_sha,'')<>'' AND cm.media_sha=om.media_sha))))`
}

func deletedReceiptSamePhysicalSQL(a, b string) string {
	da, db := receiptDocumentSQL(a+".doc_number"), receiptDocumentSQL(b+".doc_number")
	return `((char_length(` + da + `) >= 8 AND ` + da + ` = ` + db + `) OR EXISTS (
	 SELECT 1 FROM raw_messages am JOIN raw_messages bm ON bm.id=` + b + `.raw_message_id
	 WHERE am.id=` + a + `.raw_message_id AND COALESCE(am.media_sha,'')<>'' AND am.media_sha=bm.media_sha))`
}

// Old SHA shortcuts marked the attachment parsed without saving a duplicate
// receipt. Only identical bytes and an otherwise untouched legacy row can
// recover that missing evidence.
const deletedReceiptLegacyRawCopySQL = legacyParsedMediaCandidateSQL + `
 AND COALESCE(orm.media_sha,'')<>'' AND rm.media_sha=orm.media_sha
 AND rm.wa_group_jid=orm.wa_group_jid
 AND rm.wa_group_jid=COALESCE(orig.group_jid,orm.wa_group_jid,'')`

func restoreLegacyDeletedReceiptCopy(ctx context.Context, tx pgx.Tx, source int, since time.Time, primaryMatch string) (bool, error) {
	var original, copy int
	var caption, body, client string
	err := tx.QueryRow(ctx, `SELECT orig.id,rm.id,COALESCE(rm.caption,''),COALESCE(rm.body,''),
	 COALESCE(c.canonical_name,orig.recipient_raw,'')
	 FROM bank_receipts orig JOIN raw_messages orm ON orm.id=orig.raw_message_id AND orm.deleted
	 JOIN raw_messages rm ON rm.id<>orm.id
	 LEFT JOIN contacts c ON c.id=orig.contact_id
	 WHERE orm.id=$1 AND NOT orig.is_duplicate AND NOT orig.ignored AND orig.amount>0
	 AND rm.received_at >= $2 AND `+deletedReceiptLegacyRawCopySQL+`
	 AND NOT EXISTS (SELECT 1 FROM bank_receipts live JOIN raw_messages lrm ON lrm.id=live.raw_message_id
	 WHERE NOT live.is_duplicate AND NOT live.ignored AND NOT lrm.deleted
	 AND COALESCE(live.group_jid,lrm.wa_group_jid,'')=COALESCE(orig.group_jid,orm.wa_group_jid,'')
	 AND `+primaryMatch+`)
	 ORDER BY orig.id,rm.received_at,rm.id LIMIT 1 FOR UPDATE OF orig,rm`, source, since).
		Scan(&original, &copy, &caption, &body, &client)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A different caption can deliberately attribute the identical image to
	// someone else. Keep that disagreement pending rather than copying certainty.
	conflict := strings.TrimSpace(caption) != "" && contactKey(caption) != contactKey(client)
	conflict = conflict || (legacyPlainClientName(body) && contactKey(body) != contactKey(client))
	tag, err := tx.Exec(ctx, `INSERT INTO bank_receipts
	 (raw_message_id,bank,recipient_raw,recipient_bank,recipient_phone,sender_raw,sender_bank,sender_account,
	 card_owner,contact_id,amount,commission,doc_number,auth_code,status,needs_review,is_duplicate,
	 client_confirmed,group_jid,tx_date,collapsed,ignored,client_source_raw_id,tx_time_guessed,
	 superseded_by_raw,tx_date_only,clarify_asked,recognition_provisional,recognition_status)
	 SELECT rm.id,orig.bank,orig.recipient_raw,orig.recipient_bank,orig.recipient_phone,orig.sender_raw,
	 orig.sender_bank,orig.sender_account,orig.card_owner,orig.contact_id,orig.amount,orig.commission,
	 orig.doc_number,orig.auth_code,orig.status,
	 orig.needs_review OR orig.recognition_provisional OR orig.recognition_status<>'ok' OR $3,true,
	 orig.client_confirmed,rm.wa_group_jid,orig.tx_date,orig.collapsed,false,orig.client_source_raw_id,
	 orig.tx_time_guessed,orm.id,orig.tx_date_only,true,orig.recognition_provisional,orig.recognition_status
	 FROM bank_receipts orig JOIN raw_messages orm ON orm.id=orig.raw_message_id AND orm.deleted
	 JOIN raw_messages rm ON rm.id=$2
	 WHERE orig.id=$1 AND NOT orig.is_duplicate AND NOT orig.ignored AND orig.amount>0
	 AND `+deletedReceiptLegacyRawCopySQL, original, copy, conflict)
	return tag.RowsAffected() == 1, err
}

// RepairReceiptDuplicatesAfterDeletion repairs historical deletions whose
// surviving receipt was left marked as a duplicate. Keep every original row;
// promote at most one live copy, preserving its amount, client and author.
func (d *DB) RepairReceiptDuplicatesAfterDeletion(ctx context.Context, group string, since time.Time) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	// Serialize repairs for the same deleted sources before selecting a copy.
	// A second repair takes a new statement snapshot after these locks, and
	// therefore observes the first repair's promoted live receipt.
	rows, err := tx.Query(ctx, `SELECT DISTINCT orm.id FROM raw_messages orm
	 WHERE orm.deleted AND ($1='' OR orm.wa_group_jid=$1)
	 AND (EXISTS (SELECT 1 FROM bank_receipts orig
	 JOIN bank_receipts dd ON dd.is_duplicate AND NOT dd.ignored
	 JOIN raw_messages drm ON drm.id=dd.raw_message_id AND NOT drm.deleted
	 WHERE orig.raw_message_id=orm.id AND NOT orig.is_duplicate AND NOT orig.ignored
	 AND COALESCE(dd.group_jid,drm.wa_group_jid,'')=COALESCE(orig.group_jid,orm.wa_group_jid,'')
	 AND drm.received_at >= $2 AND `+deletedReceiptIdentitySQL("dd", "orig")+`)
	 OR EXISTS (SELECT 1 FROM bank_receipts orig JOIN raw_messages rm ON rm.id<>orm.id
	 WHERE orig.raw_message_id=orm.id AND NOT orig.is_duplicate AND NOT orig.ignored AND orig.amount>0
	 AND rm.received_at >= $2 AND `+deletedReceiptLegacyRawCopySQL+`))
	 ORDER BY orm.id`, group, since)
	if err != nil {
		return 0, err
	}
	var sources []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		sources = append(sources, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		return 0, tx.Commit(ctx)
	}
	// Lock originals in a stable order. The UPDATE in a live revoke takes the
	// same row lock, so no repair can race an unfinished source deletion.
	if _, err := tx.Exec(ctx, `SELECT id FROM raw_messages WHERE id=ANY($1) ORDER BY id FOR UPDATE`, sources); err != nil {
		return 0, err
	}
	var count int
	// Separate statements see a copy promoted for an earlier deleted source
	// with the same document/SHA, even when old rows recorded different links.
	for _, source := range sources {
		n, err := restoreDeletedReceiptCopies(ctx, tx, source, since, false)
		if err != nil {
			return 0, err
		}
		count += n
	}
	return count, tx.Commit(ctx)
}

// A live revoke and a historical repair lock the same source before choosing
// its replacement. Concurrent/repeated deletions cannot promote two copies.
func (d *DB) promoteDeletedReceiptCopy(ctx context.Context, rawID int) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var source int
	if err := tx.QueryRow(ctx, `SELECT id FROM raw_messages WHERE id=$1 AND deleted FOR UPDATE`, rawID).Scan(&source); err != nil {
		return 0, err
	}
	n, err := restoreDeletedReceiptCopies(ctx, tx, source, time.Time{}, true)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

func restoreDeletedReceiptCopies(ctx context.Context, tx pgx.Tx, source int, since time.Time, liveRevoke bool) (int, error) {
	identity := deletedReceiptIdentitySQL("dd", "orig")
	primaryMatch := `(` + deletedReceiptSamePhysicalSQL("live", "orig") + ` OR live.superseded_by_raw=orig.raw_message_id)`
	order := "drm.received_at,dd.id"
	if liveRevoke {
		order = "dd.created_at DESC,dd.id DESC"
		primaryMatch = `(` + primaryMatch + `
		 OR (live.client_confirmed AND orig.client_confirmed AND live.contact_id=orig.contact_id AND live.amount=orig.amount
		 AND COALESCE(live.doc_number,'')='' AND COALESCE(orig.doc_number,'')=''
		 AND COALESCE(live.auth_code,'')='' AND COALESCE(orig.auth_code,'')=''
		 AND ` + sameOpTimeSQL("live.tx_date", "live.tx_time_guessed", "orig.tx_date", "orig.tx_time_guessed") + `))`
	}

	// First use saved evidence. Recreate an exact SHA copy only when no saved
	// replacement survived. Legacy live heuristics are the final fallback.
	n, err := promoteReceiptCopies(ctx, tx, source, since, identity, primaryMatch, order)
	if err != nil || n > 0 {
		return n, err
	}
	restored, err := restoreLegacyDeletedReceiptCopy(ctx, tx, source, since, primaryMatch)
	if err != nil {
		return 0, err
	}
	if restored {
		return promoteReceiptCopies(ctx, tx, source, since, identity, primaryMatch, order)
	}
	if !liveRevoke {
		return 0, nil
	}

	// Keep the legacy live behavior for copies classified before duplicate
	// links were recorded, including tentative screens without operation time.
	legacyIdentity := `(dd.superseded_by_raw IS NULL AND (
	 ` + deletedReceiptAuthorizationSQL("dd") + `
	 OR (dd.contact_id=orig.contact_id AND dd.amount=orig.amount
	 AND ` + sameOpTimeSQL("dd.tx_date", "dd.tx_time_guessed", "orig.tx_date", "orig.tx_time_guessed") + `)
	 OR ` + printedMatchSQL("dd", "orig") + `
	 OR (dd.tx_time_guessed AND dd.client_confirmed AND orig.client_confirmed
	 AND dd.contact_id=orig.contact_id AND dd.amount=orig.amount
	 AND orm.received_at BETWEEN drm.received_at-interval '1 day' AND drm.received_at+interval '1 day'
	 AND (` + untrustedOpSQL("orig", "orm.received_at") + `
	 OR orig.tx_date BETWEEN drm.received_at-interval '3 days' AND drm.received_at+interval '2 minutes'))))`
	legacyPrimaryMatch := `(` + primaryMatch + ` OR ` + deletedReceiptAuthorizationSQL("live") + `)`
	return promoteReceiptCopies(ctx, tx, source, since, legacyIdentity, legacyPrimaryMatch, order)
}

// Authorization codes are short. Apply the same amount and operation window
// as the normal receipt matcher when using one as the only identifier.
func deletedReceiptAuthorizationSQL(copy string) string {
	auth, originalAuth := receiptDocumentSQL(copy+".auth_code"), receiptDocumentSQL("orig.auth_code")
	return `(` + auth + `<>'' AND ` + auth + `=` + originalAuth + `
	 AND ` + copy + `.amount=orig.amount
	 AND ` + copy + `.tx_date BETWEEN orig.tx_date-interval '3 days 5 minutes' AND orig.tx_date+interval '3 days 5 minutes')`
}

func promoteReceiptCopies(ctx context.Context, tx pgx.Tx, source int, since time.Time, identity, primaryMatch, order string) (int, error) {
	conflict := `(orig.client_confirmed AND dd.client_confirmed AND orig.contact_id IS DISTINCT FROM dd.contact_id)
	 OR (orig.amount>0 AND dd.amount>0 AND orig.amount<>dd.amount)
	 OR (` + receiptDocumentSQL("orig.doc_number") + `<>'' AND ` + receiptDocumentSQL("dd.doc_number") + `<>''
	 AND ` + receiptDocumentSQL("orig.doc_number") + `<>` + receiptDocumentSQL("dd.doc_number") + `)`
	rows, err := tx.Query(ctx, `UPDATE bank_receipts u SET is_duplicate=false,superseded_by_raw=NULL,same_check_parked=false,
	 needs_review=u.needs_review OR NOT u.client_confirmed OR u.contact_id IS NULL OR COALESCE(u.amount,0)<=0
	 OR u.recognition_provisional OR u.recognition_status<>'ok' OR sel.conflict
	 FROM (SELECT DISTINCT ON (orig.id) dd.id,(`+conflict+`) AS conflict
	 FROM bank_receipts orig JOIN raw_messages orm ON orm.id=orig.raw_message_id AND orm.deleted
	 JOIN bank_receipts dd ON dd.id<>orig.id AND dd.is_duplicate AND NOT dd.ignored
	 JOIN raw_messages drm ON drm.id=dd.raw_message_id AND NOT drm.deleted
	 WHERE orig.raw_message_id=$1 AND NOT orig.is_duplicate AND NOT orig.ignored
	 AND COALESCE(dd.group_jid,drm.wa_group_jid,'')=COALESCE(orig.group_jid,orm.wa_group_jid,'')
	 AND drm.received_at >= $2 AND `+identity+`
	 AND NOT EXISTS (SELECT 1 FROM bank_receipts live JOIN raw_messages lrm ON lrm.id=live.raw_message_id
	 WHERE NOT live.is_duplicate AND NOT live.ignored AND NOT lrm.deleted
	 AND COALESCE(live.group_jid,lrm.wa_group_jid,'')=COALESCE(orig.group_jid,orm.wa_group_jid,'')
	 AND `+primaryMatch+`)
	 ORDER BY orig.id,`+order+`) sel
	 WHERE u.id=sel.id AND u.is_duplicate AND NOT u.ignored
	 RETURNING u.raw_message_id`, source, since)
	if err != nil {
		return 0, err
	}
	var copies []int
	for rows.Next() {
		var copy int
		if err := rows.Scan(&copy); err != nil {
			rows.Close()
			return 0, err
		}
		copies = append(copies, copy)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, copy := range copies {
		if err := markPromotedReceiptDateConflict(ctx, tx, source, copy); err != nil {
			return 0, err
		}
		// Keep the other linked copies attached to the new primary.
		if _, err := tx.Exec(ctx, `UPDATE bank_receipts SET superseded_by_raw=$2
		 WHERE superseded_by_raw=$1 AND is_duplicate AND NOT ignored`, source, copy); err != nil {
			return 0, err
		}
	}
	return len(copies), nil
}

// Compare reliable operation days in the same Go time zone as ledger.Build.
// PostgreSQL's session zone can differ from the bot's configured local zone.
func markPromotedReceiptDateConflict(ctx context.Context, tx pgx.Tx, source, copy int) error {
	var originalDate, copyDate time.Time
	var originalGuessed, copyGuessed bool
	if err := tx.QueryRow(ctx, `SELECT orig.tx_date,orig.tx_time_guessed,cp.tx_date,cp.tx_time_guessed
	 FROM bank_receipts orig JOIN bank_receipts cp ON cp.raw_message_id=$2
	 WHERE orig.raw_message_id=$1 AND NOT orig.is_duplicate ORDER BY orig.id,cp.id LIMIT 1`, source, copy).
		Scan(&originalDate, &originalGuessed, &copyDate, &copyGuessed); err != nil {
		return err
	}
	if !originalGuessed && !copyGuessed && originalDate.In(time.Local).Format("2006-01-02") != copyDate.In(time.Local).Format("2006-01-02") {
		_, err := tx.Exec(ctx, `UPDATE bank_receipts SET needs_review=true WHERE raw_message_id=$1 AND NOT is_duplicate`, copy)
		return err
	}
	return nil
}
