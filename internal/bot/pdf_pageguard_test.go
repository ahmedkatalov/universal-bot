package bot

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jung-kurt/gofpdf"
)

func receiptPDF(t *testing.T, pages int) []byte {
	t.Helper()
	pdf := gofpdf.New("P", "mm", "A4", "")
	for n := 0; n < pages; n++ {
		pdf.AddPage()
		pdf.SetFont("Arial", "", 12)
		pdf.Cell(0, 10, "Bank receipt 15000")
	}
	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPDFPageCountRejectsIncompleteReading(t *testing.T) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("pdfinfo is required")
	}
	for _, pages := range []int{1, 2} {
		got, err := pdfPageCount(context.Background(), receiptPDF(t, pages))
		if err != nil || got != pages {
			t.Fatalf("%d pages: got=%d %v", pages, got, err)
		}
	}
	if _, err := pdfPageCount(context.Background(), []byte("not a pdf")); err == nil {
		t.Fatal("accepted broken PDF")
	}
}

func TestMultiPagePDFRemainsVisibleWithoutCountingFirstPage(t *testing.T) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("pdfinfo is required")
	}
	e := newE2E(t, e2eOpts{})
	ctx := context.Background()
	group := e2eGroup("pdf-bundle")
	sender := user("79280000990")
	waID := e2eID("PDF")
	raw, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), sender.String(), sender.User, "Мансур", "Каталов Ахмед 15000", true, "/tmp/bundle.pdf", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, group, sender.String(), waID, sberCheck("15000", "10:00:00"), raw, time.Now(), receiptPDF(t, 2), ".pdf", "Каталов Ахмед", 15000)
	state, err := e.db.GroupEventRawState(ctx, waID)
	if err != nil || state.Parsed || state.HandledAs != "pdf_multiple_pages" || state.HasReceipt {
		t.Fatalf("bundle activated or hidden: %+v %v", state, err)
	}
	if !strings.Contains(e.last().Text, "2 страниц") {
		t.Fatalf("no actionable explanation: %q", e.last().Text)
	}
	status, err := e.db.GroupReading(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{group.String()}, 10)
	if err != nil || status.UnreadMedia != 1 || status.UnresolvedMessages != 1 {
		t.Fatalf("missing from reading report: %+v %v", status, err)
	}
}
