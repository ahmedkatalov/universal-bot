package bot

import (
	"context"
	"strings"
	"testing"
)

func duplicateNoticeReceipt(amount, clock, doc string) string {
	s := sberCheck(amount, clock)
	return s[:strings.LastIndex(s, "\n")+1] + doc
}

func TestDuplicateNoticeUsesSameNormalizedOperationAsLedger(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, emp := e2eGroup("duplicate-document-notice"), user("79280000101")
	first := e.sendReceipt(grp, emp, "Мансур", duplicateNoticeReceipt("15000.25", "10:00:00", "а63042034027580i0b10070011881102"), "Исаев Харон")
	initial, _ := e.db.ReceiptByWaMessageID(context.Background(), first)
	t.Logf("initial receipt: %+v", initial)
	n := len(e.sent())
	wa := e.sendReceipt(grp, emp, "Мансур", duplicateNoticeReceipt("15000.25", "10:30:00", "A63042034027580I0B10070011881102"), "Исаев Харон")
	later, _ := e.db.ReceiptByWaMessageID(context.Background(), wa)
	t.Logf("later receipt: %+v", later)
	warned := false
	for _, m := range e.sentSince(n) {
		if m.Quoted == wa && strings.Contains(m.Text, "уже присылали") && strings.Contains(m.Text, "15000.25") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("ledger recognizes operation but live warning is missing: %+v", e.sentSince(n))
	}
	if got := sum(dayTotals(e, grp)); got != 15000.25 {
		t.Fatalf("normalized duplicate changed collection: %v", got)
	}
}

func TestDuplicateNoticeIndependentOfUnnamedQuestions(t *testing.T) {
	t.Setenv("ASK_UNNAMED_RECEIPTS", "0")
	e := newE2E(t, e2eOpts{})
	grp, emp := e2eGroup("duplicate-no-questions"), user("79280000101")
	ocr := duplicateNoticeReceipt("7000.10", "10:00:00", "DUPLICATE-NOTICE-QUESTIONS-001")
	e.sendReceipt(grp, emp, "Мансур", ocr, "Исаев Харон")
	n := len(e.sent())
	wa := e.sendReceipt(grp, emp, "Мансур", ocr, "Исаев Харон")
	for _, m := range e.sentSince(n) {
		if m.Quoted == wa && strings.Contains(m.Text, "уже присылали") {
			return
		}
	}
	t.Fatalf("disabling unnamed questions silenced duplicate warning: %+v", e.sentSince(n))
}
