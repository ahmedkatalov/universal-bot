package bot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
)

// The user's workflow: forwarded receipt + caption, a name in a separate
// message, unrelated bank payer, and cash collected by a different employee.
func TestEmployeeReportChatWorkflowAndResponses(t *testing.T) {
	ctx := context.Background()
	grp, other := e2eGroup("employee-sb"), e2eGroup("employee-other")
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Сбор СБ ЧЕКИ", other.String(): "Другая группа"}})
	worker := user("79280000101")
	for _, n := range []string{"Эстамиров Ибрагим Муслимович", "Арсаев Магомед", "Арсаханов Аслан", "Арсанукаев Магомед", "Темирбулатов Абдулла"} {
		e.aliases.Add(n, n)
	}
	old := time.Now().AddDate(0, -1, 0)
	e.sendReceipt(grp, worker, "Мансур", sberCheckOn(old, "50000.25", "10:00:00"), "Эстамиров Ибрагим Муслимович\n50т")
	ocr := sberCheck("7200.50", "11:00:00") + "\nФИО отправителя\nРасул Рахманович Ц."
	e.sendReceipt(grp, worker, "Мансур", ocr, "Арсаев Магомед 7200.50")
	e.sendReceipt(grp, worker, "Мансур", sberCheck("33600", "12:00:00"), "")
	e.sendGroup(groupText(grp, worker, "Мансур", "Арсаханов Аслан 33.600"))
	e.sendReceipt(grp, worker, "Мансур", sberCheck("5400", "13:00:00"), "Арсанукаев Магомед 5400")
	e.sendGroup(groupText(grp, worker, "Мансур", "Темирбулатов Абдулла\n17т нал взял МБ"))
	e.sendReceipt(other, user("79280000102"), "Умар", sberCheck("9000", "14:00:00"), "Другой Клиент")
	day := time.Now().Format("2006-01-02")
	base := fmt.Sprintf(`"from_date":%q,"to_date":%q,"sender":"Мансур","group":"Сбор СБ ЧЕКИ","kind":"checks"`, day, day)
	out := e.tool("senders_report", grp, `{`+base+`}`)
	for _, want := range []string{"чеки 4 шт. на 96200.75 ₽", "Эстамиров Ибрагим Муслимович", "Арсаев Магомед", "Арсаханов Аслан", "Арсанукаев Магомед", "по дате присылки в чат", "Конец списка"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Темирбулатов") || strings.Contains(out, "Другой Клиент") || strings.Contains(out, "клиент: Расул") {
		t.Fatalf("wrong filter/identity: %s", out)
	}
	page := e.tool("senders_report", grp, `{`+base+`,"limit":1,"sort_by":"amount","descending":true,"columns":["client","amount"]}`)
	if !strings.Contains(page, "next_offset=1") || !strings.Contains(page, "96200.75") || !strings.Contains(page, "1. Эстамиров") {
		t.Errorf("pagination lost full totals or sort: %s", page)
	}
	last := e.tool("senders_report", grp, `{`+base+`,"offset":3,"limit":1,"sort_by":"amount","descending":true,"columns":["client","amount"]}`)
	if !strings.Contains(last, "4. Арсанукаев") || !strings.Contains(last, "Конец списка") {
		t.Errorf("last page: %s", last)
	}
	// Independent DB connection simulates a restarted process reading saved data.
	reopened, err := db.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	from, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	stored, err := reopened.EmployeePayments(ctx, from, from.AddDate(0, 0, 1), db.EmployeeReportOptions{Sender: "89280000101", Kind: "checks"})
	if err != nil || len(stored.Stats) != 1 || stored.Stats[0].Checks != 4 || stored.Stats[0].Total != 96200.75 {
		t.Fatalf("not persistent: %+v / %v", stored, err)
	}
	// A real Responses tool cycle with a local fake provider: no external calls.
	e.assistant = e.fakeAIWithModel("gpt-6.1-sol")
	sawResult := false
	e.ai = func(c aiCall) aiReply {
		if c.hasToolResult() {
			sawResult = true
			result := strings.Join(c.toolResults(), "\n")
			if !strings.Contains(result, "96200.75") || !strings.Contains(result, "Арсаханов Аслан") {
				t.Errorf("GPT lacks details: %s", result)
			}
			return aiReply{Text: result}
		}
		return aiReply{ToolCalls: []aiToolCall{{Name: "senders_report", Args: `{` + base + `,"view":"payments"}`}}}
	}
	e.handleGroupAssistant(ctx, groupText(grp, user(ownerPhone), "Владелец", "Джарвис дай список сколько чеков закинул Мансур сумму и фио плательщиков"), "дай список сколько чеков закинул Мансур сумму и фио плательщиков")
	if !sawResult || !strings.Contains(e.last().Text, "96200.75") {
		t.Fatalf("Responses flow: %+v", e.last())
	}
	// PDF deliberately ignores text pagination and includes every client.
	e.reportDir, e.fontDir = t.TempDir(), "../../assets/fonts"
	pdfOut := e.tool("senders_report", grp, `{`+base+`,"format":"pdf","view":"payments","limit":1,"offset":3,"columns":["client","amount","date","employee","kind","bank_payer","operation_date","group","status"]}`)
	if !strings.Contains(pdfOut, "Полный PDF отправлен") {
		t.Fatalf("PDF: %s", pdfOut)
	}
	files, err := filepath.Glob(filepath.Join(e.reportDir, "employees_*.pdf"))
	if err != nil || len(files) != 1 {
		t.Fatalf("PDF missing: %v / %v", files, err)
	}
	if extractor, err := exec.LookPath("pdftotext"); err == nil {
		text, err := exec.Command(extractor, "-layout", files[0], "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"Эстамиров", "Арсаев", "Арсаханов", "Арсанукаев", "96200.75"} {
			if !strings.Contains(string(text), name) {
				t.Errorf("full PDF lost %q", name)
			}
		}
	}
}

func TestEmployeeReportHistoryNamesAndAmbiguity(t *testing.T) {
	grp := e2eGroup("employee-history")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ"}})
	e.aliases.Add("Каталов Ахмед", "Каталов Ахмед")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	makeMessage := func(phone, name, text string) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:      &waProto.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(e2eID("HIST")), Participant: proto.String(user(phone).String())},
			PushName: proto.String(name), MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())), Message: &waProto.Message{Conversation: proto.String(text)},
		}}
	}
	e.importHistorySync(&waHistorySync.HistorySync{
		Pushnames: []*waHistorySync.Pushname{{ID: proto.String(user("79280000104").String()), Pushname: proto.String("Мансур Второй")}},
		Conversations: []*waHistorySync.Conversation{{Messages: []*waHistorySync.HistorySyncMsg{
			makeMessage("79280000103", "Мансур Первый", "Каталов Ахмед 5000 наличка"),
			makeMessage("79280000104", "", "Исаев Харон 7000 наличка"),
		}}},
	})
	day := time.Now().Format("2006-01-02")
	out := e.tool("senders_report", grp, fmt.Sprintf(`{"from_date":%q,"to_date":%q,"sender":"Мансур","kind":"cash","view":"payments"}`, day, day))
	for _, want := range []string{"Мансур Первый", "Мансур Второй", "5000", "7000", "Одно имя не доказывает"} {
		if !strings.Contains(out, want) {
			t.Errorf("history name/ambiguity %q: %s", want, out)
		}
	}
}

func TestEmployeeReportTotalsAndUnverifiedLabels(t *testing.T) {
	stats := []db.EmployeeStat{{SenderStat: db.SenderStat{Total: 0.10, ChecksSum: 0.10}, PendingSum: 0.10}, {SenderStat: db.SenderStat{Total: 0.20, ChecksSum: 0.20}, PendingSum: 0.20}}
	total := employeeTotals(stats)
	if total.Total != 0.30 || total.ChecksSum != 0.30 || total.PendingSum != 0.30 {
		t.Fatalf("fractional kopecks drifted in totals: %+v", total)
	}
	label := employeeAttributionLabel("Мансур", "", "collector-label:mansur", "collector_name", "Ахмед", "79280000001")
	if !strings.Contains(label, "номер не подтверждён") || !strings.Contains(label, "Ахмед") {
		t.Fatalf("unverified collector presented as observed identity: %s", label)
	}
	label = employeeAttributionLabel("Мансур", "", "jid:123456789012345@lid", "", "", "")
	if strings.Contains(label, "+123456789012345") || !strings.Contains(label, "номер скрыт") {
		t.Fatalf("LID rendered as phone: %s", label)
	}
	cells := (&Bot{}).employeeCells(context.Background(), db.EmployeePayment{OperationAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), OperationDateUncertain: true}, []string{"operation_date"})
	if len(cells) != 1 || cells[0] != "дата требует уточнения" {
		t.Fatalf("disputed operation date presented as confirmed: %v", cells)
	}
}

func TestEmployeeReportValidatesArguments(t *testing.T) {
	for _, a := range []senderReportArgs{
		{FromDate: "bad", ToDate: "2026-11-01"}, {FromDate: "2026-11-02", ToDate: "2026-11-01"},
		{FromDate: "2026-11-01", ToDate: "2026-11-01", DateBasis: "garbage"},
		{FromDate: "2026-11-01", ToDate: "2026-11-01", Columns: []string{"made_up"}},
		{FromDate: "2026-11-01", ToDate: "2026-11-01", Offset: -1},
	} {
		if _, _, err := a.validate(); err == nil {
			t.Errorf("accepted invalid arguments %+v", a)
		}
	}
}

func TestEmployeeReportNameLearnedAfterRequestedPeriod(t *testing.T) {
	grp := e2eGroup("employee-later-name")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ"}})
	worker := user("79280000105")
	e.sendReceipt(grp, worker, "", sberCheck("15000", "10:00:00"), "Каталов Ахмед")
	later := groupText(grp, worker, "Мансур", "Принято")
	later.Info.Timestamp = time.Now().AddDate(0, 0, 7)
	e.sendGroup(later)
	day := time.Now().Format("2006-01-02")
	out := e.tool("senders_report", grp, fmt.Sprintf(`{"from_date":%q,"to_date":%q,"sender":"Мансур","kind":"checks"}`, day, day))
	if !strings.Contains(out, "чеки 1 шт. на 15000 ₽") || !strings.Contains(out, "Каталов Ахмед") {
		t.Fatalf("historical receipt lost when name learned later: %s", out)
	}
}

func TestEmployeeReportClientLookupDefaultsToPaymentDetails(t *testing.T) {
	a := senderReportArgs{FromDate: "2026-11-01", ToDate: "2026-11-01", Client: "Ханатаев Акраман"}
	if _, _, err := a.validate(); err != nil {
		t.Fatal(err)
	}
	if a.View != "payments" || a.Sender != "" {
		t.Fatalf("client lookup must show receipt details independently of employee: %+v", a)
	}
	tool := (&Bot{}).sendersTool(context.Background(), types.JID{})
	properties := tool.InputSchema["properties"].(map[string]any)
	if _, ok := properties["client"]; !ok {
		t.Fatal("missing independent client filter in tool schema")
	}
}

func TestEmployeeReportSummaryPDFIncludesEveryPayment(t *testing.T) {
	extractor, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext is required to inspect the generated PDF")
	}
	args := senderReportArgs{FromDate: "2026-11-01", ToDate: "2026-11-01", Format: "pdf", Limit: 1, Offset: 3}
	if _, _, err := args.validate(); err != nil {
		t.Fatal(err)
	}
	if args.View != "summary" {
		t.Fatalf("test must cover the all-worker summary default: %+v", args)
	}
	data := db.EmployeeReport{Rows: []db.EmployeePayment{
		{Client: "Эстамиров Ибрагим", Amount: 50000, Kind: "checks", Status: "counted", Employee: "Мансур"},
		{Client: "Хасуев Ислам", Amount: 30000, Kind: "checks", Status: "counted", Employee: "Мовлид"},
		{Client: "Ханатаев Акраман", Amount: 30000, Kind: "checks", Status: "counted", Employee: "~ M"},
		{Client: "Мусаев Рахман", Amount: 65000, Kind: "checks", Status: "counted", Employee: "Мухаммед"},
	}}
	for _, row := range data.Rows {
		data.Stats = append(data.Stats, db.EmployeeStat{SenderStat: db.SenderStat{Name: row.Employee, Count: 1, Checks: 1, Total: row.Amount, ChecksSum: row.Amount}})
	}
	b := &Bot{reportDir: t.TempDir(), fontDir: "../../assets/fonts", sendHook: func(types.JID, string, string) string { return "test-pdf" }}
	if err := b.sendEmployeePDF(context.Background(), types.JID{}, args, data, "01.11.2026", "Сбор СБ ЧЕКИ", "по дате присылки в чат"); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(b.reportDir, "employees_*.pdf"))
	if err != nil || len(files) != 1 {
		t.Fatalf("PDF missing: %v / %v", files, err)
	}
	content, err := exec.Command(extractor, "-layout", files[0], "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Полный список: 4 платежей", "Эстамиров Ибрагим", "Хасуев Ислам", "Ханатаев Акраман", "Мусаев Рахман", "175000"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("summary PDF omitted %q: %s", want, content)
		}
	}
}

func TestEmployeeReportNovemberFirstStoredReceiptFollowup(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("employee-nov1")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Сбор СБ ЧЕКИ"}})
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for i, receipt := range []struct {
		client, sender, phone, bank, payer, cardOwner, doc, auth string
		amount                                                   float64
		posted                                                   time.Duration
	}{
		{"Эстамиров Ибрагим Муслимович", "Мансур", "79280001101", "Сбер", "", "", "NOV1-ESTAMIROV-001", "", 50000, 10 * time.Minute},
		{"Хасуев Ислам", "Мовлид", "79280001102", "ВТБ", "", "Тамила", "A630-NOV1-HASUEV-002", "", 30000, 2*time.Hour + 54*time.Minute},
		{"Ханатаев Акраман", "~ M", "79280001103", "Сбер", "Элина Зилаудиевна Г.", "Милана Нажудовна К.", "1000000006086701079", "024307", 30000, 14*time.Hour + 24*time.Minute},
		{"Мусаев Рахман", "Мухаммед", "79280001104", "Сбер", "", "", "NOV1-MUSAEV-004", "", 65000, 20*time.Hour + 33*time.Minute},
	} {
		at := day.Add(receipt.posted)
		contact, err := e.db.GetOrCreateContact(ctx, receipt.client)
		if err != nil {
			t.Fatal(err)
		}
		raw, _, err := e.db.SaveRawMessage(ctx, e2eID(fmt.Sprintf("NOV1-%d", i)), grp.String(), user(receipt.phone).String(), receipt.phone, receipt.sender, receipt.client, true, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, ContactID: &contact, ClientConfirmed: true, RecipientRaw: receipt.client, Bank: receipt.bank, SenderRaw: receipt.payer, CardOwner: receipt.cardOwner, DocNumber: receipt.doc, AuthCode: receipt.auth, Amount: receipt.amount, TxDate: at, GroupJID: grp.String()}); err != nil {
			t.Fatal(err)
		}
	}
	cashAt := day.Add(13*time.Hour + 18*time.Minute)
	cashClient, err := e.db.GetOrCreateContact(ctx, "Идрисов Абузар")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("NOV1-CASH"), grp.String(), user("79280001105").String(), "79280001105", "Сафаи", "Идрисов Абузар 15т нал", false, "", cashAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.InsertTransaction(ctx, db.TransactionInput{RawMessageID: raw, ContactID: cashClient, RawName: "Идрисов Абузар", Amount: 15000, IsCash: true, TxDate: cashAt}); err != nil {
		t.Fatal(err)
	}
	base := `"from_date":"2026-11-01","to_date":"2026-11-01","group":"Сбор СБ ЧЕКИ"`
	full := e.tool("senders_report", grp, `{`+base+`,"kind":"checks","view":"payments"}`)
	for _, want := range []string{"чеки 4 шт. на 175000 ₽", "Эстамиров", "Хасуев Ислам", "Ханатаев Акраман", "Мусаев Рахман", "строки 1–4 из 4", "Конец списка"} {
		if !strings.Contains(full, want) {
			t.Errorf("complete report lacks %q: %s", want, full)
		}
	}
	if strings.Contains(full, "Идрисов") || strings.Contains(full, "15000 ₽") {
		t.Errorf("cash mixed into receipt-only report: %s", full)
	}
	followup := e.tool("senders_report", grp, `{`+base+`,"client":"Ханатаев Акраман","kind":"checks","columns":["client","amount","employee","bank_payer","posted_by"]}`)
	for _, want := range []string{"чеки 1 шт. на 30000 ₽", "Ханатаев Акраман", "~ M", "Элина Зилаудиевна Г.", "строки 1–1 из 1"} {
		if !strings.Contains(followup, want) {
			t.Errorf("client follow-up lacks %q: %s", want, followup)
		}
	}
	for _, wrong := range []string{"Мусаев", "Мухаммед", "65000", "Хасуев"} {
		if strings.Contains(followup, wrong) {
			t.Errorf("client follow-up guessed unrelated payment %q: %s", wrong, followup)
		}
	}
	wrongFilter := e.tool("senders_report", grp, `{`+base+`,"sender":"Ханатаев Акраман","kind":"checks"}`)
	if !strings.Contains(wrongFilter, "отсутствие в этой выборке не доказывает отсутствие чека клиента") || strings.Contains(wrongFilter, "65000") {
		t.Errorf("wrong worker filter made unsupported client claim: %s", wrongFilter)
	}
	all := e.tool("senders_report", grp, `{`+base+`,"view":"payments"}`)
	if !strings.Contains(all, "чеки 4 шт. на 175000 ₽; наличка 1 платежей на 15000 ₽; учтено всего 190000 ₽") || !strings.Contains(all, "Идрисов Абузар") {
		t.Errorf("cash not separated from receipt totals: %s", all)
	}
}
