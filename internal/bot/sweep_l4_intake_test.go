package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/parser"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Регрессии приёма чеков (l4 intake): свёрнутый экран другого клиента,
// один и тот же чек без подписи и с подписью, подпись при нечитаемой сумме.

// l4intakePhoto — чек-фото (с байтами картинки, чтобы сработали ветки
// «свёрнутый экран»/вижн), как его подаёт обработчик группы.
func l4intakePhoto(e *e2eBot, grp, sender types.JID, pushName, ocr, caption, mediaPath string) string {
	ctx := context.Background()
	waID := e2eID("CHK")
	text := ocr
	if caption != "" {
		text = caption + "\n" + ocr
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), sender.String(), sender.User, pushName, text, true, mediaPath, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
		ID:            waID, PushName: pushName, Timestamp: time.Now(),
	}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
	media := []byte("\xff\xd8\xff\xe0" + waID)
	if mediaPath != "" {
		if b, err := os.ReadFile(mediaPath); err == nil {
			media = b
		}
	}
	e.handleBankReceipt(ctx, grp, sender.String(), waID, text, rawID, time.Now(), media, ".jpg", payer, amt)
	e.waitIdle()
	return waID
}

// l4intakeVTB — чек ВТБ без номера операции (дубль ловится только по
// напечатанным полям: получатель, плательщик, сумма, время).
func l4intakeVTB(hm, payer, amount string) string {
	d := time.Now().Format("02.01.2006")
	return "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nДата операции " + d + ", " + hm +
		"\nИмя плательщика " + payer + "\nСумма операции " + amount + " ₽"
}

// l4intakeCollapsedNamed — работник прислал свёрнутый экран «Перевод выполнен»
// на 5000 без подписи и ответил на вопрос бота «Исаев Харон».
func l4intakeCollapsedNamed(t *testing.T, tag string) (*e2eBot, types.JID, types.JID) {
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			if strings.Contains(c.lastUserText(), "Перевод выполнен") {
				return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + day + ` 11:30:00"}`}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup(tag)
	emp := user("79001112002")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q := e.last()
	if !strings.Contains(q.Text, "НЕПОЛНЫМ") {
		t.Fatalf("нет вопроса про свёрнутый чек: %q", q.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Fatalf("свёрнутый чек после ответа не засчитан: %v", got)
	}
	return e, grp, emp
}

// Находка 15: свёрнутый экран, уже названный клиентом А, не стирается полным
// чеком того же работника на ту же сумму, подписанным клиентом Б. А полный чек
// ТОГО ЖЕ клиента или без подписи по-прежнему заменяет свёрнутый (платёж один).
func TestL4intake_15_CollapsedOfOtherClientKept(t *testing.T) {
	e, grp, emp := l4intakeCollapsedNamed(t, "coll")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", "11:30:00"), "Магомедов Ахмед", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("сбор %v, ожидали Исаев Харон 5000 + Магомедов Ахмед 5000", got)
	}
	for _, caption := range []string{"Исаев Харон", ""} {
		e, grp, emp := l4intakeCollapsedNamed(t, "coll2")
		l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", "11:30:00"), caption, "")
		if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || sum(got) != 5000 {
			t.Errorf("подпись %q: сбор %v, ожидали один платёж Исаев Харон 5000 (полный заменил свёрнутый)", caption, got)
		}
	}
}

// Находка 16: один и тот же чек без номера — сначала без подписи, потом с
// подписью; на «чей чек?» по первой копии ответили тем же клиентом. Платёж
// один, подписанная копия остаётся в учёте. А два РАЗНЫХ чека на ту же сумму
// той же карте (другое время, другой плательщик, другой получатель) — оба в
// учёте.
func TestL4intake_16_ResentWithCaptionCountedOnce(t *testing.T) {
	ctx := context.Background()
	run := func(tag, firstCheck, secondCheck string) (*e2eBot, types.JID, string, string) {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		grp := e2eGroup(tag)
		e.groupsCache[grp] = "Оплата"
		emp := user("79001112007")
		first := e.sendReceipt(grp, emp, "Сафаи", firstCheck, "")
		named := e.sendReceipt(grp, emp, "Сафаи", secondCheck, "Исаев Харон")
		e.age(grp.String())
		e.clarifyTick(ctx)
		if q := e.last(); strings.Contains(q.Text, "Чей это чек") {
			e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
		}
		return e, grp, first, named
	}
	vtb := l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")
	e, grp, _, named := run("resend", vtb, vtb)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 {
		t.Errorf("один чек, присланный дважды: в сборе %v, ожидали Исаев Харон 7000", got)
	}
	if r := e.db.TestReceiptByMessage(ctx, named); r.Recipient != "Исаев Харон" || r.Duplicate {
		t.Errorf("подписанная копия должна остаться в учёте: %+v", r)
	}
	otherRecipient := strings.Replace(vtb, "Хадижат Ибрагимовна И.", "Патимат Ахмедовна К.", 1)
	for _, tc := range []struct{ name, second string }{
		{"другое время", l4intakeVTB("13:09", "Харон Исаевич И.", "7 000")},
		{"другой плательщик", l4intakeVTB("13:02", "Зарема Ахмедовна М.", "7 000")},
		{"другой получатель", otherRecipient},
	} {
		e, grp, first, _ := run("two", vtb, tc.second)
		if r := e.db.TestReceiptByMessage(ctx, first); r.Duplicate {
			t.Errorf("%s: разный чек помечен дублем: %+v", tc.name, r)
		}
		if got := dayTotals(e, grp); got["Исаев Харон"] != 14000 {
			t.Errorf("%s: сбор %v, ожидали Исаев Харон 14000 (два разных чека)", tc.name, got)
		}
	}
}

// Находка 16: тот же чек (время с секундами), присланный без подписи ПОСЛЕ
// подписанного, — повтор, клиента не спрашиваем и дважды не считаем. Тот же
// чек, подписанный ДРУГИМ клиентом, — не склеиваем (разных клиентов не
// объединяем). Время только до минуты — см. R3: там спрашиваем.
func TestL4intake_16_UnnamedRepostAfterNamedIsDuplicate(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон", "Магомедов Ахмед")
	grp := e2eGroup("resend2")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112008")
	vtb := l4intakeVTB("13:02:41", "Харон Исаевич И.", "7 000")
	if r := parser.ParseReceipt(vtb); !r.HasTxTime || r.TxTime.Second() != 41 {
		t.Fatalf("время с секундами не прочиталось: %+v", r)
	}
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
	again := e.sendReceipt(grp, emp, "Сафаи", vtb, "")
	if r := e.db.TestReceiptByMessage(ctx, again); !r.Duplicate {
		t.Errorf("повтор того же чека без подписи не помечен дублем: %+v", r)
	}
	e.age(grp.String())
	e.clarifyTick(ctx)
	if q := e.last(); strings.Contains(q.Text, "Чей это чек") {
		t.Errorf("бот спрашивает «чей чек?» про повтор: %q", q.Text)
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || sum(got) != 7000 {
		t.Errorf("сбор %v, ожидали Исаев Харон 7000", got)
	}
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Магомедов Ахмед")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || got["Магомедов Ахмед"] != 7000 {
		t.Errorf("подписанные разными клиентами: сбор %v, ожидали оба по 7000", got)
	}

	// Подписанный оригинал удалили — копия без ФИО снова в работе (бот спросит,
	// чей чек), а не пропадает вместе с ним.
	e2 := newE2E(t, e2eOpts{WithAI: true})
	e2.ai = aiNamesAndPayments("Исаев Харон")
	grp2 := e2eGroup("resend3")
	e2.groupsCache[grp2] = "Оплата"
	named := e2.sendReceipt(grp2, emp, "Сафаи", vtb, "Исаев Харон")
	copyID := e2.sendReceipt(grp2, emp, "Сафаи", vtb, "")
	e2.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp2, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp2.String()), ID: proto.String(named), Participant: proto.String(emp.String())}}}})
	if r := e2.db.TestReceiptByMessage(ctx, copyID); r.Duplicate || !r.Found || !r.NeedsReview {
		t.Errorf("после удаления подписанного оригинала копия без ФИО должна снова ждать клиента: %+v", r)
	}
	if got := dayTotals(e2, grp2); sum(got) != 0 {
		t.Errorf("копия без ФИО ушла в сбор на владельца карты: %v", got)
	}
	e2.age(grp2.String())
	e2.clarifyTick(ctx)
	if q := e2.last(); !strings.Contains(q.Text, "Чей это чек") {
		t.Errorf("бот не спросил, чей чек, про оставшуюся копию: %q", q.Text)
	}
}

// Находка 17: сумма не прочиталась, ИИ лежал — подпись-клиент сохраняется,
// автоповтор не подменяет её владельцем карты.
func TestL4intake_17_RetryKeepsCaptionClient(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"A6290101010101010G10040011791103","status":"Выполнено","datetime":"` + day + ` 10:15:30"}`}
		}
		return aiReply{Text: ""}
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1") // ИИ недоступен
	grp := e2eGroup("retry")
	emp := user("79001112005")
	path := filepath.Join(t.TempDir(), "check.jpg")
	if err := os.WriteFile(path, []byte("\xff\xd8\xff\xe0 check photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	waID := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", path)
	e.assistant = healthy // ИИ вернулся
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	r := e.db.TestReceiptByMessage(ctx, waID)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("чек с подписью «Исаев Харон» после автоповтора: в сборе %v, ожидали Исаев Харон 5000 (чек: %+v)", got, r)
	}
}

// Находка 17 (обычный путь): на «не разобрал СУММУ» ответили только суммой —
// чек засчитан клиенту из подписи, бот не спрашивает «чей это чек?».
func TestL4intake_17_AmountAnswerKeepsCaptionClient(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			// Вижн тоже не прочитал сумму, но получателя видит.
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":0,"commission":0,"doc_number":"","status":"Выполнено","datetime":""}`}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup("amt")
	emp := user("79001112006")
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	waID := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", "")
	q := e.last()
	if !strings.Contains(q.Text, "СУММУ") {
		t.Fatalf("нет вопроса про сумму: %q", q.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "5000", q.ID, e.botJID(), q.Text))
	if a := e.last(); strings.Contains(a.Text, "чей") || strings.Contains(a.Text, "Чей") {
		t.Errorf("после ответа суммой бот спрашивает клиента, хотя он в подписи: %q", a.Text)
	}
	r := e.db.TestReceiptByMessage(ctx, waID)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("в сборе %v, ожидали Исаев Харон 5000 (чек: %+v)", got, r)
	}
}

// Находка 16 (повторный поиск при назначении клиента): первая копия пришла,
// когда ИИ лежал (сумма не прочиталась), вторую — тот же чек — прислали с ФИО.
// Автоповтор дочитал первую: теперь в группе две живые копии одного чека. Ответ
// «Исаев Харон» на вопрос по первой делает их одним платежом.
func TestL4intake_16_NamingAnswerDedupesLateReadCopy(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	names := aiNamesAndPayments("Исаев Харон")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"ВТБ","recipient":"Хадижат Ибрагимовна И.","sender":"Харон Исаевич И.","amount":7000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + day + ` 13:02:00"}`}
		}
		return names(c)
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1") // ИИ недоступен
	grp := e2eGroup("late")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112010")
	path := filepath.Join(t.TempDir(), "vtb.jpg")
	if err := os.WriteFile(path, []byte("\xff\xd8\xff\xe0 vtb photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	blurred := strings.Replace(l4intakeVTB("13:02", "Харон Исаевич И.", "7 000"), "7 000 ₽", "S OOO P", 1)
	first := l4intakePhoto(e, grp, emp, "Сафаи", blurred, "", path)
	q := e.lastWith("СУММУ")
	if q.ID == "" {
		t.Fatalf("нет вопроса про сумму: %+v", e.sent())
	}
	e.assistant = healthy // ИИ вернулся
	second := e.sendReceipt(grp, emp, "Сафаи", l4intakeVTB("13:02", "Харон Исаевич И.", "7 000"), "Исаев Харон")
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	if r := e.db.TestReceiptByMessage(ctx, first); r.Amount != 7000 {
		t.Fatalf("первая копия не дочитана: %+v", r)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || sum(got) != 7000 {
		t.Errorf("один чек в двух копиях: в сборе %v, ожидали Исаев Харон 7000 (1: %+v, 2: %+v)", got,
			e.db.TestReceiptByMessage(ctx, first), e.db.TestReceiptByMessage(ctx, second))
	}
}

// ---- Регрессии второго ревью (R0…R5) ----

// R0: ИИ лежал — чек с подписью «Исаев Харон» без суммы; тот же чек прислали
// ещё раз чётко (засчитан). Автоповтор дочитал первую копию с тем же номером
// документа — платёж один, а не два.
func TestL4intake_R0_RetryDoesNotDoubleCountResentCheck(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	doc := "A6290101010101010G10040011791103"
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"` + doc + `","status":"Выполнено","datetime":"` + day + ` 10:15:30"}`}
		}
		return aiReply{Text: ""}
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1") // ИИ недоступен
	grp := e2eGroup("rr")
	emp := user("79001112055")
	path := filepath.Join(t.TempDir(), "check.jpg")
	_ = os.WriteFile(path, []byte("\xff\xd8\xff\xe0 check photo"), 0o644)
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	first := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", path) // «не разобрал СУММУ»
	clear := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 10:15:30" +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\n" + doc
	second := e.sendReceipt(grp, emp, "Сафаи", clear, "Исаев Харон") // тот же чек, чётко — засчитан
	e.assistant = healthy
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("сбор %v, ожидали 5000 (1: %+v, 2: %+v)", got, e.db.TestReceiptByMessage(ctx, first), e.db.TestReceiptByMessage(ctx, second))
	}
}

// R0 (ответ суммой): размытый чек с подписью, потом тот же чек чётко с той же
// подписью, потом на «не разобрал СУММУ» ответили «5000». Дважды не считаем —
// бот спрашивает, тот же это чек или другой; «другой» — засчитывает отдельно.
func TestL4intake_R0_AmountAnswerDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	setup := func(tag string) (*e2eBot, types.JID, types.JID, string) {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
			if strings.Contains(c.System, "модуль разбора изображений") {
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":0,"commission":0,"doc_number":"","status":"Выполнено","datetime":""}`}
			}
			return aiReply{Text: ""}
		}
		grp := e2eGroup(tag)
		emp := user("79001112056")
		ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
		first := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", "")
		q := e.last()
		if !strings.Contains(q.Text, "СУММУ") {
			t.Fatalf("нет вопроса про сумму: %q", q.Text)
		}
		clear := "СберБанк\nЧек по операции\nДата операции " + time.Now().Add(-time.Minute).Format("02.01.2006 15:04:05") +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\nA6290101010101010G10040011791104"
		e.sendReceipt(grp, emp, "Сафаи", clear, "Исаев Харон")
		e.sendGroup(groupReply(grp, emp, "Сафаи", "5000", q.ID, e.botJID(), q.Text))
		return e, grp, emp, first
	}
	e, grp, _, first := setup("ra")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("сбор %v, ожидали 5000 (1: %+v)", got, e.db.TestReceiptByMessage(ctx, first))
	}
	a := e.last()
	if !strings.Contains(a.Text, "другой") {
		t.Errorf("бот не спросил, тот же ли это чек: %q", a.Text)
	}
	// «другой» — это отдельный платёж: засчитываем.
	e.sendGroup(groupReply(grp, user("79001112056"), "Сафаи", "другой", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("после «другой»: сбор %v, ожидали 10000", got)
	}
}

// R1: автоповтор прочитал на чеке с подписью «Операция отклонена» — денег нет,
// в сбор не идёт, в группе предупреждение.
func TestL4intake_R1_RetryDeclinedCaptionedCheckNotCounted(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Операция отклонена","datetime":"` + day + ` 10:15:30"}`}
		}
		return aiReply{Text: ""}
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	grp := e2eGroup("rf")
	emp := user("79001112062")
	path := filepath.Join(t.TempDir(), "check.jpg")
	_ = os.WriteFile(path, []byte("\xff\xd8\xff\xe0 declined photo"), 0o644)
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	waID := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", path)
	e.assistant = healthy
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Errorf("отклонённый перевод засчитан: %v (%+v)", got, e.db.TestReceiptByMessage(ctx, waID))
	}
	if a := e.last(); !strings.Contains(a.Text, "не прошёл") {
		t.Errorf("нет предупреждения об отклонённом переводе: %q", a.Text)
	}
}

// R2: на свёрнутый экран ответили коротким именем («Харон»/«Исаев»), потом
// прислали полный чек с подписью «Исаев Харон» — это тот же платёж.
func TestL4intake_R2_CollapsedShortNameReplacedByFullCheck(t *testing.T) {
	for _, tc := range []struct{ answer, caption string }{{"Харон", "Исаев Харон"}, {"Исаев", "Исаев Харон"}} {
		t.Run(tc.answer, func(t *testing.T) {
			e := newE2E(t, e2eOpts{WithAI: true})
			day := time.Now().Format("2006-01-02")
			e.ai = func(c aiCall) aiReply {
				if r, ok := fakeNameExtract(c); ok {
					return r
				}
				if strings.Contains(c.System, "модуль разбора изображений") {
					if strings.Contains(c.lastUserText(), "Перевод выполнен") {
						return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
					}
					return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + day + ` 11:30:00"}`}
				}
				return aiReply{Text: ""}
			}
			grp := e2eGroup("cv")
			emp := user("79001112057")
			l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
			q := e.last()
			e.sendGroup(groupReply(grp, emp, "Сафаи", tc.answer, q.ID, e.botJID(), q.Text))
			l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", "11:30:00"), tc.caption, "")
			if got := dayTotals(e, grp); sum(got) != 5000 {
				t.Errorf("один платёж: сбор %v, ожидали 5000", got)
			}
		})
	}
}

// R3 (а): по чеку без подписи бот спросил «чей чек?», следом пришёл такой же
// чек ВТБ (время до минуты) с подписью «Исаев Харон». На вопрос ответили
// «Исаева Марьям» — это другой клиент, оба платежа в учёте.
func TestL4intake_R3_AskedUnnamedAnswerNotDroppedByNamedTwin(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон", "Исаева Марьям")
	grp := e2eGroup("c5")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112066")
	vtb := l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.last()
	if !strings.Contains(q.Text, "Чей это чек") {
		t.Fatalf("нет вопроса: %q", q.Text)
	}
	n := len(e.sent())
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаева Марьям", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || got["Исаева Марьям"] != 7000 {
		t.Errorf("ответ пропал (бот сказал %v): сбор %v", e.sentSince(n), got)
	}
}

// R3 (б): подписанный чек ВТБ, потом такой же без подписи (время до минуты —
// может быть второй перевод в ту же минуту): бот не решает молча «повтор», а
// спрашивает, чей чек; ответ «Исаева Марьям» — оба платежа в учёте.
func TestL4intake_R3_MinuteTwinWithoutCaptionAsked(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон", "Исаева Марьям")
	grp := e2eGroup("c6")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112067")
	vtb := l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "")
	if a := e.last(); strings.Contains(a.Text, "уже присылали") {
		t.Errorf("чек с временем до минуты молча признан повтором: %q", a.Text)
	}
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.last()
	if !strings.Contains(q.Text, "Чей это чек") {
		t.Fatalf("нет вопроса: %q", q.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаева Марьям", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || got["Исаева Марьям"] != 7000 {
		t.Errorf("сбор %v, ожидали Исаев Харон 7000 + Исаева Марьям 7000", got)
	}
}

// R4: свёрнутый 5000 назван «Исаев Харон», второй свёрнутый 5000 ещё ничей;
// полный чек с подписью «Исаев Харон» заменяет свёрнутый ТОГО ЖЕ клиента, а не
// ничей.
func TestL4intake_R4_FullCheckReplacesSameClientCollapsedFirst(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			if strings.Contains(c.lastUserText(), "Перевод выполнен") {
				return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + day + ` 11:30:00"}`}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup("ce")
	emp := user("79001112059")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q := e.last()
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	other := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", "11:30:00"), "Исаев Харон", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("сбор %v", got)
	}
	if r := e.db.TestReceiptByMessage(context.Background(), other); r.Duplicate {
		t.Errorf("ничей свёрнутый (другой платёж) пропал из «не вошло»: %+v", r)
	}
}

// R5: один и тот же чек дважды без подписи, первый удалили — оставшаяся копия
// снова ждёт ответа «чей чек?», а не уходит в сбор на владельца карты.
func TestL4intake_R5_UnnamedCopyPromotedNotCountedToCardOwner(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, check string }{
		{"Сбер", sberCheck("7000.00", "13:02:11")},
		{"ВТБ", l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		grp := e2eGroup("pp")
		e.groupsCache[grp] = "Оплата"
		emp := user("79001112065")
		first := e.sendReceipt(grp, emp, "Сафаи", tc.check, "")
		second := e.sendReceipt(grp, emp, "Сафаи", tc.check, "")
		e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
			Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
				Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(first), Participant: proto.String(emp.String())}}}})
		if got := dayTotals(e, grp); sum(got) != 0 {
			t.Errorf("%s: копия без ФИО ушла в сбор на владельца карты: %v (%+v)", tc.name, got, e.db.TestReceiptByMessage(ctx, second))
		}
		if r := e.db.TestReceiptByMessage(ctx, second); !r.Found || r.Duplicate || !r.NeedsReview {
			t.Errorf("%s: копия должна снова ждать клиента: %+v", tc.name, r)
		}
	}
}

// R0 (копия без подписи): ИИ лежал — чек с подписью без суммы; тот же чек
// прислали чётко, но без подписи (бот спросит «чей чек?»). Автоповтор дочитал
// первый с тем же номером документа: платёж один, на клиента из подписи, а
// неподписанная копия — повтор (про неё не спрашиваем).
func TestL4intake_R0_RetryMakesUnnamedCopyDuplicate(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	day := time.Now().Format("2006-01-02")
	doc := "A6290101010101010G10040011791105"
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"` + doc + `","status":"Выполнено","datetime":"` + day + ` 10:15:30"}`}
		}
		return aiReply{Text: ""}
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	grp := e2eGroup("ru")
	emp := user("79001112068")
	path := filepath.Join(t.TempDir(), "check.jpg")
	_ = os.WriteFile(path, []byte("\xff\xd8\xff\xe0 check photo"), 0o644)
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", path)
	clear := "СберБанк\nЧек по операции\nДата операции " + time.Now().Format("02.01.2006") + " 10:15:30" +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\n" + doc
	unnamed := e.sendReceipt(grp, emp, "Сафаи", clear, "")
	e.assistant = healthy
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || sum(got) != 5000 {
		t.Errorf("сбор %v, ожидали Исаев Харон 5000", got)
	}
	if r := e.db.TestReceiptByMessage(ctx, unnamed); !r.Duplicate {
		t.Errorf("неподписанная копия того же чека не стала повтором: %+v", r)
	}
	e.age(grp.String())
	e.clarifyTick(ctx)
	if q := e.last(); strings.Contains(q.Text, "Чей это чек") {
		t.Errorf("бот спрашивает «чей чек?» про повтор: %q", q.Text)
	}
}

// R0 (ответ суммой): «тот же» — остаётся один платёж; чек, сделанный ПОЗЖЕ,
// чем прислали размытый, — точно другой платёж: засчитан без вопроса.
func TestL4intake_R0_AmountAnswerSameOrLater(t *testing.T) {
	run := func(tag string, clearAt time.Time) (*e2eBot, types.JID, types.JID) {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
			if strings.Contains(c.System, "модуль разбора изображений") {
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":0,"commission":0,"doc_number":"","status":"Выполнено","datetime":""}`}
			}
			return aiReply{Text: ""}
		}
		grp := e2eGroup(tag)
		emp := user("79001112069")
		ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
		l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", "")
		q := e.last()
		clear := "СберБанк\nЧек по операции\nДата операции " + clearAt.Format("02.01.2006 15:04:05") +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\n" + e2eID("DOC")
		e.sendReceipt(grp, emp, "Сафаи", clear, "Исаев Харон")
		e.sendGroup(groupReply(grp, emp, "Сафаи", "5000", q.ID, e.botJID(), q.Text))
		return e, grp, emp
	}
	e, grp, emp := run("rs", time.Now().Add(-2*time.Minute))
	a := e.last()
	e.sendGroup(groupReply(grp, emp, "Сафаи", "тот же", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("«тот же»: сбор %v, ожидали 5000", got)
	}
	e, grp, _ = run("rl", time.Now().Add(5*time.Minute))
	if a := e.last(); strings.Contains(a.Text, "другой") {
		t.Errorf("чек сделан позже размытого — это другой платёж, а бот спрашивает: %q", a.Text)
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("два разных платежа: сбор %v, ожидали 10000", got)
	}
}

// R3 (время с секундами): по неподписанному чеку спросили «чей чек?», следом
// пришёл тот же чек с подписью — бот говорит, что считает его один раз. Если
// на вопрос всё же назвали ДРУГОГО клиента — ответ не теряется: разных
// клиентов не склеиваем, оба платежа в учёте. Тем же клиентом — один платёж.
func TestL4intake_R3_SecondsTwinAskedAnswerKept(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		answer string
		want   map[string]float64
	}{
		{"Исаева Марьям", map[string]float64{"Исаев Харон": 7000, "Исаева Марьям": 7000}},
		{"Исаев Харон", map[string]float64{"Исаев Харон": 7000}},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон", "Исаева Марьям")
		grp := e2eGroup("c7")
		e.groupsCache[grp] = "Оплата"
		emp := user("79001112070")
		vtb := l4intakeVTB("13:02:41", "Харон Исаевич И.", "7 000")
		e.sendReceipt(grp, emp, "Сафаи", vtb, "")
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.last()
		if !strings.Contains(q.Text, "Чей это чек") {
			t.Fatalf("нет вопроса: %q", q.Text)
		}
		e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
		if a := e.last(); !strings.Contains(a.Text, "один раз") {
			t.Errorf("бот не сказал, что вопрос по неподписанной копии снят: %q", a.Text)
		}
		n := len(e.sent())
		e.sendGroup(groupReply(grp, emp, "Сафаи", tc.answer, q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); len(got) != len(tc.want) || got["Исаев Харон"] != tc.want["Исаев Харон"] || got["Исаева Марьям"] != tc.want["Исаева Марьям"] {
			t.Errorf("ответ %q (бот сказал %v): сбор %v, ожидали %v", tc.answer, e.sentSince(n), got, tc.want)
		}
		if len(e.sentSince(n)) == 0 {
			t.Errorf("ответ %q остался без реакции бота", tc.answer)
		}
	}
}

// R3 (время до минуты): неподписанная копия подписанного чека — бот спросил
// «чей чек?», ответили тем же клиентом: один платёж, и бот спрашивает, не
// другой ли это перевод; «другой» — засчитывает второй.
func TestL4intake_R3_MinuteTwinSameClientAsksSameOrOther(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	grp := e2eGroup("c8")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112071")
	vtb := l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.last()
	if !strings.Contains(q.Text, "Чей это чек") {
		t.Fatalf("нет вопроса: %q", q.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 {
		t.Errorf("тот же клиент: сбор %v, ожидали 7000", got)
	}
	a := e.last()
	if !strings.Contains(a.Text, "другой") {
		t.Fatalf("бот не спросил, тот же ли это перевод: %q", a.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "нет, другой", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 14000 {
		t.Errorf("«другой»: сбор %v, ожидали 14000", got)
	}
}

// R0 (автоповтор без даты и номера): ИИ лежал — размытый чек с подписью; тот
// же клиент прислал чёткий чек (засчитан). Автоповтор прочитал только сумму —
// уверенности, что это тот же чек, нет: бот не считает второй раз и
// спрашивает. Ответ на старый вопрос «какая сумма?» не задваивает; «другой» —
// засчитывает отдельно.
func TestL4intake_R0_RetryWithoutDateAsksSameOrOther(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Выполнено","datetime":""}`}
		}
		return aiReply{Text: ""}
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	grp := e2eGroup("rt")
	emp := user("79001112072")
	path := filepath.Join(t.TempDir(), "check.jpg")
	_ = os.WriteFile(path, []byte("\xff\xd8\xff\xe0 check photo"), 0o644)
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	first := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", path)
	q := e.lastWith("СУММУ")
	clear := "СберБанк\nЧек по операции\nДата операции " + time.Now().Add(-3*time.Minute).Format("02.01.2006 15:04:05") +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\n" + e2eID("DOC")
	e.sendReceipt(grp, emp, "Сафаи", clear, "Исаев Харон")
	e.assistant = healthy
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("после автоповтора: сбор %v, ожидали 5000 (%+v)", got, e.db.TestReceiptByMessage(ctx, first))
	}
	a := e.last()
	if !strings.Contains(a.Text, "другой") {
		t.Fatalf("бот не спросил, тот же ли это чек: %q", a.Text)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "5000", q.ID, e.botJID(), q.Text)) // ответ на старый вопрос
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("ответ на старый вопрос задвоил: сбор %v", got)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("«другой»: сбор %v, ожидали 10000", got)
	}
}

// R3 (б, время с секундами): подписанный чек, потом такой же без подписи —
// повтор, но с путём назад: ответ «Это другой чек, Исаева Марьям» на
// предупреждение засчитывает второй платёж на названного клиента.
func TestL4intake_R3_RepeatWarningAnswerOtherClient(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон", "Исаева Марьям")
	grp := e2eGroup("c9")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112073")
	vtb := l4intakeVTB("13:02:41", "Харон Исаевич И.", "7 000")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "Исаев Харон")
	e.sendReceipt(grp, emp, "Сафаи", vtb, "")
	w := e.lastWith("уже присылали")
	if w.ID == "" || !strings.Contains(w.Text, "другой") {
		t.Fatalf("нет предупреждения о повторе с путём назад: %+v", e.sent())
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || sum(got) != 7000 {
		t.Errorf("повтор: сбор %v, ожидали Исаев Харон 7000", got)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Это другой чек, Исаева Марьям", w.ID, e.botJID(), w.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 7000 || got["Исаева Марьям"] != 7000 {
		t.Errorf("«другой чек, Исаева Марьям»: сбор %v (бот: %q)", got, e.last().Text)
	}
}

// R0 (удаление): «похоже, тот же чек» — размытая копия не в сборе; чёткий чек
// удалили — платёж не пропадает, размытая копия снова в учёте.
func TestL4intake_R0_TentativeCopyPromotedOnDelete(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":0,"commission":0,"doc_number":"","status":"Выполнено","datetime":""}`}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup("rd")
	emp := user("79001112074")
	ocr := "C6EP6AHK\nЧek no onepaции\nФИO получателя\nХадижат Ибрагимовна И.\nСумма перевода\nS OOO,OO P"
	first := l4intakePhoto(e, grp, emp, "Сафаи", ocr, "Исаев Харон", "")
	q := e.last()
	clear := "СберБанк\nЧек по операции\nДата операции " + time.Now().Add(-time.Minute).Format("02.01.2006 15:04:05") +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер документа\n" + e2eID("DOC")
	second := e.sendReceipt(grp, emp, "Сафаи", clear, "Исаев Харон")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "5000", q.ID, e.botJID(), q.Text))
	if r := e.db.TestReceiptByMessage(ctx, first); !r.Duplicate {
		t.Fatalf("размытая копия не признана повтором: %+v", r)
	}
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(second), Participant: proto.String(emp.String())}}}})
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("после удаления чёткого чека платёж пропал: сбор %v (%+v)", got, e.db.TestReceiptByMessage(ctx, first))
	}
}
