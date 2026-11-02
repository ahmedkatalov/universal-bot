package bot

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestReceiptDocumentCaptionsOnlyForSupportedMedia(t *testing.T) {
	for _, tc := range []struct {
		mime, file string
		want       string
	}{
		{"image/jpeg", "receipt.jpg", "Ханатаев акраман 30000"},
		{"application/pdf", "receipt.pdf", "Ханатаев акраман 30000"},
		{"", "receipt.PDF", "Ханатаев акраман 30000"},
		{"application/msword", "agreement.doc", ""},
	} {
		m := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			Mimetype: proto.String(tc.mime), FileName: proto.String(tc.file), Caption: proto.String("Ханатаев акраман 30000"),
		}}
		if got := extractText(m); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestScreenshotSberVisionFailureThenReceiptRetry(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "3")
	t.Setenv("RECEIPT_VISION_FIRST", "1")
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
	grp, worker := e2eGroup("sber-photo-retry-nov1"), user("79280000706")
	path := filepath.Join(t.TempDir(), "sber.jpg")
	if err := os.WriteFile(path, []byte("photographed-sber-receipt-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ocr := `СБЕРБАНК
Чек по операции
1 ноября 2026 14:23:06(МСК)
ФИО получателя
Милана Нажудовна К.
ФИО отправителя
Элина Зилаудиевна Г.
Сумма перевода
30000.00 ₽
Комиссия
0.00 ₽
Номер документа
1000000006086701079
Код авторизации
024307`
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{}`} }
	waID := l4intakePhoto(e, grp, worker, "~M", ocr, "Ханатаев акраман", path)
	ref, err := e.db.RawMessageByWaID(ctx, waID)
	if err != nil || !ref.Found {
		t.Fatalf("source was not saved: %+v / %v", ref, err)
	}
	source, err := proto.Marshal(&waProto.Message{ImageMessage: &waProto.ImageMessage{
		Caption: proto.String("Ханатаев акраман"), DirectPath: proto.String("/receipt?test=1"), MediaKey: []byte("test-media-key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetMediaInfo(ctx, ref.ID, "Ханатаев акраман", source); err != nil {
		t.Fatal(err)
	}
	r := e.db.TestReceiptByMessage(ctx, waID)
	if !r.Found || r.Amount != 30000 || !r.NeedsReview || !r.Confirmed {
		t.Fatalf("receipt evidence or retry review state was lost: %+v", r)
	}
	from := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	rows, err := e.db.ReceiptsLedger(ctx, from, from.AddDate(0, 0, 1), "", []string{grp.String()}, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed image verification activated OCR income: %+v / %v", rows, err)
	}
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("Sber receipt was not queued for recognition retry: %+v / %v", items, err)
	}
	if items[0].RawID != ref.ID || items[0].WaID != waID || len(items[0].Proto) == 0 {
		t.Fatalf("retry lost its WhatsApp download source: %+v", items[0])
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.retryOneReceipt(ctx, items[0])
	items, err = e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("failed redownload prematurely closed pending receipt: %+v / %v", items, err)
	}
	if err := os.WriteFile(path, []byte("photographed-sber-receipt-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	// A caption/source change while the vision call is in flight invalidates
	// that result. A fresh retry may read the updated source safely.
	var edited sync.Once
	e.ai = func(c aiCall) aiReply {
		edited.Do(func() {
			if err := e.db.SetMessageBody(ctx, ref.ID, "Ханатаев акраман\nизменённая подпись"); err != nil {
				t.Error(err)
			}
		})
		return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Милана Нажудовна К.","sender":"Элина Зилаудиевна Г.","amount":30000,"commission":0,"doc_number":"1000000006086701079","auth_code":"024307","datetime":"2026-11-01 14:23:06"}`}
	}
	e.retryOneReceipt(ctx, items[0])
	if r := e.db.TestReceiptByMessage(ctx, waID); !r.NeedsReview {
		t.Fatal("stale vision result was applied after the receipt source changed")
	}
	items, err = e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("changed source did not remain retryable: %+v / %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	rows, err = e.db.ReceiptsLedger(ctx, from, from.AddDate(0, 0, 1), "", []string{grp.String()}, 10)
	if err != nil || len(rows) != 1 || rows[0].Client != "Ханатаев Акраман" || rows[0].Amount != 30000 {
		t.Fatalf("retry did not recover the Sber payment: %+v / %v", rows, err)
	}
	r = e.db.TestReceiptByMessage(ctx, waID)
	if r.NeedsReview || !r.Confirmed || r.Duplicate || r.TxDate.In(from.Location()).Format("2006-01-02 15:04:05") != "2026-11-01 14:23:06" {
		t.Fatalf("recovered receipt lost its client, identity or printed time: %+v", r)
	}
}
