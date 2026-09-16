package bot

import "testing"

// TestConsensusAmount — из 3 прочтений берётся согласованная сумма: большинство,
// а при всех разных — медиана (отсекает разовый выброс вроде лишнего нуля).
func TestConsensusAmount(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{130000, 130000, 1300000}, 130000}, // 2 из 3 совпали
		{[]float64{1300000, 130000, 130000}, 130000}, // порядок не важен
		{[]float64{130000, 140000, 1300000}, 140000}, // все разные -> медиана, выброс отсечён
		{[]float64{50000, 50000, 50000}, 50000},      // единогласно
		{[]float64{15000, 18000}, 15000},             // два чтения -> меньшее (медиана нижнего)
		{[]float64{42000}, 42000},                    // одно чтение
	}
	for _, c := range cases {
		if got := consensusAmount(c.in); got != c.want {
			t.Errorf("consensusAmount(%v) = %.0f, ожидали %.0f", c.in, got, c.want)
		}
	}
}

// TestConsensusPayments — 3 независимых чтения текстового платежа сводятся в один:
// сумма медианой (разовый misread отсекается), нал/перевод большинством,
// разовая галлюцинация (в 1 из 3) отбрасывается.
func TestConsensusPayments(t *testing.T) {
	runs := [][]aiPayment{
		{{Name: "Ахмед", Amount: 10000, Cash: true, Collector: "Дени"}, {Name: "Милана", Amount: 25000, Cash: false}},
		{{Name: "Ахмед", Amount: 5000, Cash: true}, {Name: "Милана", Amount: 25000, Cash: false}},                    // сумма Ахмеда прочлась неверно (5000)
		{{Name: "ахмед", Amount: 10000, Cash: false, Collector: "Дени"}, {Name: "Призрак", Amount: 999, Cash: true}}, // галлюцинация "Призрак" в 1 чтении
	}
	got := consensusPayments(runs, 3)
	// Ожидаем 2 платежа: Ахмед и Милана; "Призрак" (1 из 3) отброшен.
	byName := map[string]aiPayment{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if len(got) != 2 {
		t.Fatalf("ожидали 2 платежа (Призрак отброшен), получили %d: %+v", len(got), got)
	}
	a, ok := byName["Ахмед"]
	if !ok {
		t.Fatalf("Ахмед потерян: %+v", got)
	}
	if a.Amount != 10000 { // медиана [5000,10000,10000] = 10000, misread 5000 отсечён
		t.Errorf("сумма Ахмеда = %.0f, ожидали 10000 (медиана отсекает 5000)", a.Amount)
	}
	if !a.Cash { // голоса cash: true,true,false -> 2/3 -> наличка
		t.Errorf("Ахмед должен быть наличкой (2 из 3 чтений)")
	}
	if a.Collector != "Дени" {
		t.Errorf("collector Ахмеда = %q, ожидали Дени", a.Collector)
	}
	if _, ghost := byName["Призрак"]; ghost {
		t.Errorf("галлюцинация «Призрак» (1 из 3) не должна попасть в результат")
	}
}
