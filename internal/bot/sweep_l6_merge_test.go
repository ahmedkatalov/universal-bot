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

// Регрессии слияния l6 (повторы чеков + свёрнутые экраны): «экран и полный
// чек — один перевод» решает одно место (replaceScreenWith) на всех путях —
// при приёме (и когда экран нашла проверка «уже присылали»), когда чек назвали
// потом, в группе на молчании, — а время операции, по которому не судим (на
// чеке одна дата), в сравнения не идёт.

// l6mTextReceipt — чек (распознанный текст ocr, без фото), присланный в момент
// at с подписью caption.
func l6mTextReceipt(e *e2eBot, grp, sender types.JID, ocr, caption string, at time.Time) string {
	ctx := context.Background()
	waID := e2eID("CHK")
	text := ocr
	if caption != "" {
		text = caption + "\n" + ocr
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), sender.String(), sender.User, "Сафаи", text, true, "", at)
	if err != nil {
		e.t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
		ID: waID, PushName: "Сафаи", Timestamp: at}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, sender.String(), waID, text, rawID, at, nil, ".jpg", payer, amt)
	e.waitIdle()
	return waID
}

// Экран «Перевод выполнен» с подписью в 11:00, потом чек того же клиента и
// суммы, на котором напечатана только дата (время выбрано — полдень), прислан
// в 13:00. «Экран прислан раньше операции» по выбранному времени не судим:
// один перевод (с вопросом), «другой» — два.
func TestL6merge_DateOnlyCheckAfterScreen(t *testing.T) {
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -1)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(day.Add(11 * time.Hour))
	grp, emp := e2eGroup("l6mdo"), user("79001126201")
	l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", day.Add(11*time.Hour))
	n := len(e.sent())
	ocr := "Почта Банк\nКвитанция о переводе\nДата операции: " + day.Format("02.01.2006") + "\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n5 000 ₽"
	ck := l6mTextReceipt(e, grp, emp, ocr, "Исаев Харон", day.Add(13*time.Hour))
	r := e.db.TestReceiptByMessage(context.Background(), ck)
	got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1))
	q := e.lastWith("ДРУГОЙ")
	if got != 5000 || q.ID == "" || len(e.sentSince(n)) == 0 {
		t.Fatalf("экран в 11:00 + чек «только дата» того же клиента в 13:00: сбор %v, ожидали 5000 и вопрос «другой?» (чек %+v, бот %v)", got, r, e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1)); got != 10000 {
		t.Errorf("«другой» — экран отдельный платёж: сбор %v, ожидали 10000 (бот %q)", got, e.last().Text)
	}
}

// Группа на молчании: подписанный экран, через полминуты — полный чек того же
// перевода (операция за 3 минуты до экрана: экран нашла проверка «уже
// присылали»). Полный чек в сборе, экран не прячется повтором молча, а ждёт в
// «НЕ вошло»; после снятия молчания — один вопрос, «другой» — два платежа.
func TestL6merge_MutedScreenFoundByDuplicateCheckParked(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -1)
	at := day.Add(12 * time.Hour)
	op := at.Add(-3 * time.Minute)
	grp, emp := e2eGroup("l6mmu"), user("79001126202")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l5collapsedVision(op)
	_ = e.setMuted(ctx, grp.String(), true)
	screen := l5collapsedPhotoAt(e, grp, emp, l6rScreen, "Исаев Харон", at)
	l5collapsedPhotoAt(e, grp, emp, l6rFull(op, "A6290101010101010G100400117744"), "Исаев Харон", at.Add(30*time.Second))
	total := func() float64 { return l4Total(e, []string{grp.String()}, day, day.AddDate(0, 0, 1)) }
	if got := total(); got != 5000 {
		t.Errorf("молчание, экран + полный чек того же перевода: сбор %v, ожидали 5000", got)
	}
	if r := e.db.TestReceiptByMessage(ctx, screen); r.Duplicate || !r.NeedsReview {
		t.Errorf("экран на молчании спрятан повтором молча (или посчитан): %+v", r)
	}
	if len(e.sent()) != 0 {
		t.Errorf("бот писал в группу на молчании: %v", e.sent())
	}
	_ = e.setMuted(ctx, grp.String(), false)
	e.age(grp.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	asked := 0
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "ДРУГОЙ") {
			asked++
		}
	}
	q := e.lastWith("ДРУГОЙ")
	if asked != 1 || q.ID == "" {
		t.Fatalf("после снятия молчания ожидали ровно один вопрос «тот же или другой?»: %v", e.sentSince(n))
	}
	if got := total(); got != 5000 {
		t.Errorf("после вопроса сбор %v, ожидали 5000", got)
	}
	e.clarify = newClarifyState() // перезапуск: ответ найдёт вопрос в базе
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := total(); got != 10000 {
		t.Errorf("«другой» после перезапуска: сбор %v, ожидали 10000 (экран %+v)", got, e.db.TestReceiptByMessage(ctx, screen))
	}
}
