package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Чек, на котором напечатана только ДАТА операции (Почта Банк и др.): время
// операции неизвестно. Хранится с датой с чека и временем, выбранным так,
// чтобы чек сходился с текстом того же платежа и со своими копиями
// (tx_date_only = true): время уже присланной копии того же напечатанного
// чека, иначе время текста этого платежа, написанного в тот день, иначе
// полдень.

// DateOnlyTwinTime — время, с которым уже записан тот же напечатанный чек с
// одной датой (копия в другой группе или повтор): та же сумма, тот же день
// операции, тот же получатель как напечатан, номера и плательщик не
// противоречат. Так копии одного чека хранятся с одним временем. excludeID —
// сам этот чек (0 — новый).
func (d *DB) DateOnlyTwinTime(ctx context.Context, excludeID int, p PrintedReceipt) (time.Time, bool, error) {
	if p.Amount <= 0 || p.Recipient == "" {
		return time.Time{}, false, nil
	}
	day := time.Date(p.TxDate.Year(), p.TxDate.Month(), p.TxDate.Day(), 0, 0, 0, 0, time.Local)
	var t time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT br.tx_date
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.tx_date_only AND br.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND br.id <> $8
		  AND br.amount = $1::numeric
		  AND br.tx_date >= $2::timestamptz AND br.tx_date < $3::timestamptz
		  AND `+printedNorm(printedRecipientOf("br"))+` <> ''
		  AND `+printedNorm(printedRecipientOf("br"))+` = `+printedNorm("$4::text")+`
		  AND (COALESCE(br.doc_number, '') = '' OR $5 = '' OR br.doc_number = $5)
		  AND (COALESCE(br.auth_code, '') = '' OR $6 = '' OR br.auth_code = $6)
		  AND (`+printedNorm("br.sender_raw")+` = '' OR `+printedNorm("$7::text")+` = ''
		       OR `+printedNorm("br.sender_raw")+` = `+printedNorm("$7::text")+`)
		  AND (`+printedNorm("br.bank")+` = '' OR `+printedNorm("$9::text")+` = ''
		       OR `+printedNorm("br.bank")+` = `+printedNorm("$9::text")+`)
		ORDER BY COALESCE(rm.received_at, br.created_at), br.id
		LIMIT 1`,
		p.Amount, day, day.AddDate(0, 0, 1), p.Recipient, p.DocNumber, p.AuthCode, p.Sender, excludeID, p.Bank).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// DayText — платёж переводом из текста, написанный в день операции чека.
type DayText struct {
	ID       int
	Client   string
	Group    string
	Sender   string
	PostedAt time.Time
}

// DayTransferTexts — живые платежи переводом (не наличкой) на сумму amount,
// написанные в день day не позже before, по времени присылки.
func (d *DB) DayTransferTexts(ctx context.Context, amount float64, day, before time.Time) ([]DayText, error) {
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	to := from.AddDate(0, 0, 1)
	if before.Before(to) {
		to = before
	}
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(c.canonical_name, t.raw_name, ''), COALESCE(rm.wa_group_jid, ''),
		       COALESCE(rm.sender_jid, ''), rm.received_at
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE abs(t.amount - $1::numeric) < 1 AND t.is_cash = false AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND rm.received_at >= $2::timestamptz AND rm.received_at <= $3::timestamptz
		ORDER BY rm.received_at, t.id`, amount, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayText
	for rows.Next() {
		var t DayText
		if err := rows.Scan(&t.ID, &t.Client, &t.Group, &t.Sender, &t.PostedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetReceiptOpTime — время операции чека id (дата та же: на чеке только дата).
func (d *DB) SetReceiptOpTime(ctx context.Context, id int, t time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET tx_date = $2 WHERE id = $1 AND tx_date_only`, id, t)
	return err
}

// ReceiptAnchorInfo — что нужно, чтобы выбрать время чека id с одной датой:
// клиент (названный), напечатанные поля и когда его прислали.
type ReceiptAnchorInfo struct {
	Client   string
	Printed  PrintedReceipt
	PostedAt time.Time
	DateOnly bool
}

// ReceiptAnchor — данные чека id для выбора времени операции.
func (d *DB) ReceiptAnchor(ctx context.Context, id int) (ReceiptAnchorInfo, error) {
	var a ReceiptAnchorInfo
	err := d.pool.QueryRow(ctx, `
		SELECT CASE WHEN br.client_confirmed THEN COALESCE(c.canonical_name, br.recipient_raw, '') ELSE '' END,
		       `+printedRecipientOf("br")+`, COALESCE(br.sender_raw, ''), COALESCE(br.bank, ''), COALESCE(br.doc_number, ''),
		       COALESCE(br.auth_code, ''), COALESCE(br.amount, 0)::float8, br.tx_date,
		       COALESCE(rm.received_at, br.created_at), br.tx_date_only
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.id = $1`, id).
		Scan(&a.Client, &a.Printed.Recipient, &a.Printed.Sender, &a.Printed.Bank, &a.Printed.DocNumber, &a.Printed.AuthCode,
			&a.Printed.Amount, &a.Printed.TxDate, &a.PostedAt, &a.DateOnly)
	return a, err
}
