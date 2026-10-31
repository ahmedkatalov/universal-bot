package db

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"whatsapp-bot/internal/ledger"
)

// EmployeeReportOptions distinguishes posting a receipt from collecting cash.
// Chat reports include old bank operations: the question is when it was posted.
type EmployeeReportOptions struct {
	Groups      []string
	Sender      string
	DateBasis   string              // chat (default), operation
	Attribution string              // submitter (default), collector
	Kind        string              // all (default), checks, cash, transfers
	names       map[string][]string // historical WhatsApp names of each stable identity
}

type EmployeePayment struct {
	Key, Kind, Client, BankPayer string
	Amount                       float64
	OperationAt, PostedAt        time.Time
	SubmittedBy, SenderPhone     string
	Collector, GroupJID          string
	Employee, EmployeePhone      string
	Status                       string // counted, review, no_amount, held
}

type EmployeeStat struct {
	SenderStat
	PendingChecks, PendingCash, PendingTransfers int
	PendingSum                                   float64
	UnknownAmounts                               int
}

type EmployeeReport struct {
	Rows  []EmployeePayment
	Stats []EmployeeStat
}

func (d *DB) EmployeePayments(ctx context.Context, from, to time.Time, opts EmployeeReportOptions) (EmployeeReport, error) {
	book, err := d.LoadBook(ctx, from, to, "")
	if err != nil {
		return EmployeeReport{}, err
	}
	// Resolve names across saved history, including names learned AFTER the
	// requested period. A numbered old message still belongs to that employee.
	phones := map[string]bool{}
	for _, p := range book.Payments {
		for _, c := range p.Checks {
			phones[c.Phone] = true
		}
		for _, t := range p.Texts {
			phones[t.Phone] = true
		}
	}
	for _, e := range book.Excluded {
		if e.Check != nil {
			phones[e.Check.Phone] = true
		}
		if e.Text != nil {
			phones[e.Text.Phone] = true
		}
	}
	var ids []string
	for phone := range phones {
		if phone != "" {
			ids = append(ids, phone)
		}
	}
	opts.names = map[string][]string{}
	if len(ids) > 0 {
		rows, err := d.pool.Query(ctx, `
			SELECT split_part(COALESCE(NULLIF(sender_phone, ''), sender_jid, ''), '@', 1), sender_name
			FROM raw_messages
			WHERE split_part(COALESCE(NULLIF(sender_phone, ''), sender_jid, ''), '@', 1) = ANY($1::text[])
			  AND COALESCE(sender_name, '') <> ''
			GROUP BY 1, sender_name
			ORDER BY MAX(received_at) DESC, MAX(id) DESC`, ids)
		if err != nil {
			return EmployeeReport{}, err
		}
		for rows.Next() {
			var phone, name string
			if err := rows.Scan(&phone, &name); err != nil {
				rows.Close()
				return EmployeeReport{}, err
			}
			if strings.IndexFunc(name, unicode.IsLetter) >= 0 {
				opts.names[phone] = append(opts.names[phone], name)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return EmployeeReport{}, err
		}
	}
	return employeeReport(book, from, to, opts), nil
}

func moneyAdd(a, b float64) float64 { return math.Round((a+b)*100) / 100 }

func employeeIdentity(name, phone string) string {
	if phone != "" {
		return "phone:" + phone
	}
	return "name:" + nameKey(name)
}

// Query all aliases of an employee together, before filtering individual rows.
// Otherwise a changed/missing WhatsApp name would lose their earlier receipts.
func employeeMatches(q, name, phone string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return true
	}
	if digits := phoneDigits(q); digits != "" && phoneMatch(digits, phone) {
		return true
	}
	q, name = nameKey(q), nameKey(name)
	if q == "" {
		return false
	}
	for _, w := range strings.Fields(q) {
		if !strings.Contains(name, w) {
			return false
		}
	}
	return true
}

func employeeReport(book *ledger.Book, from, to time.Time, opts EmployeeReportOptions) EmployeeReport {
	groups := groupSet(opts.Groups)
	inGroup := func(g string) bool { return len(groups) == 0 || groups[g] }
	var candidates []EmployeePayment
	add := func(r EmployeePayment) {
		if !inGroup(r.GroupJID) {
			return
		}
		at := r.PostedAt
		if opts.DateBasis == "operation" {
			at = r.OperationAt
		}
		if at.Before(from) || !at.Before(to) {
			return
		}
		if opts.Kind != "" && opts.Kind != "all" && opts.Kind != r.Kind {
			return
		}
		r.Employee, r.EmployeePhone = r.SubmittedBy, r.SenderPhone
		if (r.Employee == "" || r.Employee == r.EmployeePhone) && len(opts.names[r.EmployeePhone]) > 0 {
			r.Employee = opts.names[r.EmployeePhone][0]
		}
		if opts.Attribution == "collector" && r.Collector != "" && !strings.EqualFold(r.Collector, r.SubmittedBy) {
			// The poster's phone must never be assigned to a different collector.
			r.Employee, r.EmployeePhone = r.Collector, ""
		}
		candidates = append(candidates, r)
	}
	checkRow := func(c *ledger.Check) EmployeePayment {
		client := ""
		if c.Confirmed {
			client = c.Client
		}
		return EmployeePayment{
			Key: "c" + strconv.Itoa(c.ID), Kind: "checks", Client: client, BankPayer: c.Payer, Amount: c.Amount,
			OperationAt: c.OpDate, PostedAt: c.PostedAt, SubmittedBy: c.Sender,
			SenderPhone: c.Phone, Collector: c.Collector, GroupJID: c.Group,
		}
	}
	textRow := func(t *ledger.Text) EmployeePayment {
		kind := "transfers"
		if t.Cash {
			kind = "cash"
		}
		return EmployeePayment{
			Key: "t" + strconv.Itoa(t.ID), Kind: kind, Client: t.Client, Amount: t.Amount, OperationAt: t.OpDate,
			PostedAt: t.PostedAt, SubmittedBy: t.Sender, SenderPhone: t.Phone,
			Collector: t.Collector, GroupJID: t.Group,
		}
	}
	for _, p := range book.Payments {
		var r EmployeePayment
		if p.Main() != nil {
			// Pick the first receipt in the requested groups BEFORE filtering by
			// date/person. Reposting it tomorrow or by another employee is not
			// a second collection. Text captions do not change its posting date.
			var first *ledger.Check
			for _, c := range p.Checks {
				if inGroup(c.Group) && (first == nil || c.PostedAt.Before(first.PostedAt) || (c.PostedAt.Equal(first.PostedAt) && c.ID < first.ID)) {
					first = c
				}
			}
			if first == nil {
				continue
			}
			r = checkRow(first)
			r.Client = p.Client // the canonical, confirmed client, including paired text
		} else {
			var first *ledger.Text
			for _, t := range p.Texts {
				if inGroup(t.Group) && (first == nil || t.PostedAt.Before(first.PostedAt) || (t.PostedAt.Equal(first.PostedAt) && t.ID < first.ID)) {
					first = t
				}
			}
			if first == nil {
				continue
			}
			r = textRow(first)
		}
		r.Key, r.Status, r.Amount = p.Key, "counted", p.Amount
		add(r)
	}
	// Review receipts and held cash are visible, with separate counts/sums.
	// Do not label bank sender/recipient as a confirmed client when no caption exists.
	pending := map[*ledger.Check]*ledger.Check{}
	for _, e := range book.Excluded {
		if e.Check == nil || (e.Reason != ledger.ReasonReview && e.Reason != ledger.ReasonNoAmount) || !inGroup(e.Check.Group) {
			continue
		}
		root, c := e.Root(), e.Check
		old := pending[root]
		if old == nil || c.PostedAt.Before(old.PostedAt) || (c.PostedAt.Equal(old.PostedAt) && c.ID < old.ID) {
			pending[root] = c
		}
	}
	for _, c := range pending {
		r := checkRow(c)
		r.Status = "review"
		if r.Amount <= 0 {
			r.Status = "no_amount"
		}
		add(r)
	}
	for _, e := range book.Excluded {
		if e.Text != nil && e.Reason == ledger.ReasonHeld {
			r := textRow(e.Text)
			r.Status = "held"
			add(r)
		}
	}
	matched := map[string]bool{}
	for _, r := range candidates {
		matches := employeeMatches(opts.Sender, r.Employee, r.EmployeePhone)
		for _, alias := range opts.names[r.EmployeePhone] {
			matches = matches || employeeMatches(opts.Sender, alias, r.EmployeePhone)
		}
		if matches {
			matched[employeeIdentity(r.Employee, r.EmployeePhone)] = true
		}
	}
	result := EmployeeReport{}
	by := map[string]*EmployeeStat{}
	for _, r := range candidates {
		key := employeeIdentity(r.Employee, r.EmployeePhone)
		if !matched[key] {
			continue
		}
		result.Rows = append(result.Rows, r)
		s := by[key]
		if s == nil {
			s = &EmployeeStat{SenderStat: SenderStat{Name: r.Employee, Phone: r.EmployeePhone}}
			by[key] = s
		}
		if s.Name == "" || s.Name == s.Phone {
			s.Name = r.Employee
		}
		if r.Status != "counted" {
			s.PendingSum = moneyAdd(s.PendingSum, math.Max(0, r.Amount))
			if r.Amount <= 0 {
				s.UnknownAmounts++
			}
			switch r.Kind {
			case "checks":
				s.PendingChecks++
			case "cash":
				s.PendingCash++
			default:
				s.PendingTransfers++
			}
			continue
		}
		s.Count++
		s.Total = moneyAdd(s.Total, r.Amount)
		switch r.Kind {
		case "checks":
			s.Checks++
			s.ChecksSum = moneyAdd(s.ChecksSum, r.Amount)
		case "cash":
			s.Cash++
			s.CashSum = moneyAdd(s.CashSum, r.Amount)
		default:
			s.Transfers++
			s.TransfersSum = moneyAdd(s.TransfersSum, r.Amount)
		}
	}
	for _, s := range by {
		result.Stats = append(result.Stats, *s)
	}
	sort.Slice(result.Stats, func(i, j int) bool {
		a, b := result.Stats[i], result.Stats[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return employeeIdentity(a.Name, a.Phone) < employeeIdentity(b.Name, b.Phone)
	})
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if !a.PostedAt.Equal(b.PostedAt) {
			return a.PostedAt.Before(b.PostedAt)
		}
		return a.Key < b.Key
	})
	return result
}
