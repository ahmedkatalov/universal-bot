package bot

import (
	"errors"
	"strings"
	"testing"

	"whatsapp-bot/internal/cmf"
)

var screenshotCands = []cmf.ClientInfo{
	{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
	{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
}

// Сценарий со скриншота 03.10: чек «Альмурзаева Марха А», в программе только
// тёзки по имени; сотрудник отвечает «Альмурзаева Разет» — это клиент, за
// которого платили, а не её имя.
func TestCmfAnswerScreenshot(t *testing.T) {
	q, opts := cmfAskText("Альмурзаева Марха А", 19000, screenshotCands)
	if !strings.Contains(q, "совпадает только имя") || !strings.Contains(q, "1. ") || !strings.Contains(q, "номером или ФИО") {
		t.Errorf("вопрос: %q", q)
	}
	if strings.Contains(q, "нашлось несколько клиентов") {
		t.Errorf("тёзки по имени не должны выдаваться за найденных клиентов: %q", q)
	}
	if len(opts) != 2 {
		t.Fatalf("вариантов %d", len(opts))
	}
	ans, ok := parseCmfAnswer("Альмурзаева Разет", "Альмурзаева Марха А", opts)
	if !ok || ans.Kind != ansClient || ans.Name != "Альмурзаева Разет" {
		t.Fatalf("разбор ответа: %+v ok=%v", ans, ok)
	}
	razet := cmf.ClientInfo{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"}
	plan := planCmfAnswer(ans, opts, func(name string) ([]cmf.ClientInfo, cmfMatchKind, error) {
		if name != "Альмурзаева Разет" {
			t.Errorf("искали %q", name)
		}
		return []cmf.ClientInfo{razet}, cmfStrong, nil
	})
	if plan.Action != actBind || plan.Client.ID != "77" {
		t.Errorf("план: %+v", plan)
	}
}

func TestParseCmfAnswer(t *testing.T) {
	cases := []struct {
		in        string
		kind      cmfAnswerKind
		pick      int
		confident bool
	}{
		{"2", ansPick, 1, true},
		{"№1", ansPick, 0, true},
		{"второй", ansPick, 1, true},
		{"Вторая.", ansPick, 1, true},
		{"Сайдаева", ansPick, 1, true},
		{"сайдаевой", ansPick, 1, true},
		{"Мусиева Марха", ansPick, 0, true},
		{"альмурзаева разет", ansClient, 0, true},
		{"нет в программе", ansNotInProgram, 0, true},
		{"его нет в программе", ansNotInProgram, 0, true},
		{"новый клиент", ansNotInProgram, 0, true},
		{"не знаю", ansUnknown, 0, true},
		{"хз", ansUnknown, 0, true},
		{"5", ansOther, 0, true},
		{"19000", ansOther, 0, true},
		{"Д1авала", ansOther, 0, false},
		{"а кто это?", ansOther, 0, false},
		{"Альмурзаева Разет 19000", ansOther, 0, false},
	}
	for _, c := range cases {
		a, ok := parseCmfAnswer(c.in, "Альмурзаева Марха А", screenshotCands)
		if a.Kind != c.kind || ok != c.confident || (c.kind == ansPick && a.Pick != c.pick) {
			t.Errorf("parseCmfAnswer(%q) = %+v ok=%v, ожидали kind=%v pick=%d ok=%v", c.in, a, ok, c.kind, c.pick, c.confident)
		}
	}
	one := screenshotCands[:1]
	for in, want := range map[string]cmfAnswerKind{"да": ansPick, "Да, она": ansPick, "нет": ansReject, "не она": ansReject} {
		if a, _ := parseCmfAnswer(in, "Альмурзаева Марха А", one); a.Kind != want {
			t.Errorf("один вариант, %q: %+v", in, a)
		}
	}
	// «да» при нескольких вариантах — не выбор.
	if a, _ := parseCmfAnswer("да", "Альмурзаева Марха А", screenshotCands); a.Kind == ansPick {
		t.Errorf("«да» при двух вариантах: %+v", a)
	}
}

func TestPlanCmfAnswer(t *testing.T) {
	look := func(res []cmf.ClientInfo, kind cmfMatchKind, err error) func(string) ([]cmf.ClientInfo, cmfMatchKind, error) {
		return func(string) ([]cmf.ClientInfo, cmfMatchKind, error) { return res, kind, err }
	}
	a := cmfAnswer{Kind: ansClient, Name: "Иванов Иван"}
	if p := planCmfAnswer(a, nil, look(nil, cmfNoMatch, nil)); p.Action != actNotFound {
		t.Errorf("не найден: %+v", p)
	}
	two := []cmf.ClientInfo{{ID: "1", FullName: "Иванов Иван Петрович"}, {ID: "2", FullName: "Иванов Иван Сергеевич"}}
	if p := planCmfAnswer(a, nil, look(two, cmfWeak, nil)); p.Action != actAskAgain || len(p.Options) != 2 {
		t.Errorf("двое: %+v", p)
	}
	if p := planCmfAnswer(a, nil, look(two[:1], cmfWeak, nil)); p.Action != actBind {
		t.Errorf("один, все слова совпали: %+v", p)
	}
	if p := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: "Разет"}, nil, look([]cmf.ClientInfo{{ID: "90", FullName: "Хасанова Разет Мусаевна"}}, cmfStrong, nil)); p.Action != actAskAgain {
		t.Errorf("одно имя — только с подтверждением: %+v", p)
	}
	if p := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: "Магомед"}, nil, look([]cmf.ClientInfo{{ID: "88", FullName: "Магомедова Патимат Ибрагимовна"}}, cmfExact, nil)); p.Action != actNotFound {
		t.Errorf("Магомед ≠ Магомедова: %+v", p)
	}
	if p := planCmfAnswer(a, nil, look([]cmf.ClientInfo{{ID: "3", FullName: "Петров Иван"}}, cmfWeak, nil)); p.Action != actAskAgain {
		t.Errorf("один нечёткий, фамилия другая: %+v", p)
	}
	if p := planCmfAnswer(a, nil, look(nil, cmfNoMatch, errors.New("down"))); p.Action != actLookupError {
		t.Errorf("ошибка программы: %+v", p)
	}
	if p := planCmfAnswer(cmfAnswer{Kind: ansPick, Pick: 1}, screenshotCands, nil); p.Action != actBind || p.Client.ID != "12" {
		t.Errorf("выбор: %+v", p)
	}
	if p := planCmfAnswer(cmfAnswer{Kind: ansPick, Pick: 5}, screenshotCands, nil); p.Action != actNone {
		t.Errorf("выбор вне списка: %+v", p)
	}
	if p := planCmfAnswer(cmfAnswer{Kind: ansOther}, screenshotCands, nil); p.Action != actNone {
		t.Errorf("не ответ: %+v", p)
	}
}

func TestCmfAskTextVariants(t *testing.T) {
	q, _ := cmfAskText("Каталов Ахмед", 5000, nil)
	if !strings.Contains(q, "не нашёл") || !strings.Contains(q, "ФИО клиента") {
		t.Errorf("не найден: %q", q)
	}
	q, _ = cmfAskText("Каталов Ахмед", 5000, []cmf.ClientInfo{{ID: "1", FullName: "Каталов Ахмед Нажудович"}, {ID: "2", FullName: "Каталов Ахмед Русланович"}})
	if !strings.Contains(q, "несколько подходящих") {
		t.Errorf("тёзки полностью: %q", q)
	}
	q, opts := cmfAskText("Альмурзаева Марха А", 19000, []cmf.ClientInfo{{ID: "1", FullName: "Мусиева Марха Ахмедовна"}, {ID: "2", FullName: "Альмурзаева Разет Ахмедовна"}})
	if !strings.Contains(q, "та же фамилия") || opts[0].ID != "2" {
		t.Errorf("однофамилица первой: %q %v", q, opts)
	}
	q, _ = cmfAskText("Альмурзаева Марха А", 19000, []cmf.ClientInfo{{ID: "2", FullName: "Альмурзаева Разет Ахмедовна"}})
	if !strings.Contains(q, "той же фамилией") {
		t.Errorf("одна однофамилица: %q", q)
	}
	q, _ = cmfAskText("Альмурзаева Марха А", 19000, screenshotCands[:1])
	if !strings.Contains(q, "только похожее имя") {
		t.Errorf("один тёзка: %q", q)
	}
}

func TestAssistantOutgoing(t *testing.T) {
	for in, want := range map[string]string{
		"[молчу]":       "",
		" [молчу] ":     "",
		"Молчу.":        "",
		"":              "",
		"Привет!":       "Привет!",
		"Ок [молчу]":    "Ок",
		"Напишите ФИО.": "Напишите ФИО.",
	} {
		if got := assistantOutgoing(in); got != want {
			t.Errorf("assistantOutgoing(%q) = %q, ожидали %q", in, got, want)
		}
	}
}
