package parser

import "testing"

func TestParseCorrection(t *testing.T) {
	for _, c := range []struct {
		in   string
		ok   bool
		name string
		old  float64
		new  float64
	}{
		{"не 5000 а 6000", true, "", 5000, 6000},
		{"Магомедов не 5000 а 6000", true, "Магомедов", 5000, 6000},
		{"Магомедов Ахмед не 5000, а 6000₽", true, "Магомедов Ахмед", 5000, 6000},
		{"ой, не 5 а 6 тысяч", true, "", 5000, 6000},
		{"правильно 6000", true, "", 0, 6000},
		{"Правильно: 6000р", true, "", 0, 6000},
		{"исправь на 7000", true, "", 0, 7000},
		{"Исаев сказал в следующий раз не 5000 а 6000 принесёт", false, "", 0, 0},
		{"Правильно записал, он ещё 3000 должен", false, "", 0, 0},
		{"Исаев Харон принёс не 10000 а 15000", false, "", 0, 0},
		{"не 5000 а 6000?", false, "", 0, 0},
		{"неправильно 6000", false, "", 0, 0},
	} {
		got, ok := ParseCorrection(c.in)
		if ok != c.ok || (ok && (got.Name != c.name || got.Old != c.old || got.New != c.new)) {
			t.Errorf("%q: %+v %v, хочу ok=%v %q %v→%v", c.in, got, ok, c.ok, c.name, c.old, c.new)
		}
	}
}
