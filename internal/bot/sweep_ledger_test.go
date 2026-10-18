package bot

import "testing"

// Регрессии проверки нового движка подсчёта (4-й раунд, 2026-10-18).

func TestL4_MoneyNameMatcher(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Омаров Магомед", "Умаров Магомед", false},
		{"Османов Ахмед", "Усманов Ахмед", false},
		{"Магомедов Ахмед", "Магометов Ахмед", false},
		{"Магомедов Ахмед", "Магамедов Ахмед", true},
		{"Магомедову Ахмеду", "Магомедов Ахмед", true},
		{"Ахмед Магомедов", "Магомедов Ахмед", true},
		{"Абдулаев Ислам", "Абдуллаев Ислам", true},
		{"Хусейн Исаев", "Хусеин Исаев", true},
		{"Каталов А.", "Каталов Ахмед", true},
	}
	for _, c := range cases {
		if got := sameClientMoney(c.a, c.b); got != c.want {
			t.Errorf("sameClientMoney(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
