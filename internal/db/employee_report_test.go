package db

import (
	"context"
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"
)

func TestEmployeeReportPostingDatesIdentityAndKinds(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	to := from.AddDate(0, 0, 1)
	old := from.AddDate(0, -1, 0)
	checks := []ledger.Check{
		{ID: 1, WaID: "one", Group: "sb", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Эстамиров Ибрагим Муслимович", Confirmed: true, Amount: 50000.25, OpDate: old, PostedAt: from.Add(time.Minute), Doc: "TEST-DOC-000001", Payer: "Расул Рахманович Ц.", Collector: "Сафаи"},
		{ID: 2, WaID: "copy", Group: "main", Sender: "Расул", Phone: "79280000002", ContactID: 1, Client: "Эстамиров Ибрагим Муслимович", Confirmed: true, Amount: 50000.25, OpDate: old, PostedAt: from.Add(2 * time.Minute), Doc: "TEST-DOC-000001"},
		{ID: 3, Group: "sb", Sender: "", Phone: "79280000001", ContactID: 2, Client: "Арсаев Магомед", Confirmed: true, Amount: 7200.50, OpDate: from, PostedAt: from.Add(3 * time.Minute), Doc: "TEST-DOC-000002"},
		{ID: 4, Group: "sb", Sender: "Мансур", Phone: "79280000001", Review: true, Amount: 1000, OpDate: from, PostedAt: from.Add(4 * time.Minute), Doc: "TEST-DOC-000003", Client: "Получатель Карты", Payer: "Банковский Плательщик"},
		{ID: 5, Group: "sb", Sender: "Мансур", Phone: "79280000001", Review: true, Amount: 0, OpDate: from, PostedAt: from.Add(5 * time.Minute)},
		{ID: 6, Group: "sb", Sender: "Мансур", Phone: "79280000001", ContactID: 3, Client: "Удалённый Клиент", Confirmed: true, Amount: 9000, OpDate: from, PostedAt: from.Add(6 * time.Minute), Deleted: true},
	}
	texts := []ledger.Text{
		{ID: 1, Group: "sb", Sender: "Мансур", Phone: "79280000001", ContactID: 4, Client: "Темирбулатов Абдулла", Amount: 17000, Cash: true, Collector: "МБ", OpDate: from, PostedAt: from.Add(7 * time.Minute)},
		{ID: 2, Group: "sb", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Эстамиров Ибрагим Муслимович", Amount: 50000.25, OpDate: from, PostedAt: from.Add(90 * time.Second)},
	}
	book := ledger.Build(checks, texts)
	report := employeeReport(book, from, to, EmployeeReportOptions{Sender: "Мансур"})
	if len(report.Stats) != 1 {
		t.Fatalf("stats: %+v", report.Stats)
	}
	s := report.Stats[0]
	if s.Checks != 2 || s.ChecksSum != 57200.75 || s.Cash != 1 || s.CashSum != 17000 || s.Total != 74200.75 || s.PendingChecks != 2 || s.PendingSum != 1000 || s.UnknownAmounts != 1 {
		t.Fatalf("wrong totals: %+v; rows=%+v", s, report.Rows)
	}
	if len(report.Rows) != 5 {
		t.Fatalf("lost/duplicated rows: %+v", report.Rows)
	}
	for _, r := range report.Rows {
		if r.Status != "counted" && r.Client != "" {
			t.Errorf("bank recipient became client: %+v", r)
		}
	}
	checksOnly := employeeReport(book, from, to, EmployeeReportOptions{Sender: "89280000001", Kind: "checks"})
	if len(checksOnly.Rows) != 4 || checksOnly.Stats[0].Cash != 0 || checksOnly.Stats[0].Total != 57200.75 {
		t.Fatalf("checks filter: %+v", checksOnly)
	}
	byOperation := employeeReport(book, from, to, EmployeeReportOptions{Sender: "Мансур", DateBasis: "operation", Kind: "checks"})
	if len(byOperation.Stats) != 1 || byOperation.Stats[0].Checks != 1 || byOperation.Stats[0].Total != 7200.50 {
		t.Fatalf("operation date: %+v", byOperation)
	}
	collector := employeeReport(book, from, to, EmployeeReportOptions{Sender: "МБ", Attribution: "collector", Kind: "cash"})
	if len(collector.Rows) != 1 || collector.Rows[0].EmployeePhone != "" || collector.Rows[0].SubmittedBy != "Мансур" {
		t.Fatalf("collector confused with sender: %+v", collector)
	}
	local := employeeReport(book, from, to, EmployeeReportOptions{Groups: []string{"main"}, Sender: "Расул", Kind: "checks"})
	if len(local.Rows) != 1 || local.Stats[0].Checks != 1 {
		t.Fatalf("group-specific poster: %+v", local)
	}
	otherPeriod := employeeReport(book, to, to.AddDate(0, 0, 1), EmployeeReportOptions{})
	if len(otherPeriod.Rows) != 0 {
		t.Fatalf("period boundary: %+v", otherPeriod)
	}
}

func TestEmployeeReportFullPhonesAndRussianAliases(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var texts []ledger.Text
	for i, phone := range []string{"79280000001", "89280000001", "9280000001", "12025550123", "442025550123"} {
		texts = append(texts, ledger.Text{ID: i + 1, Group: "g", Sender: "Мансур", Phone: phone, ContactID: i + 1, Client: "Клиент", Amount: float64(i+1) + 0.01, Cash: true, OpDate: day, PostedAt: day.Add(time.Duration(i) * time.Minute)})
	}
	book := ledger.Build(nil, texts)
	r := employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{Sender: "+7 (928) 000-00-01"})
	if len(r.Stats) != 1 || r.Stats[0].Count != 3 || r.Stats[0].Total != 6.03 || r.Stats[0].Phone != "79280000001" {
		t.Fatalf("one Russian phone split into workers: %+v", r)
	}
	for _, q := range []string{"12025550123", "+1 (202) 555-0123"} {
		r = employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{Sender: q})
		if len(r.Stats) != 1 || r.Stats[0].Phone != "12025550123" || r.Stats[0].Total != 4.01 {
			t.Fatalf("international suffix collision for %s: %+v", q, r)
		}
	}
}

func TestEmployeeReportCollectorNameDoesNotProvePosterIdentity(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	book := ledger.Build(nil, []ledger.Text{
		{ID: 1, Group: "g", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Клиент Один", Amount: 1000, Cash: true, Collector: "Мансур", OpDate: day, PostedAt: day},
		{ID: 2, Group: "g", Sender: "Мансур", Phone: "79280000002", ContactID: 2, Client: "Клиент Два", Amount: 2000, Cash: true, Collector: "Мансур", OpDate: day, PostedAt: day},
	})
	r := employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{Attribution: "collector", Sender: "Мансур"})
	if len(r.Stats) != 2 {
		t.Fatalf("same collector labels silently merged into one person: %+v", r)
	}
	for _, row := range r.Rows {
		if row.EmployeePhone != "" || row.AttributionReason != "collector_name" || row.RawSenderPhone == "" {
			t.Fatalf("collector inherited a poster phone: %+v", row)
		}
	}
	byPhone := employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{Attribution: "collector", Sender: "79280000001"})
	if len(byPhone.Rows) != 0 {
		t.Fatalf("full-phone filter attributed named collector without proof: %+v", byPhone)
	}
}

func TestEmployeeReportUnknownLIDIsNotAPhone(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	book := ledger.Build(nil, []ledger.Text{{ID: 1, RawID: 1, Group: "g", Sender: "Мансур", Phone: "123456789012345", ContactID: 1, Client: "Клиент", Amount: 1000, Cash: true, OpDate: day, PostedAt: day}})
	opts := EmployeeReportOptions{sources: map[int]employeeSource{1: {SenderIdentity: SenderIdentity{ActorJID: "123456789012345@lid"}, Name: "Мансур", GroupJID: "g@g.us"}}}
	r := employeeReport(book, day, day.AddDate(0, 0, 1), opts)
	if len(r.Stats) != 1 || r.Stats[0].Phone != "" || r.Rows[0].EmployeePhone != "" || r.Rows[0].IdentityKey != "jid:123456789012345@lid" {
		t.Fatalf("LID fabricated as a phone: %+v", r)
	}
	opts.Sender = "123456789012345"
	if r := employeeReport(book, day, day.AddDate(0, 0, 1), opts); len(r.Rows) != 0 {
		t.Fatalf("same digits are not observed PN↔LID proof: %+v", r)
	}
}

func TestEmployeeReportReviewConflictsAndUnknownCopies(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	base := ledger.Check{ID: 1, Group: "a", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Клиент Один", Confirmed: true, Amount: 10000, OpDate: day, PostedAt: day, Doc: "CONFLICT-EMPLOYEE-001"}
	copy := base
	copy.ID = 2
	copy.Group = "b"
	copy.Sender = "Умар"
	copy.Phone = "79280000002"
	copy.ContactID = 2
	copy.Client = "Клиент Два"
	copy.PostedAt = day.Add(time.Minute)
	book := ledger.Build([]ledger.Check{base, copy}, nil)
	r := employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{})
	if len(r.Rows) != 1 || r.Rows[0].Status == "counted" || r.Rows[0].Client != "" || r.Stats[0].Count != 0 || r.Stats[0].PendingChecks != 1 || r.Stats[0].PendingSum != 10000 {
		t.Fatalf("conflicting clients attributed to first client: %+v", r)
	}
	local := employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{Groups: []string{"b"}})
	if len(local.Rows) != 1 || local.Rows[0].Client != "" || local.Rows[0].SubmittedBy != "Умар" {
		t.Fatalf("local group inherited foreign payer: %+v", local)
	}
	base.Amount, copy.Amount = 0, 0
	base.Review, copy.Review = true, true
	book = ledger.Build([]ledger.Check{base, copy}, nil)
	r = employeeReport(book, day, day.AddDate(0, 0, 1), EmployeeReportOptions{})
	if len(r.Rows) != 1 || r.Stats[0].PendingChecks != 1 || r.Stats[0].UnknownAmounts != 1 || r.Stats[0].PendingSum != 0 {
		t.Fatalf("unknown same-doc copies counted twice: %+v", r)
	}
}

func TestEmployeeReportObservedIdentityAndPrivateUpload(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("employee-identity")
	group += "@g.us"
	at := time.Now()
	from := at.Add(-time.Hour)
	to := at.Add(time.Hour)
	contact, err := d.GetOrCreateContact(ctx, "Клиент "+group)
	if err != nil {
		t.Fatal(err)
	}
	addCash := func(wa, jid, phone, name string, amount float64) {
		raw, _, err := d.SaveRawMessage(ctx, wa, group, jid, phone, name, "Клиент наличка", false, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.InsertTransaction(ctx, TransactionInput{RawMessageID: raw, ContactID: contact, RawName: "Клиент " + group, Amount: amount, IsCash: true, TxDate: at}); err != nil {
			t.Fatal(err)
		}
	}
	addCash(group+"lid", "123456789012345@lid", "", "Мансур", 5000)
	addCash(group+"pn", "79280000101@s.whatsapp.net", "79280000101", "Mansur", 7000)
	if err := d.RememberSenderIdentity(ctx, "123456789012345@lid", "79280000101@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	r, err := d.EmployeePayments(ctx, from, to, EmployeeReportOptions{Groups: []string{group}, Sender: "Мансур", Kind: "cash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Stats) != 1 || r.Stats[0].Count != 2 || r.Stats[0].Total != 12000 || r.Stats[0].Phone != "79280000101" {
		t.Fatalf("observed PN/LID split or lost alias: %+v", r)
	}
	owner := "79280000102"
	if err := d.SetPhoneOwner(ctx, owner, "Владелец"); err != nil {
		t.Fatal(err)
	}
	raw, _, err := d.SaveRawMessage(ctx, group+"private", owner+"@s.whatsapp.net", owner+"@s.whatsapp.net", owner, "Владелец", "Клиент "+group, true, "", at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.pool.Exec(ctx, `INSERT INTO bank_receipts(raw_message_id,group_jid,contact_id,client_confirmed,recipient_raw,submitted_by,amount,tx_date,sender_raw) VALUES($1,$2,$3,true,$4,'Расул',9000,$5,'Банковский Плательщик')`, raw, group, contact, "Клиент "+group, at)
	if err != nil {
		t.Fatal(err)
	}
	r, err = d.EmployeePayments(ctx, from, to, EmployeeReportOptions{Groups: []string{group}, Sender: "Расул", Kind: "checks"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0].EmployeePhone != "" || r.Rows[0].AttributionReason != "submitted_label" || r.Rows[0].RawSenderPhone != owner || r.Rows[0].BankPayer != "Банковский Плательщик" || r.Stats[0].Total != 9000 {
		t.Fatalf("upload owner/worker/payer mixed: %+v", r)
	}
	r, err = d.EmployeePayments(ctx, from, to, EmployeeReportOptions{Groups: []string{group}, Sender: owner, Kind: "checks"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("manual worker label inherited uploader phone: %+v", r)
	}
}

func TestEmployeeReportUncertainDatesRemainVisibleForEachCandidatePeriod(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := ledger.Check{ID: 1, Group: "a", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Клиент", Confirmed: true, Amount: 10000, Doc: "EMPLOYEE-DATE-CONFLICT", OpDate: day, PostedAt: day.Add(time.Hour)}
	b := a
	b.ID = 2
	b.Group = "b"
	b.OpDate = day.AddDate(0, 0, 1)
	b.PostedAt = day.Add(2 * time.Hour)
	book := ledger.Build([]ledger.Check{a, b}, nil)
	for i := 0; i < 2; i++ {
		from := day.AddDate(0, 0, i)
		r := employeeReport(book, from, from.AddDate(0, 0, 1), EmployeeReportOptions{DateBasis: "operation"})
		if len(r.Rows) != 1 || r.Rows[0].Status == "counted" || !r.Rows[0].OperationDateUncertain || r.Stats[0].PendingChecks != 1 {
			t.Fatalf("candidate operation day %d hid uncertain receipt: %+v", i, r)
		}
	}
}

func TestEmployeeReportDictatedSourceIsNotAnEmployee(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	book := ledger.Build(nil, []ledger.Text{
		{ID: 1, RawID: 1, Group: "g", Sender: "Клиент Один", ContactID: 1, Client: "Клиент Один", Amount: 1000, Cash: true, Collector: "Мансур", OpDate: day, PostedAt: day},
		{ID: 2, RawID: 2, Group: "g", Sender: "Клиент Два", ContactID: 2, Client: "Клиент Два", Amount: 2000, Cash: true, Collector: "Умар", OpDate: day, PostedAt: day},
	})
	opts := EmployeeReportOptions{sources: map[int]employeeSource{
		1: {SenderIdentity: SenderIdentity{ActorJID: "dictated@c.us"}, Name: "Клиент Один", GroupJID: "g@g.us"},
		2: {SenderIdentity: SenderIdentity{ActorJID: "dictated@c.us"}, Name: "Клиент Два", GroupJID: "g@g.us"},
	}}
	r := employeeReport(book, day, day.AddDate(0, 0, 1), opts)
	if len(r.Stats) != 2 {
		t.Fatalf("synthetic actor merged unrelated named sources: %+v", r)
	}
	for _, row := range r.Rows {
		if row.Employee != "" || row.EmployeePhone != "" || row.AttributionReason != "unknown_sender" {
			t.Fatalf("payer assigned as posting employee: %+v", row)
		}
	}
	opts.Attribution = "collector"
	r = employeeReport(book, day, day.AddDate(0, 0, 1), opts)
	if len(r.Stats) != 2 {
		t.Fatalf("named collectors merged through synthetic source: %+v", r)
	}
	for _, row := range r.Rows {
		if row.AttributionReason != "collector_name" || row.EmployeePhone != "" {
			t.Fatalf("collector lacks uncertainty label: %+v", row)
		}
	}
}

func TestEmployeeReportUsesReceiptPostingTimeInsteadOfCaption(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	book := ledger.Build([]ledger.Check{{ID: 1, Group: "g", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Арсаев Магомед", Confirmed: true, Amount: 7200, OpDate: from.Add(-time.Hour), PostedAt: from.Add(time.Hour), Doc: "POSTED-TEST-001"}}, []ledger.Text{{ID: 1, Group: "g", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Арсаев Магомед", Amount: 7200, OpDate: from.Add(-time.Hour), PostedAt: from.Add(-time.Minute)}})
	r := employeeReport(book, from, from.AddDate(0, 0, 1), EmployeeReportOptions{Kind: "checks"})
	if len(r.Rows) != 1 || r.Stats[0].Checks != 1 {
		t.Fatalf("receipt lost because caption posted yesterday: %+v", r)
	}
}

// Regression for the November 1 Сбор СБ ЧЕКИ report: the two 30,000 receipts
// belong to different clients and different bank operations, and cash is
// separate. A client mentioned in a follow-up is not the posting employee.
func TestEmployeeReportNovemberFirstScreenshot(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	checks := []ledger.Check{
		{ID: 1, Group: "sb", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Эстамиров Ибрагим Муслимович", Confirmed: true, Amount: 50000, OpDate: day, PostedAt: day.Add(10 * time.Minute), Doc: "NOV1-ESTAMIROV-001"},
		{ID: 2, Group: "sb", Sender: "Мовлид", Phone: "79280000002", ContactID: 2, Client: "Хасуев Ислам", Confirmed: true, Amount: 30000, OpDate: day.Add(time.Hour), PostedAt: day.Add(2*time.Hour + 54*time.Minute), Doc: "A630-NOV1-HASUEV-002", Bank: "ВТБ", Printed: "Тамила"},
		{ID: 3, Group: "sb", Sender: "~ M", Phone: "79280000003", ContactID: 3, Client: "Ханатаев Акраман", Confirmed: true, Amount: 30000, OpDate: day.Add(12 * time.Hour), PostedAt: day.Add(14*time.Hour + 24*time.Minute), Doc: "1000000006086701079", Auth: "024307", Bank: "Сбер", Payer: "Элина Зилаудиевна Г.", Printed: "Милана Нажудовна К."},
		{ID: 4, Group: "sb", Sender: "Мухаммед", Phone: "79280000004", ContactID: 4, Client: "Мусаев Рахман", Confirmed: true, Amount: 65000, OpDate: day.Add(19 * time.Hour), PostedAt: day.Add(20*time.Hour + 33*time.Minute), Doc: "NOV1-MUSAEV-004"},
	}
	texts := []ledger.Text{{ID: 1, Group: "sb", Sender: "Сафаи", Phone: "79280000005", ContactID: 5, Client: "Идрисов Абузар", Amount: 15000, Cash: true, OpDate: day, PostedAt: day.Add(13*time.Hour + 18*time.Minute)}}
	book := ledger.Build(checks, texts)
	to := day.AddDate(0, 0, 1)
	full := employeeReport(book, day, to, EmployeeReportOptions{Groups: []string{"sb"}})
	var sum, cash float64
	var count int
	for _, s := range full.Stats {
		count += s.Checks
		sum += s.ChecksSum
		cash += s.CashSum
	}
	if len(full.Rows) != 5 || count != 4 || sum != 175000 || cash != 15000 {
		t.Fatalf("lost receipt or mixed cash into checks: %+v", full)
	}
	for _, query := range []string{"Ханатаев Акраман", "акраман ХАНАТАЕВ", "Ханатаев"} {
		r := employeeReport(book, day, to, EmployeeReportOptions{Groups: []string{"sb"}, Client: query, Kind: "checks"})
		if len(r.Rows) != 1 || r.Rows[0].Client != "Ханатаев Акраман" || r.Rows[0].Employee != "~ M" || r.Rows[0].BankPayer != "Элина Зилаудиевна Г." || r.Stats[0].ChecksSum != 30000 {
			t.Fatalf("client %q mixed with worker, bank payer or other 30,000 receipt: %+v", query, r)
		}
	}
	for _, query := range []string{"Элина Зилаудиевна Г.", "Милана Нажудовна К.", "Хан", "Ханатаев Ислам"} {
		if r := employeeReport(book, day, to, EmployeeReportOptions{Client: query}); len(r.Rows) != 0 {
			t.Fatalf("unrelated/incomplete client query %q matched: %+v", query, r)
		}
	}
	if r := employeeReport(book, day, to, EmployeeReportOptions{Sender: "Ханатаев Акраман"}); len(r.Rows) != 0 {
		t.Fatalf("client was treated as posting employee: %+v", r)
	}
	if r := employeeReport(book, day, to, EmployeeReportOptions{Sender: "Мухаммед", Client: "Ханатаев Акраман"}); len(r.Rows) != 0 {
		t.Fatalf("missing client reassigned to Musaev's posting employee: %+v", r)
	}
	checks[2].Confirmed, checks[2].Review = false, true
	checks[2].Client = "Милана Нажудовна К."
	book = ledger.Build(checks, texts)
	if r := employeeReport(book, day, to, EmployeeReportOptions{Client: "Милана Нажудовна К."}); len(r.Rows) != 0 {
		t.Fatalf("unconfirmed bank recipient treated as captioned client: %+v", r)
	}
	unconfirmed := employeeReport(book, day, to, EmployeeReportOptions{Kind: "checks"})
	if len(unconfirmed.Rows) != 4 {
		t.Fatalf("unconfirmed fourth receipt silently omitted: %+v", unconfirmed)
	}
	var pending int
	for _, s := range unconfirmed.Stats {
		pending += s.PendingChecks
	}
	if pending != 1 {
		t.Fatalf("fourth receipt should remain visibly pending: %+v", unconfirmed)
	}
}
