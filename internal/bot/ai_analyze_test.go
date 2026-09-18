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

// TestConsensusPaymentsSameNameTwice — один клиент с ДВУМЯ платежами в одном
// сообщении («Ахмед 5000 / Ахмед 10000»): должны выжить оба, каждый со своей
// суммой, и порядок перечисления в разных чтениях не должен их путать.
func TestConsensusPaymentsSameNameTwice(t *testing.T) {
	runs := [][]aiPayment{
		{{Name: "Ахмед", Amount: 5000, Cash: true}, {Name: "Ахмед", Amount: 10000, Cash: false}},
		{{Name: "Ахмед", Amount: 10000, Cash: false}, {Name: "Ахмед", Amount: 5000, Cash: true}}, // другой порядок
		{{Name: "Ахмед", Amount: 5000, Cash: true}, {Name: "Ахмед", Amount: 10000, Cash: false}},
	}
	got := consensusPayments(runs, 3)
	if len(got) != 2 {
		t.Fatalf("ожидали 2 платежа Ахмеда, получили %d: %+v", len(got), got)
	}
	amounts := map[float64]bool{}
	for _, p := range got {
		amounts[p.Amount] = p.Cash
	}
	if cash, ok := amounts[5000]; !ok || !cash {
		t.Errorf("платёж 5000 (наличка) потерян или не наличка: %+v", got)
	}
	if cash, ok := amounts[10000]; !ok || cash {
		t.Errorf("платёж 10000 (перевод) потерян или стал наличкой: %+v", got)
	}
	// Платёж, встретившийся лишь в 1 из 3 чтений, — отбрасывается даже с «известным» именем.
	runs2 := [][]aiPayment{
		{{Name: "Ахмед", Amount: 5000}, {Name: "Ахмед", Amount: 7000}},
		{{Name: "Ахмед", Amount: 5000}},
		{{Name: "Ахмед", Amount: 5000}},
	}
	got2 := consensusPayments(runs2, 3)
	if len(got2) != 1 || got2[0].Amount != 5000 {
		t.Errorf("ожидали один платёж 5000 (7000 лишь в 1 чтении), получили %+v", got2)
	}
}

// TestMoneyTokens — сколько «денежных» чисел в сообщении: даты/время/мелкие
// числа без суффикса не считаются; суффиксы тысяч учитываются.
func TestMoneyTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []float64
	}{
		{"магомедов иса\nНаличка\n10.000р\nнур коффе отдал", []float64{10000}},
		{"Ахмед 20000 + 5000", []float64{20000, 5000}},
		{"Умхадижиев Рахман 170т✅", []float64{170000}},
		{"У Усумова Рауфа забрал 31 т", []float64{31000}},
		{"оплата 08.09 в 12:30 — 30 000 р", []float64{30000}},
		{"за 2 месяца, 3 млн", []float64{3000000}},
		{"осталось 22 т.е. потом", nil},
		{"просто текст", nil},
		// Раньше эти суммы терялись (дата-фильтр / двоеточие / хвосты склонений) —
		// и подмена «10.000р→5000» переставала работать. Теперь распознаются.
		{"Ахмед 1.5 млн", []float64{1500000}},
		{"Оплата наличными:35.000₽", []float64{35000}},
		{"Ахмед 25 тысяч", []float64{25000}},
		{"5 косарей", []float64{5000}},
		{"2 ляма", []float64{2000000}},
	}
	for _, c := range cases {
		got := moneyTokens(c.in)
		if len(got) != len(c.want) {
			t.Errorf("moneyTokens(%q) = %v, ожидали %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("moneyTokens(%q) = %v, ожидали %v", c.in, got, c.want)
				break
			}
		}
	}
}
