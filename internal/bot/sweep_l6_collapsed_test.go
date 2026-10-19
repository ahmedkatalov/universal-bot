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

// Регрессии l6: свёрнутые экраны «Перевод выполнен», признанные «тем же
// переводом», что полный чек, — ответы на них, обратный порядок, молчание,
// перезапуск бота и время операции позже присылки чека.

// Находка 0: экран признан тем же переводом, следом голое ФИО другого клиента
// (без свайпа) — это ответ про экран: отдельный платёж того клиента, а не имя
// для следующего чека.
func TestL6collapsed_0_ScreenGuessedSameThenNameWithoutSwipe(t *testing.T) {
	ctx := context.Background()
	op, op2 := l5collapsedOp(20), l5collapsedOp(3)
	e := newE2E(t, e2eOpts{WithAI: true})
	base := l5collapsedVision(op)
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") && strings.Contains(c.lastUserText(), "7000.00") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":7000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + op2.Format("2006-01-02 15:04:05") + `"}`}
		}
		return base(c)
	}
	grp, emp := e2eGroup("l6a"), user("79001116101")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("экран другого перевода + ФИО следующим сообщением: сбор %v, ожидали Исаев Харон 5000 + Магомедов Ахмед 5000 (экран: %+v, бот ответил: %v)",
			got, e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
	}
	// Следующий чек работника, без подписи (клиента назовут потом).
	next := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op2, "7000.00", op2.Format("15:04:05")), "", "")
	if r := e.db.TestReceiptByMessage(ctx, next); r.Recipient == "Магомедов Ахмед" && r.Confirmed {
		t.Errorf("следующий чек без подписи (7000) молча записан на «Магомедов Ахмед» — ФИО, написанное про экран: %+v", r)
	}

	// То же ФИО, что у полного чека, следующим сообщением — «уже учтён», один
	// платёж, и имя не уходит следующему чеку.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") && strings.Contains(c.lastUserText(), "7000.00") {
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":7000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + op2.Format("2006-01-02 15:04:05") + `"}`}
		}
		return base(c)
	}
	grp, emp = e2eGroup("l6a2"), user("79001116102")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	n = len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || sum(got) != 5000 {
		t.Errorf("экран того же перевода + то же ФИО следующим сообщением: сбор %v, ожидали Исаев Харон 5000 (бот: %v)", got, e.sentSince(n))
	}
	if len(e.sentSince(n)) == 0 {
		t.Errorf("ФИО про экран того же перевода молча проигнорировано")
	}
	next = l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op2, "7000.00", op2.Format("15:04:05")), "", "")
	if r := e.db.TestReceiptByMessage(ctx, next); r.Recipient == "Исаев Харон" && r.Confirmed {
		t.Errorf("следующий чек без подписи (7000) молча записан на «Исаев Харон» — ФИО, написанное про экран: %+v", r)
	}
}

// Находка 1: экран с подписью, потом полный чек того же перевода с той же
// подписью — один платёж (или вопрос «тот же или другой?»), «другой»
// возвращает экран отдельным платежом.
func TestL6collapsed_1_CaptionedScreenThenFullCheck(t *testing.T) {
	op := l5collapsedOp(20)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("l6f"), user("79001116106")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	n := len(e.sent())
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("экран с подписью, потом полный чек того же перевода (операция %s): сбор %v, ожидали Исаев Харон 5000 или вопрос «тот же или другой?» (бот: %v)",
			op.Format("15:04"), got, e.sentSince(n))
	}
	q := e.lastWith("ДРУГОЙ")
	if q.ID == "" {
		t.Fatalf("бот не спросил, тот же ли это перевод: %v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("после «другой» второй платёж не засчитан: сбор %v", got)
	}

	// Полный чек, которым учтён экран, удалили — экран снова в сборе.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("l6f3"), user("79001116109")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(context.Background(), screen); !r.Duplicate {
		t.Fatalf("экран не признан тем же переводом, что полный чек: %+v", r)
	}
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(full), Participant: proto.String(emp.String())}}}})
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("полный чек удалён — экран того же перевода должен вернуться в сбор: %v (экран: %+v)", got, e.db.TestReceiptByMessage(context.Background(), screen))
	}

	// Полный чек сделан ПОСЛЕ того, как экран уже прислали, — другой перевод:
	// оба в сборе, без вопроса.
	now := time.Now()
	morning := now.Add(-5 * time.Hour)
	later := now.Add(-5 * time.Minute).Truncate(time.Second)
	if morning.Day() != now.Day() || !later.After(morning.Add(3*time.Minute)) {
		return
	}
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(later)
	grp, emp = e2eGroup("l6f2"), user("79001116107")
	l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", morning)
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(later, "5000.00", later.Format("15:04:05")), "Исаев Харон", now)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("утренний экран и вечерний чек (операция %s) — два перевода: сбор %v, ожидали 10000", later.Format("15:04"), got)
	}
}

// Находка 2: полный чек без подписи, потом свёрнутый экран того же перевода,
// голое ФИО, а позже ответ тем же ФИО на «чей это чек?» про полный — один
// платёж.
func TestL6collapsed_2_UnnamedFullThenScreenThenName(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(20)
	grp, emp := e2eGroup("l6b"), user("79001116102")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l5collapsedVision(op)
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "", "")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
	e.age(grp.String())
	e.clarifyTick(ctx)
	if q := e.lastWith("Чей это чек"); q.ID != "" {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	}
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("полный чек + свёрнутый экран одного перевода, названные одним клиентом: сбор %v, ожидали Исаев Харон 5000 (полный: %+v, экран: %+v)",
			got, e.db.TestReceiptByMessage(ctx, full), e.db.TestReceiptByMessage(ctx, screen))
	}
}

// Находка 3: в группе на молчании бот не угадывает «тот же перевод» — экран
// остаётся в «НЕ вошло» и о нём спрашивают после снятия молчания.
func TestL6collapsed_3_MutedScreenNotSilentlyMerged(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(20)
	grp, emp := e2eGroup("l6c"), user("79001116103")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l5collapsedVision(op)
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	_ = e.setMuted(ctx, grp.String(), true) // «молчи в этой группе»
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	_ = e.setMuted(ctx, grp.String(), false) // «можешь снова писать»
	from, to := l4Today()
	ex, err := e.db.ReportExclusionsBasis(ctx, from, to, []string{grp.String()}, false)
	if err != nil {
		t.Fatal(err)
	}
	e.age(grp.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	if ex.NeedsReviewCount == 0 && len(e.sentSince(n)) == 0 {
		t.Errorf("экран на 5000 в группе на молчании молча стал повтором (никого не спросили): нет ни в сборе %v, ни в «НЕ вошло» (%+v), вопроса после снятия молчания нет; экран: %+v",
			dayTotals(e, grp), ex, e.db.TestReceiptByMessage(ctx, screen))
	}

	// Подписанный экран без времени в группе на молчании — тоже не прячем как
	// повтор молча: в сборе (как раньше) или спросим.
	e = newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l5collapsedVision(op)
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	_ = e.setMuted(ctx, grp.String(), true)
	screen = l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(ctx, screen); r.Duplicate {
		t.Errorf("подписанный экран в группе на молчании молча стал повтором (спросить некому): %+v", r)
	}
}

// Находка 4: «другой» на «похоже, тот же перевод» после перезапуска бота —
// экран снова отдельный платёж, бот спрашивает клиента.
func TestL6collapsed_4_DifferentAfterRestart(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(20)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp := e2eGroup("l6d"), user("79001116104")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	q := e.lastWith("Похоже, это тот же перевод")
	if q.ID == "" {
		t.Fatalf("бот не сказал, что экран — того же перевода: %v", e.sent())
	}
	e.clarify = newClarifyState() // перезапуск бота: память вопросов пуста, база та же
	n := len(e.sent())
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if a := e.lastWith("Чей он"); a.ID != "" {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", a.ID, e.botJID(), a.Text))
	}
	if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 5000 || got["Исаев Харон"] != 5000 {
		t.Errorf("«другой» после перезапуска: сбор %v, ожидали Исаев Харон 5000 + отдельный платёж (экран: %+v, бот ответил: %v)",
			got, e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
	}

	// Обратный порядок (экран с подписью, потом полный чек): «другой» на «похоже,
	// полный чек того же перевода» после перезапуска — оба платежа в сборе.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5collapsedVision(op)
	grp, emp = e2eGroup("l6d2"), user("79001116108")
	screen = l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	q = e.lastWith("ДРУГОЙ")
	if q.ID == "" {
		t.Fatalf("бот не спросил, тот же ли это перевод: %v", e.sent())
	}
	e.clarify = newClarifyState()
	n = len(e.sent())
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("«другой» после перезапуска (экран, потом полный чек): сбор %v, ожидали Исаев Харон 10000 (экран: %+v, бот: %v)",
			got, e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
	}
}

// Находка 5: время операции на полном чеке позже, чем его прислали (ошибка
// чтения, другой часовой пояс), — по нему не судим ни в каком порядке: чек и
// его свёрнутый экран, названный тем же клиентом, — один платёж.
func TestL6collapsed_5_FutureOpTimeScreenCountedOnce(t *testing.T) {
	op := time.Now().Add(100 * time.Minute).Truncate(time.Second) // перевод 20 мин назад, напечатано +2 ч
	from, to := l4Today()
	from, to = from.AddDate(0, 0, -1), to.AddDate(0, 0, 1)
	run := func(tag string, screenFirst bool) float64 {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5collapsedVision(op)
		grp, emp := e2eGroup(tag), user("79001116105")
		full := func() {
			l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
		}
		if !screenFirst {
			full()
		}
		l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
		if q := e.lastWith("НЕПОЛНЫМ"); q.ID != "" && !strings.Contains(q.Text, "Похоже") {
			e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
		}
		if screenFirst {
			full()
		}
		return l4Total(e, []string{grp.String()}, from, to)
	}
	if got := run("l6e1", true); got != 5000 {
		t.Fatalf("экран, потом чек: %v (ожидали 5000)", got)
	}
	if got := run("l6e2", false); got != 5000 {
		t.Errorf("чек с временем операции %s (позже, чем его прислали), потом его свёрнутый экран, названный тем же клиентом: сбор %v, ожидали 5000 (в обратном порядке — 5000)",
			op.Format("15:04"), got)
	}
}
