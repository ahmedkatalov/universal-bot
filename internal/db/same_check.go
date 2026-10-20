package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

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
	// ScreenReplaced — проверяемый полный чек (время операции напечатано и/или
	// есть номер) заменил в учёте уже засчитанный экран того же перевода без
	// напечатанного времени и номера (VictimWa — экран, superseded_by_raw у него
	// — этот чек): сказать об этом и спросить «тот же или другой?» про экран
	// (ответ «другой» вернёт его). См. replaceScreenWith.
	ScreenReplaced bool
	KeptNamed      bool // клиент оставшегося в учёте назван (а не владелец карты)
	// Parked — спросить «тот же или другой?» сейчас нельзя (группа на
	// молчании): чек VictimWa (более поздний чек без даты или экран, заменённый
	// полным чеком) не повтор, а ждёт в «НЕ вошло»; бот спросит, когда молчание
	// снимут (ParkedSameChecks).
	Parked bool
}

// WeakSame — что делать со слабым совпадением «похоже, тот же перевод» (шаги
// 3–4 DedupeCompletedReceipt: общего номера нет, у одного из чеков время
// операции не прочиталось). Это догадка — её можно держать, только пока о ней
// спрашивают.
type WeakSame int

const (
	// WeakOff — слабое совпадение не ищем.
	WeakOff WeakSame = iota
	// WeakHold — более поздний чек без даты становится повтором (не в сборе),
	// а вызывающий спрашивает «тот же или другой?» (или спросить и не просили —
	// вопросы бота выключены совсем).
	WeakHold
	// WeakPark — спросить сейчас нельзя (группа на молчании): более поздний чек
	// без даты не повтор и не в сборе — ждёт в «НЕ вошло» (needs_review), с
	// каким чеком совпал — superseded_by_raw; спросят, когда молчание снимут.
	WeakPark
)

// completedReceipt — проверяемый чек.
type completedReceipt struct {
	groupJID, sender, wa string
	id, rawID            int
	received             time.Time
	live, named          bool
	contactID            *int
	client               string
	p                    PrintedReceipt
	guessed, collapsed   bool
	dateOnly             bool // на чеке только дата операции — время в tx_date выбрано
	denied               bool // человек уже ответил «другой» на «похоже, тот же чек»
}

// opTime — время операции, напечатанное на чеке, по которому можно судить «экран
// прислан не раньше, чем сделан перевод»: оно прочиталось (не время сообщения),
// на чеке не одна дата (тогда время выбрано, см. date_only.go) и оно не позже,
// чем чек прислали (иначе ошибка чтения или другой часовой пояс, см.
// PlausibleOpTime). nil — по времени не судим.
func (r completedReceipt) opTime() *time.Time {
	if r.guessed || r.dateOnly || !PlausibleOpTime(r.p.TxDate, r.received) {
		return nil
	}
	t := r.p.TxDate
	return &t
}

// screenLike — у чека нет ни напечатанного времени операции, ни номера (экран
// «Перевод выполнен», свёрнутый экран): его «время» — время сообщения.
func (r completedReceipt) screenLike() bool {
	return r.collapsed || (r.guessed && r.p.DocNumber == "" && r.p.AuthCode == "")
}

func (d *DB) loadCompletedReceipt(ctx context.Context, id int) (completedReceipt, bool, error) {
	r := completedReceipt{id: id}
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(br.group_jid, rm.wa_group_jid, ''), COALESCE(rm.sender_jid, ''), COALESCE(rm.wa_message_id, ''),
		       br.raw_message_id, rm.received_at,
		       (br.is_duplicate = false AND br.ignored = false AND COALESCE(rm.deleted, false) = false),
		       (br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, ''),
		       `+printedRecipientOf("br")+`, COALESCE(br.sender_raw, ''), COALESCE(br.bank, ''),
		       COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''), COALESCE(br.amount, 0)::float8, br.tx_date,
		       br.tx_time_guessed, br.collapsed, br.tx_date_only, br.same_check_denied
		FROM bank_receipts br
		JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		WHERE br.id = $1`, id).
		Scan(&r.groupJID, &r.sender, &r.wa, &r.rawID, &r.received, &r.live, &r.named, &r.contactID, &r.client,
			&r.p.Recipient, &r.p.Sender, &r.p.Bank, &r.p.DocNumber, &r.p.AuthCode, &r.p.Amount, &r.p.TxDate,
			&r.guessed, &r.collapsed, &r.dateOnly, &r.denied)
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
//     операции (или номером), а в группе уже засчитан чек того же клиента на
//     ту же сумму без времени и номера (экран «Перевод выполнен»), присланный
//     не раньше, чем сделана операция этого (шаг 4, replaceScreenWith).
//     Повтором становится экран: в учёте остаётся полный чек (дата операции с
//     чека), а вызывающий спрашивает «тот же или другой?» про экран
//     (ScreenReplaced).
//
// Слабые совпадения (шаги 3–4) — догадка: её держат, только пока о ней можно
// спросить (weak, см. WeakSame).
//
// Повтором становится тот, что ещё не был в сборе: сам проверяемый чек, а если
// он назван, а копия ничья, — ничья копия. Копии, названные РАЗНЫМИ клиентами,
// без общего номера операции не склеиваем.
func (d *DB) DedupeCompletedReceipt(ctx context.Context, receiptID int, amountFilled, allowTentative bool) (SameCheck, error) {
	weak := WeakOff
	if allowTentative {
		weak = WeakHold
	}
	return d.DedupeCompletedReceiptWeak(ctx, receiptID, amountFilled, weak)
}

// DedupeCompletedReceiptWeak — DedupeCompletedReceipt, слабое совпадение
// (шаги 3–4) — как скажет weak.
func (d *DB) DedupeCompletedReceiptWeak(ctx context.Context, receiptID int, amountFilled bool, weak WeakSame) (SameCheck, error) {
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

	// Слабое совпадение (шаг 3): повтором (или, если спросить нельзя, в «НЕ
	// вошло») становится более поздний чек без даты (см. holdWeakSame).
	weakHold := func(victim, kept int, victimIsThis bool, keptWa, victimWa, client string, tx time.Time) (SameCheck, error) {
		ok, err := d.holdWeakSame(ctx, weak, victim, kept)
		if err != nil || !ok {
			return SameCheck{}, err
		}
		res.Found, res.Tentative, res.VictimIsThis, res.Parked = true, true, victimIsThis, weak == WeakPark
		res.KeptWa, res.VictimWa = keptWa, victimWa
		res.Client, res.Amount, res.TxDate = client, r.p.Amount, tx
		if res.Parked {
			fmt.Printf("Чек #%d (%s): похоже, тот же перевод, что %s, а спросить нельзя — чек %s ждёт в «НЕ вошло»\n", receiptID, r.wa, keptWa, victimWa)
		} else {
			fmt.Printf("Чек #%d (%s): похоже, тот же перевод, что %s — %s не считаю, спрошу\n", receiptID, r.wa, keptWa, victimWa)
		}
		return res, nil
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
		oScreen         bool
	)
	err = d.pool.QueryRow(ctx, `
		SELECT br.id, COALESCE(rm.wa_message_id, ''), (br.client_confirmed AND br.contact_id IS NOT NULL), br.contact_id,
		       COALESCE(c.canonical_name, br.recipient_raw, ''), br.tx_date,
		       COALESCE(($3 <> '' AND br.doc_number = $3) OR ($4 <> '' AND br.auth_code = $4), false) AS strong,
		       (br.tx_time_guessed AND COALESCE(br.doc_number, '') = '' AND COALESCE(br.auth_code, '') = '')
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
		ORDER BY 7 DESC, br.tx_time_guessed, rm.received_at, br.id
		LIMIT 1`, r.groupJID, receiptID, r.p.DocNumber, r.p.AuthCode, r.p.Amount, r.p.TxDate, heur, r.contactID, r.p.Sender).
		Scan(&oID, &oWa, &oNamed, &oContact, &oClient, &oTx, &oStrong, &oScreen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return res, err
	default:
		if weak != WeakOff && !oStrong && oScreen && !r.screenLike() && r.named == oNamed {
			// Совпал экран без времени и номера, а у этого чека время операции
			// напечатано: в учёте остаётся этот чек (дата операции с него), экран —
			// повтор, и спросим «тот же или другой?» (шаг 4, replaceScreenWith).
			// Экран, присланный раньше операции этого чека, — другой перевод: тогда
			// как прежде.
			if s, err := d.replaceScreenWith(ctx, r, oID, weak); err != nil || s.Found {
				return s, err
			}
		}
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
	if weak != WeakOff && !r.denied && r.named && r.guessed && r.p.DocNumber == "" && r.p.AuthCode == "" {
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
			  -- другой часовой пояс), на чеке одна дата — время выбрано: по нему
			  -- не судим (только сутки между присылками).
			  AND (`+untrustedOpSQL("br", "rm.received_at")+` OR
			       (br.tx_date <= $5::timestamptz + interval '2 minutes'
			        AND br.tx_date >= $5::timestamptz - interval '3 days'))
			  AND rm.received_at BETWEEN $5::timestamptz - interval '1 day' AND $5::timestamptz + interval '1 day'
			  AND (`+ownerNorm+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = `+ownerNorm+`)
			  AND (`+senderNorm+` = '' OR `+printedNorm("br.sender_raw")+` = '' OR `+printedNorm("br.sender_raw")+` = `+senderNorm+`)
			  -- Полный чек, у которого экран того же перевода уже есть (учтён им
			  -- или ждёт вопроса), — не пара этому: у перевода один экран (как в
			  -- шаге 4 и CountedCheckOfCollapsedScreen). Иначе второй перевод
			  -- той же суммы (экран + полный чек) спутался бы с первым.
			  AND NOT EXISTS (
				SELECT 1 FROM bank_receipts s
				WHERE s.superseded_by_raw = br.raw_message_id AND s.id <> $2
				  AND (s.is_duplicate OR s.same_check_parked) AND NOT s.ignored)
			ORDER BY abs(extract(epoch FROM rm.received_at - $5::timestamptz)), br.id
			LIMIT 1`, r.groupJID, receiptID, r.contactID, r.p.Amount, r.received, r.p.Recipient, r.p.Sender).
			Scan(&oID, &oWa, &oClient, &oTx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return res, err
		default:
			return weakHold(receiptID, oID, true, oWa, r.wa, oClient, oTx)
		}
	}
	// 4. Обратный порядок к шагу 3: этот чек — полный, а экран того же перевода
	// без времени и номера уже засчитан (replaceScreenWith).
	return d.replaceScreenWith(ctx, r, 0, weak)
}

// holdWeakSame — слабое совпадение «похоже, тот же перевод»: чек victim учтён
// чеком kept. WeakHold — victim повтор (не в сборе), вызывающий спрашивает «тот
// же или другой?»; WeakPark — спросить нельзя (группа на молчании): victim не в
// сборе и не повтор, а ждёт в «НЕ вошло» (спросят, когда молчание снимут,
// ParkedSameChecks). В обоих случаях у victim запоминается, каким чеком он учтён
// (superseded_by_raw — одна связь на все пути): ответ «другой» или ФИО другого
// клиента, удаление чека kept или смена его клиента вернут victim в учёт.
// false — victim уже не живой.
func (d *DB) holdWeakSame(ctx context.Context, weak WeakSame, victim, kept int) (bool, error) {
	q := `UPDATE bank_receipts v SET is_duplicate = true, superseded_by_raw = k.raw_message_id,
			needs_review = CASE WHEN v.same_check_parked THEN false ELSE v.needs_review END, same_check_parked = false
		FROM bank_receipts k WHERE v.id = $1 AND k.id = $2 AND v.is_duplicate = false AND v.ignored = false`
	if weak == WeakPark {
		q = `UPDATE bank_receipts v SET needs_review = true, same_check_parked = true, superseded_by_raw = k.raw_message_id
		FROM bank_receipts k WHERE v.id = $1 AND k.id = $2 AND v.is_duplicate = false AND v.ignored = false`
	}
	tag, err := d.pool.Exec(ctx, q, victim, kept)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceScreenWithCheck — шаг 4 DedupeCompletedReceipt отдельно: полный чек
// checkID только что вошёл в сбор при приёме (подпись), а экран того же
// перевода без времени и номера уже в сборе. Шаги 1–3 при приёме сделала
// проверка «этот чек уже присылали». screenID — экран, который она нашла (тот
// же клиент, сумма, «время» экрана — время сообщения — в окне проверки
// повтора); 0 — найти (см. replaceScreenWith). weak — как со слабым
// совпадением (спросить можно — WeakHold, группа на молчании — WeakPark).
func (d *DB) ReplaceScreenWithCheck(ctx context.Context, checkID, screenID int, weak WeakSame) (SameCheck, error) {
	r, ok, err := d.loadCompletedReceipt(ctx, checkID)
	if err != nil || !ok {
		return SameCheck{}, err
	}
	return d.replaceScreenWith(ctx, r, screenID, weak)
}

// replaceScreenWith — единственное место, где решается «полный чек r и экран —
// один перевод»: у r время операции напечатано и/или есть номер операции, а в
// группе уже засчитан экран того же перевода без напечатанного времени и номера
// («Перевод выполнен» с подписью, названный в ответ свёрнутый экран). В учёте
// остаётся полный чек (по нему дата операции, его номер свяжет копии в других
// группах), экран — повтор этого чека (superseded_by_raw); номера у экрана нет
// — это не наверняка (клиент мог заплатить ту же сумму ещё раз): вызывающий
// спрашивает «тот же или другой?» про экран, ответ «другой» вернёт его. Группа
// на молчании (WeakPark) — экран ждёт в «НЕ вошло», спросят потом.
//
// screenID — экран уже найден (проверка «этот чек уже присылали»); 0 — найти:
// названный тот же клиент, та же сумма, напечатанные получатель и плательщик не
// спорят, прислан в пределах суток от r. В обоих случаях экран прислан не
// раньше, чем сделана операция r (2 минуты на разницу часов): экран показывает
// уже сделанный перевод, присланный раньше операции — другой перевод (время
// операции, по которому не судим, — см. opTime, — не проверяем). Экран, про
// который уже ответили «другой» на вопрос об ЭТОМ чеке (или неизвестно, о
// каком), и чек, уже сопоставленный с другим экраном, не трогаем; «другой» про
// другой полный чек не мешает: экран — не тот перевод, но может быть этого. Из
// нескольких экранов — сначала от того же работника, затем ближайший по
// времени к операции.
func (d *DB) replaceScreenWith(ctx context.Context, r completedReceipt, screenID int, weak WeakSame) (SameCheck, error) {
	var res SameCheck
	if weak == WeakOff || !r.live || r.p.Amount <= 0 || r.sender == "bot-forward" || r.groupJID == "" || r.screenLike() {
		return res, nil
	}
	if screenID <= 0 && (r.denied || !r.named || r.contactID == nil) {
		return res, nil
	}
	op := r.opTime()
	// Экран без времени и номера, живой, в этой группе, прислан работником (не
	// копия, пересланная ботом), не раньше операции этого чека, «другой» про него
	// сказан не об этом чеке, и экрана у этого чека ещё нет.
	screenCond := `COALESCE(br.group_jid, rm.wa_group_jid) = $1 AND br.raw_message_id <> $2
		  AND br.is_duplicate = false AND br.ignored = false
		  AND br.tx_time_guessed AND COALESCE(br.doc_number, '') = '' AND COALESCE(br.auth_code, '') = ''
		  AND (NOT br.same_check_denied OR (br.same_check_denied_vs IS NOT NULL AND br.same_check_denied_vs <> $2))
		  AND COALESCE(rm.deleted, false) = false
		  AND COALESCE(rm.sender_jid, '') <> 'bot-forward'
		  AND ($3::timestamptz IS NULL OR
		       (rm.received_at >= $3::timestamptz - interval '2 minutes'
		        AND rm.received_at <= $3::timestamptz + interval '3 days'))
		  AND NOT EXISTS (
			SELECT 1 FROM bank_receipts s
			WHERE s.superseded_by_raw = $2 AND (s.is_duplicate OR s.same_check_parked) AND NOT s.ignored)`
	var (
		sID        int
		sWa, sName string
		sNamed     bool
		err        error
	)
	if screenID > 0 {
		err = d.pool.QueryRow(ctx, `
			SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(c.canonical_name, br.recipient_raw, ''),
			       (br.client_confirmed AND br.contact_id IS NOT NULL)
			FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN contacts c ON c.id = br.contact_id
			WHERE br.id = $4 AND `+screenCond, r.groupJID, r.rawID, op, screenID).Scan(&sID, &sWa, &sName, &sNamed)
	} else {
		ownerNorm := printedNorm("$7::text")
		payerNorm := printedNorm("$8::text")
		err = d.pool.QueryRow(ctx, `
			SELECT br.id, COALESCE(rm.wa_message_id, ''), COALESCE(c.canonical_name, br.recipient_raw, ''), true
			FROM bank_receipts br
			JOIN raw_messages rm ON rm.id = br.raw_message_id
			LEFT JOIN contacts c ON c.id = br.contact_id
			WHERE `+screenCond+`
			  AND br.needs_review = false
			  AND br.client_confirmed AND br.contact_id = $4
			  AND br.amount = $5::numeric
			  AND rm.received_at BETWEEN $6::timestamptz - interval '1 day' AND $6::timestamptz + interval '1 day'
			  AND (`+ownerNorm+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = '' OR `+printedNorm(printedRecipientOf("br"))+` = `+ownerNorm+`)
			  AND (`+payerNorm+` = '' OR `+printedNorm("br.sender_raw")+` = '' OR `+printedNorm("br.sender_raw")+` = `+payerNorm+`)
			ORDER BY (COALESCE(rm.sender_jid, '') <> $9), abs(extract(epoch FROM rm.received_at - COALESCE($3::timestamptz, $6::timestamptz))), br.id
			LIMIT 1`, r.groupJID, r.rawID, op, r.contactID, r.p.Amount, r.received, r.p.Recipient, r.p.Sender, r.sender).
			Scan(&sID, &sWa, &sName, &sNamed)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if ok, err := d.holdWeakSame(ctx, weak, sID, r.id); err != nil || !ok {
		return res, err
	}
	// Тот же экран прислали и в другую группу (тот же клиент и сумма, «время» —
	// время сообщения рядом с этим экраном): отчёт считал эти копии одним
	// переводом. Теперь в учёте полный чек с напечатанным временем операции —
	// копиям экрана ставим это время, иначе они разошлись бы с полным чеком
	// (без номера копии сводятся по времени) и перевод посчитался бы дважды.
	if op != nil {
		if n, err := d.moveScreenCopies(ctx, sID, r.groupJID, *op); err != nil {
			return res, err
		} else if n > 0 {
			fmt.Printf("Экран %s: его копии в других группах (%d) — время операции с полного чека %s\n", sWa, n, op.Format("02.01 15:04:05"))
		}
	}
	res.Found, res.ScreenReplaced, res.Tentative, res.Parked = true, true, true, weak == WeakPark
	res.VictimWa, res.KeptWa = sWa, r.wa
	res.Client, res.KeptNamed = r.client, r.named
	if !r.named && sNamed {
		res.Client = sName
	}
	res.Amount, res.TxDate = r.p.Amount, r.p.TxDate
	if r.named {
		res.ThisClient = r.client
	}
	if res.Parked {
		fmt.Printf("Чек %s: тот же перевод, что экран без времени и номера (%s), а спросить нельзя — экран ждёт в «НЕ вошло»\n", r.wa, sWa)
	} else {
		fmt.Printf("Чек %s: тот же перевод, что экран без времени и номера (%s) — считаю этот чек, экран — повтор\n", r.wa, sWa)
	}
	return res, nil
}

// UndoReceiptDuplicate — человек сказал, что чек сообщения waID не повтор, а
// отдельный платёж: снова учитываем его (и запоминаем ответ — «тот же?» по
// слабому совпадению больше не спрашиваем). needsReview — клиент у него ещё не
// назван (спросить «чей чек?»).
func (d *DB) UndoReceiptDuplicate(ctx context.Context, waID string) (found bool, amount float64, client string, needsReview bool, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE bank_receipts br SET is_duplicate = false, superseded_by_raw = NULL, same_check_denied = true,
			same_check_parked = false,
			-- «другой» — про тот чек, которым этот был учтён: экраном полного
			-- чека, присланного позже, он ещё может оказаться (шаг 4).
			same_check_denied_vs = CASE WHEN br.same_check_denied AND br.same_check_denied_vs IS NULL THEN NULL
			                            ELSE br.superseded_by_raw END
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
				-- Экран (свёрнутый или без времени и номера) или чек, учтённый
				-- догадкой «похоже, тот же перевод»: каким чеком он учтён.
				(dd.superseded_by_raw IS NOT NULL AND o.raw_message_id = dd.superseded_by_raw) OR
				(dd.superseded_by_raw IS NULL AND NOT dd.collapsed AND (
				(NULLIF(dd.doc_number, '') IS NOT NULL AND o.doc_number = dd.doc_number) OR
				(NULLIF(dd.auth_code, '') IS NOT NULL AND o.auth_code = dd.auth_code) OR
				(o.contact_id = dd.contact_id AND o.amount = dd.amount
				 AND `+sameOpTimeSQL("o.tx_date", "o.tx_time_guessed", "dd.tx_date", "dd.tx_time_guessed")+`) OR
				`+printedMatchSQL("o", "dd")+` OR
				-- Время с чека не прочиталось: тот же клиент и сумма, чек сделан не
				-- позже, чем прислали этот (см. DedupeCompletedReceipt).
				-- Время операции оставшегося позже его присылки (или на нём одна
				-- дата) — по нему не судим.
				(dd.tx_time_guessed AND dd.client_confirmed AND o.client_confirmed
				 AND o.contact_id = dd.contact_id AND o.amount = dd.amount
				 AND (`+untrustedOpSQL("o", "(SELECT received_at FROM raw_messages WHERE id = o.raw_message_id)")+` OR
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
		ORDER BY 7 DESC, (o.raw_message_id = dd.superseded_by_raw) IS TRUE DESC, orm.received_at, o.id
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

// screenCopyCond — чек o — копия экрана s в другой группе: экран без
// напечатанного времени и номера, тот же названный клиент, та же сумма, «время»
// (время сообщения) в пределах двух минут от экрана, напечатанное на экранах не
// спорит. Так отчёт сводит копии без номера (см. ledger.sameCheck).
func screenCopyCond(o, orm, s, sGroup string) string {
	ro, rs := printedNorm(printedRecipientOf(o)), printedNorm(printedRecipientOf(s))
	po, ps := printedNorm(o+".sender_raw"), printedNorm(s+".sender_raw")
	return `COALESCE(` + o + `.group_jid, ` + orm + `.wa_group_jid, '') NOT IN ('', ` + sGroup + `)
		AND ` + o + `.id <> ` + s + `.id
		AND ` + o + `.is_duplicate = false AND ` + o + `.ignored = false AND COALESCE(` + orm + `.deleted, false) = false
		AND COALESCE(` + orm + `.sender_jid, '') <> 'bot-forward'
		AND ` + o + `.tx_time_guessed AND COALESCE(` + o + `.doc_number, '') = '' AND COALESCE(` + o + `.auth_code, '') = ''
		AND ` + s + `.client_confirmed AND ` + s + `.contact_id IS NOT NULL
		AND ` + o + `.client_confirmed AND ` + o + `.contact_id = ` + s + `.contact_id
		AND ` + o + `.amount = ` + s + `.amount
		AND (` + ro + ` = '' OR ` + rs + ` = '' OR ` + ro + ` = ` + rs + `)
		AND (` + po + ` = '' OR ` + ps + ` = '' OR ` + po + ` = ` + ps + `)`
}

// moveScreenCopies — экран screenID заменён полным чеком группы groupJID с
// напечатанным временем операции op: его копиям в других группах (экраны без
// времени и номера, см. screenCopyCond) ставим время операции op. Возвращает,
// скольким.
func (d *DB) moveScreenCopies(ctx context.Context, screenID int, groupJID string, op time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts o SET tx_date = $3::timestamptz
		FROM bank_receipts s, raw_messages orm
		WHERE s.id = $1 AND orm.id = o.raw_message_id AND COALESCE(s.group_jid, $2) = $2
		  AND `+screenCopyCond("o", "orm", "s", "$2")+`
		  AND abs(extract(epoch FROM o.tx_date - s.tx_date)) <= 120`, screenID, groupJID, op)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MoveScreenCopiesBack — экран сообщения screenWa снова отдельный платёж (ответ
// «другой» на «похоже, это полный чек того же перевода»): его копиям в других
// группах, которым moveScreenCopies поставил время операции op полного чека,
// возвращаем их время — время сообщения (они снова сводятся с этим экраном).
func (d *DB) MoveScreenCopiesBack(ctx context.Context, screenWa string, op time.Time) error {
	if screenWa == "" || op.IsZero() {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts o SET tx_date = orm.received_at
		FROM bank_receipts s, raw_messages srm, raw_messages orm
		WHERE srm.wa_message_id = $1 AND s.raw_message_id = srm.id AND s.is_duplicate = false
		  AND orm.id = o.raw_message_id
		  AND s.tx_time_guessed AND COALESCE(s.doc_number, '') = '' AND COALESCE(s.auth_code, '') = ''
		  AND `+screenCopyCond("o", "orm", "s", "COALESCE(s.group_jid, srm.wa_group_jid, '')")+`
		  AND o.tx_date = $2::timestamptz AND orm.received_at IS NOT NULL AND o.tx_date <> orm.received_at
		  AND abs(extract(epoch FROM orm.received_at - s.tx_date)) <= 120`, screenWa, op)
	return err
}

// moveScreenCopiesBackOf — MoveScreenCopiesBack по записи экрана screenID: его
// снова считают отдельно (полный чек, которым он был учтён, удалён или записан
// на другого клиента), а время операции того чека было op.
func (d *DB) moveScreenCopiesBackOf(ctx context.Context, screenID int, op time.Time) {
	var wa string
	if err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(rm.wa_message_id, '') FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE br.id = $1`, screenID).Scan(&wa); err != nil {
		return
	}
	if err := d.MoveScreenCopiesBack(ctx, wa, op); err != nil {
		fmt.Println("Возврат времени копиям экрана не удался:", err)
	}
}

// ParkedSameCheck — чек, отложенный в «НЕ вошло» догадкой «похоже, тот же
// перевод», пока группа молчала (WeakPark).
type ParkedSameCheck struct {
	ID          int
	WaMessageID string
	SenderJID   string
	Client      string
	Amount      float64
	// KeptOK — чек, с которым он совпал, всё ещё в сборе на того же клиента и
	// ту же сумму (иначе догадки больше нет — чек просто идёт в сбор).
	KeptOK     bool
	KeptClient string
	KeptTxDate time.Time
}

// ParkedSameChecks — отложенные «похоже, тот же перевод» чеки группы (самые
// ранние первыми): бот спрашивает о них, когда в группе снова можно писать.
func (d *DB) ParkedSameChecks(ctx context.Context, groupJID string, limit int) ([]ParkedSameCheck, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT p.id, COALESCE(prm.wa_message_id, ''), COALESCE(prm.sender_jid, ''),
		       COALESCE(pc.canonical_name, p.recipient_raw, ''), COALESCE(p.amount, 0)::float8,
		       k.id IS NOT NULL, COALESCE(k.client, ''), COALESCE(k.tx_date, p.tx_date)
		FROM bank_receipts p
		JOIN raw_messages prm ON prm.id = p.raw_message_id
		LEFT JOIN contacts pc ON pc.id = p.contact_id
		LEFT JOIN LATERAL (
			SELECT k.id, COALESCE(kc.canonical_name, k.recipient_raw, '') AS client, k.tx_date
			FROM bank_receipts k
			JOIN raw_messages krm ON krm.id = k.raw_message_id AND COALESCE(krm.deleted, false) = false
			LEFT JOIN contacts kc ON kc.id = k.contact_id
			WHERE k.raw_message_id = p.superseded_by_raw AND k.id <> p.id
			  AND k.is_duplicate = false AND k.ignored = false AND k.needs_review = false
			  AND k.client_confirmed AND k.contact_id = p.contact_id AND k.amount = p.amount
			ORDER BY k.id DESC LIMIT 1
		) k ON true
		WHERE COALESCE(p.group_jid, prm.wa_group_jid) = $1
		  AND p.same_check_parked AND p.needs_review AND p.is_duplicate = false AND p.ignored = false
		  AND COALESCE(prm.deleted, false) = false
		ORDER BY prm.received_at, p.id
		LIMIT $2`, groupJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParkedSameCheck
	for rows.Next() {
		var p ParkedSameCheck
		if err := rows.Scan(&p.ID, &p.WaMessageID, &p.SenderJID, &p.Client, &p.Amount, &p.KeptOK, &p.KeptClient, &p.KeptTxDate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UnparkSameCheck — догадки «тот же перевод» больше нет (тот чек удалён,
// исключён, записан на другого): отложенный чек идёт в сбор как прислан.
func (d *DB) UnparkSameCheck(ctx context.Context, id int) error {
	// Время операции чека, которым он был учтён (копиям экрана его ставили).
	var op *time.Time
	_ = d.pool.QueryRow(ctx, `
		SELECT k.tx_date FROM bank_receipts p JOIN bank_receipts k ON k.raw_message_id = p.superseded_by_raw AND k.id <> p.id
		WHERE p.id = $1 ORDER BY k.id DESC LIMIT 1`, id).Scan(&op)
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET same_check_parked = false, superseded_by_raw = NULL,
			needs_review = NOT (client_confirmed AND contact_id IS NOT NULL AND COALESCE(amount, 0) > 0)
		WHERE id = $1 AND same_check_parked AND is_duplicate = false`, id)
	if err == nil && tag.RowsAffected() == 1 && op != nil {
		d.moveScreenCopiesBackOf(ctx, id, *op)
	}
	return err
}

// HoldParkedSameCheck — про отложенный чек спросили «тот же или другой?»:
// теперь он повтор (не в сборе), ответ «другой» вернёт его (UndoReceiptDuplicate).
func (d *DB) HoldParkedSameCheck(ctx context.Context, id int) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts SET is_duplicate = true, needs_review = false, same_check_parked = false
		WHERE id = $1 AND same_check_parked AND needs_review AND is_duplicate = false AND ignored = false`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ParkHeldSameCheck — чек сообщения waID держали повтором по догадке «похоже,
// тот же перевод» (superseded_by_raw — каким чеком учтён), а вопрос «тот же или
// другой?» в группу не ушёл: молча догадку не держим — чек ждёт в «НЕ вошло»,
// бот спросит в следующий раз (ParkedSameChecks).
func (d *DB) ParkHeldSameCheck(ctx context.Context, waID string) error {
	if waID == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE bank_receipts br SET is_duplicate = false, needs_review = true, same_check_parked = true
		FROM raw_messages rm
		WHERE rm.id = br.raw_message_id AND rm.wa_message_id = $1 AND COALESCE(rm.deleted, false) = false
		  AND br.is_duplicate AND NOT br.ignored AND br.superseded_by_raw IS NOT NULL`, waID)
	return err
}

// ReleasedHold — чек, который считался тем же переводом, что чек другого
// клиента, и снова идёт в сбор отдельно.
type ReleasedHold struct {
	WaMessageID string
	Client      string
	Amount      float64
}

// ReleaseHeldOnClientChange — у чека keptID поменяли клиента. Чеки, которые
// считались тем же переводом только потому, что у них тот же клиент и сумма
// (слабое совпадение «похоже, тот же перевод», superseded_by_raw — этот чек;
// общего номера операции нет), теперь записаны на ДРУГОГО клиента: разных
// клиентов не склеиваем — они снова отдельные платежи (или, если ждали вопроса
// в «НЕ вошло», идут в сбор). Неназванный свёрнутый экран, признанный тем же
// переводом по работнику и сумме, клиента не имеет — его не трогаем.
func (d *DB) ReleaseHeldOnClientChange(ctx context.Context, keptID int) ([]ReleasedHold, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT s.id, COALESCE(srm.wa_message_id, ''), COALESCE(sc.canonical_name, s.recipient_raw, ''),
		       COALESCE(s.amount, 0)::float8, COALESCE(kc.canonical_name, k.recipient_raw, ''), k.tx_date
		FROM bank_receipts k
		JOIN bank_receipts s ON s.superseded_by_raw = k.raw_message_id AND s.id <> k.id
		JOIN raw_messages srm ON srm.id = s.raw_message_id AND COALESCE(srm.deleted, false) = false
		LEFT JOIN contacts sc ON sc.id = s.contact_id
		LEFT JOIN contacts kc ON kc.id = k.contact_id
		WHERE k.id = $1 AND s.ignored = false
		  AND (s.is_duplicate OR s.same_check_parked)
		  AND s.client_confirmed AND s.contact_id IS NOT NULL
		  AND s.contact_id IS DISTINCT FROM k.contact_id
		  AND NOT `+sameStrongIDSQL("s.doc_number", "s.auth_code", "k.doc_number", "k.auth_code"), keptID)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id int
		op time.Time
		r  ReleasedHold
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var kept string
		if err := rows.Scan(&c.id, &c.r.WaMessageID, &c.r.Client, &c.r.Amount, &kept, &c.op); err != nil {
			rows.Close()
			return nil, err
		}
		// Тот же клиент, записанный иначе («Харон Исаев», «Исаев»), — догадка
		// в силе.
		if clientNamesCompatible(c.r.Client, kept) {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ReleasedHold
	for _, c := range cands {
		tag, err := d.pool.Exec(ctx, `
			UPDATE bank_receipts SET is_duplicate = false, superseded_by_raw = NULL, same_check_parked = false,
				needs_review = NOT (client_confirmed AND contact_id IS NOT NULL AND COALESCE(amount, 0) > 0)
			WHERE id = $1 AND (is_duplicate OR same_check_parked) AND ignored = false`, c.id)
		if err != nil {
			return out, err
		}
		if tag.RowsAffected() == 1 {
			fmt.Printf("Чек %s (%s, %.0f ₽) считался тем же переводом, что чек #%d, а тот теперь на другом клиенте — снова отдельный платёж\n",
				c.r.WaMessageID, c.r.Client, c.r.Amount, keptID)
			// Копии экрана в других группах снова сводятся с ним, а не с тем чеком.
			d.moveScreenCopiesBackOf(ctx, c.id, c.op)
			out = append(out, c.r)
		}
	}
	return out, nil
}

// clientNamesCompatible — одно ФИО целиком входит в другое (порядок слов,
// регистр, «ё», знаки не важны; инициал совпадает со словом на ту же букву):
// «Исаев Харон» и «Харон Исаев», «Исаев» и «Исаев Харон», «Исаев Х.» и «Исаев
// Харон».
func clientNamesCompatible(a, b string) bool {
	words := func(s string) [][]rune {
		var out [][]rune
		for _, w := range strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "ё", "е"), "Ё", "Е")),
			func(r rune) bool { return !unicode.IsLetter(r) }) {
			out = append(out, []rune(w))
		}
		return out
	}
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	if len(wa) > len(wb) {
		wa, wb = wb, wa
	}
	used := make([]bool, len(wb))
	take := func(ok func(x, y []rune) bool, w []rune) bool {
		for j, x := range wb {
			if !used[j] && ok(w, x) {
				used[j] = true
				return true
			}
		}
		return false
	}
	same := func(x, y []rune) bool { return string(x) == string(y) }
	initial := func(x, y []rune) bool {
		return (len(x) == 1 || len(y) == 1) && x[0] == y[0]
	}
	var rest [][]rune
	for _, w := range wa {
		if !take(same, w) {
			rest = append(rest, w)
		}
	}
	for _, w := range rest {
		if !take(initial, w) {
			return false
		}
	}
	return true
}

// HeldScreenPending — свёрнутый экран сообщения screenWa всё ещё считается тем
// же переводом, что засчитанный полный чек (вопрос задан, «другой» не
// отвечали, клиента экрану не называли), и после него отправитель экрана в
// группу чеков больше не присылал: голое ФИО от него сейчас может быть про этот
// экран.
func (d *DB) HeldScreenPending(ctx context.Context, screenWa string) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `
		SELECT NOT EXISTS (
			SELECT 1 FROM bank_receipts b2 JOIN raw_messages r2 ON r2.id = b2.raw_message_id
			WHERE COALESCE(b2.group_jid, r2.wa_group_jid) = COALESCE(br.group_jid, rm.wa_group_jid)
			  AND r2.sender_jid = rm.sender_jid AND b2.id <> br.id
			  AND COALESCE(r2.deleted, false) = false
			  AND (r2.received_at > rm.received_at OR (r2.received_at = rm.received_at AND b2.id > br.id)))
		FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id
		WHERE rm.wa_message_id = $1 AND COALESCE(rm.deleted, false) = false
		  AND br.collapsed AND br.is_duplicate AND NOT br.ignored AND NOT br.same_check_denied
		  AND br.superseded_by_raw IS NOT NULL AND NOT br.client_confirmed
		ORDER BY br.id DESC LIMIT 1`, screenWa).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}
