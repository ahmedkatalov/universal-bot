// Package db инкапсулирует работу с PostgreSQL: контакты, алиасы,
// сырые сообщения и транзакции.
package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool
}

func Connect(ctx context.Context, connString string) (*DB, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

// SaveRawMessage сохраняет сырое сообщение. Возвращает id записи.
// waMessageID используется для идемпотентности — при повторной доставке
// того же сообщения (whatsmeow иногда шлёт события повторно) запись не дублируется.
func (d *DB) SaveRawMessage(ctx context.Context, waMessageID, groupJID, senderJID, senderName, body string, hasMedia bool, mediaPath string, receivedAt time.Time) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_name, body, has_media, media_path, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (wa_message_id) DO UPDATE SET wa_message_id = EXCLUDED.wa_message_id
		RETURNING id
	`, waMessageID, groupJID, senderJID, senderName, body, hasMedia, mediaPath, receivedAt).Scan(&id)
	return id, err
}

func (d *DB) MarkMessageParsed(ctx context.Context, rawMessageID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET parsed = true WHERE id = $1`, rawMessageID)
	return err
}

// GetOrCreateContact возвращает id контакта по каноничному имени, создавая при отсутствии.
func (d *DB) GetOrCreateContact(ctx context.Context, canonicalName string) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO contacts (canonical_name) VALUES ($1)
		ON CONFLICT (canonical_name) DO UPDATE SET canonical_name = EXCLUDED.canonical_name
		RETURNING id
	`, canonicalName).Scan(&id)
	return id, err
}

// LoadAliases возвращает все пары (alias -> canonical_name) для инициализации AliasMap при старте.
func (d *DB) LoadAliases(ctx context.Context) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ca.alias, c.canonical_name
		FROM contact_aliases ca
		JOIN contacts c ON c.id = ca.contact_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var alias, canonical string
		if err := rows.Scan(&alias, &canonical); err != nil {
			return nil, err
		}
		result[alias] = canonical
	}
	return result, rows.Err()
}

type TransactionInput struct {
	ContactID    int
	RawName      string
	Amount       float64
	Note         string
	CardTo       string
	RawMessageID int
	TxDate       time.Time
}

func (d *DB) InsertTransaction(ctx context.Context, tx TransactionInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO transactions (contact_id, raw_name, amount, note, card_to, raw_message_id, tx_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, tx.ContactID, tx.RawName, tx.Amount, nullIfEmpty(tx.Note), nullIfEmpty(tx.CardTo), tx.RawMessageID, tx.TxDate)
	return err
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ---- Банковские чеки ----

type BankReceiptInput struct {
	RawMessageID int
	Bank         string
	RecipientRaw string
	SenderRaw    string
	ContactID    *int // nil, если получателя не удалось сопоставить с контактом
	Amount       float64
	Commission   float64
	DocNumber    string
	AuthCode     string
	Status       string
	NeedsReview  bool
	TxDate       time.Time
}

func (d *DB) InsertBankReceipt(ctx context.Context, r BankReceiptInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, sender_raw, contact_id, amount, commission, doc_number, auth_code, status, needs_review, tx_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, r.RawMessageID, nullIfEmpty(r.Bank), nullIfEmpty(r.RecipientRaw), nullIfEmpty(r.SenderRaw), r.ContactID, r.Amount, r.Commission,
		nullIfEmpty(r.DocNumber), nullIfEmpty(r.AuthCode), nullIfEmpty(r.Status), r.NeedsReview, r.TxDate)
	return err
}

// ---- Сводки ----

// ContactSummary — агрегированная сумма по одному контакту за период.
type ContactSummary struct {
	CanonicalName string
	Total         float64
	Count         int
	ByCard        map[string]float64 // сумма по картам/наличным внутри этого контакта
}

// SummaryForPeriod возвращает суммы по каждому контакту за период [from, to).
// Объединяет обе таблицы: обычные текстовые транзакции и банковские чеки
// (только те, где получатель уверенно сопоставлен с контактом и не требует
// ручной проверки — needs_review = false).
func (d *DB) SummaryForPeriod(ctx context.Context, from, to time.Time) ([]ContactSummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.canonical_name, t.card_to, t.amount
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		WHERE t.tx_date >= $1 AND t.tx_date < $2

		UNION ALL

		SELECT c.canonical_name, COALESCE(br.bank, 'банк не указан'), br.amount
		FROM bank_receipts br
		JOIN contacts c ON c.id = br.contact_id
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.needs_review = false
		  AND br.contact_id IS NOT NULL
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byName := map[string]*ContactSummary{}
	var order []string

	for rows.Next() {
		var name string
		var card *string
		var amount float64
		if err := rows.Scan(&name, &card, &amount); err != nil {
			return nil, err
		}
		cs, ok := byName[name]
		if !ok {
			cs = &ContactSummary{CanonicalName: name, ByCard: map[string]float64{}}
			byName[name] = cs
			order = append(order, name)
		}
		cs.Total += amount
		cs.Count++
		cardKey := "не указано"
		if card != nil && *card != "" {
			cardKey = *card
		}
		cs.ByCard[cardKey] += amount
	}

	result := make([]ContactSummary, 0, len(order))
	for _, name := range order {
		result = append(result, *byName[name])
	}
	return result, rows.Err()
}
