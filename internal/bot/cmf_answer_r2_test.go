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

// Регрессии второго раунда враждебной проверки ответа «за кого этот платёж?».

func TestCmfAnswerR2Parse(t *testing.T) {
	one := []cmf.ClientInfo{{ID: "11", FullName: "Мусиева Марха Ахмедовна"}}
	// 1. Повтор имени плательщика с «это/за» — не выбор.
	for _, s := range []string{"за Марху", "это Марха", "Это Марха", "За Марху"} {
		if a, _ := parseCmfAnswer(s, capIncident, one); a.Kind == ansPick {
			t.Errorf("#1 %q: %+v", s, a)
		}
	}
	// 2. Составное имя через дефис — одно слово, не ФИО.
	for _, s := range []string{"Хаджи-Мурат", "это Хаджи-Мурат", "Магомед-Расул"} {
		if a, ok := parseCmfAnswer(s, capIncident, screenshotCands); ok && a.Kind == ansClient {
			t.Errorf("#2 %q: уверенно ФИО", s)
		}
	}
	p := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: "Хаджи-Мурат"}, nil, func(string) ([]cmf.ClientInfo, cmfMatchKind, error) {
		return []cmf.ClientInfo{{ID: "55", FullName: "Магомедов Хаджи-Мурат Ахмедович"}}, cmfWeak, nil
	})
	if p.Action == actBind {
		t.Errorf("#2 план: %+v", p)
	}
	// 3. «не знаю X, это за Y» — не «не знаю».
	for _, s := range []string{"не знаю их, это за Альмурзаеву Разет", "Мусиеву не знаю, платёж за Альмурзаеву Разет"} {
		if a, _ := parseCmfAnswer(s, capIncident, screenshotCands); a.Kind == ansUnknown {
			t.Errorf("#3 %q", s)
		}
	}
	if a, ok := parseCmfAnswer("не знаю", capIncident, screenshotCands); a.Kind != ansUnknown || !ok {
		t.Errorf("#3 «не знаю»: %+v", a)
	}
	// 4. Приветствия, «подождите», чеченские фразы — не ФИО.
	for _, s := range []string{"Дела реза хуьлда", "Баркалла хьуна", "Добрый день", "Подождите пожалуйста", "Дай минуту", "хьо мила ву", "она есть в программе", "да но фамилия другая"} {
		if a, ok := parseCmfAnswer(s, capIncident, screenshotCands); ok && (a.Kind == ansClient || a.Kind == ansPick) {
			t.Errorf("#4 %q: %+v", s, a)
		}
	}
	// 5–6. Номер с эмодзи, в скобках, словами.
	for s, want := range map[string]int{"2🙏": 1, "2 🙏": 1, "2 👍🏻": 1, "2👍🏽": 1, "(2)": 1, "номер два": 1, "номер второй": 1, "под номером 1": 0, "2 вариант": 1} {
		if a, ok := parseCmfAnswer(s, capIncident, screenshotCands); a.Kind != ansPick || a.Pick != want || !ok {
			t.Errorf("#5/6 %q: %+v %v", s, a, ok)
		}
	}
	// 9. Обращение к коллеге — не ФИО.
	for _, s := range []string{"Мадина, посмотри", "спроси Мадину", "Мадина знает", "Мадина глянь"} {
		if a, ok := parseCmfAnswer(s, capIncident, screenshotCands); ok && a.Kind == ansClient {
			t.Errorf("#9 %q: %+v", s, a)
		}
	}
	// Инцидент по-прежнему понимается.
	if a, ok := parseCmfAnswer("Альмурзаева Разет", capIncident, screenshotCands); a.Kind != ansClient || !ok {
		t.Errorf("инцидент: %+v %v", a, ok)
	}
	if a, ok := parseCmfAnswer("за Альмурзаеву Разет", capIncident, screenshotCands); a.Kind == ansPick || (a.Kind == ansOther && ok) {
		t.Errorf("«за Альмурзаеву Разет»: %+v %v", a, ok)
	}
}

func TestCmfAnswerR2Wording(t *testing.T) {
	// 13. Совпало только отчество — это не варианты.
	q, _ := cmfAskText("Ахмедова Зарема", 4000, []cmf.ClientInfo{{ID: "a1", FullName: "Абаева Хава Ахмедовна"}, {ID: "a2", FullName: "Дудаева Седа Ахмедовна"}, {ID: "a6", FullName: "Исаева Зарема Магомедовна"}})
	if strings.Contains(q, "Абаева") || strings.Contains(q, "Дудаева") || !strings.Contains(q, "Исаева Зарема") {
		t.Errorf("#13: %q", q)
	}
	q, _ = cmfAskText("Исаева Марха", 6000, []cmf.ClientInfo{{ID: "1", FullName: "Дудаева Зарема Исаевна"}, {ID: "2", FullName: "Мусаева Хава Исаевна"}})
	if !strings.Contains(q, "не нашёл") || strings.Contains(q, "«да»") {
		t.Errorf("#13b: %q", q)
	}
	// 15. Есть точные совпадения — не «точно такого нет».
	q, _ = cmfAskText("Альмурзаева Марха", 19000, []cmf.ClientInfo{{ID: "1", FullName: "Альмурзаева Марха Ахмедовна"}, {ID: "2", FullName: "Альмурзаева Марха Исаевна"}, {ID: "3", FullName: "Альмурзаева Разет Ахмедовна"}})
	if strings.Contains(q, "такого клиента в программе нет") || !strings.Contains(q, "(та же фамилия)") {
		t.Errorf("#15: %q", q)
	}
	// 14. «Марха А.» — имя + буква: по имени и букве не угадываем.
	if k := coveringUnique("Марха А.", screenshotCands); k >= 0 {
		t.Errorf("#14: %d", k)
	}
	if k := coveringUnique("Каталов А.", []cmf.ClientInfo{{ID: "1", FullName: "Каталов Ахмед Нажудович"}}); k != 0 {
		t.Errorf("#14 Каталов А.: %d", k)
	}
	// 21. Фраза и составное имя — не плательщик.
	for _, s := range []string{"Первый взнос", "Досрочное погашение", "Доброе утро", "Хаджи-Мурат", "Магомед-Расул"} {
		if payerEligible(s) {
			t.Errorf("#21 %q", s)
		}
	}
	if !payerEligible(capIncident) {
		t.Error("#21: инцидентный плательщик")
	}
	// 26. Метка молчания в любом виде — ничего не отправляем.
	for _, s := range []string{"[молчу]\n\n(это отмашка на чеченском, отвечать не нужно)", "[молчу] 🤐", "«[молчу]»", "*[молчу]*", "`[молчу]`", "[МОЛЧУ]", "(молчу)", "🤐"} {
		if out := assistantOutgoing(s); out != "" {
			t.Errorf("#26 %q → %q", s, out)
		}
	}
	if out := assistantOutgoing("Записал: чек — клиент Ахмед."); out == "" {
		t.Error("#26: обычный ответ съеден")
	}
}

func TestCmfAnswerR2Flow(t *testing.T) {
	f := newFlowBot(t, []cmf.ClientInfo{
		{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
		{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
		{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"},
		{ID: "78", FullName: "Альмурзаева Хава Ахмедовна"},
		{ID: "88", FullName: "Магомедова Патимат Ибрагимовна"},
	})
	ctx := context.Background()
	grp := types.NewJID(fmt.Sprintf("r2g%d", time.Now().UnixNano()), types.GroupServer)
	watch := func(id int) (string, string) {
		w, _, _ := f.db.CmfWatchByID(ctx, id)
		return w.ClientID, w.Status
	}

	// 7/18/23. «Спасибо/👍/ок/да» на «✅ Понял» — молча, решение не меняется.
	wid, _, _ := flowAsk(t, f, grp, capIncident, 19000)
	f.applyCmfWatchAnswer(ctx, grp, wid, "Альмурзаева Разет", modeSwipe, false)
	if id, _ := watch(wid); id != "77" {
		t.Fatalf("привязка: %s", id)
	}
	for _, ack := range []string{"👍", "+", "спасибо", "ок", "да", "верно", "Баркалла"} {
		n := len(f.out)
		if !f.applyCmfWatchAnswer(ctx, grp, wid, ack, modeSwipe, false) {
			t.Errorf("#7 %q: не поглощено", ack)
		}
		if len(f.out) != n {
			t.Errorf("#7 %q: бот ответил %q", ack, f.last().text)
		}
	}
	if f.tryContextAnswer(ctx, grp, "Магомедова Патимат") {
		t.Error("#7: имя без свайпа перепривязало решённый чек")
	}
	if id, _ := watch(wid); id != "77" {
		t.Errorf("#7: клиент сменился на %s", id)
	}
	if c, ok := f.payerClient(ctx, capIncident); !ok || c.ID != "77" {
		t.Error("#7: память о плательщике стёрта")
	}

	// 19. «нет» на «как в прошлый раз» — снимаем привязку и спрашиваем ФИО.
	wid2, _, note := flowAsk(t, f, grp, capIncident, 8000)
	if !strings.Contains(note.text, "как в прошлый раз") {
		t.Fatalf("ожидали «как в прошлый раз»: %q", note.text)
	}
	f.applyCmfWatchAnswer(ctx, grp, wid2, "не Разет", modeSwipe, false)
	if id, st := watch(wid2); id != "" || st != "ambiguous" {
		t.Errorf("#19: привязка не снята: %s %s", id, st)
	}
	if !strings.Contains(f.last().text, "За кого тогда") {
		t.Errorf("#19: %q", f.last().text)
	}
	// 20. После отказа плательщик помечен «платит за разных»: больше не угадываем и не запоминаем.
	f.applyCmfWatchAnswer(ctx, grp, wid2, "Альмурзаева Хава", modeSwipe, false)
	if id, _ := watch(wid2); id != "78" {
		t.Errorf("#19: новая привязка %s", id)
	}
	if strings.Contains(f.last().text, "Запомнил") {
		t.Errorf("#20: снова «Запомнил»: %q", f.last().text)
	}
	wid3, _, ask3 := flowAsk(t, f, grp, capIncident, 9000)
	if strings.Contains(ask3.text, "как в прошлый раз") {
		t.Errorf("#20: снова угадывает: %q", ask3.text)
	}
	f.applyCmfWatchAnswer(ctx, grp, wid3, "Альмурзаева Разет", modeSwipe, false)
	if strings.Contains(f.last().text, "Запомнил") {
		t.Errorf("#20: снова «Запомнил» после конфликта: %q", f.last().text)
	}

	// 11. Удалённый чек — ответ ничего не привязывает.
	wid4, wa4, _ := flowAsk(t, f, grp, "Альмурзаева Марха Б", 5000)
	if _, _, err := f.db.MarkMessageDeleted(ctx, wa4); err != nil {
		t.Fatal(err)
	}
	f.applyCmfWatchAnswer(ctx, grp, wid4, "Альмурзаева Хава", modeSwipe, false)
	if id, _ := watch(wid4); id != "" {
		t.Errorf("#11: удалённый чек привязан к %s", id)
	}
	due, _ := f.db.DueCmfWatches(ctx, time.Now().Add(time.Hour), 500)
	for _, d := range due {
		if d.ID == wid4 {
			t.Error("#11: удалённый чек в напоминаниях")
		}
	}

	// 17. Новый вопрос без списка: «1» — не выбор из старого списка.
	wid5, wa5, _ := flowAsk(t, f, grp, "Марха Висаевна", 3000)
	f.reresolveWatchByCheck(ctx, grp, wa5, "Хамзатова Луиза")
	time.Sleep(300 * time.Millisecond)
	f.applyCmfWatchAnswer(ctx, grp, wid5, "1", modeSwipe, false)
	if id, _ := watch(wid5); id != "" {
		t.Errorf("#17: «1» привязал к %s", id)
	}

	// 10. Свайп с ФИО на сам чек закрывает вопрос и запоминает настоящего плательщика.
	wid6, wa6, _ := flowAsk(t, f, grp, "Сайдаева Хеда Висаевна", 4000)
	f.reresolveWatchByCheck(ctx, grp, wa6, "Мусиева Марха Ахмедовна")
	time.Sleep(300 * time.Millisecond)
	if id, _ := watch(wid6); id != "11" {
		t.Errorf("#10: %s", id)
	}
	if a, ok := f.recentOpenAsk(grp.String()); ok && a.watchID == wid6 {
		t.Error("#10: вопрос остался открытым")
	}
	w6, _, _ := f.db.CmfWatchByID(ctx, wid6)
	if w6.ClientText != "Сайдаева Хеда Висаевна" {
		t.Errorf("#10: подпись-плательщик затёрта: %q", w6.ClientText)
	}
}

func TestCmfAnswerR2Relative(t *testing.T) {
	// 12. Родственница с той же фамилией не теряется за десятком тёзок.
	var prog []cmf.ClientInfo
	for i, s := range []string{"Абаева", "Батаева", "Гиреева", "Дудаева", "Исаева", "Кадырова", "Мусиева", "Сайдаева", "Темирова", "Умарова", "Цакаева"} {
		prog = append(prog, cmf.ClientInfo{ID: fmt.Sprintf("m%d", i), FullName: s + " Марха Исаевна"})
	}
	prog = append(prog, cmf.ClientInfo{ID: "rel", FullName: "Хасанова Разет Ахмедовна"})
	f := newFlowBot(t, prog)
	grp := types.NewJID(fmt.Sprintf("r2rel%d", time.Now().UnixNano()), types.GroupServer)
	_, _, ask := flowAsk(t, f, grp, "Хасанова Марха А", 19000)
	if !strings.Contains(ask.text, "1. Хасанова Разет Ахмедовна (та же фамилия)") {
		t.Errorf("#12: %q", ask.text)
	}
	// 8. «Джарвис, это Альмурзаева Разет» без свайпа сразу после вопроса.
	f2 := newFlowBot(t, []cmf.ClientInfo{{ID: "11", FullName: "Мусиева Марха Ахмедовна"}, {ID: "77", FullName: "Альмурзаева Разет Ахмедовна"}})
	grp2 := types.NewJID(fmt.Sprintf("r2adr%d", time.Now().UnixNano()), types.GroupServer)
	wid, _, _ := flowAsk(t, f2, grp2, "Альмурзаева Марха А", 19000)
	a, ok := f2.recentOpenAsk(grp2.String())
	if !ok || a.watchID != wid {
		t.Fatal("#8: нет открытого вопроса")
	}
	if !f2.applyCmfWatchAnswerOpts(context.Background(), grp2, a.watchID, "это Альмурзаева Разет", answerOpts{mode: modeAddressed}) {
		t.Fatal("#8: обращение по имени не понято")
	}
	if w, _, _ := f2.db.CmfWatchByID(context.Background(), wid); w.ClientID != "77" {
		t.Errorf("#8: %+v", w)
	}
}
