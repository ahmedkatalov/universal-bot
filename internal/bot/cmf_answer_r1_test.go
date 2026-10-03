package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/cmf"
)

// Регрессии враждебной проверки ответа «за кого этот платёж?».

const capIncident = "Альмурзаева Марха А"

func TestCmfAnswerR1Parse(t *testing.T) {
	opts := screenshotCands
	// 1. Отрицание — никогда не выбор отвергнутого.
	for _, s := range []string{"не Мусиева", "не сайдаева марха", "ни Мусиева ни Сайдаева", "нет это не она это Альмурзаева Разет"} {
		if a, _ := parseCmfAnswer(s, capIncident, opts); a.Kind == ansPick {
			t.Errorf("#1 %q: %+v", s, a)
		}
	}
	// 2. «нет такого, это X» — не «нет в программе».
	for _, s := range []string{"её нет в списке, это Дудаева Разет", "Марха не клиент, платит за Дудаеву Разет", "нет такого, это Дудаева Разет", "не новый клиент, Дудаева Разет"} {
		if a, _ := parseCmfAnswer(s, capIncident, opts); a.Kind == ansNotInProgram {
			t.Errorf("#2 %q: %+v", s, a)
		}
	}
	for _, s := range []string{"нет в программе", "его нет в программе", "новый клиент", "нету", "её нет в базе"} {
		if a, ok := parseCmfAnswer(s, capIncident, opts); a.Kind != ansNotInProgram || !ok {
			t.Errorf("#2+ %q: %+v %v", s, a, ok)
		}
	}
	// 5. Фразы — не ФИО.
	for _, s := range []string{"никто из них", "ни одна из них", "новая клиентка", "салам алейкум", "Ассаламу Алейкум", "сейчас уточню у Мадины", "спрошу у Магомеда"} {
		a, ok := parseCmfAnswer(s, capIncident, opts)
		if a.Kind == ansClient && ok {
			t.Errorf("#5 %q: уверенно принято за ФИО: %+v", s, a)
		}
		if a.Kind == ansPick {
			t.Errorf("#5 %q: выбор: %+v", s, a)
		}
	}
	for s, want := range map[string]int{"второй вариант": 1, "вторая марха": 1, "первая марха": 0, "2️⃣": 1} {
		if a, ok := parseCmfAnswer(s, capIncident, opts); a.Kind != ansPick || a.Pick != want || !ok {
			t.Errorf("#5 %q: %+v %v", s, a, ok)
		}
	}
	// Повтор имени плательщика — не выбор (единственный вариант-тёзка).
	if a, _ := parseCmfAnswer("Марха", capIncident, opts[:1]); a.Kind == ansPick {
		t.Errorf("повтор подписи: %+v", a)
	}
}

func TestCmfAnswerR1Names(t *testing.T) {
	// 11–15. Тёзки по имени — не «подходящие», отчество — не фамилия, инициалы важны.
	cases := []struct {
		caption string
		cands   []cmf.ClientInfo
		want    string
		notWant string
	}{
		{"Исаева Марха", []cmf.ClientInfo{{ID: "1", FullName: "Мусаева Марха Ахмедовна"}, {ID: "2", FullName: "Хасаева Марха Висхановна"}}, "совпадает только имя", "подходящих"},
		{"Альмурзаева Мадина", []cmf.ClientInfo{{ID: "1", FullName: "Альмурзаева Зарина Ахмедовна"}, {ID: "2", FullName: "Альмурзаева Марина Ахмедовна"}}, "та же фамилия", "подходящих"},
		{"Ахмедова Марха", []cmf.ClientInfo{{ID: "1", FullName: "Мусиева Марха Ахмедовна"}, {ID: "2", FullName: "Сайдаева Марха Ахмедовна"}}, "совпадает только имя", "подходящих"},
		{"Мусаева Хеда", []cmf.ClientInfo{{ID: "1", FullName: "Абаева Зарема Мусаевна"}}, "не нашёл", "той же фамилией"},
		{"Альмурзаева Мадина", []cmf.ClientInfo{{ID: "1", FullName: "Мусиева Мадина Ахмедовна"}, {ID: "2", FullName: "Сайдаева Мадина Ибрагимовна"}}, "совпадает только имя", "та же фамилия"},
		{"Альмурзаева М.", []cmf.ClientInfo{{ID: "1", FullName: "Альмурзаева Разет Ахмедовна"}}, "той же фамилией", "похоже на"},
	}
	for _, c := range cases {
		q, _ := cmfAskText(c.caption, 19000, c.cands)
		if !strings.Contains(q, c.want) || strings.Contains(q, c.notWant) {
			t.Errorf("%q: %q", c.caption, q)
		}
	}
	_, opts := cmfAskText("Альмурзаева Р.", 19000, []cmf.ClientInfo{{ID: "1", FullName: "Альмурзаева Аминат Исаевна"}, {ID: "2", FullName: "Альмурзаева Разет Ахмедовна"}})
	if opts[0].ID != "2" {
		t.Errorf("#15 инициал Р — Разет первой: %v", opts)
	}
	many := []cmf.ClientInfo{{ID: "9", FullName: "Альмурзаева Разет Ахмедовна"}}
	for i := 0; i < 6; i++ {
		many = append(many, cmf.ClientInfo{ID: fmt.Sprint(i), FullName: fmt.Sprintf("Абаева%d Мадина", i)})
	}
	if _, opts := cmfAskText("Альмурзаева Мадина", 1000, many); opts[0].ID != "9" {
		t.Errorf("#14 родственница первой и не отрезана: %v", opts)
	}
	// 16. Одинаковые ФИО различимы.
	q, _ := cmfAskText("Мусиева Марха", 5000, []cmf.ClientInfo{{ID: "11", FullName: "Мусиева Марха Ахмедовна", Phone: "79280000011"}, {ID: "13", FullName: "Мусиева Марха Ахмедовна", Phone: "79280000013"}})
	if !strings.Contains(q, "…0011") || !strings.Contains(q, "…0013") || !strings.Contains(q, "карточки") {
		t.Errorf("#16: %q", q)
	}
	// Строгое покрытие имени.
	for _, c := range [][2]string{{"Магомед", "Магомедова Патимат Ибрагимовна"}, {"Альмурзаева М.", "Альмурзаева Разет Ахмедовна"}, {"Магомедова Патимат", "Алиева Патимат Магомедовна"}, {"Исаева Марха", "Мусаева Марха Ахмедовна"}} {
		if nameCovers(c[0], c[1]) {
			t.Errorf("nameCovers(%q,%q)=true", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"Альмурзаева Разет", "Альмурзаева Разет Ахмедовна"}, {"Каталов А.", "Каталов Ахмед Нажудович"}, {"Котолов Ахмед", "Каталов Ахмед Нажудович"}, {"Хаджимурат Магомедов", "Магомедов Хаджи-Мурат Ахмедович"}} {
		if !nameCovers(c[0], c[1]) {
			t.Errorf("nameCovers(%q,%q)=false", c[0], c[1])
		}
	}
}

func TestCmfAnswerR1Payer(t *testing.T) {
	f := newFlowBot(t, nil)
	ctx := context.Background()
	// 18. Одно слово — не плательщик.
	if f.rememberPayer(ctx, "Марха", cmf.ClientInfo{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"}) {
		t.Error("#18: запомнил одно слово")
	}
	if _, ok := f.payerClient(ctx, "Марха"); ok {
		t.Error("#18: одно слово нашлось")
	}
	// 19. Два подходящих плательщика на разных клиентов — не угадываем.
	f.rememberPayer(ctx, "Альмурзаева Марха А", cmf.ClientInfo{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"})
	f.rememberPayer(ctx, "Альмурзаева Марха И", cmf.ClientInfo{ID: "78", FullName: "Альмурзаева Хава Ахмедовна"})
	for i := 0; i < 20; i++ {
		if _, ok := f.payerClient(ctx, "Альмурзаева Марха"); ok {
			t.Fatal("#19: неоднозначная память дала ответ")
		}
	}
	if c, ok := f.payerClient(ctx, "Альмурзаева Марха А"); !ok || c.ID != "77" {
		t.Errorf("#19: точный ключ: %+v %v", c, ok)
	}
	// 20. Дефис: «Магомед-Расул» — не «Магомед».
	f.rememberPayer(ctx, "Альмурзаев Магомед-Расул", cmf.ClientInfo{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"})
	for _, q := range []string{"Альмурзаев Магомед", "Альмурзаев Расул"} {
		if _, ok := f.payerClient(ctx, q); ok {
			t.Errorf("#20 %q", q)
		}
	}
}

// flowAsk — завести чек и вопрос по нему; возвращает id наблюдения, id чека и вопрос.
func flowAsk(t *testing.T, f *flowBot, grp types.JID, caption string, amount float64) (int, string, sent) {
	t.Helper()
	ctx := context.Background()
	waID := fmt.Sprintf("CHK-%d", time.Now().UnixNano())
	raw, _, err := f.db.SaveRawMessage(ctx, waID, grp.String(), "emp@s.whatsapp.net", "79990000000", "Сафаи", "чек", true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.cmfWatchReceipt(ctx, grp, "emp@s.whatsapp.net", caption, amount, time.Now(), raw)
	ask := f.last()
	wid, _ := f.cmfWatchByAsk(ctx, ask.id)
	return wid, waID, ask
}

func TestCmfAnswerR1Flow(t *testing.T) {
	f := newFlowBot(t, []cmf.ClientInfo{
		{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
		{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
		{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"},
		{ID: "88", FullName: "Магомедова Патимат Ибрагимовна"},
		{ID: "66", FullName: "Абдусаламова Зарема Ибрагимовна"},
	})
	ctx := context.Background()
	grp := types.NewJID(fmt.Sprintf("r1g%d", time.Now().UnixNano()), types.GroupServer)

	// 3. Без ИИ неясный ответ («Салам») — ничего не привязываем.
	wid, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха А", 19000)
	if wid == 0 {
		t.Fatal("нет вопроса")
	}
	f.applyCmfWatchAnswer(ctx, grp, wid, "Салам", modeSwipe, false)
	if w, _, _ := f.db.CmfWatchByID(ctx, wid); w.ClientID != "" {
		t.Errorf("#3: привязал к %s", w.ClientName)
	}
	// 8. «Магомед» — не «Магомедова Патимат».
	f.applyCmfWatchAnswer(ctx, grp, wid, "Магомед", modeSwipe, false)
	if w, _, _ := f.db.CmfWatchByID(ctx, wid); w.ClientID == "88" {
		t.Error("#8: Магомед → Магомедова")
	}
	// 9/23. Свайп на переспрос — тоже ответ по этому чеку.
	f.applyCmfWatchAnswer(ctx, grp, wid, "Хамзатова Луиза", modeSwipe, false)
	nf := f.last()
	if !strings.Contains(nf.text, "не нашёл") {
		t.Fatalf("ожидали «не нашёл»: %q", nf.text)
	}
	wid2, ok := f.cmfWatchByAsk(ctx, nf.id)
	if !ok || wid2 != wid {
		t.Fatalf("#9: переспрос не связан с чеком (%d %v)", wid2, ok)
	}
	// …и сразу после «ответьте ещё раз» ФИО без свайпа тоже понимается.
	if !f.tryContextAnswer(ctx, grp, "Альмурзаева Разет") {
		t.Fatal("#23: ФИО без свайпа после переспроса не понято")
	}
	if w, _, _ := f.db.CmfWatchByID(ctx, wid); w.ClientID != "77" {
		t.Errorf("#23: %+v", w)
	}

	// 6. Новый платёж без свайпа не принимается за «нет в программе».
	wid3, _, _ := flowAsk(t, f, grp, "Альмурзаева Хеда", 7000)
	for _, s := range []string{"Новый клиент Хасанов Ислам 5000", "его нет на месте, завтра скинет", "нет такого правила"} {
		if f.tryContextAnswer(ctx, grp, s) {
			t.Errorf("#6 %q перехвачено", s)
		}
	}
	if w, _, _ := f.db.CmfWatchByID(ctx, wid3); w.Status == "unmatched" {
		t.Error("#6: чек закрыт")
	}

	// 10. Два вопроса: ответ на один не трогает другой.
	widA, _, askA := flowAsk(t, f, grp, "Марха", 3000)
	widB, _, askB := flowAsk(t, f, grp, "Марха Висаевна", 4000)
	if widB == 0 {
		t.Fatalf("#10: второй вопрос не задан: %q", askB.text)
	}
	_ = askA
	f.applyCmfWatchAnswer(ctx, grp, widA, "1", modeSwipe, false)
	if a, ok := f.recentOpenAsk(grp.String()); !ok || a.watchID != widB {
		t.Errorf("#10: ответ на первый вопрос сбросил второй: %+v %v", a, ok)
	}
	// 7. Одно слово «Марха» не запоминается как плательщик.
	if _, ok := f.payerClient(ctx, "Марха"); ok {
		t.Error("#7: «Марха» запомнена")
	}
	_ = widB

	// 21. Исправление свайпом на сам чек переключает сверку.
	widC, waC, _ := flowAsk(t, f, grp, "Альмурзаева Марха А", 12000) // по памяти → Разет
	if w, _, _ := f.db.CmfWatchByID(ctx, widC); w.ClientID != "77" {
		t.Fatalf("#21: память плательщика не сработала: %+v", w)
	}
	if !strings.Contains(f.last().text, "как в прошлый раз") {
		t.Errorf("#21: нет короткой пометки: %q", f.last().text)
	}
	if !f.reresolveWatchByCheck(ctx, grp, waC, "Мусиева Марха Ахмедовна") {
		t.Fatal("#21: наблюдение по чеку не найдено")
	}
	time.Sleep(300 * time.Millisecond)
	if w, _, _ := f.db.CmfWatchByID(ctx, widC); w.ClientID != "11" {
		t.Errorf("#21: сверка не переключилась: %+v", w)
	}
	if _, ok := f.payerClient(ctx, "Альмурзаева Марха А"); ok {
		t.Error("#21: ошибочная связь плательщика не забыта")
	}
}
