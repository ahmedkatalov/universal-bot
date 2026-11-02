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
	Client      string              // confirmed client from the caption, never a posting employee
	DateBasis   string              // chat (default), operation
	Attribution string              // submitter (default), collector
	Kind        string              // all (default), checks, cash, transfers
	names       map[string][]string // historical WhatsApp names of each stable identity
	labels      map[string]string
	sources     map[int]employeeSource
	origins     map[string]int
	submitted   map[int]string
}

type employeeSource struct {
	SenderIdentity
	Name, GroupJID, WaID string
}

type EmployeePayment struct {
	Key, Kind, Client, BankPayer   string
	Amount                         float64
	OperationAt, PostedAt          time.Time
	SubmittedBy, SenderPhone       string
	Collector, GroupJID            string
	Employee, EmployeePhone        string
	Status                         string // counted, review, no_amount, held
	IdentityKey, SenderIdentityKey string
	AttributionReason              string // empty: observed actor; collector_name/submitted_label: unverified label
	RawSenderName, RawSenderPhone  string
	ReviewReason                   string
	OperationDateUncertain         bool
}

type EmployeeStat struct {
	SenderStat
	PendingChecks, PendingCash, PendingTransfers int
	PendingSum                                   float64
	UnknownAmounts                               int
	IdentityKey, AttributionReason               string
	RawSenderName, RawSenderPhone                string
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
	if err := d.employeeSources(ctx, book, &opts); err != nil {
		return EmployeeReport{}, err
	}
	return employeeReport(book, from, to, opts), nil
}

// Identity comes from observed WhatsApp IDs, independently of printed bank
// names or a manually supplied historical worker label.
func (d *DB) employeeSources(ctx context.Context, book *ledger.Book, opts *EmployeeReportOptions) error {
	rawIDs, checkIDs, oldPhones, origins := map[int]bool{}, map[int]bool{}, map[string]bool{}, map[string]bool{}
	addCheck := func(c *ledger.Check) {
		rawIDs[c.RawID] = true
		checkIDs[c.ID] = true
		oldPhones[c.Phone] = true
		if c.FwdOrigin != "" {
			origins[c.FwdOrigin] = true
		}
	}
	addText := func(t *ledger.Text) { rawIDs[t.RawID] = true; oldPhones[t.Phone] = true }
	for _, p := range book.Payments {
		for _, c := range p.Checks {
			addCheck(c)
		}
		for _, t := range p.Texts {
			addText(t)
		}
	}
	for _, e := range book.Excluded {
		if e.Check != nil {
			addCheck(e.Check)
		}
		if e.Text != nil {
			addText(e.Text)
		}
	}
	var rawList, checkList []int
	var waList []string
	for id := range rawIDs {
		if id > 0 {
			rawList = append(rawList, id)
		}
	}
	for id := range checkIDs {
		if id > 0 {
			checkList = append(checkList, id)
		}
	}
	for id := range origins {
		waList = append(waList, id)
	}
	opts.sources = map[int]employeeSource{}
	opts.origins = map[string]int{}
	opts.submitted = map[int]string{}
	opts.labels = map[string]string{}
	opts.names = map[string][]string{}
	if len(rawList) > 0 || len(waList) > 0 {
		rows, err := d.pool.Query(ctx, `SELECT id,COALESCE(sender_name,''),COALESCE(wa_group_jid,''),COALESCE(wa_message_id,'') FROM raw_messages WHERE id=ANY($1::int[]) OR wa_message_id=ANY($2::text[])`, rawList, waList)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int
			var src employeeSource
			if err := rows.Scan(&id, &src.Name, &src.GroupJID, &src.WaID); err != nil {
				rows.Close()
				return err
			}
			opts.sources[id] = src
			opts.origins[src.WaID] = id
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rawList = nil
		for id := range opts.sources {
			rawList = append(rawList, id)
		}
		actors, err := d.RawSenderIdentities(ctx, rawList)
		if err != nil {
			return err
		}
		for id, identity := range actors {
			src := opts.sources[id]
			src.SenderIdentity = identity
			opts.sources[id] = src
			oldPhones[identity.Phone] = true
		}
	}
	if len(checkList) > 0 {
		rows, err := d.pool.Query(ctx, `SELECT id,submitted_by FROM bank_receipts WHERE id=ANY($1::int[]) AND COALESCE(submitted_by,'')<>''`, checkList)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			opts.submitted[id] = name
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	owners, err := d.ListPhoneOwners(ctx)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		key := employeeIdentity("", owner.Phone)
		opts.labels[key] = owner.Name
		opts.names[key] = append(opts.names[key], owner.Name)
	}
	// Include names learned outside the requested period. Observed LID links
	// backfill sender_phone; the decoder still rejects unresolved LIDs as phones.
	var phones []string
	for phone := range oldPhones {
		if phone == "" {
			continue
		}
		phones = append(phones, phone)
		if canonical := canonicalPhoneNumber(phone); canonical != "" {
			phones = append(phones, canonical)
			if len(canonical) == 11 && canonical[0] == '7' {
				phones = append(phones, "8"+canonical[1:], canonical[1:])
			}
		}
	}
	if len(phones) > 0 {
		rows, err := d.pool.Query(ctx, `SELECT MAX(id),sender_name FROM raw_messages
   WHERE split_part(COALESCE(NULLIF(sender_phone,''),sender_jid,''),'@',1)=ANY($1::text[]) AND COALESCE(sender_name,'')<>''
   GROUP BY split_part(COALESCE(NULLIF(sender_phone,''),sender_jid,''),'@',1),sender_name ORDER BY MAX(received_at) DESC,MAX(id) DESC`, phones)
		if err != nil {
			return err
		}
		type alias struct {
			id   int
			name string
		}
		var aliases []alias
		var aliasIDs []int
		for rows.Next() {
			var a alias
			if err := rows.Scan(&a.id, &a.name); err != nil {
				rows.Close()
				return err
			}
			if strings.IndexFunc(a.name, unicode.IsLetter) >= 0 {
				aliases = append(aliases, a)
				aliasIDs = append(aliasIDs, a.id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		identities, err := d.RawSenderIdentities(ctx, aliasIDs)
		if err != nil {
			return err
		}
		for _, a := range aliases {
			identity := identities[a.id]
			key := employeeActorKey(identity)
			if key != "" {
				opts.names[key] = append(opts.names[key], a.name)
			}
		}
	}
	return nil
}

func employeeActorKey(identity SenderIdentity) string {
	if phone := canonicalPhoneNumber(identity.Phone); phone != "" {
		return "phone:" + phone
	}
	if strings.HasSuffix(identity.ActorJID, "@lid") || strings.HasSuffix(identity.ActorJID, "@s.whatsapp.net") {
		return "jid:" + identity.ActorJID
	}
	return ""
}

func (opts EmployeeReportOptions) source(r *EmployeePayment, rawID, checkID int, origin string) {
	if id, ok := opts.origins[origin]; origin != "" && ok {
		rawID = id
	}
	src, known := opts.sources[rawID]
	r.SenderPhone = canonicalPhoneNumber(r.SenderPhone)
	key := ""
	if r.SenderPhone != "" {
		key = employeeIdentity(r.SubmittedBy, r.SenderPhone)
	}
	if known {
		key = employeeActorKey(src.SenderIdentity)
		r.SenderPhone = canonicalPhoneNumber(src.Phone)
		name := src.Name
		// Dictated/import helper IDs are not WhatsApp people. Their display
		// name may be the CLIENT, so it cannot identify a posting employee.
		if key == "" {
			name = ""
		}
		if strings.IndexFunc(name, unicode.IsLetter) < 0 {
			name = ""
		}
		if label := opts.labels[key]; key != "" && label != "" {
			name = label
		} else if key != "" && name == "" && len(opts.names[key]) > 0 {
			name = opts.names[key][0]
		}
		r.SubmittedBy = name
	}
	if key == "" {
		key = "unlinked:" + r.Key
		r.AttributionReason = "unknown_sender"
	}
	r.RawSenderName, r.RawSenderPhone = r.SubmittedBy, r.SenderPhone
	r.SenderIdentityKey, r.IdentityKey = key, key
	// Imported history inside a group has a genuine author. A worker label on
	// a private upload is separate evidence and cannot inherit the owner's phone.
	if label := opts.submitted[checkID]; label != "" && (!known || !strings.HasSuffix(src.GroupJID, "@g.us") || src.ActorJID == "bot-forward") {
		if !known {
			r.RawSenderName, r.RawSenderPhone = "", ""
		}
		r.SubmittedBy, r.SenderPhone = label, ""
		r.IdentityKey = "submitted-label:" + nameKey(label) + "|source:" + key
		r.AttributionReason = "submitted_label"
	}
}

func moneyAdd(a, b float64) float64 { return math.Round((a+b)*100) / 100 }

func employeeIdentity(name, phone string) string {
	if phone = canonicalPhoneNumber(phone); phone != "" {
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
	if digits := canonicalPhoneNumber(q); digits != "" && phoneMatch(digits, phone) {
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

func employeeClientMatches(query, client string) bool {
	if strings.TrimSpace(query) == "" {
		return true
	}
	words := strings.Fields(contactKey(query))
	clientWords := " " + contactKey(client) + " "
	if len(words) == 0 {
		return false
	}
	for _, word := range words {
		if !strings.Contains(clientWords, " "+word+" ") {
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
		// Client and posting employee are independent identities. In particular,
		// a printed bank payer/recipient is not evidence of a captioned client.
		if !employeeClientMatches(opts.Client, r.Client) {
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
		if (r.Employee == "" || r.Employee == r.EmployeePhone) && len(opts.names[r.IdentityKey]) > 0 {
			r.Employee = opts.names[r.IdentityKey][0]
		}
		if opts.Attribution == "collector" && r.Collector != "" {
			// Equal display names do not prove that the named collector is the
			// poster. Keep the label separate until an identity is confirmed.
			r.Employee, r.EmployeePhone = r.Collector, ""
			r.IdentityKey = "collector-label:" + nameKey(r.Collector) + "|source:" + r.SenderIdentityKey
			r.AttributionReason = "collector_name"
		}
		candidates = append(candidates, r)
	}
	checkRow := func(c *ledger.Check) EmployeePayment {
		client := ""
		if c.Confirmed {
			client = c.Client
		}
		r := EmployeePayment{
			Key: "c" + strconv.Itoa(c.ID), Kind: "checks", Client: client, BankPayer: c.Payer, Amount: c.Amount,
			OperationAt: c.OpDate, PostedAt: c.PostedAt, SubmittedBy: c.Sender,
			SenderPhone: c.Phone, Collector: c.Collector, GroupJID: c.Group,
		}
		opts.source(&r, c.RawID, c.ID, c.FwdOrigin)
		return r
	}
	textRow := func(t *ledger.Text) EmployeePayment {
		kind := "transfers"
		if t.Cash {
			kind = "cash"
		}
		r := EmployeePayment{
			Key: "t" + strconv.Itoa(t.ID), Kind: kind, Client: t.Client, Amount: t.Amount, OperationAt: t.OpDate,
			PostedAt: t.PostedAt, SubmittedBy: t.Sender, SenderPhone: t.Phone,
			Collector: t.Collector, GroupJID: t.Group,
		}
		opts.source(&r, t.RawID, 0, "")
		return r
	}
	for _, p := range book.Payments {
		if p.ReviewRequired() {
			continue
		}
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
	pendingReasons := map[*ledger.Check]string{}
	uncertainDates := map[*ledger.Check]bool{}
	for _, e := range book.Excluded {
		if e.Check == nil || (e.Reason != ledger.ReasonReview && e.Reason != ledger.ReasonNoAmount) || !inGroup(e.Check.Group) {
			continue
		}
		root, c := e.Root(), e.Check
		if e.Into != nil && e.Into.ReviewRequired() {
			var reasons []string
			if len(e.Into.Conflicts) > 0 {
				reasons = append(reasons, "в копиях чека указаны разные клиенты")
			}
			if e.Into.AmountConflict {
				reasons = append(reasons, "в копиях чека разные суммы; сумма требует уточнения")
			}
			if e.Into.DateConflict {
				reasons = append(reasons, "в копиях чека разные даты операции")
				uncertainDates[root] = true
				// The disputed date is only a candidate for period selection. Do
				// not hide this pending payment just because another copy's date
				// was selected as the representative first.
				if opts.DateBasis == "operation" && (c.OpDate.Before(from) || !c.OpDate.Before(to)) {
					continue
				}
			}
			pendingReasons[root] = strings.Join(reasons, "; ")
		}
		old := pending[root]
		if old == nil || c.PostedAt.Before(old.PostedAt) || (c.PostedAt.Equal(old.PostedAt) && c.ID < old.ID) {
			pending[root] = c
		}
	}
	for root, c := range pending {
		r := checkRow(c)
		r.ReviewReason = pendingReasons[root]
		r.OperationDateUncertain = uncertainDates[root]
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
		if r.AttributionReason == "" {
			for _, alias := range opts.names[r.IdentityKey] {
				matches = matches || employeeMatches(opts.Sender, alias, r.EmployeePhone)
			}
		}
		matches = matches || (strings.Contains(opts.Sender, "@") && r.IdentityKey == "jid:"+strings.TrimSpace(opts.Sender))
		if matches {
			matched[r.IdentityKey] = true
		}
	}
	result := EmployeeReport{}
	by := map[string]*EmployeeStat{}
	for _, r := range candidates {
		key := r.IdentityKey
		if !matched[key] {
			continue
		}
		result.Rows = append(result.Rows, r)
		s := by[key]
		if s == nil {
			s = &EmployeeStat{SenderStat: SenderStat{Name: r.Employee, Phone: r.EmployeePhone}, IdentityKey: r.IdentityKey, AttributionReason: r.AttributionReason, RawSenderName: r.RawSenderName, RawSenderPhone: r.RawSenderPhone}
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
