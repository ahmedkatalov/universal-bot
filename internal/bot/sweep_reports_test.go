package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Регрессии отчётов: выбор группы по точному названию, /отчет только для
// админов, сутки по Москве, «НЕ вошло» во всех отчётах с суммами.

func (e *e2eBot) tool(name string, chat types.JID, args string) string {
	e.t.Helper()
	for _, tl := range e.assistantTools(context.Background(), chat, types.JID{}, true, true) {
		if tl.Name == name {
			out, err := tl.Handle(context.Background(), json.RawMessage(args))
			if err != nil {
				return "ERR: " + err.Error()
			}
			return out
		}
	}
	e.t.Fatalf("нет инструмента %s", name)
	return ""
}

func TestSweepGroupExactName(t *testing.T) {
	g1, g2 := e2eGroup("i1"), e2eGroup("i2")
	e := newE2E(t, e2eOpts{Groups: map[string]string{g1.String(): "Оплата клиентов", g2.String(): "СБ оплата клиентов"}})
	ctx := context.Background()
	if j, _, err := e.resolveGroup(ctx, "оплата клиентов"); err != nil || j != g1 {
		t.Errorf("точное название: %v %v", j, err)
	}
	if j, _, err := e.resolveGroup(ctx, "«СБ  оплата клиентов»"); err != nil || j != g2 {
		t.Errorf("с кавычками/пробелами: %v %v", j, err)
	}
	if _, _, err := e.resolveGroup(ctx, "оплата"); err == nil {
		t.Error("неоднозначный кусок «оплата» должен переспросить")
	}
	if j, _, err := e.resolveGroup(ctx, "сб"); err != nil || j != g2 {
		t.Errorf("слово «сб»: %v %v", j, err)
	}
}

func TestSweepReportCommandAdminsOnly(t *testing.T) {
	grp, other := e2eGroup("c"), e2eGroup("c2")
	e := newE2E(t, e2eOpts{Admins: []string{"79287836800"}})
	e.reportDir, e.fontDir = t.TempDir(), "../../assets/fonts"
	e.aliases.Add("Ахмед", "Ахмед")
	e.sendGroup(groupText(other, user("79001110002"), "Ислам", "Ахмед 50000"))
	n := len(e.sent())
	e.sendGroup(groupText(grp, user("79001110001"), "Расул", "/отчет"))
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "[документ]") || strings.Contains(m.Text, "50") {
			t.Fatalf("не-админ получил отчёт: %+v", m)
		}
	}
	e.sendGroup(groupText(grp, user("79287836800"), "Владелец", "/отчет"))
	if got := e.sentSince(n); len(got) == 0 {
		t.Error("админ не получил отчёт")
	}
}

func TestSweepLedgerMoscowDay(t *testing.T) {
	grp := e2eGroup("h1")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ Оплата"}})
	day := time.Now().AddDate(0, 0, -1)
	ocr := "СберБанк\nЧек по операции\nДата операции " + day.Format("02.01.2006") + " 01:30:00" +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n15000 ₽\nНомер операции в СБП\n" + e2eID("OP")
	e.sendReceipt(grp, user("79001110001"), "Расул", ocr, "Хасанов Ислам")
	d := day.Format("2006-01-02")
	out := e.tool("receipts_ledger", grp, `{"from_date":"`+d+`","to_date":"`+d+`"}`)
	if !strings.Contains(out, "15000") {
		t.Errorf("ночной чек (01:30 МСК) не в журнале своего дня: %s", out)
	}
}

func TestSweepReportsShowExcluded(t *testing.T) {
	grp := e2eGroup("f")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ Оплата"}})
	e.sendReceipt(grp, user("79001110001"), "Расул", sberCheck("15000", "10:00:00"), "Магомедов Ахмед")
	e.sendReceipt(grp, user("79001110001"), "Расул", sberCheck("20000", "10:05:00"), "")
	d := time.Now().Format("2006-01-02")
	for _, name := range []string{"senders_report", "receipts_ledger"} {
		out := e.tool(name, grp, `{"from_date":"`+d+`","to_date":"`+d+`"}`)
		if !strings.Contains(out, "20000") || !strings.Contains(out, "НЕ вошло") {
			t.Errorf("%s прячет непроверенный чек на 20000: %s", name, out)
		}
	}
	out := e.tool("send_finance_report", grp, `{"from_date":"`+d+`","to_date":"`+d+`","format":"text","date_basis":"chat"}`)
	if !strings.Contains(out, "НЕ вошло") || !strings.Contains(out, "20000") {
		t.Errorf("отчёт по дате чата без «НЕ вошло»: %s", out)
	}
}
