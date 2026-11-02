package db

import (
	"context"
	"time"
)

// ReceiptRetry — чек, ожидающий повторного распознавания (зрение было недоступно).
type ReceiptRetry struct {
	ID        int
	RawID     int
	WaID      string
	Proto     []byte
	MediaPath string
	GroupJID  string
	Attempts  int
	// In-flight results apply only to this receipt/source snapshot. The
	// source fingerprint excludes local paths so redownloading is safe.
	ReceiptVersion string
	SourceVersion  string
}

const receiptRetrySourceVersion = `md5(jsonb_build_array(rm.edit_count, rm.body, rm.caption, encode(rm.media_proto, 'hex'), rm.handled_as)::text)`

const receiptRetryCols = `br.id, rm.id, COALESCE(rm.wa_message_id, ''), rm.media_proto,
       COALESCE(rm.media_path, ''), COALESCE(br.group_jid, rm.wa_group_jid, ''), br.recognition_attempts,
       br.xmin::text, ` + receiptRetrySourceVersion

func (d *DB) ReceiptRetryByID(ctx context.Context, id int) (ReceiptRetry, error) {
	var r ReceiptRetry
	err := d.pool.QueryRow(ctx, `SELECT `+receiptRetryCols+`
		FROM bank_receipts br JOIN raw_messages rm ON rm.id=br.raw_message_id
		WHERE br.id=$1 AND NOT br.ignored AND NOT COALESCE(rm.deleted,false)`, id).
		Scan(&r.ID, &r.RawID, &r.WaID, &r.Proto, &r.MediaPath, &r.GroupJID, &r.Attempts, &r.ReceiptVersion, &r.SourceVersion)
	return r, err
}

// MarkReceiptForRetry ставит чек(и) сообщения в очередь на повторное
// распознавание: зрение было недоступно, перечитаем позже, когда ИИ вернётся.
func (d *DB) MarkReceiptForRetry(ctx context.Context, rawMessageID int, errDetail string, nextAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			recognition_status     = 'pending',
			recognition_attempts   = 0,
			next_retry_at          = $2::timestamptz,
			last_recognition_error = $3,
			recognition_provisional = COALESCE(recognition_provisional,false) OR $3 = 'vision verdict unavailable'
		WHERE raw_message_id = $1 AND ignored = false AND recognition_status = 'ok'
	`, rawMessageID, nextAt, errDetail)
	return err
}

// DueReceiptRetries возвращает чеки, которым пора перечитаться (pending, срок
// подошёл, попыток меньше лимита), с путём к сохранённому изображению.
func (d *DB) DueReceiptRetries(ctx context.Context, now time.Time, maxAttempts, limit int) ([]ReceiptRetry, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := d.pool.Query(ctx, `
		SELECT `+receiptRetryCols+`
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.recognition_status = 'pending'
		  AND br.next_retry_at IS NOT NULL AND br.next_retry_at <= $1::timestamptz
		  AND br.recognition_attempts < $2
		  AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		ORDER BY br.next_retry_at ASC
		LIMIT $3
	`, now, maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptRetry
	for rows.Next() {
		var r ReceiptRetry
		if err := rows.Scan(&r.ID, &r.RawID, &r.WaID, &r.Proto, &r.MediaPath, &r.GroupJID, &r.Attempts, &r.ReceiptVersion, &r.SourceVersion); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecognizedReceipt — результат повторного распознавания для заполнения чека.
type RecognizedReceipt struct {
	Bank       string
	Recipient  string
	Sender     string
	DocNumber  string
	AuthCode   string
	Status     string
	Amount     float64
	Commission float64
	TxDate     time.Time
	HasTxDate  bool
	// TimeDefaulted — на чеке только дата, время в TxDate выбрано (полдень):
	// чек хранится как «только дата» (tx_date_only), время потом уточняется
	// по копии того же чека или тексту этого платежа.
	TimeDefaulted bool
	Declined      bool // банк перевод отклонил — денег нет, в сбор не идёт
}

// FillReceiptFromRetry заполняет чек данными, распознанными на повторе, и снимает
// его с очереди. Клиента, уже подтверждённого владельцем (client_confirmed), НЕ
// затираем — обновляем только банковские поля и сумму. needs_review снимаем лишь
// если есть и сумма, и подтверждённый клиент. Отклонённый банком перевод
// (Declined) в учёт не идёт: ignored, как при приёме чека. Свёрнутый экран
// перестаёт быть свёрнутым, только если повтор прочитал дату, получателя или
// номер документа.
func (d *DB) FillReceiptFromRetry(ctx context.Context, id int, r RecognizedReceipt) error {
	_, err := d.fillReceiptFromRetry(ctx, id, r, "", "")
	return err
}

// FillReceiptFromRetryIfCurrent rejects stale image reads after a correction,
// caption edit, another retry or a WhatsApp deletion during the network call.
func (d *DB) FillReceiptFromRetryIfCurrent(ctx context.Context, it ReceiptRetry, r RecognizedReceipt) (bool, error) {
	if it.ReceiptVersion == "" || it.SourceVersion == "" {
		return false, nil
	}
	return d.fillReceiptFromRetry(ctx, it.ID, r, it.ReceiptVersion, it.SourceVersion)
}

func (d *DB) fillReceiptFromRetry(ctx context.Context, id int, r RecognizedReceipt, receiptVersion, sourceVersion string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts AS br SET
			bank          = COALESCE(NULLIF($2,''), bank),
			recipient_raw = CASE WHEN client_confirmed THEN recipient_raw ELSE COALESCE(NULLIF($3,''), recipient_raw) END,
			sender_raw    = COALESCE(NULLIF($4,''), sender_raw),
			doc_number    = COALESCE(NULLIF($5,''), doc_number),
			auth_code     = COALESCE(NULLIF($6,''), auth_code),
			status        = COALESCE(NULLIF($7,''), status),
			-- Непроверенный OCR не является известной суммой: после настоящего
			-- consensus заменяем его. Активная ручная поправка сильнее модели.
			-- Для обычных известных сумм сохраняем прежнее поведение.
			amount        = CASE WHEN recognition_provisional AND EXISTS (
			                       SELECT 1 FROM amount_corrections ac
			                       WHERE ac.kind = 'receipt' AND ac.target_id = br.id AND NOT ac.reverted
			                     ) THEN amount
			                     WHEN recognition_provisional AND $8::numeric > 0 THEN $8::numeric
			                     WHEN amount > 0 THEN amount
			                     WHEN $8::numeric > 0 THEN $8::numeric ELSE amount END,
			commission    = CASE WHEN commission > 0 THEN commission
			                     WHEN $9::numeric > 0 THEN $9::numeric ELSE commission END,
			tx_date       = CASE WHEN $11 THEN $10::timestamptz ELSE tx_date END,
			tx_time_guessed = CASE WHEN $11 THEN false ELSE tx_time_guessed END,
			tx_date_only  = CASE WHEN $11 THEN $13 ELSE tx_date_only END,
			-- Свёрнутый экран «Перевод выполнен» остаётся свёрнутым, если и при
			-- повторе не видно ни даты, ни получателя, ни номера: иначе полный чек
			-- того же перевода его не заменит, и перевод посчитается дважды.
			collapsed     = collapsed AND NOT ($11 OR NULLIF($3,'') IS NOT NULL OR NULLIF($5,'') IS NOT NULL),
			recognition_status = 'ok',
			recognition_provisional = false,
			next_retry_at      = NULL,
			ignored       = ignored OR $12,
			needs_review  = CASE WHEN $12 THEN false
			                     WHEN (CASE WHEN $8::numeric > 0 THEN $8::numeric ELSE amount END) > 0
			                          AND client_confirmed AND contact_id IS NOT NULL
			                     THEN false ELSE true END
		WHERE id = $1
		  AND ($14 = '' OR (br.xmin::text = $14 AND NOT br.ignored))
		  AND ($15 = '' OR EXISTS (
		    SELECT 1 FROM raw_messages rm WHERE rm.id=br.raw_message_id
		      AND NOT COALESCE(rm.deleted,false) AND rm.handled_as IS NULL
		      AND `+receiptRetrySourceVersion+` = $15))
	`, id, r.Bank, r.Recipient, r.Sender, r.DocNumber, r.AuthCode, r.Status,
		r.Amount, r.Commission, r.TxDate, r.HasTxDate, r.Declined, r.TimeDefaulted, receiptVersion, sourceVersion)
	return tag.RowsAffected() > 0, err
}

// BumpReceiptRetry — попытка не удалась (зрение всё ещё недоступно): +1 попытка,
// новый срок; при исчерпании лимита помечаем 'failed' (останется needs_review для
// ручной проверки, но повторять перестаём).
func (d *DB) BumpReceiptRetry(ctx context.Context, id int, errDetail string, nextAt time.Time, maxAttempts int) error {
	return d.bumpReceiptRetry(ctx, id, errDetail, nextAt, maxAttempts, "", "")
}

func (d *DB) BumpReceiptRetryIfCurrent(ctx context.Context, it ReceiptRetry, errDetail string, nextAt time.Time, maxAttempts int) error {
	if it.ReceiptVersion == "" || it.SourceVersion == "" {
		return nil
	}
	return d.bumpReceiptRetry(ctx, it.ID, errDetail, nextAt, maxAttempts, it.ReceiptVersion, it.SourceVersion)
}

func (d *DB) bumpReceiptRetry(ctx context.Context, id int, errDetail string, nextAt time.Time, maxAttempts int, receiptVersion, sourceVersion string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts AS br SET
			recognition_attempts   = recognition_attempts + 1,
			last_recognition_error = $2,
			next_retry_at          = $3::timestamptz,
			recognition_status     = CASE WHEN recognition_attempts + 1 >= $4 THEN 'failed' ELSE 'pending' END
		WHERE id = $1
		  AND ($5 = '' OR (br.xmin::text = $5 AND NOT br.ignored))
		  AND ($6 = '' OR EXISTS (
		    SELECT 1 FROM raw_messages rm WHERE rm.id=br.raw_message_id
		      AND NOT COALESCE(rm.deleted,false) AND rm.handled_as IS NULL
		      AND `+receiptRetrySourceVersion+` = $6))
	`, id, errDetail, nextAt, maxAttempts, receiptVersion, sourceVersion)
	return err
}

// ResolveReceiptRetry снимает чек с очереди с указанным статусом (напр.
// 'not_receipt'); ignore=true дополнительно исключает его из учёта.
func (d *DB) ResolveReceiptRetry(ctx context.Context, id int, status string, ignore bool) error {
	return d.resolveReceiptRetry(ctx, id, status, ignore, "", "")
}

func (d *DB) ResolveReceiptRetryIfCurrent(ctx context.Context, it ReceiptRetry, status string, ignore bool) error {
	if it.ReceiptVersion == "" || it.SourceVersion == "" {
		return nil
	}
	return d.resolveReceiptRetry(ctx, it.ID, status, ignore, it.ReceiptVersion, it.SourceVersion)
}

func (d *DB) resolveReceiptRetry(ctx context.Context, id int, status string, ignore bool, receiptVersion, sourceVersion string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts AS br SET
			recognition_status = $2,
			next_retry_at      = NULL,
			ignored      = CASE WHEN $3 THEN true  ELSE ignored END,
			needs_review = CASE WHEN $3 THEN false ELSE needs_review END
		WHERE id = $1
		  AND ($4 = '' OR (br.xmin::text = $4 AND NOT br.ignored))
		  AND ($5 = '' OR EXISTS (
		    SELECT 1 FROM raw_messages rm WHERE rm.id=br.raw_message_id
		      AND NOT COALESCE(rm.deleted,false) AND rm.handled_as IS NULL
		      AND `+receiptRetrySourceVersion+` = $5))
	`, id, status, ignore, receiptVersion, sourceVersion)
	return err
}

// HoldMediaForReview keeps unsupported/ambiguous files visible without a
// repeated first-page-only recognition attempt.
func (d *DB) HoldMediaForReview(ctx context.Context, rawID int, reason string) (bool, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE raw_messages SET handled_as=$2,parsed=false,media_next_at=NULL
		WHERE id=$1 AND handled_as IS DISTINCT FROM $2`, rawID, reason)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE bank_receipts SET needs_review=true,recognition_status='failed'
		WHERE raw_message_id=$1 AND NOT ignored`, rawID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (d *DB) MediaReviewEvidence(ctx context.Context, rawID int) (body, caption string, err error) {
	err = d.pool.QueryRow(ctx, `SELECT COALESCE(body,''),COALESCE(caption,'') FROM raw_messages WHERE id=$1`, rawID).Scan(&body, &caption)
	return
}

// A later verified read may resolve this specific review reason without
// clearing unrelated holds (for example, an unsupported multipage PDF).
func (d *DB) ReleaseMediaReview(ctx context.Context, rawID int, reason string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET handled_as=NULL
		WHERE id=$1 AND handled_as=$2 AND NOT COALESCE(deleted,false)`, rawID, reason)
	return err
}
