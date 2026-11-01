package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"whatsapp-bot/internal/ai"
)

func TestPaymentExtractionRequiresExplicitVerdict(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"unexpected":1}`, `{"payments":null}`, `{"payments":{}}`,
		`{"payments":[{"name":"Исаев Харон"}]}`, `{"payments":[{"name":"Исаев Харон","amount":-100}]}`,
		`{"payments":[{"name":"","amount":100}]}`, `{"payments":[{"name":"Исаев Харон","amount":100,"cash":"yes"}]}`,
		`{"payments":[{"name":"Исаев Харон","amount":100},{}]}`, `not JSON`,
	} {
		if _, _, valid := decodePaymentExtraction(input); valid {
			t.Errorf("accepted incomplete extraction: %s", input)
		}
	}
	for _, input := range []string{
		`{"payments":[],"clarify":""}`,
		`{"payments":[],"clarify":"Чья наличка?"}`,
		`{"payments":[{"name":"Исаев Харон","amount":100}]}`,
	} {
		if _, _, valid := decodePaymentExtraction(input); !valid {
			t.Errorf("valid legacy extraction rejected: %s", input)
		}
	}
}

func TestReceiptExtractionRequiresClassificationAndAmount(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"recipient":"Хадижат","amount":5000}`, `{"kind":"receipt","recipient":"Хадижат"}`,
		`{"kind":"receipt","amount":null}`, `{"kind":"receipt","amount":"5000"}`,
		`{"kind":"unknown","amount":5000}`, `{"kind":"receipt","amount":-5000}`,
		`{"kind":"receipt","amount":5000,"datetime":"not a date"}`,
	} {
		if _, valid := decodeReceiptExtraction(input); valid {
			t.Errorf("accepted incomplete receipt: %s", input)
		}
	}
	for _, input := range []string{
		`{"kind":"receipt","amount":5000,"recipient":"Хадижат"}`, `{"kind":"receipt","amount":0,"recipient":"Хадижат"}`,
		`{"kind":"cash"}`, `{"kind":"other"}`,
	} {
		if _, valid := decodeReceiptExtraction(input); !valid {
			t.Errorf("valid classification rejected: %s", input)
		}
	}
}

type extractionReply struct {
	text   string
	status int
}

func extractionVisionBot(t *testing.T, replies []extractionReply) *Bot {
	t.Helper()
	var next atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(next.Add(1)-1) % len(replies)
		reply := replies[i]
		if reply.status != 0 {
			w.WriteHeader(reply.status)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid test request"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply.text}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return &Bot{assistant: ai.New("test-key", "test-model", "test-vision", srv.URL)}
}

func TestVisionConsensusNeedsQuorumAndConsistentFinancialFields(t *testing.T) {
	base := `{"kind":"receipt","amount":15000,"recipient":"Хадижат И.","datetime":"2026-11-01 10:00:00","status":"Выполнено","doc_number":"TEST-DOC-15000"}`
	for _, tc := range []struct {
		name        string
		replies     []extractionReply
		kind        string
		amount      float64
		ok, verdict bool
	}{
		{"malformed", []extractionReply{{"not JSON", 0}, {`{}`, 0}, {`{"amount":15000}`, 0}}, "", 0, false, false},
		{"one-good-of-three", []extractionReply{{base, 0}, {"", http.StatusBadRequest}, {"", http.StatusBadRequest}}, "", 0, false, false},
		{"different-amounts", []extractionReply{{base, 0}, {strings.Replace(base, `"amount":15000`, `"amount":150000`, 1), 0}, {strings.Replace(base, `"amount":15000`, `"amount":1500`, 1), 0}}, "", 0, false, false},
		{"conflicting-dates", []extractionReply{{base, 0}, {base, 0}, {strings.Replace(base, "2026-11-01", "2026-10-31", 1), 0}}, "", 0, false, false},
		{"conflicting-status", []extractionReply{{base, 0}, {base, 0}, {strings.Replace(base, "Выполнено", "Операция отклонена", 1), 0}}, "", 0, false, false},
		{"pending-status", []extractionReply{{strings.Replace(base, "Выполнено", "В обработке", 1), 0}}, "", 0, false, false},
		{"confirmed", []extractionReply{{base, 0}}, "receipt", 15000, true, true},
		{"one-amount-outlier", []extractionReply{{base, 0}, {base, 0}, {strings.Replace(base, `"amount":15000`, `"amount":150000`, 1), 0}}, "receipt", 15000, true, true},
		{"explicit-other", []extractionReply{{`{"kind":"other"}`, 0}}, "other", 0, false, true},
		{"cash-unknown-amount", []extractionReply{{`{"kind":"cash","amount":10000}`, 0}, {`{"kind":"cash","amount":20000}`, 0}, {`{"kind":"cash","amount":30000}`, 0}}, "cash", 0, true, true},
		{"type-tie", []extractionReply{{base, 0}, {`{"kind":"cash"}`, 0}, {`{"kind":"other"}`, 0}}, "", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RECEIPT_VISION_READS", "3")
			b := extractionVisionBot(t, tc.replies)
			r, ok, verdict := b.aiVisionReceiptConsensus(context.Background(), []byte("test-image"), ".jpg", "")
			if ok != tc.ok || verdict != tc.verdict || r.Kind != tc.kind || r.Amount != tc.amount {
				t.Fatalf("rec=%+v ok=%v verdict=%v; want %s %.2f %v %v", r, ok, verdict, tc.kind, tc.amount, tc.ok, tc.verdict)
			}
		})
	}
}

func TestVerifiedReceiptDoesNotInventAnUnconfirmedTime(t *testing.T) {
	common := aiReceipt{Kind: "receipt", Amount: 15000, Recipient: "Хадижат И.", Status: "Выполнено", Datetime: "2026-11-01"}
	recs := []aiReceipt{common, common, common}
	recs[2].Datetime = "2026-11-01 10:00:00"
	r, ok := verifiedReceipt(recs, 2)
	if !ok || r.Datetime != "2026-11-01" {
		t.Fatalf("invented time: %+v / %v", r, ok)
	}
	recs[1].Datetime = "2026-11-01 10:00:00"
	r, ok = verifiedReceipt(recs, 2)
	if !ok || r.Datetime != "2026-11-01 10:00:00" {
		t.Fatalf("lost confirmed time: %+v / %v", r, ok)
	}
}

func TestMalformedPaymentExtractionRemainsRetryable(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(aiCall) aiReply { return aiReply{Text: `{}`} }
	ctx := context.Background()
	grp, worker := e2eGroup("malformed-payment"), user("79281119991")
	text := "Абдулвахидов Харон 35600 наличными"
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("M"), grp.String(), worker.String(), worker.String(), "Мансур", text, false, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, time.Now(), false, true)
	rows, err := e.db.UnparsedTextMessagesSince(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == raw {
			found = true
		}
	}
	if !found {
		t.Fatal("malformed extraction closed a real unknown payment as parsed")
	}
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("invented payment: %v", got)
	}
}

func TestMalformedVisionImageRemainsRetryable(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(aiCall) aiReply { return aiReply{Text: "not JSON"} }
	ctx := context.Background()
	grp, worker := e2eGroup("malformed-vision"), user("79281119992")
	wa := e.sendReceiptMedia(grp, worker, "Мансур", "", "", []byte("new-malformed-vision-image"))
	raw, err := e.db.RawMessageByWaID(ctx, wa)
	if err != nil {
		t.Fatal(err)
	}
	if !e.db.NeedsMediaReread(ctx, raw.ID) {
		t.Fatal("malformed vision output hid unread image as a non-receipt")
	}
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("invented receipt: %v", got)
	}
}

func TestVisionDisagreementDoesNotActivateOCRPayment(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	var read atomic.Int64
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			amounts := []int{15000, 150000, 1500}
			a := amounts[int(read.Add(1)-1)%len(amounts)]
			return aiReply{Text: fmt.Sprintf(`{"kind":"receipt","amount":%d,"recipient":"Хадижат И.","datetime":%q,"status":"Выполнено","doc_number":"DISPUTED-DOC-001"}`, a, time.Now().Format("2006-01-02")+" 10:00:00")}
		}
		return aiReply{Text: `{"client":"Каталов Ахмед"}`}
	}
	grp, worker := e2eGroup("disputed-ocr"), user("79281119993")
	e.sendReceiptMedia(grp, worker, "Мансур", sberCheck("15000", "10:00:00"), "Каталов Ахмед", []byte("new-disputed-vision-image"))
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("unresolved vision silently activated OCR amount: %v", got)
	}
}

func TestAIExtractionBatchRollbackAndIdempotentRetry(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(aiCall) aiReply {
		return aiReply{Text: `{"payments":[{"name":"Каталов Ахмед","amount":15000,"cash":true},{"name":"Исаев Харон","amount":10000,"cash":true}],"clarify":""}`}
	}
	ctx := context.Background()
	grp, worker := e2eGroup("atomic-ai-batch"), user("79281119994")
	text, at := "Каталов Ахмед 15000 наличка\nИсаев Харон 10000 наличка", time.Now()
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("ATOMIC"), grp.String(), worker.String(), worker.String(), "Мансур", text, false, "", at)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	fn, trigger := "test_ai_batch_failure", "test_ai_batch_failure_trigger"
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+trigger+" ON transactions")
		_, _ = conn.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+fn+"()")
		_ = conn.Close(context.Background())
	})
	_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.raw_message_id=%d AND NEW.amount=15000 THEN RAISE EXCEPTION 'test second item failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER %s BEFORE INSERT ON transactions FOR EACH ROW EXECUTE FUNCTION %s()`, fn, raw, trigger, fn))
	if err != nil {
		t.Fatal(err)
	}
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, at, false, true)
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM transactions WHERE raw_message_id=$1`, raw).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a failed batch retained %d partial payment rows", count)
	}
	var parsed bool
	if err := conn.QueryRow(ctx, `SELECT parsed FROM raw_messages WHERE id=$1`, raw).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if parsed {
		t.Fatal("failed whole-message extraction was closed as parsed")
	}
	if _, err := conn.Exec(ctx, "DROP TRIGGER "+trigger+" ON transactions; DROP FUNCTION "+fn+"()"); err != nil {
		t.Fatal(err)
	}
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, at, false, true)
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, at, false, true)
	var total float64
	if err := conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(amount),0)::float8 FROM transactions WHERE raw_message_id=$1`, raw).Scan(&count, &total); err != nil {
		t.Fatal(err)
	}
	if count != 2 || total != 25000 {
		t.Fatalf("retry duplicated or lost items: count=%d total=%v", count, total)
	}
}
