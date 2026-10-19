package db

import "context"

// TestReceiptTimeFlags — как записано время операции чека (по id сообщения
// WhatsApp): угадано (не с чека) и «на чеке только дата» (только для тестов).
func (d *DB) TestReceiptTimeFlags(ctx context.Context, waMessageID string) (guessed, dateOnly bool) {
	_ = d.pool.QueryRow(ctx, `
		SELECT br.tx_time_guessed, br.tx_date_only
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 ORDER BY br.id DESC LIMIT 1`, waMessageID).Scan(&guessed, &dateOnly)
	return guessed, dateOnly
}
