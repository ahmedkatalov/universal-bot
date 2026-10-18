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
		{"не 5000р а 6000р", true, "", 5000, 6000},
		{"Магомедов не 5000₽ а 6000₽", true, "Магомедов", 5000, 6000},
		{"не 5000 руб а 6000 руб", true, "", 5000, 6000},
		{"не 5000 рублей а 6000", true, "", 5000, 6000},
		{"не 5.000р, а 6.000р", true, "", 5000, 6000},
		{"магомедов не 5000 а 6000", true, "магомедов", 5000, 6000},
		{"Нал не 5000 а 6000", true, "", 5000, 6000},
		{"Ок не 5000 а 6000", true, "", 5000, 6000},
	} {
		got, ok := ParseCorrection(c.in)
		if ok != c.ok || (ok && (got.Name != c.name || got.Old != c.old || got.New != c.new)) {
			t.Errorf("%q: %+v %v, хочу ok=%v %q %v→%v", c.in, got, ok, c.ok, c.name, c.old, c.new)
		}
	}
}

func TestParseCashMarkers(t *testing.T) {
	for _, c := range []struct {
		in        string
		name      string
		amount    float64
		cash      bool
		collector string
	}{
		{"Исаев Харон 10000 у Дени", "Исаев Харон", 10000, true, "Дени"},
		{"Магомедов Ахмед 15000 нал", "Магомедов Ахмед", 15000, true, ""},
		{"Магомедов Ахмед нал 15000", "Магомедов Ахмед", 15000, true, ""},
		{"Наличка Магомедов Ахмед 15000", "Магомедов Ахмед", 15000, true, ""},
		{"27000 наличка", "Наличка", 27000, true, ""},
		{"Нал 27000", "Наличка", 27000, true, ""},
		{"Магомедов Ахмед\nналичка 15000", "Магомедов Ахмед", 15000, true, ""},
		{"Магомедов Ахмед 15000\nналичка", "Магомедов Ахмед", 15000, true, ""},
		{"Магомедов Ахмед 15000 Мансур взял", "Магомедов Ахмед", 15000, true, "Мансур"},
		{"Магомедов Ахмед 15000", "Магомедов Ахмед", 15000, false, ""},
	} {
		r := ParseMessage(c.in)
		if len(r.Transactions) != 1 {
			t.Errorf("%q: %+v", c.in, r)
			continue
		}
		tr := r.Transactions[0]
		if (tr.RawName != c.name && !(c.name == "Наличка" && tr.RawName == "Нал")) || tr.Amount != c.amount || tr.IsCash != c.cash || tr.Collector != c.collector {
			t.Errorf("%q: %+v, хочу %q %v cash=%v у=%q", c.in, tr, c.name, c.amount, c.cash, c.collector)
		}
	}
	// Наличка заголовком блока — только для своего блока.
	r := ParseMessage("Наличка:\nМагомедов 5000\n\nМилана\n7000")
	for _, tr := range r.Transactions {
		if tr.RawName == "Милана" && tr.IsCash {
			t.Errorf("блок Миланы помечен наличкой: %+v", r.Transactions)
		}
		if tr.RawName == "Магомедов" && !tr.IsCash {
			t.Errorf("блок под «Наличка:» не наличка: %+v", r.Transactions)
		}
	}
}
