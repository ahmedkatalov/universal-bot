-- Схема базы данных для WhatsApp-бота учёта финансов.
-- Применяется один раз при разворачивании: psql -f schema.sql

CREATE TABLE IF NOT EXISTS contacts (
    id              SERIAL PRIMARY KEY,
    canonical_name  TEXT NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS contact_aliases (
    id          SERIAL PRIMARY KEY,
    contact_id  INT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    alias       TEXT NOT NULL UNIQUE
);

-- Сырые сообщения из группы — хранятся всегда, даже если парсер не справился.
-- Это позволяет переразобрать историю заново после исправления парсера.
CREATE TABLE IF NOT EXISTS raw_messages (
    id            SERIAL PRIMARY KEY,
    wa_message_id TEXT UNIQUE,        -- ID сообщения в WhatsApp, для идемпотентности
    wa_group_jid  TEXT NOT NULL,
    sender_jid    TEXT NOT NULL,
    sender_name   TEXT,
    body          TEXT,
    has_media     BOOLEAN NOT NULL DEFAULT false,
    media_path    TEXT,               -- путь к сохранённому фото чека, если есть
    received_at   TIMESTAMPTZ NOT NULL,
    parsed        BOOLEAN NOT NULL DEFAULT false,
    deleted       BOOLEAN NOT NULL DEFAULT false, -- сообщение удалили в WhatsApp; его платежи/чеки не считаются
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transactions (
    id              SERIAL PRIMARY KEY,
    contact_id      INT REFERENCES contacts(id),
    raw_name        TEXT NOT NULL,     -- имя ровно как было написано в сообщении
    amount          NUMERIC(12,2) NOT NULL,
    note            TEXT,              -- "аванс", "премия", "долг" и т.п.
    card_to         TEXT,              -- "втб", "карта (банк не указан)", "наличные"
    raw_message_id  INT REFERENCES raw_messages(id),
    tx_date         DATE NOT NULL,     -- дата операции (из сообщения или дата получения)
    ignored         BOOLEAN NOT NULL DEFAULT false, -- ручное исключение из учёта через ассистента
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_transactions_contact ON transactions(contact_id);
CREATE INDEX IF NOT EXISTS idx_transactions_date ON transactions(tx_date);
CREATE INDEX IF NOT EXISTS idx_raw_messages_parsed ON raw_messages(parsed);

-- Скриншоты банковских переводов (Альфа-Банк, ВТБ и т.д.), распознанные OCR.
-- Хранятся отдельно от transactions, т.к. содержат больше служебных полей
-- (код авторизации, номер документа) и своя логика проверки статуса.
CREATE TABLE IF NOT EXISTS bank_receipts (
    id              SERIAL PRIMARY KEY,
    raw_message_id  INT REFERENCES raw_messages(id),
    bank            TEXT,
    recipient_raw   TEXT,           -- ФИО получателя (чья карта) как распознано OCR
    sender_raw      TEXT,           -- ФИО отправителя (клиент, который платил)
    contact_id      INT REFERENCES contacts(id), -- сопоставлено через alias, если получилось
    amount          NUMERIC(12,2),
    commission      NUMERIC(12,2),
    doc_number      TEXT,
    auth_code       TEXT,
    status          TEXT,
    needs_review    BOOLEAN NOT NULL DEFAULT false, -- true если сумму/получателя не удалось распознать уверенно
    is_duplicate    BOOLEAN NOT NULL DEFAULT false, -- true если это повторно присланный чек (см. FindDuplicateReceipt)
    -- Группа, к которой относится чек. Обычно совпадает с raw_messages.wa_group_jid,
    -- но для чеков, дозагруженных владельцем через личку ("бота не было в группе
    -- 1-2 июля, вот пропущенные чеки"), здесь целевая группа, а не JID лички.
    group_jid       TEXT,
    -- Кто отправил чек. Для чеков из групп берётся из raw_messages (sender),
    -- здесь заполняется только при дозагрузке через личку, если владелец
    -- указал, от кого эти чеки (например, "это чеки Расула").
    submitted_by    TEXT,
    ignored         BOOLEAN NOT NULL DEFAULT false, -- ручное исключение из учёта через ассистента
    -- Дата И ВРЕМЯ операции (не время получения сообщения в WhatsApp) —
    -- берётся с самого чека, когда OCR смог его распознать; иначе время
    -- получения сообщения используется как приближение. Точное время нужно
    -- и для отчётов за конкретный день, и для детекта дублей чеков.
    tx_date         TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_bank_receipts_contact ON bank_receipts(contact_id);
CREATE INDEX IF NOT EXISTS idx_bank_receipts_review ON bank_receipts(needs_review);
CREATE INDEX IF NOT EXISTS idx_bank_receipts_duplicate ON bank_receipts(is_duplicate);
CREATE INDEX IF NOT EXISTS idx_bank_receipts_group ON bank_receipts(group_jid);

-- Стартовый набор контактов и алиасов на основе твоих реальных сообщений.
INSERT INTO contacts (canonical_name) VALUES
    ('Наличка'), ('Ахмед'), ('Милана'), ('Яхита'), ('Нажуд'),
    ('Пияна'), ('Сафаи'), ('Хадижат')
ON CONFLICT (canonical_name) DO NOTHING;

INSERT INTO contact_aliases (contact_id, alias)
SELECT id, 'нал' FROM contacts WHERE canonical_name = 'Наличка'
ON CONFLICT (alias) DO NOTHING;

INSERT INTO contact_aliases (contact_id, alias)
SELECT id, 'пиян' FROM contacts WHERE canonical_name = 'Пияна'
ON CONFLICT (alias) DO NOTHING;

INSERT INTO contact_aliases (contact_id, alias)
SELECT id, 'хадижа' FROM contacts WHERE canonical_name = 'Хадижат'
ON CONFLICT (alias) DO NOTHING;
