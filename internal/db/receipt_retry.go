package db

import (
	"context"
	"time"
)

// ReceiptRetry — чек, ожидающий повторного распознавания (зрение было недоступно).
type ReceiptRetry struct {
	ID        int
	MediaPath string
	GroupJID  string
	Attempts  int
}

// MarkReceiptForRetry ставит чек(и) сообщения в очередь на повторное
// распознавание: зрение было недоступно, перечитаем позже, когда ИИ вернётся.
func (d *DB) MarkReceiptForRetry(ctx context.Context, rawMessageID int, errDetail string, nextAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			recognition_status     = 'pending',
			recognition_attempts   = 0,
			next_retry_at          = $2::timestamptz,
			last_recognition_error = $3
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
		SELECT br.id, COALESCE(rm.media_path, ''), COALESCE(br.group_jid, rm.wa_group_jid, ''), br.recognition_attempts
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
		if err := rows.Scan(&r.ID, &r.MediaPath, &r.GroupJID, &r.Attempts); err != nil {
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
}

// FillReceiptFromRetry заполняет чек данными, распознанными на повторе, и снимает
// его с очереди. Клиента, уже подтверждённого владельцем (client_confirmed), НЕ
// затираем — обновляем только банковские поля и сумму. needs_review снимаем лишь
// если есть и сумма, и подтверждённый клиент.
func (d *DB) FillReceiptFromRetry(ctx context.Context, id int, r RecognizedReceipt) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			bank          = COALESCE(NULLIF($2,''), bank),
			recipient_raw = CASE WHEN client_confirmed THEN recipient_raw ELSE COALESCE(NULLIF($3,''), recipient_raw) END,
			sender_raw    = COALESCE(NULLIF($4,''), sender_raw),
			doc_number    = COALESCE(NULLIF($5,''), doc_number),
			auth_code     = COALESCE(NULLIF($6,''), auth_code),
			status        = COALESCE(NULLIF($7,''), status),
			-- Сумму заполняем ТОЛЬКО если её ещё нет (amount = 0). Никогда не
			-- перезаписываем уже известную сумму — иначе автоповтор мог бы откатить
			-- ручную правку владельца (manually supplied correction outranks AI).
			amount        = CASE WHEN amount > 0 THEN amount
			                     WHEN $8::numeric > 0 THEN $8::numeric ELSE amount END,
			commission    = CASE WHEN commission > 0 THEN commission
			                     WHEN $9::numeric > 0 THEN $9::numeric ELSE commission END,
			tx_date       = CASE WHEN $11 THEN $10::timestamptz ELSE tx_date END,
			collapsed     = false,
			recognition_status = 'ok',
			next_retry_at      = NULL,
			needs_review  = CASE WHEN (CASE WHEN $8::numeric > 0 THEN $8::numeric ELSE amount END) > 0
			                          AND client_confirmed AND contact_id IS NOT NULL
			                     THEN false ELSE true END
		WHERE id = $1
	`, id, r.Bank, r.Recipient, r.Sender, r.DocNumber, r.AuthCode, r.Status,
		r.Amount, r.Commission, r.TxDate, r.HasTxDate)
	return err
}

// BumpReceiptRetry — попытка не удалась (зрение всё ещё недоступно): +1 попытка,
// новый срок; при исчерпании лимита помечаем 'failed' (останется needs_review для
// ручной проверки, но повторять перестаём).
func (d *DB) BumpReceiptRetry(ctx context.Context, id int, errDetail string, nextAt time.Time, maxAttempts int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			recognition_attempts   = recognition_attempts + 1,
			last_recognition_error = $2,
			next_retry_at          = $3::timestamptz,
			recognition_status     = CASE WHEN recognition_attempts + 1 >= $4 THEN 'failed' ELSE 'pending' END
		WHERE id = $1
	`, id, errDetail, nextAt, maxAttempts)
	return err
}

// ResolveReceiptRetry снимает чек с очереди с указанным статусом (напр.
// 'not_receipt'); ignore=true дополнительно исключает его из учёта.
func (d *DB) ResolveReceiptRetry(ctx context.Context, id int, status string, ignore bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			recognition_status = $2,
			next_retry_at      = NULL,
			ignored      = CASE WHEN $3 THEN true  ELSE ignored END,
			needs_review = CASE WHEN $3 THEN false ELSE needs_review END
		WHERE id = $1
	`, id, status, ignore)
	return err
}
