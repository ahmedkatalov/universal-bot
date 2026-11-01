package ledger

import (
	"math"
	"testing"
	"time"
)

func TestMoneyIdentityPreservesEveryKopeck(t *testing.T) {
	if sameMoney(10000.10, 10000.20) || sameMoney(10000.10, 10000.11) {
		t.Fatal("different kopecks were treated as the same money")
	}
	if amountKey(10000.10) == amountKey(10000.20) {
		t.Fatal("money index discarded kopecks")
	}
	x, y := 0.1, 0.2
	if !sameMoney(x+y, 0.3) {
		t.Fatal("binary float representation changed the same monetary amount")
	}
}

func TestLedgerRejectsFractionalKopecksAndInvalidAmounts(t *testing.T) {
	for _, a := range []float64{1.005, math.NaN(), math.Inf(1), -1, 1e10} {
		if _, valid := moneyMinor(a); valid {
			t.Fatalf("invalid monetary source accepted: %v", a)
		}
		book := Build([]Check{chk(1, "g", "Магомедов Ахмед", 1, a, t0, "")}, nil)
		if len(book.Counted(all)) != 0 {
			t.Fatalf("invalid amount reached confirmed totals: %v", a)
		}
	}
	for _, a := range []float64{0.29, 10000.11, 9999999999.97} {
		if _, valid := moneyMinor(a); !valid {
			t.Fatalf("valid kopecks rejected due to float representation: %.2f", a)
		}
	}
}

func TestDifferentKopecksNeverMergeChecksOrHideText(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 10000.10, t0, "")
	b := chk(2, "b", "Магомедов Ахмед", 1, 10000.20, t0, "")
	book := Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 2 {
		t.Fatalf("distinct transfers merged: %+v", book.Payments)
	}
	book = Build([]Check{a}, []Text{txt(1, "a", "Магомедов Ахмед", 1, 10000.20, t0.Add(2*time.Minute), false)})
	if len(book.Counted(all)) != 2 {
		t.Fatalf("different text amount hid a payment: %+v", book.Payments)
	}
}

func TestManualKopeckCorrectionWinsAcrossReceiptCopies(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 10000.10, t0, "MONEY-CORRECTION-0001")
	b := chk(2, "b", "Магомедов Ахмед", 1, 10000.15, t0, "MONEY-CORRECTION-0001")
	b.Fixed = true
	b.PostedAt = a.PostedAt.Add(time.Minute)
	book := Build([]Check{a, b}, nil)
	if len(book.Payments) != 1 || book.Payments[0].Amount != 10000.15 {
		t.Fatalf("manual 5-kopeck correction lost: %+v", book.Payments)
	}
}

func TestSplitTotalRequiresExactKopecks(t *testing.T) {
	checks := []Check{chk(1, "g", "Магомедов Ахмед", 1, 10000.10, t0, ""), chk(2, "g", "Магомедов Ахмед", 1, 5000.10, t0.Add(time.Minute), "")}
	for _, tc := range []struct {
		amount float64
		count  int
	}{{15000.20, 2}, {15000.21, 3}} {
		book := Build(append([]Check(nil), checks...), []Text{txt(1, "g", "Магомедов Ахмед", 1, tc.amount, t0.Add(5*time.Minute), false)})
		if len(book.Counted(all)) != tc.count {
			t.Fatalf("sum %.2f: count=%d, want %d", tc.amount, len(book.Counted(all)), tc.count)
		}
	}
}

func TestReceiptClientConflictNeverChoosesFirstPostedName(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "CLIENT-CONFLICT-001")
	b := chk(2, "b", "Исаев Харон", 2, 15000, t0, "CLIENT-CONFLICT-001")
	for _, order := range [][]Check{{a, b}, {b, a}, {func() Check { c := a; c.PostedAt = b.PostedAt.Add(time.Minute); return c }(), b}} {
		book := Build(order, nil)
		if len(book.Payments) != 1 || len(book.Payments[0].Conflicts) == 0 {
			t.Fatalf("physical receipt lost its conflict: %+v", book.Payments)
		}
		p := book.Payments[0]
		if p.Client != "" || p.ClientID != 0 {
			t.Fatalf("arbitrary first client remained authoritative: %+v", p)
		}
		for _, filter := range []Filter{all, inGroup("a"), inGroup("b")} {
			if len(book.Counted(filter)) != 0 {
				t.Fatalf("conflicting client was counted for group %v", filter.Groups)
			}
			if len(book.OldChecks(Filter{From: filter.From, To: filter.To, Groups: filter.Groups, ByChat: true, MonthStart: t0.AddDate(0, -1, 0)})) != 0 {
				t.Fatal("conflicting receipt escaped through old-check totals")
			}
		}
		roots := map[*Check]bool{}
		if len(book.Left(all)) != 2 {
			t.Fatalf("every conflicting caption must remain visible: %+v", book.Left(all))
		}
		for _, e := range book.Left(all) {
			if e.Reason != ReasonReview || e.Check == nil || e.Check.Confirmed || !e.Check.Review || e.Check.Client != "" || e.Amount() != 15000 || e.Into != p {
				t.Fatalf("conflict is not an unassigned review receipt: %+v", e)
			}
			roots[e.Root()] = true
		}
		if len(roots) != 1 {
			t.Fatal("copies doubled the unknown-client pending sum")
		}
	}
}

func TestShortFirstCaptionCannotBridgeConflictingFullClients(t *testing.T) {
	checks := []Check{chk(1, "a", "Ахмед", 1, 15000, t0, "CLIENT-BRIDGE-001"), chk(2, "b", "Магомедов Ахмед", 2, 15000, t0, "CLIENT-BRIDGE-001"), chk(3, "c", "Исаев Ахмед", 3, 15000, t0, "CLIENT-BRIDGE-001")}
	book := Build(checks, nil)
	if len(book.Counted(all)) != 0 || len(book.Payments) != 1 || len(book.Payments[0].Conflicts) == 0 {
		t.Fatalf("short name bridged two different clients: %+v", book.Payments)
	}
}

func TestConfirmedReviewCopyCannotOverrideAnotherClientSilently(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "CLIENT-REVIEW-CONFLICT-001")
	b := chk(2, "b", "Исаев Харон", 2, 0, t0, "CLIENT-REVIEW-CONFLICT-001")
	b.Review = true
	book := Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 0 || len(book.Payments) != 1 || len(book.Payments[0].Conflicts) == 0 {
		t.Fatalf("confirmed contradictory caption disappeared as incomplete copy: %+v", book.Payments)
	}
}

func TestUnknownAmountCopiesSharePendingIdentity(t *testing.T) {
	a := chk(1, "a", "", 0, 0, t0, "UNKNOWN-AMOUNT-COPY-001")
	a.Review, a.Confirmed = true, false
	b := chk(2, "b", "", 0, 0, t0, "UNKNOWN-AMOUNT-COPY-001")
	b.Review, b.Confirmed = true, false
	book := Build([]Check{a, b}, nil)
	roots := map[*Check]bool{}
	for _, e := range book.Left(all) {
		if e.Reason != ReasonNoAmount {
			t.Fatalf("wrong unknown-amount reason: %+v", e)
		}
		roots[e.Root()] = true
	}
	if len(book.Left(all)) != 2 || len(roots) != 1 {
		t.Fatalf("unknown amount copies counted as separate checks: %+v", book.Left(all))
	}
}

func TestSameLongDocAmountDisagreementHasNoArbitraryTotal(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "AMOUNT-DISAGREEMENT-001")
	b := chk(2, "b", "Магомедов Ахмед", 1, 18000, t0, "AMOUNT-DISAGREEMENT-001")
	for _, order := range [][]Check{{a, b}, {b, a}} {
		book := Build(order, nil)
		if len(book.Payments) != 1 || !book.Payments[0].AmountConflict || book.Payments[0].Amount != 0 || len(book.Counted(all)) != 0 {
			t.Fatalf("first OCR chose total for conflicting amount: %+v", book.Payments)
		}
		roots := map[*Check]bool{}
		for _, e := range book.Left(all) {
			if e.Reason != ReasonNoAmount || e.Check.Amount != 0 || e.Check.Client != "Магомедов Ахмед" {
				t.Fatalf("lost known client or invented amount: %+v", e)
			}
			roots[e.Root()] = true
		}
		if len(roots) != 1 {
			t.Fatal("amount conflict became two physical receipts")
		}
	}
	a.Fixed = true
	book := Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 1 || book.Payments[0].AmountConflict || book.Payments[0].Amount != 15000 {
		t.Fatal("manual amount correction did not outrank OCR")
	}
	b.Fixed = true
	book = Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 0 || !book.Payments[0].AmountConflict {
		t.Fatal("contradictory manual corrections chose the first one")
	}
}

func TestSameLongDocDifferentDaysIsOnePendingOperation(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "DATE-DISAGREEMENT-001")
	b := chk(2, "b", "Магомедов Ахмед", 1, 15000, t0.Add(72*time.Hour), "DATE-DISAGREEMENT-001")
	for _, order := range [][]Check{{a, b}, {b, a}} {
		book := Build(order, nil)
		if len(book.Payments) != 1 || !book.Payments[0].DateConflict || !book.Payments[0].OpDate.IsZero() || len(book.Counted(all)) != 0 {
			t.Fatalf("date conflict became a second operation or chose first day: %+v", book.Payments)
		}
		if len(book.Left(all)) != 2 {
			t.Fatal("contradictory dates disappeared from review")
		}
	}
}

func TestSameLongDocKnownDateOutranksGuessedPostingDate(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "DATE-KNOWN-001")
	a.TimeGuessed = true
	b := chk(2, "b", "Магомедов Ахмед", 1, 15000, t0.Add(-72*time.Hour), "DATE-KNOWN-001")
	b.PostedAt = a.PostedAt.Add(time.Minute)
	book := Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 1 || book.Payments[0].DateConflict || !book.Payments[0].OpDate.Equal(b.OpDate) {
		t.Fatalf("posting-time fallback outvoted real bank operation date: %+v", book.Payments)
	}
}

func TestForwardedOCRCannotDisputeOriginalMoneyOrDate(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 15000, t0, "ORIGINAL-COPY-001")
	b := chk(2, "b", "Исаев Харон", 2, 18000, t0.Add(72*time.Hour), "WRONG-COPY-002")
	b.FwdOrigin = a.WaID
	book := Build([]Check{a, b}, nil)
	if len(book.Counted(all)) != 1 || book.Payments[0].ReviewRequired() || book.Payments[0].Amount != 15000 {
		t.Fatalf("derived OCR overruled independent original: %+v", book.Payments)
	}
}
