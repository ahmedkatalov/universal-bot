package bot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tests simulate the next scheduled tick; production never bypasses backoff.
func (e *e2eBot) retryAITextAfterBackoff(ctx context.Context) {
	e.t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `UPDATE raw_messages SET text_next_at=now()-interval '1 second' WHERE text_next_at IS NOT NULL`)
	_ = conn.Close(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	e.retryAIText(ctx)
}

func TestTextExtractionQuorumAndPersistentBackoff(t *testing.T) {
	t.Setenv("PAYMENT_READS", "3")
	e := newE2E(t, e2eOpts{WithAI: true})
	var reads atomic.Int64
	e.ai = func(aiCall) aiReply {
		if reads.Add(1) == 1 {
			return aiReply{Text: `{"payments":[{"name":"Абдулвахидов Харон","amount":35600,"cash":true}],"clarify":""}`}
		}
		return aiReply{Text: `{}`}
	}
	ctx := context.Background()
	grp, worker := e2eGroup("text-backoff-quorum"), user("79281119995")
	text, at := "Абдулвахидов Харон 35600 наличными", time.Now()
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("TQ"), grp.String(), worker.String(), worker.String(), "Мансур", text, false, "", at)
	if err != nil {
		t.Fatal(err)
	}
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, at, false, true)
	if got := sum(dayTotals(e, grp)); got != 0 {
		t.Fatalf("one valid response out of three confirmed money: %v", got)
	}
	first := reads.Load()
	if first != 3 {
		t.Fatalf("expected three independent readings, got %d", first)
	}
	e.retryAIText(ctx)
	if reads.Load() != first {
		t.Fatal("in-memory queue bypassed durable backoff")
	}
	e.aiRetryMu.Lock()
	e.aiRetry = nil
	e.aiRetryMu.Unlock()
	e.retryAIText(ctx)
	if reads.Load() != first {
		t.Fatal("restart bypassed durable backoff")
	}
	e.ai = func(aiCall) aiReply {
		reads.Add(1)
		return aiReply{Text: `{"payments":[{"name":"Абдулвахидов Харон","amount":35600,"cash":true}],"clarify":""}`}
	}
	e.retryAITextAfterBackoff(ctx)
	if got := sum(dayTotals(e, grp)); got != 35600 {
		t.Fatalf("due retry failed to recover payment: %v", got)
	}
}

func TestTextRetryMalformedMessageDoesNotStarveOtherMessages(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	ctx := context.Background()
	e.ai = func(c aiCall) aiReply {
		if len(c.lastUserText()) > 0 && strings.Contains(c.lastUserText(), "Нечитаемый") {
			return aiReply{Text: `{}`}
		}
		return aiReply{Text: `{"payments":[{"name":"Исаев Харон","amount":10000,"cash":true}],"clarify":""}`}
	}
	grp, worker := e2eGroup("text-retry-fairness"), user("79281119996")
	for i, text := range []string{"Нечитаемый Клиент 5000 наличка", "Исаев Харон 10000 наличка"} {
		_, _, err := e.db.SaveRawMessage(ctx, e2eID("TF"), grp.String(), worker.String(), worker.String(), "Мансур", text, false, "", time.Now().Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	e.retryAIText(ctx)
	if got := sum(dayTotals(e, grp)); got != 10000 {
		t.Fatalf("malformed first message starved a readable payment: %v", got)
	}
}

func TestTextRetryRestoresCashPhotoHintAfterRestart(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	ctx := context.Background()
	e.ai = func(aiCall) aiReply { return aiReply{Text: `{}`} }
	grp, worker := e2eGroup("cash-hint-restart"), user("79281119997")
	text, at := "Абдулвахидов Харон 35600", time.Now()
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("CH"), grp.String(), worker.String(), worker.String(), "Мансур", text, false, "", at)
	if err != nil {
		t.Fatal(err)
	}
	e.aiRescueUnparsed(ctx, grp, "Мансур", []string{text}, raw, at, true, true)
	e.aiRetryMu.Lock()
	e.aiRetry = nil
	e.aiRetryMu.Unlock()
	e.ai = func(aiCall) aiReply {
		return aiReply{Text: `{"payments":[{"name":"Абдулвахидов Харон","amount":35600,"cash":false}],"clarify":""}`}
	}
	e.retryAITextAfterBackoff(ctx)
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var cash bool
	if err := conn.QueryRow(ctx, `SELECT is_cash FROM transactions WHERE raw_message_id=$1`, raw).Scan(&cash); err != nil {
		t.Fatal(err)
	}
	if !cash {
		t.Fatal("cash photo evidence was lost after restarting the retry queue")
	}
}

func TestTextRetryReloadsBodyAfterQueuedEdit(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	ctx := context.Background()
	e.ai = func(c aiCall) aiReply {
		amount := 20000
		if strings.Contains(c.lastUserText(), "10000") {
			amount = 10000
		}
		return aiReply{Text: fmt.Sprintf(`{"payments":[{"name":"Абдулвахидов Харон","amount":%d,"cash":true}],"clarify":""}`, amount)}
	}
	grp, worker := e2eGroup("text-retry-edit-order"), user("79281119998")
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("TRE"), grp.String(), worker.String(), worker.String(), "Мансур", "Абдулвахидов Харон 10000 наличка", false, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	blocked, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	e.enqueueChat(grp.String(), func() { close(blocked); <-release })
	<-blocked
	editDone := make(chan error, 1)
	e.enqueueChat(grp.String(), func() {
		_, err := e.db.PrepareMessageEdit(ctx, raw, "Абдулвахидов Харон 20000 наличка")
		editDone <- err
	})
	retried := make(chan struct{})
	go func() { defer close(retried); e.retryAIText(ctx) }()
	queued := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		e.chatQMu.Lock()
		q := e.chatQ[grp.String()]
		queued = q != nil && atomic.LoadInt64(&q.pending) >= 3
		e.chatQMu.Unlock()
		if queued {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !queued {
		t.Fatal("retry did not enter the same chat queue behind the edit")
	}
	close(release)
	select {
	case <-retried:
	case <-time.After(5 * time.Second):
		t.Fatal("queued retry did not complete")
	}
	if err := <-editDone; err != nil {
		t.Fatal(err)
	}
	if got := sum(dayTotals(e, grp)); got != 20000 {
		t.Fatalf("retry wrote old body under edited revision: %v", got)
	}
}
