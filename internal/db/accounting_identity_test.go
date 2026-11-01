package db

import (
	"context"
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"
)

func TestEditedAttributionDoesNotFollowPositionOrPaymentKind(t *testing.T) {
	old := []TxState{{ID: 1, ContactID: 10, RawName: "Магомедов Ахмед", Amount: 1000.10, Cash: true, SourceRawID: 5, Collector: "Сафаи", DupDecided: true}}
	for _, n := range []TxState{
		{ID: 2, ContactID: 20, RawName: "Исаев Харон", Amount: 1000.10, Cash: true},
		{ID: 2, ContactID: 20, RawName: "Исаев Ахмед", Amount: 1000.10, Cash: true},
		{ID: 2, ContactID: 10, RawName: "Магомедов Ахмед", Amount: 1000.10, Cash: false},
	} {
		if got := matchEditedTxStates(old, []TxState{n}); len(got) != 0 {
			t.Fatalf("metadata crossed client or payment kind: new=%+v matches=%+v", n, got)
		}
	}
}

func accountingReceipt(t *testing.T, d *DB, group, waID, client, doc string, operation, posted time.Time) {
	t.Helper()
	ctx := context.Background()
	raw, _, err := d.SaveRawMessage(ctx, waID, group, "79280000001@s.whatsapp.net", "79280000001@s.whatsapp.net", "Мансур", client, true, "", posted)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := d.GetOrCreateContact(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO bank_receipts(raw_message_id,group_jid,contact_id,recipient_raw,amount,client_confirmed,doc_number,tx_date)
	 VALUES($1,$2,$3,$4,1000.10,true,$5,$6)`, raw, group, cid, client, doc, operation); err != nil {
		t.Fatal(err)
	}
}

func TestAccountingClientReportKeepsConflictingEvidence(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("client-conflict")
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a, b := "Клиент Первый "+group, "Клиент Другой "+group
	doc := "conflicting-doc-" + group
	accountingReceipt(t, d, group, group+"-1", a, doc, day.Add(time.Hour), day.Add(time.Hour))
	accountingReceipt(t, d, group+"-other", group+"-2", b, doc, day.Add(time.Hour), day.Add(2*time.Hour))
	to := day.AddDate(0, 0, 1)
	for _, client := range []string{a, b} {
		r, err := d.PersonReport(ctx, client, &day, &to, "")
		if err != nil || len(r) != 0 {
			t.Fatalf("client filter hid conflicting copy and attributed funds: client=%q rows=%+v err=%v", client, r, err)
		}
	}
}

func TestAccountingDocumentEvidenceSurvivesOldRepost(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("old-repost")
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old := day.AddDate(0, 0, -120)
	doc := "old-doc-" + group
	client := "Клиент Повтора " + group
	accountingReceipt(t, d, group, group+"-1", client, doc, old, old)
	accountingReceipt(t, d, group, group+"-2", client, doc, old, day.Add(time.Hour))
	book, err := d.LoadBook(ctx, day, day.AddDate(0, 0, 1), "")
	if err != nil {
		t.Fatal(err)
	}
	filter := ledger.Filter{From: day, To: day.AddDate(0, 0, 1), ByChat: true, Groups: map[string]bool{group: true}}
	if got := book.Counted(filter); len(got) != 0 {
		t.Fatalf("old repost became a fresh collection: %+v", got)
	}
}

func TestAccountingCardsDoNotChooseBetweenConflictingOperationDates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, _ := uniqueScope("card-date-conflict")
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	doc := "date-conflict-" + group
	client := "Клиент Даты " + group
	accountingReceipt(t, d, group, group+"-1", client, doc, day.Add(time.Hour), day.Add(time.Hour))
	accountingReceipt(t, d, group, group+"-2", client, doc, day.AddDate(0, 0, -3).Add(time.Hour), day.Add(2*time.Hour))
	for _, from := range []time.Time{day, day.AddDate(0, 0, -3)} {
		cards, err := d.CardTotals(ctx, from, from.AddDate(0, 0, 1), []string{group})
		if err != nil || len(cards) != 0 {
			t.Fatalf("card report guessed an operation date: from=%s cards=%+v err=%v", from, cards, err)
		}
	}
}

func TestEditedAttributionReorderingAndExplicitShortName(t *testing.T) {
	old := []TxState{
		{ID: 1, ContactID: 10, RawName: "Магомедов Ахмед", Amount: 1000.10, SourceRawID: 5},
		{ID: 2, ContactID: 20, RawName: "Исаев Харон", Amount: 1000.20},
	}
	cur := []TxState{
		{ID: 3, ContactID: 20, RawName: "Исаев Харон", Amount: 1000.20},
		{ID: 4, ContactID: 30, RawName: "Ахмед", Amount: 2000},
	}
	got := matchEditedTxStates(old, cur)
	if len(got) != 2 || got[3].ID != 2 || got[4].ID != 1 {
		t.Fatalf("lost proven client link on reordered amount edit: %+v", got)
	}
	old = append(old, TxState{ID: 5, ContactID: 40, RawName: "Исаев Ахмед", Amount: 3000, SourceRawID: 6})
	if got := matchEditedTxStates(old, cur); got[4].ID != 0 {
		t.Fatalf("ambiguous Ахмед arbitrarily assigned: %+v", got)
	}
}

func TestEditedAttributionDoesNotChooseBetweenIdenticalRows(t *testing.T) {
	old := []TxState{{ID: 1, ContactID: 10, RawName: "Клиент", Amount: 1000.10}, {ID: 2, ContactID: 10, RawName: "Клиент", Amount: 1000.20}}
	cur := []TxState{{ID: 3, ContactID: 10, RawName: "Клиент", Amount: 1000.20}, {ID: 4, ContactID: 10, RawName: "Клиент", Amount: 1000.10}}
	if got := matchEditedTxStates(old, cur); got[3].ID != 2 || got[4].ID != 1 {
		t.Fatalf("cents or reordering changed identity: %+v", got)
	}
	old[1].Amount = old[0].Amount
	cur[0].Amount = cur[1].Amount
	if got := matchEditedTxStates(old, cur); len(got) != 0 {
		t.Fatalf("ambiguous identical rows matched by position: %+v", got)
	}
}

func TestAccountingSummaryAddsWholeCents(t *testing.T) {
	var payments []*ledger.Payment
	for i := 0; i < 1000; i++ {
		payments = append(payments, &ledger.Payment{Client: "Клиент", Cash: true, Amount: 0.10})
	}
	summary := summarize(payments)
	if len(summary) != 1 || summary[0].Count != 1000 || summary[0].Total != 100 {
		t.Fatalf("cent accumulation drifted: %+v", summary)
	}
}

func TestAccountingCentsDoNotConsumeAnotherPayerName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	group, sender := uniqueScope("cents-name")
	base := time.Now().Add(-time.Minute)
	if err := d.EnqueuePendingName(ctx, group, sender, "Клиент Один", 1000.10, 0, base); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := d.TakePendingNameFor(ctx, group, sender, base.Add(-time.Minute), 1000.20); err != nil || ok {
		t.Fatalf("different amount consumed payer: %+v ok=%v err=%v", p, ok, err)
	}
	if p, ok, err := d.TakePendingNameFor(ctx, group, sender, base.Add(-time.Minute), 1000.10); err != nil || !ok || p.Name != "Клиент Один" {
		t.Fatalf("exact amount lost payer: %+v ok=%v err=%v", p, ok, err)
	}
}

func TestAccountingRejectsUnrepresentableMoneyBeforeCommit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	inputs[0].Amount = 1.005
	if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "ai", inputs); err == nil {
		t.Fatal("sub-kopeck batch was silently rounded")
	}
	if err := d.InsertTransaction(ctx, inputs[0]); err == nil {
		t.Fatal("sub-kopeck payment was silently rounded")
	}
	if rows, batches := paymentBatchCounts(t, d, rawID); rows != 0 || batches != 0 {
		t.Fatalf("invalid money partly committed: rows=%d batches=%d", rows, batches)
	}
}

func TestAccountingCentsCorrectionAndCaptionOnlyTouchExactPayment(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	inputs[0].Amount, inputs[1].Amount = 1000.10, 1000.20
	if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "deterministic", inputs); err != nil {
		t.Fatal(err)
	}
	var group, sender string
	if err := d.pool.QueryRow(ctx, "SELECT wa_group_jid,sender_jid FROM raw_messages WHERE id=$1", rawID).Scan(&group, &sender); err != nil {
		t.Fatal(err)
	}
	targets, err := d.CorrTargetsFromSender(ctx, group, sender, 1000.10, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(targets) != 1 || targets[0].Amount != 1000.10 {
		t.Fatalf("correction selected neighbouring cents: %+v err=%v", targets, err)
	}
	if n, err := d.IgnoreTextTxOfRaw(ctx, rawID, []float64{1000.10}); err != nil || n != 1 {
		t.Fatalf("receipt caption consumed other payment: n=%d err=%v", n, err)
	}
	remaining, err := d.TxRefsByRawID(ctx, rawID)
	if err != nil || len(remaining) != 1 || remaining[0].Amount != 1000.20 {
		t.Fatalf("unrelated cents payment disappeared: %+v err=%v", remaining, err)
	}
}

func TestEditedAttributionDifferentClientKeepsNewExplicitFacts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	rawID, inputs := paymentBatchFixture(t, d)
	inputs[0].IsCash = true
	if _, err := d.ApplyParsedPaymentBatch(ctx, rawID, 0, "deterministic", inputs[:1]); err != nil {
		t.Fatal(err)
	}
	rows, err := d.TxRefsByRawID(ctx, rawID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fixture: %+v %v", rows, err)
	}
	old := []TxState{{ID: rows[0].ID, ContactID: inputs[1].ContactID, RawName: inputs[1].RawName, Amount: inputs[0].Amount, Cash: true, SourceRawID: rawID, Collector: "Сафаи", DupDecided: true}}
	if err := d.CarryOverEditedTx(ctx, rawID, old); err != nil {
		t.Fatal(err)
	}
	var cid, source int
	var collector string
	if err := d.pool.QueryRow(ctx, "SELECT contact_id,COALESCE(client_source_raw_id,0),COALESCE(collector,'') FROM transactions WHERE id=$1", rows[0].ID).Scan(&cid, &source, &collector); err != nil {
		t.Fatal(err)
	}
	if cid != inputs[0].ContactID || source != 0 || collector != "" {
		t.Fatalf("old client's facts leaked into new payment: client=%d source=%d collector=%q", cid, source, collector)
	}
}
