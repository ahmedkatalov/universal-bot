package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const paymentBatchSchema = `CREATE TABLE IF NOT EXISTS parsed_payment_batches (
	raw_message_id INT NOT NULL REFERENCES raw_messages(id) ON DELETE CASCADE,
	revision INT NOT NULL,
	phase TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	item_count INT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (raw_message_id, revision, phase)
)`

var ErrPaymentBatchChanged = errors.New("сохранённый разбор сообщения отличается от повторного; требуется проверка")
var ErrMessageRevisionChanged = errors.New("сообщение изменено или удалено во время разбора")

// ApplyParsedPaymentBatch writes a whole extraction phase atomically. The
// persisted token protects retries after a restart or an ambiguous COMMIT.
// Identical payments repeated inside the source remain separate rows.
func (d *DB) ApplyParsedPaymentBatch(ctx context.Context, rawID, revision int, phase string, inputs []TransactionInput) (int, error) {
	if len(inputs) == 0 {
		return 0, nil
	}
	normalized := append([]TransactionInput(nil), inputs...)
	for i := range normalized {
		in := &normalized[i]
		if in.RawMessageID != rawID || in.ContactID <= 0 || !validMoneyValue(in.Amount, false) {
			return 0, fmt.Errorf("неверная запись разбора сообщения")
		}
		local := in.TxDate.In(time.Local)
		// transactions.tx_date is a DATE: preserve the Moscow calendar day,
		// rather than converting its midnight to the preceding UTC date.
		in.TxDate = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(hash[:])
	// Probe repeats before acquiring a transaction connection. Calling pool
	// methods from inside a transaction deadlocks a pool limited to one slot.
	dups := make([]bool, len(normalized))
	for i, in := range normalized {
		if in.IsCash && in.DupCheck {
			if name := d.contactName(ctx, in.ContactID); !cashPlaceholder(name) {
				_, held, err := d.cashRepeatPrev(ctx, cashRepeatProbe{ContactID: in.ContactID, Name: name, Amount: in.Amount, TxDate: in.TxDate, RawID: rawID})
				if err != nil {
					return 0, err
				}
				dups[i] = held
			}
		}
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	var current int
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT edit_count,deleted FROM raw_messages WHERE id=$1 FOR UPDATE`, rawID).Scan(&current, &deleted); err != nil {
		return 0, err
	}
	if deleted || current != revision {
		return 0, ErrMessageRevisionChanged
	}
	var old string
	var count int
	err = tx.QueryRow(ctx, `SELECT fingerprint,item_count FROM parsed_payment_batches WHERE raw_message_id=$1 AND revision=$2 AND phase=$3`, rawID, revision, phase).Scan(&old, &count)
	if err == nil {
		if old != fingerprint {
			return 0, ErrPaymentBatchChanged
		}
		return count, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	// A deployment may encounter a partly written message from the old
	// per-row writer. Never append the full list on top of that unknown state.
	// Subsequent phases of an already tracked revision remain valid.
	var legacy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transactions WHERE raw_message_id=$1 AND NOT ignored)
		AND NOT EXISTS(SELECT 1 FROM parsed_payment_batches WHERE raw_message_id=$1 AND revision=$2)`, rawID, revision).Scan(&legacy); err != nil {
		return 0, err
	}
	if legacy {
		return 0, ErrPaymentBatchChanged
	}
	for i, in := range normalized {
		_, err = tx.Exec(ctx, `INSERT INTO transactions
			(contact_id,raw_name,amount,note,card_to,is_cash,collector,raw_message_id,tx_date,dup_pending,client_source_raw_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,0))`,
			in.ContactID, in.RawName, in.Amount, nullIfEmpty(in.Note), nullIfEmpty(in.CardTo), in.IsCash, nullIfEmpty(in.Collector), rawID, in.TxDate, dups[i], in.ClientSourceRawID)
		if err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO parsed_payment_batches(raw_message_id,revision,phase,fingerprint,item_count) VALUES($1,$2,$3,$4,$5)`, rawID, revision, phase, fingerprint, len(inputs)); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(inputs), nil
}
