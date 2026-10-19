// Package db инкапсулирует работу с PostgreSQL: контакты, алиасы,
// сырые сообщения и транзакции.
package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"whatsapp-bot/internal/ledger"
)

// nameKey нормализует имя для группировки в отчётах: регистр и лишние пробелы
// не должны плодить дубли одного человека. Без этого «Хадаев Али» и «Хадаев
// али» попадали в две строки отчёта, и суммы по человеку были неверные.
func nameKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// contactKey — ключ человека для склейки контактов: без регистра, ё=е, без
// знаков, слова по алфавиту («магомедов ахмед» = «Ахмед Магомедов»).
func contactKey(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "ё", "е"), "Ё", "Е"))
	f := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	sort.Strings(f)
	return strings.Join(f, " ")
}

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
	// Один процесс мигрирует за раз (второй экземпляр бота или параллельные
	// тесты ждут): иначе одновременный CREATE OR REPLACE FUNCTION падает.
	lockConn, lerr := pool.Acquire(ctx)
	if lerr != nil {
		return lerr
	}
	defer lockConn.Release()
	if _, lerr := lockConn.Exec(ctx, `SELECT pg_advisory_lock(734200118)`); lerr != nil {
		return lerr
	}
	defer lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(734200118)`)
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
		// collector — кто ЗАБРАЛ деньги по чеку (ответственный). Отдельно от
		// submitted_by (кто ПРИСЛАЛ чек в WhatsApp), чтобы «кто прислал» в журнале
		// не подменялся ответственным.
		"ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS collector TEXT",
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
	// Реальный телефон отправителя (в форме <номер>@s.whatsapp.net). При новой
	// адресации WhatsApp (LID) sender_jid — это СКРЫТЫЙ идентификатор @lid, а
	// настоящий номер приходит в том же событии (SenderAlt). Храним его отдельно,
	// чтобы отчёты показывали реальный номер и подтягивали имя из phone_owners,
	// даже когда sender_jid — LID. Пусто/NULL = номер не известен (старые строки,
	// пересланные ботом копии, надиктованные платежи). sender_jid НЕ трогаем —
	// на нём держатся ключи спаривания (pending_client_names, cmf_watch и т.п.).
	if _, err := pool.Exec(ctx, `ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS sender_phone TEXT`); err != nil {
		return fmt.Errorf("добавление колонки sender_phone: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS ignored BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.ignored: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS ignored BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки bank_receipts.ignored: %w", err)
	}
	// Наличка: кто ФИЗИЧЕСКИ забрал деньги (ответственный). Если не указан —
	// бот спрашивает «у кого наличка», collector_asked помечает, что уже спросил.
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS collector TEXT`); err != nil {
		return fmt.Errorf("добавление колонки transactions.collector: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS collector_asked BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.collector_asked: %w", err)
	}
	// Наличка-повтор: та же сумма тому же клиенту за 30 дней может быть случайным
	// дублем (кто-то скинул одну и ту же наличку дважды). dup_pending — платёж
	// придержан из сбора, пока владелец не подтвердит «новый»; dup_asked — уже
	// спросили. У чеков от дублей защищает номер операции, у налички номера нет.
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS dup_pending BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.dup_pending: %w", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS dup_asked BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.dup_asked: %w", err)
	}
	// id сообщения-вопроса «новый или тот же?» — чтобы ответ владельца привязался
	// к нужной транзакции ДАЖЕ после перезапуска бота (иначе связь жила только в
	// памяти и придержанный платёж навсегда выпадал из сбора).
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS dup_ask_msg_id TEXT`); err != nil {
		return fmt.Errorf("добавление колонки transactions.dup_ask_msg_id: %w", err)
	}
	// id вопроса «у кого наличка?» — чтобы ответ владельца привязался к наличке
	// ДАЖЕ после перезапуска бота (связь cashAskMap в памяти теряется).
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS collector_ask_msg_id TEXT`); err != nil {
		return fmt.Errorf("добавление колонки transactions.collector_ask_msg_id: %w", err)
	}
	// id вопроса «чей чек / какая сумма?» — чтобы ответ владельца на вопрос по
	// чеку нашёл чек ДАЖЕ после перезапуска бота (askMap живёт только в памяти).
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS clarify_ask_msg_id TEXT`); err != nil {
		return fmt.Errorf("добавление колонки bank_receipts.clarify_ask_msg_id: %w", err)
	}
	// Свёрнутый чек (экран «Перевод выполнен» без даты/номера/получателя). Помечаем
	// флагом, чтобы пришедший следом ПОЛНЫЙ чек на ту же сумму мог заменить его
	// (SupersedeCollapsedReceipt) даже если владелец уже успел привязать к нему
	// клиента — иначе один платёж посчитался бы дважды.
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS collapsed BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки bank_receipts.collapsed: %w", err)
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
	// Ключ человека у контакта (без регистра/порядка слов) — для склейки.
	if _, err := pool.Exec(ctx, `ALTER TABLE contacts ADD COLUMN IF NOT EXISTS name_key TEXT`); err != nil {
		return fmt.Errorf("колонка contacts.name_key: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_contacts_name_key ON contacts(name_key)`); err != nil {
		return fmt.Errorf("индекс contacts.name_key: %w", err)
	}
	if rows, err := pool.Query(ctx, `SELECT id, canonical_name FROM contacts WHERE name_key IS NULL`); err == nil {
		type kv struct {
			id  int
			key string
		}
		var todo []kv
		for rows.Next() {
			var id int
			var name string
			if rows.Scan(&id, &name) == nil {
				todo = append(todo, kv{id, contactKey(name)})
			}
		}
		rows.Close()
		for _, x := range todo {
			_, _ = pool.Exec(ctx, `UPDATE contacts SET name_key = $2 WHERE id = $1`, x.id, x.key)
		}
	}
	// Какое сообщение назвало клиента чека (имя после чека): удалят его —
	// привязку откатываем, чек снова ждёт клиента.
	if _, err := pool.Exec(ctx, `ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS client_source_raw_id INT`); err != nil {
		return fmt.Errorf("колонка bank_receipts.client_source_raw_id: %w", err)
	}
	// id вопроса бота «за кого этот платёж?» — чтобы ответ свайпом на вопрос нашёл
	// своё наблюдение даже после перезапуска бота.
	if _, err := pool.Exec(ctx, `ALTER TABLE cmf_watch ADD COLUMN IF NOT EXISTS ask_msg_id TEXT`); err != nil {
		return fmt.Errorf("колонка cmf_watch.ask_msg_id: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_cmf_watch_ask ON cmf_watch(ask_msg_id)`); err != nil {
		return fmt.Errorf("индекс cmf_watch.ask_msg_id: %w", err)
	}
	// Все сообщения бота по наблюдению (вопрос, переспросы, «как в прошлый раз»):
	// свайп на ЛЮБОЕ из них — ответ по этому чеку.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS cmf_watch_asks (
			ask_msg_id TEXT NOT NULL,
			watch_id   INT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("таблица cmf_watch_asks: %w", err)
	}
	// Одно сообщение бота может касаться нескольких чеков (общее напоминание), а
	// у вопроса — свой список вариантов (номер в ответе — из ЭТОГО списка).
	for _, q := range []string{
		`ALTER TABLE cmf_watch_asks DROP CONSTRAINT IF EXISTS cmf_watch_asks_pkey`,
		`ALTER TABLE cmf_watch_asks ADD COLUMN IF NOT EXISTS options TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_cmf_watch_asks_pair ON cmf_watch_asks(ask_msg_id, watch_id)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("cmf_watch_asks: %w", err)
		}
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

	// Durable-очередь «имён без чека» (ФИО клиента, присланное ПЕРЕД чеком).
	// Раньше жила только в памяти и терялась при рестарте — теперь переживает
	// перезапуск, а порядок выдачи идёт по времени сообщения WhatsApp (received_at),
	// а не по времени обработки, поэтому FIFO чек↔имя не зависит от того, какая
	// горутина/OCR закончилась первой. Съеденные/протухшие записи чистит уборщик.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS pending_client_names (
			id             SERIAL PRIMARY KEY,
			group_jid      TEXT NOT NULL,
			sender_jid     TEXT NOT NULL,
			name           TEXT NOT NULL,
			amount         DOUBLE PRECISION NOT NULL DEFAULT 0,
			raw_message_id INTEGER,
			received_at    TIMESTAMPTZ NOT NULL,
			consumed       BOOLEAN NOT NULL DEFAULT false,
			consumed_at    TIMESTAMPTZ,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы pending_client_names: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_pending_names_queue
		ON pending_client_names (group_jid, sender_jid, received_at)
		WHERE consumed = false
	`); err != nil {
		return fmt.Errorf("индекс pending_client_names: %w", err)
	}

	// Планировщик напоминаний/сообщений: разовые и повторяющиеся (ежедневно/
	// еженедельно) задания. Переживают рестарт; выполнение идемпотентно (задание
	// «сдвигается» на следующий срок атомарно перед отправкой, поэтому после
	// перезапуска одно и то же не уходит дважды).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS scheduled_jobs (
			id           SERIAL PRIMARY KEY,
			created_by   TEXT NOT NULL,
			kind         TEXT NOT NULL,          -- once | daily | weekly
			target_kind  TEXT NOT NULL,          -- me | here | group | person
			target       TEXT NOT NULL,          -- JID получателя
			target_label TEXT NOT NULL DEFAULT '',
			message      TEXT NOT NULL,
			next_run_at  TIMESTAMPTZ NOT NULL,
			at_hour      INT NOT NULL DEFAULT 0,
			at_minute    INT NOT NULL DEFAULT 0,
			weekday      INT NOT NULL DEFAULT -1, -- 0=вс..6=сб (для weekly)
			active       BOOLEAN NOT NULL DEFAULT true,
			last_run_at  TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы scheduled_jobs: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_due
		ON scheduled_jobs (next_run_at) WHERE active = true
	`); err != nil {
		return fmt.Errorf("индекс scheduled_jobs: %w", err)
	}

	// Фоновый повтор распознавания чеков: если в момент прихода чека модель
	// зрения была недоступна (сбой API/сети), чек всё равно сохраняется как
	// needs_review, а эти поля позволяют перечитать его позже, когда ИИ вернётся,
	// и заполнить сумму/получателя, не заставляя работника переслать чек.
	for _, ddl := range []string{
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS recognition_status TEXT NOT NULL DEFAULT 'ok'`,
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS recognition_attempts INT NOT NULL DEFAULT 0`,
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ`,
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS last_recognition_error TEXT`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("миграция recognition-полей чеков: %w", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS idx_receipts_retry
		ON bank_receipts (next_retry_at) WHERE recognition_status = 'pending'
	`); err != nil {
		return fmt.Errorf("индекс повтора распознавания: %w", err)
	}

	// Наличные платежи: помечаем текстовые транзакции, которые реально наличка
	// (а не переписанный текстом чек), чтобы отчёты считали их в сбор. Флаг
	// проставляется при вставке; для уже накопленных данных — разовый backfill
	// по тексту сообщения (наличка/нал/кэш/офис/«у ‹имя›») и по card_to.
	if _, err := pool.Exec(ctx, `ALTER TABLE transactions ADD COLUMN IF NOT EXISTS is_cash BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return fmt.Errorf("добавление колонки transactions.is_cash: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE transactions t SET is_cash = true
		FROM raw_messages rm
		WHERE rm.id = t.raw_message_id
		  AND t.is_cash = false
		  AND (
		    COALESCE(rm.body, '') ~* '(налич|\yнал\y|кэш|\ycash\y|\yофис|\yу\s+\S)'
		    OR COALESCE(t.card_to, '') ~* 'налич'
		  )
	`); err != nil {
		return fmt.Errorf("backfill transactions.is_cash: %w", err)
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

	// Расходы: владелец пишет «сделал расход 5000 на бензин» — бот записывает
	// сюда; по запросу «скинь мои расходы» строит PDF. Отдельно от сбора.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS expenses (
			id         SERIAL PRIMARY KEY,
			amount     NUMERIC NOT NULL,
			note       TEXT,
			created_by TEXT,
			spent_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("создание таблицы expenses: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_expenses_spent_at ON expenses(spent_at)`); err != nil {
		return fmt.Errorf("индекс expenses: %w", err)
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
	for _, q := range []string{
		// Счётчик правок сообщения: разбор, начатый до правки, свой итог не пишет.
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS edit_count INT NOT NULL DEFAULT 0`,
		// Чем оказалось сообщение: 'answer' — ответ на вопрос бота, 'assistant' —
		// обращение к ассистенту (правка такого сообщения платежом не становится).
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS handled_as TEXT`,
		// Вопрос бота «❓ чей это платёж?» по этому сообщению — переживает рестарт.
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS who_ask_msg_id TEXT`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS who_ask_amount NUMERIC(12,2)`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS who_ask_cash BOOLEAN`,
		`CREATE INDEX IF NOT EXISTS idx_raw_messages_who_ask ON raw_messages(who_ask_msg_id) WHERE who_ask_msg_id IS NOT NULL`,
		// Какое сообщение (свайп с ФИО) уточнило клиента текстового платежа.
		`ALTER TABLE transactions ADD COLUMN IF NOT EXISTS client_source_raw_id INT`,
		// Наличка, признанная повтором («тот же»): повтором какого платежа.
		// Удалят сообщение того платежа — повтор вернётся в учёт (деньги не пропадут).
		`ALTER TABLE transactions ADD COLUMN IF NOT EXISTS dup_same_as INT`,
		`CREATE INDEX IF NOT EXISTS idx_transactions_dup_same_as ON transactions(dup_same_as) WHERE dup_same_as IS NOT NULL`,
		// Чтения ИИ по сообщению разошлись, а спросить по нему было нельзя (импорт
		// истории, пересчёт, болтовня с цифрой, группа на «молчи»): фоновый
		// повтор его не перечитывает и после перезапуска — только «пересчитай».
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS ai_disputed BOOLEAN NOT NULL DEFAULT false`,
		// Поправки суммы «не 5000 а 6000»: удалят/изменят поправку — откатим.
		`CREATE TABLE IF NOT EXISTS amount_corrections (
			id SERIAL PRIMARY KEY,
			raw_message_id INT NOT NULL,
			kind TEXT NOT NULL,
			target_id INT NOT NULL,
			old_amount NUMERIC(12,2) NOT NULL,
			new_amount NUMERIC(12,2) NOT NULL,
			reverted BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_amount_corrections_raw ON amount_corrections(raw_message_id)`,
		// Ответ на вопрос бота: на какой вопрос (вид и цель) и какое сообщение он
		// цитировал — чтобы правка ответа применилась к тому же вопросу.
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS answer_kind TEXT`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS answer_target TEXT`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS quoted_wa_id TEXT`,
		// id фото/документа, которое бот отправил в группу при пересылке чека
		// (удалят его там — копия выпадает из учёта той группы).
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS fwd_sent_id TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_raw_messages_fwd_sent ON raw_messages(fwd_sent_id) WHERE fwd_sent_id IS NOT NULL`,
		// Копию чека поправил владелец в целевой группе — исходный её больше не перезаписывает.
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS mirror_locked BOOLEAN NOT NULL DEFAULT false`,
		// Время операции на чеке НЕ прочиталось — в tx_date время сообщения
		// (по нему нельзя сравнивать «тот же чек» с точным временем другого).
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS tx_time_guessed BOOLEAN NOT NULL DEFAULT false`,
		// На чеке напечатана только ДАТА операции: время в tx_date выбрано
		// (копия того же чека, текст этого платежа в тот день или полдень).
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS tx_date_only BOOLEAN NOT NULL DEFAULT false`,
		// Свёрнутый экран «Перевод выполнен», признанный тем же переводом, что
		// полный чек: сообщение (raw_messages.id) этого полного чека. Ответ ФИО
		// другого клиента на вопрос по экрану возвращает экран в учёт, удалят
		// полный чек — экран снова в работе.
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS superseded_by_raw INT`,
		// Человек ответил «другой» на «похоже, это тот же чек» — назовут клиента,
		// бот не переспрашивает «тот же?» по слабому совпадению.
		`ALTER TABLE bank_receipts ADD COLUMN IF NOT EXISTS same_check_denied BOOLEAN NOT NULL DEFAULT false`,
		// Подпись к фото отдельно от распознанного текста (для повторного чтения),
		// и само сообщение с фото, если его не удалось скачать (перекачаем позже).
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS caption TEXT`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS media_proto BYTEA`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS media_attempts INT NOT NULL DEFAULT 0`,
		`ALTER TABLE raw_messages ADD COLUMN IF NOT EXISTS media_next_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS idx_raw_messages_media_retry ON raw_messages(media_next_at) WHERE has_media AND NOT parsed`,
		// Копия чека, пересланная ботом в другую группу, — зеркало исходного:
		// исключили/удалили/поправили исходный — то же с копией.
		`CREATE OR REPLACE FUNCTION mirror_forward_copies() RETURNS trigger AS $$
		DECLARE src_wa TEXT;
		BEGIN
			IF pg_trigger_depth() > 1 THEN RETURN NEW; END IF;
			SELECT wa_message_id INTO src_wa FROM raw_messages
			 WHERE id = NEW.raw_message_id AND COALESCE(sender_jid, '') <> 'bot-forward';
			IF src_wa IS NULL OR src_wa = '' THEN RETURN NEW; END IF;
			UPDATE bank_receipts cp SET
				ignored = NEW.ignored,
				is_duplicate = NEW.is_duplicate,
				amount = CASE WHEN NEW.amount > 0 THEN NEW.amount ELSE cp.amount END,
				tx_date = NEW.tx_date,
				contact_id = NEW.contact_id,
				recipient_raw = NEW.recipient_raw,
				client_confirmed = NEW.client_confirmed,
				needs_review = NEW.needs_review OR NOT (CASE WHEN NEW.amount > 0 THEN NEW.amount ELSE cp.amount END > 0),
				clarify_asked = true
			FROM raw_messages crm
			WHERE cp.raw_message_id = crm.id AND crm.sender_jid = 'bot-forward'
			  AND NOT cp.mirror_locked
			  AND starts_with(crm.wa_message_id, src_wa || '-fwd-');
			RETURN NEW;
		END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_mirror_forward_copies ON bank_receipts`,
		`CREATE TRIGGER trg_mirror_forward_copies AFTER UPDATE ON bank_receipts
			FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION mirror_forward_copies()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("миграция (%s…): %w", strings.Fields(q)[0]+" "+strings.Fields(q)[1], err)
		}
	}
	return nil
}

func (d *DB) Close() {
	d.pool.Close()
}

// SaveRawMessage сохраняет сырое сообщение. Возвращает id записи и existed —
// была ли такая запись УЖЕ (повторная доставка: whatsmeow иногда шлёт события
// повторно). existed=true -> сообщение уже сохраняли; вызывающий должен НЕ
// обрабатывать его снова, иначе платежи запишутся дважды. (xmax<>0 истинно,
// когда строка обновилась по ON CONFLICT, т.е. это повторная доставка.)
// senderPhone — реальный телефон отправителя (<номер>@s.whatsapp.net) или ""
// если не известен (LID без SenderAlt, пересланная копия, надиктовка). Пишется
// отдельно от senderJID (который может быть LID), чтобы отчёты и инструменты
// могли показать настоящий номер.
func (d *DB) SaveRawMessage(ctx context.Context, waMessageID, groupJID, senderJID, senderPhone, senderName, body string, hasMedia bool, mediaPath string, receivedAt time.Time) (int, bool, error) {
	var id int
	var existed bool
	err := d.pool.QueryRow(ctx, `
		INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_phone, sender_name, body, has_media, media_path, received_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9)
		ON CONFLICT (wa_message_id) DO UPDATE SET
			-- повторная доставка того же сообщения: не перетираем, но если номер
			-- не был известен, а теперь известен — дозаполняем (не ухудшаем данные).
			sender_phone = COALESCE(raw_messages.sender_phone, NULLIF(EXCLUDED.sender_phone, ''))
		RETURNING id, (xmax <> 0)
	`, waMessageID, groupJID, senderJID, senderPhone, senderName, body, hasMedia, mediaPath, receivedAt).Scan(&id, &existed)
	return id, existed, err
}

// SenderPhoneByMessageID возвращает РЕАЛЬНЫЙ номер (цифры) отправителя исходного
// сообщения по его wa_message_id, или "" если номер не известен (сообщения нет в
// базе, либо отправитель был LID и настоящий номер не пойман). Используется, когда
// владелец свайпом отвечает на чьё-то сообщение: по id цитаты достаём телефон, не
// принимая LID за номер.
func (d *DB) SenderPhoneByMessageID(ctx context.Context, waMessageID string) (string, error) {
	var phone string
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(split_part(COALESCE(NULLIF(sender_phone, ''),
			CASE WHEN sender_jid LIKE '%@s.whatsapp.net' THEN sender_jid END), '@', 1), '')
		FROM raw_messages WHERE wa_message_id = $1
	`, waMessageID).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return phone, err
}

// OldestMessageInfo возвращает НАСТОЯЩИЙ (не пересланный ботом) самый ранний
// wa_message_id и время для группы — якорь для запроса более старой истории с
// телефона (on-demand history sync). ok=false — по группе сообщений ещё нет.
func (d *DB) OldestMessageInfo(ctx context.Context, groupJID string) (waID string, ts time.Time, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT wa_message_id, received_at
		FROM raw_messages
		WHERE wa_group_jid = $1
		  AND COALESCE(wa_message_id, '') <> ''
		  AND position('-fwd-' in wa_message_id) = 0
		ORDER BY received_at ASC
		LIMIT 1
	`, groupJID).Scan(&waID, &ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return waID, ts, true, nil
}

// SetMediaInfo — подпись к фото и (если фото не скачалось) само сообщение для
// повторной попытки.
func (d *DB) SetMediaInfo(ctx context.Context, rawID int, caption string, proto []byte) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET caption = NULLIF($2, ''), media_proto = $3,
		media_next_at = CASE WHEN $3::bytea IS NULL THEN media_next_at ELSE now() + interval '2 minutes' END WHERE id = $1`,
		rawID, caption, proto)
	return err
}

// MediaRetry — фото/PDF из группы, которое так и не стало чеком: не скачалось
// или не прочиталось (ИИ был недоступен).
type MediaRetry struct {
	RawID      int
	WaID       string
	GroupJID   string
	SenderJID  string
	SenderName string
	Body       string
	Caption    string
	MediaPath  string
	Proto      []byte
	ReceivedAt time.Time
	Attempts   int
}

const mediaRetryCols = `rm.id, COALESCE(rm.wa_message_id, ''), COALESCE(rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''),
	COALESCE(rm.sender_name, ''), COALESCE(rm.body, ''), COALESCE(rm.caption, ''), COALESCE(rm.media_path, ''), rm.media_proto,
	rm.received_at, rm.media_attempts`

func scanMediaRetries(rows pgx.Rows) ([]MediaRetry, error) {
	defer rows.Close()
	var out []MediaRetry
	for rows.Next() {
		var m MediaRetry
		if err := rows.Scan(&m.RawID, &m.WaID, &m.GroupJID, &m.SenderJID, &m.SenderName, &m.Body, &m.Caption,
			&m.MediaPath, &m.Proto, &m.ReceivedAt, &m.Attempts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DueMediaRetries — фото без чека, которым пора повторить скачивание/чтение.
func (d *DB) DueMediaRetries(ctx context.Context, limit int) ([]MediaRetry, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+mediaRetryCols+`
		FROM raw_messages rm
		WHERE rm.has_media AND NOT rm.parsed AND NOT COALESCE(rm.deleted, false)
		  AND COALESCE(rm.wa_group_jid, '') LIKE '%@g.us' AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND rm.received_at >= now() - interval '7 days' AND rm.received_at < now() - interval '3 minutes'
		  AND (rm.media_next_at IS NULL OR rm.media_next_at <= now()) AND rm.media_attempts < 8
		  AND (COALESCE(rm.media_path, '') <> '' OR rm.media_proto IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
		ORDER BY rm.received_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanMediaRetries(rows)
}

// GroupMediaToReread — для «перечитай группу»: фото/PDF группы за период без
// засчитанного чека (не скачалось, не прочиталось, сумма не распознана).
func (d *DB) GroupMediaToReread(ctx context.Context, groupJID string, since time.Time, limit int) ([]MediaRetry, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+mediaRetryCols+`
		FROM raw_messages rm
		WHERE rm.has_media AND NOT COALESCE(rm.deleted, false) AND ($1 = '' OR rm.wa_group_jid = $1)
		  AND COALESCE(rm.wa_group_jid, '') LIKE '%@g.us' AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND rm.received_at >= $2
		  AND (COALESCE(rm.media_path, '') <> '' OR rm.media_proto IS NOT NULL)
		  AND (
		    (NOT rm.parsed AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id))
		    OR EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id AND NOT br.ignored AND NOT br.is_duplicate
		               AND (br.amount <= 0 OR br.recognition_status IN ('failed', 'pending')))
		  )
		ORDER BY rm.received_at LIMIT $3`, groupJID, since, limit)
	if err != nil {
		return nil, err
	}
	return scanMediaRetries(rows)
}

// NeedsMediaReread — фото сообщения так и не стало чеком (не скачалось/не прочиталось).
func (d *DB) NeedsMediaReread(ctx context.Context, rawID int) bool {
	var ok bool
	_ = d.pool.QueryRow(ctx, `SELECT has_media AND NOT parsed AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
		FROM raw_messages rm WHERE id = $1`, rawID).Scan(&ok)
	return ok
}

// BumpMediaRetry — попытка не удалась: следующая позже.
func (d *DB) BumpMediaRetry(ctx context.Context, rawID int, next time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET media_attempts = media_attempts + 1, media_next_at = $2 WHERE id = $1`, rawID, next)
	return err
}

// SetMediaPath — фото скачалось при повторе.
func (d *DB) SetMediaPath(ctx context.Context, rawID int, path string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET media_path = $2 WHERE id = $1`, rawID, path)
	return err
}

// ReceiptOfRaw — чек сообщения (если есть): id и сумма.
func (d *DB) ReceiptOfRaw(ctx context.Context, rawID int) (id int, amount float64, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `SELECT id, amount::float8 FROM bank_receipts WHERE raw_message_id = $1 AND NOT is_duplicate ORDER BY id DESC LIMIT 1`, rawID).Scan(&id, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	return id, amount, err == nil, err
}

// GapAnchor — для запроса истории с телефона: первое сообщение после самого
// большого перерыва в группе за последние days дней (бот был отключён). Нет
// заметного перерыва — самое свежее сообщение.
func (d *DB) GapAnchor(ctx context.Context, groupJID string, days int) (waID string, ts time.Time, gap time.Duration, ok bool, err error) {
	var gapSec float64
	err = d.pool.QueryRow(ctx, `
		WITH m AS (
			SELECT wa_message_id, received_at,
			       received_at - lag(received_at) OVER (ORDER BY received_at) AS gap
			FROM raw_messages
			WHERE wa_group_jid = $1 AND COALESCE(wa_message_id, '') <> '' AND position('-fwd-' in wa_message_id) = 0
			  AND received_at >= now() - make_interval(days => $2))
		SELECT wa_message_id, received_at, COALESCE(extract(epoch FROM gap), 0)::float8 FROM m
		ORDER BY CASE WHEN gap > interval '3 hours' THEN 0 ELSE 1 END, gap DESC NULLS LAST, received_at DESC
		LIMIT 1`, groupJID, days).Scan(&waID, &ts, &gapSec)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, 0, false, nil
	}
	if err != nil {
		return "", time.Time{}, 0, false, err
	}
	gap = time.Duration(gapSec) * time.Second
	if gap < 3*time.Hour {
		// Перерыва нет — просим историю перед самым свежим сообщением.
		if err := d.pool.QueryRow(ctx, `SELECT wa_message_id, received_at FROM raw_messages
			WHERE wa_group_jid = $1 AND COALESCE(wa_message_id, '') <> '' AND position('-fwd-' in wa_message_id) = 0
			ORDER BY received_at DESC LIMIT 1`, groupJID).Scan(&waID, &ts); err != nil {
			return "", time.Time{}, 0, false, err
		}
		gap = 0
	}
	return waID, ts, gap, true, nil
}

// FoundCheck — найденный чек или платёж с состоянием в учёте.
type FoundCheck struct {
	Kind     string // чек / текст / фото
	ID       int
	WaID     string
	Client   string
	Card     string
	Amount   float64
	TxDate   time.Time
	Received time.Time
	Sender   string
	GroupJID string
	Status   string // засчитан / дубль / исключён / удалён / ждёт клиента / не распознан / наличка-повтор
	DupOf    string // для дубля: когда и кем прислан первый
}

// FindChecks — поиск чека/платежа по сумме и/или имени во ВСЕХ состояниях (в
// том числе дубли, удалённые, исключённые, непрочитанные фото), с причиной,
// почему он не в сборе.
func (d *DB) FindChecks(ctx context.Context, amount, tolerance float64, name, groupJID string, from, to time.Time, limit int) ([]FoundCheck, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT * FROM (
			SELECT 'чек' AS kind, br.id, COALESCE(rm.wa_message_id, '') AS wa, COALESCE(c.canonical_name, br.recipient_raw, '') AS client,
			       COALESCE(br.card_owner, '') AS card, br.amount::float8 AS amount, br.tx_date AS txd,
			       COALESCE(rm.received_at, br.tx_date) AS recv, COALESCE(NULLIF(rm.sender_name, ''), rm.sender_jid, '') AS sender,
			       COALESCE(br.group_jid, rm.wa_group_jid, '') AS grp,
			       CASE WHEN COALESCE(rm.deleted, false) THEN 'удалён в WhatsApp'
			            WHEN br.ignored THEN 'исключён из учёта'
			            WHEN br.is_duplicate THEN 'повтор уже присланного чека'
			            WHEN br.amount <= 0 THEN 'сумма не распознана'
			            WHEN br.needs_review OR NOT br.client_confirmed THEN 'ждёт клиента («чей чек?»)'
			            ELSE 'засчитан' END AS status,
			       COALESCE(br.recipient_raw, '') || ' ' || COALESCE(br.card_owner, '') || ' ' || COALESCE(rm.body, '') AS hay
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN contacts c ON c.id = br.contact_id
			UNION ALL
			SELECT CASE WHEN t.is_cash THEN 'наличка' ELSE 'текст' END, t.id, COALESCE(rm.wa_message_id, ''),
			       COALESCE(c.canonical_name, t.raw_name, ''), '', t.amount::float8, t.tx_date::timestamptz,
			       COALESCE(rm.received_at, t.tx_date::timestamptz), COALESCE(NULLIF(rm.sender_name, ''), rm.sender_jid, ''),
			       COALESCE(rm.wa_group_jid, ''),
			       CASE WHEN COALESCE(rm.deleted, false) THEN 'удалён в WhatsApp'
			            WHEN t.ignored THEN 'исключён из учёта'
			            WHEN t.dup_pending THEN 'придержан как возможный повтор налички'
			            ELSE 'записан' END,
			       COALESCE(t.raw_name, '') || ' ' || COALESCE(rm.body, '')
			FROM transactions t
			LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN contacts c ON c.id = t.contact_id
			UNION ALL
			SELECT 'фото', rm.id, COALESCE(rm.wa_message_id, ''), '', '', 0, rm.received_at, rm.received_at,
			       COALESCE(NULLIF(rm.sender_name, ''), rm.sender_jid, ''), COALESCE(rm.wa_group_jid, ''),
			       CASE WHEN COALESCE(rm.media_path, '') = '' THEN 'фото не скачалось' ELSE 'фото не прочитано как чек' END,
			       COALESCE(rm.body, '') || ' ' || COALESCE(rm.caption, '')
			FROM raw_messages rm
			WHERE rm.has_media AND NOT COALESCE(rm.deleted, false) AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
			  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
			  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id)
		) x
		WHERE x.recv >= $4 AND x.recv < $5
		  AND ($3 = '' OR x.grp = $3)
		  AND ($1::float8 <= 0 OR abs(x.amount - $1) <= $2 OR (x.kind = 'фото' AND position((round($1)::bigint)::text in replace(x.hay, ' ', '')) > 0))
		ORDER BY x.recv DESC LIMIT 2000`, amount, tolerance, groupJID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Имя сравниваем здесь, а не в SQL: ILIKE с кириллицей зависит от локали базы.
	q := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "ё", "е"))
	var out []FoundCheck
	for rows.Next() {
		var f FoundCheck
		var hay string
		if err := rows.Scan(&f.Kind, &f.ID, &f.WaID, &f.Client, &f.Card, &f.Amount, &f.TxDate, &f.Received, &f.Sender, &f.GroupJID, &f.Status, &hay); err != nil {
			return nil, err
		}
		if q != "" {
			h := strings.ToLower(strings.ReplaceAll(hay+" "+f.Client+" "+f.Card, "ё", "е"))
			if !strings.Contains(h, q) {
				continue
			}
		}
		out = append(out, f)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func (d *DB) MarkMessageParsed(ctx context.Context, rawMessageID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET parsed = true WHERE id = $1`, rawMessageID)
	return err
}

// GetOrCreateContact возвращает id контакта по каноничному имени, создавая при отсутствии.
func (d *DB) GetOrCreateContact(ctx context.Context, canonicalName string) (int, error) {
	var id int
	// Тот же человек, написанный иначе («магомедов ахмед», «Ахмед Магомедов»),
	// — тот же контакт: иначе текст и чек одного платежа не сводились в один и
	// сбор задваивался.
	key := contactKey(canonicalName)
	if key != "" {
		err := d.pool.QueryRow(ctx, `SELECT id FROM contacts WHERE name_key = $1 ORDER BY id LIMIT 1`, key).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	err := d.pool.QueryRow(ctx, `
		INSERT INTO contacts (canonical_name, name_key) VALUES ($1, $2)
		ON CONFLICT (canonical_name) DO UPDATE SET canonical_name = EXCLUDED.canonical_name
		RETURNING id
	`, canonicalName, key).Scan(&id)
	return id, err
}

// resolveContact находит контакт по имени: сначала точное совпадение каноничного
// имени (без учёта регистра), иначе ЕДИНСТВЕННОЕ частичное. Возвращает понятную
// ошибку, если не найден или неоднозначен — чтобы владелец уточнил.
func (d *DB) resolveContact(ctx context.Context, name string) (id int, canonical string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, "", fmt.Errorf("пустое имя")
	}
	err = d.pool.QueryRow(ctx, `SELECT id, canonical_name FROM contacts WHERE lower(canonical_name)=lower($1)`, name).Scan(&id, &canonical)
	if err == nil {
		return id, canonical, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", err
	}
	rows, err := d.pool.Query(ctx, `SELECT id, canonical_name FROM contacts WHERE canonical_name ILIKE '%'||$1||'%' ORDER BY canonical_name LIMIT 3`, name)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var ids []int
	var names []string
	for rows.Next() {
		var i int
		var n string
		if err := rows.Scan(&i, &n); err != nil {
			return 0, "", err
		}
		ids = append(ids, i)
		names = append(names, n)
	}
	switch len(ids) {
	case 0:
		return 0, "", fmt.Errorf("клиент %q не найден", name)
	case 1:
		return ids[0], names[0], nil
	default:
		return 0, "", fmt.Errorf("по «%s» несколько клиентов (%s) — назови полное имя", name, strings.Join(names, ", "))
	}
}

// MergeContacts объединяет контакт-ДУБЛИКАТ fromName в toName: переносит все его
// чеки и платежи на toName, старое имя делает синонимом нового и удаляет пустой
// контакт. Нужен владельцу, чтобы руками исправить задвоение одного человека
// (разные написания ФИО создали два контакта). Всё в одной транзакции (или всё,
// или ничего). Возвращает число перенесённых записей и каноничные имена обоих.
func (d *DB) MergeContacts(ctx context.Context, fromName, toName string) (moved int, fromCanonical, toCanonical string, err error) {
	fromID, fromCanonical, err := d.resolveContact(ctx, fromName)
	if err != nil {
		return 0, "", "", err
	}
	toID, toCanonical, err := d.resolveContact(ctx, toName)
	if err != nil {
		return 0, "", "", err
	}
	if fromID == toID {
		return 0, fromCanonical, toCanonical, fmt.Errorf("«%s» и «%s» — это уже один контакт, объединять нечего", fromCanonical, toCanonical)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, "", "", err
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx, `UPDATE transactions SET contact_id=$1 WHERE contact_id=$2`, toID, fromID)
	if err != nil {
		return 0, "", "", err
	}
	moved += int(ct.RowsAffected())
	cr, err := tx.Exec(ctx, `UPDATE bank_receipts SET contact_id=$1 WHERE contact_id=$2`, toID, fromID)
	if err != nil {
		return 0, "", "", err
	}
	moved += int(cr.RowsAffected())
	// Переносим алиасы старого контакта на новый (пропуская те, что уже есть у нового).
	if _, err = tx.Exec(ctx, `
		UPDATE contact_aliases SET contact_id=$1
		WHERE contact_id=$2 AND alias NOT IN (SELECT alias FROM contact_aliases WHERE contact_id=$1)`, toID, fromID); err != nil {
		return 0, "", "", err
	}
	// Старое каноничное имя — синонимом нового, чтобы будущие упоминания старого
	// написания находили объединённый контакт.
	if _, err = tx.Exec(ctx, `
		INSERT INTO contact_aliases (contact_id, alias) VALUES ($1, lower($2))
		ON CONFLICT (alias) DO NOTHING`, toID, fromCanonical); err != nil {
		return 0, "", "", err
	}
	// Удаляем опустевший контакт (оставшиеся его алиасы уйдут каскадом).
	if _, err = tx.Exec(ctx, `DELETE FROM contacts WHERE id=$1`, fromID); err != nil {
		return 0, "", "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, "", "", err
	}
	return moved, fromCanonical, toCanonical, nil
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
	IsCash       bool   // платёж помечен как наличка (иначе — дубль чека, в сбор не идёт)
	Collector    string // кто забрал наличку (ответственный), если известен
	RawMessageID int
	TxDate       time.Time
	DupCheck     bool // проверять на повтор налички (та же сумма клиента за 30 дней)
	// ClientSourceRawID — сообщение, которое назвало клиента (ответ на «❓ чей
	// платёж?»): удалят/изменят его — платёж узнает.
	ClientSourceRawID int
}

// SameCashClient — один ли это клиент для проверки повтора налички, когда
// контакты разные: опечатка («Магамедов»/«Магомедов»), падеж, порядок слов.
// Бот подставляет строгое сравнение имён для денег (Омаров ≠ Умаров); по
// умолчанию — те же слова без учёта регистра, ё/е и порядка.
var SameCashClient = func(a, b string) bool {
	ka := contactKey(a)
	return ka != "" && ka == contactKey(b)
}

// CashDupCheck — включена ли проверка повтора налички (бот подставляет свой
// выключатель): выключена — вернувшуюся наличку не придерживаем, вопроса о ней
// не будет.
var CashDupCheck = func() bool { return true }

// cashPlaceholder — «клиент»-заглушка налички без имени: разные люди.
func cashPlaceholder(name string) bool {
	return name == "Наличка" || name == "(не указано)"
}

// cashRepeatProbe — наличка, которую проверяем на повтор.
type cashRepeatProbe struct {
	TxID      int // сам платёж (не сравниваем с собой); 0 — ещё не записан
	ContactID int
	Name      string // каноничное имя клиента
	Amount    float64
	TxDate    time.Time
	RawID     int  // сообщение платежа: его группа; строки того же сообщения — не повтор
	Later     bool // считать и наличку ПОЗЖЕ платежа (освобождение придержки)
}

// cashRepeatPrev — засчитанная наличка, повтором которой может быть платёж:
// та же группа, та же сумма, за 30 дней до него, другое сообщение, не удалена,
// не исключена и сама не придержана. Клиент — тот же контакт ИЛИ то же имя,
// написанное иначе (SameCashClient): переотправленная с исправленной опечаткой
// наличка — тоже повтор. Контакты при этом НЕ склеиваются. Возвращает самую
// свежую такую наличку.
func (d *DB) cashRepeatPrev(ctx context.Context, p cashRepeatProbe) (prev cashPrev, found bool, err error) {
	if p.Amount <= 0 || p.RawID == 0 || cashPlaceholder(p.Name) {
		return cashPrev{}, false, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(t.contact_id, 0), COALESCE(c.canonical_name, t.raw_name, ''), t.tx_date
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.amount = $1 AND t.is_cash = true
		  AND t.ignored = false AND t.dup_pending = false
		  AND t.tx_date >= $2::date - 30 AND ($5 OR t.tx_date <= $2::date)
		  AND rm.wa_group_jid = (SELECT wa_group_jid FROM raw_messages WHERE id = $3)
		  AND t.raw_message_id <> $3 AND t.id <> $4
		  AND COALESCE(rm.deleted, false) = false
		ORDER BY t.tx_date DESC, t.id DESC
		LIMIT 500`, p.Amount, p.TxDate, p.RawID, p.TxID, p.Later)
	if err != nil {
		return cashPrev{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var c cashPrev
		var cid int
		if err := rows.Scan(&c.ID, &cid, &c.Name, &c.When); err != nil {
			return cashPrev{}, false, err
		}
		if cashPlaceholder(c.Name) {
			continue // «Наличка» без имени — не тот же клиент
		}
		if p.ContactID != 0 && cid == p.ContactID || p.Name != "" && SameCashClient(p.Name, c.Name) {
			return c, true, nil
		}
	}
	return cashPrev{}, false, rows.Err()
}

// cashPrev — наличка, повтором которой может быть платёж.
type cashPrev struct {
	ID   int
	When time.Time
	Name string // как записан её клиент
}

// cashProbe — платёж txID как наличка, которую проверяют на повтор.
func (d *DB) cashProbe(ctx context.Context, txID int) (cashRepeatProbe, error) {
	p := cashRepeatProbe{TxID: txID}
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(t.contact_id, 0), COALESCE(c.canonical_name, t.raw_name, ''), t.amount::float8,
		       t.tx_date, COALESCE(t.raw_message_id, 0)
		FROM transactions t LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.id = $1`, txID).Scan(&p.ContactID, &p.Name, &p.Amount, &p.TxDate, &p.RawID)
	return p, err
}

// contactName — каноничное имя контакта ("" если нет).
func (d *DB) contactName(ctx context.Context, contactID int) string {
	var n string
	if contactID == 0 {
		return ""
	}
	_ = d.pool.QueryRow(ctx, `SELECT canonical_name FROM contacts WHERE id = $1`, contactID).Scan(&n)
	return n
}

func (d *DB) InsertTransaction(ctx context.Context, tx TransactionInput) error {
	dupPending := false
	if tx.IsCash && tx.DupCheck && tx.ContactID != 0 && tx.Amount > 0 {
		// Есть ли уже засчитанная наличка тому же клиенту (или то же имя с
		// опечаткой/в другом порядке слов) на ТУ ЖЕ сумму за последние 30 дней
		// В ТОЙ ЖЕ ГРУППЕ? Если да — это может быть случайный повтор (одну и ту
		// же наличку скинули дважды, второй раз — с исправленной опечаткой).
		// Придерживаем платёж (dup_pending) и просим подтвердить «новый / тот
		// же». Группу учитываем обязательно: тот же клиент на ту же сумму в
		// РАЗНЫХ группах — это разные платежи, придерживать нельзя (как и у
		// чеков, см. FindDuplicateReceipt). Наличка без имени («Нал 27000») —
		// разные люди: не повтор.
		if name := d.contactName(ctx, tx.ContactID); !cashPlaceholder(name) {
			_, held, err := d.cashRepeatPrev(ctx, cashRepeatProbe{ContactID: tx.ContactID, Name: name,
				Amount: tx.Amount, TxDate: tx.TxDate, RawID: tx.RawMessageID})
			dupPending = held && err == nil // при ошибке не придерживаем — лучше засчитать, чем потерять
		}
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO transactions (contact_id, raw_name, amount, note, card_to, is_cash, collector, raw_message_id, tx_date, dup_pending, client_source_raw_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, 0))
	`, tx.ContactID, tx.RawName, tx.Amount, nullIfEmpty(tx.Note), nullIfEmpty(tx.CardTo), tx.IsCash, nullIfEmpty(tx.Collector), tx.RawMessageID, tx.TxDate, dupPending, tx.ClientSourceRawID)
	return err
}

// CashDupAsk — наличка, похожая на повтор (та же сумма тому же клиенту за 30
// дней): придержана из сбора, по ней ещё не спрашивали «новый или тот же».
type CashDupAsk struct {
	TxID        int
	Client      string
	Amount      float64
	WaMessageID string
	SenderJID   string
	PrevWhen    time.Time // дата предыдущего такого платежа (для текста вопроса)
	// PrevName — как записан клиент предыдущей налички, если иначе (опечатка,
	// другой порядок слов): сотрудник видит, с какой записью сравнили.
	PrevName string
}

// CashDupNeedingConfirm — придержанные повторы налички, по которым пора спросить.
func (d *DB) CashDupNeedingConfirm(ctx context.Context, groupJID string, olderThan time.Time, limit int) ([]CashDupAsk, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(c.canonical_name, t.raw_name, ''), t.amount::float8,
		       COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''),
		       COALESCE(t.contact_id, 0), t.tx_date, t.raw_message_id
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE rm.wa_group_jid = $1
		  AND t.is_cash = true AND t.dup_pending = true AND t.dup_asked = false
		  AND t.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND t.amount > 0 AND t.created_at < $2
		ORDER BY t.created_at
		LIMIT $3
	`, groupJID, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CashDupAsk
	var probes []cashRepeatProbe
	for rows.Next() {
		var a CashDupAsk
		var p cashRepeatProbe
		if err := rows.Scan(&a.TxID, &a.Client, &a.Amount, &a.WaMessageID, &a.SenderJID,
			&p.ContactID, &p.TxDate, &p.RawID); err != nil {
			return nil, err
		}
		p.TxID, p.Name, p.Amount = a.TxID, a.Client, a.Amount
		out = append(out, a)
		probes = append(probes, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// С чем сравнили — та же наличка, из-за которой придержали (та же группа,
	// 30 дней, тот же клиент или то же имя, записанное иначе): её дата, а если
	// клиент записан иначе (опечатка) — и её написание. Иначе вопрос назвал бы
	// прошломесячный платёж, и ответ «новый» засчитал бы повтор дважды.
	for k := range out {
		if prev, ok, err := d.cashRepeatPrev(ctx, probes[k]); err == nil && ok {
			out[k].PrevWhen = prev.When
			if contactKey(prev.Name) != contactKey(out[k].Client) {
				out[k].PrevName = prev.Name
			}
		}
	}
	return out, nil
}

// CountCashDupNeedingConfirm — сколько придержанных повторов налички ждут ответа.
func (d *DB) CountCashDupNeedingConfirm(ctx context.Context, groupJID string, olderThan time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.wa_group_jid = $1
		  AND t.is_cash = true AND t.dup_pending = true AND t.dup_asked = false
		  AND t.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND t.amount > 0 AND t.created_at < $2
	`, groupJID, olderThan).Scan(&n)
	return n, err
}

// MarkCashDupAsked помечает, что про повтор этой налички уже спросили, и
// сохраняет id сообщения-вопроса — чтобы ответ владельца привязался к платежу
// даже после перезапуска бота (память dupAskMap теряется, БД — нет).
func (d *DB) MarkCashDupAsked(ctx context.Context, txID int, askMsgID string) error {
	_, err := d.pool.Exec(ctx, `UPDATE transactions SET dup_asked = true, dup_ask_msg_id = $2 WHERE id = $1`, txID, nullIfEmpty(askMsgID))
	return err
}

// DupTxByAskMsg находит придержанный платёж по id сообщения-вопроса «новый или
// тот же?» — запасной путь, если связь потерялась из памяти (рестарт бота).
func (d *DB) DupTxByAskMsg(ctx context.Context, askMsgID string) (int, bool, error) {
	if askMsgID == "" {
		return 0, false, nil
	}
	var txID int
	err := d.pool.QueryRow(ctx, `
		SELECT id FROM transactions
		WHERE dup_ask_msg_id = $1 AND dup_pending = true AND ignored = false
		ORDER BY id DESC LIMIT 1
	`, askMsgID).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return txID, err == nil, err
}

// ResolveCashDup применяет ответ владельца на вопрос про повтор налички:
// isNew=true — новый платёж (снимаем придержку, идёт в сбор);
// isNew=false — тот же (повтор): помечаем ignored, из сбора исключён.
func (d *DB) ResolveCashDup(ctx context.Context, txID int, isNew bool) (bool, float64, string, error) {
	var amount float64
	var client string
	var err error
	if isNew {
		err = d.pool.QueryRow(ctx, `
			UPDATE transactions SET dup_pending = false, dup_asked = true
			WHERE id = $1 AND dup_pending = true RETURNING amount::float8, COALESCE(raw_name, '')
		`, txID).Scan(&amount, &client)
	} else {
		// Только ещё не решённый повтор: случайное «нет» позже не снимет уже
		// подтверждённую владельцем наличку. Запоминаем, повтором ЧЕГО его
		// признали (dup_same_as): удалят то сообщение — этот платёж вернётся.
		err = d.pool.QueryRow(ctx, `
			UPDATE transactions SET ignored = true, dup_pending = false, dup_asked = true, dup_same_as = NULLIF($2, 0)
			WHERE id = $1 AND dup_pending = true RETURNING amount::float8, COALESCE(raw_name, '')
		`, txID, d.cashSameAs(ctx, txID)).Scan(&amount, &client)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", nil
	}
	return err == nil, amount, client, err
}

// CashNeedingCollector — наличные платежи, у которых НЕ указан ответственный
// (кто забрал) и бот ещё не спрашивал. Возвращает и id сообщения, чтобы задать
// вопрос цитатой и привязать ответ.
type CashCollectorAsk struct {
	TxID        int
	Client      string
	Amount      float64
	WaMessageID string
	SenderJID   string
	GroupJID    string
}

func (d *DB) CashNeedingCollector(ctx context.Context, groupJID string, olderThan time.Time, limit int) ([]CashCollectorAsk, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(c.canonical_name, t.raw_name, ''), t.amount::float8,
		       COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''), COALESCE(rm.wa_group_jid, '')
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE rm.who_ask_msg_id IS NULL AND -- пока открыт «❓ чей платёж?», «у кого?» не спрашиваем
		       rm.wa_group_jid = $1
		  AND t.is_cash = true AND t.dup_pending = false AND COALESCE(t.collector, '') = '' AND t.collector_asked = false
		  AND t.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND t.amount > 0 AND t.created_at < $2
		ORDER BY t.created_at
		LIMIT $3
	`, groupJID, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CashCollectorAsk
	for rows.Next() {
		var a CashCollectorAsk
		if err := rows.Scan(&a.TxID, &a.Client, &a.Amount, &a.WaMessageID, &a.SenderJID, &a.GroupJID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountCashNeedingCollector — сколько наличных платежей ждут ответственного.
func (d *DB) CountCashNeedingCollector(ctx context.Context, groupJID string, olderThan time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.who_ask_msg_id IS NULL AND -- пока открыт «❓ чей платёж?», «у кого?» не спрашиваем
		       rm.wa_group_jid = $1
		  AND t.is_cash = true AND t.dup_pending = false AND COALESCE(t.collector, '') = '' AND t.collector_asked = false
		  AND t.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND t.amount > 0 AND t.created_at < $2
	`, groupJID, olderThan).Scan(&n)
	return n, err
}

// MarkTxCollectorAsked помечает, что про ответственного по этой наличке спросили,
// и сохраняет id сообщения-вопроса — чтобы ответ владельца («у Нура») привязался
// к нужной наличке даже после перезапуска бота (память cashAskMap теряется).
func (d *DB) MarkTxCollectorAsked(ctx context.Context, txID int, askMsgID string) error {
	_, err := d.pool.Exec(ctx, `UPDATE transactions SET collector_asked = true, collector_ask_msg_id = $2 WHERE id = $1`, txID, nullIfEmpty(askMsgID))
	return err
}

// TxByCollectorAskMsg находит наличку по id сообщения-вопроса «у кого наличка?» —
// запасной путь, если связь потерялась из памяти (перезапуск бота).
func (d *DB) TxByCollectorAskMsg(ctx context.Context, askMsgID string) (int, bool, error) {
	if askMsgID == "" {
		return 0, false, nil
	}
	var txID int
	err := d.pool.QueryRow(ctx, `
		SELECT id FROM transactions
		WHERE collector_ask_msg_id = $1 AND COALESCE(collector, '') = '' AND ignored = false
		ORDER BY id DESC LIMIT 1
	`, askMsgID).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return txID, err == nil, err
}

// SetTxCollector записывает ответственного (кто забрал наличку) по id транзакции.
func (d *DB) SetTxCollector(ctx context.Context, txID int, collector string, onlyIfEmpty bool) (bool, float64, string, error) {
	var amount float64
	var client string
	err := d.pool.QueryRow(ctx, `
		UPDATE transactions SET collector = $2, collector_asked = true
		WHERE id = $1 AND (NOT $3 OR COALESCE(collector, '') = '')
		RETURNING amount::float8, COALESCE(raw_name, '')
	`, txID, collector, onlyIfEmpty).Scan(&amount, &client)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", nil
	}
	return err == nil, amount, client, err
}

// MarkLatestPaymentCash помечает наличкой самый свежий текстовый платёж от
// данного отправителя в данном чате (с момента since). Нужно, когда наличка
// подтверждается ОТДЕЛЬНЫМ сообщением — фото пачки денег: «ФИО+сумма» пришло
// текстом, а рядом сообщением-фоткой денег помечено, что это нал. Возвращает
// сумму помеченного платежа (0, если подходящего платежа нет).
func (d *DB) MarkLatestPaymentCash(ctx context.Context, groupJID, senderJID string, since time.Time) (float64, error) {
	var amount float64
	// Не помечаем наличкой текст, у которого есть парный распознанный чек в той
	// же группе (это перевод/дубль чека, а не наличка) — иначе фото денег,
	// присланное рядом, «воскресит» отброшенный дубль и задвоит сбор.
	err := d.pool.QueryRow(ctx, `
		UPDATE transactions t SET is_cash = true
		FROM raw_messages rm
		WHERE rm.id = t.raw_message_id
		  AND rm.sender_jid = $1
		  AND rm.wa_group_jid = $2
		  AND t.is_cash = false
		  AND t.ignored = false
		  AND rm.received_at >= $3
		  AND t.id = (
			SELECT t2.id FROM transactions t2
			JOIN raw_messages rm2 ON rm2.id = t2.raw_message_id
			WHERE rm2.sender_jid = $1 AND rm2.wa_group_jid = $2
			  AND t2.is_cash = false AND t2.ignored = false AND rm2.received_at >= $3
			  AND NOT EXISTS (
				SELECT 1 FROM bank_receipts brd
				WHERE brd.contact_id = t2.contact_id
				  AND brd.amount = t2.amount
				  AND brd.needs_review = false
				  AND brd.is_duplicate = false AND brd.ignored = false
				  AND COALESCE(brd.group_jid, '') = COALESCE(rm2.wa_group_jid, '')
			  )
			ORDER BY rm2.received_at DESC LIMIT 1
		  )
		RETURNING t.amount
	`, senderJID, groupJID, since).Scan(&amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return amount, err
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ---- Банковские чеки ----

type BankReceiptInput struct {
	RawMessageID      int
	Bank              string
	RecipientRaw      string // ФИО клиента (кому принадлежит чек — для атрибуции/поиска)
	RecipientBank     string
	RecipientPhone    string
	SenderRaw         string // ФИО отправителя (плательщик по чеку)
	SenderBank        string
	SenderAccount     string
	CardOwner         string // получатель, напечатанный на чеке (владелец карты), если отличается от клиента
	ContactID         *int   // nil, если получателя не удалось сопоставить с контактом
	Amount            float64
	Commission        float64
	DocNumber         string
	AuthCode          string
	Status            string
	NeedsReview       bool
	IsDuplicate       bool
	ClientConfirmed   bool      // клиент известен точно (ФИО написали рядом с чеком)
	ClientSourceRawID int       // сообщение с ФИО, которое дало чеку клиента (0 — нет)
	GroupJID          string    // группа, к которой относится чек (для дозагрузки из лички — целевая группа)
	SubmittedBy       string    // кто прислал чек; пусто для чеков из групп (там отправитель в raw_messages)
	TxDate            time.Time // время ОПЕРАЦИИ (с чека, если распозналось), не время получения сообщения
	Collapsed         bool      // свёрнутый экран банка (без даты/номера/получателя) — ждёт полного чека
	Ignored           bool      // в сбор не идёт (например, перевод отклонён банком)
	TxTimeGuessed     bool      // время операции с чека не прочиталось — TxDate это время сообщения
	TxDateOnly        bool      // на чеке только дата операции — время в TxDate выбрано (см. date_only.go)
	SupersededByRaw   int       // свёрнутый экран того же перевода, что полный чек этого сообщения (0 — нет)
}

func (d *DB) InsertBankReceipt(ctx context.Context, r BankReceiptInput) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, recipient_bank, recipient_phone, sender_raw, sender_bank, sender_account,
			 card_owner, contact_id, amount, commission, doc_number, auth_code, status, needs_review, is_duplicate,
			 client_confirmed, group_jid, submitted_by, tx_date, collapsed, ignored, client_source_raw_id, tx_time_guessed,
			 superseded_by_raw, tx_date_only)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, NULLIF($24, 0), $25,
			NULLIF($26, 0), $27)
	`, r.RawMessageID, nullIfEmpty(r.Bank), nullIfEmpty(r.RecipientRaw), nullIfEmpty(r.RecipientBank), nullIfEmpty(r.RecipientPhone),
		nullIfEmpty(r.SenderRaw), nullIfEmpty(r.SenderBank), nullIfEmpty(r.SenderAccount), nullIfEmpty(r.CardOwner),
		r.ContactID, r.Amount, r.Commission, nullIfEmpty(r.DocNumber), nullIfEmpty(r.AuthCode), nullIfEmpty(r.Status),
		r.NeedsReview, r.IsDuplicate, r.ClientConfirmed, nullIfEmpty(r.GroupJID), nullIfEmpty(r.SubmittedBy), r.TxDate, r.Collapsed, r.Ignored, r.ClientSourceRawID, r.TxTimeGuessed,
		r.SupersededByRaw, r.TxDateOnly)
	return err
}

// SupersededCollapsed — что вернул SupersedeCollapsedReceipt: нашли ли свёрнутый
// чек, который заменяет полный, и его атрибуцию (если владелец уже назвал клиента
// в ответ на свёрнутый) — чтобы перенести клиента на полный чек и не потерять сбор.
type SupersededCollapsed struct {
	Found           bool
	ContactID       *int
	RecipientRaw    string
	ClientConfirmed bool
}

// SupersedeCollapsedReceipt — пришёл ПОЛНЫЙ чек на ту же сумму от того же
// отправителя в той же группе: свёрнутый экран «Перевод выполнен», который он
// прислал до этого, — это тот же платёж. Помечаем свёрнутый дублем, иначе он
// навсегда висел бы в «не вошло» и задваивал «Максимум за период».
//
// Совпадение только когда ОТПРАВИТЕЛЬ ИЗВЕСТЕН (senderJID != "") — иначе
// пересланный из другой группы чек (там senderJID пустой) пометил бы дублем
// чужой свёрнутый платёж на ту же сумму и деньги молча пропали бы.
//
// Свёрнутые чеки помечены флагом collapsed — сопоставляем по нему, а НЕ по
// needs_review/contact_id: владелец мог уже ответить ФИО на свёрнутый (тогда
// contact_id заполнен, needs_review=false), и такой чек тоже нужно заменить.
// Окно — 3 дня по ВРЕМЕНИ ПОЛУЧЕНИЯ сообщения (rm.received_at), а не created_at:
// created_at — момент вставки ПОСЛЕ распознавания (вижн), он позже времени
// сообщения, из-за чего быстрый «вот полный» и разбор бэклога не срабатывали.
//
// Возвращает атрибуцию заменённого свёрнутого чека: если он был привязан к
// клиенту, вызывающий переносит клиента на полный чек (иначе платёж выпал бы).
//
// rank решает, ТОТ ЛИ это платёж и насколько вероятно (nil — любой свёрнутый,
// самый свежий): 0 — точно не тот (свёрнутый, уже записанный на клиента А, — не
// тот же платёж, что полный чек, подписанный клиентом Б, даже на ту же сумму;
// его не трогаем, иначе платёж А молча пропал бы). Из подходящих берём с самым
// высоким рангом (свёрнутый ТОГО ЖЕ клиента важнее ничьего), при равенстве —
// самый свежий.
//
// opTime — время операции, НАПЕЧАТАННОЕ на полном чеке (нулевое — не
// прочиталось). Экран показывает перевод, который уже сделан: экран, присланный
// в группу РАНЬШЕ, чем сделана операция полного чека (больше чем на пару минут
// — разница часов), — это другой перевод, его не заменяем. byRaw — сообщение
// полного чека: запоминаем у экрана, каким чеком он заменён.
func (d *DB) SupersedeCollapsedReceipt(ctx context.Context, groupJID, senderJID string, amount float64, before, opTime time.Time, byRaw int, rank func(SupersededCollapsed) int) (SupersededCollapsed, error) {
	var res SupersededCollapsed
	if amount <= 0 || groupJID == "" || senderJID == "" {
		return res, nil
	}
	var op *time.Time
	if !opTime.IsZero() {
		op = &opTime
	}
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, br.contact_id, COALESCE(br.recipient_raw, ''), br.client_confirmed
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND rm.sender_jid = $2
		  AND br.amount = $3::numeric
		  AND br.collapsed = true
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND rm.received_at >= $4::timestamptz - interval '3 days' AND rm.received_at <= $4::timestamptz
		  AND ($5::timestamptz IS NULL OR rm.received_at >= $5::timestamptz - interval '2 minutes')
		ORDER BY rm.received_at DESC LIMIT 20
	`, groupJID, senderJID, amount, before, op)
	if err != nil {
		return res, err
	}
	type cand struct {
		id int
		c  SupersededCollapsed
	}
	var cands []cand
	for rows.Next() {
		var x cand
		if err := rows.Scan(&x.id, &x.c.ContactID, &x.c.RecipientRaw, &x.c.ClientConfirmed); err != nil {
			rows.Close()
			return res, err
		}
		x.c.Found = true
		cands = append(cands, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	ranks := make([]int, len(cands))
	for i, x := range cands {
		ranks[i] = 1
		if rank != nil {
			ranks[i] = rank(x.c)
		}
	}
	for {
		best := -1
		for i := range cands {
			if ranks[i] > 0 && (best < 0 || ranks[i] > ranks[best]) {
				best = i // кандидаты идут от свежих к старым: при равном ранге — свежий
			}
		}
		if best < 0 {
			return res, nil
		}
		tag, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET is_duplicate = true, superseded_by_raw = NULLIF($2, 0)
			WHERE id = $1 AND is_duplicate = false AND collapsed = true`, cands[best].id, byRaw)
		if err != nil {
			return res, err
		}
		if tag.RowsAffected() == 1 {
			return cands[best].c, nil
		}
		ranks[best] = 0 // его уже кто-то заменил — берём следующий
	}
}

// CountedCheckForScreen — засчитанный полный чек, перевод которого, похоже,
// показывает пришедший следом свёрнутый экран.
type CountedCheckForScreen struct {
	Found  bool
	RawID  int       // сообщение полного чека
	WaID   string    // его wa_message_id
	Client string    // на кого записан
	TxDate time.Time // время операции с чека
}

// CountedCheckOfCollapsedScreen — пришёл свёрнутый экран «Перевод выполнен»
// (сумма без даты, получателя и номера) — обратный порядок к
// SupersedeCollapsedReceipt: тот же работник незадолго до этого (3 часа)
// прислал в эту группу ПОЛНЫЙ чек на ту же сумму, уже засчитанный клиенту, с
// напечатанным временем операции не позже, чем прислали экран, и ни с каким
// другим экраном этот чек ещё не сопоставлен. Тогда экран, скорее всего, того
// же перевода. Подходящих чеков два и больше — не угадываем (Found=false).
func (d *DB) CountedCheckOfCollapsedScreen(ctx context.Context, groupJID, senderJID string, amount float64, screenAt time.Time) (CountedCheckForScreen, error) {
	var res CountedCheckForScreen
	if amount <= 0 || groupJID == "" || senderJID == "" || senderJID == "bot-forward" {
		return res, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, COALESCE(rm.wa_message_id, ''), COALESCE(c.canonical_name, br.recipient_raw, ''), br.tx_date
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND rm.sender_jid = $2
		  AND br.amount = $3::numeric
		  AND br.collapsed = false AND br.tx_time_guessed = false
		  AND br.is_duplicate = false AND br.ignored = false AND br.needs_review = false
		  AND br.client_confirmed AND br.contact_id IS NOT NULL
		  AND COALESCE(rm.deleted, false) = false
		  AND rm.received_at BETWEEN $4::timestamptz - interval '3 hours' AND $4::timestamptz
		  AND br.tx_date <= $4::timestamptz + interval '2 minutes'
		  AND br.tx_date >= $4::timestamptz - interval '3 days'
		  AND NOT EXISTS (
			SELECT 1 FROM bank_receipts s
			WHERE s.superseded_by_raw = br.raw_message_id AND s.is_duplicate AND NOT s.ignored)
		ORDER BY rm.received_at DESC
		LIMIT 2
	`, groupJID, senderJID, amount, screenAt)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if err := rows.Scan(&res.RawID, &res.WaID, &res.Client, &res.TxDate); err != nil {
			return CountedCheckForScreen{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return CountedCheckForScreen{}, err
	}
	if n != 1 {
		return CountedCheckForScreen{}, nil
	}
	res.Found = true
	return res, nil
}

// MarkScreenAsked — вопрос бота по свёрнутому экрану сообщения rawID (в т.ч.
// уже признанному тем же переводом, что полный чек): ответ на него найдёт экран
// и после перезапуска бота.
func (d *DB) MarkScreenAsked(ctx context.Context, rawID int, askMsgID string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET clarify_asked = true, clarify_ask_msg_id = COALESCE(NULLIF($2, ''), clarify_ask_msg_id)
		WHERE raw_message_id = $1 AND collapsed = true`, rawID, askMsgID)
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
	return d.UnconfirmedReceiptsOrdered(ctx, groupJID, olderThan, limit, false)
}

// UnconfirmedReceiptsOrdered — newest=true: сначала самые свежие (когда чеков без ответа много).
func (d *DB) UnconfirmedReceiptsOrdered(ctx context.Context, groupJID string, olderThan time.Time, limit int, newest bool) ([]ClarifyReceipt, error) {
	order := "br.created_at"
	if newest {
		order = "br.created_at DESC"
	}
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''),
		       COALESCE(br.card_owner, br.recipient_raw, ''), br.amount::float8, br.tx_date
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.client_confirmed = false AND br.clarify_asked = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  `+notGroupForwardCopy+`
		  AND br.amount > 0
		  AND br.created_at < $2
		ORDER BY `+order+`
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

// RecentSenderTexts возвращает тексты последних НЕ-медиа сообщений отправителя
// в группе с момента since (свежие первыми). Нужны, чтобы перед вопросом «чей
// это чек» проанализировать, не написали ли ФИО клиента рядом отдельным
// сообщением («Магамадов Алха\n22.000₽»).
func (d *DB) RecentSenderTexts(ctx context.Context, groupJID, senderJID string, since time.Time, limit int) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT body FROM raw_messages
		WHERE wa_group_jid = $1 AND sender_jid = $2
		  AND received_at >= $3 AND COALESCE(deleted, false) = false
		  AND COALESCE(body, '') <> ''
		  -- Только текстовые сообщения: подпись к фото/OCR-текст чека — не ФИО клиента.
		  AND has_media = false
		  -- Ответ на вопрос бота / обращение к ассистенту — не подпись к другому чеку.
		  AND handled_as IS NULL
		ORDER BY received_at DESC
		LIMIT $4
	`, groupJID, senderJID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		out = append(out, body)
	}
	return out, rows.Err()
}

// UnrecognizedReceipts — чеки, у которых бот НЕ смог прочитать сумму
// (amount <= 0), по которым ещё не спрашивал. Это фото/PDF, где ни OCR, ни ИИ,
// ни зрение не вытащили сумму — бот спросит в группе, что на чеке.
func (d *DB) UnrecognizedReceipts(ctx context.Context, groupJID string, olderThan time.Time, limit int) ([]ClarifyReceipt, error) {
	return d.UnrecognizedReceiptsOrdered(ctx, groupJID, olderThan, limit, false)
}

// UnrecognizedReceiptsOrdered — newest=true: сначала самые свежие (когда чеков без ответа много).
func (d *DB) UnrecognizedReceiptsOrdered(ctx context.Context, groupJID string, olderThan time.Time, limit int, newest bool) ([]ClarifyReceipt, error) {
	order := "br.created_at"
	if newest {
		order = "br.created_at DESC"
	}
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''),
		       COALESCE(br.card_owner, br.recipient_raw, ''), br.amount::float8, br.tx_date
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.clarify_asked = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  `+notGroupForwardCopy+`
		  AND br.amount <= 0
		  AND br.created_at < $2
		ORDER BY `+order+`
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

// CountUnrecognized — сколько чеков с непрочитанной суммой в группе старше olderThan.
func (d *DB) CountUnrecognized(ctx context.Context, groupJID string, olderThan time.Time) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.clarify_asked = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  `+notGroupForwardCopy+`
		  AND br.amount <= 0
		  AND br.created_at < $2
	`, groupJID, olderThan).Scan(&n)
	return n, err
}

// FillReceiptByMessage применяет ответ владельца на вопрос про чек:
//   - если задано имя ($2 != ”) — ставит клиента (recipient + contact);
//   - если задана сумма ($4 > 0) — ставит/ИСПРАВЛЯЕТ сумму (в т.ч. поверх
//     ошибочно прочитанной — для правки подозрительных сумм);
//   - пустой ответ (только «да/верно») просто снимает флаги (подтверждение).
//
// Возвращает итоговую сумму чека.
//
// Тот же чек, уже учтённый в группе, ищет DedupeCompletedReceipt (без вопроса в
// группе, поэтому самое слабое совпадение — у чека не прочиталось время — не
// ищет). Кто может спросить «тот же чек или другой?», вызывает
// FillReceiptByMessageID и DedupeCompletedReceipt сам.
func (d *DB) FillReceiptByMessage(ctx context.Context, waMessageID, name string, contactID *int, amount float64) (found bool, amount_ float64, needsReview bool, err error) {
	rid, found, got, nr, err := d.FillReceiptByMessageID(ctx, waMessageID, name, contactID, amount)
	if err != nil || !found {
		return found, got, nr, err
	}
	if name != "" || amount > 0 {
		// Чек стал «чьим» (или полным) — тот же чек, присланный ещё раз, больше не
		// считаем вторым платежом.
		if _, e := d.DedupeCompletedReceipt(ctx, rid, amount > 0, false); e != nil {
			fmt.Println("Проверка повтора чека после ответа не удалась:", e)
		}
	}
	return true, got, nr, nil
}

// FillReceiptByMessageID — FillReceiptByMessage без поиска повтора; возвращает
// и id заполненного чека.
func (d *DB) FillReceiptByMessageID(ctx context.Context, waMessageID, name string, contactID *int, amount float64) (rid int, found bool, amount_ float64, needsReview bool, err error) {
	var got float64
	var nr bool
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET
			card_owner    = CASE WHEN $2 = '' THEN card_owner ELSE COALESCE(NULLIF(card_owner, ''), NULLIF(recipient_raw, '')) END,
			recipient_raw = CASE WHEN $2 = '' THEN recipient_raw ELSE $2 END,
			contact_id    = CASE WHEN $2 = '' THEN contact_id    ELSE $3 END,
			-- $4::numeric ОБЯЗАТЕЛЕН: иначе Postgres выводит тип $4 как integer по
			-- первому сравнению с 0, и pgx молча обрезает float64 (22000.50 -> 22000).
			amount        = CASE WHEN $4::numeric > 0 THEN $4::numeric ELSE amount END,
			-- needs_review снимаем ТОЛЬКО если у чека есть И сумма, И ИЗВЕСТНЫЙ клиент.
			-- «Известный» = имя назвали в этом ответе ($2<>'') ИЛИ клиент уже был
			-- подтверждён. Ответ ОДНОЙ суммой на чек без клиента НЕ засчитывает его
			-- под владельцем карты (contact_id там — печатный получатель, а не клиент):
			-- иначе деньги молча падали в сбор не на того человека.
			needs_review  = CASE WHEN (CASE WHEN $4::numeric > 0 THEN $4::numeric ELSE amount END) > 0
			                          AND ($2 <> '' OR client_confirmed)
			                     THEN false ELSE true END,
			-- Ответ ТОЛЬКО суммой (имя не назвали) НЕ подтверждает клиента: сумму
			-- поправили, но кто клиент — всё ещё неизвестно, поэтому не гасим
			-- флаги, чтобы бот мог спросить «чей это чек?». Имя в ответе или
			-- простое «да/верно» — подтверждают.
			client_confirmed = CASE WHEN $2 = '' AND $4::numeric > 0 THEN client_confirmed ELSE true END,
			clarify_asked    = CASE WHEN $2 = '' AND $4::numeric > 0 THEN clarify_asked    ELSE true END
		WHERE id = (
			SELECT br.id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1
		)
		RETURNING id, amount::float8, needs_review
	`, waMessageID, name, contactID, amount).Scan(&rid, &got, &nr)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, 0, false, nil
	}
	if err != nil {
		return 0, false, 0, false, err
	}
	if amount > 0 {
		_ = d.SyncWatchWithReceipt(ctx, rid)
	}
	return rid, true, got, nr, nil
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
		  AND COALESCE(rm.deleted, false) = false
		  `+notGroupForwardCopy+`
		  AND br.amount > 0
		  AND br.created_at < $2
	`, groupJID, olderThan).Scan(&n)
	return n, err
}

// GroupAmountMedian — медианная сумма чеков группы за период (since..) и число
// чеков, на которых она посчитана. Нужна, чтобы ловить подозрительно большие
// суммы (напр. OCR/зрение приписали лишний ноль): 130 000 -> 1 300 000.
func (d *DB) GroupAmountMedian(ctx context.Context, groupJID string, since time.Time) (float64, int, error) {
	var median float64
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY br.amount), 0)::float8, COUNT(*)
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		  AND br.created_at >= $2
	`, groupJID, since).Scan(&median, &n)
	return median, n, err
}

// MarkReceiptAsked помечает, что бот уже спросил про этот чек, и запоминает id
// сообщения-вопроса (askMsgID, может быть пустым) — по нему ответ владельца
// найдёт чек даже после перезапуска бота.
func (d *DB) MarkReceiptAsked(ctx context.Context, receiptID int, askMsgID string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts
		SET clarify_asked = true,
		    clarify_ask_msg_id = COALESCE(NULLIF($2, ''), clarify_ask_msg_id)
		WHERE id = $1
	`, receiptID, askMsgID)
	return err
}

// MarkReceiptAskedByMessage помечает «уже спросили» чек по id его сообщения в
// WhatsApp — когда бот сразу флажит нераспознанный чек прямо в группе, чтобы
// клариф-цикл не переспросил о нём повторно. askMsgID — id вопроса бота (для
// привязки ответа после перезапуска), может быть пустым.
func (d *DB) MarkReceiptAskedByMessage(ctx context.Context, waMessageID, askMsgID string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts
		SET clarify_asked = true,
		    clarify_ask_msg_id = COALESCE(NULLIF($2, ''), clarify_ask_msg_id)
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false
			ORDER BY br.id DESC LIMIT 1
		)
	`, waMessageID, askMsgID)
	return err
}

// ReceiptWaIDByAskMsg — по id сообщения-вопроса бота («чей чек?», «проверьте
// сумму») находит wa_message_id самого чека. Резерв на случай, если связь в
// памяти (askMap) потерялась после перезапуска.
func (d *DB) ReceiptWaIDByAskMsg(ctx context.Context, askMsgID string) (string, bool, error) {
	if askMsgID == "" {
		return "", false, nil
	}
	var waID string
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(rm.wa_message_id, '')
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.clarify_ask_msg_id = $1
		ORDER BY br.id DESC LIMIT 1
	`, askMsgID).Scan(&waID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return waID, waID != "", nil
}

// ClientAlreadyAttributedFrom — этот клиент уже привязан к ДРУГОМУ чеку того же
// отправителя в группе за недавнее время? Нужно, чтобы ФИО из соседнего
// сообщения не «прилипало» ко второму чеку: «Иванов» + чек1 + чек2 — Иванов
// принадлежит чеку1, а чек2 надо спросить.
func (d *DB) ClientAlreadyAttributedFrom(ctx context.Context, groupJID, senderJID string, contactID int, exceptWaMessageID string, since time.Time) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
			  AND rm.sender_jid = $2
			  AND br.contact_id = $3
			  AND COALESCE(rm.wa_message_id, '') <> $4
			  AND rm.received_at >= $5
			  AND br.is_duplicate = false AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
		)
	`, groupJID, senderJID, contactID, exceptWaMessageID, since).Scan(&exists)
	return exists, err
}

// ClientChecksOfAmountFrom — сколько ДРУГИХ чеков того же отправителя в группе
// за недавнее время уже привязано к этому клиенту на эту сумму. Строка «ФИО
// сумма» называет один чек на свою сумму: «Исаев Харон 5000» + два чека по
// 5000 — второй надо спросить, а чек на 10000 из «Исаев Харон\n10000\n5000»
// чеку на 5000 не мешает.
func (d *DB) ClientChecksOfAmountFrom(ctx context.Context, groupJID, senderJID string, contactID int, exceptWaMessageID string, since time.Time, amount float64) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT count(*) FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND rm.sender_jid = $2
		  AND br.contact_id = $3
		  AND COALESCE(rm.wa_message_id, '') <> $4
		  AND rm.received_at >= $5
		  AND abs(br.amount - $6::float8) < 1
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
	`, groupJID, senderJID, contactID, exceptWaMessageID, since, amount).Scan(&n)
	return n, err
}

// DuplicateWindow — окно вокруг времени операции, в котором совпадение
// получателя и суммы считается вероятным повтором одного и того же чека
// (например, кто-то по ошибке переслал одно и то же фото дважды).
const DuplicateWindow = 5 * time.Minute

// FindDuplicateReceipt проверяет, не встречался ли уже такой же чек — по
// совокупности всех параметров сразу:
//   - получатель (сопоставленный контакт, а если он не определён — ФИО как в чеке);
//   - сумма (точное совпадение);
//   - время операции: напечатанное на обоих чеках — то же самое, угаданное
//     (не прочиталось) — в пределах DuplicateWindow;
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
//
// txGuessed — время операции не прочиталось с чека (txDate — время сообщения):
// тогда совпадение по времени ищем в окне DuplicateWindow; напечатанное время
// должно совпасть (см. sameOpTimeSQL).
func (d *DB) FindDuplicateReceipt(ctx context.Context, groupJID, docNumber, authCode string, contactID *int, recipientRaw string, amount float64, txDate time.Time, txGuessed bool) (bool, time.Time, error) {
	dup, err := d.FindDuplicateReceiptEx(ctx, groupJID, docNumber, authCode, contactID, recipientRaw, "", amount, txDate, txGuessed)
	return dup.Found, dup.TxDate, err
}

// DuplicateMatch — найденный ранее тот же чек.
type DuplicateMatch struct {
	Found     bool
	TxDate    time.Time
	Client    string // на кого записан найденный чек
	StrongID  bool   // совпал номер операции/документа или код авторизации
	WaMessage string // id WhatsApp сообщения с найденным чеком
}

// FindDuplicateReceiptEx ищет тот же чек, присланный раньше. Совпадение
// номера операции (документа) или кода авторизации — это тот же чек, КАК БЫ
// его ни подписали. Без номеров — тот же клиент/получатель, та же сумма,
// то же время операции и тот же ПЛАТЕЛЬЩИК на чеке: два разных клиента,
// перечислившие одинаковую сумму на одну карту, — не дубль. Время: оба
// напечатаны — одно и то же (до секунды; до минуты, если чек печатает только
// минуты), иначе это разные переводы; угадано хоть одно — окно DuplicateWindow.
func (d *DB) FindDuplicateReceiptEx(ctx context.Context, groupJID, docNumber, authCode string, contactID *int, recipientRaw, senderRaw string, amount float64, txDate time.Time, txGuessed bool) (DuplicateMatch, error) {
	var m DuplicateMatch
	// Номера на чеке может не быть (NULL в базе) — «совпал номер» должен быть
	// false, а не NULL: иначе Scan в bool падает и повтор не ловится вовсе.
	err := d.pool.QueryRow(ctx, `
		SELECT br.tx_date, COALESCE(c.canonical_name, br.recipient_raw, ''),
		       COALESCE(($7 <> '' AND br.doc_number = $7) OR ($8 <> '' AND br.auth_code = $8), false),
		       COALESCE(rm.wa_message_id, '')
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.amount = $1
		  AND ($6 = '' OR br.group_jid = $6)
		  AND br.is_duplicate = false
		  AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND (
		    -- Совпал сильный идентификатор — тот же чек, независимо от подписи и
		    -- времени (дата могла не прочитаться на одной из копий). Код
		    -- авторизации короткий — для него ограничиваем окно в несколько дней.
		    ($7 <> '' AND br.doc_number = $7)
		    OR ($8 <> '' AND br.auth_code = $8
		        AND br.tx_date BETWEEN $2::timestamptz - interval '3 days' AND $3::timestamptz + interval '3 days')
		    -- Эвристика для чеков без номеров: тот же клиент, окно по времени,
		    -- номера не противоречат и плательщик на чеке тот же.
		    OR (
		      (($4::int IS NOT NULL AND br.contact_id = $4) OR ($4::int IS NULL AND br.recipient_raw = $5))
		      AND br.tx_date BETWEEN $2::timestamptz AND $3::timestamptz
		      AND `+sameOpTimeSQL("br.tx_date", "br.tx_time_guessed", "$11::timestamptz", "$10::boolean")+`
		      AND (COALESCE(br.doc_number, '') = '' OR $7 = '' OR br.doc_number = $7)
		      AND (COALESCE(br.auth_code, '') = '' OR $8 = '' OR br.auth_code = $8)
		      AND (COALESCE(br.sender_raw, '') = '' OR $9 = ''
		           OR lower(split_part(trim(br.sender_raw), ' ', 1)) = lower(split_part(trim($9), ' ', 1)))
		    )
		  )
		ORDER BY 3 DESC, br.tx_date
		LIMIT 1
	`, amount, txDate.Add(-DuplicateWindow), txDate.Add(DuplicateWindow), contactID, recipientRaw, groupJID, docNumber, authCode, senderRaw,
		txGuessed, txDate).
		Scan(&m.TxDate, &m.Client, &m.StrongID, &m.WaMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	m.Found = true
	return m, nil
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
	// Если удалили сообщение с ОРИГИНАЛОМ чека, а его копию бот пометил дублем в
	// той же группе — повышаем ОДНУ живую копию до оригинала. Иначе платёж исчез
	// бы совсем: оригинал удалён, копия скрыта как дубль. Ровно одну (свежую) на
	// каждый удалённый оригинал — чтобы не задвоить, если копий несколько.
	// Копия без ФИО (клиентом у неё записан владелец карты), повышенная после
	// удаления оригинала, снова ждёт ответа «чей чек?», а не уходит в сбор на
	// владельца карты — подписан был оригинал или нет (копию-повтор вставляют
	// без needs_review: клиента у оригинала не спрашивали дважды).
	if _, e := d.pool.Exec(ctx, `
		UPDATE bank_receipts u SET is_duplicate = false, superseded_by_raw = NULL,
			needs_review = u.needs_review OR NOT u.client_confirmed
		FROM (
			SELECT DISTINCT ON (orig.id) dd.id
			FROM bank_receipts orig
			JOIN bank_receipts dd ON dd.id <> orig.id AND dd.is_duplicate = true AND dd.ignored = false
				AND COALESCE(dd.group_jid, '') = COALESCE(orig.group_jid, '')
				AND (
					-- Свёрнутый экран, заменённый этим полным чеком.
					dd.superseded_by_raw = orig.raw_message_id OR
					(NULLIF(orig.doc_number, '') IS NOT NULL AND dd.doc_number = orig.doc_number) OR
					(NULLIF(orig.auth_code, '') IS NOT NULL AND dd.auth_code = orig.auth_code) OR
					(dd.contact_id = orig.contact_id AND dd.amount = orig.amount
					 AND `+sameOpTimeSQL("dd.tx_date", "dd.tx_time_guessed", "orig.tx_date", "orig.tx_time_guessed")+`) OR
					-- Тот же напечатанный чек (копия без ФИО при подписанном оригинале).
					`+printedMatchSQL("dd", "orig")+` OR
					-- Копия без прочитанного времени, признанная «похоже, тот же»
					-- (DedupeCompletedReceipt): тот же клиент и сумма.
					(dd.tx_time_guessed AND dd.client_confirmed AND orig.client_confirmed
					 AND dd.contact_id = orig.contact_id AND dd.amount = orig.amount
					 AND orig.tx_date <= (SELECT received_at FROM raw_messages WHERE id = dd.raw_message_id) + interval '2 minutes'
					 AND orig.tx_date >= (SELECT received_at FROM raw_messages WHERE id = dd.raw_message_id) - interval '3 days')
				)
			JOIN raw_messages drm ON drm.id = dd.raw_message_id AND COALESCE(drm.deleted, false) = false
			WHERE orig.raw_message_id = $1 AND orig.is_duplicate = false AND orig.ignored = false
			ORDER BY orig.id, dd.created_at DESC
		) sel
		WHERE u.id = sel.id
	`, rawID); e != nil {
		fmt.Println("Промоут дубля после удаления оригинала не удался:", e)
	}
	d.releaseMessageLinks(ctx, rawID)
	if e := d.restoreSameCashOf(ctx, rawID); e != nil {
		fmt.Println("Возврат налички, признанной повтором удалённой, не удался:", e)
	}
	// Удалили поправку «не 5000 а 6000» — сумма возвращается.
	if _, e := d.RevertCorrections(ctx, rawID); e != nil {
		fmt.Println("Откат поправки суммы удалённого сообщения не удался:", e)
	}
	// Копии этого чека в других группах — тот же платёж: тоже убираем.
	if _, e := d.pool.Exec(ctx, `
		UPDATE bank_receipts cp SET ignored = true FROM raw_messages crm
		WHERE cp.raw_message_id = crm.id AND crm.sender_jid = 'bot-forward'
		  AND starts_with(crm.wa_message_id, $1 || '-fwd-')`, waMessageID); e != nil {
		fmt.Println("Удаление копий пересланного чека не удалось:", e)
	}
	// Сверка по удалённому чеку больше не нужна — ни вопросов, ни напоминаний.
	if _, e := d.pool.Exec(ctx, `UPDATE cmf_watch SET status = 'deleted' WHERE raw_message_id = $1 AND status <> 'found'`, rawID); e != nil {
		fmt.Println("Закрытие сверки удалённого чека не удалось:", e)
	}
	if err = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transactions WHERE raw_message_id = $1 AND ignored = false`, rawID).Scan(&txCount); err != nil {
		return 0, 0, err
	}
	if err = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM bank_receipts WHERE raw_message_id = $1 AND ignored = false AND is_duplicate = false`, rawID).Scan(&receiptCount); err != nil {
		return 0, 0, err
	}
	return txCount, receiptCount, nil
}

// releaseMessageLinks — сообщение удалили или изменили: его имя больше не ждёт
// чека, чеки, которым оно дало клиента, снова ждут клиента, а наличку,
// придержанную как «повтор» его налички, отпускаем.
func (d *DB) releaseMessageLinks(ctx context.Context, rawID int) {
	if _, e := d.pool.Exec(ctx, `UPDATE pending_client_names SET consumed = true, consumed_at = now() WHERE raw_message_id = $1 AND consumed = false`, rawID); e != nil {
		fmt.Println("Очередь имён: не снял имя удалённого сообщения:", e)
	}
	// Сверка этих чеков тоже забывает клиента: напоминать «занесите за X» по
	// чеку, у которого клиента отозвали, нельзя.
	if _, e := d.pool.Exec(ctx, `
		UPDATE cmf_watch SET client_text = NULL, client_id = NULL, client_name = NULL, candidates = NULL, status = 'noname'
		WHERE status NOT IN ('found', 'deleted', 'not_in_program')
		  AND raw_message_id IN (SELECT raw_message_id FROM bank_receipts WHERE client_source_raw_id = $1)`, rawID); e != nil {
		fmt.Println("Откат клиента сверки не удался:", e)
	}
	if _, e := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET
			recipient_raw = COALESCE(NULLIF(card_owner, ''), recipient_raw),
			needs_review = true, client_confirmed = false, client_source_raw_id = NULL,
			-- клиента отозвали — бот снова спросит «чей чек?»
			clarify_asked = false, clarify_ask_msg_id = NULL
		WHERE client_source_raw_id = $1`, rawID); e != nil {
		fmt.Println("Откат привязки клиента от удалённого сообщения не удался:", e)
	}
	// Удалили наличку, из-за которой следующую такую же придержали как «повтор»,
	// — повтора больше нет: отпускаем ОДНУ придержанную (если других живых
	// таких же нет).
	if e := d.releaseCashRepeatOf(ctx, rawID); e != nil {
		fmt.Println("Освобождение придержанной налички после удаления не удалось:", e)
	}
}

// releaseCashRepeatOf — наличку сообщения rawID удалили (или правят): по каждой
// её строке отпускаем ОДНУ наличку, придержанную как её повтор (тот же клиент
// или то же имя, записанное иначе; та же сумма), если других живых таких же,
// повтором которых она может быть, больше нет.
func (d *DB) releaseCashRepeatOf(ctx context.Context, rawID int) error {
	type cashRow struct {
		contactID int
		name      string
		amount    float64
	}
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(t.contact_id, 0), COALESCE(c.canonical_name, t.raw_name, ''), t.amount::float8
		FROM transactions t LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.raw_message_id = $1 AND t.is_cash = true AND t.amount > 0
		ORDER BY t.id`, rawID)
	if err != nil {
		return err
	}
	var dels []cashRow
	for rows.Next() {
		var r cashRow
		if err := rows.Scan(&r.contactID, &r.name, &r.amount); err != nil {
			rows.Close()
			return err
		}
		if !cashPlaceholder(r.name) {
			dels = append(dels, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, del := range dels {
		hrows, err := d.pool.Query(ctx, `
			SELECT h.id, COALESCE(h.contact_id, 0), COALESCE(c.canonical_name, h.raw_name, ''),
			       h.tx_date, h.raw_message_id
			FROM transactions h
			JOIN raw_messages hrm ON hrm.id = h.raw_message_id
			LEFT JOIN contacts c ON c.id = h.contact_id
			WHERE h.dup_pending = true AND h.ignored = false AND h.is_cash = true
			  AND COALESCE(hrm.deleted, false) = false AND h.amount = $1
			ORDER BY h.created_at, h.id
			LIMIT 200`, del.amount)
		if err != nil {
			return err
		}
		var held []cashRepeatProbe
		for hrows.Next() {
			p := cashRepeatProbe{Amount: del.amount, Later: true}
			if err := hrows.Scan(&p.TxID, &p.ContactID, &p.Name, &p.TxDate, &p.RawID); err != nil {
				hrows.Close()
				return err
			}
			held = append(held, p)
		}
		hrows.Close()
		if err := hrows.Err(); err != nil {
			return err
		}
		for _, h := range held {
			if !(del.contactID != 0 && h.ContactID == del.contactID || SameCashClient(del.name, h.Name)) {
				continue
			}
			if _, still, err := d.cashRepeatPrev(ctx, h); err != nil || still {
				continue // есть другая живая такая же наличка — повтор остаётся вопросом
			}
			if _, err := d.pool.Exec(ctx, `UPDATE transactions SET dup_pending = false WHERE id = $1 AND dup_pending = true`, h.TxID); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// OperationRef — ссылка на операцию (текстовый платёж или чек) для ручных
// корректировок через ассистента.
type OperationRef struct {
	Kind     string // "transaction" | "receipt"
	ID       int
	Name     string
	Amount   float64
	TxDate   time.Time
	Ignored  bool
	GroupJID string
	Cash     bool
	Received time.Time // когда прислали в чат
	// Кто прислал запись (имя из памяти номеров / WhatsApp), его номер и у кого
	// деньги — чтобы владелец мог сказать «исключи текст Ислама».
	Sender    string
	Phone     string
	Collector string
}

// FindOperations ищет операции по сумме с необязательными уточнениями:
// имя человека (подстрока), диапазон дат, группа. Возвращает до 30 последних —
// используется ассистентом, чтобы найти операцию, которую владелец просит
// исключить из учёта или вернуть обратно.
func (d *DB) FindOperations(ctx context.Context, amount float64, person string, from, to *time.Time, groupJID string) ([]OperationRef, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT * FROM (
			SELECT 'transaction' AS kind, t.id, COALESCE(c.canonical_name, t.raw_name) AS name,
			       t.amount::float8 AS amount, t.tx_date::timestamptz AS tx_date,
			       COALESCE(rm.wa_group_jid, '') AS group_jid, t.ignored,
			       t.is_cash AS cash, COALESCE(rm.received_at, t.tx_date::timestamptz) AS received,
			       COALESCE(NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), '') AS sender,
			       split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
			       COALESCE(t.collector, '') AS collector
			FROM transactions t
			LEFT JOIN contacts c ON c.id = t.contact_id
			LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
			WHERE t.amount = $1 AND COALESCE(rm.deleted, false) = false

			UNION ALL

			SELECT 'receipt', br.id, COALESCE(c.canonical_name, br.recipient_raw, ''),
			       br.amount::float8, br.tx_date, COALESCE(br.group_jid, ''), br.ignored,
			       false, COALESCE(rm.received_at, br.tx_date),
			       COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), ''),
			       split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1),
			       COALESCE(br.collector, '')
			FROM bank_receipts br
			LEFT JOIN contacts c ON c.id = br.contact_id
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
			WHERE br.amount = $1 AND br.is_duplicate = false AND COALESCE(rm.deleted, false) = false
		) ops
		WHERE ($2 = '' OR ops.name ILIKE '%' || $2 || '%')
		  AND ($3::timestamptz IS NULL OR ops.tx_date >= $3)
		  AND ($4::timestamptz IS NULL OR ops.tx_date < $4)
		  AND ($5 = '' OR ops.group_jid = $5)
		ORDER BY ops.tx_date DESC, ops.received DESC
		LIMIT 30
	`, amount, person, from, to, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OperationRef
	for rows.Next() {
		var op OperationRef
		var groupJID string
		if err := rows.Scan(&op.Kind, &op.ID, &op.Name, &op.Amount, &op.TxDate, &groupJID, &op.Ignored, &op.Cash, &op.Received,
			&op.Sender, &op.Phone, &op.Collector); err != nil {
			return nil, err
		}
		op.GroupJID = groupJID
		out = append(out, op)
	}
	return out, rows.Err()
}

// ClearDuplicateFlag снимает пометку "дубль" с последнего чека, помеченного
// дублем, подходящего по сумме (и, если заданы, по имени/группе) — когда
// владелец говорит "это не дубль, засчитай". Возвращает данные снятого чека.
func (d *DB) ClearDuplicateFlag(ctx context.Context, amount float64, person, groupJID string) (found bool, name string, txDate time.Time, err error) {
	found, name, txDate, _, err = d.ClearDuplicateFlagRef(ctx, amount, person, groupJID)
	return
}

// ClearDuplicateFlagRef — то же, плюс id WhatsApp сообщения с чеком.
func (d *DB) ClearDuplicateFlagRef(ctx context.Context, amount float64, person, groupJID string) (found bool, name string, txDate time.Time, waID string, err error) {
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
		RETURNING COALESCE((SELECT canonical_name FROM contacts WHERE id = bank_receipts.contact_id), recipient_raw, ''), tx_date,
		          COALESCE((SELECT wa_message_id FROM raw_messages WHERE id = bank_receipts.raw_message_id), '')
	`, amount, person, groupJID).Scan(&name, &txDate, &waID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", time.Time{}, "", nil
	}
	if err != nil {
		return false, "", time.Time{}, "", err
	}
	return true, name, txDate, waID, nil
}

// BookSpans — ОДНА книга движка подсчёта по записям нескольких несмежных
// периодов [from, to) (с тем же запасом по краям, что у LoadBook; запись,
// попавшая в несколько периодов, берётся один раз). Старый чек (дата операции
// на месяц раньше присылки) и текст, присланный рядом с ним, должны оказаться в
// одной книге: в книге «вокруг даты операции» текста нет, и чек там — платёж
// без своей пары.
func (d *DB) BookSpans(ctx context.Context, spans [][2]time.Time) (*ledger.Book, error) {
	var checks []ledger.Check
	var texts []ledger.Text
	seenC, seenT := map[int]bool{}, map[int]bool{}
	for _, s := range spans {
		cs, err := d.loadLedgerChecks(ctx, s[0].Add(-ledgerPad), s[1].Add(ledgerPad), "")
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if !seenC[c.ID] {
				seenC[c.ID] = true
				checks = append(checks, c)
			}
		}
		ts, err := d.loadLedgerTexts(ctx, s[0].Add(-ledgerPad), s[1].Add(ledgerPad), "")
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if !seenT[t.ID] {
				seenT[t.ID] = true
				texts = append(texts, t)
			}
		}
	}
	return ledger.Build(checks, texts), nil
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
	q := `UPDATE ` + table + ` SET ignored = $2 WHERE id = $1`
	if table == "transactions" {
		// Ручное решение владельца важнее ответа «тот же»: такую наличку
		// удаление другого сообщения не вернёт.
		q = `UPDATE transactions SET ignored = $2, dup_same_as = NULL WHERE id = $1`
	}
	_, err := d.pool.Exec(ctx, q, id, ignored)
	if err == nil && kind == "receipt" {
		d.lockCopy(ctx, id)
	}
	return err
}

// ReceiptTextTwins — живые текстовые платежи того же контакта и суммы, что чек
// (перевод, присланы в пределах ±2 дней): при смене клиента чека они следуют
// за ним. Исключение и поправка суммы берут пару чек↔текст из движка подсчёта
// (bot.payIndex), а не отсюда: движок объединяет с чеком ровно один текст.
func (d *DB) ReceiptTextTwins(ctx context.Context, receiptID int) ([]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id FROM bank_receipts br
		LEFT JOIN raw_messages brm ON brm.id = br.raw_message_id
		JOIN transactions t ON t.contact_id = br.contact_id AND t.amount = br.amount
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE br.id = $1 AND br.contact_id IS NOT NULL
		  AND t.is_cash = false AND t.ignored = false AND t.dup_pending = false
		  AND COALESCE(rm.deleted, false) = false
		  -- В любой группе: в сборе по всем группам текст в другой группе тоже
		  -- считается копией этого чека (groupPairClause).
		  AND COALESCE(rm.received_at, t.tx_date::timestamptz) >= COALESCE(brm.received_at, br.tx_date) - interval '2 days'
		  AND COALESCE(rm.received_at, t.tx_date::timestamptz) <  COALESCE(brm.received_at, br.tx_date) + interval '2 days'
		ORDER BY t.id`, receiptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CorrTarget — платёж (текст или чек), который может исправить поправка суммы.
type CorrTarget struct {
	Kind      string // "tx" | "receipt"
	ID        int
	WaID      string
	Name      string
	Amount    float64
	ContactID int
	Cash      bool
	Received  time.Time
}

const corrTargetsSQL = `
	SELECT * FROM (
		SELECT 'tx' AS kind, t.id, COALESCE(rm.wa_message_id, '') AS wa, COALESCE(c.canonical_name, t.raw_name, '') AS name,
		       t.amount::float8 AS amount, COALESCE(t.contact_id, 0) AS cid, t.is_cash AS cash, rm.received_at AS at,
		       rm.wa_group_jid AS g, COALESCE(rm.sender_jid, '') AS snd
		FROM transactions t JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.ignored = false AND COALESCE(rm.deleted, false) = false
		UNION ALL
		SELECT 'receipt', br.id, COALESCE(rm.wa_message_id, ''), COALESCE(c.canonical_name, br.recipient_raw, ''),
		       br.amount::float8, COALESCE(br.contact_id, 0), false, rm.received_at,
		       COALESCE(br.group_jid, rm.wa_group_jid), COALESCE(rm.sender_jid, '')
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.ignored = false AND br.is_duplicate = false AND COALESCE(rm.deleted, false) = false
	) x `

func scanCorrTargets(rows pgx.Rows) ([]CorrTarget, error) {
	defer rows.Close()
	var out []CorrTarget
	for rows.Next() {
		var c CorrTarget
		var g, snd string
		if err := rows.Scan(&c.Kind, &c.ID, &c.WaID, &c.Name, &c.Amount, &c.ContactID, &c.Cash, &c.Received, &g, &snd); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CorrTargetsOfMessage — платежи сообщения (свайп поправкой на него).
func (d *DB) CorrTargetsOfMessage(ctx context.Context, waID string) ([]CorrTarget, error) {
	rows, err := d.pool.Query(ctx, corrTargetsSQL+`WHERE x.wa = $1 ORDER BY x.kind DESC, x.id`, waID)
	if err != nil {
		return nil, err
	}
	return scanCorrTargets(rows)
}

// CorrTargetsFromSender — свежие платежи сотрудника на сумму amount (поправка
// без свайпа).
func (d *DB) CorrTargetsFromSender(ctx context.Context, groupJID, senderJID string, amount float64, since time.Time) ([]CorrTarget, error) {
	rows, err := d.pool.Query(ctx, corrTargetsSQL+`WHERE x.g = $1 AND x.snd = $2 AND abs(x.amount - $3) < 1 AND x.at >= $4 ORDER BY x.at DESC`,
		groupJID, senderJID, amount, since)
	if err != nil {
		return nil, err
	}
	return scanCorrTargets(rows)
}

// SetReceiptAmount — поправить сумму чека (сверка ждёт новую сумму).
func (d *DB) SetReceiptAmount(ctx context.Context, receiptID int, amount float64) error {
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET amount = $2::numeric WHERE id = $1`, receiptID, amount); err != nil {
		return err
	}
	return d.SyncWatchWithReceipt(ctx, receiptID)
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
// UnclearItems — непонятые ЧЕКИ. from/to (необязательно) фильтруют по дате
// присылки в чат (received_at), чтобы «нераспознанные за сентябрь» не мешались с
// августовскими. Ветка 'message' (медиа без чека/платежа) берёт ТОЛЬКО ещё не
// обработанные (parsed=false): фото, которые бот классифицировал как НЕ чек
// (паспорт, случайная картинка) или как наличку, помечаются parsed и сюда НЕ
// попадают — иначе в «нераспознанных чеках» оказывались бы паспорта и фото денег.
func (d *DB) UnclearItems(ctx context.Context, groupJID string, from, to *time.Time, limit int) ([]UnclearItem, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT 'receipt', br.id, COALESCE(br.group_jid, ''), COALESCE(br.recipient_raw, ''),
		        COALESCE(br.amount, 0)::float8, COALESCE(rm.received_at, br.tx_date), COALESCE(rm.media_path, ''),
		        COALESCE(NULLIF(rm.sender_name, ''), split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '')
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.needs_review = true AND br.ignored = false AND br.is_duplicate = false
		  AND COALESCE(rm.deleted, false) = false
		  AND ($1 = '' OR br.group_jid = $1)
		  AND ($3::timestamptz IS NULL OR COALESCE(rm.received_at, br.tx_date) >= $3)
		  AND ($4::timestamptz IS NULL OR COALESCE(rm.received_at, br.tx_date) < $4))

		UNION ALL

		(SELECT 'message', rm.id, rm.wa_group_jid, '', 0, rm.received_at, COALESCE(rm.media_path, ''),
		        COALESCE(NULLIF(rm.sender_name, ''), split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1))
		FROM raw_messages rm
		WHERE rm.has_media = true AND rm.deleted = false AND rm.parsed = false
		  -- и фото, которое не скачалось (его сообщение сохранено для повтора)
		  AND ((rm.media_path IS NOT NULL AND rm.media_path <> '') OR rm.media_proto IS NOT NULL)
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = rm.id)
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id)
		  AND ($1 = '' OR rm.wa_group_jid = $1)
		  AND ($3::timestamptz IS NULL OR rm.received_at >= $3)
		  AND ($4::timestamptz IS NULL OR rm.received_at < $4))

		ORDER BY 6 DESC
		LIMIT $2
	`, groupJID, limit, from, to)
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
				card_owner    = CASE WHEN $3 = '' THEN card_owner ELSE COALESCE(NULLIF(card_owner, ''), NULLIF(recipient_raw, '')) END,
				recipient_raw = COALESCE(NULLIF($3, ''), recipient_raw),
				-- $4::numeric — иначе тип выводится как integer и копейки обрезаются.
				amount        = CASE WHEN $4::numeric > 0 THEN $4::numeric ELSE amount END,
				tx_date       = COALESCE($5, tx_date),
				-- Имя в правке подтверждает клиента (для будущих правок одной суммой).
				client_confirmed = CASE WHEN $3 = '' THEN client_confirmed ELSE true END,
				-- Снимаем needs_review ТОЛЬКО когда чек полный: есть сумма > 0 И
				-- ИЗВЕСТНЫЙ клиент (имя дали в этой правке ИЛИ клиент уже подтверждён).
				-- Правка одной суммой на чек без клиента НЕ засчитывает его под
				-- владельцем карты (contact_id там — печатный получатель, не клиент).
				needs_review  = NOT ( COALESCE($2, contact_id) IS NOT NULL
				                      AND ($3 <> '' OR client_confirmed)
				                      AND (CASE WHEN $4::numeric > 0 THEN $4::numeric ELSE COALESCE(amount, 0) END) > 0 )
			WHERE id = $1
		`, id, contactID, recipientName, amount, txDate)
		if err == nil && (amount > 0 || txDate != nil) {
			_ = d.SyncWatchWithReceipt(ctx, id)
		}
		if err == nil {
			d.lockCopy(ctx, id)
		}
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
	// Disputed — чтения ИИ разошлись, а спросить было нельзя (см. MarkAIDisputed):
	// фоновый повтор такое не перечитывает, только «пересчитай».
	Disputed bool
}

// MarkAIDisputed — чтения ИИ по сообщению разошлись, а спросить о нём нельзя
// (импорт истории, пересчёт, «молчи»): фоновый повтор не должен перечитывать
// его и спрашивать задним числом — и после перезапуска бота.
func (d *DB) MarkAIDisputed(ctx context.Context, rawID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET ai_disputed = true WHERE id = $1`, rawID)
	return err
}

// RawQuote — чем процитировать сообщение в вопросе бота.
type RawQuote struct {
	WaID       string
	SenderJID  string
	ReceivedAt time.Time
}

// RawQuoteRef — id WhatsApp, отправитель и время сообщения (id в нашей базе).
func (d *DB) RawQuoteRef(ctx context.Context, rawID int) (RawQuote, error) {
	var q RawQuote
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(wa_message_id, ''), COALESCE(sender_jid, ''), received_at FROM raw_messages WHERE id = $1`, rawID).
		Scan(&q.WaID, &q.SenderJID, &q.ReceivedAt)
	return q, err
}

// TxIDsByRawID — живые платежи сообщения (id в нашей базе).
func (d *DB) TxIDsByRawID(ctx context.Context, rawID int) ([]int, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM transactions WHERE raw_message_id = $1 AND ignored = false ORDER BY id`, rawID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SenderJIDByRawID — кто прислал сообщение (id в нашей базе).
func (d *DB) SenderJIDByRawID(ctx context.Context, rawID int) (string, error) {
	var s string
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(sender_jid,'') FROM raw_messages WHERE id = $1`, rawID).Scan(&s)
	return s, err
}

// RawMessageRef — сообщение из нашей базы по id WhatsApp.
type RawMessageRef struct {
	ID         int
	GroupJID   string
	SenderJID  string
	SenderName string
	Body       string
	HasMedia   bool
	Deleted    bool
	HandledAs  string // 'answer' / 'assistant' / пусто
	AnswerKind string // на какой вопрос бота был ответ: receipt / who / dup / collector / cmf
	AnswerTo   string // цель ответа: id сообщения чека, id платежа, id наблюдения
	QuotedID   string // какое сообщение цитировало (свайп)
	ReceivedAt time.Time
	Found      bool
}

// RawMessageByWaID — сообщение по id WhatsApp (для правок «изменить сообщение»).
func (d *DB) RawMessageByWaID(ctx context.Context, waID string) (RawMessageRef, error) {
	var r RawMessageRef
	err := d.pool.QueryRow(ctx, `
		SELECT id, COALESCE(wa_group_jid,''), COALESCE(sender_jid,''), COALESCE(sender_name,''), COALESCE(body,''), has_media, COALESCE(deleted,false),
		       COALESCE(handled_as, ''), COALESCE(answer_kind, ''), COALESCE(answer_target, ''), COALESCE(quoted_wa_id, ''), received_at
		FROM raw_messages WHERE wa_message_id = $1 ORDER BY id DESC LIMIT 1`, waID).
		Scan(&r.ID, &r.GroupJID, &r.SenderJID, &r.SenderName, &r.Body, &r.HasMedia, &r.Deleted, &r.HandledAs,
			&r.AnswerKind, &r.AnswerTo, &r.QuotedID, &r.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Found = true
	return r, nil
}

// EditUndo — что было в сообщении до правки.
type EditUndo struct {
	OldTx         []TxRef         // живые платежи сообщения
	LabeledChecks []string        // id WhatsApp чеков, которым сообщение дало клиента
	LabeledTxs    []int           // текстовые платежи, клиента которых уточнило сообщение
	Corrections   []CorrectionRef // поправки суммы из этого сообщения (уже откачены)
	HandledAs     string          // 'answer' / 'assistant' / пусто
	OldStates     []TxState       // платежи сообщения до правки (что к ним добавили позже)
}

// TxState — что о платеже узнали после записи: уточнённый клиент, кто забрал,
// решение «новый/тот же», поправки суммы.
type TxState struct {
	ID          int
	ContactID   int
	RawName     string
	Amount      float64
	Cash        bool
	Collector   string
	SourceRawID int  // сообщение, уточнившее клиента
	DupDecided  bool // владелец ответил «новый»
}

func (d *DB) txStates(ctx context.Context, rawID int) ([]TxState, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, COALESCE(contact_id, 0), COALESCE(raw_name, ''), amount::float8, is_cash, COALESCE(collector, ''),
		       COALESCE(client_source_raw_id, 0), dup_asked AND NOT dup_pending
		FROM transactions WHERE raw_message_id = $1 AND ignored = false ORDER BY id`, rawID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TxState
	for rows.Next() {
		var t TxState
		if err := rows.Scan(&t.ID, &t.ContactID, &t.RawName, &t.Amount, &t.Cash, &t.Collector, &t.SourceRawID, &t.DupDecided); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CarryOverEditedTx — после правки сообщения его платежи записаны заново;
// переносим на новые то, что узнали о старых: клиента, уточнённого свайпом,
// кто забрал наличку, ответ «новый» на «повтор?», поправки «не X а Y».
func (d *DB) CarryOverEditedTx(ctx context.Context, rawID int, old []TxState) error {
	if len(old) == 0 {
		return nil
	}
	cur, err := d.txStates(ctx, rawID)
	if err != nil {
		return err
	}
	used := map[int]bool{}
	pair := func(n TxState) (TxState, bool) {
		// Сначала тот же клиент и сумма, потом тот же клиент, потом по порядку.
		for _, pass := range []int{0, 1, 2} {
			for i, o := range old {
				if used[i] {
					continue
				}
				switch pass {
				case 0:
					if o.ContactID != n.ContactID && o.RawName != n.RawName || math.Abs(o.Amount-n.Amount) >= 1 {
						continue
					}
				case 1:
					if o.ContactID != n.ContactID && o.RawName != n.RawName {
						continue
					}
				case 2:
					if len(old) != len(cur) {
						continue
					}
				}
				used[i] = true
				return o, true
			}
		}
		return TxState{}, false
	}
	for _, n := range cur {
		o, ok := pair(n)
		if !ok {
			continue
		}
		if o.SourceRawID != 0 && o.ContactID != 0 {
			if _, err := d.pool.Exec(ctx, `UPDATE transactions SET contact_id = $2, raw_name = $3, client_source_raw_id = $4 WHERE id = $1`,
				n.ID, o.ContactID, o.RawName, o.SourceRawID); err != nil {
				return err
			}
			n.ContactID = o.ContactID
		}
		if n.Collector == "" && o.Collector != "" {
			_, _ = d.pool.Exec(ctx, `UPDATE transactions SET collector = $2 WHERE id = $1`, n.ID, o.Collector)
		}
		// Живые поправки суммы этого платежа: та же сумма до поправки — применяем.
		var corrNew float64
		if err := d.pool.QueryRow(ctx, `
			WITH c AS (UPDATE amount_corrections SET target_id = $2 WHERE kind = 'tx' AND target_id = $1 AND reverted = false
			           RETURNING old_amount, new_amount, id)
			SELECT COALESCE((SELECT new_amount::float8 FROM c WHERE abs(old_amount - $3) < 0.01 ORDER BY id DESC LIMIT 1), 0)`,
			o.ID, n.ID, n.Amount).Scan(&corrNew); err == nil && corrNew > 0 {
			_, _ = d.pool.Exec(ctx, `UPDATE transactions SET amount = $2 WHERE id = $1`, n.ID, corrNew)
			n.Amount = corrNew
		}
		if o.DupDecided && n.Cash && n.ContactID == o.ContactID && math.Abs(n.Amount-o.Amount) < 1 {
			_, _ = d.pool.Exec(ctx, `UPDATE transactions SET dup_pending = false, dup_asked = true WHERE id = $1`, n.ID)
		}
	}
	return nil
}

// SetAnswerTarget — сообщение ответило на вопрос бота kind про target.
func (d *DB) SetAnswerTarget(ctx context.Context, rawID int, kind, target string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET answer_kind = $2, answer_target = $3, handled_as = 'answer', parsed = true WHERE id = $1`,
		rawID, kind, target)
	return err
}

// SetQuotedID — какое сообщение цитировал свайп.
func (d *DB) SetQuotedID(ctx context.Context, rawID int, quoted string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET quoted_wa_id = NULLIF($2, '') WHERE id = $1`, rawID, quoted)
	return err
}

// TxIDsBySource — платежи, клиента которых назвало сообщение srcRawID.
func (d *DB) TxIDsBySource(ctx context.Context, srcRawID int) ([]int, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM transactions WHERE client_source_raw_id = $1 AND ignored = false ORDER BY id`, srcRawID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RedecideCashDup — ответ «новый / тот же» поменяли: засчитать или убрать повтор.
func (d *DB) RedecideCashDup(ctx context.Context, txID int, isNew bool) (amount float64, client string, err error) {
	sameAs := 0
	if !isNew {
		sameAs = d.cashSameAs(ctx, txID)
	}
	err = d.pool.QueryRow(ctx, `
		UPDATE transactions SET ignored = NOT $2, dup_pending = false, dup_asked = true, dup_same_as = NULLIF($3, 0) WHERE id = $1
		RETURNING amount::float8, COALESCE(raw_name, '')`, txID, isNew, sameAs).Scan(&amount, &client)
	return
}

// cashSameAs — наличка, повтором которой признают платёж txID (ответ «тот
// же»): та же, из-за которой его придержали и о которой спросили (см.
// cashRepeatPrev). 0 — такой больше нет.
func (d *DB) cashSameAs(ctx context.Context, txID int) int {
	p, err := d.cashProbe(ctx, txID)
	if err != nil {
		return 0
	}
	prev, ok, err := d.cashRepeatPrev(ctx, p)
	if err != nil || !ok {
		return 0
	}
	return prev.ID
}

// restoreSameCashOf — сообщение rawID удалили. Наличку, которую на вопрос
// «новый или тот же?» признали повтором ЕГО налички («тот же»), возвращаем:
// повторять больше нечего — иначе платёж пропал бы совсем (типичный случай:
// наличку переотправили с исправленной опечаткой, ответили «тот же», потом
// удалили сообщение с опечаткой). Возвращённая проверяется на повтор заново,
// как только что записанная: есть другая такая же наличка того же клиента за
// 30 дней — придерживается и бот спросит снова; нет — идёт в сбор.
func (d *DB) restoreSameCashOf(ctx context.Context, rawID int) error {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id FROM transactions r
		JOIN raw_messages rrm ON rrm.id = r.raw_message_id
		WHERE r.ignored = true AND r.is_cash = true AND COALESCE(rrm.deleted, false) = false
		  AND r.dup_same_as IN (SELECT id FROM transactions WHERE raw_message_id = $1)
		ORDER BY r.tx_date, r.id`, rawID)
	if err != nil {
		return err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		p, err := d.cashProbe(ctx, id)
		if err != nil {
			return err
		}
		held := false
		if CashDupCheck() {
			if _, held, err = d.cashRepeatPrev(ctx, p); err != nil {
				return err
			}
		}
		if _, err := d.pool.Exec(ctx, `
			UPDATE transactions SET ignored = false, dup_same_as = NULL, dup_pending = $2,
				dup_asked = false, dup_ask_msg_id = NULL
			WHERE id = $1 AND ignored = true AND dup_same_as IS NOT NULL`, id, held); err != nil {
			return err
		}
	}
	return nil
}

// CountLabeledBy — сколько живых чеков назвало сообщение (до его удаления).
func (d *DB) CountLabeledBy(ctx context.Context, waID string) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.client_source_raw_id
		WHERE rm.wa_message_id = $1 AND br.ignored = false AND br.is_duplicate = false`, waID).Scan(&n)
	return n, err
}

// HasAnyTx — у сообщения есть платежи (в том числе исключённые).
func (d *DB) HasAnyTx(ctx context.Context, rawID int) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transactions WHERE raw_message_id = $1)`, rawID).Scan(&ok)
	return ok, err
}

// PrepareMessageEdit — сообщение изменили в WhatsApp: снимаем его платежи,
// привязки и поправки (как при удалении, но сообщение остаётся), увеличиваем
// счётчик правок (разбор старого текста, ещё идущий в фоне, свой итог не
// запишет) и ставим новый текст на повторный разбор. Ответ на вопрос бота
// на повторный разбор как платёж не ставится.
func (d *DB) PrepareMessageEdit(ctx context.Context, rawID int, body string) (EditUndo, error) {
	var u EditUndo
	var err error
	if err = d.pool.QueryRow(ctx, `SELECT COALESCE(handled_as, '') FROM raw_messages WHERE id = $1`, rawID).Scan(&u.HandledAs); err != nil {
		return u, err
	}
	if u.HandledAs != "" {
		// Ответ на вопрос бота / обращение к ассистенту: своих платежей и
		// привязок не снимаем — новый ответ перезапишет то, что дал старый.
		_, err = d.pool.Exec(ctx, `UPDATE raw_messages SET body = $2, edit_count = edit_count + 1 WHERE id = $1`, rawID, body)
		return u, err
	}
	if u.OldStates, err = d.txStates(ctx, rawID); err != nil {
		return u, err
	}
	if u.OldTx, err = d.TxRefsByRawID(ctx, rawID); err != nil {
		return u, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT rm.wa_message_id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.client_source_raw_id = $1 AND br.ignored = false ORDER BY br.id`, rawID)
	if err != nil {
		return u, err
	}
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			rows.Close()
			return u, err
		}
		u.LabeledChecks = append(u.LabeledChecks, w)
	}
	rows.Close()
	trows, err := d.pool.Query(ctx, `SELECT id FROM transactions WHERE client_source_raw_id = $1 AND ignored = false ORDER BY id`, rawID)
	if err != nil {
		return u, err
	}
	for trows.Next() {
		var id int
		if err := trows.Scan(&id); err != nil {
			trows.Close()
			return u, err
		}
		u.LabeledTxs = append(u.LabeledTxs, id)
	}
	trows.Close()
	if u.Corrections, err = d.RevertCorrections(ctx, rawID); err != nil {
		return u, err
	}
	// Прежние строки сообщения заменит новый разбор: их «тот же» больше не
	// действует (новая строка заново пройдёт проверку на повтор).
	if _, err := d.pool.Exec(ctx, `UPDATE transactions SET ignored = true, dup_same_as = NULL WHERE raw_message_id = $1 AND (ignored = false OR dup_same_as IS NOT NULL)`, rawID); err != nil {
		return u, err
	}
	d.releaseMessageLinks(ctx, rawID)
	_, err = d.pool.Exec(ctx, `
		UPDATE raw_messages SET body = $2, edit_count = edit_count + 1, ai_disputed = false,
			parsed = CASE WHEN handled_as IS NULL THEN false ELSE true END
		WHERE id = $1`, rawID, body)
	return u, err
}

// WhoAsk — вопрос «❓ чей это платёж?» по сообщению.
type WhoAsk struct {
	RawID  int
	Amount float64
	Cash   bool
	TxDate time.Time
}

// SetWhoAsk — запомнить вопрос «❓ чей платёж?» (переживает рестарт бота).
func (d *DB) SetWhoAsk(ctx context.Context, rawID int, askMsgID string, amount float64, cash bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET who_ask_msg_id = $2, who_ask_amount = $3, who_ask_cash = $4 WHERE id = $1`,
		rawID, askMsgID, amount, cash)
	return err
}

// WhoAskByMsg — вопрос по id сообщения бота (ответ свайпом после рестарта).
func (d *DB) WhoAskByMsg(ctx context.Context, askMsgID string) (WhoAsk, bool, error) {
	var w WhoAsk
	err := d.pool.QueryRow(ctx, `
		SELECT id, COALESCE(who_ask_amount, 0)::float8, COALESCE(who_ask_cash, false), received_at
		FROM raw_messages WHERE who_ask_msg_id = $1 AND NOT COALESCE(deleted, false) LIMIT 1`, askMsgID).
		Scan(&w.RawID, &w.Amount, &w.Cash, &w.TxDate)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, false, nil
	}
	return w, err == nil, err
}

// ClearWhoAsk — на вопрос ответили.
func (d *DB) ClearWhoAsk(ctx context.Context, rawID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET who_ask_msg_id = NULL WHERE id = $1`, rawID)
	return err
}

// RawBodyByID — текст и отправитель сообщения.
func (d *DB) RawBodyByID(ctx context.Context, rawID int) (body, senderName string, err error) {
	err = d.pool.QueryRow(ctx, `SELECT COALESCE(body, ''), COALESCE(sender_name, '') FROM raw_messages WHERE id = $1`, rawID).Scan(&body, &senderName)
	return
}

// MessageEditCount — сколько раз сообщение правили (для разбора в фоне).
func (d *DB) MessageEditCount(ctx context.Context, rawID int) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT edit_count FROM raw_messages WHERE id = $1`, rawID).Scan(&n)
	return n, err
}

// SetHandledAs — сообщение оказалось ответом на вопрос бота ('answer') или
// обращением к ассистенту ('assistant'): платежом его не считаем.
func (d *DB) SetHandledAs(ctx context.Context, rawID int, kind string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET handled_as = $2, parsed = true WHERE id = $1`, rawID, kind)
	return err
}

// SetMessageBody — текст сообщения для учёта (без обращения к боту).
func (d *DB) SetMessageBody(ctx context.Context, rawID int, body string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET body = $2 WHERE id = $1`, rawID, body)
	return err
}

// SetReceiptSourceByMessage — клиента чека назвало сообщение srcRawID (ответ на
// «чей чек?»): удалят или изменят ответ — чек узнает.
func (d *DB) SetReceiptSourceByMessage(ctx context.Context, receiptWaID string, srcRawID int) error {
	if srcRawID <= 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET client_source_raw_id = $2
		WHERE id = (SELECT br.id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		            WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1)`, receiptWaID, srcRawID)
	return err
}

// SetTxClientFrom — клиент текстового платежа уточнён сообщением srcRawID.
func (d *DB) SetTxClientFrom(ctx context.Context, txID, contactID int, name string, srcRawID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE transactions SET contact_id = $2, raw_name = $3, client_source_raw_id = NULLIF($4, 0) WHERE id = $1`,
		txID, contactID, name, srcRawID)
	return err
}

// CorrectionRef — поправка суммы из сообщения.
type CorrectionRef struct {
	Kind     string // "tx" | "receipt"
	TargetID int
	Old, New float64
}

// RecordCorrection — запомнить поправку «не old а new» сообщения rawID.
func (d *DB) RecordCorrection(ctx context.Context, rawID int, kind string, targetID int, old, amount float64) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO amount_corrections (raw_message_id, kind, target_id, old_amount, new_amount) VALUES ($1, $2, $3, $4, $5)`,
		rawID, kind, targetID, old, amount)
	return err
}

// RevertCorrections — поправку удалили/изменили: суммы, которые она поменяла,
// возвращаются (если с тех пор их не правили иначе).
func (d *DB) RevertCorrections(ctx context.Context, rawID int) ([]CorrectionRef, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE amount_corrections SET reverted = true WHERE raw_message_id = $1 AND reverted = false
		RETURNING kind, target_id, old_amount::float8, new_amount::float8`, rawID)
	if err != nil {
		return nil, err
	}
	var out []CorrectionRef
	for rows.Next() {
		var c CorrectionRef
		if err := rows.Scan(&c.Kind, &c.TargetID, &c.Old, &c.New); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	for _, c := range out {
		switch c.Kind {
		case "tx":
			if _, err := d.pool.Exec(ctx, `UPDATE transactions SET amount = $2 WHERE id = $1 AND abs(amount - $3) < 0.01`, c.TargetID, c.Old, c.New); err != nil {
				return out, err
			}
			_ = d.RecheckCashRepeat(ctx, c.TargetID)
		case "receipt":
			if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET amount = $2 WHERE id = $1 AND abs(amount - $3) < 0.01`, c.TargetID, c.Old, c.New); err != nil {
				return out, err
			}
			_ = d.SyncWatchWithReceipt(ctx, c.TargetID)
		}
	}
	return out, rows.Err()
}

// CorrectionsByMessage — сколько живых поправок суммы в сообщении (по id WhatsApp).
func (d *DB) CorrectionsByMessage(ctx context.Context, waID string) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM amount_corrections ac JOIN raw_messages rm ON rm.id = ac.raw_message_id
		WHERE rm.wa_message_id = $1 AND ac.reverted = false`, waID).Scan(&n)
	return n, err
}

// RecheckCashRepeat — сумму налички поправили: придержать её как повтор, если
// теперь она совпала с прежней наличкой клиента за 30 дней, или отпустить, если
// больше не совпадает. Подтверждённую владельцем («новый») не трогаем.
func (d *DB) RecheckCashRepeat(ctx context.Context, txID int) error {
	// Повтор — та же наличка того же клиента (или того же имени, записанного
	// иначе: опечатка, порядок слов) за 30 дней в той же группе, см. cashRepeatPrev.
	p := cashRepeatProbe{TxID: txID}
	var hasContact bool
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(t.contact_id, 0), t.contact_id IS NOT NULL, COALESCE(c.canonical_name, ''),
		       t.amount::float8, t.tx_date, t.raw_message_id
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.id = $1 AND t.is_cash = true AND t.ignored = false
		  AND NOT (t.dup_asked AND NOT t.dup_pending)`, txID).Scan(&p.ContactID, &hasContact, &p.Name, &p.Amount, &p.TxDate, &p.RawID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // не наличка, исключена или владелец уже подтвердил «новый»
	}
	if err != nil {
		return err
	}
	held := false
	if hasContact {
		if _, held, err = d.cashRepeatPrev(ctx, p); err != nil {
			return err
		}
	}
	_, err = d.pool.Exec(ctx, `
		UPDATE transactions SET dup_pending = $2
		WHERE id = $1 AND is_cash = true AND ignored = false AND NOT (dup_asked AND NOT dup_pending)`, txID, held)
	return err
}

// TxRefsByRawID — живые платежи сообщения: клиент и сумма.
func (d *DB) TxRefsByRawID(ctx context.Context, rawID int) ([]TxRef, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(c.canonical_name, t.raw_name, ''), t.amount::float8, t.is_cash
		FROM transactions t LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.raw_message_id = $1 AND t.ignored = false ORDER BY t.id`, rawID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TxRef
	for rows.Next() {
		var r TxRef
		if err := rows.Scan(&r.ID, &r.Name, &r.Amount, &r.Cash); err != nil {
			return nil, err
		}
		r.Found = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// TxRef — текстовый платёж (для поправки суммы).
type TxRef struct {
	ID     int
	Name   string
	Amount float64
	Cash   bool
	Found  bool
}

// TextTxOfMessage — единственный живой платёж сообщения (по id WhatsApp).
func (d *DB) TextTxOfMessage(ctx context.Context, waID string) (TxRef, error) {
	var r TxRef
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(t.raw_name,''), t.amount::float8 FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.wa_message_id = $1 AND t.ignored = false AND COALESCE(rm.deleted, false) = false ORDER BY t.id`, waID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if err := rows.Scan(&r.ID, &r.Name, &r.Amount); err != nil {
			return r, err
		}
	}
	r.Found = n == 1
	return r, rows.Err()
}

// LatestTxFromSenderWithAmount — свежий платёж отправителя на эту сумму (для
// «не 5000 а 6000» следом без свайпа).
func (d *DB) LatestTxFromSenderWithAmount(ctx context.Context, groupJID, senderJID string, amount float64, since time.Time) (TxRef, error) {
	var r TxRef
	err := d.pool.QueryRow(ctx, `
		SELECT t.id, COALESCE(t.raw_name,''), t.amount::float8 FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.wa_group_jid = $1 AND rm.sender_jid = $2 AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false AND abs(t.amount - $3) < 1 AND t.created_at >= $4
		ORDER BY t.created_at DESC LIMIT 1`, groupJID, senderJID, amount, since).Scan(&r.ID, &r.Name, &r.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Found = true
	return r, nil
}

// SetTxAmount — поправить сумму платежа.
func (d *DB) SetTxAmount(ctx context.Context, txID int, amount float64) error {
	_, err := d.pool.Exec(ctx, `UPDATE transactions SET amount = $2 WHERE id = $1`, txID, amount)
	return err
}

// UnparsedTextMessagesSince — неразобранные текстовые сообщения групп с момента
// since (для повторного разбора, когда ИИ снова доступен).
func (d *DB) UnparsedTextMessagesSince(ctx context.Context, since time.Time, limit int) ([]UnparsedMessage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, rm.wa_group_jid, COALESCE(rm.sender_name, ''), COALESCE(rm.body, ''), rm.received_at, rm.ai_disputed
		FROM raw_messages rm
		WHERE rm.parsed = false AND rm.has_media = false AND rm.deleted = false AND rm.handled_as IS NULL
		  AND COALESCE(rm.body, '') <> '' AND rm.received_at >= $1
		  AND COALESCE(rm.wa_group_jid, '') LIKE '%@g.us'
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id AND t.ignored = false)
		ORDER BY rm.received_at
		LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnparsedMessage
	for rows.Next() {
		var m UnparsedMessage
		if err := rows.Scan(&m.ID, &m.GroupJID, &m.SenderName, &m.Body, &m.ReceivedAt, &m.Disputed); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) UnparsedTextMessages(ctx context.Context, limit int) ([]UnparsedMessage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, rm.wa_group_jid, COALESCE(rm.sender_name, ''), COALESCE(rm.body, ''), rm.received_at
		FROM raw_messages rm
		WHERE rm.parsed = false AND rm.has_media = false AND rm.deleted = false
		  AND COALESCE(rm.body, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.raw_message_id = rm.id AND t.ignored = false)
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

// ReportExclusions — суммы, которые НЕ вошли в сбор за период (и почему), чтобы
// отчёт был сверяем до рубля. Именно эти «тихие» исключения дают расхождение с
// ручным подсчётом: бот не считает то, в чём не уверен, пока владелец не
// разберётся. Считаем по дате операции (tx_date) — как основной сбор.
type ReportExclusions struct {
	NeedsReviewCount int     // чеки, где бот не распознал клиента/сумму (НЕ в сумме)
	NeedsReviewSum   float64 // их сумма — станет деньгами после разбора
	HeldCashCount    int     // придержанная наличка-повтор (ждём «новый/тот же?»)
	HeldCashSum      float64
	TextDupCount     int     // текстовые платежи, зачтённые как ДУБЛЬ чека (уже в сумме — как чек)
	TextDupSum       float64 // информационно: НЕ прибавлять к обороту
}

// ReportExclusions считает исключённые из сбора суммы за период [from, to) по
// дате операции (операционный режим). Три корзины, чтобы сбор был сверяем:
//   - NeedsReview — деньги ВНЕ суммы (после разбора добавятся к обороту);
//   - HeldCash    — придержанные повторы налички (тоже вне суммы);
//   - TextDup     — тексты, зачтённые как дубль чека (уже в сумме — информационно,
//     чтобы владелец мог заметить, если какой-то из них ОТДЕЛЬНЫЙ платёж).
func (d *DB) ReportExclusions(ctx context.Context, from, to time.Time, groupJIDs []string) (ReportExclusions, error) {
	return d.ReportExclusionsBasis(ctx, from, to, groupJIDs, false)
}

// ReportExclusionsBasis — то же, но byChat=true считает период по дате, когда
// сообщение прислали в чат (для отчёта «по дате в чате»).
func (d *DB) legacyReportExclusionsBasis(ctx context.Context, from, to time.Time, groupJIDs []string, byChat bool) (ReportExclusions, error) {
	var ex ReportExclusions
	brDate, oDate, tDate := "br.tx_date", "o.tx_date", "t.tx_date::timestamptz"
	if byChat {
		brDate, oDate, tDate = "COALESCE(rm.received_at, br.tx_date)", "COALESCE(orm.received_at, o.tx_date)", "COALESCE(rm.received_at, t.tx_date::timestamptz)"
	}
	// 1) Непроверенные чеки: needs_review ИЛИ без contact_id (иначе выпал бы из
	//    суммы молча). Дедуп кросс-групповых копий (как в SummaryForPeriod).
	//    Клиент непроверенного чека НЕИЗВЕСТЕН (contact_id там, если есть, — это
	//    владелец карты или догадка), поэтому «уже посчитан текстом» до проверки
	//    определить нельзя — раньше такой фильтр молча ПРЯТАЛ чеки из списка.
	//    Дедуп с текстом происходит после проверки (countableTextCondition), а
	//    «Максимум» — честная верхняя оценка: каждый непроверенный чек в списке.
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(amount), 0)::float8 FROM (
			SELECT DISTINCT ON (COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
				br.amount
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE `+brDate+` >= $1::timestamptz AND `+brDate+` < $2::timestamptz
			  AND (br.needs_review = true OR br.contact_id IS NULL)
			  AND br.is_duplicate = false AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
			  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
			  -- Не добавляем, если тот же платёж уже посчитан РАСПОЗНАННОЙ копией
			  -- в другой группе — иначе «Максимум» задвоил бы его. Сопоставляем по
			  -- номеру документа, а если его нет — по синтетическому id пересылки
			  -- (копия чека получает wa_message_id вида «<оригинал>-fwd-<группа>»).
			  AND NOT EXISTS (
				SELECT 1 FROM bank_receipts o
				LEFT JOIN raw_messages orm ON orm.id = o.raw_message_id
				WHERE o.id <> br.id
				  AND o.needs_review = false AND o.is_duplicate = false AND o.ignored = false
				  AND COALESCE(orm.deleted, false) = false
				  AND `+oDate+` >= $1 AND `+oDate+` < $2
				  AND ($3::text[] IS NULL OR o.group_jid = ANY($3))
				  AND (
					(NULLIF(br.doc_number, '') IS NOT NULL AND o.doc_number = br.doc_number)
					OR (COALESCE(rm.wa_message_id, '') <> '' AND COALESCE(orm.wa_message_id, '') <> ''
						AND starts_with(rm.wa_message_id, orm.wa_message_id || '-fwd-'))
				  )
			  )
			ORDER BY COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text)
		) dedup
	`, from, to, groupSliceArg(groupJIDs)).Scan(&ex.NeedsReviewCount, &ex.NeedsReviewSum)
	if err != nil {
		return ex, err
	}
	// 2) Придержанная наличка-повтор: ждёт ответа «новый или тот же?».
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(t.amount), 0)::float8
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE `+tDate+` >= $1::timestamptz AND `+tDate+` < $2::timestamptz
		  AND t.is_cash = true AND t.dup_pending = true AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false AND t.amount > 0
		  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3))
	`, from, to, groupSliceArg(groupJIDs)).Scan(&ex.HeldCashCount, &ex.HeldCashSum)
	if err != nil {
		return ex, err
	}
	// 3) Текстовые платежи, отброшенные как ДУБЛЬ чека (тот же контакт+сумма+
	//    группа+период, чек распознан и посчитан). Уже учтены как чек — показываем
	//    информационно, чтобы владелец заметил, если это был ОТДЕЛЬНЫЙ платёж.
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(t.amount), 0)::float8
		FROM transactions t
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE `+tDate+` >= $1::timestamptz AND `+tDate+` < $2::timestamptz
		  AND t.is_cash = false AND t.dup_pending = false AND t.ignored = false
		  AND COALESCE(rm.deleted, false) = false AND t.amount > 0
		  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3))
		  -- Зеркало countableTextCondition: чек-близнец ищем в ±2 дня от текста,
		  -- а не в границах периода — иначе цифры «дубль» и «посчитано» расходились.
		  AND EXISTS (
			SELECT 1 FROM bank_receipts brd
			LEFT JOIN raw_messages brm ON brm.id = brd.raw_message_id
			WHERE brd.contact_id = t.contact_id AND brd.amount = t.amount
			  AND brd.needs_review = false AND brd.is_duplicate = false AND brd.ignored = false
			  AND COALESCE(brm.deleted, false) = false
			  -- Зеркало countableTextCondition: пара по времени ПРИСЫЛКИ (±2 дня).
			  AND COALESCE(brm.received_at, brd.tx_date) >= COALESCE(rm.received_at, t.tx_date::timestamptz) - interval '2 days'
			  AND COALESCE(brm.received_at, brd.tx_date) <  COALESCE(rm.received_at, t.tx_date::timestamptz) + interval '2 days'
			  `+groupPairClause("$3")+`
		  )
	`, from, to, groupSliceArg(groupJIDs)).Scan(&ex.TextDupCount, &ex.TextDupSum)
	return ex, err
}

// SimilarRef — уже записанный похожий платёж.
type SimilarRef struct {
	Name   string
	Amount float64
	Kind   string // «чек» / «наличка» / «перевод»
	When   time.Time
}

// SimilarPayment — есть ли в группе такой же платёж того же клиента: наличка —
// в пределах 30 дней (правило повтора налички), перевод/чек — ±2 дня.
func (d *DB) SimilarPayment(ctx context.Context, groupJID string, contactID int, amount float64, when time.Time, cash bool) (SimilarRef, bool, error) {
	var r SimilarRef
	days := 2
	if cash {
		days = 30
	}
	err := d.pool.QueryRow(ctx, `
		SELECT name, amount, kind, at FROM (
			SELECT COALESCE(c.canonical_name, t.raw_name, '') AS name, t.amount::float8 AS amount,
			       CASE WHEN t.is_cash THEN 'наличка' ELSE 'перевод' END AS kind, t.tx_date::timestamptz AS at
			FROM transactions t
			JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN contacts c ON c.id = t.contact_id
			WHERE t.contact_id = $2 AND abs(t.amount - $3) < 1 AND t.ignored = false
			  AND COALESCE(rm.deleted, false) = false AND rm.wa_group_jid = $1
			  AND t.is_cash = $5
			  AND t.tx_date::timestamptz BETWEEN $4::timestamptz - make_interval(days => $6) AND $4::timestamptz + make_interval(days => $6)
			UNION ALL
			SELECT COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, 'чек', br.tx_date
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN contacts c ON c.id = br.contact_id
			WHERE NOT $5 AND br.contact_id = $2 AND abs(br.amount - $3) < 1
			  AND br.ignored = false AND br.is_duplicate = false AND COALESCE(rm.deleted, false) = false
			  AND br.group_jid = $1
			  AND br.tx_date BETWEEN $4::timestamptz - interval '2 days' AND $4::timestamptz + interval '2 days'
		) x ORDER BY at DESC LIMIT 1`, groupJID, contactID, amount, when, cash, days).Scan(&r.Name, &r.Amount, &r.Kind, &r.When)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, nil
}

// PersonPending — что по человеку НЕ вошло в сбор: придержанная наличка-повтор
// и чеки на проверке, подписанные его именем.
func (d *DB) PersonPending(ctx context.Context, person string, from, to *time.Time, groupJID string) (heldN int, heldSum float64, reviewN int, reviewSum float64, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(t.amount), 0)::float8
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		WHERE t.dup_pending = true AND t.ignored = false AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(c.canonical_name, t.raw_name, '') ILIKE '%' || $1 || '%'
		  AND ($2::timestamptz IS NULL OR t.tx_date >= $2) AND ($3::timestamptz IS NULL OR t.tx_date < $3)
		  AND ($4 = '' OR rm.wa_group_jid = $4)`, person, from, to, groupJID).Scan(&heldN, &heldSum)
	if err != nil {
		return
	}
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(br.amount), 0)::float8
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.needs_review = true AND br.ignored = false AND br.is_duplicate = false
		  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
		  AND COALESCE(br.recipient_raw, '') ILIKE '%' || $1 || '%'
		  AND ($2::timestamptz IS NULL OR br.tx_date >= $2) AND ($3::timestamptz IS NULL OR br.tx_date < $3)
		  AND ($4 = '' OR br.group_jid = $4)`, person, from, to, groupJID).Scan(&reviewN, &reviewSum)
	return
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

// ReconReceipt — чек для сверки с программой, со служебными полями: чтобы склеить
// копии ОДНОГО чека из разных групп (пересылка) и отсеять неподтверждённые (у них
// «имя» — это владелец карты, а не клиент).
type ReconReceipt struct {
	ID          int
	Name        string
	Amount      float64
	TxDate      time.Time
	GroupJID    string
	DocNumber   string
	WaMessageID string
	NeedsReview bool
}

// ReceiptsForReconcile — все действующие чеки за [from, to) по ВСЕМ группам (для
// полной картины клиента при сверке; фильтр по группе применяет вызывающий).
func (d *DB) ReceiptsForReconcile(ctx context.Context, from, to time.Time) ([]ReconReceipt, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT br.id, COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date,
		       COALESCE(br.group_jid,''), COALESCE(br.doc_number,''), COALESCE(rm.wa_message_id,''),
		       br.needs_review
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.amount > 0
		ORDER BY br.tx_date, br.id
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconReceipt
	for rows.Next() {
		var r ReconReceipt
		if err := rows.Scan(&r.ID, &r.Name, &r.Amount, &r.TxDate, &r.GroupJID, &r.DocNumber, &r.WaMessageID, &r.NeedsReview); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReconCash — наличка/текстовый платёж клиента для сверки (только те, что идут в
// сбор: не дубль чека, не придержанный повтор).
type ReconCash struct {
	ID       int
	Name     string
	Amount   float64
	TxDate   time.Time
	GroupJID string
}

// CashForReconcile — засчитываемые текстовые платежи/наличка за [from, to).
func (d *DB) CashForReconcile(ctx context.Context, from, to time.Time) ([]ReconCash, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, c.canonical_name, t.amount::float8, t.tx_date::timestamptz, COALESCE(rm.wa_group_jid,'')
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE t.tx_date >= $1::timestamptz AND t.tx_date < $2::timestamptz
		  AND t.ignored = false
		  AND t.amount > 0
		  AND COALESCE(rm.deleted, false) = false
		  AND `+textCountable("NULL")+`
		ORDER BY t.tx_date
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconCash
	for rows.Next() {
		var r ReconCash
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
		  AND br.created_at >= $3::timestamptz
		  AND br.client_confirmed = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		LIMIT 1
	`, groupJID, senderJID, since).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReattributeOldestUnconfirmedReceipt привязывает клиента к САМОМУ СТАРОМУ
// неподтверждённому чеку от отправителя (FIFO). Порядок — по времени сообщения
// WhatsApp (rm.received_at), а не по времени вставки строки: так привязка не
// зависит от того, какая горутина/OCR закончилась первой, и переживает рестарт.
func (d *DB) ReattributeOldestUnconfirmedReceipt(ctx context.Context, groupJID, senderJID string, since time.Time, name string, contactID *int) (found bool, amount float64, err error) {
	found, amount, _, err = d.ReattributeOldestUnconfirmedReceiptFrom(ctx, groupJID, senderJID, since, name, contactID, 0, 0)
	return found, amount, err
}

// OldestUnconfirmedReceiptAmount — сумма самого старого неподтверждённого чека
// отправителя (тот, к которому привяжется имя), ok=false — такого нет.
func (d *DB) OldestUnconfirmedReceiptAmount(ctx context.Context, groupJID, senderJID string, since time.Time) (float64, bool, error) {
	amount, _, ok, err := d.OldestUnconfirmedReceiptCard(ctx, groupJID, senderJID, since)
	return amount, ok, err
}

// OldestUnconfirmedReceiptCard — то же и владелец карты (получатель,
// напечатанный на чеке): его ФИО — не клиент этого чека.
func (d *DB) OldestUnconfirmedReceiptCard(ctx context.Context, groupJID, senderJID string, since time.Time) (float64, string, bool, error) {
	var amount float64
	var owner string
	err := d.pool.QueryRow(ctx, `
		SELECT br.amount::float8, COALESCE(NULLIF(br.card_owner, ''), NULLIF(br.recipient_raw, ''), '') FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND rm.sender_jid = $2
		  AND br.created_at >= $3::timestamptz
		  AND br.client_confirmed = false
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		ORDER BY rm.received_at ASC, br.id ASC
		LIMIT 1
	`, groupJID, senderJID, since).Scan(&amount, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return amount, owner, true, nil
}

// ReceiptCardOwnerByMessage — владелец карты (получатель, напечатанный на
// чеке) чека по id его сообщения WhatsApp; "" — чека нет.
func (d *DB) ReceiptCardOwnerByMessage(ctx context.Context, waMessageID string) (string, error) {
	var owner string
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(br.card_owner, ''), NULLIF(br.recipient_raw, ''), '')
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1
		ORDER BY br.id DESC
		LIMIT 1
	`, waMessageID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

// ReattributeOldestUnconfirmedReceiptFrom — то же, с запоминанием, КАКОЕ
// сообщение назвало клиента (sourceRawID: удалят его — привязка откатится), и
// (amount > 0) только к чеку на эту сумму.
func (d *DB) ReattributeOldestUnconfirmedReceiptFrom(ctx context.Context, groupJID, senderJID string, since time.Time, name string, contactID *int, sourceRawID int, amount0 float64) (found bool, amount float64, checkRawID int, err error) {
	var src any
	if sourceRawID > 0 {
		src = sourceRawID
	}
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET
			card_owner = COALESCE(NULLIF(card_owner, ''), NULLIF(recipient_raw, '')),
			recipient_raw = $4, contact_id = $5, needs_review = false, client_confirmed = true,
			client_source_raw_id = $6
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
			  AND rm.sender_jid = $2
			  -- Окно (право на спаривание) — по времени ПОЯВЛЕНИЯ строки (created_at):
			  -- при офлайн-догрузке пачка чеков с ОЧЕНЬ старым временем сообщения всё
			  -- равно только что вставлена и должна париться. Порядок FIFO — по
			  -- времени сообщения WhatsApp (received_at), чтобы не зависеть от OCR.
			  AND br.created_at >= $3::timestamptz
			  AND br.client_confirmed = false
			  AND br.is_duplicate = false AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND ($7::float8 <= 0 OR abs(br.amount - $7::float8) < 1)
			ORDER BY rm.received_at ASC, br.id ASC
			LIMIT 1
		)
		RETURNING amount::float8, COALESCE(raw_message_id, 0)
	`, groupJID, senderJID, since, name, contactID, src, amount0).Scan(&amount, &checkRawID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, 0, nil
	}
	if err != nil {
		return false, 0, 0, err
	}
	return true, amount, checkRawID, nil
}

// CmfWatchByRawMessage — наблюдение сверки по сообщению с чеком (id в нашей базе).
func (d *DB) CmfWatchByRawMessage(ctx context.Context, rawID int) (int, bool, error) {
	var id int
	err := d.pool.QueryRow(ctx, `SELECT id FROM cmf_watch WHERE raw_message_id = $1 ORDER BY id DESC LIMIT 1`, rawID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// TextTxByMessage — текстовые платежи сообщения (для уточнения клиента свайпом).
func (d *DB) TextTxByMessage(ctx context.Context, waMessageID string) ([]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id FROM transactions t JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.wa_message_id = $1 AND t.ignored = false ORDER BY t.id`, waMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetTxClient — поменять клиента текстового платежа (уточнили ФИО свайпом).
func (d *DB) SetTxClient(ctx context.Context, txID, contactID int, rawName string) error {
	_, err := d.pool.Exec(ctx, `UPDATE transactions SET contact_id = $2, raw_name = $3 WHERE id = $1`, txID, contactID, rawName)
	return err
}

// EnqueuePendingName сохраняет ФИО клиента, присланное ПЕРЕД чеком, в durable
// очередь (переживает рестарт). Порядок выдачи — по времени сообщения WhatsApp
// (received_at), а не по времени вставки. Держим не больше 20 несъеденных имён
// на отправителя (старые гасим), чтобы «висяки» не копились.
func (d *DB) EnqueuePendingName(ctx context.Context, groupJID, senderJID, name string, amount float64, rawMessageID int, receivedAt time.Time) error {
	var rid any
	if rawMessageID > 0 {
		rid = rawMessageID
	}
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO pending_client_names (group_jid, sender_jid, name, amount, raw_message_id, received_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, groupJID, senderJID, name, amount, rid, receivedAt); err != nil {
		return err
	}
	_, _ = d.pool.Exec(ctx, `
		UPDATE pending_client_names SET consumed = true, consumed_at = now()
		WHERE id IN (
			SELECT id FROM pending_client_names
			WHERE group_jid = $1 AND sender_jid = $2 AND consumed = false
			ORDER BY received_at DESC, id DESC OFFSET 20
		)
	`, groupJID, senderJID)
	return nil
}

// TakePendingName атомарно забирает САМОЕ РАННЕЕ (по времени сообщения) ждущее
// имя от group+sender, не старше окна (received_at >= since), и помечает его
// съеденным в той же операции. FOR UPDATE SKIP LOCKED: два чека, пришедшие
// одновременно от одного отправителя, не заберут одно и то же имя. ok=false,
// если очередь пуста или всё протухло.
func (d *DB) TakePendingName(ctx context.Context, groupJID, senderJID string, since time.Time) (name string, amount float64, ok bool, err error) {
	return d.takePendingName(ctx, groupJID, senderJID, since, false)
}

// TakePendingNameWithAmount — как TakePendingName, но берёт только имя С СУММОЙ
// (>0): для случая «фото наличных пришло ПОСЛЕ строки ФИО+сумма».
func (d *DB) TakePendingNameWithAmount(ctx context.Context, groupJID, senderJID string, since time.Time) (name string, amount float64, ok bool, err error) {
	return d.takePendingName(ctx, groupJID, senderJID, since, true)
}

// PendingName — имя из очереди «имя перед чеком».
type PendingName struct {
	ID     int
	Name   string
	Amount float64
	RawID  int
}

// TakePendingNameFor — самое раннее ждущее имя отправителя, подходящее чеку на
// сумму checkAmount: без суммы или с той же суммой (имя «Ахмед 15000» не
// достанется чеку на 20000). Имена из удалённых сообщений не берём.
func (d *DB) TakePendingNameFor(ctx context.Context, groupJID, senderJID string, since time.Time, checkAmount float64) (PendingName, bool, error) {
	var p PendingName
	var rid *int
	err := d.pool.QueryRow(ctx, `
		UPDATE pending_client_names SET consumed = true, consumed_at = now()
		WHERE id = (
			SELECT pn.id FROM pending_client_names pn
			WHERE pn.group_jid = $1 AND pn.sender_jid = $2 AND pn.consumed = false
			  AND pn.created_at >= $3::timestamptz
			  AND (pn.amount = 0 OR ($4::float8 > 0 AND abs(pn.amount - $4::float8) < 1))
			  AND NOT EXISTS (SELECT 1 FROM raw_messages rm WHERE rm.id = pn.raw_message_id AND rm.deleted)
			ORDER BY pn.received_at ASC, pn.id ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, name, amount, raw_message_id
	`, groupJID, senderJID, since, checkAmount).Scan(&p.ID, &p.Name, &p.Amount, &rid)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if rid != nil {
		p.RawID = *rid
	}
	return p, true, nil
}

// PendingNamesOf — несъеденные имена отправителя (для сверки с подписью к чеку).
func (d *DB) PendingNamesOf(ctx context.Context, groupJID, senderJID string, since time.Time) ([]PendingName, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, name, amount, COALESCE(raw_message_id, 0) FROM pending_client_names
		WHERE group_jid = $1 AND sender_jid = $2 AND consumed = false AND created_at >= $3::timestamptz
		ORDER BY received_at ASC, id ASC`, groupJID, senderJID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingName
	for rows.Next() {
		var p PendingName
		if err := rows.Scan(&p.ID, &p.Name, &p.Amount, &p.RawID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ConsumePendingNameByID — пометить имя из очереди использованным.
func (d *DB) ConsumePendingNameByID(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `UPDATE pending_client_names SET consumed = true, consumed_at = now() WHERE id = $1`, id)
	return err
}

func (d *DB) takePendingName(ctx context.Context, groupJID, senderJID string, since time.Time, requireAmount bool) (string, float64, bool, error) {
	amountCond := "AND NOT EXISTS (SELECT 1 FROM raw_messages rm WHERE rm.id = pending_client_names.raw_message_id AND rm.deleted)"
	if requireAmount {
		amountCond += " AND amount > 0"
	}
	var name string
	var amount float64
	// Окно — по времени ПОЯВЛЕНИЯ записи (created_at, = времени обработки), а не по
	// времени сообщения: пачка имён, догруженная после простоя, вставлена только
	// что и должна париться. Порядок FIFO — по времени сообщения WhatsApp (received_at).
	err := d.pool.QueryRow(ctx, `
		UPDATE pending_client_names SET consumed = true, consumed_at = now()
		WHERE id = (
			SELECT id FROM pending_client_names
			WHERE group_jid = $1 AND sender_jid = $2 AND consumed = false
			  AND created_at >= $3::timestamptz `+amountCond+`
			ORDER BY received_at ASC, id ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING name, amount
	`, groupJID, senderJID, since).Scan(&name, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return name, amount, true, nil
}

// PurgePendingNames чистит очередь: удаляет съеденные и протухшие записи.
func (d *DB) PurgePendingNames(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM pending_client_names
		WHERE consumed = true OR received_at < $1::timestamptz
	`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReceiptIdentity — «отпечаток» чека для поиска того же чека в других группах.
type ReceiptIdentity struct {
	Found        bool
	ID           int
	DocNumber    string
	AuthCode     string
	Bank         string
	RecipientRaw string
	Amount       float64
	TxDate       time.Time
	Client       string // ФИО клиента (по attribution), для показа
}

// ReceiptByWaMessageID находит чек по id ЕГО сообщения WhatsApp (когда владелец
// свайпнул на сам чек и спросил «в какую ещё группу его отправляли»).
func (d *DB) ReceiptByWaMessageID(ctx context.Context, waMessageID string) (ReceiptIdentity, error) {
	var r ReceiptIdentity
	err := d.pool.QueryRow(ctx, `
		SELECT br.id, COALESCE(br.doc_number,''), COALESCE(br.auth_code,''), COALESCE(br.bank,''),
		       COALESCE(br.recipient_raw,''), br.amount::float8, br.tx_date,
		       COALESCE(c.canonical_name, br.recipient_raw, '')
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE rm.wa_message_id = $1
		ORDER BY br.id DESC
		LIMIT 1
	`, waMessageID).Scan(&r.ID, &r.DocNumber, &r.AuthCode, &r.Bank, &r.RecipientRaw, &r.Amount, &r.TxDate, &r.Client)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Found = true
	return r, nil
}

// ReceiptSender — кто и с какого РЕАЛЬНОГО номера прислал чек (для ответа на
// «с какого номера этот чек / чей это чек»). SenderPhone — цифры настоящего
// номера ("" если не пойман: старая строка, LID без SenderAlt, пересланная
// ботом копия). SenderName — имя из памяти номеров или подпись в WhatsApp.
type ReceiptSender struct {
	Found       bool
	Amount      float64
	Recipient   string // получатель, напечатанный на чеке (часто владелец карты)
	Client      string // привязанный клиент (ФИО), если известен
	TxDate      time.Time
	ReceivedAt  time.Time
	SenderPhone string
	SenderName  string
	GroupJID    string
}

const receiptSenderSelect = `
	SELECT COALESCE(br.amount,0)::float8,
	       COALESCE(br.recipient_raw,''),
	       COALESCE(c.canonical_name,''),
	       br.tx_date,
	       COALESCE(rm.received_at, br.tx_date),
	       COALESCE(split_part(COALESCE(NULLIF(rm.sender_phone,''),
	           CASE WHEN rm.sender_jid LIKE '%@s.whatsapp.net' THEN rm.sender_jid END), '@', 1), ''),
	       COALESCE(NULLIF(po.name,''), NULLIF(rm.sender_name,''), ''),
	       COALESCE(br.group_jid,'')
	FROM bank_receipts br
	LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
	LEFT JOIN contacts c ON c.id = br.contact_id
	LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone,''), rm.sender_jid, ''), '@', 1)`

func scanReceiptSender(row pgx.Row) (ReceiptSender, error) {
	var r ReceiptSender
	err := row.Scan(&r.Amount, &r.Recipient, &r.Client, &r.TxDate, &r.ReceivedAt, &r.SenderPhone, &r.SenderName, &r.GroupJID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Found = true
	return r, nil
}

// ReceiptSenderByMessageID — кто прислал ИМЕННО этот чек (по id его сообщения,
// на которое владелец ответил свайпом).
func (d *DB) ReceiptSenderByMessageID(ctx context.Context, waMessageID string) (ReceiptSender, error) {
	return scanReceiptSender(d.pool.QueryRow(ctx, receiptSenderSelect+`
		WHERE rm.wa_message_id = $1 ORDER BY br.id DESC LIMIT 1`, waMessageID))
}

// LatestReceiptSender — кто прислал ПОСЛЕДНИЙ чек в группе (когда владелец
// спрашивает «с какого номера последний чек», не отвечая на конкретный).
func (d *DB) LatestReceiptSender(ctx context.Context, groupJID string) (ReceiptSender, error) {
	return scanReceiptSender(d.pool.QueryRow(ctx, receiptSenderSelect+`
		WHERE ($1 = '' OR br.group_jid = $1)
		  AND COALESCE(rm.deleted,false) = false
		ORDER BY COALESCE(rm.received_at, br.tx_date) DESC LIMIT 1`, groupJID))
}

// ReceiptOccurrence — одно появление чека (в какой группе, когда, кто прислал).
type ReceiptOccurrence struct {
	GroupJID    string
	ReceivedAt  time.Time
	Submitter   string
	Method      string // по чему совпало: «номер документа» / «код авторизации» / «банк+сумма+время»
	IsDuplicate bool
}

// FindReceiptOccurrences ищет ВСЕ появления ТОГО ЖЕ чека (во всех группах). Тот
// же чек определяется строгим идентификатором (номер документа или код
// авторизации); если их нет — безопасным запасным совпадением банк+сумма+время
// операции (с точностью до 90 секунд) + получатель. Разные платежи с одинаковой
// суммой НЕ считаются одним чеком (нужен строгий id или точное совпадение времени).
func (d *DB) FindReceiptOccurrences(ctx context.Context, ident ReceiptIdentity) ([]ReceiptOccurrence, error) {
	if !ident.Found {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(br.group_jid, rm.wa_group_jid, ''),
		       COALESCE(rm.received_at, br.created_at),
		       COALESCE(NULLIF(po.name,''), NULLIF(br.submitted_by,''), NULLIF(rm.sender_name,''), ''),
		       br.is_duplicate,
		       CASE
		         WHEN $1 <> '' AND COALESCE(br.doc_number,'') = $1 THEN 'номер документа'
		         WHEN $2 <> '' AND COALESCE(br.auth_code,'') = $2 THEN 'код авторизации'
		         ELSE 'банк+сумма+время'
		       END
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE br.ignored = false
		  -- Сумма обязана совпасть ВСЕГДА: банки переиспользуют короткие коды
		  -- авторизации, а номер документа может пересекаться между банками — без
		  -- сверки суммы это склеило бы РАЗНЫЕ платежи в «один чек».
		  AND br.amount = $4::numeric
		  AND (
		    ($1 <> '' AND COALESCE(br.doc_number,'') = $1)
		    OR ($2 <> '' AND COALESCE(br.auth_code,'') = $2)
		    OR ($1 = '' AND $2 = ''
		        AND COALESCE(br.bank,'') = $3
		        AND abs(extract(epoch FROM br.tx_date - $5::timestamptz)) <= 90
		        AND COALESCE(br.recipient_raw,'') = $6)
		  )
		ORDER BY COALESCE(rm.received_at, br.created_at) ASC
	`, ident.DocNumber, ident.AuthCode, ident.Bank, ident.Amount, ident.TxDate, ident.RecipientRaw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptOccurrence
	for rows.Next() {
		var o ReceiptOccurrence
		if err := rows.Scan(&o.GroupJID, &o.ReceivedAt, &o.Submitter, &o.IsDuplicate, &o.Method); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ReattributeReceiptByMessage переписывает получателя у чека, на который
// ответили (свайп), — по id сообщения WhatsApp.
func (d *DB) ReattributeReceiptByMessage(ctx context.Context, waMessageID, name string, contactID *int) (found bool, amount float64, err error) {
	return d.ReattributeReceiptByMessageFrom(ctx, waMessageID, name, contactID, 0)
}

// ReattributeReceiptByMessageFrom — то же, с запоминанием сообщения, которое
// назвало клиента (удалят — привязка откатится).
func (d *DB) ReattributeReceiptByMessageFrom(ctx context.Context, waMessageID, name string, contactID *int, sourceRawID int) (found bool, amount float64, err error) {
	var rid int
	var src any
	if sourceRawID > 0 {
		src = sourceRawID
	}
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET
			card_owner = COALESCE(NULLIF(card_owner, ''), NULLIF(recipient_raw, '')),
			recipient_raw = $2, contact_id = $3, needs_review = false, client_confirmed = true,
			client_source_raw_id = COALESCE($4, client_source_raw_id)
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false
			ORDER BY br.id DESC LIMIT 1
		)
		RETURNING id, amount::float8
	`, waMessageID, name, contactID, src).Scan(&rid, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	// Чек получил клиента — тот же чек, присланный ещё раз, не считаем вторым
	// платежом.
	if _, e := d.DedupeCompletedReceipt(ctx, rid, false, false); e != nil {
		fmt.Println("Проверка повтора чека после привязки клиента не удалась:", e)
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

// LedgerRow — строка «журнала чеков»: кто прислал, кому засчитан, в какую группу.
type LedgerRow struct {
	Kind        string    // "чек", "наличка" или "перевод"
	Client      string    // клиент, кому засчитан платёж
	Amount      float64   // сумма
	TxDate      time.Time // дата операции
	SubmittedBy string    // КТО прислал в WhatsApp
	Collector   string    // кто ЗАБРАЛ наличку (если указан)
	GroupJID    string    // в какую группу прислали
	Bank        string    // банк чека / способ
	CardOwner   string    // получатель на чеке (владелец карты) — для чеков
}

// ReceiptsLedger — ЕДИНЫЙ журнал ВСЕХ платежей за период: и чеки, и наличка/
// переводы. По каждому видно тип (чек/наличка/перевод), клиента, сумму, дату,
// КТО прислал и в какую ГРУППУ, а для налички — кто забрал. Фильтры: person, groupJIDs.
func (d *DB) legacyReceiptsLedger(ctx context.Context, from, to time.Time, person string, groupJIDs []string, limit int) ([]LedgerRow, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT DISTINCT ON (COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
		        'чек' AS kind,
		        COALESCE(c.canonical_name, br.recipient_raw, '') AS client,
		        br.amount::float8 AS amount, br.tx_date,
		        COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''),
		                 split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '') AS submitted_by,
		        COALESCE(br.collector, '') AS collector,
		        COALESCE(br.group_jid, rm.wa_group_jid, '') AS grp,
		        COALESCE(br.bank, '') AS bank,
		        COALESCE(br.card_owner, '') AS card_owner
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.is_duplicate = false AND br.ignored = false AND br.needs_review = false
		  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
		  AND ($3 = '' OR COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $3 || '%')
		  AND ($4::text[] IS NULL OR br.group_jid = ANY($4))
		ORDER BY COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))

		UNION ALL

		(SELECT CASE WHEN t.is_cash THEN 'наличка' ELSE 'перевод' END AS kind,
		        COALESCE(c.canonical_name, t.raw_name, '') AS client,
		        t.amount::float8 AS amount, t.tx_date,
		        COALESCE(NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '') AS submitted_by,
		        COALESCE(t.collector, '') AS collector,
		        COALESCE(rm.wa_group_jid, '') AS grp,
		        COALESCE(t.card_to, '') AS bank,
		        '' AS card_owner
		FROM transactions t
		JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE t.tx_date >= $1 AND t.tx_date < $2
		  AND t.ignored = false AND COALESCE(rm.deleted, false) = false AND t.amount > 0
		  AND `+textCountable("$4")+`
		  AND ($3 = '' OR COALESCE(c.canonical_name, t.raw_name, '') ILIKE '%' || $3 || '%')
		  AND ($4::text[] IS NULL OR rm.wa_group_jid = ANY($4)))

		ORDER BY 4 DESC
		LIMIT $5
	`, from, to, person, groupSliceArg(groupJIDs), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.Kind, &r.Client, &r.Amount, &r.TxDate, &r.SubmittedBy, &r.Collector, &r.GroupJID, &r.Bank, &r.CardOwner); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MissingCheck — платёж (чек ИЛИ наличка), который есть в другой группе, но не
// попал в целевую.
type MissingCheck struct {
	Kind        string // "чек" или "наличка"
	Client      string
	Amount      float64
	TxDate      time.Time
	GroupJID    string // в какой группе он ЕСТЬ
	SubmittedBy string // кто его туда прислал / кто забрал (для налички)
	DocNumber   string
}

// ChecksMissingFromGroup — платежи за период (чеки И наличка), которые попали в
// ДРУГИЕ группы, но которых НЕТ в целевой группе targetJID. «Тот же чек» — по
// номеру операции, без него (и для налички) — по клиенту и сумме. sourceJIDs
// ограничивает группы-источники (nil = все, кроме целевой). person — фильтр по клиенту.
func (d *DB) ChecksMissingFromGroup(ctx context.Context, targetJID string, from, to time.Time, sourceJIDs []string, person string, limit int) ([]MissingCheck, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT 'чек' AS kind,
		        COALESCE(c.canonical_name, br.recipient_raw, '') AS client,
		        br.amount::float8 AS amount, br.tx_date,
		        COALESCE(br.group_jid, rm.wa_group_jid, '') AS grp,
		        COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''),
		                 split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '') AS who,
		        COALESCE(br.doc_number, '') AS doc
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE br.tx_date >= $1 AND br.tx_date < $2
		  AND br.is_duplicate = false AND br.ignored = false AND br.needs_review = false
		  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
		  AND COALESCE(br.group_jid, rm.wa_group_jid, '') <> $3
		  AND ($4::text[] IS NULL OR COALESCE(br.group_jid, rm.wa_group_jid, '') = ANY($4))
		  AND ($6 = '' OR COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $6 || '%')
		  AND NOT EXISTS (
			SELECT 1 FROM bank_receipts t
			LEFT JOIN raw_messages trm ON trm.id = t.raw_message_id
			WHERE COALESCE(t.group_jid, trm.wa_group_jid, '') = $3
			  AND t.is_duplicate = false AND t.ignored = false AND t.needs_review = false
			  AND COALESCE(trm.deleted, false) = false
			  AND (
				-- Есть номер операции — сверяем строго по нему (уникален).
				(NULLIF(br.doc_number, '') IS NOT NULL AND t.doc_number = br.doc_number)
				-- Нет номера — запасной вариант по клиенту+сумме, но в пределах
				-- ±3 дней, иначе ежемесячный платёж на ту же сумму «спрячет»
				-- реально не дошедший.
				OR (NULLIF(br.doc_number, '') IS NULL
					AND br.contact_id IS NOT NULL
					AND t.contact_id = br.contact_id AND t.amount = br.amount
					AND t.tx_date BETWEEN br.tx_date - interval '3 days' AND br.tx_date + interval '3 days')
			  )
		  ))

		UNION ALL

		(SELECT 'наличка' AS kind,
		        COALESCE(c.canonical_name, tx.raw_name, '') AS client,
		        tx.amount::float8 AS amount, tx.tx_date,
		        COALESCE(rm.wa_group_jid, '') AS grp,
		        COALESCE(NULLIF(tx.collector, ''), NULLIF(po.name, ''), NULLIF(rm.sender_name, ''),
		                 split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '') AS who,
		        '' AS doc
		FROM transactions tx
		JOIN raw_messages rm ON rm.id = tx.raw_message_id
		LEFT JOIN contacts c ON c.id = tx.contact_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE tx.tx_date >= $1 AND tx.tx_date < $2
		  AND tx.is_cash = true AND tx.ignored = false AND tx.dup_pending = false
		  AND COALESCE(rm.deleted, false) = false AND tx.amount > 0
		  AND COALESCE(rm.wa_group_jid, '') <> $3
		  AND ($4::text[] IS NULL OR COALESCE(rm.wa_group_jid, '') = ANY($4))
		  AND ($6 = '' OR COALESCE(c.canonical_name, tx.raw_name, '') ILIKE '%' || $6 || '%')
		  AND tx.contact_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM transactions t2
			JOIN raw_messages rm2 ON rm2.id = t2.raw_message_id
			WHERE COALESCE(rm2.wa_group_jid, '') = $3
			  AND t2.is_cash = true AND t2.ignored = false AND COALESCE(rm2.deleted, false) = false
			  AND t2.contact_id = tx.contact_id AND t2.amount = tx.amount
			  -- ±3 дня: у налички нет номера операции, поэтому та же сумма тому же
			  -- клиенту в другом периоде не должна маскировать не дошедшую.
			  AND t2.tx_date BETWEEN tx.tx_date - interval '3 days' AND tx.tx_date + interval '3 days'
		  ))

		ORDER BY 4 DESC
		LIMIT $5
	`, from, to, targetJID, groupSliceArg(sourceJIDs), limit, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingCheck
	for rows.Next() {
		var m MissingCheck
		if err := rows.Scan(&m.Kind, &m.Client, &m.Amount, &m.TxDate, &m.GroupJID, &m.SubmittedBy, &m.DocNumber); err != nil {
			return nil, err
		}
		out = append(out, m)
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

// StoredMessage — сохранённое сообщение из группы (для «что писал номер X»).
type StoredMessage struct {
	GroupJID   string
	Who        string // имя из памяти номеров или пуш-имя отправителя
	Phone      string // номер отправителя (часть до @)
	Body       string
	HasMedia   bool
	Deleted    bool // сообщение удалили в WhatsApp, но тело у нас осталось
	ReceivedAt time.Time
}

// MessagesFrom возвращает СОХРАНЁННЫЕ сообщения отправителя по последним цифрам
// номера и/или имени за период — ВКЛЮЧАЯ удалённые в WhatsApp (тело сохраняется
// при получении и НЕ стирается при удалении). Так владелец может поднять, что
// написал конкретный номер, даже если тот удалил сообщение «у всех».
func (d *DB) MessagesFrom(ctx context.Context, phoneSuffix, nameLike string, from, to *time.Time, groupJID string, limit int) ([]StoredMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT rm.wa_group_jid,
		       COALESCE(NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), '') AS who,
		       split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
		       COALESCE(rm.body, ''), rm.has_media, COALESCE(rm.deleted, false), rm.received_at
		FROM raw_messages rm
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE ($1 = '' OR split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) LIKE '%' || $1)
		  AND ($2 = '' OR COALESCE(po.name, '') ILIKE '%' || $2 || '%' OR COALESCE(rm.sender_name, '') ILIKE '%' || $2 || '%')
		  AND ($3::timestamptz IS NULL OR rm.received_at >= $3::timestamptz)
		  AND ($4::timestamptz IS NULL OR rm.received_at < $4::timestamptz)
		  AND ($5 = '' OR rm.wa_group_jid = $5)
		ORDER BY rm.received_at DESC
		LIMIT $6
	`, phoneSuffix, nameLike, from, to, groupJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.GroupJID, &m.Who, &m.Phone, &m.Body, &m.HasMedia, &m.Deleted, &m.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SenderCandidate — кандидат-получатель личного сообщения (реальный номер).
type SenderCandidate struct {
	Phone string
	Name  string
}

// SenderCandidates ищет РЕАЛЬНЫЕ номера по последним цифрам и/или имени, чтобы
// отправить личное сообщение по частичному номеру или имени. Берём ТОЛЬКО тех,
// у кого известен настоящий телефон: либо сохранён sender_phone (из SenderAlt —
// так находятся и LID-отправители, чей реальный номер мы поймали), либо
// sender_jid уже в видимой форме @s.whatsapp.net. Голый @lid как номер НЕ
// предлагаем — иначе личное сообщение ушло бы на псевдо-номер. Имя из памяти
// номеров подхватывается join'ом по этому же реальному телефону.
func (d *DB) SenderCandidates(ctx context.Context, phoneSuffix, nameLike string, limit int) ([]SenderCandidate, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	// realPhone — реальный телефон отправителя: предпочитаем sender_phone, иначе
	// sender_jid, НО только если он телефонной формы (@s.whatsapp.net); для голого
	// @lid даёт NULL, и такая строка отсеивается (кандидатом не станет).
	const realPhone = `split_part(COALESCE(NULLIF(rm.sender_phone, ''),
		CASE WHEN rm.sender_jid LIKE '%@s.whatsapp.net' THEN rm.sender_jid END), '@', 1)`
	rows, err := d.pool.Query(ctx, `
		SELECT phone, COALESCE(MAX(name), '') AS name
		FROM (
			SELECT `+realPhone+` AS phone,
			       COALESCE(NULLIF(po.name, ''), NULLIF(rm.sender_name, '')) AS name,
			       rm.received_at AS seen
			FROM raw_messages rm
			LEFT JOIN phone_owners po ON po.phone = `+realPhone+`
			WHERE (NULLIF(rm.sender_phone, '') IS NOT NULL OR rm.sender_jid LIKE '%@s.whatsapp.net')
			  AND ($1 = '' OR `+realPhone+` LIKE '%' || $1)
			  AND ($2 = '' OR COALESCE(po.name, '') ILIKE '%' || $2 || '%' OR COALESCE(rm.sender_name, '') ILIKE '%' || $2 || '%')
		) t
		WHERE phone IS NOT NULL AND length(phone) BETWEEN 10 AND 15
		GROUP BY phone
		ORDER BY MAX(seen) DESC NULLS LAST
		LIMIT $3
	`, phoneSuffix, nameLike, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SenderCandidate
	for rows.Next() {
		var c SenderCandidate
		if err := rows.Scan(&c.Phone, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetReceiptCollectorByMessage привязывает чек (по id сообщения WhatsApp)
// к ответственному, который "забрал" деньги — заполняет submitted_by.
// Возвращает данные чека для подтверждения.
func (d *DB) SetReceiptCollectorByMessage(ctx context.Context, waMessageID, collector string) (found bool, amount float64, recipient string, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts SET collector = $2
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
		UPDATE bank_receipts SET collector = $2
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
	ID          int
	GroupJID    string
	SenderJID   string
	ClientText  string
	ClientID    string
	ClientName  string
	Candidates  string
	Amount      float64
	TxDate      time.Time
	Status      string
	CreatedAt   time.Time
	WaMessageID string // сообщение с чеком (для DueCmfWatches)
}

// InsertCmfWatch создаёт наблюдение, возвращает id.
// ErrWatchExists — у чека уже есть наблюдение (его завели параллельно).
var ErrWatchExists = errors.New("наблюдение по чеку уже есть")

func (d *DB) InsertCmfWatch(ctx context.Context, rawMessageID int, groupJID, senderJID, clientText string, amount float64, txDate time.Time, status string) (int, error) {
	// Одно наблюдение на чек: заводят его и разбор чека, и подбор «забытых»
	// чеков — блокировка по сообщению не даёт завести два.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(734200119, $1)`, rawMessageID); err != nil {
		return 0, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cmf_watch WHERE raw_message_id = $1)`, rawMessageID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists && rawMessageID > 0 {
		return 0, ErrWatchExists
	}
	var id int
	if err := tx.QueryRow(ctx, `
		INSERT INTO cmf_watch (raw_message_id, group_jid, sender_jid, client_text, amount, tx_date, status)
		VALUES (NULLIF($1, 0), $2, $3, NULLIF($4, ''), $5, $6, $7)
		RETURNING id
	`, rawMessageID, groupJID, senderJID, clientText, amount, txDate, status).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
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

// TouchCmfWatch — наблюдение проверяли (даже если программа ответила ошибкой).
func (d *DB) TouchCmfWatch(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `UPDATE cmf_watch SET checked_at = now() WHERE id = $1`, id)
	return err
}

// LookupWatchesForRetry — чеки, по которым поиск клиента в программе не
// удался, — давно не проверявшиеся первыми.
func (d *DB) LookupWatchesForRetry(ctx context.Context, limit int) ([]CmfWatch, error) {
	return d.cmfWatchesByStatus(ctx, "lookup", time.Now().Add(-10*time.Minute), time.Now().AddDate(0, 0, -30), limit)
}

// NonameWatchesWithClient — сверка «без клиента», а у чека клиент уже назван.
func (d *DB) NonameWatchesWithClient(ctx context.Context, limit int) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT cw.id, cw.group_jid, COALESCE(cw.sender_jid,''), COALESCE(c.canonical_name, br.recipient_raw, ''),
		       cw.amount::float8, cw.tx_date, cw.created_at
		FROM cmf_watch cw
		JOIN bank_receipts br ON br.raw_message_id = cw.raw_message_id
		JOIN raw_messages rm ON rm.id = cw.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE cw.status = 'noname' AND cw.created_at >= now() - interval '30 days'
		  AND br.client_confirmed AND NOT br.needs_review AND NOT br.ignored AND NOT br.is_duplicate
		  AND NOT COALESCE(rm.deleted, false) AND COALESCE(c.canonical_name, br.recipient_raw, '') <> ''
		ORDER BY cw.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfWatch
	for rows.Next() {
		var w CmfWatch
		if err := rows.Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.Amount, &w.TxDate, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DueCmfWatches — наблюдения со статусом watch, созданные раньше cutoff:
// пора проверить, внесён ли платёж в программу.
func (d *DB) DueCmfWatches(ctx context.Context, cutoff time.Time, limit int) ([]CmfWatch, error) {
	return d.cmfWatchesByStatus(ctx, "watch", cutoff, time.Time{}, limit)
}

// RemindedCmfWatches — по чекам уже напомнили; проверяем, не внесли ли с тех пор
// (чтобы «внеси в программу» потом не брал их за невнесённые).
func (d *DB) RemindedCmfWatches(ctx context.Context, since time.Time, limit int) ([]CmfWatch, error) {
	return d.cmfWatchesByStatus(ctx, "reminded", time.Now().Add(time.Hour), since, limit)
}

func (d *DB) cmfWatchesByStatus(ctx context.Context, status string, cutoff, since time.Time, limit int) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT cw.id, cw.group_jid, COALESCE(cw.sender_jid,''), COALESCE(cw.client_text,''), COALESCE(cw.client_id,''),
		       COALESCE(cw.client_name,''), COALESCE(cw.candidates,''), cw.amount::float8, cw.tx_date, cw.status, cw.created_at,
		       COALESCE(rm.wa_message_id, '')
		FROM cmf_watch cw
		LEFT JOIN raw_messages rm ON rm.id = cw.raw_message_id
		WHERE cw.status = $3 AND cw.created_at < $1 AND cw.created_at >= $4
		  AND NOT COALESCE(rm.deleted, false)
		  -- Чек исключили из учёта («скинули по ошибке») или это дубль — не напоминаем.
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = cw.raw_message_id
		                  AND (br.ignored OR br.is_duplicate))
		-- Давно не проверявшиеся — первыми: чек, по которому программа отвечает
		-- ошибкой, не загораживает очередь остальным.
		ORDER BY cw.checked_at NULLS FIRST, cw.created_at
		LIMIT $2
	`, cutoff, limit, status, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfWatch
	for rows.Next() {
		var w CmfWatch
		if err := rows.Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.ClientID, &w.ClientName,
			&w.Candidates, &w.Amount, &w.TxDate, &w.Status, &w.CreatedAt, &w.WaMessageID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SyncWatchWithReceipt — у чека поправили сумму или дату: сверка с программой
// ждёт уже исправленное (а не ошибку распознавания). Привязанное наблюдение,
// которое уже нашли/напомнили, проверяется заново.
func (d *DB) SyncWatchWithReceipt(ctx context.Context, receiptID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE cmf_watch cw SET amount = br.amount, tx_date = br.tx_date,
			status = CASE WHEN cw.status IN ('found', 'reminded') AND COALESCE(cw.client_id, '') <> '' THEN 'watch' ELSE cw.status END
		FROM bank_receipts br
		WHERE br.id = $1 AND cw.raw_message_id = br.raw_message_id AND cw.status <> 'deleted'
		  AND br.amount > 0 AND (cw.amount <> br.amount OR cw.tx_date <> br.tx_date)`, receiptID)
	return err
}

// OpenWatchesForClientAmount — невнесённые чеки клиента программы на эту сумму
// (для «внеси чек … в программу» без даты), старые первыми. Исключённые и
// дубли не берём.
func (d *DB) OpenWatchesForClientAmount(ctx context.Context, clientID string, amount float64) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT cw.id, cw.group_jid, COALESCE(cw.sender_jid,''), COALESCE(cw.client_text,''), COALESCE(cw.client_id,''),
		       COALESCE(cw.client_name,''), COALESCE(cw.candidates,''), cw.amount::float8, cw.tx_date, cw.status, cw.created_at,
		       COALESCE(rm.wa_message_id, '')
		FROM cmf_watch cw
		LEFT JOIN raw_messages rm ON rm.id = cw.raw_message_id
		WHERE cw.client_id = $1 AND abs(cw.amount - $2) < 1 AND cw.status IN ('watch', 'reminded')
		  AND NOT COALESCE(rm.deleted, false)
		  AND NOT EXISTS (SELECT 1 FROM bank_receipts br WHERE br.raw_message_id = cw.raw_message_id
		                  AND (br.ignored OR br.is_duplicate))
		  AND cw.tx_date >= now() - interval '120 days'
		ORDER BY cw.tx_date`, clientID, amount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfWatch
	for rows.Next() {
		var w CmfWatch
		if err := rows.Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.ClientID, &w.ClientName,
			&w.Candidates, &w.Amount, &w.TxDate, &w.Status, &w.CreatedAt, &w.WaMessageID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// notGroupForwardCopy — не копия чека, пересланного из ГРУППЫ (про неё
// спрашивают и её сверяют в исходной группе). Копия из лички владельца — сама
// себе основная запись: про неё спрашиваем в целевой группе.
const notGroupForwardCopy = `AND NOT (COALESCE(rm.sender_jid, '') = 'bot-forward' AND EXISTS (
			SELECT 1 FROM raw_messages orm JOIN bank_receipts ob ON ob.raw_message_id = orm.id
			WHERE COALESCE(orm.wa_group_jid, '') LIKE '%@g.us' AND COALESCE(orm.sender_jid, '') <> 'bot-forward'
			  AND starts_with(rm.wa_message_id, orm.wa_message_id || '-fwd-')))`

// IsGroupForwardCopy — сообщение — копия чека, пересланного из рабочей группы.
func (d *DB) IsGroupForwardCopy(ctx context.Context, rawID int) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM raw_messages rm WHERE rm.id = $1
		AND NOT (TRUE `+notGroupForwardCopy+`))`, rawID).Scan(&ok)
	return ok, err
}

// ReceiptClientByMessage — клиент чека по id сообщения: имя и назван ли он
// точно (ФИО рядом с чеком / ответ на «чей чек?»).
func (d *DB) ReceiptClientByMessage(ctx context.Context, waID string) (name string, confirmed bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COALESCE(c.canonical_name, br.recipient_raw, ''), br.client_confirmed AND NOT br.needs_review
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1`, waID).Scan(&name, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return
}

// SyncForwardCopies — копии чеков, пересланных из групп, повторяют исходный
// чек (клиент, сумма, дата, исключён/дубль/удалён). Обычно это делает триггер
// при изменении исходного; здесь — для только что созданных копий и на случай,
// если исходный удалили. Возвращает, сколько копий обновили.
func (d *DB) SyncForwardCopies(ctx context.Context) (int64, error) { return d.SyncForwardCopy(ctx, 0) }

// SyncForwardCopy — то же для одной копии (copyRawID; 0 — для всех, кроме
// поправленных владельцем в целевой группе).
func (d *DB) SyncForwardCopy(ctx context.Context, copyRawID int) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts cp SET
			ignored = src.ignored OR COALESCE(srm.deleted, false),
			is_duplicate = src.is_duplicate,
			amount = CASE WHEN src.amount > 0 THEN src.amount ELSE cp.amount END,
			tx_date = src.tx_date,
			contact_id = src.contact_id, recipient_raw = src.recipient_raw,
			client_confirmed = src.client_confirmed,
			needs_review = src.needs_review OR NOT (CASE WHEN src.amount > 0 THEN src.amount ELSE cp.amount END > 0),
			clarify_asked = true
		FROM bank_receipts src
		JOIN raw_messages srm ON srm.id = src.raw_message_id AND COALESCE(srm.sender_jid, '') <> 'bot-forward'
		JOIN raw_messages crm ON starts_with(crm.wa_message_id, srm.wa_message_id || '-fwd-') AND crm.sender_jid = 'bot-forward'
		WHERE cp.raw_message_id = crm.id AND NOT cp.mirror_locked AND ($1 = 0 OR crm.id = $1)
		  AND (cp.ignored IS DISTINCT FROM (src.ignored OR COALESCE(srm.deleted, false))
		       OR cp.is_duplicate IS DISTINCT FROM src.is_duplicate
		       OR (src.amount > 0 AND cp.amount IS DISTINCT FROM src.amount)
		       OR cp.tx_date IS DISTINCT FROM src.tx_date
		       OR cp.contact_id IS DISTINCT FROM src.contact_id
		       OR cp.client_confirmed IS DISTINCT FROM src.client_confirmed
		       OR cp.needs_review IS DISTINCT FROM (src.needs_review OR NOT (CASE WHEN src.amount > 0 THEN src.amount ELSE cp.amount END > 0)))`, copyRawID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReceiptIDByMessage — чек сообщения (по id WhatsApp).
func (d *DB) ReceiptIDByMessage(ctx context.Context, waID string) (int, bool, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		SELECT br.id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 AND br.is_duplicate = false ORDER BY br.id DESC LIMIT 1`, waID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// ReceiptWatchInfo — данные чека для сверки с программой (по id WhatsApp):
// сообщение, группа, отправитель, сумма, дата; есть ли уже наблюдение.
type ReceiptWatchInfo struct {
	RawID     int
	GroupJID  string
	SenderJID string
	Amount    float64
	TxDate    time.Time
	HasWatch  bool
	Countable bool // клиент подтверждён, сумма есть, не дубль и не исключён
}

func (d *DB) ReceiptWatchInfoByMessage(ctx context.Context, waID string) (ReceiptWatchInfo, bool, error) {
	var r ReceiptWatchInfo
	err := d.pool.QueryRow(ctx, `
		SELECT br.raw_message_id, COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''),
		       br.amount::float8, br.tx_date,
		       EXISTS (SELECT 1 FROM cmf_watch cw WHERE cw.raw_message_id = br.raw_message_id),
		       br.amount > 0 AND br.client_confirmed AND NOT br.needs_review AND NOT br.is_duplicate AND NOT br.ignored
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 AND NOT COALESCE(rm.deleted, false)
		ORDER BY br.is_duplicate, br.id DESC LIMIT 1`, waID).
		Scan(&r.RawID, &r.GroupJID, &r.SenderJID, &r.Amount, &r.TxDate, &r.HasWatch, &r.Countable)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// SetForwardSentID — id фото, которое бот отправил в группу как копию чека.
func (d *DB) SetForwardSentID(ctx context.Context, copyRawID int, sentID string) error {
	_, err := d.pool.Exec(ctx, `UPDATE raw_messages SET fwd_sent_id = $2 WHERE id = $1`, copyRawID, sentID)
	return err
}

// CopyBySentID — копия чека по id отправленного ботом фото (для удаления).
func (d *DB) CopyBySentID(ctx context.Context, sentID string) (string, bool, error) {
	var wa string
	err := d.pool.QueryRow(ctx, `SELECT wa_message_id FROM raw_messages WHERE fwd_sent_id = $1 LIMIT 1`, sentID).Scan(&wa)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return wa, err == nil, err
}

// CloneReceiptToCopy — копия чека в целевой группе повторяет исходный чек
// (сумма, дата, клиент, номера). Тот же чек, уже присланный в эту группу
// вручную, — копия дубль.
func (d *DB) CloneReceiptToCopy(ctx context.Context, srcReceiptID, copyRawID int, targetGroup string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO bank_receipts
			(raw_message_id, bank, recipient_raw, recipient_bank, recipient_phone, sender_raw, sender_bank, sender_account,
			 card_owner, contact_id, amount, commission, doc_number, auth_code, status, needs_review, is_duplicate,
			 client_confirmed, group_jid, submitted_by, tx_date, collapsed, ignored, clarify_asked)
		SELECT $2, bank, recipient_raw, recipient_bank, recipient_phone, sender_raw, sender_bank, sender_account,
		       card_owner, contact_id, amount, commission, doc_number, auth_code, status, needs_review OR amount <= 0,
		       is_duplicate OR EXISTS (
		           SELECT 1 FROM bank_receipts o JOIN raw_messages orm ON orm.id = o.raw_message_id
		           WHERE o.group_jid = $3 AND o.ignored = false AND o.is_duplicate = false AND NOT COALESCE(orm.deleted, false)
		             AND ((NULLIF(src.doc_number, '') IS NOT NULL AND o.doc_number = src.doc_number)
		                  OR (NULLIF(src.auth_code, '') IS NOT NULL AND o.auth_code = src.auth_code AND o.amount = src.amount))),
		       client_confirmed, $3, submitted_by, tx_date, collapsed, ignored, true
		FROM bank_receipts src WHERE src.id = $1`, srcReceiptID, copyRawID, targetGroup)
	if err != nil {
		return err
	}
	_, err = d.pool.Exec(ctx, `UPDATE raw_messages SET parsed = true WHERE id = $1`, copyRawID)
	return err
}

// lockCopy — владелец поправил копию чека в целевой группе: исходный её больше
// не перезаписывает.
func (d *DB) lockCopy(ctx context.Context, receiptID int) {
	_, _ = d.pool.Exec(ctx, `UPDATE bank_receipts br SET mirror_locked = true FROM raw_messages rm
		WHERE br.id = $1 AND rm.id = br.raw_message_id AND rm.sender_jid = 'bot-forward'`, receiptID)
}

// ReceiptsWithoutWatch — засчитанные чеки последних дней без наблюдения сверки
// (клиента назвали позже, «это не дубль», дубль стал основным после удаления
// первого): им тоже нужна сверка с программой. Копии из групп не берём.
type MissingWatch struct {
	WaID      string
	RawID     int
	GroupJID  string
	SenderJID string
	Client    string
	Amount    float64
	TxDate    time.Time
}

func (d *DB) ReceiptsWithoutWatch(ctx context.Context, since time.Time, limit int) ([]MissingWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(rm.wa_message_id, ''), br.raw_message_id, COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''),
		       COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.created_at >= $1 AND br.tx_date >= now() - interval '30 days' AND br.amount > 0 AND br.client_confirmed AND NOT br.needs_review
		  AND NOT br.is_duplicate AND NOT br.ignored AND NOT COALESCE(rm.deleted, false)
		  AND NOT EXISTS (SELECT 1 FROM cmf_watch cw WHERE cw.raw_message_id = br.raw_message_id)
		  `+notGroupForwardCopy+`
		ORDER BY br.created_at LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingWatch
	for rows.Next() {
		var m MissingWatch
		if err := rows.Scan(&m.WaID, &m.RawID, &m.GroupJID, &m.SenderJID, &m.Client, &m.Amount, &m.TxDate); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ForwardCopyGroups — группы, куда бот переслал копию чека.
func (d *DB) ForwardCopyGroups(ctx context.Context, origWa string) ([]string, error) {
	if origWa == "" {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT wa_group_jid FROM raw_messages
		WHERE sender_jid = 'bot-forward' AND starts_with(wa_message_id, $1 || '-fwd-') AND NOT COALESCE(deleted, false)`, origWa)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UnnamedReceiptOfSenderBefore — чек сотрудника без клиента, присланный не
// раньше чем за window до at (импорт истории: «чек, потом ФИО»).
func (d *DB) UnnamedReceiptOfSenderBefore(ctx context.Context, groupJID, senderJID string, at time.Time, window time.Duration) (string, bool, error) {
	var wa string
	err := d.pool.QueryRow(ctx, `
		SELECT rm.wa_message_id FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND rm.sender_jid = $2
		  AND rm.received_at <= $3 AND rm.received_at >= $3::timestamptz - make_interval(secs => $4)
		  AND br.client_confirmed = false AND br.is_duplicate = false AND br.ignored = false
		  AND NOT COALESCE(rm.deleted, false)
		ORDER BY rm.received_at DESC LIMIT 1`, groupJID, senderJID, at, int(window.Seconds())).Scan(&wa)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return wa, err == nil, err
}

// SenderTextsBefore — тексты сотрудника за window до at (самые близкие первыми).
func (d *DB) SenderTextsBefore(ctx context.Context, groupJID, senderJID string, at time.Time, window time.Duration) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(body, '') FROM raw_messages
		WHERE wa_group_jid = $1 AND sender_jid = $2 AND has_media = false AND handled_as IS NULL
		  AND NOT COALESCE(deleted, false) AND received_at < $3 AND received_at >= $3::timestamptz - make_interval(secs => $4)
		ORDER BY received_at DESC LIMIT 3`, groupJID, senderJID, at, int(window.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkReceiptsAskedByRaw — не спрашивать «чей чек?» про чек сообщения (импорт истории молчит).
func (d *DB) MarkReceiptsAskedByRaw(ctx context.Context, rawID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET clarify_asked = true WHERE raw_message_id = $1`, rawID)
	return err
}

// ReceiptRef — сообщение и группа чека (по id чека).
func (d *DB) ReceiptRef(ctx context.Context, receiptID int) (rawID int, waID, groupJID string, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COALESCE(br.raw_message_id, 0), COALESCE(rm.wa_message_id, ''), COALESCE(br.group_jid, '')
		FROM bank_receipts br LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id WHERE br.id = $1`, receiptID).
		Scan(&rawID, &waID, &groupJID)
	return
}

// SetCmfWatchAsk запоминает id сообщения бота по наблюдению (вопрос, переспрос,
// подтверждение, напоминание) и варианты, которые были в этом сообщении (JSON;
// пусто — вариантов не было). Прежние id не забываются — свайп на любой работает.
func (d *DB) SetCmfWatchAsk(ctx context.Context, id int, askMsgID, options string) error {
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO cmf_watch_asks (ask_msg_id, watch_id, options) VALUES ($1, $2, NULLIF($3, ''))
		ON CONFLICT (ask_msg_id, watch_id) DO UPDATE SET options = EXCLUDED.options`, askMsgID, id, options); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE cmf_watch SET ask_msg_id = $2 WHERE id = $1`, id, askMsgID)
	return err
}

// CmfAskRef — чек, которого касается сообщение бота, и варианты в этом сообщении.
type CmfAskRef struct {
	WatchID int
	Options string // JSON вариантов, показанных в этом сообщении ("" — не сохранены)
}

// CmfWatchesByAsk — все чеки, которых касается сообщение бота askMsgID.
func (d *DB) CmfWatchesByAsk(ctx context.Context, askMsgID string) ([]CmfAskRef, error) {
	if askMsgID == "" {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT watch_id, COALESCE(options, '') FROM cmf_watch_asks WHERE ask_msg_id = $1
		UNION
		SELECT id, '' FROM cmf_watch WHERE ask_msg_id = $1 AND id NOT IN (SELECT watch_id FROM cmf_watch_asks WHERE ask_msg_id = $1)
		ORDER BY 1`, askMsgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CmfAskRef
	for rows.Next() {
		var r CmfAskRef
		if err := rows.Scan(&r.WatchID, &r.Options); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CmfWatchStatusByMessages — статус сверки по id сообщений с чеками.
func (d *DB) CmfWatchStatusByMessages(ctx context.Context, waIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(waIDs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (rm.wa_message_id) rm.wa_message_id, cw.status
		FROM cmf_watch cw JOIN raw_messages rm ON rm.id = cw.raw_message_id
		WHERE rm.wa_message_id = ANY($1)
		ORDER BY rm.wa_message_id, cw.id DESC`, waIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var wa, st string
		if err := rows.Scan(&wa, &st); err != nil {
			return nil, err
		}
		out[wa] = st
	}
	return out, rows.Err()
}

// CmfPayerHistory — прежние решения по чекам с похожей подписью-плательщиком
// (для «платит за разных»): подписи, клиенты и статусы.
func (d *DB) CmfPayerHistory(ctx context.Context, firstWord string, excludeID int) ([]CmfWatch, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, group_jid, COALESCE(sender_jid,''), COALESCE(client_text,''), COALESCE(client_id,''),
		       COALESCE(client_name,''), COALESCE(candidates,''), amount::float8, tx_date, status, created_at
		FROM cmf_watch
		WHERE id <> $2 AND client_text ILIKE $1 || '%'
		  AND status IN ('watch', 'reminded', 'found', 'not_in_program')
		ORDER BY id DESC LIMIT 200`, firstWord, excludeID)
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

// TestReceipt — состояние чека по id сообщения (для тестов).
type TestReceipt struct {
	Recipient   string
	Amount      float64
	NeedsReview bool
	Confirmed   bool
	Duplicate   bool
	TxDate      time.Time
	Found       bool
}

// TestReceiptByMessage — чек по id сообщения WhatsApp (только для тестов).
func (d *DB) TestReceiptByMessage(ctx context.Context, waMessageID string) TestReceipt {
	var r TestReceipt
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(br.recipient_raw,''), br.amount::float8, br.needs_review, br.client_confirmed, br.is_duplicate, br.tx_date
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 ORDER BY br.id DESC LIMIT 1`, waMessageID).
		Scan(&r.Recipient, &r.Amount, &r.NeedsReview, &r.Confirmed, &r.Duplicate, &r.TxDate)
	r.Found = err == nil
	return r
}

// TestAgeGroup — «состарить» чеки и платежи группы на d (только для тестов:
// чтобы клариф-цикл увидел их как ждущие вопроса).
func (d *DB) TestAgeGroup(ctx context.Context, groupJID string, by time.Duration) error {
	sec := int(by.Seconds())
	if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET created_at = created_at - make_interval(secs => $2) WHERE group_jid = $1`, groupJID, sec); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE transactions t SET created_at = t.created_at - make_interval(secs => $2)
		FROM raw_messages rm WHERE rm.id = t.raw_message_id AND rm.wa_group_jid = $1`, groupJID, sec)
	return err
}

// TestAgeCmfWatches — «состарить» наблюдения сверки (только для тестов).
func (d *DB) TestAgeCmfWatches(ctx context.Context, by time.Duration) error {
	_, err := d.pool.Exec(ctx, `UPDATE cmf_watch SET created_at = created_at - make_interval(secs => $1)`, int(by.Seconds()))
	return err
}

// TestCmfWatchState — статус, клиент и сумма наблюдения по чеку (для тестов).
func (d *DB) TestCmfWatchState(ctx context.Context, waID string) (status, clientID string, amount float64) {
	_ = d.pool.QueryRow(ctx, `
		SELECT cw.status, COALESCE(cw.client_id, ''), cw.amount::float8 FROM cmf_watch cw
		JOIN raw_messages rm ON rm.id = cw.raw_message_id WHERE rm.wa_message_id = $1
		ORDER BY cw.id DESC LIMIT 1`, waID).Scan(&status, &clientID, &amount)
	return
}

// TestResetAll — очистить все данные (только для тестовой БД!): каждый
// сквозной тест начинает с чистой базы.
func (d *DB) TestResetAll(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `TRUNCATE transactions, bank_receipts, raw_messages, pending_client_names,
		cmf_watch, cmf_watch_asks, contacts, contact_aliases, expenses, forward_rules, phone_owners,
		scheduled_jobs, assistant_history, bot_settings, amount_corrections RESTART IDENTITY CASCADE`)
	return err
}

// TestResetCmfWatches — очистка наблюдений сверки (только для тестовой БД).
func (d *DB) TestResetCmfWatches(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM cmf_watch_asks; DELETE FROM cmf_watch`)
	return err
}

// CmfWatchByAsk — наблюдение, по которому бот написал сообщение askMsgID.
func (d *DB) CmfWatchByAsk(ctx context.Context, askMsgID string) (int, bool, error) {
	if askMsgID == "" {
		return 0, false, nil
	}
	var id int
	err := d.pool.QueryRow(ctx, `
		SELECT watch_id FROM cmf_watch_asks WHERE ask_msg_id = $1
		UNION ALL
		SELECT id FROM cmf_watch WHERE ask_msg_id = $1
		LIMIT 1`, askMsgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ClearCmfWatchClient снимает привязку наблюдения к клиенту (клиента назвали
// неверно) и ставит статус.
func (d *DB) ClearCmfWatchClient(ctx context.Context, id int, status string) error {
	_, err := d.pool.Exec(ctx, `UPDATE cmf_watch SET client_id = NULL, client_name = NULL, status = $2, checked_at = now() WHERE id = $1`, id, status)
	return err
}

// CmfWatchByCheckMessage — наблюдение по id сообщения с самим чеком.
func (d *DB) CmfWatchByCheckMessage(ctx context.Context, waMessageID string) (int, bool, error) {
	if waMessageID == "" {
		return 0, false, nil
	}
	var id int
	err := d.pool.QueryRow(ctx, `
		SELECT cw.id FROM cmf_watch cw
		JOIN raw_messages rm ON rm.id = cw.raw_message_id
		WHERE rm.wa_message_id = $1
		ORDER BY cw.id DESC LIMIT 1`, waMessageID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// CmfWatchFull — наблюдение вместе с id сообщения-чека в WhatsApp.
type CmfWatchFull struct {
	CmfWatch
	WaMessageID string
	Deleted     bool // сообщение с чеком удалили в WhatsApp
}

// CmfWatchByID возвращает наблюдение по id (и id сообщения с чеком).
func (d *DB) CmfWatchByID(ctx context.Context, id int) (CmfWatchFull, bool, error) {
	var w CmfWatchFull
	err := d.pool.QueryRow(ctx, `
		SELECT cw.id, cw.group_jid, COALESCE(cw.sender_jid,''), COALESCE(cw.client_text,''), COALESCE(cw.client_id,''),
		       COALESCE(cw.client_name,''), COALESCE(cw.candidates,''), cw.amount::float8, cw.tx_date, cw.status, cw.created_at,
		       COALESCE(rm.wa_message_id,''), COALESCE(rm.deleted, false)
		FROM cmf_watch cw
		LEFT JOIN raw_messages rm ON rm.id = cw.raw_message_id
		WHERE cw.id = $1
	`, id).Scan(&w.ID, &w.GroupJID, &w.SenderJID, &w.ClientText, &w.ClientID, &w.ClientName,
		&w.Candidates, &w.Amount, &w.TxDate, &w.Status, &w.CreatedAt, &w.WaMessageID, &w.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, false, nil
	}
	if err != nil {
		return w, false, err
	}
	return w, true, nil
}

// SetReceiptClientByMessage — по ответу в группе «это оплата за клиента X»
// записывает клиента чеку (по id сообщения с чеком). В отличие от
// ReattributeReceiptByMessage НЕ трогает владельца карты: прежнее имя здесь —
// плательщик из подписи, а не получатель на чеке.
func (d *DB) SetReceiptClientByMessage(ctx context.Context, waMessageID, name string, contactID *int) (bool, error) {
	// Текстовая копия этого платежа («Магомедов Ахмед 15000» рядом с чеком)
	// следует за чеком при каждой смене клиента — иначе текст и чек разойдутся
	// по разным клиентам и платёж посчитается дважды.
	var twins []int
	if rid, ok, err := d.ReceiptIDByMessage(ctx, waMessageID); err == nil && ok {
		twins, _ = d.ReceiptTextTwins(ctx, rid)
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET recipient_raw = $2, contact_id = $3, needs_review = false, client_confirmed = true
		WHERE id = (
			SELECT br.id FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE rm.wa_message_id = $1 AND br.is_duplicate = false
			ORDER BY br.id DESC LIMIT 1
		)
	`, waMessageID, name, contactID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 && contactID != nil {
		for _, tx := range twins {
			_, _ = d.pool.Exec(ctx, `UPDATE transactions SET contact_id = $2, raw_name = $3 WHERE id = $1`, tx, *contactID, name)
		}
	}
	return tag.RowsAffected() > 0, nil
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

// ContactStat — контакт с числом привязанных записей (для поиска дублей:
// при слиянии оставляем того, у кого записей больше — меньше риск потерять связь).
type ContactStat struct {
	Name    string
	Records int // чеки + платежи (не удалённые/не дубли)
}

// ContactsWithCounts возвращает все контакты и сколько к каждому привязано
// учётных записей. Нужен для «разбери дубли»: бот группирует похожие имена.
func (d *DB) ContactsWithCounts(ctx context.Context) ([]ContactStat, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.canonical_name,
		       (SELECT COUNT(*) FROM bank_receipts br WHERE br.contact_id = c.id AND br.is_duplicate = false AND br.ignored = false)
		     + (SELECT COUNT(*) FROM transactions t WHERE t.contact_id = c.id AND t.ignored = false) AS records
		FROM contacts c
		ORDER BY c.canonical_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactStat
	for rows.Next() {
		var s ContactStat
		if err := rows.Scan(&s.Name, &s.Records); err != nil {
			return nil, err
		}
		out = append(out, s)
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
func (d *DB) legacyPersonReport(ctx context.Context, personQuery string, from, to *time.Time, groupJID string) ([]PersonStats, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.canonical_name,
			COALESCE(r.cnt, 0), COALESCE(r.total, 0),
			COALESCE(t.cnt, 0), COALESCE(t.total, 0),
			LEAST(r.first_op, t.first_op), GREATEST(r.last_op, t.last_op)
		FROM contacts c
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS cnt, SUM(x.amount) AS total,
			       MIN(x.tx_date) AS first_op, MAX(x.tx_date) AS last_op
			FROM (
				-- Один чек, присланный в несколько групп (или пересланный), — один
				-- платёж, как в сборе (SummaryForPeriod).
				SELECT DISTINCT ON (br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
				       br.amount, br.tx_date
				FROM bank_receipts br
				LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
				WHERE br.contact_id = c.id
				  -- needs_review=false — как в сборе (SummaryForPeriod): неподтверждённые
				  -- чеки в «всего» не считаем, иначе отчёт по человеку расходится со сбором.
				  AND br.needs_review = false
				  AND br.is_duplicate = false AND br.ignored = false
				  AND COALESCE(rm.deleted, false) = false
				  AND ($2::timestamptz IS NULL OR br.tx_date >= $2)
				  AND ($3::timestamptz IS NULL OR br.tx_date < $3)
				  AND ($4 = '' OR br.group_jid = $4)
				  AND NOT EXISTS (
					SELECT 1 FROM bank_receipts o
					JOIN raw_messages orm ON orm.id = o.raw_message_id
					WHERE o.id <> br.id AND o.contact_id = c.id
					  AND o.needs_review = false AND o.is_duplicate = false AND o.ignored = false
					  AND COALESCE(orm.deleted, false) = false
					  AND ($4 = '' OR o.group_jid = $4)
					  AND COALESCE(rm.wa_message_id, '') <> '' AND COALESCE(orm.wa_message_id, '') <> ''
					  AND starts_with(rm.wa_message_id, orm.wa_message_id || '-fwd-')
				  )
				ORDER BY br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text), br.created_at
			) x
		) r ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS cnt, SUM(tx.amount) AS total,
			       MIN(tx.tx_date)::timestamptz AS first_op, MAX(tx.tx_date)::timestamptz AS last_op
			FROM transactions tx
			LEFT JOIN raw_messages rm ON rm.id = tx.raw_message_id
			WHERE tx.contact_id = c.id
			  AND tx.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  -- Дедуп «текст↔чек» как в сборе (countableTextCondition, с учётом
			  -- удалённого чека): текст-дубль распознанного чека в «всего» не
			  -- задваиваем. Наличка и тексты без парного чека считаются как обычно.
			  AND tx.dup_pending = false
			  AND (
			    tx.is_cash = true
			    OR NOT EXISTS (
			      SELECT 1 FROM bank_receipts brd
			      LEFT JOIN raw_messages brm ON brm.id = brd.raw_message_id
			      WHERE brd.contact_id = tx.contact_id
			        AND brd.amount = tx.amount
			        AND brd.needs_review = false
			        AND brd.is_duplicate = false AND brd.ignored = false
			        AND COALESCE(brm.deleted, false) = false
			        AND COALESCE(brm.received_at, brd.tx_date) >= COALESCE(rm.received_at, tx.tx_date::timestamptz) - interval '2 days'
			        AND COALESCE(brm.received_at, brd.tx_date) <  COALESCE(rm.received_at, tx.tx_date::timestamptz) + interval '2 days'
			        AND (COALESCE(brd.group_jid, '') = COALESCE(rm.wa_group_jid, '') OR $4 = '' OR brd.group_jid = $4)
			    )
			  )
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
// countableTextCondition — какой ТЕКСТОВЫЙ платёж считать в сбор. Считаем, если
// это наличка (is_cash) ИЛИ у него НЕТ парного распознанного чека (тот же
// контакт + та же сумма + та же группа за период). То есть каждый платёж
// считается ОДИН раз: если работник и чек прислал, и переписал текстом — берём
// чек, а текст-дубль отбрасываем; но если чек не распознался, платёж не
// теряется — учитывается текст. Это чинит и задвоение, и недосчёт.
const countableTextConditionTmpl = `(
		t.dup_pending = false
		AND (
			t.is_cash = true
			OR NOT EXISTS (
				SELECT 1 FROM bank_receipts brd
				-- LEFT (не INNER): у чеков из импорта/ручных raw_message_id может быть
				-- NULL — такой чек всё равно должен прятать свой текст-дубль.
				LEFT JOIN raw_messages brm ON brm.id = brd.raw_message_id
				WHERE brd.contact_id = t.contact_id
				  AND brd.amount = t.amount
				  AND brd.needs_review = false
				  AND brd.is_duplicate = false AND brd.ignored = false
				  -- Удалённый в WhatsApp чек из сбора уже исключён (ветка чеков),
				  -- поэтому он НЕ должен продолжать прятать свой текст-дубль — иначе
				  -- из сбора пропадал бы весь платёж (и чек, и текст).
				  AND COALESCE(brm.deleted, false) = false
				  -- Пару ищем по ВРЕМЕНИ ПРИСЫЛКИ (±2 дня): когда чек и его текст-дубль
				  -- ПОПАЛИ в чат, а не по дате операции на чеке. Иначе СТАРЫЙ чек
				  -- (операция недели назад), присланный сегодня вместе с сегодняшним
				  -- текстом, не спаривался — и оба считались (задвоение). Для чеков без
				  -- сообщения (импорт/ручные) запасной ориентир — дата операции.
				  AND COALESCE(brm.received_at, brd.tx_date) >= COALESCE(rm.received_at, t.tx_date::timestamptz) - interval '2 days'
				  AND COALESCE(brm.received_at, brd.tx_date) <  COALESCE(rm.received_at, t.tx_date::timestamptz) + interval '2 days'
				  {GROUPCLAUSE}
			)
		)
	)`

// groupPairClause — где искать чек-близнец текста: в той же группе ИЛИ в
// любой из выбранных групп (в сборе «по всем группам» текст в основной и чек в
// группе СБ — один платёж). gp — SQL-параметр со списком групп (NULL — все).
func groupPairClause(gp string) string {
	return "AND (COALESCE(brd.group_jid, '') = COALESCE(rm.wa_group_jid, '') OR " + gp + "::text[] IS NULL OR brd.group_jid = ANY(" + gp + "))"
}

// textCountable — countableTextCondition для запроса со списком групп в gp.
func textCountable(gp string) string {
	return strings.Replace(countableTextConditionTmpl, "{GROUPCLAUSE}", groupPairClause(gp), 1)
}

// textCountableByChat — то же для режима «по дате чата».
func textCountableByChat(gp string) string {
	return strings.Replace(countableTextConditionByChatTmpl, "{GROUPCLAUSE}", groupPairClause(gp), 1)
}

// countableTextConditionByChat — тот же дедуп «текст vs чек», но для режима ПО
// ДАТЕ ЧАТА: парный чек ищем по ЕГО дате присылки (brm.received_at в [$1,$2)),
// а НЕ по дате операции. Иначе поздно присланный чек (дата операции в другом
// дне/месяце) не находился бы, и его текст-дубль задваивал бы сумму. Так текст
// отбрасывается, если рядом в тот же период прислали парный чек — считается один
// раз (сам чек — в основной сумме, если его дата операции в месяце периода, или
// отдельной «⚠️ старые чеки» секцией, если нет). $1/$2 в обоих чат-режимах = from/to.
const countableTextConditionByChatTmpl = `(
		t.dup_pending = false
		AND (
			t.is_cash = true
			OR NOT EXISTS (
				SELECT 1 FROM bank_receipts brd
				LEFT JOIN raw_messages brm ON brm.id = brd.raw_message_id
				WHERE brd.contact_id = t.contact_id
				  AND brd.amount = t.amount
				  AND brd.needs_review = false
				  AND brd.is_duplicate = false AND brd.ignored = false
				  -- Удалённый чек не прячет свой текст-дубль (см. countableTextCondition).
				  AND COALESCE(brm.deleted, false) = false
				  -- ±2 дня относительно присылки текста (см. countableTextCondition).
				  AND brm.received_at >= rm.received_at - interval '2 days' AND brm.received_at < rm.received_at + interval '2 days'
				  {GROUPCLAUSE}
			)
		)
	)`

// fwdDedupNotExists отбрасывает ПЕРЕСЛАННУЮ копию чека (её wa_message_id вида
// «<оригинал>-fwd-<группа>»), если ОРИГИНАЛ присутствует и посчитан в той же
// выборке. Нужно потому, что пересланная копия распознаётся заново и может дать
// ДРУГОЙ номер документа/дату — тогда ключ DISTINCT ON расходится и обе копии
// выживают, задваивая сумму в сводке по всем группам. Это условие — В ДОПОЛНЕНИЕ
// к DISTINCT ON. Фильтр групп на оригинале ОБЯЗАТЕЛЕН ($3), иначе отчёт по одной
// группе зря прятал бы копию, найдя оригинал в другой. Требует в основном запросе
// алиасы br (чек) и rm (его raw_messages) и параметры $1/$2 (период tx_date), $3
// (группы) — как в SummaryForPeriod/CardTotals/SenderStats.
const fwdDedupNotExists = `AND NOT EXISTS (
			SELECT 1 FROM bank_receipts o
			JOIN raw_messages orm ON orm.id = o.raw_message_id
			WHERE o.id <> br.id
			  AND o.needs_review = false AND o.is_duplicate = false AND o.ignored = false
			  AND COALESCE(orm.deleted, false) = false
			  AND o.tx_date >= $1 AND o.tx_date < $2
			  AND ($3::text[] IS NULL OR o.group_jid = ANY($3))
			  AND COALESCE(rm.wa_message_id, '') <> '' AND COALESCE(orm.wa_message_id, '') <> ''
			  AND starts_with(rm.wa_message_id, orm.wa_message_id || '-fwd-')
		)`

func (d *DB) legacySummaryForPeriod(ctx context.Context, from, to time.Time, groupJIDs []string) ([]ContactSummary, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT c.canonical_name, t.card_to, t.amount
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		-- $1/$2 явно timestamptz: первое текстовое упоминание параметра задаёт его
		-- тип, а transactions.tx_date — DATE. Без каста время границ отбрасывалось,
		-- и ветка чеков (timestamptz) сравнивалась с полночью по TZ сервера.
		WHERE t.tx_date >= $1::timestamptz AND t.tx_date < $2::timestamptz
		  AND t.ignored = false
		  AND `+textCountable("$3")+`
		  AND COALESCE(rm.deleted, false) = false
		  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3)))

		UNION ALL

		(SELECT DISTINCT ON (COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
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
		  `+fwdDedupNotExists+`
		ORDER BY COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
	`, from, to, groupSliceArg(groupJIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanContactSummaries(rows)
}

// scanContactSummaries сворачивает строки (имя, карта, сумма) в разбивку по
// контактам с нормализацией имени. Общая для операционного и «чат»-режимов.
func scanContactSummaries(rows pgx.Rows) ([]ContactSummary, error) {
	byName := map[string]*ContactSummary{}
	var order []string
	for rows.Next() {
		var name string
		var card *string
		var amount float64
		if err := rows.Scan(&name, &card, &amount); err != nil {
			return nil, err
		}
		// Группируем по нормализованному ключу, чтобы «Хадаев Али» и «Хадаев
		// али» были одним человеком. Для показа берём первый вариант написания.
		key := nameKey(name)
		cs, ok := byName[key]
		if !ok {
			cs = &ContactSummary{CanonicalName: name, ByCard: map[string]float64{}}
			byName[key] = cs
			order = append(order, key)
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
	for _, key := range order {
		result = append(result, *byName[key])
	}
	return result, rows.Err()
}

// SummaryForPeriodByChat — сводка в режиме «по дате В ЧАТЕ» (когда прислали),
// а не по дате операции на чеке. Фильтр по received_at сообщения. Чеки, у
// которых дата операции ВНЕ месяца периода [monthStart, monthEnd), в основную
// сумму НЕ берём — их отдельно показывает ChecksPostedOldOperation. Наличка и
// текст датируются присылкой, поэтому попадают как обычно.
func (d *DB) legacySummaryForPeriodByChat(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string) ([]ContactSummary, error) {
	rows, err := d.pool.Query(ctx, `
		(SELECT c.canonical_name, t.card_to, t.amount
		FROM transactions t
		JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		WHERE rm.received_at >= $1 AND rm.received_at < $2
		  AND t.ignored = false
		  AND `+textCountableByChat("$3")+`
		  AND COALESCE(rm.deleted, false) = false
		  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3)))

		UNION ALL

		(SELECT DISTINCT ON (COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
			c.canonical_name, COALESCE(br.bank, 'банк не указан'), br.amount
		FROM bank_receipts br
		JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.received_at >= $1 AND rm.received_at < $2
		  AND br.tx_date >= $4 AND br.tx_date < $5
		  AND br.needs_review = false
		  AND br.is_duplicate = false
		  AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND br.contact_id IS NOT NULL
		  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
		ORDER BY COALESCE(br.contact_id::text, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
	`, from, to, groupSliceArg(groupJIDs), monthStart, monthEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanContactSummaries(rows)
}

// OldCheck — чек, ПРИСЛАННЫЙ в период, но с датой операции из ДРУГОГО месяца
// (в режиме «по дате чата» показывается отдельно, вне суммы дня).
type OldCheck struct {
	Name        string
	Amount      float64
	TxDate      time.Time
	SubmittedBy string
}

// ChecksPostedOldOperation — чеки, присланные (received_at) в [from,to), у
// которых дата операции вне месяца периода [monthStart, monthEnd). Для отдельной
// «⚠️ старые чеки» секции в режиме по дате чата.
func (d *DB) legacyChecksPostedOldOperation(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string, limit int) ([]OldCheck, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
			COALESCE(c.canonical_name, br.recipient_raw, ''), br.amount::float8, br.tx_date,
			COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), '')
		FROM bank_receipts br
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE rm.received_at >= $1 AND rm.received_at < $2
		  AND (br.tx_date < $4 OR br.tx_date >= $5)
		  AND br.needs_review = false AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false AND br.amount > 0
		  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
		ORDER BY COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text), br.tx_date
		LIMIT $6
	`, from, to, groupSliceArg(groupJIDs), monthStart, monthEnd, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OldCheck
	for rows.Next() {
		var o OldCheck
		if err := rows.Scan(&o.Name, &o.Amount, &o.TxDate, &o.SubmittedBy); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
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
func (d *DB) legacyCardTotals(ctx context.Context, from, to time.Time, groupJIDs []string) ([]CardTotal, error) {
	// Берём построчно и агрегируем в Go по нормализованному имени владельца
	// карты — чтобы варианты написания одного человека («Танзила Рахмановна С»
	// и «Танзила Рахмановна С.») не разбивались на две карты и не недосчитывали.
	// DISTINCT ON схлопывает один и тот же чек, разосланный в несколько групп
	// (работник кинул в СБ-группу, оттуда переслали в основную — это НЕ дубль,
	// см. FindDuplicateReceipt), чтобы карта получателя не задваивалась при
	// подсчёте по всем группам. Ключ: номер документа, а без него —
	// контакт/получатель + сумма + дата операции (как в SummaryForPeriod).
	rows, err := d.pool.Query(ctx, `
		SELECT card, bank, amount FROM (
			SELECT DISTINCT ON (COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
				COALESCE(NULLIF(br.card_owner, ''), NULLIF(br.recipient_raw, ''), 'не распознано') AS card,
				COALESCE(NULLIF(br.recipient_bank, ''), NULLIF(br.bank, ''), '') AS bank,
				br.amount::float8 AS amount
			FROM bank_receipts br
			LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
			WHERE br.tx_date >= $1 AND br.tx_date < $2
			  AND br.is_duplicate = false AND br.ignored = false
			  AND COALESCE(rm.deleted, false) = false
			  AND br.amount > 0
			  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
			  `+fwdDedupNotExists+`
			ORDER BY COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text)
		) dedup
	`, from, to, groupSliceArg(groupJIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byCard := map[string]*CardTotal{}
	var order []string
	for rows.Next() {
		var card, bank string
		var amount float64
		if err := rows.Scan(&card, &bank, &amount); err != nil {
			return nil, err
		}
		key := nameKey(card)
		c, ok := byCard[key]
		if !ok {
			c = &CardTotal{CardOwner: card, Bank: bank}
			byCard[key] = c
			order = append(order, key)
		}
		if c.Bank == "" && bank != "" {
			c.Bank = bank
		}
		c.Count++
		c.Total += amount
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]CardTotal, 0, len(order))
	for _, key := range order {
		out = append(out, *byCard[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out, nil
}

// SenderStats возвращает статистику по отправителям чеков за период [from, to):
// кто сколько чеков прислал и на какую сумму. Дубли не считаются.
// groupJID — фильтр по группе (пусто = все), senderQuery — фильтр по человеку:
// подстрока имени или номера телефона (пусто = все отправители).
func (d *DB) legacySenderStats(ctx context.Context, from, to time.Time, groupJIDs []string, senderQuery string) ([]SenderStat, error) {
	// "Сбор" по ответственному включает И чеки (bank_receipts), И наличку/
	// текстовые платежи (transactions) — всё, что человек собрал. Имя берём
	// в приоритете из памяти номеров (phone_owners по телефону отправителя),
	// затем из submitted_by/pushname.
	rows, err := d.pool.Query(ctx, `
		SELECT s.sender_name, s.phone, COUNT(*), COALESCE(SUM(s.amount), 0)
		FROM (
			-- DISTINCT ON схлопывает один и тот же чек, разосланный в несколько
			-- групп (как в SummaryForPeriod/CardTotals), чтобы сбор отправителя не
			-- задваивался при подсчёте по всем группам. Из копий берём самую
			-- раннюю (br.created_at) — обычно это тот, кто реально собрал и первым
			-- запостил, а не тот, кто переслал.
			SELECT sender_name, phone, amount FROM (
				SELECT DISTINCT ON (COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
					COALESCE(NULLIF(br.collector, ''), NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
					split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
					br.amount
				FROM bank_receipts br
				LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
				LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
				WHERE br.tx_date >= $1 AND br.tx_date < $2
				  AND br.is_duplicate = false
				  AND br.ignored = false
				  AND br.needs_review = false
				  AND br.contact_id IS NOT NULL
				  AND COALESCE(rm.deleted, false) = false
				  AND br.amount > 0
				  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
				  `+fwdDedupNotExists+`
				ORDER BY COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text), br.created_at
			) bank_dedup

			UNION ALL

			SELECT
				COALESCE(NULLIF(t.collector, ''), NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
				split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
				t.amount
			FROM transactions t
			JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
			WHERE t.tx_date >= $1 AND t.tx_date < $2
			  AND t.ignored = false
			  AND `+textCountable("$3")+`
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
	return scanSenderStats(rows)
}

func scanSenderStats(rows pgx.Rows) ([]SenderStat, error) {
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

// SenderStatsByChat — «сбор по отправителю» в режиме по дате В ЧАТЕ (received_at).
// Чеки с датой операции вне месяца периода [monthStart, monthEnd) в сбор не
// берём (их отдельно покажет ChecksPostedOldOperation). Наличка/текст —
// по дате присылки.
func (d *DB) legacySenderStatsByChat(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string, senderQuery string) ([]SenderStat, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT s.sender_name, s.phone, COUNT(*), COALESCE(SUM(s.amount), 0)
		FROM (
			SELECT sender_name, phone, amount FROM (
				SELECT DISTINCT ON (COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text))
					COALESCE(NULLIF(br.collector, ''), NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
					split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
					br.amount
				FROM bank_receipts br
				LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
				LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
				WHERE rm.received_at >= $1 AND rm.received_at < $2
				  AND br.tx_date >= $5 AND br.tx_date < $6
				  AND br.is_duplicate = false
				  AND br.ignored = false
				  AND br.needs_review = false
				  AND br.contact_id IS NOT NULL
				  AND COALESCE(rm.deleted, false) = false
				  AND br.amount > 0
				  AND ($3::text[] IS NULL OR br.group_jid = ANY($3))
				ORDER BY COALESCE(br.contact_id::text, br.recipient_raw, '') || '|' || br.amount::text || '|' || COALESCE(NULLIF(br.doc_number, ''), br.tx_date::text), br.created_at
			) bank_dedup

			UNION ALL

			SELECT
				COALESCE(NULLIF(t.collector, ''), NULLIF(po.name, ''), NULLIF(rm.sender_name, ''), '') AS sender_name,
				split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1) AS phone,
				t.amount
			FROM transactions t
			JOIN raw_messages rm ON rm.id = t.raw_message_id
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
			WHERE rm.received_at >= $1 AND rm.received_at < $2
			  AND t.ignored = false
			  AND `+textCountableByChat("$3")+`
			  AND COALESCE(rm.deleted, false) = false
			  AND t.amount > 0
			  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3))
		) s
		WHERE ($4 = '' OR s.sender_name ILIKE '%' || $4 || '%' OR s.phone LIKE '%' || $4 || '%')
		GROUP BY s.sender_name, s.phone
		ORDER BY 4 DESC
	`, from, to, groupSliceArg(groupJIDs), senderQuery, monthStart, monthEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSenderStats(rows)
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

// ---- Расходы ----

// Expense — одна запись расхода.
type Expense struct {
	ID        int
	Amount    float64
	Note      string
	CreatedBy string
	SpentAt   time.Time
}

// InsertExpense записывает расход. Возвращает id.
func (d *DB) InsertExpense(ctx context.Context, amount float64, note, createdBy string, spentAt time.Time) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO expenses (amount, note, created_by, spent_at)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, amount, nullIfEmpty(note), nullIfEmpty(createdBy), spentAt).Scan(&id)
	return id, err
}

// ExpensesForPeriod возвращает расходы за период [from, to) и их сумму.
func (d *DB) ExpensesForPeriod(ctx context.Context, from, to time.Time) ([]Expense, float64, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, amount::float8, COALESCE(note, ''), COALESCE(created_by, ''), spent_at
		FROM expenses
		WHERE spent_at >= $1 AND spent_at < $2
		ORDER BY spent_at
	`, from, to)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Expense
	var total float64
	for rows.Next() {
		var e Expense
		if err := rows.Scan(&e.ID, &e.Amount, &e.Note, &e.CreatedBy, &e.SpentAt); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
		total += e.Amount
	}
	return out, total, rows.Err()
}

// DeleteExpense удаляет расход по id (ручная отмена ошибочной записи).
func (d *DB) DeleteExpense(ctx context.Context, id int) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM expenses WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
