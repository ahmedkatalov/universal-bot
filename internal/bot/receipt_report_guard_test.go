package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
)

func receiptReportScenario(t *testing.T, missingHanataev bool) (*e2eBot, types.JID) {
	t.Helper()
	grp := e2eGroup("receipt-report-source")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{grp.String(): "Сбор СБ ЧЕКИ"}})
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for i, row := range []struct {
		client, sender, doc, payer string
		amount                     float64
		posted                     time.Duration
	}{
		{"Эстамиров Ибрагим Муслимович", "Мансур", "A6304905527090B10060011881102", "", 50000, 10 * time.Minute},
		{"Хасуев Ислам", "Мовлид", "A6304225636335050B10020011881102", "Ислам Майрбекович В.", 30000, 2*time.Hour + 54*time.Minute},
		{"Ханатаев Акраман", "~ M", "1000000006086701079", "Элина Зилаудиевна Г.", 30000, 14*time.Hour + 24*time.Minute},
		{"Мусаев Рахман", "Мухаммад", "B6305172923681100B10100011881102", "Макка Салмановна М.", 65000, 20*time.Hour + 33*time.Minute},
	} {
		phone := fmt.Sprintf("7928000210%d", i)
		rawID, _, err := e.db.SaveRawMessage(context.Background(), e2eID("REPORT-SOURCE"), grp.String(), user(phone).String(), phone, row.sender, row.client, true, "/tmp/test-receipt-source.jpg", day.Add(row.posted))
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && missingHanataev {
			continue
		}
		cid, err := e.db.GetOrCreateContact(context.Background(), row.client)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(context.Background(), db.BankReceiptInput{RawMessageID: rawID, GroupJID: grp.String(), RecipientRaw: row.client, SenderRaw: row.payer, ContactID: &cid, Amount: row.amount, DocNumber: row.doc, ClientConfirmed: true, TxDate: day.Add(row.posted)}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(context.Background(), rawID); err != nil {
			t.Fatal(err)
		}
	}
	e.ai = func(c aiCall) aiReply {
		if c.hasToolResult() {
			return aiReply{Text: "Итого 3 чека на 145000 ₽. Всё проверено."} // deliberately wrong/truncated model answer
		}
		return aiReply{ToolCalls: []aiToolCall{{Name: "senders_report", Args: `{"from_date":"2026-11-01","to_date":"2026-11-01","group":"Сбор СБ ЧЕКИ","kind":"checks","view":"summary"}`}}}
	}
	return e, grp
}

func TestReceiptReportCannotLoseFourthReceiptInModelSummary(t *testing.T) {
	for _, inGroup := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%v", inGroup), func(t *testing.T) {
			e, grp := receiptReportScenario(t, false)
			query := "Дай ФИО плательщиков, сотрудника и сумму за 1 ноября"
			if inGroup {
				e.handleGroupAssistant(context.Background(), groupText(grp, user(ownerPhone), "Владелец", query), query)
			} else {
				e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", query))
			}
			out := e.last().Text
			for _, want := range []string{"чеки 4 шт. на 175000 ₽", "Ханатаев Акраман", "Мусаев Рахман", "Хасуев Ислам", "Эстамиров", "Конец списка"} {
				if !strings.Contains(out, want) {
					t.Errorf("model lost authoritative %q: %s", want, out)
				}
			}
			if strings.Contains(out, "145000") || strings.Contains(out, "Всё проверено") {
				t.Fatalf("unsupported model summary reached WhatsApp: %s", out)
			}
		})
	}
}

func TestReceiptReportSurvivesProviderFailureAfterSuccessfulTool(t *testing.T) {
	for _, inGroup := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%v", inGroup), func(t *testing.T) {
			e, grp := receiptReportScenario(t, false)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"message":"test provider unavailable after tool"}}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "report-call", "type": "function", "function": map[string]any{"name": "senders_report", "arguments": `{"from_date":"2026-11-01","to_date":"2026-11-01","group":"Сбор СБ ЧЕКИ","kind":"checks"}`}}}}}}})
			}))
			defer srv.Close()
			e.assistant = ai.New("test-key", "test-model", "test-model", srv.URL)
			query := "Дай ФИО и суммы за 1 ноября"
			if inGroup {
				e.handleGroupAssistant(context.Background(), groupText(grp, user(ownerPhone), "Владелец", query), query)
			} else {
				e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", query))
			}
			out := e.last().Text
			if requests.Load() < 2 || !strings.Contains(out, "Ханатаев Акраман") || !strings.Contains(out, "175000") {
				t.Fatalf("a completed database report was lost after provider error: requests=%d out=%s", requests.Load(), out)
			}
		})
	}
}

func TestReceiptClientFollowupUsesPersistedClientScopeAndOriginalSource(t *testing.T) {
	e, _ := receiptReportScenario(t, false)
	e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", "Дай ФИО плательщиков за 1 ноября"))
	e.history = make(map[string][]ai.Turn) // report selection survives loss/reload of conversational history
	e.ai = func(c aiCall) aiReply {
		t.Error("a known receipt follow-up was left to a model that could guess the wrong person")
		return aiReply{Text: "Возможно, это Мусаев на 65000"}
	}
	e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", "А как насчёт ханатаев акраман?"))
	out := e.last().Text
	for _, want := range []string{"Фильтр ФИО клиента", "Ханатаев Акраман", "чеки 1 шт. на 30000 ₽", "~ M", "Элина Зилаудиевна Г.", "14:24"} {
		if !strings.Contains(out, want) {
			t.Errorf("follow-up lost %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Мусаев") || strings.Contains(out, "65000") {
		t.Fatalf("client was replaced by an unrelated worker/payment: %s", out)
	}
	other := privateText(user("79280009999"), "Другой владелец", "А как насчёт ханатаев акраман?")
	if _, handled := e.receiptClientFollowup(context.Background(), other.Info, extractText(other.Message), true); handled {
		t.Fatal("another conversation inherited the private report selection")
	}
	if _, handled := e.receiptClientFollowup(context.Background(), privateText(user(ownerPhone), "Владелец", "").Info, "А как насчёт ханатаев акраман?", false); handled {
		t.Fatal("unprivileged caller inherited report access")
	}
}

func TestReceiptReportUnreadCaptionCannotBecomeDefinitiveAbsence(t *testing.T) {
	e, _ := receiptReportScenario(t, true)
	e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", "Дай ФИО плательщиков за 1 ноября"))
	if out := e.last().Text; !strings.Contains(out, "Отчёт предварительный") || !strings.Contains(out, "не разобраны вложения: 1") || strings.Contains(out, "Всё проверено") {
		t.Fatalf("model hid the saved but unread fourth receipt: %s", out)
	}
	e.handlePrivateMessage(context.Background(), privateText(user(ownerPhone), "Владелец", "А как насчет Ханатаев Акраман?"))
	out := e.last().Text
	for _, want := range []string{"Ханатаев Акраман", "Проверка исходных сообщений", "~ M", "14:24", "разбор не завершён"} {
		if !strings.Contains(out, want) {
			t.Errorf("unread source hidden (%q): %s", want, out)
		}
	}
	if strings.Contains(out, "65000") || strings.Contains(out, "Мусаев") {
		t.Fatalf("unread receipt was replaced with an unrelated payment: %s", out)
	}
}

func TestReceiptReportGuardPreservesOtherToolActionAndWarnings(t *testing.T) {
	b := &Bot{}
	tools, _, result := b.trackReceiptReports(context.Background(), privateText(user(ownerPhone), "Владелец", "").Info, "ФИО", []ai.Tool{
		{Name: "senders_report", Handle: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("Строка отчёта\n", 100) + "Ханатаев Акраман 30000", nil
		}},
		{Name: "cmf_connection_check", Handle: func(context.Context, json.RawMessage) (string, error) {
			return "Связь с программой есть", nil
		}},
		{Name: "group_reading_status", Handle: func(context.Context, json.RawMessage) (string, error) {
			return "⚠️ Ещё один исходник ожидает чтения", nil
		}},
	})
	for _, tool := range tools {
		if _, err := tool.Handle(context.Background(), []byte(`{"from_date":"2026-11-01","to_date":"2026-11-01"}`)); err != nil {
			t.Fatal(err)
		}
	}
	out := result.apply("[молчу]")
	for _, want := range []string{"Ханатаев Акраман 30000", "Связь с программой есть", "⚠️ Ещё один исходник"} {
		if !strings.Contains(out, want) {
			t.Fatalf("authoritative result truncated or action lost (%q): %s", want, out)
		}
	}
}

func TestReceiptReportGuardKeepsSelectionsAndPages(t *testing.T) {
	b := &Bot{}
	version := "исходный"
	tools, _, result := b.trackReceiptReports(context.Background(), privateText(user(ownerPhone), "Владелец", "").Info, "Дай список по двум группам", []ai.Tool{
		{Name: "senders_report", Handle: func(_ context.Context, input json.RawMessage) (string, error) {
			var args senderReportArgs
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			return fmt.Sprintf("Группа %s, страница %d, %s", args.Group, args.Offset, version), nil
		}},
	})
	call := func(group string, offset int) {
		t.Helper()
		input, _ := json.Marshal(senderReportArgs{FromDate: "2026-11-01", ToDate: "2026-11-01", Group: group, Offset: offset})
		if _, err := tools[0].Handle(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	call("А", 0)
	call("Б", 0)
	call("А", 50)
	version = "обновлённый"
	call("А", 0)
	out := result.apply("Модель сократила список")
	for _, want := range []string{"Группа А, страница 0, обновлённый", "Группа Б, страница 0, исходный", "Группа А, страница 50, исходный"} {
		if strings.Count(out, want) != 1 {
			t.Errorf("lost selection/page %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Группа А, страница 0, исходный") {
		t.Fatalf("stale snapshot of the same selection/page was retained: %s", out)
	}
}
