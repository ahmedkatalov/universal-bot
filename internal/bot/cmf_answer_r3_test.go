package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

// Регрессии третьего раунда (только серьёзное: неверная привязка, потеря,
// перехват сообщения, ответ в чат вместо понимания).

func TestCmfAnswerR3Parse(t *testing.T) {
	// 2. «Мусаева Марха» — НЕ вариант «Мусиева Марха».
	cands := []cmf.ClientInfo{{ID: "13", FullName: "Абаева Марха Исаевна"}, {ID: "11", FullName: "Мусиева Марха Ахмедовна"}}
	if a, _ := parseCmfAnswer("Мусаева Марха", capIncident, cands); a.Kind == ansPick {
		t.Errorf("#2: %+v", a)
	}
	p := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: "Дудаева Седа"}, nil, func(string) ([]cmf.ClientInfo, cmfMatchKind, error) {
		return []cmf.ClientInfo{{ID: "21", FullName: "Дадаева Седа Ахмедовна"}}, cmfWeak, nil
	})
	if p.Action == actBind {
		t.Errorf("#2 Дудаева→Дадаева: %+v", p)
	}
	// 3. Только имя («Хаджи Мурат», «Марха А.») — не привязываем без вопроса.
	for _, name := range []string{"Хаджи Мурат", "Марха А.", "Разет А."} {
		p := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: name}, nil, func(string) ([]cmf.ClientInfo, cmfMatchKind, error) {
			return []cmf.ClientInfo{{ID: "1", FullName: "Магомедов Хаджи-Мурат Ахмедович"}, {ID: "2", FullName: "Мусиева Марха Ахмедовна"}, {ID: "3", FullName: "Хасанова Разет Ахмедовна"}}, cmfWeak, nil
		})
		if p.Action == actBind {
			t.Errorf("#3 %q: %+v", name, p)
		}
	}
	for _, s := range []string{"Хаджи Мурат", "Жена Магомеда", "Мама Ислама", "Сестра Мадины"} {
		if payerEligible(s) {
			t.Errorf("#3/#14 payerEligible(%q)", s)
		}
	}
	// 4. Родство рядом с ФИО — не уверенное «ФИО» с родством внутри.
	for _, s := range []string{"Её дочь Альмурзаева Разет", "это мама Альмурзаевой Разет", "Альмурзаева Разет, сестра", "старший брат"} {
		if a, ok := parseCmfAnswer(s, capIncident, screenshotCands); ok && a.Kind == ansClient {
			t.Errorf("#4 %q: %+v", s, a)
		}
	}
	if a, ok := parseCmfAnswer("Марха за Альмурзаеву Разет", capIncident, screenshotCands); a.Kind != ansClient || !ok || strings.Contains(strings.ToLower(a.Name), "марха") {
		t.Errorf("#4 «X за Y»: %+v %v", a, ok)
	}
	// 5. «никто из них» — не «нет в программе».
	for _, s := range []string{"никто из них", "никто", "ни одна из них"} {
		if a, _ := parseCmfAnswer(s, capIncident, screenshotCands); a.Kind == ansNotInProgram {
			t.Errorf("#5 %q", s)
		}
	}
	// 12. Магомед ≠ Магомед-Расул, Мурат ≠ Хаджи-Мурат.
	if nameCovers("Исаев Магомед", "Исаев Магомед-Расул Ахмедович") {
		t.Error("#12 Магомед/Магомед-Расул")
	}
	if k := coveringUnique("Мурат Исаев", []cmf.ClientInfo{{ID: "1", FullName: "Исаев Хаджи-Мурат Ахмедович"}}); k >= 0 {
		t.Error("#12 Мурат/Хаджи-Мурат")
	}
	if !nameCovers("Хаджи Мурат Магомедов", "Магомедов Хаджимурат") || !nameCovers("Хаджимурат Магомедов", "Магомедов Хаджи-Мурат Ахмедович") {
		t.Error("#12: написания одного составного имени")
	}
	// 11. Совпало только отчество — не вариант и не вытесняет тёзок.
	var many []cmf.ClientInfo
	for i, sur := range []string{"Абаева", "Алиева", "Батаева", "Бисаева", "Гаева", "Дадаева", "Ериева"} {
		many = append(many, cmf.ClientInfo{ID: fmt.Sprint("p", i), FullName: sur + " Хава Ахмедовна"})
	}
	many = append(many, cmf.ClientInfo{ID: "11", FullName: "Мусиева Марха Ахмедовна"}, cmf.ClientInfo{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"})
	q, opts := cmfAskText("Альмурзаева Марха Ахмедовна", 19000, many)
	if strings.Contains(q, "Хава") || len(opts) != 2 {
		t.Errorf("#11: %q", q)
	}
}

func TestCmfAnswerR3Flow(t *testing.T) {
	f := newFlowBot(t, []cmf.ClientInfo{
		{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
		{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
		{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"},
		{ID: "78", FullName: "Альмурзаева Хава Ахмедовна"},
	})
	ctx := context.Background()
	grp := types.NewJID(fmt.Sprintf("r3g%d", time.Now().UnixNano()), types.GroupServer)
	watch := func(id int) db.CmfWatchFull {
		w, _, _ := f.db.CmfWatchByID(ctx, id)
		return w
	}

	// 1/6. «да» свайпом на вопрос с одним вариантом — привязка.
	f2 := newFlowBot(t, []cmf.ClientInfo{{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"}})
	g2 := types.NewJID(fmt.Sprintf("r3y%d", time.Now().UnixNano()), types.GroupServer)
	wid, _, ask := flowAsk(t, f2, g2, "Альмурзаева Хеда", 5000)
	if !strings.Contains(ask.text, "«да»") {
		t.Fatalf("ожидали вопрос с «да»: %q", ask.text)
	}
	f2.applyCmfWatchAnswer(ctx, g2, wid, "да", modeSwipe, false)
	if w, _, _ := f2.db.CmfWatchByID(ctx, wid); w.ClientID != "77" || !strings.HasPrefix(f2.last().text, "✅") {
		t.Errorf("#1: %+v / %q", w.CmfWatch, f2.last().text)
	}

	f = newFlowBot(t, []cmf.ClientInfo{
		{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
		{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
		{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"},
		{ID: "78", FullName: "Альмурзаева Хава Ахмедовна"},
	})
	// 5. «никто из них» — спрашиваем ФИО, чек открыт.
	wid, _, _ = flowAsk(t, f, grp, capIncident, 19000)
	f.applyCmfWatchAnswer(ctx, grp, wid, "никто из них", modeSwipe, false)
	if w := watch(wid); w.Status == "not_in_program" || w.Status == "unmatched" || !strings.Contains(f.last().text, "За кого тогда") {
		t.Errorf("#5: %s / %q", w.Status, f.last().text)
	}
	// 9. «её нет» / «нету» без свайпа — не закрывают чек; «нет в программе» — закрывает.
	for _, s := range []string{"её нет", "нету", "никто", "его нет"} {
		if f.tryContextAnswer(ctx, grp, s) {
			t.Errorf("#9 %q перехвачено", s)
		}
	}
	if !f.tryContextAnswer(ctx, grp, "нет в программе") || watch(wid).Status != "not_in_program" {
		t.Errorf("#9: явное «нет в программе» не закрыло чек: %s", watch(wid).Status)
	}

	// 8. Номер — из того списка, что был в сообщении, на которое ответили.
	widB, _, askB := flowAsk(t, f, grp, capIncident, 7000)
	var origOpts []cmf.ClientInfo
	_ = json.Unmarshal([]byte(watch(widB).Candidates), &origOpts)
	f.applyCmfWatchAnswer(ctx, grp, widB, "3", modeSwipe, false) // ошибочно
	mis := origOpts[2].ID
	if watch(widB).ClientID != mis {
		t.Fatalf("#8: выбор 3: %+v", watch(widB).CmfWatch)
	}
	// Поправка «нет, 1» свайпом на исходный вопрос.
	refs, _ := f.db.CmfWatchesByAsk(ctx, askB.id)
	if len(refs) != 1 {
		t.Fatalf("#8: вопрос не связан: %v", refs)
	}
	var o []cmf.ClientInfo
	_ = json.Unmarshal([]byte(refs[0].Options), &o)
	f.applyCmfWatchAnswerOpts(ctx, grp, widB, "нет, 1", answerOpts{mode: modeSwipe, askOptions: &o})
	if watch(widB).ClientID != origOpts[0].ID {
		t.Errorf("#8: поправка номером не применилась: %+v", watch(widB).CmfWatch)
	}

	// 15. «нет это за Альмурзаеву Хаву» на «как в прошлый раз» — перепривязка.
	f.SettingReset(ctx)
	widC, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха Б", 4000)
	f.applyCmfWatchAnswer(ctx, grp, widC, "Альмурзаева Разет", modeSwipe, false)
	widD, _, note := flowAsk(t, f, grp, "Альмурзаева Марха Б", 8000)
	if !strings.Contains(note.text, "как в прошлый раз") {
		t.Fatalf("#15: ожидали «как в прошлый раз»: %q", note.text)
	}
	f.applyCmfWatchAnswer(ctx, grp, widD, "нет это за Альмурзаеву Хаву", modeSwipe, false)
	if watch(widD).ClientID != "78" {
		t.Errorf("#15: %+v / %q", watch(widD).CmfWatch, f.last().text)
	}
	if _, ok := f.payerClient(ctx, "Альмурзаева Марха Б"); ok {
		t.Error("#15: плательщик не помечен «платит за разных»")
	}

	// 13. После явного «нет в программе» плательщик не запоминается как чей-то.
	widE, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха В", 5000)
	f.applyCmfWatchAnswer(ctx, grp, widE, "нет в программе", modeSwipe, false)
	widF, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха В", 19000)
	f.applyCmfWatchAnswer(ctx, grp, widF, "Альмурзаева Разет", modeSwipe, false)
	if strings.Contains(f.last().text, "Запомнил") {
		t.Errorf("#13: запомнил плательщика после «нет в программе»: %q", f.last().text)
	}

	// 7. Имя после НОВОГО чека — для нового чека, не ответ на старый вопрос.
	widG, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха Г", 6000)
	_ = widG
	sender := types.NewJID("emp", types.DefaultUserServer)
	raw, _, _ := f.db.SaveRawMessage(ctx, fmt.Sprintf("NEWCHK-%d", time.Now().UnixNano()), grp.String(), sender.String(), "7999", "Сафаи", "чек", true, "", time.Now())
	_ = f.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, Amount: 3000, NeedsReview: true, GroupJID: grp.String(), TxDate: time.Now()})
	m := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true}, Timestamp: time.Now()}}
	if !f.contextAnswerBlocked(ctx, m) {
		t.Error("#7: имя после нового чека перехватывается старым вопросом")
	}

	// 17/10. cmf_resolve: чужой message_id — ошибка, не «последний неясный»; без id — по сумме.
	widH, _, _ := flowAsk(t, f, grp, "Альмурзаева Марха Д", 19000)
	widI, _, _ := flowAsk(t, f, grp, "Хамзатова Луиза", 4000)
	tool := f.cmfResolveTool(grp)
	if _, err := tool.Handle(ctx, json.RawMessage(`{"message_id":"SOME-REMINDER","client_name":"Альмурзаева Разет Ахмедовна"}`)); err == nil {
		t.Error("#17: неизвестный message_id привязал какой-то чек")
	}
	out, err := tool.Handle(ctx, json.RawMessage(`{"amount":19000,"client_name":"Альмурзаева Разет Ахмедовна"}`))
	if err != nil || watch(widH).ClientID != "77" || watch(widI).ClientID != "" {
		t.Errorf("#10: %q %v H=%s I=%s", out, err, watch(widH).ClientID, watch(widI).ClientID)
	}

	// 18. Поправка свайпом на напоминание — по этому чеку, не в чат.
	widJ, _, _ := flowAsk(t, f, grp, "Сайдаева Хеда", 2000)
	f.applyCmfWatchAnswer(ctx, grp, widJ, "1", modeSwipe, false)
	w := watch(widJ)
	if w.ClientID == "" {
		t.Fatalf("#18: не привязан")
	}
	f.cmfSay(ctx, grp, w, "⏰ Занесите, пожалуйста, оплату в программу… — клиент "+w.ClientName)
	if refs, _ := f.db.CmfWatchesByAsk(ctx, f.last().id); len(refs) != 1 || refs[0].WatchID != widJ {
		t.Errorf("#18: напоминание не связано с чеком: %v", refs)
	}
}

// SettingReset — сбросить память о плательщиках (для теста).
func (f *flowBot) SettingReset(ctx context.Context) { _ = f.db.SettingSet(ctx, settingPayerMap, "") }

func TestCmfAnswerR3ClarifyFollowUp(t *testing.T) {
	// 16. Переспрос «А чей это чек?» связан с чеком — свайп на него не уходит в чат.
	f := newFlowBot(t, nil)
	ctx := context.Background()
	grp := types.NewJID(fmt.Sprintf("r3c%d", time.Now().UnixNano()), types.GroupServer)
	f.askAboutReceipt(ctx, grp, "CHECK-X", "Поправил сумму. А чей это чек? Напишите ФИО клиента — тогда засчитаю.", false, true)
	id := f.last().id
	f.clarify.mu.Lock()
	got := f.clarify.askMap[id]
	f.clarify.mu.Unlock()
	if got != "CHECK-X" {
		t.Errorf("#16: переспрос не связан с чеком: %q", got)
	}
}
