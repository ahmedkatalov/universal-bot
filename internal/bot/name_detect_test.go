package bot

import "testing"

func TestLooksLikeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"цихаев саляхь", "цихаев саляхь", true},
		{"Атабаев Турпал", "Атабаев Турпал", true},
		{"Эдиев Нурмагомед Нурдиевич", "Эдиев Нурмагомед Нурдиевич", true},
		{"Касумова марям 25.000", "Касумова марям", true}, // сумма отброшена
		{"Ок", "", false}, // реплика, не имя
		{"да", "", false},
		{"спасибо", "", false},
		{"Милана", "", false},       // одно слово — не считаем ФИО клиента
		{"Милана 25000", "", false}, // это платёж
		{"чей это чек?", "", false}, // вопрос
	}
	for _, c := range cases {
		got, ok := looksLikeName(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("looksLikeName(%q) = (%q,%v), ожидали (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestClarifyNameFromReply — разбор ОТВЕТА на вопрос бота про чек. Главное:
// вопрос владельца («Записал чек?») НЕ должен становиться именем клиента, а
// целые фразы («клиент X сумма Y дата Z») отдаются ассистенту (deferToAI).
func TestClarifyNameFromReply(t *testing.T) {
	cases := []struct {
		in       string
		wantName string  // "" — имя НЕ должно извлекаться (уйдёт ассистенту/по сумме)
		wantAmt  float64 // 0 — сумму не проверяем
	}{
		// Реальный баг из чата: вопрос владельца принимался за ФИО клиента.
		{"Записал чек ?", "", 0},
		{"записал чек", "", 0}, // без «?»: и «записал», и «чек» — стоп-слова -> пусто
		{"а почему не посчитал", "", 0},
		// Чистое ФИО — берём как есть.
		{"Шовхаев Якъуб", "Шовхаев Якъуб", 0},
		{"Шовхаев акъуб✅", "Шовхаев акъуб", 0},       // эмодзи, прилипшая к слову, отброшена
		{"Клиент Шовхаев Якъуб", "Шовхаев Якъуб", 0}, // «Клиент» — стоп-слово
		// Одно имя, имя+сумма — короткий ответ, разбираем.
		{"Ахмед", "Ахмед", 0},
		{"Ахмед 15000", "Ахмед", 15000},
		{"15000", "", 15000}, // только сумма
		// Длинная структурная фраза — ассистенту (он запишет через fix_receipt).
		{"Клиент Шовхаев Якъуб сумма 21500 Дата 17.09.2026 секунды 10:41", "", 0},
	}
	for _, c := range cases {
		name, amt, def := clarifyNameFromReply(c.in)
		// Главный инвариант: имя должно совпасть с ожидаемым (для фраз/вопросов — пусто).
		if name != c.wantName {
			t.Errorf("clarifyNameFromReply(%q): имя = %q, ожидали %q (defer=%v, amt=%.0f)", c.in, name, c.wantName, def, amt)
		}
		if c.wantAmt > 0 && amt != c.wantAmt {
			t.Errorf("clarifyNameFromReply(%q): сумма = %.0f, ожидали %.0f", c.in, amt, c.wantAmt)
		}
		// Пустое имя без суммы обязано уйти дальше (ассистенту), а не «записаться».
		if c.wantName == "" && c.wantAmt == 0 && !def && !(name == "" && amt == 0) {
			t.Errorf("clarifyNameFromReply(%q): фраза/вопрос не ушли ассистенту", c.in)
		}
	}
}

// TestLooksLikeNameCash — «ФИО + сумма + эмодзи + служебные слова» очищается
// до чистого ФИО (важно для налички: «Шошуков Руслан 22т ✅ За 2 месяца»).
func TestLooksLikeNameCash(t *testing.T) {
	cases := map[string]string{
		"Шошуков Руслан 22т ✅ За 2 месяца": "Шошуков Руслан",
		"Магомедов Иса ✅":                  "Магомедов Иса", // эмодзи отброшена
		"Иса ✅":                            "",              // одно имя-слово — не ФИО
		"бакиев ахмед":                     "бакиев ахмед",
	}
	for in, want := range cases {
		got, ok := looksLikeName(in)
		if want == "" {
			if ok {
				t.Errorf("looksLikeName(%q) = (%q,true), ожидали false", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("looksLikeName(%q) = (%q,%v), ожидали (%q,true)", in, got, ok, want)
		}
	}
}

// TestFirstNameLine — ФИО извлекается из многострочного сообщения рядом с чеком
// («Магамадов Алха\n22.000₽ ✅» -> «Магамадов Алха»).
func TestFirstNameLine(t *testing.T) {
	cases := map[string]string{
		"Магамадов Алха\n22.000₽ ✅":    "Магамадов Алха",
		"22000\nЦихаев Саляхь":         "Цихаев Саляхь",
		"Его чек":                      "", // не ФИО (одно значимое слово)
		"просто\nтекст без имени 5000": "",
		"Юсупов Умар":                  "Юсупов Умар", // однострочное тоже работает
	}
	for in, want := range cases {
		got, ok := firstNameLine(in)
		if want == "" {
			if ok {
				t.Errorf("firstNameLine(%q) = (%q,true), ожидали false", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("firstNameLine(%q) = (%q,%v), ожидали (%q,true)", in, got, ok, want)
		}
	}
}

// TestExtractCollectorName — из ответа «у кого наличка» вытаскивается имя.
func TestExtractCollectorName(t *testing.T) {
	cases := map[string]string{
		"у Дени":         "Дени",
		"Мансур взял":    "Мансур",
		"отдал Адаму":    "Адаму",
		"Дени":           "Дени",
		"у Дени наличка": "Дени",
		"взял":           "", // только маркер, имени нет
		"У Нура":         "Нура",
		"у нура":         "нура",
	}
	for in, want := range cases {
		if got := extractCollectorName(in); got != want {
			t.Errorf("extractCollectorName(%q) = %q, ожидали %q", in, got, want)
		}
	}
}

// TestParseCashDupAnswer — ответ на вопрос «повтор налички: новый или тот же?».
func TestParseCashDupAnswer(t *testing.T) {
	type res struct {
		resolved bool
		isNew    bool
	}
	cases := map[string]res{
		"новый":            {true, true},
		"это новый платёж": {true, true},
		"да, новый":        {true, true},
		"отдельный":        {true, true},
		"ещё один":         {true, true},
		"да":               {true, true},
		"тот же":           {true, false},
		"это тот же самый": {true, false},
		"тоже самое":       {true, false},
		"повтор":           {true, false},
		"дубль":            {true, false},
		"не новый":         {true, false},
		"нет":              {true, false},
		"старый":           {true, false},
		"хз":               {false, false}, // непонятно — переспросим
		"":                 {false, false},
	}
	for in, want := range cases {
		gotResolved, gotNew := parseCashDupAnswer(in)
		if gotResolved != want.resolved || gotNew != want.isNew {
			t.Errorf("parseCashDupAnswer(%q) = (%v, %v), ожидали (%v, %v)", in, gotResolved, gotNew, want.resolved, want.isNew)
		}
	}
}

// TestLooksLikeSilence — «отписки» модели (нечего добавить и т.п.) не постятся.
func TestLooksLikeSilence(t *testing.T) {
	silent := []string{
		"Пустое сообщение без контекста — тут добавить нечего.",
		"Тут мне нечего добавить",
		"Промолчу",
		"Не по теме, воздержусь",
	}
	for _, s := range silent {
		if !looksLikeSilence(s) {
			t.Errorf("looksLikeSilence(%q) = false, ожидали true", s)
		}
	}
	useful := []string{
		"Похоже, у Ахмеда за июль уже проходит 50000 — проверь, не дубль ли.",
		"Этот чек лучше записать на Мусаева, а не на Нажуда.",
	}
	for _, s := range useful {
		if looksLikeSilence(s) {
			t.Errorf("looksLikeSilence(%q) = true, ожидали false", s)
		}
	}
}
