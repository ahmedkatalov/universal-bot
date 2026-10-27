package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// sendReceiptMedia — как sendReceipt, но с БАЙТАМИ картинки (для отлова повтора
// по хэшу). Возвращает wa id.
func (e *e2eBot) sendReceiptMedia(grp, sender types.JID, pushName, ocrText, caption string, media []byte) string {
	ctx := context.Background()
	waID := e2eID("CHK")
	text := ocrText
	if caption != "" {
		text = caption + "\n" + ocrText
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), sender.String(), sender.User, pushName, text, true, "", time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
		ID:            waID, PushName: pushName, Timestamp: time.Now(),
	}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, sender.String(), waID, text, rawID, time.Now(), media, ".jpg", payer, payerAmount)
	e.waitIdle()
	return waID
}

// Та же картинка чека, присланная второй раз: бот сразу предупреждает о повторе
// и в сбор не считает дважды.
func TestL9_SameImageWarnedAndNotDoubleCounted(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("dupimg")
	u := user("79287836800")
	img := []byte("\xff\xd8\xff\xe0JFIF-одна-и-та-же-картинка-чека-0001")
	ocr := sberCheck("15000.00", "10:00:00")

	e.sendReceiptMedia(grp, u, "Сафаи", ocr, "Магомедов Ахмед", img)
	if got := sum(dayTotals(e, grp)); got != 15000 {
		t.Fatalf("первый чек: сбор %v, хочу 15000", got)
	}

	n := len(e.sent())
	e.sendReceiptMedia(grp, u, "Сафаи", ocr, "Магомедов Ахмед", img) // та же картинка
	if got := sum(dayTotals(e, grp)); got != 15000 {
		t.Errorf("та же картинка второй раз: сбор %v, хочу 15000 (не дважды)", got)
	}
	warned := false
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "уже присылали") || strings.Contains(strings.ToLower(m.Text), "повтор") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("бот не предупредил о повторной картинке: %v", e.sentSince(n))
	}

	// Другая картинка того же клиента на ту же сумму в другую минуту — НЕ повтор
	// по хэшу (это может быть второй реальный перевод); хэш не должен мешать.
	n = len(e.sent())
	img2 := []byte("\xff\xd8\xff\xe0JFIF-совсем-другая-картинка-0002")
	e.sendReceiptMedia(grp, u, "Сафаи", sberCheck("15000.00", "10:30:00"), "Магомедов Ахмед", img2)
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "уже присылали") {
			t.Errorf("другая картинка ошибочно названа повтором по хэшу: %q", m.Text)
		}
	}
}
