package parser

import "testing"

func TestParseBlockFormat(t *testing.T) {
	msg := `Нал
27000
40500
30000

Милана
47000
20000

Ахмед
13000
11000
8000`

	res := ParseMessage(msg)

	if len(res.Unparsed) != 0 {
		t.Errorf("ожидали 0 нераспознанных строк, получили %d: %v", len(res.Unparsed), res.Unparsed)
	}

	want := 8
	if len(res.Transactions) != want {
		t.Fatalf("ожидали %d транзакций, получили %d: %+v", want, len(res.Transactions), res.Transactions)
	}

	sumByName := map[string]float64{}
	for _, tr := range res.Transactions {
		sumByName[tr.RawName] += tr.Amount
	}

	checks := map[string]float64{
		"Нал":    97500,
		"Милана": 67000,
		"Ахмед":  32000,
	}
	for name, want := range checks {
		if got := sumByName[name]; got != want {
			t.Errorf("%s: ожидали %.0f, получили %.0f", name, want, got)
		}
	}
}

func TestParseWithNotesAndThousands(t *testing.T) {
	msg := `Ибрем
31000
26000

Расул
3000 аванс
20к премия

Хамзат
1500р`

	res := ParseMessage(msg)

	var расулАванс, расулПремия float64
	for _, tr := range res.Transactions {
		if tr.RawName == "Расул" {
			if tr.Note == "аванс" {
				расулАванс = tr.Amount
			}
			if tr.Note == "премия" {
				расулПремия = tr.Amount
			}
		}
	}

	if расулАванс != 3000 {
		t.Errorf("аванс Расула: ожидали 3000, получили %.0f", расулАванс)
	}
	if расулПремия != 20000 {
		t.Errorf("премия Расула: ожидали 20000 (20к), получили %.0f", расулПремия)
	}
}

func TestParseFreeformCardLine(t *testing.T) {
	msg := `Расул
9тысяя Скинул на твою карту`

	res := ParseMessage(msg)

	if len(res.Transactions) != 1 {
		t.Fatalf("ожидали 1 транзакцию, получили %d: %+v", len(res.Transactions), res.Transactions)
	}
	if res.Transactions[0].Amount != 9000 {
		t.Errorf("ожидали 9000, получили %.0f", res.Transactions[0].Amount)
	}
}

func TestAliasResolution(t *testing.T) {
	am := NewAliasMap()
	if am.Resolve("Пиян") != "Пияна" {
		t.Errorf("Пиян должен разрешаться в Пияна, получили %s", am.Resolve("Пиян"))
	}
	if am.Resolve("Хадижа") != "Хадижат" {
		t.Errorf("Хадижа должен разрешаться в Хадижат, получили %s", am.Resolve("Хадижа"))
	}
	if am.Resolve("Новый Человек") != "Новый Человек" {
		t.Errorf("неизвестное имя должно возвращаться как есть")
	}
}

func TestExpenseListFormat(t *testing.T) {
	// Формат из твоего сообщения про расходы кофейни: "Имя Сумма" в одной строке.
	msg := `Расходы
Кофейня 235364р
Кухня 52565р

Зарплата
Повар 10400р
Уборщица 5тысяч

Аренда 40тысяч
Электричество 21250р`

	res := ParseMessage(msg)

	if len(res.Unparsed) != 0 {
		t.Errorf("ожидали 0 нераспознанных строк, получили: %v", res.Unparsed)
	}

	want := map[string]float64{
		"Кофейня":       235364,
		"Кухня":         52565,
		"Повар":         10400,
		"Уборщица":      5000,
		"Аренда":        40000,
		"Электричество": 21250,
	}

	got := map[string]float64{}
	for _, tr := range res.Transactions {
		got[tr.RawName] = tr.Amount
	}

	for name, wantAmount := range want {
		if got[name] != wantAmount {
			t.Errorf("%s: ожидали %.0f, получили %.0f", name, wantAmount, got[name])
		}
	}
}
