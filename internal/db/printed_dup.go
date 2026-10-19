package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Один и тот же чек БЕЗ номера операции, присланный дважды (сначала без ФИО,
// потом с ФИО — «забыл подписать»), узнаём по тому, что НАПЕЧАТАНО на чеке, а
// не по тому, на кого его записали: у неподписанной копии клиент — владелец
// карты с чека, у подписанной — названный клиент, и прежняя проверка «тот же
// контакт» их не связывала. Печатные поля: получатель (как напечатан), сумма,
// ТОЧНОЕ время операции, плательщик и банк (если прочитаны у обоих). Разное
// время, другой плательщик или другой получатель — разные чеки, не дубль.

// PrintedReceipt — поля чека, как они напечатаны (до подмены получателя на клиента).
type PrintedReceipt struct {
	Recipient string // получатель с чека (владелец карты)
	Sender    string // плательщик с чека
	Bank      string
	DocNumber string
	AuthCode  string
	Amount    float64
	TxDate    time.Time // время операции С ЧЕКА
}

// PrintedTwin — живой чек группы с теми же печатными полями.
type PrintedTwin struct {
	Found      bool
	ID         int
	Named      bool // клиент назван (подпись/ответ), а не просто владелец карты
	ContactID  *int
	Client     string // на кого записан
	TxDate     time.Time
	WaMessage  string
	ReceivedAt time.Time
	Asked      bool // по нему уже спрашивали «чей чек?»
}

// MinutePrecision — время операции на чеке без секунд (ВТБ печатает только часы
// и минуты): два разных перевода в одну минуту с одной карты на одну карту
// бывают (родственник платит за двоих подряд), поэтому «тот же чек» по такому
// времени молча не решаем — спрашиваем человека.
func MinutePrecision(t time.Time) bool { return t.Second() == 0 && t.Nanosecond() == 0 }

// FutureOpSlack — насколько напечатанное время операции может быть позже
// присылки самого чека (часы телефона и банка расходятся). Позже — ошибка
// чтения или другой часовой пояс: по такому времени «тот же перевод или
// другой» не судим.
const FutureOpSlack = 10 * time.Minute

// PlausibleOpTime — напечатанное время операции op не позже, чем чек прислали
// (posted), с допуском FutureOpSlack.
func PlausibleOpTime(op, posted time.Time) bool { return !op.After(posted.Add(FutureOpSlack)) }

// implausibleOpSQL — SQL-условие «время операции tx чека позже его присылки
// posted (больше FutureOpSlack)»: по такому времени не судим (см. PlausibleOpTime).
func implausibleOpSQL(tx, posted string) string {
	return fmt.Sprintf(`(%s > %s + interval '%d seconds')`, tx, posted, int(FutureOpSlack/time.Second))
}

// sameOpTimeSQL — время операции двух чеков (a и b; *Guessed — время не с
// чека, а по сообщению) не мешает считать их одним и тем же чеком. Если оба
// времени НАПЕЧАТАНЫ на чеках, то копия того же чека печатает то же время: до
// секунды, а если хоть один чек печатает только минуты (ВТБ, РСХБ) — та же
// минута. Разное напечатанное время — разные операции (крупный платёж, разбитый
// по лимиту банка на переводы с разницей в минуты). Окно DuplicateWindow — только
// когда время хоть одного чека угадано.
func sameOpTimeSQL(aTx, aGuessed, bTx, bGuessed string) string {
	return fmt.Sprintf(`(CASE WHEN %[2]s OR %[4]s
		THEN abs(extract(epoch FROM %[1]s - %[3]s)) <= %[5]d
		ELSE (%[1]s = %[3]s
		      OR ((extract(second FROM %[1]s) = 0 OR extract(second FROM %[3]s) = 0)
		          AND date_trunc('minute', %[1]s) = date_trunc('minute', %[3]s))) END)`,
		aTx, aGuessed, bTx, bGuessed, int(DuplicateWindow/time.Second))
}

// sameStrongIDSQL — у чеков a и b общий номер операции или код авторизации
// (NULL-безопасно: номер хранится NULL, если его нет на чеке).
func sameStrongIDSQL(aDoc, aAuth, bDoc, bAuth string) string {
	return fmt.Sprintf(`COALESCE((NULLIF(%[1]s, '') IS NOT NULL AND %[1]s = %[3]s)
		OR (NULLIF(%[2]s, '') IS NOT NULL AND %[2]s = %[4]s), false)`, aDoc, aAuth, bDoc, bAuth)
}

// printedNorm — сравнение печатных строк без пробелов, знаков и «ё».
func printedNorm(expr string) string {
	return `regexp_replace(lower(translate(COALESCE(` + expr + `, ''), 'ёЁ', 'еЕ')), '[[:space:][:punct:]]+', '', 'g')`
}

// printedRecipientOf — получатель, как напечатан на чеке br: у чека с
// названным клиентом он сохранён в card_owner, у неназванного — в recipient_raw.
func printedRecipientOf(br string) string {
	return `CASE WHEN ` + br + `.client_confirmed THEN COALESCE(` + br + `.card_owner, '')
	        ELSE COALESCE(NULLIF(` + br + `.card_owner, ''), ` + br + `.recipient_raw, '') END`
}

// printedMatchSQL — условие «чеки a и b — один и тот же напечатанный чек».
func printedMatchSQL(a, b string) string {
	ra, rb := printedNorm(printedRecipientOf(a)), printedNorm(printedRecipientOf(b))
	sa, sb := printedNorm(a+".sender_raw"), printedNorm(b+".sender_raw")
	ba, bb := printedNorm(a+".bank"), printedNorm(b+".bank")
	return fmt.Sprintf(`(%[1]s.amount > 0 AND %[1]s.amount = %[2]s.amount
		AND %[1]s.tx_date = %[2]s.tx_date
		AND %[3]s <> '' AND %[3]s = %[4]s
		AND (COALESCE(%[1]s.doc_number, '') = '' OR COALESCE(%[2]s.doc_number, '') = '' OR %[1]s.doc_number = %[2]s.doc_number)
		AND (COALESCE(%[1]s.auth_code, '') = '' OR COALESCE(%[2]s.auth_code, '') = '' OR %[1]s.auth_code = %[2]s.auth_code)
		AND (%[5]s = '' OR %[6]s = '' OR %[5]s = %[6]s)
		AND (%[7]s = '' OR %[8]s = '' OR %[7]s = %[8]s))`, a, b, ra, rb, sa, sb, ba, bb)
}

// FindPrintedTwin ищет в группе живой чек, напечатанный так же, как p (тот же
// чек, присланный ещё раз). excludeID — сам проверяемый чек (0 — новый).
// Копии, пересланные ботом из других групп, не трогаем: они повторяют свой
// исходный чек.
func (d *DB) FindPrintedTwin(ctx context.Context, groupJID string, excludeID int, p PrintedReceipt) (PrintedTwin, error) {
	var t PrintedTwin
	if groupJID == "" || p.Amount <= 0 || p.Recipient == "" {
		return t, nil
	}
	err := d.pool.QueryRow(ctx, `
		WITH p AS (
			SELECT $3::numeric AS amount, $4::timestamptz AS tx_date, true AS client_confirmed,
			       $5::text AS card_owner, $5::text AS recipient_raw, $6::text AS sender_raw, $7::text AS bank,
			       $8::text AS doc_number, $9::text AS auth_code
		)
		SELECT br.id, (br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, ''), br.tx_date,
		       COALESCE(rm.wa_message_id, ''), rm.received_at, br.clarify_asked
		FROM p, bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1
		  AND br.id <> $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND `+printedMatchSQL("br", "p")+`
		ORDER BY rm.received_at, br.id
		LIMIT 1
	`, groupJID, excludeID, p.Amount, p.TxDate, p.Recipient, p.Sender, p.Bank, p.DocNumber, p.AuthCode).
		Scan(&t.ID, &t.Named, &t.ContactID, &t.Client, &t.TxDate, &t.WaMessage, &t.ReceivedAt, &t.Asked)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.Found = true
	return t, nil
}

// MarkReceiptDuplicate помечает чек повтором (если он ещё живой). Чек, который
// ждал вопроса «тот же перевод?» в «НЕ вошло» (same_check_parked), ждать
// перестаёт: «на проверку» его отправила только эта догадка.
func (d *DB) MarkReceiptDuplicate(ctx context.Context, id int) (bool, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET is_duplicate = true,
		needs_review = CASE WHEN same_check_parked THEN false ELSE needs_review END, same_check_parked = false
		WHERE id = $1 AND is_duplicate = false`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
