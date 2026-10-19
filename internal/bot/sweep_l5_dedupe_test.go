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
)

// Регрессии проверки «этот чек уже присылали» (l5 dedupe): NULL в номере
// операции, два настоящих чека с разным напечатанным временем, перечитанный
// после сбоя ИИ чек и один чек, подписанный в двух группах разными клиентами.

// 12: первая копия без номера операции (doc_number NULL), вторая — полный чек
// с номером: проверка дубля не должна падать на NULL, платёж один.
func TestL5dedupe_1_NullDocNumber(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	op := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	doc := "A6290101010101010G10040011791177"
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if strings.Contains(c.System, "модуль разбора изображений") {
			if strings.Contains(c.lastUserText(), "Перевод выполнен") {
				return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"","status":"Перевод выполнен","datetime":""}`}
			}
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"commission":0,"doc_number":"` + doc + `","status":"Выполнено","datetime":"` + op.Format("2006-01-02 15:04:05") + `"}`}
		}
		return aiReply{Text: ""}
	}
	grp, emp := e2eGroup("l5nulldoc"), user("79001115923")
	screen := l4intakePhoto(e, grp, emp, "Сафаи", "СберБанк\nПеревод выполнен\nХадижат Ибрагимовна И.\n5 000 ₽\nСохранить чек", "Исаев Харон", "")
	full := "СберБанк\nЧек по операции\nДата операции " + op.Format("02.01.2006 15:04:05") +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n5000.00 ₽\nНомер операции в СБП\n" + doc
	ck2 := l4intakePhoto(e, grp, emp, "Сафаи", full, "Исаев Харон", "")
	if got := dayTotals(e, grp); got["Исаев Харон"] != 5000 {
		t.Errorf("тот же перевод (экран + полный чек через минуту, тот же клиент и сумма): сбор %v, ожидали Исаев Харон 5000", got)
	}
	// Совпало без общего номера (у экрана его нет) — путь назад «другой».
	if w := e.last(); !strings.Contains(w.Text, "другой") {
		t.Errorf("повтор без общего номера — нужен путь назад «другой»: %q", w.Text)
	}
	// В учёте остаётся полный чек (дата операции и номер — с него), повтор —
	// экран без времени и номера (l6 находка 07).
	o, err := e.db.DuplicateOriginalOf(context.Background(), screen)
	if err != nil || !o.Found || o.Strong || o.KeptWa != ck2 {
		t.Errorf("оригинал повтора-экрана — полный чек %s: %+v, ошибка %v", ck2, o, err)
	}
}

// 12 (уровень БД): у ранней копии нет номера операции — запросы проверки
// повтора не падают на NULL и находят её.
func TestL5dedupe_2_NullDocNumberQueries(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	grp, emp := e2eGroup("l5nulldb"), user("79001115924")
	at := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	noDoc := "СберБанк\nЧек по операции\nДата операции " + at.Format("02.01.2006 15:04:05") +
		"\nФИО получателя\nХадижат Ибрагимовна И.\nСумма перевода\n7000.00 ₽"
	ck := e.sendReceipt(grp, emp, "Сафаи", noDoc, "Исаев Харон")
	if r := e.db.TestReceiptByMessage(ctx, ck); r.Duplicate || r.Amount != 7000 {
		t.Fatalf("первый чек: %+v", r)
	}
	cid, err := e.db.GetOrCreateContact(ctx, "Исаев Харон")
	if err != nil {
		t.Fatal(err)
	}
	const doc = "B111222333444555666777888"
	m, err := e.db.FindDuplicateReceiptEx(ctx, grp.String(), doc, "", &cid, "Исаев Харон", "", 7000, at, false)
	if err != nil || !m.Found || m.StrongID || m.WaMessage != ck {
		t.Fatalf("тот же чек с номером (у ранней копии номера нет): %+v, ошибка %v", m, err)
	}
	// Время с чека не прочиталось (время сообщения через 40 с) — совпадение в окне.
	if m, err := e.db.FindDuplicateReceiptEx(ctx, grp.String(), doc, "", &cid, "Исаев Харон", "", 7000, at.Add(40*time.Second), true); err != nil || !m.Found {
		t.Errorf("время угадано: %+v, ошибка %v", m, err)
	}
	// Напечатано другое время (до секунды) — другой перевод.
	if m, err := e.db.FindDuplicateReceiptEx(ctx, grp.String(), doc, "", &cid, "Исаев Харон", "", 7000, at.Add(40*time.Second), false); err != nil || m.Found {
		t.Errorf("напечатано другое время — другой чек, а нашёлся повтор: %+v, ошибка %v", m, err)
	}
}

// 13: два разных перевода на одну сумму (лимит банка), напечатанное время
// разное — оба засчитаны; без подписи второй не помечен повтором.
func TestL5dedupe_3_TwoRealChecksMinutesApart(t *testing.T) {
	ctx := context.Background()
	rshb := func(at time.Time) string {
		return "РСХБ\nПеревод по номеру телефона через СБП\nИсполнен\n18000 ₽\n" + at.Format("02.01.2006, 15:04") +
			"\nКомиссия\n0 ₽\nПолучатель\nАхмед Нажудович К\nНомер телефона получателя\n79287836800\nБанк получателя\nСбербанк"
	}
	sberNoDoc := func(at time.Time) string {
		return "СберБанк\nЧек по операции\nДата операции " + at.Format("02.01.2006 15:04:05") +
			"\nФИО получателя\nХадижат Ибрагимовна И.\nФИО отправителя\nХарон Исаевич И.\nСумма перевода\n18000.00 ₽"
	}
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	for name, mk := range map[string]func(time.Time) string{"РСХБ": rshb, "Сбер без номера": sberNoDoc} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		grp, emp := e2eGroup("l5lim"), user("79001115925")
		e.sendReceipt(grp, emp, "Сафаи", mk(base), "Исаев Харон")
		e.sendReceipt(grp, emp, "Сафаи", mk(base.Add(150*time.Second)), "Исаев Харон")
		if got := dayTotals(e, grp); got["Исаев Харон"] != 36000 {
			t.Errorf("%s: два перевода (время на чеках %s и %s): сбор %v, ожидали Исаев Харон 36000; бот: %q",
				name, base.Format("15:04:05"), base.Add(150*time.Second).Format("15:04:05"), got, e.last().Text)
		}
	}
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Исаев Харон")
	grp, emp := e2eGroup("l5lim2"), user("79001115926")
	e.sendReceipt(grp, emp, "Сафаи", rshb(base), "")
	b := e.sendReceipt(grp, emp, "Сафаи", rshb(base.Add(2*time.Minute)), "")
	if r := e.db.TestReceiptByMessage(ctx, b); r.Duplicate {
		t.Errorf("без подписи: чек с другим временем (%s) помечен повтором и не будет спрошен «чей чек?»: бот %q",
			base.Add(2*time.Minute).Format("15:04"), e.last().Text)
	}
}

// 15: после сбоя ИИ автоповтор дочитал два настоящих чека с разным временем —
// оба в сборе; а совпавший по клиенту и времени без номера — спрашиваем
// «тот же или другой?», и ответ «другой» возвращает его в сбор.
func TestL5dedupe_4_RetrySecondRealCheck(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, t1, t2 time.Time, docB string, want float64) {
		e := newE2E(t, e2eOpts{WithAI: true})
		day := time.Now().Format("2006-01-02")
		dir := t.TempDir()
		pa, pb := filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.jpg")
		_ = os.WriteFile(pa, []byte("\xff\xd8\xff\xe0 first transfer photo"), 0o644)
		_ = os.WriteFile(pb, []byte("\xff\xd8\xff\xe0 second transfer photo"), 0o644)
		b64b := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0 second transfer photo"))
		e.ai = func(c aiCall) aiReply {
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
			if strings.Contains(c.System, "модуль разбора изображений") {
				raw, _ := json.Marshal(c.Raw)
				at, doc := t1, ""
				if strings.Contains(string(raw), b64b) {
					at, doc = t2, docB
				}
				return aiReply{Text: `{"kind":"receipt","bank":"ВТБ","recipient":"Хадижат Ибрагимовна И.","sender":"Харон Исаевич И.","amount":15000,"commission":0,"doc_number":"` + doc + `","status":"Выполнено","datetime":"` + day + ` ` + at.Format("15:04:05") + `"}`}
			}
			return aiReply{Text: ""}
		}
		healthy := e.assistant
		e.assistant = ai.New("k", "m", "v", "http://127.0.0.1:1")
		grp, emp := e2eGroup("l5rlim"), user("79001115928")
		blurred := "ВТБ\nИсходящий перевод СБП\nХадижат Ибрагимовна И.\nСтатус Выполнено\nИмя плательщика Харон Исаевич И.\nСумма операции S OOO P"
		l4intakePhoto(e, grp, emp, "Сафаи", blurred, "Исаев Харон", pa)
		l4intakePhoto(e, grp, emp, "Сафаи", blurred, "Исаев Харон", pb)
		e.assistant = healthy
		items, err := e.db.DueReceiptRetries(ctx, time.Now().Add(time.Hour), maxReceiptRetries(), 10)
		if err != nil || len(items) != 2 {
			t.Fatalf("автоповтор: %v %v", items, err)
		}
		for _, it := range items {
			e.retryOneReceipt(ctx, it)
		}
		e.waitIdle()
		if got := dayTotals(e, grp); got["Исаев Харон"] != want {
			t.Fatalf("перечитали чеки (время %s и %s): сбор %v, ожидали %.0f; бот: %q",
				t1.Format("15:04:05"), t2.Format("15:04:05"), got, want, e.last().Text)
		}
		if want == 30000 {
			return
		}
		// Совпали клиент, сумма и время, номера нет — не наверняка: путь назад.
		w := e.lastWith("тот же чек")
		if !strings.Contains(w.Text, "другой") {
			t.Fatalf("перечитанный чек без номера назван повтором без «другой»: %q", w.Text)
		}
		e.sendGroup(groupReply(grp, emp, "Сафаи", "другой", w.ID, e.botJID(), w.Text))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 30000 {
			t.Errorf("ответ «другой» не вернул чек в сбор: %v (бот: %q)", got, e.last().Text)
		}
	}
	t1 := time.Now().Add(-40 * time.Minute).Truncate(time.Second)
	t.Run("разное время", func(t *testing.T) { run(t, t1, t1.Add(150*time.Second), "", 30000) })
	t.Run("то же время", func(t *testing.T) { run(t, t1, t1, "", 15000) })
	// 12: номер прочитался только у второго — у засчитанного он NULL.
	t.Run("номер только у второго", func(t *testing.T) { run(t, t1, t1, "A6290101010101010G10040011791178", 15000) })
}

// 10: один и тот же чек (тот же номер операции) подписан в двух группах
// РАЗНЫМИ клиентами — бот спрашивает во второй группе, чей он, и ответ
// переносит платёж в обеих копиях.
func TestL5dedupe_5_SameCheckTwoClientsAsk(t *testing.T) {
	sb, mg := e2eGroup("l5ta"), e2eGroup("l5tb")
	for _, answer := range []string{"Исаев Харон", "Магомедов Ахмед"} {
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Groups: map[string]string{sb.String(): "СБ оплата", mg.String(): "Оплата клиентов"}})
		e.ai = aiNamesAndPayments("Магомедов Ахмед", "Исаев Харон")
		ocr := sberCheck("15000.00", "10:00:00")
		e.sendReceipt(sb, user("79001119108"), "Сафаи", ocr, "Магомедов Ахмед")
		n := len(e.sent())
		emp := user("79001119109")
		e.sendReceipt(mg, emp, "Ислам", ocr, "Исаев Харон")
		var q e2eSent
		for _, m := range e.sentSince(n) {
			if m.Chat == mg.String() && strings.Contains(m.Text, "Исаев") && strings.Contains(m.Text, "Магомедов") && strings.Contains(m.Text, "СБ оплата") {
				q = m
			}
		}
		if q.ID == "" {
			t.Fatalf("чек подписан в «Оплата клиентов» «Исаев Харон», а в «СБ оплата» — «Магомедов Ахмед»: бот не спросил, чей он; бот: %+v", e.sentSince(n))
		}
		e.sendGroup(groupReply(mg, emp, "Ислам", answer, q.ID, e.botJID(), q.Text))
		other := "Магомедов Ахмед"
		if answer == other {
			other = "Исаев Харон"
		}
		for _, g := range []struct {
			jid  string
			tots map[string]float64
		}{{"СБ", dayTotals(e, sb)}, {"основная", dayTotals(e, mg)}} {
			if g.tots[answer] != 15000 || g.tots[other] != 0 {
				t.Errorf("ответ «%s»: сбор %s %v, ожидали %s 15000; бот: %q", answer, g.jid, g.tots, answer, e.last().Text)
			}
		}
	}
}
