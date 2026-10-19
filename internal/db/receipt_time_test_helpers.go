package db

import "context"

// TestReceiptTimeGuessed — время операции чека (по id сообщения WhatsApp)
// записано как угаданное, а не напечатанное (только для тестов).
func (d *DB) TestReceiptTimeGuessed(ctx context.Context, waMessageID string) bool {
	var guessed bool
	_ = d.pool.QueryRow(ctx, `
		SELECT br.tx_time_guessed
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 ORDER BY br.id DESC LIMIT 1`, waMessageID).Scan(&guessed)
	return guessed
}
