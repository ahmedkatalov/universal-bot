package bot

import "testing"

func TestStripBotName(t *testing.T) {
	b := &Bot{botName: "Джарвис"}

	cases := []struct {
		in      string
		want    string
		matched bool
	}{
		{"Джарвис скинь отчет", "скинь отчет", true},
		{"джарвис, какой сбор за июль?", "какой сбор за июль?", true},
		{"ДЖАРВИС: привет", "привет", true},
		{"Джарвис", "Привет!", true},          // одно имя — здороваемся
		{"Джарвисом все довольны", "", false}, // не обращение, часть слова
		{"скинь отчет Джарвис", "", false},    // имя не в начале
		{"Милана 25000", "", false},           // обычный платёж
		{"/отчет", "", false},                 // команда
	}
	for _, c := range cases {
		got, matched := b.stripBotName(c.in)
		if matched != c.matched || (matched && got != c.want) {
			t.Errorf("stripBotName(%q): ожидали (%q, %v), получили (%q, %v)", c.in, c.want, c.matched, got, matched)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"89287836800":      "79287836800", // российская 8 -> 7 (формат WhatsApp)
		"89899171578":      "79899171578",
		"+7 928 783-68-00": "79287836800",
		"79287836800":      "79287836800", // уже нормализован
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Errorf("normalizePhone(%q) = %q, ожидали %q", in, got, want)
		}
	}
}
