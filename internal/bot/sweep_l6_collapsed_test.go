package bot

import (
	"context"
	"regexp"
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

// Находка 0 (и R0): экран признан тем же переводом, следом голое ФИО ДРУГОГО
// клиента (без свайпа). Это или клиент экрана (тогда экран — другой платёж),
// или имя для следующего чека — не угадываем: экран молча не «раздваиваем»,
// имя ждёт следующий чек, а бот спрашивает, не про экран ли оно; «да» —
// экран отдельным платежом этого клиента (и имя следующему чеку не уходит).
// То же ФИО, что у полного чека, — про экран: «уже учтён».
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
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || sum(got) != 5000 {
		t.Errorf("голое ФИО другого клиента после экрана того же перевода молча решило судьбу экрана: сбор %v (экран: %+v)", got, e.db.TestReceiptByMessage(ctx, screen))
	}
	q := e.lastWith("это клиент экрана")
	if q.ID == "" {
		t.Fatalf("бот не спросил, про экран ли ФИО: %v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "да", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("«да» — экран платёж Магомедова: сбор %v, ожидали Исаев Харон 5000 + Магомедов Ахмед 5000 (экран: %+v, бот: %v)",
			got, e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
	}
	// Следующий чек работника, без подписи: имя ушло на экран — ему не достаётся.
	next := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op2, "7000.00", op2.Format("15:04:05")), "", "")
	if r := e.db.TestReceiptByMessage(ctx, next); r.Recipient == "Магомедов Ахмед" && r.Confirmed {
		t.Errorf("следующий чек без подписи (7000) молча записан на «Магомедов Ахмед» — ФИО, отданное экрану: %+v", r)
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
	// повтор молча: ждёт в «НЕ вошло», о нём спросят после снятия молчания
	// (подробнее — R4).
	e = newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l5collapsedVision(op)
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	_ = e.setMuted(ctx, grp.String(), true)
	screen = l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(ctx, screen); r.Duplicate || !r.NeedsReview {
		t.Errorf("подписанный экран в группе на молчании: ожидали «НЕ вошло» (ждёт вопроса), а не повтор и не сбор: %+v", r)
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

// Регрессии второго круга (R<n> — находка n ревью): экран, признанный тем же
// переводом, что полный чек, не перехватывает голое ФИО, предназначенное
// более свежему вопросу или следующему чеку, и платежи «ФИО сумма» (R0);
// короткое имя того же клиента — «уже учтён» (R1); в группе на молчании
// догадка ждёт в «НЕ вошло» и спрашивается потом (R2); удаление чека с
// временем «в будущем» не оживляет чужой старый экран (R3); два перевода по
// экрану и полному чеку — каждый один раз (R4); полный чек, названный позже,
// сверяется с экраном (R5); смена клиента полного чека возвращает экран (R6).

// l6rAI — вижн свёрнутого экрана/полного чека на op (extra — свои ответы по
// подстроке распознанного текста) и разбор платежей по известным клиентам.
func l6rAI(op time.Time, extra map[string]string) func(c aiCall) aiReply {
	base := l5collapsedVision(op)
	pay := aiNamesAndPayments("Исаев Харон", "Алиев Ислам", "Магомедов Ахмед")
	return func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора изображений") {
			u := c.lastUserText()
			for k, v := range extra {
				if strings.Contains(u, k) {
					return aiReply{Text: v}
				}
			}
			return base(c)
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			return pay(c)
		}
		return base(c)
	}
}

// l6rExtra — вижн свёрнутого экрана на 3000 и полного чека на 7000 (op2).
func l6rExtra(op2 time.Time) map[string]string {
	return map[string]string{
		"3 000":   `{"kind":"receipt","bank":"","recipient":"","amount":3000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`,
		"7000.00": `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":7000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + op2.Format("2006-01-02 15:04:05") + `"}`,
	}
}

// l6rHeldScreen — полный чек «Исаев Харон» на 5000 и следом свёрнутый экран
// того же перевода («Похоже, это тот же перевод»). Возвращает экран.
func l6rHeldScreen(t *testing.T, e *e2eBot, grp, emp types.JID, op time.Time) string {
	t.Helper()
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	if q := e.lastWith("Похоже, это тот же перевод"); q.ID == "" {
		t.Fatalf("нет «тот же перевод»: %v", e.sent())
	}
	return screen
}

// R0 (P1): после экрана того же перевода пришёл свёрнутый экран ДРУГОГО
// перевода (3000, «ответьте ФИО»), голое ФИО — ответ про него, а не про экран
// того же перевода.
func TestL6collapsed_R0_NameForLaterScreenNotTakenByHeldScreen(t *testing.T) {
	ctx := context.Background()
	op, op2 := l5collapsedOp(20), l5collapsedOp(5)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rAI(op, l6rExtra(op2))
	grp, emp := e2eGroup("l6r0p1"), user("79001126031")
	screen := l6rHeldScreen(t, e, grp, emp, op)
	screen2 := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n3 000 ₽\nСообщение получателю", "", "")
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 3000 || sum(got) != 8000 {
		t.Errorf("сбор %v, ожидали Исаев 5000 + Магомедов 3000 (экран1 %+v, экран2 %+v, бот %v)", got,
			e.db.TestReceiptByMessage(ctx, screen), e.db.TestReceiptByMessage(ctx, screen2), e.sentSince(n))
	}
}

// R0 (P2): ответ на «🤔 Чей это чек?» клариф-цикла про следующий чек (7000)
// — про него, а не про экран того же перевода.
func TestL6collapsed_R0_AnswerToWhoseCheckNotTakenByHeldScreen(t *testing.T) {
	ctx := context.Background()
	op, op2 := l5collapsedOp(20), l5collapsedOp(5)
	grp, emp := e2eGroup("l6r0p2"), user("79001126032")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l6rAI(op, l6rExtra(op2))
	screen := l6rHeldScreen(t, e, grp, emp, op)
	next := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op2, "7000.00", op2.Format("15:04:05")), "", "")
	e.age(grp.String())
	e.clarifyTick(ctx)
	if q := e.lastWith("Чей это чек"); q.ID == "" {
		t.Fatalf("нет «Чей это чек?»: %v", e.sent())
	}
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 7000 || sum(got) != 12000 {
		t.Errorf("сбор %v, ожидали Исаев 5000 + Магомедов 7000 (чек 7000: %+v, экран %+v, бот %v)", got,
			e.db.TestReceiptByMessage(ctx, next), e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
	}
}

// R0: ответ «Алиев Ислам» на «чек на 7000 — ЧЕЙ он?» (вопрос при приёме) —
// про чек 7000, экран того же перевода остаётся повтором.
func TestL6collapsed_R0_AnswerToNewerCheckAskNotTakenByHeldScreen(t *testing.T) {
	ctx := context.Background()
	op, op2 := l5collapsedOp(20), l5collapsedOp(2)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rAI(op, map[string]string{"7000.00": `{"kind":"receipt","bank":"Сбербанк","recipient":"","amount":7000,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + op2.Format("2006-01-02 15:04:05") + `"}`})
	grp, emp := e2eGroup("l6r0"), user("79001126009")
	screen := l6rHeldScreen(t, e, grp, emp, op)
	ck := l4intakePhoto(e, grp, emp, "Сафаи", "СберБанк\nЧек по операции\nДата операции "+op2.Format("02.01.2006 15:04:05")+"\nСумма перевода\n7000.00 ₽", "", "")
	if q := e.lastWith("ЧЕЙ он"); q.ID == "" {
		t.Fatalf("бот не спросил про чек 7000: %v", e.sent())
	}
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Алиев Ислам"))
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 5000 || got["Алиев Ислам"] != 7000 || sum(got) != 12000 {
		t.Errorf("ответ «Алиев Ислам» на «чек на 7000 — ЧЕЙ он?»: сбор %v, ожидали Исаев 5000 + Алиев 7000; чек 7000: %+v; бот: %v", got, e.db.TestReceiptByMessage(ctx, ck), e.sentSince(n))
	}
	if r := e.db.TestReceiptByMessage(ctx, screen); !r.Duplicate {
		t.Errorf("экран того же перевода перестал быть повтором: %+v", r)
	}
}

// R0 (P5): «ФИО, потом чек» сразу после экрана того же перевода — имя
// достаётся следующему чеку (бот лишь спрашивает, не про экран ли оно; «нет»
// ничего не меняет).
func TestL6collapsed_R0_NameThenCheckAfterHeldScreen(t *testing.T) {
	ctx := context.Background()
	op, op2 := l5collapsedOp(20), l5collapsedOp(5)
	for i, nm := range []string{"Магомедов Ахмед", "Алиев Ислам"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l6rAI(op, l6rExtra(op2))
		grp, emp := e2eGroup("l6r0p5"), user("7900112604"+string(rune('0'+i)))
		screen := l6rHeldScreen(t, e, grp, emp, op)
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", nm))
		next := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op2, "7000.00", op2.Format("15:04:05")), "", "")
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 5000 || got[nm] != 7000 || sum(got) != 12000 {
			t.Errorf("«%s», потом его чек 7000: сбор %v, ожидали Исаев 5000 + %s 7000; чек 7000: %+v; экран %+v; бот: %v",
				nm, got, nm, e.db.TestReceiptByMessage(ctx, next), e.db.TestReceiptByMessage(ctx, screen), e.sentSince(n))
		}
		if q := e.lastWith("это клиент экрана"); q.ID != "" {
			e.sendGroup(groupReply(grp, emp, "Сафаи", "нет", q.ID, e.botJID(), q.Text))
			if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 || got[nm] != 7000 || sum(got) != 12000 {
				t.Errorf("«нет» на «это клиент экрана?»: сбор %v", got)
			}
		}
	}
}

// R0 (P3): «ФИО сумма» на ту же сумму после экрана того же перевода — это
// платёж (текст и чек этого клиента — один платёж), а не ответ про экран.
func TestL6collapsed_R0_TextPaymentSameAmountAfterHeldScreen(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	opI := now.Add(-40 * time.Minute).Truncate(time.Second)
	opA := now.Add(-2 * time.Minute).Truncate(time.Second)
	if opI.Day() != now.Day() {
		t.Skip("около полуночи")
	}
	for i, nm := range []string{"Алиев Ислам", "Магомедов Ахмед"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		grp, emp := e2eGroup("l6r1"), user("7900112600"+string(rune('6'+i)))
		e.ai = l6rAI(opI, nil)
		l5collapsedPhotoAt(e, grp, emp, sberCheckOn(opI, "5000.00", opI.Format("15:04:05")), "Исаев Харон", now.Add(-12*time.Minute))
		screen := l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", now.Add(-11*time.Minute))
		if q := e.lastWith("Похоже, это тот же перевод"); q.ID == "" {
			t.Fatalf("нет «тот же перевод»: %v", e.sent())
		}
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", nm+" 5000"))
		mid := dayTotals(e, grp)
		if r := e.db.TestReceiptByMessage(ctx, screen); !r.Duplicate {
			t.Errorf("«%s 5000» съеден как ответ про экран того же перевода: экран %+v, сбор %v", nm, r, mid)
		}
		e.ai = l6rAI(opA, nil)
		ck := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(opA, "5000.00", opA.Format("15:04:05")), nm, "")
		got := dayTotals(e, grp)
		if got["Исаев Харон"] != 5000 || got[nm] != 5000 || sum(got) != 10000 {
			t.Errorf("«%s 5000» + его чек с подписью: сбор %v (после текста %v), ожидали Исаев 5000 + %s 5000; чек: %+v; бот: %v",
				nm, got, mid, nm, e.db.TestReceiptByMessage(ctx, ck), e.sentSince(n))
		}
	}
}

// R1: короткое имя того же клиента («Харон», «Исаев») после экрана того же
// перевода «Исаев Харон» — без свайпа и свайпом на вопрос бота — один платёж
// (и клиента «Харон» из обрывка известного ФИО не заводим).
func TestL6collapsed_R1_SameClientShortNameAfterSameScreen(t *testing.T) {
	for i, nm := range []string{"Харон", "Исаев"} {
		for _, swipe := range []bool{false, true} {
			op := l5collapsedOp(20)
			e := newE2E(t, e2eOpts{WithAI: true})
			e.ai = l6rAI(op, nil)
			grp, emp := e2eGroup("l6r2"), user("7900112601"+string(rune('0'+i)))
			screen := l6rHeldScreen(t, e, grp, emp, op)
			q := e.lastWith("Похоже, это тот же перевод")
			n := len(e.sent())
			if swipe {
				e.sendGroup(groupReply(grp, emp, "Сафаи", nm, q.ID, e.botJID(), q.Text))
			} else {
				e.sendGroup(groupText(grp, emp, "Сафаи", nm))
			}
			if got := dayTotals(e, grp); sum(got) != 5000 || got["Исаев Харон"] != 5000 {
				t.Errorf("«%s» (свайп %v) после экрана того же перевода Исаева Харона: сбор %v, ожидали 5000; экран %+v; бот: %v",
					nm, swipe, got, e.db.TestReceiptByMessage(context.Background(), screen), e.sentSince(n))
			}
		}
	}
}

// R2: в группе на молчании догадка «тот же перевод» не держится молча и не
// превращается во второй платёж: более поздний чек без даты ждёт в «НЕ
// вошло», после снятия молчания бот спрашивает; «другой» — два платежа.
func TestL6collapsed_R2_MutedSameTransferParkedThenAsked(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(20)
	from, to := l4Today()
	run := func(tag, phone string, muteFirst bool, screenStep func(e *e2eBot, grp, emp types.JID) string) {
		t.Helper()
		grp, emp := e2eGroup(tag), user(phone)
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
		e.ai = l6rAI(op, nil)
		if muteFirst {
			_ = e.setMuted(ctx, grp.String(), true)
		}
		l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
		_ = e.setMuted(ctx, grp.String(), true)
		screen := screenStep(e, grp, emp)
		if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
			t.Errorf("%s: молчание — полный + экран того же перевода: сбор %v, ожидали 5000 (экран %+v)", tag, got, e.db.TestReceiptByMessage(ctx, screen))
		}
		ex, err := e.db.ReportExclusionsBasis(ctx, from, to, []string{grp.String()}, false)
		if err != nil {
			t.Fatal(err)
		}
		if ex.NeedsReviewCount == 0 {
			t.Errorf("%s: экран на молчании не виден в «НЕ вошло»: %+v; экран %+v", tag, ex, e.db.TestReceiptByMessage(ctx, screen))
		}
		_ = e.setMuted(ctx, grp.String(), false)
		e.age(grp.String())
		n := len(e.sent())
		e.clarifyTick(ctx)
		q := e.lastWith("ДРУГОЙ")
		if q.ID == "" || len(e.sentSince(n)) == 0 {
			t.Fatalf("%s: после снятия молчания про экран не спросили: %v (экран %+v)", tag, e.sentSince(n), e.db.TestReceiptByMessage(ctx, screen))
		}
		if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
			t.Errorf("%s: после вопроса сбор %v, ожидали 5000", tag, got)
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
			t.Errorf("%s: «другой» — второй платёж не засчитан: %v (экран %+v)", tag, got, e.db.TestReceiptByMessage(ctx, screen))
		}
	}
	captioned := func(e *e2eBot, grp, emp types.JID) string {
		return l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	}
	run("l6r4a", "79001126003", false, captioned)
	// Группа молчит с самого начала (как в находке): полный чек и подписанный
	// экран — тоже ждёт в «НЕ вошло» и спрашивается после снятия молчания.
	run("l6r4m", "79001126013", true, captioned)
	run("l6r4b", "79001126004", false, func(e *e2eBot, grp, emp types.JID) string {
		screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
		e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
		return screen
	})

	// Обратный порядок на молчании: подписанный экран, потом полный чек того
	// же перевода — экран ждёт в «НЕ вошло», в сборе один платёж.
	grp, emp := e2eGroup("l6r4r"), user("79001126015")
	e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l6rAI(op, nil)
	_ = e.setMuted(ctx, grp.String(), true)
	screen := captioned(e, grp, emp)
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("молчание, экран потом полный чек: сбор %v, ожидали 5000 (экран %+v)", got, e.db.TestReceiptByMessage(ctx, screen))
	}
	_ = e.setMuted(ctx, grp.String(), false)
	e.age(grp.String())
	n := len(e.sent())
	e.clarifyTick(ctx)
	if q := e.lastWith("ДРУГОЙ"); q.ID == "" || len(e.sentSince(n)) == 0 {
		t.Errorf("молчание, экран потом полный чек: после снятия молчания не спросили: %v", e.sentSince(n))
	}

	// Перезапуск бота, пока группа молчит: отложенный экран не уходит в сбор
	// пересопоставлением клиентов при старте.
	grp, emp = e2eGroup("l6r4s"), user("79001126016")
	e = newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l6rAI(op, nil)
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	_ = e.setMuted(ctx, grp.String(), true)
	screen = captioned(e, grp, emp)
	rid, ok, err := e.db.ReceiptIDByMessage(ctx, screen)
	if err != nil || !ok {
		t.Fatalf("нет чека экрана: %v %v", ok, err)
	}
	if r := e.db.TestReceiptByMessage(ctx, screen); r.Duplicate || !r.NeedsReview {
		t.Fatalf("экран на молчании не ждёт в «НЕ вошло»: %+v", r)
	}
	un, err := e.db.UnresolvedReceipts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range un {
		if u.ID == rid {
			t.Errorf("отложенный экран попал в пересопоставление клиентов при старте (ушёл бы в сбор молча): %+v", u)
		}
	}

	// Полный чек, с которым совпал отложенный экран, удалили на молчании —
	// экран сразу в сборе.
	grp, emp = e2eGroup("l6r4c"), user("79001126014")
	e = newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата"}})
	e.ai = l6rAI(op, nil)
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	_ = e.setMuted(ctx, grp.String(), true)
	screen = captioned(e, grp, emp)
	e.revoke(grp, emp, full)
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("полный чек удалён на молчании — отложенный экран должен войти в сбор: %v (экран %+v)", got, e.db.TestReceiptByMessage(ctx, screen))
	}
}

// revoke — сотрудник удалил своё сообщение waID в группе.
func (e *e2eBot) revoke(grp, emp types.JID, waID string) {
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(waID), Participant: proto.String(emp.String())}}}})
}

var (
	l6rDateRe = regexp.MustCompile(`Дата операции (\d\d\.\d\d\.\d{4} \d\d:\d\d:\d\d)`)
	l6rAmtRe  = regexp.MustCompile(`Сумма перевода\n(\d+)\.00`)
)

// l6rVisionParse — вижн полного чека по его тексту (дата и сумма) и экрана
// «Перевод выполнен» на 5000.
func l6rVisionParse() func(c aiCall) aiReply {
	return func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			u := c.lastUserText()
			if strings.Contains(u, "Перевод выполнен") {
				return aiReply{Text: `{"kind":"receipt","bank":"","recipient":"","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
			d, a := l6rDateRe.FindStringSubmatch(u), l6rAmtRe.FindStringSubmatch(u)
			if d != nil && a != nil {
				t, _ := time.ParseInLocation("02.01.2006 15:04:05", d[1], time.Local)
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":` + a[1] + `,"commission":0,"doc_number":"","status":"Выполнено","datetime":"` + t.Format("2006-01-02 15:04:05") + `"}`}
			}
		}
		return aiReply{Text: ""}
	}
}

// R3: удалили сегодняшний полный чек, у которого время операции прочиталось
// «в будущем» (позже присылки), — старый экран того же клиента и суммы,
// учтённый ДРУГИМ (живым) полным чеком 10 дней назад, не оживает.
func TestL6collapsed_R3_DeleteImplausibleCheckKeepsOldScreenHeld(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	old := now.AddDate(0, 0, -10)
	oldOp := old.Add(-20 * time.Minute).Truncate(time.Second)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rVisionParse()
	grp, emp := e2eGroup("l6r3d"), user("79001126051")
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(oldOp, "5000.00", oldOp.Format("15:04:05")), "Исаев Харон", old)
	sOld := l5collapsedPhotoAt(e, grp, emp, "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", old.Add(time.Minute))
	if r := e.db.TestReceiptByMessage(ctx, sOld); !r.Duplicate {
		t.Fatalf("старый экран не стал повтором: %+v", r)
	}
	from := time.Date(old.Year(), old.Month(), old.Day(), 0, 0, 0, 0, old.Location()).AddDate(0, 0, -1)
	to := from.AddDate(0, 0, 3)
	before := l4Total(e, []string{grp.String()}, from, to)
	fut := now.Add(3 * time.Hour).Truncate(time.Second)
	fNew := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(fut, "5000.00", fut.Format("15:04:05")), "Исаев Харон", "")
	e.revoke(grp, emp, fNew)
	if r := e.db.TestReceiptByMessage(ctx, sOld); !r.Duplicate {
		t.Errorf("удалили сегодняшний чек (время «в будущем») — ожил старый экран: %+v; сбор того дня был %v, стал %v", r, before, l4Total(e, []string{grp.String()}, from, to))
	}

	// Свой экран такого чека (время «в будущем», экран следом) при удалении
	// чека оживает: платёж остаётся один.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rVisionParse()
	grp, emp = e2eGroup("l6r3e"), user("79001126052")
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(fut, "5000.00", fut.Format("15:04:05")), "Исаев Харон", "")
	scr := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(ctx, scr); !r.Duplicate {
		t.Fatalf("экран того же перевода не стал повтором: %+v", r)
	}
	e.revoke(grp, emp, full)
	if r := e.db.TestReceiptByMessage(ctx, scr); r.Duplicate {
		t.Errorf("удалили полный чек (время «в будущем») — его экран не вернулся в сбор: %+v", r)
	}
}

// R4: два перевода по 5000 (операции T−60 и T−30), по каждому — экран с
// подписью и полный чек. Каждый перевод — один раз: экран второго перевода не
// сопоставляется с полным чеком первого, у которого экран уже есть, а «другой»
// про экран (он не тот же перевод, что ПЕРВЫЙ чек) не мешает узнать в нём
// экран второго полного чека.
func TestL6collapsed_R4_TwoTransfersScreenPlusFullEach(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	opA := now.Add(-60 * time.Minute).Truncate(time.Second)
	opB := now.Add(-30 * time.Minute).Truncate(time.Second)
	if opA.Day() != now.Day() {
		t.Skip("около полуночи")
	}
	at := func(m int) time.Time { return now.Add(time.Duration(-m) * time.Minute) }
	ai := l6rVisionParse()
	screen := "Перевод выполнен\n5 000 ₽\nСообщение получателю"

	// Как в находке: экран1, полный A, экран2, («другой», если спросят), полный B.
	grp, emp := e2eGroup("l6r6a"), user("79001126061")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = ai
	l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", at(50))
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(opA, "5000.00", opA.Format("15:04:05")), "Исаев Харон", at(49))
	n := len(e.sent())
	s2 := l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", at(20))
	if q := e.lastWith("ДРУГОЙ"); q.ID != "" && len(e.sentSince(n)) > 0 {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	}
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(opB, "5000.00", opB.Format("15:04:05")), "Исаев Харон", at(19))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("два перевода по 5000, по каждому экран+полный чек: сбор %v, ожидали 10000 (экран2: %+v)", got, e.db.TestReceiptByMessage(ctx, s2))
	}

	// Полный A, экран2 (бот спросил «тот же?» про A), честное «другой»,
	// полный B — экран2 узнаётся экраном B: 10000, и бот спрашивает.
	grp, emp = e2eGroup("l6r6b"), user("79001126062")
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = ai
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(opA, "5000.00", opA.Format("15:04:05")), "Исаев Харон", at(49))
	n = len(e.sent())
	s2 = l5collapsedPhotoAt(e, grp, emp, screen, "Исаев Харон", at(20))
	q := e.lastWith("ДРУГОЙ")
	if q.ID == "" || len(e.sentSince(n)) == 0 {
		t.Fatalf("экран после полного чека той же суммы: бот не спросил «тот же или другой?»: %v", e.sentSince(n))
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Fatalf("«другой» — экран2 отдельный платёж: сбор %v", got)
	}
	n = len(e.sent())
	l5collapsedPhotoAt(e, grp, emp, sberCheckOn(opB, "5000.00", opB.Format("15:04:05")), "Исаев Харон", at(19))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
		t.Errorf("«другой» про экран2 (не перевод A) + полный чек B: сбор %v, ожидали 10000 (экран2: %+v, бот %v)", got, e.db.TestReceiptByMessage(ctx, s2), e.sentSince(n))
	}
	q = e.lastWith("ДРУГОЙ")
	if q.ID == "" || len(e.sentSince(n)) == 0 {
		t.Fatalf("экран2 узнан экраном полного чека B без вопроса: %v", e.sentSince(n))
	}
	// И про B тоже «другой» — три платежа, как сказали.
	e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 15000 {
		t.Errorf("«другой» и про полный чек B: сбор %v, ожидали 15000", got)
	}
}

// R5: полный чек без подписи, названный ПОСЛЕ экрана того же перевода (ФИО
// следом, свайпом, второе ФИО по очереди) — один платёж и вопрос «тот же или
// другой?»; «другой» — два платежа.
func TestL6collapsed_R5_FullCheckNamedLaterMatchesScreen(t *testing.T) {
	op := l5collapsedOp(20)
	run := func(tag, phone string, name func(e *e2eBot, grp, emp types.JID, full string)) {
		t.Helper()
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l6rAI(op, nil)
		grp, emp := e2eGroup(tag), user(phone)
		l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
		n := len(e.sent())
		full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "", "")
		name(e, grp, emp, full)
		if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
			t.Errorf("%s: экран с подписью + полный чек того же перевода, названный потом: сбор %v, ожидали 5000; полный %+v; бот %v",
				tag, got, e.db.TestReceiptByMessage(context.Background(), full), e.sentSince(n))
		}
		q := e.lastWith("ДРУГОЙ")
		if q.ID == "" {
			t.Fatalf("%s: бот не спросил «тот же или другой?»: %v", tag, e.sentSince(n))
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 10000 {
			t.Errorf("%s: после «другой» второй платёж не засчитан: %v", tag, got)
		}
	}
	run("l6r3a", "79001126001", func(e *e2eBot, grp, emp types.JID, full string) {
		e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
	})
	run("l6r3b", "79001126002", func(e *e2eBot, grp, emp types.JID, full string) {
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", full, emp, ""))
	})

	// Полный чек без подписи, свёрнутый экран, ФИО (уходит экрану), ещё раз ФИО
	// (полному чеку по очереди) — один платёж.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rAI(op, nil)
	grp, emp := e2eGroup("l6r3c"), user("79001126021")
	l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "", "")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "", "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон"))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("полный + экран, ФИО дважды: сбор %v, ожидали 5000 или вопрос", got)
	}
}

// R6: полный чек, которым учтён экран того же клиента, переназначили свайпом
// на другого клиента — экран снова отдельный платёж (разных клиентов не
// склеиваем), и бот говорит об этом.
func TestL6collapsed_R6_HeldScreenReleasedWhenFullCheckReattributed(t *testing.T) {
	ctx := context.Background()
	op := l5collapsedOp(20)
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rAI(op, nil)
	grp, emp := e2eGroup("l6r5"), user("79001126008")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	full := l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	if r := e.db.TestReceiptByMessage(ctx, screen); !r.Duplicate {
		t.Fatalf("экран не признан тем же переводом, что полный чек: %+v", r)
	}
	n := len(e.sent())
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", full, emp, ""))
	got := dayTotals(e, grp)
	if got["Исаев Харон"] != 5000 || got["Магомедов Ахмед"] != 5000 {
		t.Errorf("полный чек переназначен на Магомедова: сбор %v, ожидали Исаев 5000 (экран) + Магомедов 5000; экран %+v; полный %+v; бот %v",
			got, e.db.TestReceiptByMessage(ctx, screen), e.db.TestReceiptByMessage(ctx, full), e.sentSince(n))
	}
	if q := e.lastWith("снова считаю"); q.ID == "" {
		t.Errorf("бот не сказал, что экран снова отдельный платёж: %v", e.sentSince(n))
	}

	// Тот же клиент, записанный иначе («Харон Исаев»), — экран остаётся тем же
	// переводом.
	e = newE2E(t, e2eOpts{WithAI: true})
	e.ai = l6rAI(op, nil)
	grp, emp = e2eGroup("l6r5b"), user("79001126018")
	l4intakePhoto(e, grp, emp, "Сафаи", "Перевод выполнен\n5 000 ₽\nСообщение получателю", "Исаев Харон", "")
	full = l4intakePhoto(e, grp, emp, "Сафаи", sberCheckOn(op, "5000.00", op.Format("15:04:05")), "Исаев Харон", "")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Харон Исаев", full, emp, ""))
	if got := dayTotals(e, grp); sum(got) != 5000 {
		t.Errorf("полный чек переписан на того же клиента иначе: сбор %v, ожидали 5000", got)
	}
}
