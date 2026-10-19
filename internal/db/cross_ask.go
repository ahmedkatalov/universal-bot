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
	// ReceivedAt — когда чек прислали (старый чек присылают и через неделю
	// после операции).
	ReceivedAt time.Time
	// SourceRawID — сообщение, которое назвало клиента (имя перед чеком, имя
	// следом, ответ); 0 — подпись к самому чеку или не назван.
	SourceRawID int
}

const crossCheckCols = `br.id, COALESCE(br.raw_message_id, 0), COALESCE(rm.wa_message_id, ''),
	COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''),
	(br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
	COALESCE(c.canonical_name, br.recipient_raw, ''),
	COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''), COALESCE(br.amount, 0)::float8, br.tx_date,
	br.cross_ask_msg_id IS NOT NULL, COALESCE(rm.received_at, br.tx_date), COALESCE(br.client_source_raw_id, 0)`

func scanCrossCheck(row pgx.Row) (CrossCheck, error) {
	var c CrossCheck
	err := row.Scan(&c.ID, &c.RawID, &c.WaID, &c.GroupJID, &c.SenderJID, &c.Named, &c.ContactID, &c.Client,
		&c.DocNumber, &c.AuthCode, &c.Amount, &c.TxDate, &c.Asked, &c.ReceivedAt, &c.SourceRawID)
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

// LatestCheckFrom — последний (по времени сообщения) живой чек, присланный
// senderJID в группу groupJID не раньше since.
func (d *DB) LatestCheckFrom(ctx context.Context, groupJID, senderJID string, since time.Time) (CrossCheck, error) {
	if groupJID == "" || senderJID == "" {
		return CrossCheck{}, nil
	}
	return scanCrossCheck(d.pool.QueryRow(ctx, `
		SELECT `+crossCheckCols+`
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND rm.sender_jid = $2
		  AND rm.received_at >= $3::timestamptz
		  AND br.is_duplicate = false AND br.ignored = false AND COALESCE(rm.deleted, false) = false
		ORDER BY rm.received_at DESC, br.id DESC LIMIT 1`, groupJID, senderJID, since))
}

// OpenPendingNameOf — имя из очереди «написали перед чеком», пришедшее
// сообщением rawID, ещё никому не досталось (и сообщение не удалено): id записи
// очереди. ok=false — его уже взял чек (или его не было).
func (d *DB) OpenPendingNameOf(ctx context.Context, rawID int) (id int, ok bool, err error) {
	if rawID <= 0 {
		return 0, false, nil
	}
	err = d.pool.QueryRow(ctx, `
		SELECT pn.id FROM pending_client_names pn
		WHERE pn.raw_message_id = $1 AND pn.consumed = false
		  AND NOT EXISTS (SELECT 1 FROM raw_messages rm WHERE rm.id = pn.raw_message_id AND rm.deleted)
		ORDER BY pn.id DESC LIMIT 1`, rawID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
