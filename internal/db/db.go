// Package db инкапсулирует работу с PostgreSQL: контакты, алиасы,
// сырые сообщения и транзакции.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
	if err := runMigrations(ctx, pool); err != nil {
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}
	return &DB{pool: pool}, nil
}

// runMigrations доводит уже развёрнутую БД (созданную по старой schema.sql)
// до текущей схемы. Каждая миграция идемпотентна — безопасно гонять при
// каждом старте бота, в том числе на БД, которая уже мигрирована.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	// tx_date раньше был DATE (только дата, без времени операции). Теперь
	// нужно точное время — для отчётов "чеки за такое-то число" и для
	// детекта дублей. Меняем тип только если он ещё не TIMESTAMPTZ, чтобы
	// не перезаписывать таблицу на каждом рестарте бота без необходимости.
	var txDateType string
	err := pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'bank_receipts' AND column_name = 'tx_date'
	`).Scan(&txDateType)
	if err != nil {
		return fmt.Errorf("проверка типа tx_date: %w", err)
	}
	if txDateType != "timestamp with time zone" {
		if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ALTER COLUMN tx_date TYPE TIMESTAMPTZ USING tx_date::timestamptz`); err != nil {
			return fmt.Errorf("миграция tx_date в TIMESTAMPTZ: %w", err)
		}
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS is_duplicate BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки is_duplicate: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_receipts_duplicate ON bank_receipts(is_duplicate)`); err != nil {
		return fmt.Errorf("создание индекса is_duplicate: %w", err)
	}
	return nil
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
	IsDuplicate  bool
	TxDate       time.Time // время ОПЕРАЦИИ (с чека, если распозналось), не время получения сообщения
}

func (d *DB) InsertBankReceipt(ctx context.Context, r BankReceiptInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, sender_raw, contact_id, amount, commission, doc_number, auth_code, status, needs_review, is_duplicate, tx_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, r.RawMessageID, nullIfEmpty(r.Bank), nullIfEmpty(r.RecipientRaw), nullIfEmpty(r.SenderRaw), r.ContactID, r.Amount, r.Commission,
		nullIfEmpty(r.DocNumber), nullIfEmpty(r.AuthCode), nullIfEmpty(r.Status), r.NeedsReview, r.IsDuplicate, r.TxDate)
	return err
}

// DuplicateWindow — окно вокруг времени операции, в котором совпадение
// получателя и суммы считается вероятным повтором одного и того же чека
// (например, кто-то по ошибке переслал одно и то же фото дважды).
const DuplicateWindow = 5 * time.Minute

// FindDuplicateReceipt проверяет, не встречался ли уже такой же чек.
// Сначала — по точному совпадению номера документа/кода авторизации
// (это ID самой банковской операции, самый надёжный признак). Если их нет
// или совпадения не нашлось — по получателю, сумме и времени операции
// в пределах DuplicateWindow. Возвращает true и время найденного оригинала.
func (d *DB) FindDuplicateReceipt(ctx context.Context, docNumber, authCode string, contactID *int, recipientRaw string, amount float64, txDate time.Time) (bool, time.Time, error) {
	if docNumber != "" || authCode != "" {
		var existing time.Time
		err := d.pool.QueryRow(ctx, `
			SELECT tx_date FROM bank_receipts
			WHERE ($1 <> '' AND doc_number = $1) OR ($2 <> '' AND auth_code = $2)
			ORDER BY tx_date
			LIMIT 1
		`, docNumber, authCode).Scan(&existing)
		if err == nil {
			return true, existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, err
		}
	}

	var existing time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT tx_date FROM bank_receipts
		WHERE amount = $1
		  AND tx_date BETWEEN $2 AND $3
		  AND (
		    ($4::int IS NOT NULL AND contact_id = $4) OR
		    ($4::int IS NULL AND recipient_raw = $5)
		  )
		ORDER BY tx_date
		LIMIT 1
	`, amount, txDate.Add(-DuplicateWindow), txDate.Add(DuplicateWindow), contactID, recipientRaw).Scan(&existing)
	if err == nil {
		return true, existing, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	return false, time.Time{}, err
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
// (только те, где получатель уверенно сопоставлен с контактом, не требует
// ручной проверки — needs_review = false — и не помечен как дубль другого
// уже учтённого чека — is_duplicate = false).
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
		  AND br.is_duplicate = false
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
