package bot

import (
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"
)

// A disputed receipt is useful evidence for review, but not a confirmed
// grouping on which corrections may cascade into another client's message.
func TestConflictingReceiptCannotReassignPairedClientText(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	a := ledger.Check{ID: 1, WaID: "receipt-a", Group: "a", Client: "Магомедов Ахмед", ContactID: 1, Confirmed: true, Amount: 10000, Doc: "CONFLICT-CASCADE-001", OpDate: at, PostedAt: at.Add(time.Minute)}
	b := ledger.Check{ID: 2, WaID: "receipt-b", Group: "b", Client: "Исаев Харон", ContactID: 2, Confirmed: true, Amount: 10000, Doc: a.Doc, OpDate: at, PostedAt: at.Add(2 * time.Minute)}
	text := ledger.Text{ID: 3, Group: "b", Client: b.Client, ContactID: 2, Amount: 10000, OpDate: at, PostedAt: at.Add(3 * time.Minute)}
	book := ledger.Build([]ledger.Check{a, b}, []ledger.Text{text})
	if len(book.Payments) != 1 || !book.Payments[0].ReviewRequired() || len(book.Payments[0].Texts) != 1 {
		t.Fatalf("fixture lacks disputed paired evidence: %+v", book.Payments)
	}
	cid := 3
	if moved := crossPairedTextsOfBook(book, map[string]bool{a.WaID: true}, map[string]bool{"b": true}, "Эстамиров Ибрагим", &cid); len(moved) != 0 {
		t.Fatalf("uncertain pairing would rewrite another client's text: %+v", moved)
	}
	x := &payIndex{of: map[string]*ledger.Payment{}, fwd: map[int]bool{}, twins: map[*ledger.Payment][]*ledger.Text{}, splits: map[int]*splitTotal{}, partOf: map[string][]*splitTotal{}}
	x.add(book)
	if x.payment("receipt", a.ID) != nil || x.payment("receipt", b.ID) != nil || x.payment("transaction", text.ID) != nil {
		t.Fatal("manual correction index treats disputed evidence as confirmed income")
	}
	// Keep the normal correction path for an undisputed receipt/text pair.
	book = ledger.Build([]ledger.Check{b}, []ledger.Text{text})
	if moved := crossPairedTextsOfBook(book, map[string]bool{b.WaID: true}, map[string]bool{"b": true}, "Эстамиров Ибрагим", &cid); len(moved) != 1 || moved[0].id != text.ID {
		t.Fatalf("normal paired correction stopped working: %+v", moved)
	}
}

func TestExplicitCrossResolutionMovesOnlyPersistedSourceText(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.Local)
	a := ledger.Check{ID: 1, WaID: "confirmed-old-receipt", Group: "a", Client: "Магомедов Ахмед", ContactID: 1, Confirmed: true, Amount: 10000, Doc: "EXPLICIT-RESOLUTION-001", OpDate: at, PostedAt: at.Add(time.Minute)}
	b := ledger.Check{ID: 2, WaID: "conflicting-copy", Group: "b", Client: "Исаев Харон", ContactID: 2, Confirmed: true, Amount: 10000, Doc: a.Doc, OpDate: at, PostedAt: at.Add(2 * time.Minute)}
	old := ledger.Text{ID: 3, RawID: 103, Group: "a", Client: a.Client, ContactID: 1, Amount: 10000, OpDate: at, PostedAt: at}
	unlinked := ledger.Text{ID: 4, RawID: 104, Group: "b", Client: b.Client, ContactID: 2, Amount: 10000, OpDate: at, PostedAt: at.Add(3 * time.Minute)}
	book := ledger.Build([]ledger.Check{a, b}, []ledger.Text{old, unlinked})
	if len(book.Payments) != 1 || len(book.Payments[0].Texts) != 2 || !book.Payments[0].ReviewRequired() {
		t.Fatalf("fixture lacks conflicting linked texts: %+v", book.Payments)
	}
	cid := 3
	was := map[string]bool{a.WaID: true, b.WaID: true}
	groups := map[string]bool{"a": true, "b": true}
	sources := map[int]bool{old.RawID: true}
	if moved := crossPairedTextsForResolutionOfBook(book, was, groups, "Эстамиров Ибрагим", &cid, sources, false); len(moved) != 0 {
		t.Fatal("source link alone bypassed lack of explicit resolution")
	}
	moved := crossPairedTextsForResolutionOfBook(book, was, groups, "Эстамиров Ибрагим", &cid, sources, true)
	if len(moved) != 1 || moved[0].id != old.ID {
		t.Fatalf("explicit resolution lost the old link or guessed an unrelated text: %+v", moved)
	}
	book.Payments[0].AmountConflict = true
	if moved := crossPairedTextsForResolutionOfBook(book, was, groups, "Эстамиров Ибрагим", &cid, sources, true); len(moved) != 0 {
		t.Fatal("client-only answer silently resolved an amount conflict")
	}
}
