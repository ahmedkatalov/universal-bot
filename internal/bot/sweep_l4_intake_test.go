package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ai"

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

// Находка 16: тот же чек, присланный без подписи ПОСЛЕ подписанного, —
// повтор, клиента не спрашиваем и дважды не считаем. Тот же чек, подписанный
// ДРУГИМ клиентом, — не склеиваем (разных клиентов не объединяем).
func TestL4intake_16_UnnamedRepostAfterNamedIsDuplicate(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон", "Магомедов Ахмед")
	grp := e2eGroup("resend2")
	e.groupsCache[grp] = "Оплата"
	emp := user("79001112008")
	vtb := l4intakeVTB("13:02", "Харон Исаевич И.", "7 000")
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
