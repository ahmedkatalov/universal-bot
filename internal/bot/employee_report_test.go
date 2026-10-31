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
	for _, want := range []string{"Мансур Первый", "Мансур Второй", "5000", "7000", "несколько сотрудников"} {
		if !strings.Contains(out, want) {
			t.Errorf("history name/ambiguity %q: %s", want, out)
		}
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
