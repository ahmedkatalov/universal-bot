package bot

import (
	"context"
	"strings"
	"testing"
)

func TestDuplicateImageWarnsAndRemainsInReceiptJournal(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, emp := e2eGroup("image-duplicate-journal"), user("79280000101")
	image := []byte("same-receipt-image-for-journal-test")
	ocr := duplicateNoticeReceipt("7000.10", "10:00:00", "IMAGE-JOURNAL-OPERATION-000001")
	e.sendReceiptMedia(grp, emp, "Мансур", ocr, "Исаев Харон", image)
	n := len(e.sent())
	wa := e.sendReceiptMedia(grp, emp, "Мансур", ocr, "Исаев Харон", image)
	receipt, err := e.db.ReceiptByWaMessageID(context.Background(), wa)
	if err != nil || !receipt.Found || receipt.Amount != 7000.10 {
		t.Fatalf("warned duplicate disappeared from journal: %+v err=%v", receipt, err)
	}
	warned := false
	for _, m := range e.sentSince(n) {
		if m.Quoted == wa && strings.Contains(m.Text, "уже присылали") {
			warned = true
		}
	}
	if !warned || sum(dayTotals(e, grp)) != 7000.10 {
		t.Fatalf("image duplicate warning or collection wrong: warned=%v totals=%v", warned, dayTotals(e, grp))
	}
}

func TestDuplicateImageDoesNotDiscardChangedClientCaption(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, emp := e2eGroup("image-client-correction"), user("79280000101")
	e.aliases.Add("Исаев Харон", "Исаев Харон")
	e.aliases.Add("Магомедов Ахмед", "Магомедов Ахмед")
	image := []byte("same-receipt-image-for-caption-test")
	ocr := duplicateNoticeReceipt("7000.10", "10:00:00", "IMAGE-CAPTION-OPERATION-000001")
	e.sendReceiptMedia(grp, emp, "Мансур", ocr, "Исаев Харон", image)
	n := len(e.sent())
	wa := e.sendReceiptMedia(grp, emp, "Мансур", ocr, "Магомедов Ахмед", image)
	var question e2eSent
	for _, m := range e.sentSince(n) {
		if m.Quoted == wa && strings.Contains(m.Text, "сейчас подписан") && strings.Contains(m.Text, "Магомедов Ахмед") {
			question = m
		}
	}
	if question.ID == "" {
		t.Fatalf("SHA shortcut swallowed changed client: %+v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Мансур", "да", question.ID, e.botJID(), question.Text))
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 7000.10 || sum(got) != 7000.10 {
		t.Fatalf("confirmed correction failed or doubled money: %v", got)
	}
}

func TestDuplicateOperationDifferentAmountsWarnsWithoutChoosingOne(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, emp := e2eGroup("operation-amount-conflict"), user("79280000101")
	doc := "AMOUNT-CONFLICT-OPERATION-000001"
	first := e.sendReceipt(grp, emp, "Мансур", duplicateNoticeReceipt("7000.10", "10:00:00", doc), "Исаев Харон")
	n := len(e.sent())
	wa := e.sendReceipt(grp, emp, "Мансур", duplicateNoticeReceipt("7000.20", "10:00:00", doc), "Исаев Харон")
	warned := false
	for _, m := range e.sentSince(n) {
		if m.Quoted == wa && strings.Contains(m.Text, "номером операции") && strings.Contains(m.Text, "различаются") {
			warned = true
		}
	}
	if !warned || sum(dayTotals(e, grp)) != 0 {
		t.Fatalf("conflict silent or arbitrary amount counted: warned=%v totals=%v messages=%+v", warned, dayTotals(e, grp), e.sentSince(n))
	}
	for _, item := range []struct {
		wa     string
		amount float64
	}{{first, 7000.10}, {wa, 7000.20}} {
		check, err := e.db.ReceiptByWaMessageID(context.Background(), item.wa)
		if err != nil || !check.Found || check.Amount != item.amount {
			t.Fatalf("conflicting evidence was overwritten: %+v err=%v", check, err)
		}
	}
}
