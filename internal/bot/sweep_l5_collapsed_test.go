package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// Регрессии l5: свёрнутые экраны «Перевод выполнен» и полные чеки того же
// (или другого) перевода.

// l5collapsedPhotoAt — чек-фото, присланное в момент at (как его подаёт
// обработчик группы).
func l5collapsedPhotoAt(e *e2eBot, grp, sender types.JID, ocr, caption string, at time.Time) string {
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
	msg := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
		ID:            waID, PushName: "Сафаи", Timestamp: at,
	}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, sender.String(), waID, text, rawID, at, []byte("\xff\xd8\xff\xe0"+waID), ".jpg", payer, amt)
	e.waitIdle()
	return waID
}

// l5collapsedVision — фейковый вижн: свёрнутый экран (без даты и получателя),
// экран успеха с получателем (без даты) и полный чек на op.
func l5collapsedVision(op time.Time) func(c aiCall) aiReply {
	return func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			u := c.lastUserText()
			switch {
			case strings.Contains(u, "Перевод выполнен") && strings.Contains(u, "Хадижат"):
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			case strings.Contains(u, "Перевод выполнен"):
				return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + op.Format("2006-01-02 15:04:05") + `"}`}
		}
		return aiReply{Text: ""}
	}
}

// l5collapsedOp — время операции «minutes минут назад», но сегодня (сбор
// считаем за сегодня).
func l5collapsedOp(minutes int) time.Time {
	now := time.Now()
	op := now.Add(-time.Duration(minutes) * time.Minute).Truncate(time.Second)
	if op.Day() != now.Day() {
		op = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 1, 0, now.Location())
	}
	return op
}

// l5collapsedPastHMS — время операции полного чека: сегодня и раньше, чем
// сейчас (чек того же перевода, что экран, присланный сейчас, не может быть
// сделан позже экрана).
func l5collapsedPastHMS() string { return l5collapsedOp(30).Format("15:04:05") }

// Находка 11: работник прислал полный чек и экран «Перевод выполнен» ТОГО ЖЕ
// перевода (оба с подписью клиента или экран назван по просьбе бота) — платёж
// один, а не два.
func TestL5collapsed_11_ScreenOfCountedTransferNotCountedTwice(t *testing.T) {
	op := l5collapsedOp(20)
	full := sberCheck("5000.00", op.Format("15:04:05"))

	// (а) полный чек, потом экран успеха с получателем (без даты и номера).
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("scr1"), user("79001115921")
	l4intakePhoto(e, grp, emp, "Сафаи", full, "Исаев Харон", "")
	n := len(e.sent())
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "СберБанк\nПеревод выполнен\nХадижат Ибрагимовна И.\n5 000 ₽\nСохранить чек", "Исаев Харон", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("(а) полный чек + экран «Перевод выполнен» того же перевода: сбор %v, ожидали Исаев Харон 5000 (бот сказал: %v)", got, e.sentSince(n))
	}
	// Не уверены — спросили «другой?»; ответ «другой» возвращает второй платёж.
	q := e.lastWith("ДРУГОЙ")
	if q.ID == "" {
		t.Fatalf("(а) бот не спросил, тот же ли это перевод: %v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("(а) после ответа «другой» экран %s не засчитан отдельно: сбор %v", screen, got)
	}

	// (б) полный чек, потом свёрнутый экран; бот просит ФИО; работник отвечает ФИО.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("scr2"), user("79001115922")
	l4intakePhoto(e, grp, emp, "Сафаи", full, "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q = e.lastWith("НЕПОЛНЫМ")
	if q.ID == "" {
		t.Fatalf("(б) нет вопроса про свёрнутый экран: %v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("(б) полный чек + его свёрнутый экран, названный по просьбе бота: сбор %v, ожидали Исаев Харон 5000 (бот: %q)", got, e.last().Text)
	}

	// Экран, названный ДРУГИМ клиентом, — другой платёж (разных клиентов не склеиваем).
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("scr3"), user("79001115923")
	l4intakePhoto(e, grp, emp, "Сафаи", full, "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q = e.lastWith("НЕПОЛНЫМ")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("экран назван другим клиентом: сбор %v, ожидали Исаев Харон 5000 + Магомедов Ахмед 5000", got)
	}
}

// Находка 14: полный чек, операция по которому сделана ПОСЛЕ того, как
// свёрнутый экран уже был в группе, — другой перевод: экран не заменяет и
// клиента экрана молча не получает.
func TestL5collapsed_14_LaterFullCheckKeepsEarlierCollapsed(t *testing.T) {
	for _, caption := range []string{"Исаев Харон", ""} {
		e := newE2E(t, e2eOpts{WithAI: true})
		now := time.Now()
		morning := now.Add(-5 * time.Hour)
		if morning.Day() != now.Day() {
			morning = time.Date(now.Year(), now.Month(), now.Day(), 0, 5, 0, 0, time.Local)
		}
		op := now.Add(-5 * time.Minute).Truncate(time.Second)
		if !op.After(morning.Add(3 * time.Minute)) {
			t.Skip("слишком раннее время суток для сценария «утро/вечер»")
		}
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup("late"), user("79001115924")
		l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", morning)
		q := e.lastWith("НЕПОЛНЫМ")
		if q.ID == "" {
			t.Fatalf("нет вопроса про свёрнутый: %v", e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
			t.Fatalf("свёрнутый после ответа не засчитан: %v", got)
		}
		full := l5collapsedPhotoAt(e, grp, emp, sberCheck("5000.00", op.Format("15:04:05")), caption, now)
		r := e.db.TestReceiptByMessage(context.Background(), full)
		got := dayTotals(e, grp)
		if caption != "" && got["Исаев Харон"] != 10000 {
			t.Errorf("подпись «%s»: Исаев Харон заплатил 5000 утром (экран прислан в %s) и 5000 вечером (операция %s): сбор %v, ожидали 10000",
				caption, morning.Format("15:04"), op.Format("15:04"), got)
		}
		if caption == "" {
			if r.Recipient == "Исаев Харон" {
				t.Errorf("чек без подписи (операция %s — после утреннего экрана %s) молча записан на «Исаев Харон»: сбор %v",
					op.Format("15:04"), morning.Format("15:04"), got)
			}
			if got["Исаев Харон"] != 5000 {
				t.Errorf("утренний платёж Исаев Харон пропал: сбор %v", got)
			}
		}
	}

	// Полный чек ТОГО ЖЕ перевода (операция раньше, чем прислали экран), без
	// подписи, — по-прежнему заменяет экран и получает его клиента.
	e := newE2E(t, e2eOpts{WithAI: true})
	now := time.Now()
	op := l5collapsedOp(40)
	screenAt := op.Add(10 * time.Minute)
	if screenAt.After(now) {
		screenAt = now
	}
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("same"), user("79001115925")
	l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", screenAt)
	q := e.lastWith("НЕПОЛНЫМ")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	full := l5collapsedPhotoAt(e, grp, emp, sberCheck("5000.00", op.Format("15:04:05")), "", now)
	if r := e.db.TestReceiptByMessage(context.Background(), full); r.Recipient != "Исаев Харон" {
		t.Errorf("полный чек того же перевода не получил клиента экрана: %+v", r)
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || sum(got) != 5000 {
		t.Errorf("экран + полный чек того же перевода: сбор %v, ожидали Исаев Харон 5000", got)
	}
}

// Находка 16: ничей свёрнутый экран «заменил» подписанный полный чек другого
// клиента; ответ на вопрос бота про экран (ФИО другого клиента) — не теряем:
// это отдельный платёж.
func TestL5collapsed_16_CollapsedAnswerOtherClientKept(t *testing.T) {
	op := l5collapsedOp(10)
	run := func(tag, answer string) (*e2eBot, types.JID, int) {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup(tag), user("79001115927")
		l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
		q := e.lastWith("НЕПОЛНЫМ")
		if q.ID == "" {
			t.Fatalf("нет вопроса про свёрнутый: %v", e.sent())
		}
		l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Format("15:04:05")), "Магомедов Ахмед", "")
		n := len(e.sent())
		e.sendGroup(groupReply(grp, emp, "Сафаи", answer, q.ID, e.botJID(), q.Text))
		return e, grp, n
	}
	e, grp, n := run("csup", "Исаев Харон")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("свёрнутый назван «Исаев Харон», полный чек — «Магомедов Ахмед»: сбор %v, ожидали оба по 5000 (бот ответил: %v)", got, e.sentSince(n))
	}
	if len(e.sentSince(n)) == 0 {
		t.Errorf("ответ про свёрнутый экран молча проигнорирован")
	}
	// Ответ тем же клиентом, что у полного чека, — это и есть тот перевод.
	e, grp, n = run("csup2", "Магомедов Ахмед")
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || sum(got) != 5000 {
		t.Errorf("свёрнутый и полный — один перевод Магомедов Ахмед: сбор %v (бот ответил: %v)", got, e.sentSince(n))
	}
	if len(e.sentSince(n)) == 0 {
		t.Errorf("ответ про свёрнутый экран (уже учтён полным чеком) молча проигнорирован")
	}
}

// Находка 17: ИИ лежал, свёрнутый экран перечитан автоповтором (даты и
// получателя по-прежнему нет) — он остаётся свёрнутым: полный чек того же
// перевода его заменяет, ответ ФИО на старый вопрос не считает перевод дважды.
func TestL5collapsed_17_RetryKeepsCollapsedFlag(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	at := l5collapsedOp(20)
	screenBytes := []byte("\xff\xd8\xff\xe0 collapsed screen")
	full := l5collapsedVision(at)
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			raw, _ := json.Marshal(c.Raw)
			if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(screenBytes)) {
				return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
		}
		return full(c)
	}
	healthy := e.assistant
	e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
	grp, emp := e2eGroup("rcol"), user("79001115929")
	path := filepath.Join(t.TempDir(), "screen.jpg")
	if err := os.WriteFile(path, screenBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	coll := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", path)
	q := e.lastWith("НЕПОЛНЫМ")
	if q.ID == "" {
		t.Fatalf("нет вопроса про свёрнутый: %v", e.sent())
	}
	e.assistant = healthy
	items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("автоповтор: %v %v", items, err)
	}
	e.retryOneReceipt(ctx, items[0])
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", at.Format("15:04:05")), "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(ctx, coll); !r.Duplicate {
		t.Errorf("полный чек не заменил свёрнутый экран (тот висит в «не вошло»): %+v", r)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("один перевод (свёрнутый экран + полный чек): сбор %v, ожидали Исаев Харон 5000", got)
	}
}

// Находка 11 (экран после полного чека): «другой» на «похоже, тот же перевод»
// — экран отдельный платёж, бот спрашивает клиента; два подходящих полных чека
// — не угадываем, обычный вопрос «пришёл НЕПОЛНЫМ».
func TestL5collapsed_11_ScreenAfterCheckAnswers(t *testing.T) {
	op := l5collapsedOp(20)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("scr4"), user("79001115931")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q := e.lastWith("НЕПОЛНЫМ")
	if !strings.Contains(q.Text, "Похоже, это тот же перевод") {
		t.Fatalf("бот не сказал, что экран — того же перевода, что засчитанный чек: %q", q.Text)
	}
	if r := e.db.TestReceiptByMessage(context.Background(), screen); !r.Duplicate {
		t.Errorf("экран засчитанного перевода не помечен повтором: %+v", r)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	a := e.lastWith("Чей он")
	if a.ID == "" {
		t.Fatalf("после «другой» бот не спросил клиента: %v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("экран другого перевода после «другой» и ФИО: сбор %v, ожидали Исаев Харон 5000 + Магомедов Ахмед 5000", got)
	}

	// «другой», а потом ТОТ ЖЕ клиент (заплатил дважды по 5000): работник уже
	// сказал, что это другой платёж, — бот не переспрашивает «тот же?».
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("scr6"), user("79001115934")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q = e.lastWith("НЕПОЛНЫМ")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	a = e.lastWith("Чей он")
	n := len(e.sent())
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", a.ID, e.botJID(), a.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("«другой» и тот же клиент: сбор %v, ожидали Исаев Харон 10000 (бот: %v)", got, e.sentSince(n))
	}

	// Два засчитанных полных чека на ту же сумму — какой из них, не угадываем.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("scr5"), user("79001115932")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Add(time.Minute).Format("15:04:05")), "Магомедов Ахмед", "")
	screen = l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q = e.lastWith("НЕПОЛНЫМ")
	if q.ID == "" || strings.Contains(q.Text, "Похоже") {
		t.Errorf("два подходящих чека — бот должен просто спросить про экран: %q", q.Text)
	}
	if r := e.db.TestReceiptByMessage(context.Background(), screen); r.Duplicate {
		t.Errorf("экран при двух подходящих чеках помечен повтором: %+v", r)
	}
}

// Находка 16 (связь экрана с полным чеком): полный чек, которым учтён экран,
// удалили — экран снова в работе (ждёт клиента), а не пропадает.
func TestL5collapsed_16_DeletedFullCheckRevivesScreen(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(10)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("csup3"), user("79001115933")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheck("5000.00", op.Format("15:04:05")), "Магомедов Ахмед", "")
	if r := e.db.TestReceiptByMessage(ctx, screen); !r.Duplicate {
		t.Fatalf("полный чек не заменил экран: %+v", r)
	}
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(full), Participant: proto.String(emp.String())}}}})
	if r := e.db.TestReceiptByMessage(ctx, screen); r.Duplicate || !r.NeedsReview {
		t.Errorf("полный чек удалён — экран должен снова ждать клиента: %+v", r)
	}
	if got := dayTotals(e, grp); sum(got) != 0 {
		t.Errorf("после удаления полного чека: сбор %v, ожидали 0 (экран ждёт клиента)", got)
	}
}
