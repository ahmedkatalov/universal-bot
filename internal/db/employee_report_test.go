package db

import (
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

func TestEmployeeReportUsesReceiptPostingTimeInsteadOfCaption(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	book := ledger.Build([]ledger.Check{{ID: 1, Group: "g", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Арсаев Магомед", Confirmed: true, Amount: 7200, OpDate: from.Add(-time.Hour), PostedAt: from.Add(time.Hour), Doc: "POSTED-TEST-001"}}, []ledger.Text{{ID: 1, Group: "g", Sender: "Мансур", Phone: "79280000001", ContactID: 1, Client: "Арсаев Магомед", Amount: 7200, OpDate: from.Add(-time.Hour), PostedAt: from.Add(-time.Minute)}})
	r := employeeReport(book, from, from.AddDate(0, 0, 1), EmployeeReportOptions{Kind: "checks"})
	if len(r.Rows) != 1 || r.Stats[0].Checks != 1 {
		t.Fatalf("receipt lost because caption posted yesterday: %+v", r)
	}
}
