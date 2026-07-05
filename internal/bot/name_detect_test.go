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
