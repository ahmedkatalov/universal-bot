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

	// Привязка чека к группе и (для дозагруженных через личку) к отправителю.
	// Старые строки дозаполняем группой из исходного сообщения.
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS group_jid TEXT`); err != nil {
		return fmt.Errorf("добавление колонки group_jid: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS submitted_by TEXT`); err != nil {
		return fmt.Errorf("добавление колонки submitted_by: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE bank_receipts br SET group_jid = rm.wa_group_jid
		FROM raw_messages rm
		WHERE br.group_jid IS NULL AND rm.id = br.raw_message_id
	`); err != nil {
		return fmt.Errorf("дозаполнение group_jid: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_receipts_group ON bank_receipts(group_jid)`); err != nil {
		return fmt.Errorf("создание индекса group_jid: %w", err)
	}

	// Поддержка удалённых сообщений и ручных корректировок: платёж/чек из
	// удалённого в WhatsApp сообщения исключается из отчётов; ignored —
	// ручное исключение через ассистента ("не считай тот чек на 5000").
	if _, err := pool.Exec(ctx, `ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS deleted BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки deleted: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS ignored BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.ignored: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS ignored BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки bank_receipts.ignored: %w", err)
	}

	// Починка ложных дублей: раньше дубль искался по ВСЕМ группам, из-за чего
	// чек, легально пересланный из группы СБ в основную, помечался дублем.
	// Снимаем флаг с чеков, у которых нет оригинала в ТОЙ ЖЕ группе.
	// Идемпотентно: у настоящих внутригрупповых дублей оригинал есть, флаг
	// остаётся. Запускается при каждом старте, на малых объёмах это дёшево.
	if _, err := pool.Exec(ctx, `
		UPDATE bank_receipts br SET is_duplicate = false
		WHERE br.is_duplicate = true
		  AND NOT EXISTS (
			SELECT 1 FROM bank_receipts o
			WHERE o.id <> br.id
			  AND o.is_duplicate = false
			  AND COALESCE(o.group_jid, '') = COALESCE(br.group_jid, '')
			  AND (
				(br.doc_number IS NOT NULL AND br.doc_number <> '' AND o.doc_number = br.doc_number) OR
				(br.auth_code IS NOT NULL AND br.auth_code <> '' AND o.auth_code = br.auth_code) OR
				(o.contact_id = br.contact_id AND o.amount = br.amount
				 AND abs(extract(epoch FROM o.tx_date - br.tx_date)) <= 300)
			  )
		  )
	`); err != nil {
		return fmt.Errorf("починка ложных дублей: %w", err)
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
	GroupJID     string    // группа, к которой относится чек (для дозагрузки из лички — целевая группа)
	SubmittedBy  string    // кто прислал чек; пусто для чеков из групп (там отправитель в raw_messages)
	TxDate       time.Time // время ОПЕРАЦИИ (с чека, если распозналось), не время получения сообщения
}

func (d *DB) InsertBankReceipt(ctx context.Context, r BankReceiptInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, sender_raw, contact_id, amount, commission, doc_number, auth_code, status, needs_review, is_duplicate, group_jid, submitted_by, tx_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, r.RawMessageID, nullIfEmpty(r.Bank), nullIfEmpty(r.RecipientRaw), nullIfEmpty(r.SenderRaw), r.ContactID, r.Amount, r.Commission,
		nullIfEmpty(r.DocNumber), nullIfEmpty(r.AuthCode), nullIfEmpty(r.Status), r.NeedsReview, r.IsDuplicate,
		nullIfEmpty(r.GroupJID), nullIfEmpty(r.SubmittedBy), r.TxDate)
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
//
// groupJID ограничивает проверку одной группой: рабочий процесс владельца —
// работники кидают чеки в одну группу, оттуда их пересылают в другую, и это
// НЕ дубль. Дубль — только повтор того же чека в той же группе. Пустой
// groupJID означает поиск по всем группам (используется для информационной
// проверки чеков, присланных в личку).
func (d *DB) FindDuplicateReceipt(ctx context.Context, groupJID, docNumber, authCode string, contactID *int, recipientRaw string, amount float64, txDate time.Time) (bool, time.Time, error) {
	// Оригиналом для проверки считается только ЖИВАЯ запись: не помеченная
	// сама дублем, не исключённая вручную и не из удалённого в WhatsApp
	// сообщения. Иначе чек, который уже убрали из учёта (удалили сообщение
	// или исключили через ассистента), продолжал бы блокировать повторную
	// отправку как "дубль".
	if docNumber != "" || authCode != "" {
		var existing time.Time
		err := d.pool.QueryRow(ctx, `
			SELECT br.tx_date FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE (($1 <> '' AND br.doc_number = $1) OR ($2 <> '' AND br.auth_code = $2))
			  AND ($3 = '' OR br.group_jid = $3)
			  AND br.is_duplicate = false
			  AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			ORDER BY br.tx_date
			LIMIT 1
		`, docNumber, authCode, groupJID).Scan(&existing)
		if err == nil {
			return true, existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, err
		}
	}

	var existing time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT br.tx_date FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.amount = $1
		  AND br.tx_date BETWEEN $2 AND $3
		  AND (
		    ($4::int IS NOT NULL AND br.contact_id = $4) OR
		    ($4::int IS NULL AND br.recipient_raw = $5)
		  )
		  AND ($6 = '' OR br.group_jid = $6)
		  AND br.is_duplicate = false
		  AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		ORDER BY br.tx_date
		LIMIT 1
	`, amount, txDate.Add(-DuplicateWindow), txDate.Add(DuplicateWindow), contactID, recipientRaw, groupJID).Scan(&existing)
	if err == nil {
		return true, existing, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	return false, time.Time{}, err
}

// UnresolvedReceipt — чек, получателя которого не удалось сопоставить
// с контактом при первичной обработке (needs_review = true).
type UnresolvedReceipt struct {
	ID           int
	RecipientRaw string
}

// UnresolvedReceipts возвращает чеки на ручной проверке, у которых есть
// распознанный получатель и сумма — кандидаты на повторное сопоставление
// после обновления логики алиасов или добавления новых алиасов в БД.
func (d *DB) UnresolvedReceipts(ctx context.Context) ([]UnresolvedReceipt, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, recipient_raw FROM bank_receipts
		WHERE needs_review = true AND recipient_raw IS NOT NULL AND amount > 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UnresolvedReceipt
	for rows.Next() {
		var r UnresolvedReceipt
		if err := rows.Scan(&r.ID, &r.RecipientRaw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AssignReceiptContact привязывает чек к контакту и снимает пометку
// ручной проверки — вызывается, когда получателя удалось сопоставить
// при повторном проходе.
func (d *DB) AssignReceiptContact(ctx context.Context, receiptID, contactID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET contact_id = $2, needs_review = false WHERE id = $1
	`, receiptID, contactID)
	return err
}

// MarkMessageDeleted помечает сообщение WhatsApp удалённым (пользователь
// удалил его в чате) и возвращает, сколько платежей и чеков было к нему
// привязано — они автоматически выпадают из всех отчётов.
func (d *DB) MarkMessageDeleted(ctx context.Context, waMessageID string) (txCount, receiptCount int, err error) {
	var rawID int
	err = d.pool.QueryRow(ctx, `
		UPDATE raw_messages SET deleted = true WHERE wa_message_id = $1 RETURNING id
	`, waMessageID).Scan(&rawID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil // сообщение не было у нас в базе (например, болтовня)
	}
	if err != nil {
		return 0, 0, err
	}
	if err = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transactions WHERE raw_message_id = $1 AND ignored = false`, rawID).Scan(&txCount); err != nil {
		return 0, 0, err
	}
	if err = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_receipts WHERE raw_message_id = $1 AND ignored = false AND is_duplicate = false`, rawID).Scan(&receiptCount); err != nil {
		return 0, 0, err
	}
	return txCount, receiptCount, nil
}

// OperationRef — ссылка на операцию (текстовый платёж или чек) для ручных
// корректировок через ассистента.
type OperationRef struct {
	Kind    string // "transaction" | "receipt"
	ID      int
	Name    string
	Amount  float64
	TxDate  time.Time
	Ignored bool
}

// FindOperations ищет операции по сумме с необязательными уточнениями:
// имя человека (подстрока), диапазон дат, группа. Возвращает до 10 последних —
// используется ассистентом, чтобы найти операцию, которую владелец просит
// исключить из учёта или вернуть обратно.
func (d *DB) FindOperations(ctx context.Context, amount float64, person string, from, to *time.Time, groupJID string) ([]OperationRef, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT * FROM (
			SELECT 'transaction' AS kind, t.id, COALESCE(c.canonical_name, t.raw_name) AS name,
			       t.amount::float8 AS amount, t.tx_date::timestamptz AS tx_date,
			       COALESCE(rm.wa_group_jid, '') AS group_jid, t.ignored
			FROM transactions t
			LEFT JOIN contacts c ON c.id = t.contact_id
			LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
			WHERE t.amount = $1 AND COALESCE(rm.deleted, false) = false

			UNION ALL

			SELECT 'receipt', br.id, COALESCE(c.canonical_name, br.recipient_raw, ''),
			       br.amount::float8, br.tx_date, COALESCE(br.group_jid, ''), br.ignored
			FROM bank_receipts br
			LEFT JOIN contacts c ON c.id = br.contact_id
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE br.amount = $1 AND br.is_duplicate = false AND COALESCE(rm.deleted, false) = false
		) ops
		WHERE ($2 = '' OR ops.name ILIKE '%' || $2 || '%')
		  AND ($3::timestamptz IS NULL OR ops.tx_date >= $3)
		  AND ($4::timestamptz IS NULL OR ops.tx_date < $4)
		  AND ($5 = '' OR ops.group_jid = $5)
		ORDER BY ops.tx_date DESC
		LIMIT 10
	`, amount, person, from, to, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OperationRef
	for rows.Next() {
		var op OperationRef
		var groupJID string
		if err := rows.Scan(&op.Kind, &op.ID, &op.Name, &op.Amount, &op.TxDate, &groupJID, &op.Ignored); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// SetOperationIgnored включает/выключает ручное исключение операции из учёта.
func (d *DB) SetOperationIgnored(ctx context.Context, kind string, id int, ignored bool) error {
	table := ""
	switch kind {
	case "transaction":
		table = "transactions"
	case "receipt":
		table = "bank_receipts"
	default:
		return fmt.Errorf("неизвестный тип операции %q", kind)
	}
	_, err := d.pool.Exec(ctx, `UPDATE `+table+` SET ignored = $2 WHERE id = $1`, id, ignored)
	return err
}

// ListContacts возвращает все каноничные имена из учёта — ассистент подмешивает
// их в промпт, чтобы исправлять опечатки в запросах ("ахмет каталов" -> "Ахмед").
func (d *DB) ListContacts(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT canonical_name FROM contacts ORDER BY canonical_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// PersonStats — сводка по одному человеку: сколько чеков на его имя (карту)
// и сколько текстовых платежей, с общими суммами и границами периода.
type PersonStats struct {
	Name         string
	ReceiptCount int
	ReceiptTotal float64
	PaymentCount int
	PaymentTotal float64
	FirstOp      *time.Time
	LastOp       *time.Time
}

// PersonReport ищет людей по подстроке имени (без учёта регистра) и для
// каждого считает чеки и текстовые платежи. from/to — необязательные границы
// периода, groupJID — необязательный фильтр по группе. До 5 совпадений.
func (d *DB) PersonReport(ctx context.Context, personQuery string, from, to *time.Time, groupJID string) ([]PersonStats, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.canonical_name,
			COALESCE(r.cnt, 0), COALESCE(r.total, 0),
			COALESCE(t.cnt, 0), COALESCE(t.total, 0),
			LEAST(r.first_op, t.first_op), GREATEST(r.last_op, t.last_op)
		FROM contacts c
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS cnt, SUM(br.amount) AS total,
			       MIN(br.tx_date) AS first_op, MAX(br.tx_date) AS last_op
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE br.contact_id = c.id
			  AND br.is_duplicate = false AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND ($2::timestamptz IS NULL OR br.tx_date >= $2)
			  AND ($3::timestamptz IS NULL OR br.tx_date < $3)
			  AND ($4 = '' OR br.group_jid = $4)
		) r ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS cnt, SUM(tx.amount) AS total,
			       MIN(tx.tx_date)::timestamptz AS first_op, MAX(tx.tx_date)::timestamptz AS last_op
			FROM transactions tx
			LEFT JOIN raw_messages rm ON rm.id = tx.raw_message_id
			WHERE tx.contact_id = c.id
			  AND tx.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND ($2::timestamptz IS NULL OR tx.tx_date >= $2)
			  AND ($3::timestamptz IS NULL OR tx.tx_date < $3)
			  AND ($4 = '' OR rm.wa_group_jid = $4)
		) t ON true
		WHERE c.canonical_name ILIKE '%' || $1 || '%'
		  AND (COALESCE(r.cnt, 0) > 0 OR COALESCE(t.cnt, 0) > 0)
		ORDER BY COALESCE(r.total, 0) + COALESCE(t.total, 0) DESC
		LIMIT 5
	`, personQuery, from, to, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PersonStats
	for rows.Next() {
		var p PersonStats
		if err := rows.Scan(&p.Name, &p.ReceiptCount, &p.ReceiptTotal, &p.PaymentCount, &p.PaymentTotal, &p.FirstOp, &p.LastOp); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
// groupJID — необязательный фильтр по группе: пустая строка = все группы.
//
// Один и тот же чек может легально лежать в двух группах (работники кидают
// в группу СБ, владельцы пересылают в основную) — при сводке по всем группам
// такие копии схлопываются в одну операцию через DISTINCT ON по номеру
// документа (а без него — по контакту+сумме+времени операции).
func (d *DB) SummaryForPeriod(ctx context.Context, from, to time.Time, groupJID string) ([]ContactSummary, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT c.canonical_name, t.card_to, t.amount
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE t.tx_date >= $1 AND t.tx_date < $2
		  AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND ($3 = '' OR rm.wa_group_jid = $3))

		UNION ALL

		(SELECT DISTINCT ON (COALESCE(NULLIF(br.doc_number, ''), br.contact_id::text || '|' || br.amount::text || '|' || br.tx_date::text))
			c.canonical_name, COALESCE(br.bank, 'банк не указан'), br.amount
		FROM bank_receipts br
		JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.needs_review = false
		  AND br.is_duplicate = false
		  AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.contact_id IS NOT NULL
		  AND ($3 = '' OR br.group_jid = $3)
		ORDER BY COALESCE(NULLIF(br.doc_number, ''), br.contact_id::text || '|' || br.amount::text || '|' || br.tx_date::text))
	`, from, to, groupJID)
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

// SenderStat — статистика по одному отправителю чеков: сколько чеков он
// прислал в группу и на какую общую сумму (это его "сбор").
type SenderStat struct {
	Name  string // имя в WhatsApp (pushname) или переопределение из submitted_by; "" если неизвестно
	Phone string // номер телефона (из JID отправителя)
	Count int
	Total float64
}

// SenderStats возвращает статистику по отправителям чеков за период [from, to):
// кто сколько чеков прислал и на какую сумму. Дубли не считаются.
// groupJID — фильтр по группе (пусто = все), senderQuery — фильтр по человеку:
// подстрока имени или номера телефона (пусто = все отправители).
func (d *DB) SenderStats(ctx context.Context, from, to time.Time, groupJID, senderQuery string) ([]SenderStat, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT s.sender_name, s.phone, COUNT(*), COALESCE(SUM(s.amount), 0)
		FROM (
			SELECT
				COALESCE(NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
				split_part(COALESCE(rm.sender_jid, ''), '@', 1) AS phone,
				br.amount
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE br.tx_date >= $1 AND br.tx_date < $2
			  AND br.is_duplicate = false
			  AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND br.amount > 0
			  AND ($3 = '' OR br.group_jid = $3)
		) s
		WHERE ($4 = '' OR s.sender_name ILIKE '%' || $4 || '%' OR s.phone LIKE '%' || $4 || '%')
		GROUP BY s.sender_name, s.phone
		ORDER BY 4 DESC
	`, from, to, groupJID, senderQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SenderStat
	for rows.Next() {
		var s SenderStat
		if err := rows.Scan(&s.Name, &s.Phone, &s.Count, &s.Total); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
