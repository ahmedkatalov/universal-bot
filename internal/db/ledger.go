package db

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"whatsapp-bot/internal/ledger"
)

// Загрузка «улик» (чеков и текстовых платежей) для движка подсчёта ledger.
// Берём период с запасом по краям: текст и его чек присылают в пределах двух
// суток, и пара на краю периода должна найтись.

const ledgerPad = 4 * 24 * time.Hour

const ledgerSenderExpr = `COALESCE(NULLIF(po.name, ''), NULLIF(%s, ''), NULLIF(rm.sender_name, ''), split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), '')`

// LoadBook — книга платежей по всем группам для периода [from, to) (по дате
// операции или присылки — с запасом), client — необязательный фильтр (подстрока
// имени клиента; пусто — все).
func (d *DB) LoadBook(ctx context.Context, from, to time.Time, client string) (*ledger.Book, error) {
	checks, err := d.loadLedgerChecks(ctx, from.Add(-ledgerPad), to.Add(ledgerPad), client)
	if err != nil {
		return nil, err
	}
	texts, err := d.loadLedgerTexts(ctx, from.Add(-ledgerPad), to.Add(ledgerPad), client)
	if err != nil {
		return nil, err
	}
	return ledger.Build(checks, texts), nil
}

func (d *DB) loadLedgerChecks(ctx context.Context, from, to time.Time, client string) ([]ledger.Check, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+ledgerCheckCols+`
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE ((br.tx_date >= $1 AND br.tx_date < $2) OR (rm.received_at >= $1 AND rm.received_at < $2))
		  AND ($3 = '' OR COALESCE(c.canonical_name, br.recipient_raw, '') ILIKE '%' || $3 || '%')`, from, to, client)
	if err != nil {
		return nil, err
	}
	out, err := scanLedgerChecks(rows)
	if err != nil || client != "" {
		return out, err
	}
	// Тот же чек (тот же номер документа), присланный в другую группу намного
	// раньше или позже периода, тоже нужен: иначе дневной отчёт не узнает, что
	// чек уже посчитан в другой день, и посчитает его второй раз.
	have := map[int]bool{}
	var docs []string
	seen := map[string]bool{}
	for _, c := range out {
		have[c.ID] = true
		if nd := ledger.NormDoc(c.Doc); len([]rune(nd)) >= 6 && !seen[nd] {
			seen[nd] = true
			docs = append(docs, nd)
		}
	}
	if len(docs) == 0 {
		return out, nil
	}
	more, err := d.loadLedgerChecksByDocs(ctx, docs, from.AddDate(0, 0, -60), to.AddDate(0, 0, 60))
	if err != nil {
		return nil, err
	}
	for _, c := range more {
		if !have[c.ID] {
			have[c.ID] = true
			out = append(out, c)
		}
	}
	return out, nil
}

const ledgerCheckCols = `br.id, COALESCE(br.raw_message_id, 0), COALESCE(rm.wa_message_id, ''),
		       CASE WHEN COALESCE(rm.sender_jid, '') = 'bot-forward' AND position('-fwd-' in COALESCE(rm.wa_message_id, '')) > 0
		                 AND NOT br.mirror_locked
		            THEN split_part(rm.wa_message_id, '-fwd-', 1) ELSE '' END,
		       COALESCE(br.group_jid, rm.wa_group_jid, ''),
		       COALESCE(NULLIF(po.name, ''), NULLIF(br.submitted_by, ''), NULLIF(rm.sender_name, ''), split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1), ''),
		       split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1),
		       COALESCE(br.contact_id, 0), COALESCE(c.canonical_name, br.recipient_raw, ''),
		       br.client_confirmed, br.needs_review, COALESCE(br.amount, 0)::float8, br.tx_date,
		       COALESCE(rm.received_at, br.tx_date),
		       COALESCE(br.doc_number, ''), COALESCE(br.auth_code, ''),
		       COALESCE(NULLIF(br.bank, ''), ''),
		       COALESCE(NULLIF(br.recipient_bank, ''), NULLIF(br.bank, ''), ''),
		       COALESCE(NULLIF(br.card_owner, ''), NULLIF(br.recipient_raw, ''), ''),
		       COALESCE(NULLIF(br.card_owner, ''), CASE WHEN NOT br.client_confirmed THEN NULLIF(br.recipient_raw, '') END, ''),
		       COALESCE(br.sender_raw, ''),
		       EXISTS (SELECT 1 FROM amount_corrections ac WHERE ac.kind = 'receipt' AND ac.target_id = br.id AND NOT ac.reverted),
		       COALESCE(br.collector, ''),
		       br.ignored, br.is_duplicate, COALESCE(rm.deleted, false), br.created_at`

// loadLedgerChecksByDocs — чеки с теми же номерами документов (в пределах ±60 дней).
func (d *DB) loadLedgerChecksByDocs(ctx context.Context, docs []string, from, to time.Time) ([]ledger.Check, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT `+ledgerCheckCols+`
		FROM bank_receipts br
		LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		LEFT JOIN contacts c ON c.id = br.contact_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE br.doc_number IS NOT NULL AND br.doc_number <> ''
		  AND br.tx_date >= $2 AND br.tx_date < $3
		  AND regexp_replace(translate(upper(br.doc_number), 'АВСЕЁНКМОРТХУ', 'ABCEEHKMOPTXY'), '[^0-9A-ZА-Я]', '', 'g') = ANY($1)`,
		docs, from, to)
	if err != nil {
		return nil, err
	}
	return scanLedgerChecks(rows)
}

func scanLedgerChecks(rows pgx.Rows) ([]ledger.Check, error) {
	defer rows.Close()
	var out []ledger.Check
	for rows.Next() {
		var c ledger.Check
		if err := rows.Scan(&c.ID, &c.RawID, &c.WaID, &c.FwdOrigin, &c.Group, &c.Sender, &c.Phone,
			&c.ContactID, &c.Client, &c.Confirmed, &c.Review, &c.Amount, &c.OpDate, &c.PostedAt,
			&c.Doc, &c.Auth, &c.Bank, &c.CardBank, &c.CardOwner, &c.Printed, &c.Payer, &c.Fixed, &c.Collector,
			&c.Ignored, &c.Duplicate, &c.Deleted, &c.Created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) loadLedgerTexts(ctx context.Context, from, to time.Time, client string) ([]ledger.Text, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, COALESCE(t.raw_message_id, 0), COALESCE(rm.wa_message_id, ''), COALESCE(rm.wa_group_jid, ''),
		       `+strings.Replace(ledgerSenderExpr, "%s", "NULL", 1)+`,
		       split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1),
		       COALESCE(t.contact_id, 0), COALESCE(c.canonical_name, t.raw_name, ''),
		       COALESCE(t.amount, 0)::float8, t.is_cash, COALESCE(t.card_to, ''), COALESCE(t.collector, ''),
		       t.dup_pending, t.ignored, COALESCE(rm.deleted, false),
		       t.tx_date::timestamptz, COALESCE(rm.received_at, t.tx_date::timestamptz)
		FROM transactions t
		LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		LEFT JOIN contacts c ON c.id = t.contact_id
		LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone, ''), rm.sender_jid, ''), '@', 1)
		WHERE ((t.tx_date >= $1::timestamptz AND t.tx_date < $2::timestamptz) OR (rm.received_at >= $1 AND rm.received_at < $2))
		  AND ($3 = '' OR COALESCE(c.canonical_name, t.raw_name, '') ILIKE '%' || $3 || '%')`, from, to, client)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Text
	for rows.Next() {
		var t ledger.Text
		if err := rows.Scan(&t.ID, &t.RawID, &t.WaID, &t.Group, &t.Sender, &t.Phone, &t.ContactID, &t.Client,
			&t.Amount, &t.Cash, &t.Card, &t.Collector, &t.Held, &t.Ignored, &t.Deleted, &t.OpDate, &t.PostedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// groupSet — список групп как множество (nil/пусто — все).
func groupSet(groupJIDs []string) map[string]bool {
	var out map[string]bool
	for _, g := range groupJIDs {
		if g == "" {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[g] = true
	}
	return out
}

// ---- Отчёты на движке ledger ----

// SummaryForPeriod — сбор по клиентам за период [from, to) по дате операции.
func (d *DB) SummaryForPeriod(ctx context.Context, from, to time.Time, groupJIDs []string) ([]ContactSummary, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	return summarize(book.Counted(ledger.Filter{From: from, To: to, Groups: groupSet(groupJIDs)})), nil
}

// SummaryForPeriodByChat — сбор по дате присылки в чат; чеки с датой операции
// вне месяца периода [monthStart, monthEnd) в сумму не идут (ChecksPostedOldOperation).
func (d *DB) SummaryForPeriodByChat(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string) ([]ContactSummary, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	return summarize(book.Counted(ledger.Filter{From: from, To: to, ByChat: true, MonthStart: monthStart, MonthEnd: monthEnd, Groups: groupSet(groupJIDs)})), nil
}

func sortByOp(ps []*ledger.Payment) {
	sort.SliceStable(ps, func(i, j int) bool {
		if !ps[i].OpDate.Equal(ps[j].OpDate) {
			return ps[i].OpDate.Before(ps[j].OpDate)
		}
		return ps[i].Key < ps[j].Key
	})
}

func summarize(ps []*ledger.Payment) []ContactSummary {
	sortByOp(ps)
	byName := map[string]*ContactSummary{}
	var order []string
	for _, p := range ps {
		key := nameKey(p.Client)
		cs, ok := byName[key]
		if !ok {
			cs = &ContactSummary{CanonicalName: p.Client, ByCard: map[string]float64{}}
			byName[key] = cs
			order = append(order, key)
		}
		cs.Total += p.Amount
		cs.Count++
		cs.ByCard[p.Card()] += p.Amount
	}
	out := make([]ContactSummary, 0, len(order))
	for _, k := range order {
		out = append(out, *byName[k])
	}
	return out
}

// CardTotals — на чьи карты сколько пришло за период (по дате операции): все
// деньги, пришедшие на карту, в том числе чеки, где клиент ещё не назван.
func (d *DB) CardTotals(ctx context.Context, from, to time.Time, groupJIDs []string) ([]CardTotal, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	f := ledger.Filter{From: from, To: to, Groups: groupSet(groupJIDs)}
	byCard := map[string]*CardTotal{}
	var order []string
	add := func(c *ledger.Check, amount float64) {
		card := c.CardOwner
		if card == "" {
			card = "не распознано"
		}
		key := nameKey(card)
		ct, ok := byCard[key]
		if !ok {
			ct = &CardTotal{CardOwner: card, Bank: c.CardBank}
			byCard[key] = ct
			order = append(order, key)
		}
		if ct.Bank == "" {
			ct.Bank = c.CardBank
		}
		ct.Count++
		ct.Total += amount
	}
	pays := book.Counted(f)
	sortByOp(pays)
	for _, p := range pays {
		if m := p.Main(); m != nil {
			add(m, p.Amount)
		}
	}
	// Непроверенные чеки: деньги на карту пришли, клиент ещё не назван. Один
	// чек, присланный в две группы, — один раз.
	seen := map[*ledger.Check]bool{}
	for _, e := range book.Left(f) {
		if e.Check != nil && e.Reason == ledger.ReasonReview && e.Check.Amount > 0 && !seen[e.Root()] {
			seen[e.Root()] = true
			add(e.Check, e.Check.Amount)
		}
	}
	out := make([]CardTotal, 0, len(order))
	for _, k := range order {
		out = append(out, *byCard[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out, nil
}

// SenderStats — кто сколько собрал (чеки и наличка/тексты) по дате операции.
func (d *DB) SenderStats(ctx context.Context, from, to time.Time, groupJIDs []string, senderQuery string) ([]SenderStat, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	return senderStats(book.Counted(ledger.Filter{From: from, To: to, Groups: groupSet(groupJIDs)}), senderQuery), nil
}

// SenderStatsByChat — то же по дате присылки в чат.
func (d *DB) SenderStatsByChat(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string, senderQuery string) ([]SenderStat, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	return senderStats(book.Counted(ledger.Filter{From: from, To: to, ByChat: true, MonthStart: monthStart, MonthEnd: monthEnd, Groups: groupSet(groupJIDs)}), senderQuery), nil
}

func senderStats(ps []*ledger.Payment, q string) []SenderStat {
	q = strings.ToLower(strings.TrimSpace(q))
	type key struct{ name, phone string }
	by := map[key]*SenderStat{}
	var order []key
	sortByOp(ps)
	for _, p := range ps {
		name, phone := p.Sender()
		if q != "" && !strings.Contains(strings.ToLower(name), q) && !strings.Contains(phone, q) {
			continue
		}
		k := key{name, phone}
		st, ok := by[k]
		if !ok {
			st = &SenderStat{Name: name, Phone: phone}
			by[k] = st
			order = append(order, k)
		}
		st.Count++
		st.Total += p.Amount
	}
	out := make([]SenderStat, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}

// ReceiptsLedger — журнал платежей за период: по строке на платёж.
func (d *DB) ReceiptsLedger(ctx context.Context, from, to time.Time, person string, groupJIDs []string, limit int) ([]LedgerRow, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	ps := book.Counted(ledger.Filter{From: from, To: to, Groups: groupSet(groupJIDs)})
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].OpDate.After(ps[j].OpDate) })
	q := strings.ToLower(strings.TrimSpace(person))
	var out []LedgerRow
	for _, p := range ps {
		if q != "" && !strings.Contains(strings.ToLower(p.Client), q) {
			continue
		}
		r := LedgerRow{Client: p.Client, Amount: p.Amount, TxDate: p.OpDate, GroupJID: p.HomeGroup()}
		if m := p.Main(); m != nil {
			r.Kind, r.SubmittedBy, r.Collector, r.Bank, r.CardOwner = "чек", m.Sender, m.Collector, m.Bank, m.CardOwner
		} else {
			t := p.Texts[0]
			r.Kind = "перевод"
			if t.Cash {
				r.Kind = "наличка"
			}
			r.SubmittedBy, r.Collector, r.Bank = t.Sender, t.Collector, t.Card
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// PersonReport — по людям, чьё имя содержит запрос: чеки и текстовые платежи,
// посчитанные так же, как в сборе. До 5 человек, крупнейшие первыми.
func (d *DB) PersonReport(ctx context.Context, personQuery string, from, to *time.Time, groupJID string) ([]PersonStats, error) {
	f, t := time.Date(2000, 1, 1, 0, 0, 0, 0, time.Local), time.Now().AddDate(1, 0, 0)
	if from != nil {
		f = *from
	}
	if to != nil {
		t = *to
	}
	book, err := d.LoadBook(ctx, f, t, personQuery)
	if err != nil {
		return nil, err
	}
	var groups map[string]bool
	if groupJID != "" {
		groups = map[string]bool{groupJID: true}
	}
	q := strings.ToLower(strings.TrimSpace(personQuery))
	by := map[string]*PersonStats{}
	var order []string
	ps := book.Counted(ledger.Filter{From: f, To: t, Groups: groups})
	sortByOp(ps)
	for _, p := range ps {
		if q != "" && !strings.Contains(strings.ToLower(p.Client), q) {
			continue
		}
		k := ClientKeyOf(p.Client)
		st, ok := by[k]
		if !ok {
			st = &PersonStats{Name: p.Client}
			by[k] = st
			order = append(order, k)
		}
		if p.Main() != nil {
			st.ReceiptCount++
			st.ReceiptTotal += p.Amount
		} else {
			st.PaymentCount++
			st.PaymentTotal += p.Amount
		}
		op := p.OpDate
		if st.FirstOp == nil || op.Before(*st.FirstOp) {
			st.FirstOp = &op
		}
		if st.LastOp == nil || op.After(*st.LastOp) {
			o := op
			st.LastOp = &o
		}
	}
	out := make([]PersonStats, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ReceiptTotal+out[i].PaymentTotal > out[j].ReceiptTotal+out[j].PaymentTotal
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out, nil
}

// ClientKeyOf — ключ клиента (как в движке подсчёта).
func ClientKeyOf(s string) string { return ledger.ClientKey(s) }

// ReportExclusionsBasis — что НЕ вошло в сбор за период и почему (по дате
// операции или, byChat, по дате присылки).
func (d *DB) ReportExclusionsBasis(ctx context.Context, from, to time.Time, groupJIDs []string, byChat bool) (ReportExclusions, error) {
	var ex ReportExclusions
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return ex, err
	}
	f := ledger.Filter{From: from, To: to, ByChat: byChat, Groups: groupSet(groupJIDs)}
	// Тот же непроверенный чек в нескольких группах — один раз (копии уже
	// посчитанного чека движок относит к его платежу).
	seen := map[*ledger.Check]bool{}
	for _, e := range book.Left(f) {
		switch e.Reason {
		case ledger.ReasonReview:
			c := e.Check
			if c.Amount <= 0 || seen[e.Root()] {
				continue
			}
			seen[e.Root()] = true
			ex.NeedsReviewCount++
			ex.NeedsReviewSum += c.Amount
		case ledger.ReasonHeld:
			if e.Text.Cash {
				ex.HeldCashCount++
				ex.HeldCashSum += e.Text.Amount
			}
		case ledger.ReasonTwin, ledger.ReasonSplit:
			ex.TextDupCount++
			ex.TextDupSum += e.Text.Amount
		}
	}
	return ex, nil
}

// ChecksPostedOldOperation — режим по дате чата: чеки, присланные в период, с
// датой операции вне месяца периода (в сумму не входят).
func (d *DB) ChecksPostedOldOperation(ctx context.Context, from, to, monthStart, monthEnd time.Time, groupJIDs []string, limit int) ([]OldCheck, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return nil, err
	}
	ps := book.OldChecks(ledger.Filter{From: from, To: to, ByChat: true, MonthStart: monthStart, MonthEnd: monthEnd, Groups: groupSet(groupJIDs)})
	sortByOp(ps)
	var out []OldCheck
	for _, p := range ps {
		m := p.Main()
		out = append(out, OldCheck{Name: p.Client, Amount: p.Amount, TxDate: p.OpDate, SubmittedBy: m.Sender})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Book — книга платежей для отчёта «из чего сложен сбор».
func (d *DB) Book(ctx context.Context, from, to time.Time, client string) (*ledger.Book, error) {
	return d.LoadBook(ctx, from, to, client)
}

// LegacyTotal — сбор за период старым способом (SQL по каждой таблице), для
// сравнения с движком при диагностике.
func (d *DB) LegacyTotal(ctx context.Context, from, to time.Time, groupJIDs []string) (float64, error) {
	ss, err := d.legacySummaryForPeriod(ctx, from, to, groupJIDs)
	if err != nil {
		return 0, err
	}
	var t float64
	for _, s := range ss {
		t += s.Total
	}
	return t, nil
}
