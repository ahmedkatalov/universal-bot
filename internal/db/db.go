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

	// Полные данные чека: карта/банк/телефон сторон — для справки и показа.
	for _, col := range []string{
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS recipient_bank TEXT",
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS recipient_phone TEXT",
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS sender_bank TEXT",
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS sender_account TEXT",
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS card_owner TEXT",
		// client_confirmed — клиент подтверждён (ФИО написали рядом или владелец
		// ответил на вопрос). clarify_asked — бот уже спросил "чей чек".
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS client_confirmed BOOLEAN NOT NULL DEFAULT false",
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS clarify_asked BOOLEAN NOT NULL DEFAULT false",
	} {
		if _, err := pool.Exec(ctx, col); err != nil {
			return fmt.Errorf("добавление колонки чека: %w", err)
		}
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

	// Правила пересылки чеков между чатами ("все чеки из X скидывай в Y").
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS forward_rules (
			id          SERIAL PRIMARY KEY,
			source      TEXT NOT NULL UNIQUE,
			source_name TEXT,
			target_jid  TEXT NOT NULL,
			target_name TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы forward_rules: %w", err)
	}

	// Сверка чеков с программой рассрочек (cmf): каждый чек из группы
	// становится "наблюдением" — бот следит, внесли ли платёж в программу,
	// и напоминает о забытых. bot_settings — простые настройки (например,
	// к какой точке относить чеки, не найденные в программе).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS cmf_watch (
			id             SERIAL PRIMARY KEY,
			raw_message_id INT REFERENCES raw_messages(id),
			group_jid      TEXT NOT NULL,
			sender_jid     TEXT,
			client_text    TEXT,          -- имя плательщика из подписи к чеку
			client_id      TEXT,          -- id клиента в cmf, когда сопоставлен
			client_name    TEXT,          -- полное имя клиента из cmf
			candidates     TEXT,          -- JSON кандидатов при неоднозначности
			amount         NUMERIC(12,2) NOT NULL,
			tx_date        TIMESTAMPTZ NOT NULL,
			status         TEXT NOT NULL DEFAULT 'noname',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
			checked_at     TIMESTAMPTZ
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы cmf_watch: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_cmf_watch_status ON cmf_watch(status)`); err != nil {
		return fmt.Errorf("индекс cmf_watch: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS bot_settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы bot_settings: %w", err)
	}

	// Память номеров: телефон (кто прислал чек) -> имя ответственного,
	// который "забрал" деньги. Редактируется владельцем через чат и
	// переживает рестарты.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS phone_owners (
			phone      TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы phone_owners: %w", err)
	}

	// История диалога ассистента: раньше жила только в памяти и стиралась при
	// каждом рестарте контейнера — бот «забывал» разговор. Теперь пишем в БД,
	// поэтому память переживает перезапуски. Ключ chat_key — JID отправителя
	// (личка) или JID группы (обращение по имени в группе).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS assistant_history (
			id         BIGSERIAL PRIMARY KEY,
			chat_key   TEXT NOT NULL,
			from_user  BOOLEAN NOT NULL,
			body       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы assistant_history: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_assistant_history_key ON assistant_history(chat_key, id)`); err != nil {
		return fmt.Errorf("индекс assistant_history: %w", err)
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
	RawMessageID    int
	Bank            string
	RecipientRaw    string // ФИО клиента (кому принадлежит чек — для атрибуции/поиска)
	RecipientBank   string
	RecipientPhone  string
	SenderRaw       string // ФИО отправителя (плательщик по чеку)
	SenderBank      string
	SenderAccount   string
	CardOwner       string // получатель, напечатанный на чеке (владелец карты), если отличается от клиента
	ContactID       *int   // nil, если получателя не удалось сопоставить с контактом
	Amount          float64
	Commission      float64
	DocNumber       string
	AuthCode        string
	Status          string
	NeedsReview     bool
	IsDuplicate     bool
	ClientConfirmed bool      // клиент известен точно (ФИО написали рядом с чеком)
	GroupJID        string    // группа, к которой относится чек (для дозагрузки из лички — целевая группа)
	SubmittedBy     string    // кто прислал чек; пусто для чеков из групп (там отправитель в raw_messages)
	TxDate          time.Time // время ОПЕРАЦИИ (с чека, если распозналось), не время получения сообщения
}

func (d *DB) InsertBankReceipt(ctx context.Context, r BankReceiptInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, recipient_bank, recipient_phone, sender_raw, sender_bank, sender_account,
			 card_owner, contact_id, amount, commission, doc_number, auth_code, status, needs_review, is_duplicate,
			 client_confirmed, group_jid, submitted_by, tx_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
	`, r.RawMessageID, nullIfEmpty(r.Bank), nullIfEmpty(r.RecipientRaw), nullIfEmpty(r.RecipientBank), nullIfEmpty(r.RecipientPhone),
		nullIfEmpty(r.SenderRaw), nullIfEmpty(r.SenderBank), nullIfEmpty(r.SenderAccount), nullIfEmpty(r.CardOwner),
		r.ContactID, r.Amount, r.Commission, nullIfEmpty(r.DocNumber), nullIfEmpty(r.AuthCode), nullIfEmpty(r.Status),
		r.NeedsReview, r.IsDuplicate, r.ClientConfirmed, nullIfEmpty(r.GroupJID), nullIfEmpty(r.SubmittedBy), r.TxDate)
	return err
}

// ClarifyReceipt — чек, по которому бот не уверен, чей это клиент, и хочет
// спросить в группе.
type ClarifyReceipt struct {
	ID          int
	WaMessageID string
	SenderJID   string
	CardOwner   string
	Amount      float64
	TxDate      time.Time
}

// UnconfirmedReceipts возвращает чеки без подтверждённого клиента, по которым
// бот ещё не спрашивал, старше olderThan, в группе. Если таких много (bulk-
// импорт) — вызывающий код может не спрашивать. rawMessageID нужен, чтобы
// потом привязать ответ к чеку по wa_message_id.
func (d *DB) UnconfirmedReceipts(ctx context.Context, groupJID string, olderThan time.Time, limit int) ([]ClarifyReceipt, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''),
		       COALESCE(br.card_owner, br.recipient_raw, ''), br.amount::float8, br.tx_date
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.client_confirmed = false AND br.clarify_asked = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		  AND br.created_at < $2
		ORDER BY br.created_at
		LIMIT $3
	`, groupJID, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClarifyReceipt
	for rows.Next() {
		var c ClarifyReceipt
		if err := rows.Scan(&c.ID, &c.WaMessageID, &c.SenderJID, &c.CardOwner, &c.Amount, &c.TxDate); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountUnconfirmed — сколько чеков без подтверждённого клиента в группе старше
// olderThan (чтобы отличить "запуталась в паре" от массового импорта).
func (d *DB) CountUnconfirmed(ctx context.Context, groupJID string, olderThan time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.client_confirmed = false AND br.clarify_asked = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
		  AND br.created_at < $2
	`, groupJID, olderThan).Scan(&n)
	return n, err
}

// MarkReceiptAsked помечает, что бот уже спросил про этот чек.
func (d *DB) MarkReceiptAsked(ctx context.Context, receiptID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET clarify_asked = true WHERE id = $1`, receiptID)
	return err
}

// ConfirmReceiptClientByMessage привязывает клиента к чеку (по id сообщения
// чека) — когда владелец ответил на вопрос бота. Возвращает сумму.
func (d *DB) ConfirmReceiptClientByMessage(ctx context.Context, waMessageID, name string, contactID *int) (bool, float64, error) {
	var amount float64
	err := d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET recipient_raw = $2, contact_id = $3, needs_review = false,
			client_confirmed = true, clarify_asked = true
		WHERE id = (
			SELECT br.id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1
		)
		RETURNING amount::float8
	`, waMessageID, name, contactID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, amount, nil
}

// DuplicateWindow — окно вокруг времени операции, в котором совпадение
// получателя и суммы считается вероятным повтором одного и того же чека
// (например, кто-то по ошибке переслал одно и то же фото дважды).
const DuplicateWindow = 5 * time.Minute

// FindDuplicateReceipt проверяет, не встречался ли уже такой же чек — по
// совокупности всех параметров сразу:
//   - получатель (сопоставленный контакт, а если он не определён — ФИО как в чеке);
//   - сумма (точное совпадение);
//   - время операции (в пределах DuplicateWindow);
//   - номер документа / код авторизации НЕ противоречат: либо совпадают, либо
//     у одного из чеков их нет. Разные номера = разные операции, поэтому два
//     реально разных перевода одному человеку на одну сумму дублем не станут,
//     даже если совпали по времени.
//
// Оригиналом считается только ЖИВАЯ запись: не помеченная сама дублем, не
// исключённая вручную и не из удалённого в WhatsApp сообщения — иначе чек,
// уже убранный из учёта, продолжал бы блокировать повторную отправку.
//
// groupJID ограничивает проверку одной группой: рабочий процесс владельца —
// работники кидают чеки в одну группу, оттуда их пересылают в другую, и это
// НЕ дубль. Пустой groupJID = поиск по всем группам (для информационной
// проверки чеков из лички).
func (d *DB) FindDuplicateReceipt(ctx context.Context, groupJID, docNumber, authCode string, contactID *int, recipientRaw string, amount float64, txDate time.Time) (bool, time.Time, error) {
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
		  -- номер документа не противоречит: совпадает, либо у одного из чеков его нет
		  AND (COALESCE(br.doc_number, '') = '' OR $7 = '' OR br.doc_number = $7)
		  -- код авторизации не противоречит
		  AND (COALESCE(br.auth_code, '') = '' OR $8 = '' OR br.auth_code = $8)
		ORDER BY br.tx_date
		LIMIT 1
	`, amount, txDate.Add(-DuplicateWindow), txDate.Add(DuplicateWindow), contactID, recipientRaw, groupJID, docNumber, authCode).Scan(&existing)
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

// ClearDuplicateFlag снимает пометку "дубль" с последнего чека, помеченного
// дублем, подходящего по сумме (и, если заданы, по имени/группе) — когда
// владелец говорит "это не дубль, засчитай". Возвращает данные снятого чека.
func (d *DB) ClearDuplicateFlag(ctx context.Context, amount float64, person, groupJID string) (found bool, name string, txDate time.Time, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET is_duplicate = false, ignored = false
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			LEFT JOIN contacts c ON c.id = br.contact_id
			WHERE br.is_duplicate = true
			  AND br.amount = $1
			  AND ($2 = '' OR COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $2 || '%')
			  AND ($3 = '' OR br.group_jid = $3)
			ORDER BY br.tx_date DESC
			LIMIT 1
		)
		RETURNING COALESCE((SELECT canonical_name FROM contacts WHERE id = bank_receipts.contact_id), recipient_raw, ''), tx_date
	`, amount, person, groupJID).Scan(&name, &txDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", time.Time{}, nil
	}
	if err != nil {
		return false, "", time.Time{}, err
	}
	return true, name, txDate, nil
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

// ForwardRule — правило пересылки чеков: все чеки из source идут в target.
// source — JID группы или 'dm' (личные чаты боту).
type ForwardRule struct {
	Source     string
	SourceName string
	TargetJID  string
	TargetName string
}

// SetForwardRule включает пересылку чеков из source в target (замещает
// существующее правило для этого источника).
func (d *DB) SetForwardRule(ctx context.Context, source, sourceName, targetJID, targetName string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO forward_rules (source, source_name, target_jid, target_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source) DO UPDATE SET target_jid = $3, target_name = $4
	`, source, sourceName, targetJID, targetName)
	return err
}

// DeleteForwardRule выключает пересылку из указанного источника. Пустой
// source удаляет ВСЕ правила. Возвращает, сколько правил удалено.
func (d *DB) DeleteForwardRule(ctx context.Context, source string) (int, error) {
	if source == "" {
		res, err := d.pool.Exec(ctx, `DELETE FROM forward_rules`)
		if err != nil {
			return 0, err
		}
		return int(res.RowsAffected()), nil
	}
	res, err := d.pool.Exec(ctx, `DELETE FROM forward_rules WHERE source = $1`, source)
	if err != nil {
		return 0, err
	}
	return int(res.RowsAffected()), nil
}

// ListForwardRules возвращает все активные правила пересылки.
func (d *DB) ListForwardRules(ctx context.Context) ([]ForwardRule, error) {
	rows, err := d.pool.Query(ctx, `SELECT source, COALESCE(source_name,''), target_jid, COALESCE(target_name,'') FROM forward_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForwardRule
	for rows.Next() {
		var r ForwardRule
		if err := rows.Scan(&r.Source, &r.SourceName, &r.TargetJID, &r.TargetName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnclearItem — "непонятый" элемент: чек, который не удалось уверенно
// разобрать (needs_review), либо фото/файл, из которого вообще не вышло
// извлечь операцию. Kind: "receipt" (строка bank_receipts) или "message"
// (сырое медиа-сообщение без единой распознанной операции).
type UnclearItem struct {
	Kind         string
	ID           int // id чека (receipt) или id raw-сообщения (message)
	GroupJID     string
	RecipientRaw string
	Amount       float64
	TxDate       time.Time
	MediaPath    string
	SenderName   string
}

// UnclearItems возвращает непонятые чеки и медиа-сообщения (свежие первыми).
// groupJID — необязательный фильтр по группе.
func (d *DB) UnclearItems(ctx context.Context, groupJID string, limit int) ([]UnclearItem, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT 'receipt', br.id, COALESCE(br.group_jid, ''), COALESCE(br.recipient_raw, ''),
		        COALESCE(br.amount, 0)::float8, br.tx_date, COALESCE(rm.media_path, ''),
		        COALESCE(NULLIF(rm.sender_name, ''), split_part(COALESCE(rm.sender_jid, ''), '@', 1), '')
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.needs_review = true AND br.ignored = false AND br.is_duplicate = false
		  AND COALESCE(rm.deleted, false) = false
		  AND ($1 = '' OR br.group_jid = $1))

		UNION ALL

		(SELECT 'message', rm.id, rm.wa_group_jid, '', 0, rm.received_at, COALESCE(rm.media_path, ''),
		        COALESCE(NULLIF(rm.sender_name, ''), split_part(rm.sender_jid, '@', 1))
		FROM raw_messages rm
		WHERE rm.has_media = true AND rm.deleted = false
		  AND rm.media_path IS NOT NULL AND rm.media_path <> ''
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id)
		  AND ($1 = '' OR rm.wa_group_jid = $1))

		ORDER BY 6 DESC
		LIMIT $2
	`, groupJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UnclearItem
	for rows.Next() {
		var it UnclearItem
		if err := rows.Scan(&it.Kind, &it.ID, &it.GroupJID, &it.RecipientRaw, &it.Amount, &it.TxDate, &it.MediaPath, &it.SenderName); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UnclearMediaPath возвращает путь к сохранённому файлу непонятого элемента.
func (d *DB) UnclearMediaPath(ctx context.Context, kind string, id int) (string, error) {
	var path *string
	var err error
	switch kind {
	case "receipt":
		err = d.pool.QueryRow(ctx, `
			SELECT rm.media_path FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE br.id = $1
		`, id).Scan(&path)
	case "message":
		err = d.pool.QueryRow(ctx, `SELECT media_path FROM raw_messages WHERE id = $1`, id).Scan(&path)
	default:
		return "", fmt.Errorf("неизвестный тип элемента %q", kind)
	}
	if err != nil {
		return "", err
	}
	if path == nil || *path == "" {
		return "", fmt.Errorf("файл для этой записи не сохранён")
	}
	return *path, nil
}

// FixReceipt применяет данные, продиктованные владельцем, к непонятому чеку:
// для kind="receipt" обновляет существующую запись, для kind="message"
// создаёт запись чека поверх сырого медиа-сообщения. Нулевые/пустые значения
// не трогают существующие поля.
func (d *DB) FixReceipt(ctx context.Context, kind string, id int, contactID *int, recipientName string, amount float64, txDate *time.Time) error {
	switch kind {
	case "receipt":
		_, err := d.pool.Exec(ctx, `
			UPDATE bank_receipts SET
				contact_id    = COALESCE($2, contact_id),
				recipient_raw = COALESCE(NULLIF($3, ''), recipient_raw),
				amount        = CASE WHEN $4 > 0 THEN $4 ELSE amount END,
				tx_date       = COALESCE($5, tx_date),
				needs_review  = false
			WHERE id = $1
		`, id, contactID, recipientName, amount, txDate)
		return err
	case "message":
		var groupJID string
		var receivedAt time.Time
		if err := d.pool.QueryRow(ctx, `SELECT wa_group_jid, received_at FROM raw_messages WHERE id = $1`, id).Scan(&groupJID, &receivedAt); err != nil {
			return fmt.Errorf("сообщение %d не найдено: %w", id, err)
		}
		when := receivedAt
		if txDate != nil {
			when = *txDate
		}
		_, err := d.pool.Exec(ctx, `
			INSERT INTO bank_receipts (raw_message_id, recipient_raw, contact_id, amount, needs_review, group_jid, tx_date)
			VALUES ($1, NULLIF($2, ''), $3, $4, false, $5, $6)
		`, id, recipientName, contactID, amount, groupJID, when)
		if err != nil {
			return err
		}
		_, err = d.pool.Exec(ctx, `UPDATE raw_messages SET parsed = true WHERE id = $1`, id)
		return err
	default:
		return fmt.Errorf("неизвестный тип элемента %q", kind)
	}
}

// UnparsedTextMessages возвращает текстовые сообщения из групп, которые так и
// не дали ни одной операции — кандидаты на повторный разбор ("пересчитай").
type UnparsedMessage struct {
	ID         int
	GroupJID   string
	SenderName string
	Body       string
	ReceivedAt time.Time
}

func (d *DB) UnparsedTextMessages(ctx context.Context, limit int) ([]UnparsedMessage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, rm.wa_group_jid, COALESCE(rm.sender_name, ''), COALESCE(rm.body, ''), rm.received_at
		FROM raw_messages rm
		WHERE rm.parsed = false AND rm.has_media = false AND rm.deleted = false
		  AND COALESCE(rm.body, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id)
		ORDER BY rm.received_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UnparsedMessage
	for rows.Next() {
		var m UnparsedMessage
		if err := rows.Scan(&m.ID, &m.GroupJID, &m.SenderName, &m.Body, &m.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecentReceiptsOutsidePeriod — чеки, ДОБАВЛЕННЫЕ недавно (created_at >=
// createdAfter, т.е. "эти, которые только что скинули"), но с датой ОПЕРАЦИИ
// вне запрошенного периода [from,to). Нужны, чтобы отчёт явно отметил, какие
// чеки не вошли в сумму из-за даты на самом чеке.
func (d *DB) RecentReceiptsOutsidePeriod(ctx context.Context, from, to, createdAfter time.Time, groupJIDs []string, limit int) ([]LedgerReceipt, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date, COALESCE(br.group_jid,'')
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.created_at >= $3
		  AND (br.tx_date < $1 OR br.tx_date >= $2)
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		  AND COALESCE(c.canonical_name, br.recipient_raw, '') <> ''
		  AND ($4::text[] IS NULL OR br.group_jid = ANY($4))
		ORDER BY br.tx_date DESC
		LIMIT $5
	`, from, to, createdAfter, groupSliceArg(groupJIDs), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerReceipt
	for rows.Next() {
		var r LedgerReceipt
		if err := rows.Scan(&r.Name, &r.Amount, &r.TxDate, &r.GroupJID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LedgerReceipt — распознанный чек из учёта для живой сверки с программой.
type LedgerReceipt struct {
	ID       int
	Name     string // каноничное имя получателя (или recipient_raw)
	Amount   float64
	TxDate   time.Time
	GroupJID string
}

// ReceiptsForPeriod возвращает распознанные банковские чеки за период
// [from, to) — не дубли, не исключённые, из неудалённых сообщений,
// с суммой и получателем. groupJID — необязательный фильтр по группе.
func (d *DB) ReceiptsForPeriod(ctx context.Context, from, to time.Time, groupJID string) ([]LedgerReceipt, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date, COALESCE(br.group_jid,'')
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		  AND COALESCE(c.canonical_name, br.recipient_raw, '') <> ''
		  AND ($3 = '' OR br.group_jid = $3)
		ORDER BY br.tx_date DESC
	`, from, to, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerReceipt
	for rows.Next() {
		var r LedgerReceipt
		if err := rows.Scan(&r.ID, &r.Name, &r.Amount, &r.TxDate, &r.GroupJID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasUnconfirmedReceiptFrom — есть ли свежий чек БЕЗ подтверждённого клиента
// от этого отправителя, ждущий имени. Так отличаем "имя после чека" (есть
// ждущий чек -> привязать) от "имя перед чеком" (нет -> запомнить в очередь).
func (d *DB) HasUnconfirmedReceiptFrom(ctx context.Context, groupJID, senderJID string, since time.Time) (bool, error) {
	var one int
	err := d.pool.QueryRow(ctx, `
		SELECT 1 FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND rm.sender_jid = $2
		  AND br.created_at > $3
		  AND br.client_confirmed = false
		  AND br.is_duplicate = false AND br.ignored = false
		LIMIT 1
	`, groupJID, senderJID, since).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReattributeOldestUnconfirmedReceipt привязывает клиента к САМОМУ СТАРОМУ
// неподтверждённому чеку от отправителя (FIFO — по порядку сообщений в чате).
func (d *DB) ReattributeOldestUnconfirmedReceipt(ctx context.Context, groupJID, senderJID string, since time.Time, name string, contactID *int) (found bool, amount float64, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET recipient_raw = $4, contact_id = $5, needs_review = false, client_confirmed = true
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
			  AND rm.sender_jid = $2
			  AND br.created_at > $3
			  AND br.client_confirmed = false
			  AND br.is_duplicate = false AND br.ignored = false
			ORDER BY br.created_at ASC
			LIMIT 1
		)
		RETURNING amount::float8
	`, groupJID, senderJID, since, name, contactID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, amount, nil
}

// ReattributeReceiptByMessage переписывает получателя у чека, на который
// ответили (свайп), — по id сообщения WhatsApp.
func (d *DB) ReattributeReceiptByMessage(ctx context.Context, waMessageID, name string, contactID *int) (found bool, amount float64, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET recipient_raw = $2, contact_id = $3, needs_review = false, client_confirmed = true
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false
			ORDER BY br.id DESC LIMIT 1
		)
		RETURNING amount::float8
	`, waMessageID, name, contactID).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, amount, nil
}

// ReceiptDetail — полные данные чека для показа владельцу.
type ReceiptDetail struct {
	Client         string // клиент, кому принадлежит чек
	CardOwner      string // получатель на чеке (владелец карты)
	RecipientBank  string
	RecipientPhone string
	Sender         string
	SenderBank     string
	SenderAccount  string
	Bank           string
	Amount         float64
	Commission     float64
	DocNumber      string
	AuthCode       string
	Status         string
	TxDate         time.Time
}

// ReceiptDetailsForPerson возвращает полные данные чеков клиента (по имени),
// свежие первыми — для ответа "покажи все данные чека Цихаева".
func (d *DB) ReceiptDetailsForPerson(ctx context.Context, personQuery string, limit int) ([]ReceiptDetail, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(c.canonical_name, br.recipient_raw, ''), COALESCE(br.card_owner, ''),
		       COALESCE(br.recipient_bank, ''), COALESCE(br.recipient_phone, ''),
		       COALESCE(br.sender_raw, ''), COALESCE(br.sender_bank, ''), COALESCE(br.sender_account, ''),
		       COALESCE(br.bank, ''), br.amount::float8, COALESCE(br.commission, 0)::float8,
		       COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''), COALESCE(br.status, ''), br.tx_date
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $1 || '%'
		ORDER BY br.tx_date DESC
		LIMIT $2
	`, personQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptDetail
	for rows.Next() {
		var r ReceiptDetail
		if err := rows.Scan(&r.Client, &r.CardOwner, &r.RecipientBank, &r.RecipientPhone, &r.Sender, &r.SenderBank,
			&r.SenderAccount, &r.Bank, &r.Amount, &r.Commission, &r.DocNumber, &r.AuthCode, &r.Status, &r.TxDate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReceiptFile — сохранённый файл чека для отправки по запросу.
type ReceiptFile struct {
	Name      string
	Amount    float64
	TxDate    time.Time
	MediaPath string
}

// ReceiptFilesForPerson находит сохранённые файлы чеков конкретного человека
// (по подстроке имени получателя/контакта) за необязательный период —
// чтобы бот мог "скинуть чек Миланы". Свежие первыми, до limit штук.
func (d *DB) ReceiptFilesForPerson(ctx context.Context, personQuery string, from, to *time.Time, groupJID string, limit int) ([]ReceiptFile, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date, rm.media_path
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND rm.media_path IS NOT NULL AND rm.media_path <> ''
		  AND COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $1 || '%'
		  AND ($2::timestamptz IS NULL OR br.tx_date >= $2)
		  AND ($3::timestamptz IS NULL OR br.tx_date < $3)
		  AND ($4 = '' OR br.group_jid = $4)
		ORDER BY br.tx_date DESC
		LIMIT $5
	`, personQuery, from, to, groupJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptFile
	for rows.Next() {
		var f ReceiptFile
		var path *string
		if err := rows.Scan(&f.Name, &f.Amount, &f.TxDate, &path); err != nil {
			return nil, err
		}
		if path != nil {
			f.MediaPath = *path
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- Настройки бота ----

// SettingGet возвращает значение настройки или "" если её нет.
func (d *DB) SettingGet(ctx context.Context, key string) (string, error) {
	var v string
	err := d.pool.QueryRow(ctx, `SELECT value FROM bot_settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SettingSet сохраняет настройку.
func (d *DB) SettingSet(ctx context.Context, key, value string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bot_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = $2
	`, key, value)
	return err
}

// ---- Память номеров (кто "забрал" деньги) ----

// PhoneOwner — сопоставление телефона с ответственным.
type PhoneOwner struct {
	Phone string
	Name  string
}

// SetPhoneOwner сохраняет/меняет владельца номера (навсегда).
func (d *DB) SetPhoneOwner(ctx context.Context, phone, name string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO phone_owners (phone, name) VALUES ($1, $2)
		ON CONFLICT (phone) DO UPDATE SET name = $2, updated_at = now()
	`, phone, name)
	return err
}

// RemovePhoneOwner убирает номер из памяти.
func (d *DB) RemovePhoneOwner(ctx context.Context, phone string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM phone_owners WHERE phone = $1`, phone)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetReceiptCollectorByMessage привязывает чек (по id сообщения WhatsApp)
// к ответственному, который "забрал" деньги — заполняет submitted_by.
// Возвращает данные чека для подтверждения.
func (d *DB) SetReceiptCollectorByMessage(ctx context.Context, waMessageID, collector string) (found bool, amount float64, recipient string, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET submitted_by = $2
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1
			ORDER BY br.id DESC LIMIT 1
		)
		RETURNING amount::float8, COALESCE(recipient_raw, '')
	`, waMessageID, collector).Scan(&amount, &recipient)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", nil
	}
	if err != nil {
		return false, 0, "", err
	}
	return true, amount, recipient, nil
}

// SetReceiptCollectorLatest привязывает к ответственному последний чек в группе
// (по желанию — с конкретной суммой). Для команды "запиши этот чек на X",
// сказанной сразу после отправки чека.
func (d *DB) SetReceiptCollectorLatest(ctx context.Context, groupJID, collector string, amount float64) (found bool, gotAmount float64, recipient string, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET submitted_by = $2
		WHERE id = (
			SELECT id FROM bank_receipts
			WHERE group_jid = $1 AND is_duplicate = false AND ignored = false
			  AND ($3 <= 0 OR amount = $3)
			ORDER BY created_at DESC LIMIT 1
		)
		RETURNING amount::float8, COALESCE(recipient_raw, '')
	`, groupJID, collector, amount).Scan(&gotAmount, &recipient)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", nil
	}
	if err != nil {
		return false, 0, "", err
	}
	return true, gotAmount, recipient, nil
}

// ListPhoneOwners возвращает всю память номеров.
func (d *DB) ListPhoneOwners(ctx context.Context) ([]PhoneOwner, error) {
	rows, err := d.pool.Query(ctx, `SELECT phone, name FROM phone_owners ORDER BY name, phone`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PhoneOwner
	for rows.Next() {
		var p PhoneOwner
		if err := rows.Scan(&p.Phone, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Сверка чеков с программой рассрочек (cmf) ----

// CmfWatch — наблюдение за чеком: внесли ли платёж в программу.
// Статусы: noname (нет имени плательщика), watch (клиент найден, ждём платёж
// в программе), ambiguous (несколько кандидатов, ждём уточнения), unmatched
// (клиент не найден в программе), found (платёж внесён), reminded (напомнили,
// платёж так и не внесён), dismissed (закрыто вручную).
type CmfWatch struct {
	ID         int
	GroupJID   string
	SenderJID  string
	ClientText string
	ClientID   string
	ClientName string
	Candidates string
	Amount     float64
	TxDate     time.Time
	Status     string
	CreatedAt  time.Time
}

// InsertCmfWatch создаёт наблюдение, возвращает id.
func (d *DB) InsertCmfWatch(ctx context.Context, rawMessageID int, groupJID, senderJID, clientText string, amount float64, txDate time.Time, status string) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO cmf_watch (raw_message_id, group_jid, sender_jid, client_text, amount, tx_date, status)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7)
		RETURNING id
	`, rawMessageID, groupJID, senderJID, clientText, amount, txDate, status).Scan(&id)
	return id, err
}

// UpdateCmfWatch обновляет поля наблюдения (пустые значения не трогают).
func (d *DB) UpdateCmfWatch(ctx context.Context, id int, clientText, clientID, clientName, candidates, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE cmf_watch SET
			client_text = COALESCE(NULLIF($2, ''), client_text),
			client_id   = COALESCE(NULLIF($3, ''), client_id),
			client_name = COALESCE(NULLIF($4, ''), client_name),
			candidates  = COALESCE(NULLIF($5, ''), candidates),
			status      = COALESCE(NULLIF($6, ''), status),
			checked_at  = now()
		WHERE id = $1
	`, id, clientText, clientID, clientName, candidates, status)
	return err
}

// ListCmfWatches возвращает наблюдения в указанных статусах (свежие первыми).
func (d *DB) ListCmfWatches(ctx context.Context, statuses []string, limit int) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, group_jid, COALESCE(sender_jid,''), COALESCE(client_text,''), COALESCE(client_id,''),
		       COALESCE(client_name,''), COALESCE(candidates,''), amount::float8, tx_date, status, created_at
		FROM cmf_watch
		WHERE status = ANY($1)
		ORDER BY created_at DESC
		LIMIT $2
	`, statuses, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfWatch
	for rows.Next() {
		var w CmfWatch
		if err := rows.Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.ClientID, &w.ClientName,
			&w.Candidates, &w.Amount, &w.TxDate, &w.Status, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DueCmfWatches — наблюдения со статусом watch, созданные раньше cutoff:
// пора проверить, внесён ли платёж в программу.
func (d *DB) DueCmfWatches(ctx context.Context, cutoff time.Time, limit int) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, group_jid, COALESCE(sender_jid,''), COALESCE(client_text,''), COALESCE(client_id,''),
		       COALESCE(client_name,''), COALESCE(candidates,''), amount::float8, tx_date, status, created_at
		FROM cmf_watch
		WHERE status = 'watch' AND created_at < $1
		ORDER BY created_at
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfWatch
	for rows.Next() {
		var w CmfWatch
		if err := rows.Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.ClientID, &w.ClientName,
			&w.Candidates, &w.Amount, &w.TxDate, &w.Status, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// LatestNonameWatch — последнее наблюдение без имени от этого отправителя в
// этой группе (для привязки имени, присланного отдельным сообщением).
func (d *DB) LatestNonameWatch(ctx context.Context, groupJID, senderJID string, since time.Time) (int, bool, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		SELECT id FROM cmf_watch
		WHERE group_jid = $1 AND sender_jid = $2 AND status = 'noname' AND created_at > $3
		ORDER BY created_at DESC
		LIMIT 1
	`, groupJID, senderJID, since).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
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
// groupSliceArg превращает список групп в аргумент для SQL ANY($n::text[]):
// nil/пусто -> nil (значит "все группы", условие пропускается).
func groupSliceArg(groupJIDs []string) any {
	if len(groupJIDs) == 0 {
		return nil
	}
	return groupJIDs
}

// groupJIDs — необязательный список групп (nil/пусто = все группы).
func (d *DB) SummaryForPeriod(ctx context.Context, from, to time.Time, groupJIDs []string) ([]ContactSummary, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT c.canonical_name, t.card_to, t.amount
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE t.tx_date >= $1 AND t.tx_date < $2
		  AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3)))

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
		  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
		ORDER BY COALESCE(NULLIF(br.doc_number, ''), br.contact_id::text || '|' || br.amount::text || '|' || br.tx_date::text))
	`, from, to, groupSliceArg(groupJIDs))
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

// CardTotal — сколько перевели на карту конкретного получателя.
type CardTotal struct {
	CardOwner string // чья карта получила деньги (ФИО получателя с чека)
	Bank      string // банк получателя, если распознан
	Count     int
	Total     float64
}

// CardTotals — разбивка «на чьи карты сколько перевели» за период. Владелец
// карты = получатель, напечатанный на чеке (card_owner, а если нет — recipient_raw).
// Не путать с клиентом рассрочки: сюда идёт именно владелец карты-получателя.
func (d *DB) CardTotals(ctx context.Context, from, to time.Time, groupJIDs []string) ([]CardTotal, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(br.card_owner, ''), NULLIF(br.recipient_raw, ''), 'не распознано') AS card,
		       COALESCE(NULLIF(br.recipient_bank, ''), NULLIF(br.bank, ''), '') AS bank,
		       COUNT(*), COALESCE(SUM(br.amount), 0)
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
		GROUP BY card, bank
		ORDER BY 4 DESC
	`, from, to, groupSliceArg(groupJIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardTotal
	for rows.Next() {
		var c CardTotal
		if err := rows.Scan(&c.CardOwner, &c.Bank, &c.Count, &c.Total); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SenderStats возвращает статистику по отправителям чеков за период [from, to):
// кто сколько чеков прислал и на какую сумму. Дубли не считаются.
// groupJID — фильтр по группе (пусто = все), senderQuery — фильтр по человеку:
// подстрока имени или номера телефона (пусто = все отправители).
func (d *DB) SenderStats(ctx context.Context, from, to time.Time, groupJIDs []string, senderQuery string) ([]SenderStat, error) {
	// "Сбор" по ответственному включает И чеки (bank_receipts), И наличку/
	// текстовые платежи (transactions) — всё, что человек собрал. Имя берём
	// в приоритете из памяти номеров (phone_owners по телефону отправителя),
	// затем из submitted_by/pushname.
	rows, err := d.pool.Query(ctx, `
		SELECT s.sender_name, s.phone, COUNT(*), COALESCE(SUM(s.amount), 0)
		FROM (
			SELECT
				COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
				split_part(COALESCE(rm.sender_jid, ''), '@', 1) AS phone,
				br.amount
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(rm.sender_jid, ''), '@', 1)
			WHERE br.tx_date >= $1 AND br.tx_date < $2
			  AND br.is_duplicate = false
			  AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND br.amount > 0
			  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))

			UNION ALL

			SELECT
				COALESCE(NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
				split_part(COALESCE(rm.sender_jid, ''), '@', 1) AS phone,
				t.amount
			FROM transactions t
			JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(rm.sender_jid, ''), '@', 1)
			WHERE t.tx_date >= $1 AND t.tx_date < $2
			  AND t.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND t.amount > 0
			  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3))
		) s
		WHERE ($4 = '' OR s.sender_name ILIKE '%' || $4 || '%' OR s.phone LIKE '%' || $4 || '%')
		GROUP BY s.sender_name, s.phone
		ORDER BY 4 DESC
	`, from, to, groupSliceArg(groupJIDs), senderQuery)
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

// ChatTurn — одна реплика диалога ассистента, как она лежит в БД. Отдельный от
// ai.Turn тип, чтобы пакет db не зависел от пакета ai (нет лишней связности).
type ChatTurn struct {
	ChatKey  string
	FromUser bool
	Text     string
}

// AllChatHistory возвращает всю сохранённую историю диалогов ассистента в
// хронологическом порядке (по id). Бот группирует её по chat_key при старте,
// чтобы восстановить память после рестарта.
func (d *DB) AllChatHistory(ctx context.Context) ([]ChatTurn, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT chat_key, from_user, body FROM assistant_history ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChatTurn
	for rows.Next() {
		var t ChatTurn
		if err := rows.Scan(&t.ChatKey, &t.FromUser, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveChatTurns дописывает новые реплики диалога и подрезает историю ключа до
// последних keep записей — чтобы таблица не росла бесконечно, а память
// оставалась ограниченной тем же окном, что и в оперативке.
func (d *DB) SaveChatTurns(ctx context.Context, key string, turns []ChatTurn, keep int) error {
	if len(turns) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range turns {
		batch.Queue(`INSERT INTO assistant_history (chat_key, from_user, body) VALUES ($1, $2, $3)`, key, t.FromUser, t.Text)
	}
	if keep > 0 {
		batch.Queue(`
			DELETE FROM assistant_history
			WHERE chat_key = $1 AND id NOT IN (
				SELECT id FROM assistant_history WHERE chat_key = $1 ORDER BY id DESC LIMIT $2
			)
		`, key, keep)
	}
	br := d.pool.SendBatch(ctx, batch)
	defer br.Close()
	for i := 0; i < batch.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}
