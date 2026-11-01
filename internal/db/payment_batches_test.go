package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func paymentBatchFixture(t *testing.T, d *DB) (int, []TransactionInput) {
	t.Helper()
	ctx := context.Background()
	group, sender := uniqueScope("batch")
	group += "@g.us"
	received := time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("MSK", 3*3600))
	rawID, _, err := d.SaveRawMessage(ctx, group+"-source", group, sender, "", "Мансур", "Клиент Один 1000\nКлиент Два 2000", false, "", received)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []TransactionInput
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("Клиент Пакета %s %d", group, i)
		cid, err := d.GetOrCreateContact(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, TransactionInput{ContactID: cid, RawName: name, Amount: float64((i + 1) * 1000), RawMessageID: rawID, TxDate: received})
	}
	return rawID, inputs
}

func paymentBatchCounts(t *testing.T, d *DB, rawID int) (int, int) {
	t.Helper()
	var rows, batches int
	if err := d.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM transactions WHERE raw_message_id=$1", rawID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM parsed_payment_batches WHERE raw_message_id=$1", rawID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	return rows, batches
}

func TestPaymentBatchRetryAndChangedPayload(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	for attempt := 0; attempt < 2; attempt++ {
		saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs)
		if err != nil || saved != 2 {
			t.Fatalf("attempt %d: saved=%d err=%v", attempt, saved, err)
		}
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 2 || batches != 1 {
		t.Fatalf("retry duplicated payment rows: rows=%d batches=%d", rows, batches)
	}
	changed := append([]TransactionInput(nil), inputs...)
	changed[1].Amount = 2001
	if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", changed); !errors.Is(err, ErrPaymentBatchChanged) {
		t.Fatalf("changed batch silently accepted: %v", err)
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 2 || batches != 1 {
		t.Fatalf("changed payload wrote new rows: rows=%d batches=%d", rows, batches)
	}
}

func TestPaymentBatchRollsBackEveryRowOnDatabaseRejection(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	// A database failure on the SECOND payment must roll back the first too.
	// This exercises the commit boundary rather than preflight validation.
	name := fmt.Sprintf("test_batch_reject_%d", rawID)
	_, err := d.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
	 BEGIN IF NEW.raw_message_id = %d AND NEW.amount = 2000 THEN RAISE EXCEPTION 'test batch rejection'; END IF; RETURN NEW; END $$;
	 CREATE TRIGGER %s BEFORE INSERT ON transactions FOR EACH ROW EXECUTE FUNCTION %s()`, name, rawID, name, name))
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err := d.pool.Exec(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON transactions; DROP FUNCTION IF EXISTS %s()", name, name)); err != nil {
			t.Errorf("drop test trigger: %v", err)
		}
	}
	t.Cleanup(drop)
	if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); err == nil || saved != 0 {
		t.Fatalf("database rejection not propagated: saved=%d err=%v", saved, err)
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 0 || batches != 0 {
		t.Fatalf("partial commit: rows=%d batches=%d", rows, batches)
	}
	drop()
	if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); err != nil || saved != 2 {
		t.Fatalf("failed batch consumed its retry token: saved=%d err=%v", saved, err)
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 2 || batches != 1 {
		t.Fatalf("bad recovery: rows=%d batches=%d", rows, batches)
	}
}

func TestPaymentBatchPreservesIdenticalSourceItems(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	item := inputs[0]
	item.Amount = 1234.45
	item.IsCash = true
	item.DupCheck = true
	inputs = []TransactionInput{item, item}
	for attempt := 0; attempt < 2; attempt++ {
		if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "deterministic", inputs); err != nil || saved != 2 {
			t.Fatalf("identical source items: saved=%d err=%v", saved, err)
		}
	}
	var count, held int
	var total float64
	if err := d.pool.QueryRow(ctx, "SELECT COUNT(*),COALESCE(SUM(amount),0)::float8,COUNT(*) FILTER (WHERE dup_pending) FROM transactions WHERE raw_message_id=$1", rawID).Scan(&count, &total, &held); err != nil {
		t.Fatal(err)
	}
	if count != 2 || total != 2468.90 || held != 0 {
		t.Fatalf("legitimate identical payments collapsed or held: count=%d sum=%.2f held=%d", count, total, held)
	}
}

func TestPaymentBatchRejectsStaleAndDeletedMessage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for _, scenario := range []string{"edited", "deleted", "edited_retry"} {
		t.Run(scenario, func(t *testing.T) {
			rawID, inputs := paymentBatchFixture(t, d)
			expectedRows, expectedBatches := 0, 0
			if scenario == "edited_retry" {
				if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); err != nil {
					t.Fatal(err)
				}
				expectedRows, expectedBatches = 2, 1
			}
			query := "UPDATE raw_messages SET edit_count=edit_count+1 WHERE id=$1"
			if scenario == "deleted" {
				query = "UPDATE raw_messages SET deleted=true WHERE id=$1"
			}
			if _, err := d.pool.Exec(ctx, query, rawID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); !errors.Is(err, ErrMessageRevisionChanged) {
				t.Fatalf("stale/deleted message wrote successfully: %v", err)
			}
			if rows, batches := paymentBatchCounts(t, d, rawID); rows != expectedRows || batches != expectedBatches {
				t.Fatalf("stale batch changed rows: rows=%d batches=%d", rows, batches)
			}
		})
	}
}

func TestPaymentBatchKeepsMoscowCalendarDate(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); err != nil {
		t.Fatal(err)
	}
	rows, err := d.pool.Query(ctx, "SELECT tx_date::text FROM transactions WHERE raw_message_id=$1 ORDER BY id", rawID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			t.Fatal(err)
		}
		if date != "2026-11-01" {
			t.Errorf("00:30 Moscow was moved to yesterday: %s", date)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !inputs[0].TxDate.Equal(time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("MSK", 3*3600))) || inputs[0].TxDate.Location().String() != "MSK" {
		t.Fatal("batch processing mutated caller's timestamp")
	}
}

func TestPaymentBatchSingleConnectionDoesNotStarveItsOwnTransaction(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	// The schema is already migrated by testDB. Construct a one-connection pool
	// directly so this tests the batch boundary, not migration-lock acquisition.
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	single := &DB{pool: pool}
	var group, sender string
	if err := d.pool.QueryRow(ctx, "SELECT wa_group_jid,sender_jid FROM raw_messages WHERE id=$1", rawID).Scan(&group, &sender); err != nil {
		t.Fatal(err)
	}
	previous, _, err := d.SaveRawMessage(ctx, group+"previous", group, sender, "", "Мансур", "прошлая наличка", false, "", inputs[0].TxDate.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	prior := inputs[0]
	prior.RawMessageID = previous
	prior.IsCash = true
	if err := d.InsertTransaction(ctx, prior); err != nil {
		t.Fatal(err)
	}
	for i := range inputs {
		inputs[i].IsCash = true
		inputs[i].DupCheck = true
	}
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if saved, err := single.ApplyParsedPaymentBatch(deadline, rawID, 0, "ai", inputs); err != nil || saved != 2 {
		t.Fatalf("one-connection pool blocked its own cash lookup: saved=%d err=%v", saved, err)
	}
	var held int
	if err := single.pool.QueryRow(ctx, "SELECT COUNT(*) FROM transactions WHERE raw_message_id=$1 AND dup_pending", rawID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Fatalf("cash-duplicate check was bypassed: held=%d want 1", held)
	}
}

func TestPaymentBatchConcurrentRetriesCommitOneCopy(t *testing.T) {
	d := testDB(t)
	rawID, inputs := paymentBatchFixture(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs)
			if err == nil && saved != 2 {
				err = fmt.Errorf("saved=%d", saved)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 2 || batches != 1 {
		t.Fatalf("overlapping retries duplicated records: rows=%d batches=%d", rows, batches)
	}
}

func TestPaymentBatchLegacyPartialWriteRequiresReview(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	inputs[0].Amount, inputs[1].Amount = 10000, 20000
	// Older code could persist A before failing on B, without a durable batch
	// token. Replaying [A,B] cannot safely infer whether A was already applied.
	if err := d.InsertTransaction(ctx, inputs[0]); err != nil {
		t.Fatal(err)
	}
	if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); saved != 0 || !errors.Is(err, ErrPaymentBatchChanged) {
		t.Fatalf("legacy partial write was replayed: saved=%d err=%v", saved, err)
	}
	var count int
	var amount float64
	var parsed bool
	if err := d.pool.QueryRow(ctx, "SELECT COUNT(*),COALESCE(SUM(amount),0)::float8 FROM transactions WHERE raw_message_id=$1", rawID).Scan(&count, &amount); err != nil {
		t.Fatal(err)
	}
	if count != 1 || amount != 10000 {
		t.Fatalf("legacy partial payment duplicated: rows=%d total=%.0f; must stay one A10000", count, amount)
	}
	if _, batches := paymentBatchCounts(t, d, rawID); batches != 0 {
		t.Fatalf("review conflict incorrectly consumed batch token: %d", batches)
	}
	if err := d.pool.QueryRow(ctx, "SELECT parsed FROM raw_messages WHERE id=$1", rawID).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if parsed {
		t.Fatal("unresolved legacy partial message marked completely parsed")
	}
}

func TestPaymentBatchAllowsQuestionPhaseAfterCommittedAI(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	inputs[0].Amount, inputs[1].Amount = 10000, 20000
	if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs[:1]); saved != 1 || err != nil {
		t.Fatalf("initial AI phase: saved=%d err=%v", saved, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if saved, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "question", inputs[1:]); saved != 1 || err != nil {
			t.Fatalf("question phase attempt %d: saved=%d err=%v", attempt, saved, err)
		}
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 2 || batches != 2 {
		t.Fatalf("legitimate next phase blocked or duplicated: rows=%d batches=%d", rows, batches)
	}
	var amount float64
	if err := d.pool.QueryRow(ctx, "SELECT COALESCE(SUM(amount),0)::float8 FROM transactions WHERE raw_message_id=$1", rawID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != 30000 {
		t.Fatalf("wrong total across extraction phases: %.0f", amount)
	}
}
