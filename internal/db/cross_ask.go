package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Тот же чек (общий номер операции) подписан в двух группах разными клиентами:
// бот спрашивает «чей он?». Чек для этой проверки и сам вопрос — переживают
// перезапуск бота (вопрос хранится у чека, по которому спросили).

// CrossCheck — живой чек группы с номером операции/кодом авторизации.
type CrossCheck struct {
	Found     bool
	ID        int
	RawID     int
	WaID      string
	GroupJID  string
	SenderJID string
	Named     bool // клиента назвали (подпись, имя рядом, ответ)
	ContactID *int
	Client    string
	DocNumber string
	AuthCode  string
	Amount    float64
	TxDate    time.Time
	Asked     bool // по нему уже открыт вопрос «чей он?»
}

const crossCheckCols = `br.id, COALESCE(br.raw_message_id, 0), COALESCE(rm.wa_message_id, ''),
	COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''),
	(br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
	COALESCE(c.canonical_name, br.recipient_raw, ''),
	COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''), COALESCE(br.amount, 0)::float8, br.tx_date,
	br.cross_ask_msg_id IS NOT NULL`

func scanCrossCheck(row pgx.Row) (CrossCheck, error) {
	var c CrossCheck
	err := row.Scan(&c.ID, &c.RawID, &c.WaID, &c.GroupJID, &c.SenderJID, &c.Named, &c.ContactID, &c.Client,
		&c.DocNumber, &c.AuthCode, &c.Amount, &c.TxDate, &c.Asked)
	if errors.Is(err, pgx.ErrNoRows) {
		return CrossCheck{}, nil
	}
	if err != nil {
		return CrossCheck{}, err
	}
	c.Found = true
	return c, nil
}

// CrossCheckOf — живой (не повтор, не исключён, не удалён) чек сообщения waID
// или, если waID пусто, сообщения rawID.
func (d *DB) CrossCheckOf(ctx context.Context, waID string, rawID int) (CrossCheck, error) {
	if waID == "" && rawID <= 0 {
		return CrossCheck{}, nil
	}
	return scanCrossCheck(d.pool.QueryRow(ctx, `
		SELECT `+crossCheckCols+`
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE (($1 <> '' AND rm.wa_message_id = $1) OR ($1 = '' AND br.raw_message_id = $2))
		  AND br.is_duplicate = false AND br.ignored = false AND COALESCE(rm.deleted, false) = false
		ORDER BY br.id DESC LIMIT 1`, waID, rawID))
}

// SetCrossAsk — по чеку сообщения receiptWaID бот спросил askMsgID «чей он?».
func (d *DB) SetCrossAsk(ctx context.Context, receiptWaID, askMsgID string) error {
	if receiptWaID == "" || askMsgID == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET cross_ask_msg_id = $2
		WHERE id = (SELECT br.id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		            WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1)`, receiptWaID, askMsgID)
	return err
}

// CrossAskCheck — чек, по которому бот спросил askMsgID «чей он?» (вопрос ещё
// открыт). Found=false — такого вопроса нет или на него уже ответили.
func (d *DB) CrossAskCheck(ctx context.Context, askMsgID string) (CrossCheck, error) {
	if askMsgID == "" {
		return CrossCheck{}, nil
	}
	return scanCrossCheck(d.pool.QueryRow(ctx, `
		SELECT `+crossCheckCols+`
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.cross_ask_msg_id = $1
		  AND br.is_duplicate = false AND br.ignored = false AND COALESCE(rm.deleted, false) = false
		ORDER BY br.id DESC LIMIT 1`, askMsgID))
}

// ClearCrossAsk — на вопрос askMsgID ответили.
func (d *DB) ClearCrossAsk(ctx context.Context, askMsgID string) error {
	if askMsgID == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET cross_ask_msg_id = NULL WHERE cross_ask_msg_id = $1`, askMsgID)
	return err
}
