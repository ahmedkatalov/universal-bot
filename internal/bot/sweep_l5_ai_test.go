package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Регрессии L5 (ИИ-разбор текстов и дата чека): время «18 октября 2026 в
// 14:05», наличка без имени при разных формулировках вопроса, предлог «к» после
// суммы, повтор налички с дописанным/исправленным именем.

// l5aiCheckNamed — текст чека с датой операции словом месяца и «в» перед временем.
func l5aiCheckNamed(at time.Time, sep string) string {
	months := []string{"", "января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
	return "Перевод выполнен\nДата операции " + at.Format("2") + " " + months[at.Month()] + " " + at.Format("2006") + sep + at.Format("15:04") +
		"\nПолучатель\nХадижат Ибрагимовна И.\nОтправитель\nХарон Исаевич И.\nСумма перевода\n7 000 ₽"
}

// 18. «Дата операции 18 октября 2026 в 15:03» — это 15:03, а не полночь: два
// перевода клиента за день на одну сумму (в 15:03 и 21:03) — два платежа.
func TestL5ai_18_NamedMonthDateKeepsTime(t *testing.T) {
	ctx := context.Background()
	for _, sep := range []string{" в ", ", ", " г. в "} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = aiNamesAndPayments("Исаев Харон")
		grp, emp := e2eGroup("l5nmd"), user("79001115930")
		t2 := time.Now().Add(-5 * time.Minute).Truncate(time.Minute)
		t1 := t2.Add(-6 * time.Hour)
		if t1.Day() != t2.Day() {
			t1 = time.Date(t2.Year(), t2.Month(), t2.Day(), 0, 1, 0, 0, time.Local)
		}
		a := e.sendReceipt(grp, emp, "Сафаи", l5aiCheckNamed(t1, sep), "Исаев Харон")
		e.sendReceipt(grp, emp, "Сафаи", l5aiCheckNamed(t2, sep), "Исаев Харон")
		if r := e.db.TestReceiptByMessage(ctx, a); !r.TxDate.Equal(t1) {
			t.Errorf("%q: время операции на чеке %s, записано %s", sep, t1.Format("02.01 15:04"), r.TxDate.Format("02.01 15:04"))
		}
		if got := dayTotals(e, grp); got["Исаев Харон"] != 14000 {
			t.Errorf("%q: два перевода в %s и %s: сбор %v, ожидали Исаев Харон 14000 (бот: %q)", sep, t1.Format("15:04"), t2.Format("15:04"), got, e.last().Text)
		}
	}
}

// l5aiClarifyWorded — ИИ, чьи три чтения не записывают наличку без имени и
// спрашивают о ней КАЖДОЕ своими словами (как настоящая модель при разной
// температуре). named — платёж с ФИО, который чтения видят одинаково.
func l5aiClarifyWorded(phr []string, named string) func(c aiCall) aiReply {
	var mu sync.Mutex
	k := 0
	return func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		mu.Lock()
		q := phr[k%len(phr)]
		k++
		mu.Unlock()
		ps := ""
		if named != "" && strings.Contains(c.lastUserText(), strings.Fields(named)[0]) {
			ps = `{"name":"` + named + `","amount":15000,"cash":false}`
		}
		return aiReply{Text: `{"payments":[` + ps + `],"clarify":"` + q + `"}`}
	}
}

// 19. «Нал 27000» без имени: чтения ИИ согласны, что это платёж без клиента,
// но формулируют вопрос по-разному — всё равно спросить «❓» один раз и держать
// 27000 в сборе заглушкой «Наличка» до ответа. Молча терять деньги нельзя.
func TestL5ai_19_UnnamedCashAskedWhenReadsWordQuestionDifferently(t *testing.T) {
	phr := []string{"Чья это наличка 27000?", "От кого наличные 27 000 ₽?", "Кто клиент по наличке 27000?"}
	for _, text := range []string{"Нал 27000", "27000 наличка", "Исаев Харон 15000\nНал 27000"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		named := ""
		if strings.Contains(text, "Исаев") {
			named = "Исаев Харон"
		}
		e.ai = l5aiClarifyWorded(phr, named)
		grp, emp := e2eGroup("l5cl"), user("79285551003")
		e.groupsCache[grp] = "Оплата"
		n := len(e.sent())
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		got := dayTotals(e, grp)
		asks := 0
		for _, m := range e.sentSince(n) {
			if strings.HasPrefix(m.Text, "❓") {
				asks++
			}
		}
		want := 27000.0
		if named != "" {
			want += 15000
		}
		if asks != 1 || got["Наличка"] != 27000 || sum(got) != want {
			t.Errorf("%q: вопросов «❓» %d, сбор %v (%v); ожидали один вопрос и 27000 заглушкой «Наличка» до ответа (всего %v)", text, asks, sum(got), got, want)
		}
	}

	// Ответ на вопрос даёт заглушке клиента.
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = l5aiClarifyWorded(phr, "")
	grp, emp := e2eGroup("l5cla"), user("79285551004")
	e.groupsCache[grp] = "Оплата"
	e.sendGroup(groupText(grp, emp, "Сафаи", "Нал 27000"))
	q := e.lastWith("❓")
	if q.ID == "" {
		t.Fatalf("нет вопроса «❓»: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Исаев Харон", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, grp); got["Исаев Харон"] != 27000 || sum(got) != 27000 {
		t.Errorf("ответ «Исаев Харон» на «❓»: сбор %v, ожидали Исаев Харон 27000", got)
	}

	// Меньшинство чтений спросило (двое увидели болтовню) — вопроса нет.
	e = newE2E(t, e2eOpts{WithAI: true})
	var mu sync.Mutex
	k := 0
	e.ai = func(c aiCall) aiReply {
		if r, ok := fakeNameExtract(c); ok {
			return r
		}
		if !strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: ""}
		}
		mu.Lock()
		k++
		first := k == 1
		mu.Unlock()
		if first {
			return aiReply{Text: `{"payments":[],"clarify":"Чья это наличка 27000?"}`}
		}
		return aiReply{Text: `{"payments":[],"clarify":""}`}
	}
	grp, emp = e2eGroup("l5clm"), user("79285551005")
	e.groupsCache[grp] = "Оплата"
	n := len(e.sent())
	e.sendGroup(groupText(grp, emp, "Сафаи", "Нал 27000"))
	for _, m := range e.sentSince(n) {
		if strings.HasPrefix(m.Text, "❓") {
			t.Errorf("спросило лишь одно чтение из трёх — переспрашивать не нужно: %q", m.Text)
		}
	}
}

// 20. «Исаев Харон 15000 к Адаму» — «к» тут предлог (кому отдали наличку), а не
// «тысяч»: сумма 15000, а не 15 000 000. Склеенное «15к» и «31 т» — тысячи.
func TestL5ai_20_KPrepositionNotThousands(t *testing.T) {
	for _, text := range []string{"Исаев Харон 15000 к Адаму", "Исаев Харон наличка 15000 к Адаму", "Исаев Харон 15000 т Адаму"} {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: `{"payments":[{"name":"Исаев Харон","amount":15000,"cash":true,"collector":"Адам"}],"clarify":""}`}
			}
			if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
				return aiReply{Text: `{"client":"Исаев Харон"}`}
			}
			return aiReply{Text: ""}
		}
		grp, emp := e2eGroup("l5kk"), user("79285551002")
		e.groupsCache[grp] = "Оплата"
		e.sendGroup(groupText(grp, emp, "Сафаи", text))
		if got := dayTotals(e, grp); got["Исаев Харон"] != 15000 || sum(got) != 15000 {
			t.Errorf("%q: у Исаева %v, ожидали 15000 (%v)", text, got["Исаев Харон"], got)
		}
	}
	// Подсказка ИИ и страховка по сумме читают «к»/«т» так же, как парсер.
	cases := map[string][]float64{
		"Исаев Харон 15000 к Адаму":   {15000},
		"Отдал 15000 к Адаму":         {15000},
		"Исаев Харон 15к":             {15000},
		"Исаев Харон 15 к":            {15000},
		"У Усумова Рауфа забрал 31 т": {31000},
		"Исаев Харон 15k":             {15000},
		"Исаев Харон 15000 т":         {15000},
	}
	for in, want := range cases {
		got := moneyTokens(in)
		if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
			t.Errorf("moneyTokens(%q) = %v, ожидали %v", in, got, want)
		}
	}
}

// 23. Наличку переотправили с исправленным именем (пропущенная буква в коротком
// имени, или сначала только фамилия, потом ФИО) — тот же клиент, та же сумма за
// 30 дней: придержать и спросить «новый или тот же?». Разных клиентов
// (Омаров/Умаров, Магомед/Магомедов Ахмед, Алан/Аслан) не придерживать.
func TestL5ai_23_CashResentWithFixedNameHeld(t *testing.T) {
	emp := user("79285551005")
	run := func(first, second string) (float64, int, *e2eBot, types.JID) {
		e := newE2E(t, e2eOpts{WithAI: true})
		e.ai = func(c aiCall) aiReply {
			if r, ok := fakeNameExtract(c); ok {
				return r
			}
			if !strings.Contains(c.System, "модуль разбора платежей") {
				return aiReply{Text: ""}
			}
			name := first
			if strings.Contains(c.lastUserText(), second+" ") {
				name = second
			}
			return aiReply{Text: `{"payments":[{"name":"` + name + `","amount":15000,"cash":true}],"clarify":""}`}
		}
		grp := e2eGroup("l5ty")
		e.groupsCache[grp] = "Оплата"
		e.sendGroup(groupText(grp, emp, "Сафаи", first+" нал 15000"))
		e.sendGroup(groupText(grp, emp, "Сафаи", second+" нал 15000"))
		return sum(dayTotals(e, grp)), l4textsHeld(e, grp), e, grp
	}
	for _, p := range [][2]string{{"Исаев Харн", "Исаев Харон"}, {"Магомедов Ахмд", "Магомедов Ахмед"},
		{"Сулейманов Тимр", "Сулейманов Тимур"}, {"Исаев", "Исаев Харон"}, {"Исаев Иса", "Исаев Исса"}} {
		if s, held, _, _ := run(p[0], p[1]); s != 15000 || held != 1 {
			t.Errorf("«%s нал 15000», затем исправленное «%s нал 15000»: сбор %v, придержано %d — ожидали 15000 и вопрос «новый или тот же?»",
				p[0], p[1], s, held)
		}
	}
	for _, p := range [][2]string{{"Омаров Ахмед", "Умаров Ахмед"}, {"Магомед", "Магомедов Ахмед"},
		{"Исаев Алан", "Исаев Аслан"}, {"Исаев Али", "Исаев Алви"}, {"Исаев Харон", "Исаев Ахмед"}} {
		if s, held, _, _ := run(p[0], p[1]); s != 30000 || held != 0 {
			t.Errorf("«%s» и «%s» — разные клиенты: сбор %v, придержано %d — ожидали 30000 и без придержки", p[0], p[1], s, held)
		}
	}

	// Вопрос называет прежнюю запись, ответ «тот же» оставляет одну наличку.
	ctx := context.Background()
	_, _, e, g := run("Исаев", "Исаев Харон")
	e.age(g.String())
	e.clarifyTick(ctx)
	q := e.lastWith("НОВЫЙ платёж")
	if q.ID == "" {
		t.Fatalf("нет вопроса «новый или тот же?»: %+v", e.sent())
	}
	if !strings.Contains(q.Text, "«Исаев»") {
		t.Errorf("вопрос должен назвать прежнюю запись «Исаев»: %q", q.Text)
	}
	e.sendGroup(groupReply(g, emp, "Сафаи", "тот же", q.ID, e.botJID(), q.Text))
	if got := dayTotals(e, g); sum(got) != 15000 {
		t.Errorf("ответ «тот же»: сбор %v (%v), ожидали 15000", sum(got), got)
	}
}
