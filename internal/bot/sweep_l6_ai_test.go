package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/parser"
)

// Регрессии L6 (ИИ-разбор налички без имени и дата чека без времени): вопрос
// «❓ чей?» по наличке не забирает имя следующего чека, наличка без имени с той
// же суммой, что у соседнего платежа, и в других словах не теряется, итог дня
// не становится клиентом «за сегодня», ответ «тот же» оставляет исправленное
// ФИО, а время «12:00» у чека с одной датой — угаданное.

// l6aiAsks — сколько вопросов «❓» бот задал после n-го сообщения.
func l6aiAsks(e *e2eBot, n int) int {
	asks := 0
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			asks++
		}
	}
	return asks
}

// 11. После «❓ Чья это наличка 27000?» сотрудник, как обычно, присылает чек без
// подписи и следом ФИО — имя для чека (он ждёт клиента), а не ответ про наличку.
func TestL6ai_11_CashQuestionDoesNotStealCheckThenName(t *testing.T) {
	phr := []string{"Чья это наличка 27000?", "От кого наличные 27 000 ₽?", "Кто клиент по наличке 27000?"}
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5aiClarifyWorded(phr, "")
	grp, emp := e2eGroup("l6steal"), user("79285551204")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", "Нал 27000"))
	if q := e.lastWith("❓"); q.ID == "" {
		t.Fatalf("нет вопроса «❓» по наличке: %+v", e.sent())
	}
	at := time.Now().Add(-3 * time.Minute)
	if at.Day() != time.Now().Day() {
		at = time.Now()
	}
	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(at, "7 000", at.Format("15:04:05")), "")
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед"))
	got := dayTotals(e, grp)
	if got["Магомедов Ахмед"] != 7000 || got["Наличка"] != 27000 {
		t.Errorf("«Нал 27000» (❓), затем чек 7000 и «Магомедов Ахмед»: сбор %v — ожидали чек 7000 на Магомедова Ахмеда и наличку 27000 ждущей ответа (бот: %q)", got, e.last().Text)
	}
	// Вопрос по наличке по-прежнему отвечается свайпом.
	q := e.lastWith("❓")
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 27000 || got["Магомедов Ахмед"] != 7000 || sum(got) != 34000 {
		t.Errorf("свайп «Исаев Харон» на «❓» по наличке: сбор %v, ожидали Исаев Харон 27000 + Магомедов Ахмед 7000", got)
	}

	// Чека после вопроса нет — ФИО без свайпа от того же сотрудника — ответ про наличку.
	e2 := newE2E(t, e2eOpts{WithAI: true})
	e2.ai = l5aiClarifyWorded(phr, "")
	grp2 := e2eGroup("l6steal2")
	e2.groupsCache[grp2] = "Оплата"
	e2.sendGroup(groupText(grp2, emp, "Сафаи", "Нал 27000"))
	e2.sendGroup(groupText(grp2, emp, "Сафаи", "Магомедов Ахмед"))
	if got := dayTotals(e2, grp2); got["Магомедов Ахмед"] != 27000 || sum(got) != 27000 {
		t.Errorf("«Нал 27000» (❓), затем «Магомедов Ахмед» без чека: сбор %v, ожидали Магомедов Ахмед 27000", got)
	}
}

// 12. «Исаев Харон 15000 / Нал 15000»: наличка без имени на ту же сумму, что у
// платежа с ФИО, — отдельные деньги: держим заглушкой и спрашиваем «❓».
func TestL6ai_12_UnnamedCashSameAmountAsNamedAsked(t *testing.T) {
	phr := []string{"Чья это наличка 15000?", "От кого наличные 15 000 ₽?", "Кто клиент по наличке 15000?"}
	for _, text := range []string{"Исаев Харон 15000\nНал 15000", "Нал 15000\nИсаев Харон 15000"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5aiClarifyWorded(phr, "Исаев Харон")
		grp, emp := e2eGroup("l6eq"), user("79285551201")
		e.groupsCache[grp] = "Оплата"
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		got := dayTotals(e, grp)
		if asks := l6aiAsks(e, n); asks != 1 || got["Наличка"] != 15000 || sum(got) != 30000 {
			t.Errorf("%q: вопросов «❓» %d, сбор %v (%v); ожидали Исаев Харон 15000 + Наличка 15000 и один вопрос «чей?»", text, asks, sum(got), got)
		}
		q := e.lastWith("❓")
		if q.ID == "" {
			continue
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 15000 || got["Исаев Харон"] != 15000 || sum(got) != 30000 {
			t.Errorf("%q, ответ «Магомедов Ахмед» на «❓»: сбор %v, ожидали Исаев Харон 15000 + Магомедов Ахмед 15000", text, got)
		}
	}
}

// 13. Наличка без имени другими словами в списке («Забрал наличку 27000»,
// «27000 наличными в офис», «Принесли 27000 наличкой») — тоже спросить и
// держать в сборе, а не терять.
func TestL6ai_13_UnnamedCashOtherWordingInListAsked(t *testing.T) {
	phr := []string{"Чья это наличка 27000?", "От кого наличные 27 000 ₽?", "Кто клиент по наличке 27000?"}
	for _, text := range []string{"Исаев Харон 15000\nЗабрал наличку 27000", "Исаев Харон 15000\n27000 наличными в офис", "Исаев Харон 15000\nПринесли 27000 наличкой"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5aiClarifyWorded(phr, "Исаев Харон")
		grp, emp := e2eGroup("l6ow"), user("79285551202")
		e.groupsCache[grp] = "Оплата"
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		got := dayTotals(e, grp)
		if asks := l6aiAsks(e, n); asks != 1 || sum(got) != 42000 || got["Наличка"] != 27000 {
			t.Errorf("%q: вопросов «❓» %d, сбор %v (%v); ИИ спросил, чья наличка 27000 — бот должен спросить и держать её заглушкой «Наличка»", text, asks, sum(got), got)
		}
		q := e.lastWith("❓")
		if q.ID == "" {
			continue
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got["Магомедов Ахмед"] != 27000 || sum(got) != 42000 {
			t.Errorf("%q, ответ «Магомедов Ахмед» на «❓»: сбор %v, ожидали Исаев Харон 15000 + Магомедов Ахмед 27000", text, got)
		}
	}
	// «Исаев Харон 27000 / 27000 наличными в офис»: та же сумма у платежа с ФИО
	// не «покрывает» непрочитанную парсером строку налички.
	{
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: `{"payments":[{"name":"Исаев Харон","amount":27000,"cash":false}],"clarify":"Чья наличка 27000?"}`}
			}
			return aiReply{Text: ""}
		}
		grp := e2eGroup("l6ow3")
		e.groupsCache[grp] = "Оплата"
		n := len(e.sent())
		e.sendGroup(groupText(grp, user("79285551208"), "Сафаи", "Исаев Харон 27000\n27000 наличными в офис"))
		if got := dayTotals(e, grp); l6aiAsks(e, n) != 1 || got["Наличка"] != 27000 || sum(got) != 54000 {
			t.Errorf("«Исаев Харон 27000 / 27000 наличными в офис»: вопросов %d, сбор %v, ожидали Исаев Харон 27000 + Наличка 27000", l6aiAsks(e, n), got)
		}
	}
	// Наличку с ФИО ИИ записал, а спросил о другом — строка с ФИО не
	// становится заглушкой: заглушек нет, денег не прибавилось.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Исаев Харон","amount":15000,"cash":false},{"name":"Магомедов Ахмед","amount":27000,"cash":true}],"clarify":"Кто забрал наличку 27000?"}`}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup("l6ow2")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, user("79285551207"), "Сафаи", "Исаев Харон 15000\nМагомедов Ахмед 27000 наличными в офис"))
	if got := dayTotals(e, grp); got["Наличка"] != 0 || sum(got) != 42000 {
		t.Errorf("ИИ записал оба платежа и спросил о другом: сбор %v, ожидали Исаев Харон 15000 + Магомедов Ахмед 27000 без заглушки", got)
	}
}

// 14. Итог дня «Нал за сегодня 85000» — не платёж клиента «за сегодня»; ответ
// «это итог за день, не платёж» снимает его из сбора.
func TestL6ai_14_DayTotalAnswerNotPaymentHonoured(t *testing.T) {
	phr := []string{"Чья это наличка 85000?", "От кого наличные 85 000 ₽?", "Это общая наличка за день или платёж одного клиента?"}
	e := newE2E(t, e2eOpts{WithAI: true})
	worded := l5aiClarifyWorded(phr, "")
	named := aiNamesAndPayments("Исаев Харон", "Магомедов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") && strings.Contains(c.lastUserText(), "за сегодня") {
			return worded(c)
		}
		return named(c)
	}
	grp, emp := e2eGroup("l6tot"), user("79285551206")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", "Исаев Харон нал 40000"))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Магомедов Ахмед нал 45000"))
	e.sendGroup(groupText(grp, emp, "Сафаи", "Нал за сегодня 85000"))
	q := e.lastWith("❓")
	if q.ID == "" {
		t.Fatalf("нет вопроса «❓»: %+v", e.sent())
	}
	if got := dayTotals(e, grp); got["за сегодня"] != 0 {
		t.Errorf("итог «Нал за сегодня 85000» записан клиентом «за сегодня»: сбор %v", got)
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Это итог за день, не платёж", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); sum(got) != 85000 {
		t.Errorf("две налички 40000+45000 и итог «Нал за сегодня 85000», ответ «это итог за день, не платёж»: сбор %v (%v) — ожидали 85000 (бот: %q)", sum(got), got, e.last().Text)
	}

	// «Итого нал 85000» — тоже не клиент «итого»; «Нал 27000», ответ «это не
	// платёж, ошибся» — наличка уходит из сбора; ответ с ФИО — по-прежнему ответ.
	for _, c := range []struct{ text, answer, client string }{
		{"итого нал 85000", "Это общая сумма за день", ""},
		{"Нал 27000", "Это не платёж, ошибся", ""},
		{"Нал 27000", "Исаев Харон", "Исаев Харон"},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l5aiClarifyWorded(phr, "")
		grp := e2eGroup("l6tot2")
		e.groupsCache[grp] = "Оплата"
		e.sendGroup(groupText(grp, emp, "Сафаи", c.text))
		got := dayTotals(e, grp)
		if got["итого"] != 0 || got["Наличка"] == 0 {
			t.Errorf("%q: сбор %v, ожидали заглушку «Наличка» до ответа", c.text, got)
		}
		q := e.lastWith("❓")
		if q.ID == "" {
			t.Fatalf("%q: нет вопроса «❓»: %+v", c.text, e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", c.answer, q.ID, e.botJID(), q.Text))
		got = dayTotals(e, grp)
		switch {
		case c.client == "" && sum(got) != 0:
			t.Errorf("%q, ответ %q: сбор %v, ожидали 0 (не платёж; бот: %q)", c.text, c.answer, got, e.last().Text)
		case c.client != "" && (got[c.client] != 27000 || sum(got) != 27000):
			t.Errorf("%q, ответ %q: сбор %v, ожидали %s 27000", c.text, c.answer, got, c.client)
		}
	}
}

// l6aiCashByLine — ИИ читает «ФИО нал сумма у Адама» построчно, имя — как написано.
func l6aiCashByLine(c aiCall) aiReply {
	if r, ok := fakeNameExtract(c); ok {
		return r
	}
	if !strings.Contains(c.System, "модуль разбора платежей") {
		return aiReply{Text: ""}
	}
	u := c.lastUserText()
	if i := strings.Index(u, "Строки:\n"); i >= 0 {
		u = u[i+len("Строки:\n"):]
	}
	if i := strings.Index(u, "\n\nЧисла-суммы"); i >= 0 {
		u = u[:i]
	}
	var ps []string
	for _, ln := range strings.Split(u, "\n") {
		var name []string
		var amt float64
		cash := false
		for _, f := range strings.Fields(ln) {
			if v := parseAmountWord(f); v > 0 {
				amt = v
				break
			}
			if f == "нал" {
				cash = true
				continue
			}
			name = append(name, f)
		}
		if amt > 0 && len(name) > 0 {
			coll := ""
			if strings.Contains(ln, "у Адама") {
				coll = "Адам"
			}
			ps = append(ps, fmt.Sprintf(`{"name":%q,"amount":%v,"cash":%v,"collector":%q}`, strings.Join(name, " "), amt, cash, coll))
		}
	}
	return aiReply{Text: `{"payments":[` + strings.Join(ps, ",") + `],"clarify":""}`}
}

// 15. Наличку прислали повторно с исправленным/дописанным ФИО, бот держит её и
// спрашивает «новый или тот же?»; ответ «тот же» — один платёж на ИСПРАВЛЕННОЕ
// (полное) ФИО. Полное имя прежней записи не меняется на короткое, а имя
// известного клиента — на опечатку нового.
func TestL6ai_15_SameAnswerKeepsCorrectedClient(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		before, first, second, want string
	}{
		{"", "Исаев Харн", "Исаев Харон", "Исаев Харон"},
		{"", "Исаев", "Исаев Харон", "Исаев Харон"},
		{"", "Исаев Харон", "Исаев", "Исаев Харон"},
		{"Исаев Харон 5000", "Исаев Харон", "Исаев Харн", "Исаев Харон"},
	} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = l6aiCashByLine
		grp, emp := e2eGroup("l6same"), user("79285551203")
		e.groupsCache[grp] = "Оплата"
		extra := 0.0
		if c.before != "" {
			e.sendGroup(groupText(grp, emp, "Сафаи", c.before))
			extra = 5000
		}
		e.sendGroup(groupText(grp, emp, "Сафаи", c.first+" нал 15000 у Адама"))
		e.sendGroup(groupText(grp, emp, "Сафаи", c.second+" нал 15000 у Адама"))
		e.age(grp.String())
		e.clarifyTick(ctx)
		q := e.lastWith("НОВЫЙ платёж")
		if q.ID == "" {
			t.Fatalf("%q→%q: нет вопроса «новый или тот же?»: %+v", c.first, c.second, e.sent())
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "тот же", q.ID, e.botJID(), q.Text))
		if got := dayTotals(e, grp); got[c.want] != 15000+extra || sum(got) != 15000+extra {
			t.Errorf("«%s нал 15000», затем «%s нал 15000», ответ «тот же»: сбор %v — ожидали %s %v", c.first, c.second, got, c.want, 15000+extra)
		}
	}
}

// 28. Чек с одной датой операции без времени (парсер ставит 12:00) и утренний
// текст «ФИО сумма» того же платежа — один платёж, когда бы чек ни прислали.
func TestL6ai_28_DateOnlyCheckTimeIsGuessed(t *testing.T) {
	for _, post := range []time.Duration{11*time.Hour + 30*time.Minute, 13 * time.Hour} {
		grp := e2eGroup("l6dt")
		e := newE2E(t, e2eOpts{WithAI: true, Groups: map[string]string{grp.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед")
		saf := user("79001115611")
		day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.Local)
		m := groupText(grp, saf, "Сафаи", "Магомедов Ахмед 15000")
		m.Info.Timestamp = day.Add(8*time.Hour + 40*time.Minute)
		e.sendGroup(m)
		ctx := context.Background()
		caption := "Магомедов Ахмед"
		ocr := "Почта Банк\nКвитанция о переводе\nДата операции: 14.10.2026\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n15 000 ₽\nНомер операции\nP" + strings.Trim(e2eID(""), "-")[:10]
		text := caption + "\n" + ocr
		when := day.Add(post)
		waID := e2eID("CHK")
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), saf.String(), saf.User, "Сафаи", text, true, "", when)
		if err != nil {
			t.Fatal(err)
		}
		msg := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: saf, IsGroup: true},
			ID: waID, PushName: "Сафаи", Timestamp: when}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
		payer, amt := e.resolveReceiptPayer(ctx, msg, caption)
		e.handleBankReceipt(ctx, grp, saf.String(), waID, text, rawID, when, nil, ".jpg", payer, amt)
		e.waitIdle()
		got := l4Total(e, nil, day, day.AddDate(0, 0, 1))
		if got != 15000 {
			t.Errorf("текст «Магомедов Ахмед 15000» в 08:40 и чек этого перевода (дата без времени), присланный в %s: сбор %v, хочу 15000",
				when.Format("15:04"), got)
		}
		if !e.db.TestReceiptTimeGuessed(ctx, waID) {
			r := e.db.TestReceiptByMessage(ctx, waID)
			t.Errorf("чек с одной датой (прислан в %s): время %s записано как напечатанное, ожидали угаданное", when.Format("15:04"), r.TxDate.Format("02.01 15:04"))
		}
	}
}

// 28б. Дата без времени от ИИ — полдень и «время придумано»; с временем — нет.
func TestL6ai_28_AIDateOnlyIsDefaulted(t *testing.T) {
	for _, c := range []struct {
		in       string
		want     time.Time
		dateOnly bool
	}{
		{"2026-10-14", time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local), true},
		{"14.10.2026", time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local), true},
		{"2026-10-14 08:41", time.Date(2026, 10, 14, 8, 41, 0, 0, time.Local), false},
		{"14.10.2026 08:41:05", time.Date(2026, 10, 14, 8, 41, 5, 0, time.Local), false},
	} {
		got, ok, dateOnly := parseAIDatetime(c.in)
		if !ok || !got.Equal(c.want) || dateOnly != c.dateOnly {
			t.Errorf("%q: %v ok=%v dateOnly=%v, ожидали %v dateOnly=%v", c.in, got, ok, dateOnly, c.want, c.dateOnly)
		}
	}
	// Только дата от ИИ не затирает напечатанное время того же дня.
	rd := parser.ParseReceipt("Перевод выполнен\nДата операции 14.10.2026 08:41\nСумма перевода\n7 000 ₽")
	applyAIReceiptAuthoritative(&rd, aiReceipt{Kind: "receipt", Amount: 7000, Datetime: "2026-10-14"})
	if rd.TxTimeGuessed() || rd.TxTime.Format("15:04") != "08:41" {
		t.Errorf("ИИ дал только дату: время %s (угадано=%v), ожидали напечатанное 08:41", rd.TxTime.Format("15:04"), rd.TxTimeGuessed())
	}
	// Чтение ИИ с одной датой — время угаданное.
	var rd2 parser.ReceiptData
	applyAIReceiptAuthoritative(&rd2, aiReceipt{Kind: "receipt", Amount: 7000, Datetime: "2026-10-14"})
	if !rd2.HasTxTime || !rd2.TxTimeGuessed() {
		t.Errorf("ИИ дал только дату: найдено=%v угадано=%v, ожидали дату с угаданным временем", rd2.HasTxTime, rd2.TxTimeGuessed())
	}
}

// 14б. Что считается ответом «это не платёж» на «❓ чей платёж?» и «именем»
// строки, которое на деле служебные слова.
func TestL6ai_14_NotPaymentAnswerWords(t *testing.T) {
	for _, c := range []struct {
		text  string
		asked float64
		swipe bool
		want  bool
	}{
		{"Это итог за день, не платёж", 85000, true, true},
		{"Это общая сумма за день", 85000, true, true},
		{"итого 85000", 85000, true, true},
		{"Это не платёж, ошибся", 27000, false, true},
		{"не считай", 27000, false, true},
		{"Отчёт за день", 27000, false, false},
		{"итого 90000", 85000, true, false},
		{"Исаев Харон, итог", 85000, true, false},
		{"Исаев Харон", 27000, true, false},
		{"Магомедов Ахмед не платил", 27000, true, false},
	} {
		if got := notPaymentAnswer(c.text, c.asked, c.swipe); got != c.want {
			t.Errorf("%q (свайп=%v): «не платёж»=%v, ожидали %v", c.text, c.swipe, got, c.want)
		}
	}
	for _, c := range []struct {
		raw  string
		want bool
	}{
		{"за сегодня", true}, {"итого", true}, {"Забрал", true}, {"Принесли", true},
		{"Исаев Харон", false}, {"Исаев", false}, {"Исаев Харон за октябрь", false},
	} {
		if got := notClientName(c.raw); got != c.want {
			t.Errorf("notClientName(%q)=%v, ожидали %v", c.raw, got, c.want)
		}
	}
}
