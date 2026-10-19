package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Чек становится «полным» или «чьим» не только при приёме: автоповтор
// распознавания дочитывает сумму (ИИ лежал), работник отвечает суммой или ФИО
// на вопрос бота, владелец перепривязывает чек. Проверка «этот чек уже
// присылали» при приёме такой чек не поймала (тогда у него не было суммы,
// номера, клиента) — повторяем её в момент, когда он стал полным, иначе один
// платёж засчитался бы дважды.

// SameCheck — что нашла DedupeCompletedReceipt.
type SameCheck struct {
	Found        bool
	VictimIsThis bool      // повтором стал сам проверяемый чек (иначе — найденная копия)
	VictimWa     string    // сообщение чека, помеченного повтором
	KeptWa       string    // сообщение чека, оставшегося в учёте
	Client       string    // на кого записан оставшийся в учёте
	Amount       float64   // сумма
	TxDate       time.Time // время операции оставшегося
	Strong       bool      // совпал номер операции / код авторизации
	Tentative    bool      // уверенности нет — спросить «тот же чек или другой?»
	ThisClient   string    // клиент проверяемого чека (если назван)
	OtherClient  bool      // тот же номер операции, но названы РАЗНЫЕ клиенты
	// VictimCounted — повтором стал ДРУГОЙ, уже засчитанный чек (экран без
	// времени операции), а в учёте остался проверяемый полный чек: сказать об
	// этом и спросить «тот же или другой?» про тот чек.
	VictimCounted bool
}

// completedReceipt — проверяемый чек.
type completedReceipt struct {
	groupJID, sender, wa string
	rawID                int
	received             time.Time
	live, named          bool
	contactID            *int
	client               string
	p                    PrintedReceipt
	guessed, collapsed   bool
	denied               bool // человек уже ответил «другой» на «похоже, тот же чек»
}

func (d *DB) loadCompletedReceipt(ctx context.Context, id int) (completedReceipt, bool, error) {
	var r completedReceipt
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''), COALESCE(rm.wa_message_id, ''),
		       br.raw_message_id, rm.received_at,
		       (br.is_duplicate = false AND br.ignored = false AND COALESCE(rm.deleted, false) = false),
		       (br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, ''),
		       `+printedRecipientOf("br")+`, COALESCE(br.sender_raw, ''), COALESCE(br.bank, ''),
		       COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''), COALESCE(br.amount, 0)::float8, br.tx_date,
		       br.tx_time_guessed, br.collapsed, br.same_check_denied
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.id = $1`, id).
		Scan(&r.groupJID, &r.sender, &r.wa, &r.rawID, &r.received, &r.live, &r.named, &r.contactID, &r.client,
			&r.p.Recipient, &r.p.Sender, &r.p.Bank, &r.p.DocNumber, &r.p.AuthCode, &r.p.Amount, &r.p.TxDate,
			&r.guessed, &r.collapsed, &r.denied)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// DedupeCompletedReceipt — чек receiptID только что стал полным (amountFilled:
// сумму дочитал автоповтор или назвал человек) или «чьим» (назвали клиента).
// Если в группе уже живёт тот же чек — платёж один:
//   - тот же номер операции / код авторизации, или тот же клиент, сумма и то же
//     напечатанное время операции (как при приёме чека, см. sameOpTimeSQL; без
//     общего номера — tentative: это мог быть и второй перевод);
//   - тот же НАПЕЧАТАННЫЙ чек без номера (получатель, плательщик, сумма, время;
//     при времени до минуты — tentative);
//   - (tentative) у этого чека время операции не прочиталось, а в группе уже
//     засчитан чек того же клиента на ту же сумму той же карте, сделанный не
//     позже, чем прислали этот: скорее всего, его прислали ещё раз чётче. Это
//     не наверняка — чек помечается повтором, а вызывающий спрашивает в группе
//     «тот же или другой?»; без allowTentative (спросить некому) это слабое
//     совпадение не ищем;
//   - (tentative, обратный порядок) этот чек — полный, с напечатанным временем
//     операции, а в группе уже засчитан чек того же клиента на ту же сумму без
//     времени и номера (экран «Перевод выполнен»), присланный не раньше, чем
//     сделана операция этого (см. dedupeScreenOfCheck). Повтором становится
//     экран: в учёте остаётся полный чек (дата операции с чека), а вызывающий
//     спрашивает «тот же или другой?» про экран (VictimCounted).
//
// Повтором становится тот, что ещё не был в сборе: сам проверяемый чек, а если
// он назван, а копия ничья, — ничья копия. Копии, названные РАЗНЫМИ клиентами,
// без общего номера операции не склеиваем.
func (d *DB) DedupeCompletedReceipt(ctx context.Context, receiptID int, amountFilled, allowTentative bool) (SameCheck, error) {
	return d.dedupeCompletedReceipt(ctx, receiptID, amountFilled, allowTentative, false)
}

// dedupeCompletedReceipt — DedupeCompletedReceipt; screenOnly — только шаг 4.
func (d *DB) dedupeCompletedReceipt(ctx context.Context, receiptID int, amountFilled, allowTentative, screenOnly bool) (SameCheck, error) {
	var res SameCheck
	r, ok, err := d.loadCompletedReceipt(ctx, receiptID)
	if err != nil || !ok {
		return res, err
	}
	// Свёрнутый экран тоже проверяем: его назвали (ответ ФИО на «пришёл
	// НЕПОЛНЫМ»), а полный чек того же перевода уже засчитан — шаг 3 ниже.
	if !r.live || r.p.Amount <= 0 || r.sender == "bot-forward" || r.groupJID == "" {
		return res, nil
	}
	if r.named {
		res.ThisClient = r.client
	}
	mark := func(victim, kept int, victimIsThis bool, keptWa, victimWa, client string, tx time.Time) (SameCheck, error) {
		ok, err := d.MarkReceiptDuplicate(ctx, victim)
		if err != nil || !ok {
			return SameCheck{}, err
		}
		// Повтором стал свёрнутый экран — запоминаем, каким чеком он учтён (ответ
		// ФИО другого клиента по экрану вернёт его в учёт, см. DuplicateOriginalOf).
		if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts v SET superseded_by_raw = k.raw_message_id
			FROM bank_receipts k WHERE v.id = $1 AND k.id = $2 AND v.collapsed`, victim, kept); err != nil {
			return SameCheck{}, err
		}
		res.Found, res.VictimIsThis = true, victimIsThis
		res.KeptWa, res.VictimWa = keptWa, victimWa
		res.Client, res.Amount, res.TxDate = client, r.p.Amount, tx
		if victimIsThis {
			fmt.Printf("Чек #%d (%s): тот же чек уже учтён (%s) — этот не считаю повторно\n", receiptID, r.wa, keptWa)
		} else {
			fmt.Printf("Чек #%d (%s): тот же чек без клиента (%s) — его считаю повтором\n", receiptID, r.wa, victimWa)
		}
		return res, nil
	}

	if screenOnly {
		if !allowTentative {
			return res, nil
		}
		return d.dedupeScreenOfCheck(ctx, receiptID, r, res, mark)
	}

	// 1. Номер операции / код авторизации; тот же клиент и время (как при приёме:
	// напечатанное время у обоих — то же самое, см. sameOpTimeSQL).
	heur := amountFilled && !r.guessed
	var (
		oID             int
		oWa, oClient    string
		oNamed, oStrong bool
		oContact        *int
		oTx             time.Time
	)
	err = d.pool.QueryRow(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), (br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, ''), br.tx_date,
		       COALESCE(($3 <> '' AND br.doc_number = $3) OR ($4 <> '' AND br.auth_code = $4), false) AS strong
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND br.id <> $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND br.amount = $5::numeric
		  AND (
		    ($3 <> '' AND br.doc_number = $3)
		    OR ($4 <> '' AND br.auth_code = $4
		        AND br.tx_date BETWEEN $6::timestamptz - interval '3 days' AND $6::timestamptz + interval '3 days')
		    OR ($7 AND $8::int IS NOT NULL AND br.contact_id = $8
		        AND br.tx_date BETWEEN $6::timestamptz - interval '5 minutes' AND $6::timestamptz + interval '5 minutes'
		        AND `+sameOpTimeSQL("br.tx_date", "br.tx_time_guessed", "$6::timestamptz", "false")+`
		        AND (COALESCE(br.doc_number, '') = '' OR $3 = '' OR br.doc_number = $3)
		        AND (COALESCE(br.auth_code, '') = '' OR $4 = '' OR br.auth_code = $4)
		        AND (COALESCE(br.sender_raw, '') = '' OR $9 = ''
		             OR lower(split_part(trim(br.sender_raw), ' ', 1)) = lower(split_part(trim($9), ' ', 1))))
		  )
		ORDER BY 7 DESC, rm.received_at, br.id
		LIMIT 1`, r.groupJID, receiptID, r.p.DocNumber, r.p.AuthCode, r.p.Amount, r.p.TxDate, heur, r.contactID, r.p.Sender).
		Scan(&oID, &oWa, &oNamed, &oContact, &oClient, &oTx, &oStrong)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return res, err
	default:
		res.Strong = oStrong
		// Без общего номера совпали клиент, сумма и время — это может быть и
		// второй перевод: как при приёме, говорим «похоже» и ждём «другой».
		res.Tentative = !oStrong
		if r.named && !oNamed {
			// Этот назван, а копия ничья (ждёт «чей чек?») — повтором становится она.
			return mark(oID, receiptID, false, r.wa, oWa, r.client, r.p.TxDate)
		}
		res.OtherClient = r.named && oNamed && (oContact == nil || r.contactID == nil || *oContact != *r.contactID)
		return mark(receiptID, oID, true, oWa, r.wa, oClient, oTx)
	}

	// 2. Тот же напечатанный чек без номера (только у названного: ничьей копии
	// клиента назовут — тогда и сверим).
	if r.named && !r.guessed && r.p.Recipient != "" {
		tw, err := d.FindPrintedTwin(ctx, r.groupJID, receiptID, r.p)
		if err != nil {
			return res, err
		}
		if tw.Found {
			switch {
			case !tw.Named && !MinutePrecision(r.p.TxDate):
				return mark(tw.ID, receiptID, false, r.wa, tw.WaMessage, r.client, r.p.TxDate)
			case tw.Named && tw.ContactID != nil && r.contactID != nil && *tw.ContactID == *r.contactID:
				// Время до минуты — второй перевод тому же клиенту в ту же минуту
				// маловероятен, но возможен: вызывающий спросит «тот же или другой?».
				res.Tentative = MinutePrecision(r.p.TxDate)
				return mark(receiptID, tw.ID, true, tw.WaMessage, r.wa, tw.Client, tw.TxDate)
			}
			// Ничья копия при времени до минуты — решит её «чей чек?»; названные
			// разными клиентами — не склеиваем.
		}
	}

	// 3. Время операции у этого чека не прочиталось (и номера нет) — тот же
	// клиент, сумма, карта, и чек сделан не позже, чем прислали этот. И когда
	// дочитали сумму, и когда назвали клиента (экран «Перевод выполнен» без
	// даты, названный в ответ на вопрос бота, при уже засчитанном полном чеке).
	// Человек уже сказал про этот чек «другой» — это слабое совпадение не ищем.
	if allowTentative && !r.denied && r.named && r.guessed && r.p.DocNumber == "" && r.p.AuthCode == "" {
		ownerNorm := printedNorm("$6::text")
		senderNorm := printedNorm("$7::text")
		err = d.pool.QueryRow(ctx, `
			SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(c.canonical_name, br.recipient_raw, ''), br.tx_date
			FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN contacts c ON c.id = br.contact_id
			WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND br.id <> $2
			  AND br.is_duplicate = false AND br.ignored = false AND br.needs_review = false
			  AND br.collapsed = false
			  AND COALESCE(rm.deleted, false) = false
			  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
			  AND br.client_confirmed AND br.contact_id = $3
			  AND br.amount = $4::numeric
			  -- Время операции позже присылки самого чека — ошибка чтения (или
			  -- другой часовой пояс): по нему не судим.
			  AND (`+implausibleOpSQL("br.tx_date", "rm.received_at")+` OR
			       (br.tx_date <= $5::timestamptz + interval '2 minutes'
			        AND br.tx_date >= $5::timestamptz - interval '3 days'))
			  AND rm.received_at BETWEEN $5::timestamptz - interval '1 day' AND $5::timestamptz + interval '1 day'
			  AND (`+ownerNorm+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = `+ownerNorm+`)
			  AND (`+senderNorm+` = '' OR `+printedNorm("br.sender_raw")+` = '' OR `+printedNorm("br.sender_raw")+` = `+senderNorm+`)
			ORDER BY abs(extract(epoch FROM rm.received_at - $5::timestamptz)), br.id
			LIMIT 1`, r.groupJID, receiptID, r.contactID, r.p.Amount, r.received, r.p.Recipient, r.p.Sender).
			Scan(&oID, &oWa, &oClient, &oTx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return res, err
		default:
			res.Tentative = true
			return mark(receiptID, oID, true, oWa, r.wa, oClient, oTx)
		}
	}
	// 4. Обратный порядок к шагу 3: этот чек — полный, а экран того же перевода
	// без времени уже засчитан.
	if allowTentative {
		return d.dedupeScreenOfCheck(ctx, receiptID, r, res, mark)
	}
	return res, nil
}

// DedupeScreenOfCountedCheck — только шаг 4 DedupeCompletedReceipt: полный чек
// receiptID (с напечатанным временем операции) только что засчитан клиенту при
// приёме (подпись), а экран того же перевода без времени уже в сборе. Шаги 1–3
// при приёме уже сделала проверка «этот чек уже присылали». Вызывать, только
// когда в группе можно спросить «тот же или другой?».
func (d *DB) DedupeScreenOfCountedCheck(ctx context.Context, receiptID int) (SameCheck, error) {
	return d.dedupeCompletedReceipt(ctx, receiptID, false, true, true)
}

// dedupeScreenOfCheck — шаг 4: чек r назван, время операции НАПЕЧАТАНО (и ему
// можно верить — не позже, чем чек прислали), а в группе уже засчитан чек того
// же клиента на ту же сумму без времени операции и номера (экран «Перевод
// выполнен» с подписью или названный в ответ), присланный не раньше, чем
// сделана операция r (2 минуты на разницу часов), и в пределах суток от r.
// Экран показывает уже сделанный перевод — скорее всего, тот же; но это не
// наверняка (клиент мог заплатить ту же сумму ещё раз): экран становится
// повтором (superseded_by_raw — этот чек), вызывающий спрашивает «тот же или
// другой?» (ответ «другой» возвращает экран). Экран, про который уже ответили
// «другой», и чек, уже сопоставленный с другим экраном, не трогаем. Из
// нескольких экранов — сначала от того же работника, затем ближайший по
// времени к операции.
func (d *DB) dedupeScreenOfCheck(ctx context.Context, receiptID int, r completedReceipt, res SameCheck,
	mark func(victim, kept int, victimIsThis bool, keptWa, victimWa, client string, tx time.Time) (SameCheck, error)) (SameCheck, error) {
	if r.denied || !r.named || r.guessed || r.collapsed || r.contactID == nil || r.p.Amount <= 0 {
		return res, nil
	}
	// Время операции позже присылки самого чека — ошибка чтения: «экран
	// прислан не раньше операции» по нему не проверяем.
	var op *time.Time
	if PlausibleOpTime(r.p.TxDate, r.received) {
		t := r.p.TxDate
		op = &t
	}
	ownerNorm := printedNorm("$6::text")
	senderNorm := printedNorm("$7::text")
	var (
		sID        int
		sWa        string
		sCollapsed bool
	)
	err := d.pool.QueryRow(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), br.collapsed
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND br.id <> $2
		  AND br.is_duplicate = false AND br.ignored = false AND br.needs_review = false
		  AND br.tx_time_guessed
		  AND COALESCE(br.doc_number, '') = '' AND COALESCE(br.auth_code, '') = ''
		  AND NOT br.same_check_denied
		  AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND br.client_confirmed AND br.contact_id = $3
		  AND br.amount = $4::numeric
		  AND rm.received_at BETWEEN $5::timestamptz - interval '1 day' AND $5::timestamptz + interval '1 day'
		  AND ($8::timestamptz IS NULL OR
		       (rm.received_at >= $8::timestamptz - interval '2 minutes'
		        AND rm.received_at <= $8::timestamptz + interval '3 days'))
		  AND (`+ownerNorm+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = `+ownerNorm+`)
		  AND (`+senderNorm+` = '' OR `+printedNorm("br.sender_raw")+` = '' OR `+printedNorm("br.sender_raw")+` = `+senderNorm+`)
		  AND NOT EXISTS (
			SELECT 1 FROM bank_receipts s
			WHERE s.superseded_by_raw = $9 AND s.is_duplicate AND NOT s.ignored)
		ORDER BY (COALESCE(rm.sender_jid, '') <> $10), abs(extract(epoch FROM rm.received_at - COALESCE($8::timestamptz, $5::timestamptz))), br.id
		LIMIT 1`, r.groupJID, receiptID, r.contactID, r.p.Amount, r.received, r.p.Recipient, r.p.Sender, op, r.rawID, r.sender).
		Scan(&sID, &sWa, &sCollapsed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return res, nil
	case err != nil:
		return res, err
	}
	out, err := mark(sID, receiptID, false, r.wa, sWa, r.client, r.p.TxDate)
	if err != nil || !out.Found {
		return out, err
	}
	out.Tentative, out.VictimCounted = true, true
	// Экран с подписью не свёрнутый — mark связь не запомнил: каким чеком он
	// учтён, нужно и ему (ответ ФИО другого клиента, удаление полного чека).
	if !sCollapsed {
		if _, err := d.pool.Exec(ctx, `UPDATE bank_receipts SET superseded_by_raw = $2 WHERE id = $1 AND is_duplicate`, sID, r.rawID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// UndoReceiptDuplicate — человек сказал, что чек сообщения waID не повтор, а
// отдельный платёж: снова учитываем его (и запоминаем ответ — «тот же?» по
// слабому совпадению больше не спрашиваем). needsReview — клиент у него ещё не
// назван (спросить «чей чек?»).
func (d *DB) UndoReceiptDuplicate(ctx context.Context, waID string) (found bool, amount float64, client string, needsReview bool, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts br SET is_duplicate = false, superseded_by_raw = NULL, same_check_denied = true
		WHERE br.id = (
			SELECT b2.id FROM bank_receipts b2 JOIN raw_messages rm ON rm.id = b2.raw_message_id
			WHERE rm.wa_message_id = $1 AND COALESCE(rm.deleted, false) = false
			ORDER BY b2.id DESC LIMIT 1
		) AND br.is_duplicate = true AND br.ignored = false
		RETURNING COALESCE(br.amount, 0)::float8,
		          COALESCE((SELECT canonical_name FROM contacts WHERE id = br.contact_id), br.recipient_raw, ''),
		          br.needs_review OR NOT (br.client_confirmed AND br.contact_id IS NOT NULL)`, waID).
		Scan(&amount, &client, &needsReview)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", false, nil
	}
	if err != nil {
		return false, 0, "", false, err
	}
	if needsReview {
		// Клиента не назвали — пусть клариф-цикл спросит «чей чек?».
		_, _ = d.pool.Exec(ctx, `UPDATE bank_receipts br SET needs_review = true, clarify_asked = false
			FROM raw_messages rm WHERE rm.id = br.raw_message_id AND rm.wa_message_id = $1 AND br.is_duplicate = false`, waID)
	}
	return true, amount, client, needsReview, nil
}

// MarkSameCheckAsk — бот спросил «похоже, это тот же чек/перевод… (или
// «другой»)» про чек сообщения checkWa: ответ «другой» найдёт чек и после
// перезапуска бота (SameCheckByAskMsg).
func (d *DB) MarkSameCheckAsk(ctx context.Context, checkWa, askMsgID string) error {
	if checkWa == "" || askMsgID == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts br SET same_check_ask_msg_id = $2
		WHERE br.id = (
			SELECT b2.id FROM bank_receipts b2 JOIN raw_messages rm ON rm.id = b2.raw_message_id
			WHERE rm.wa_message_id = $1 AND COALESCE(rm.deleted, false) = false
			ORDER BY b2.id DESC LIMIT 1
		)`, checkWa, askMsgID)
	return err
}

// SameCheckByAskMsg — по id вопроса «похоже, это тот же чек» — сообщение чека,
// который всё ещё считается повтором (ответ «другой» вернёт его в учёт).
func (d *DB) SameCheckByAskMsg(ctx context.Context, askMsgID string) (string, bool, error) {
	if askMsgID == "" {
		return "", false, nil
	}
	var wa string
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(rm.wa_message_id, '')
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.same_check_ask_msg_id = $1 AND br.is_duplicate AND NOT br.ignored
		  AND COALESCE(rm.deleted, false) = false
		ORDER BY br.id DESC LIMIT 1`, askMsgID).Scan(&wa)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return wa, wa != "", nil
}

// DupOriginal — чек-повтор и живой чек, повтором которого он считается.
type DupOriginal struct {
	Found         bool
	KeptWa        string
	KeptNamed     bool
	KeptContactID *int
	KeptClient    string
	Amount        float64
	TxDate        time.Time
	Strong        bool // общий номер операции / код авторизации
}

// DuplicateOriginalOf — чек сообщения waID помечен повтором (сам при приёме или
// позже, когда пришла его подписанная копия): какой живой чек остался в учёте.
// У свёрнутого экрана «Перевод выполнен» — полный чек, которым он учтён
// (superseded_by_raw). Found=false — чек не повтор или оригинал не найден.
func (d *DB) DuplicateOriginalOf(ctx context.Context, waID string) (DupOriginal, error) {
	var o DupOriginal
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(orm.wa_message_id, ''), (o.client_confirmed AND o.contact_id IS NOT NULL), o.contact_id,
		       COALESCE(c.canonical_name, o.recipient_raw, ''), COALESCE(o.amount, 0)::float8, o.tx_date,
		       `+sameStrongIDSQL("dd.doc_number", "dd.auth_code", "o.doc_number", "o.auth_code")+` AS strong
		FROM bank_receipts dd
		JOIN raw_messages drm ON drm.id = dd.raw_message_id
		JOIN bank_receipts o ON o.id <> dd.id AND o.is_duplicate = false AND o.ignored = false
			AND COALESCE(o.group_jid, '') = COALESCE(dd.group_jid, '')
			AND (
				-- Экран (свёрнутый или подписанный без времени): каким полным
				-- чеком он учтён.
				(dd.superseded_by_raw IS NOT NULL AND o.raw_message_id = dd.superseded_by_raw) OR
				(dd.superseded_by_raw IS NULL AND NOT dd.collapsed AND (
				(NULLIF(dd.doc_number, '') IS NOT NULL AND o.doc_number = dd.doc_number) OR
				(NULLIF(dd.auth_code, '') IS NOT NULL AND o.auth_code = dd.auth_code) OR
				(o.contact_id = dd.contact_id AND o.amount = dd.amount
				 AND `+sameOpTimeSQL("o.tx_date", "o.tx_time_guessed", "dd.tx_date", "dd.tx_time_guessed")+`) OR
				`+printedMatchSQL("o", "dd")+` OR
				-- Время с чека не прочиталось: тот же клиент и сумма, чек сделан не
				-- позже, чем прислали этот (см. DedupeCompletedReceipt).
				-- Время операции оставшегося позже его присылки — по нему не судим.
				(dd.tx_time_guessed AND dd.client_confirmed AND o.client_confirmed
				 AND o.contact_id = dd.contact_id AND o.amount = dd.amount
				 AND (`+implausibleOpSQL("o.tx_date", "(SELECT received_at FROM raw_messages WHERE id = o.raw_message_id)")+` OR
				      (o.tx_date <= drm.received_at + interval '2 minutes'
				       AND o.tx_date >= drm.received_at - interval '3 days')))
				))
			)
		JOIN raw_messages orm ON orm.id = o.raw_message_id AND COALESCE(orm.deleted, false) = false
			AND COALESCE(orm.sender_jid, '') <> 'bot-forward'
			AND (NOT dd.tx_time_guessed OR dd.collapsed OR dd.superseded_by_raw IS NOT NULL
			     OR orm.received_at BETWEEN drm.received_at - interval '1 day' AND drm.received_at + interval '1 day')
		LEFT JOIN contacts c ON c.id = o.contact_id
		WHERE drm.wa_message_id = $1 AND COALESCE(drm.deleted, false) = false
		  AND dd.is_duplicate = true AND dd.ignored = false
		ORDER BY 7 DESC, orm.received_at, o.id
		LIMIT 1`, waID).
		Scan(&o.KeptWa, &o.KeptNamed, &o.KeptContactID, &o.KeptClient, &o.Amount, &o.TxDate, &o.Strong)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	o.Found = true
	return o, nil
}

// ReceiptReplyRef — сообщение, отправитель и группа чека (чтобы ответить на него
// цитатой).
func (d *DB) ReceiptReplyRef(ctx context.Context, receiptID int) (waID, senderJID, groupJID string, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COALESCE(rm.wa_message_id, ''), COALESCE(rm.sender_jid, ''), COALESCE(br.group_jid, rm.wa_group_jid, '')
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id WHERE br.id = $1`, receiptID).
		Scan(&waID, &senderJID, &groupJID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", nil
	}
	return
}

// OtherGroupCopy — тот же чек (общий номер операции), засчитанный в другой
// группе на названного клиента.
type OtherGroupCopy struct {
	GroupJID  string
	WaMessage string
	ContactID *int
	Client    string
}

// NamedCopiesInOtherGroups — живые копии чека с тем же номером операции (или
// кодом авторизации при той же сумме) в ДРУГИХ группах, где клиент назван
// (подпись, ответ) и записан не на contactID. Копии, пересланные ботом, не в
// счёт — они повторяют свой исходный чек. excludeRawID — сообщение самого чека.
func (d *DB) NamedCopiesInOtherGroups(ctx context.Context, groupJID string, excludeRawID int, docNumber, authCode string, amount float64, txDate time.Time, contactID *int) ([]OtherGroupCopy, error) {
	if docNumber == "" && authCode == "" {
		return nil, nil
	}
	// Номер операции — тот же чек (короткий номер — только при той же сумме, как
	// в отчёте); код авторизации короткий — та же сумма и несколько дней.
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.wa_message_id, ''), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, '')
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE COALESCE(br.group_jid, rm.wa_group_jid, '') NOT IN ('', $1)
		  AND br.raw_message_id <> $2
		  AND br.is_duplicate = false AND br.ignored = false AND br.collapsed = false
		  AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND br.client_confirmed AND br.contact_id IS NOT NULL
		  AND ($7::int IS NULL OR br.contact_id <> $7)
		  AND br.tx_date BETWEEN $6::timestamptz - interval '3 days' AND $6::timestamptz + interval '3 days'
		  AND (
		    ($3 <> '' AND br.doc_number = $3 AND (char_length($3) >= 8 OR br.amount = $5::numeric))
		    OR ($4 <> '' AND br.auth_code = $4 AND br.amount = $5::numeric)
		  )
		ORDER BY rm.received_at, br.id
		LIMIT 5`, groupJID, excludeRawID, docNumber, authCode, amount, txDate, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OtherGroupCopy
	for rows.Next() {
		var o OtherGroupCopy
		if err := rows.Scan(&o.GroupJID, &o.WaMessage, &o.ContactID, &o.Client); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
