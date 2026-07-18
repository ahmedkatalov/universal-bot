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

func TestResolveNameFIO(t *testing.T) {
	// Банки печатают на чеке полное ФИО ("Милана Нажудовна К."), а в алиасах
	// у нас только имена — ResolveName должен сопоставлять по словам.
	am := NewAliasMap()

	cases := []struct {
		raw     string
		want    string
		matched bool
	}{
		{"Милана Нажудовна К.", "Милана", true},
		{"Ахмед Нажудович К", "Ахмед", true},
		{"Хадижат Имрановна С.", "Хадижат", true},
		{"Милана", "Милана", true},                      // точное совпадение тоже работает
		{"Иван Иванович И.", "Иван Иванович И.", false}, // незнакомое ФИО — на проверку
		{"К.", "К.", false}, // одни инициалы — не сопоставляем
	}
	for _, c := range cases {
		got, matched := am.ResolveName(c.raw)
		if got != c.want || matched != c.matched {
			t.Errorf("ResolveName(%q): ожидали (%q, %v), получили (%q, %v)", c.raw, c.want, c.matched, got, matched)
		}
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

// TestIsCash — правило владельца: наличка помечается словом наличка/нал/кэш,
// «офис» или «у ‹имя›»; чистое «ФИО+сумма» — это дубль чека, не наличка.
func TestIsCash(t *testing.T) {
	cash := []string{
		"Шошуков Руслан 22т наличка",
		"нал 15000",
		"Ахмед Каталов 10000 у Дени",
		"Иса 5000 офис",
		"кэш 3000",
		"передал 20000 наличными",
	}
	notCash := []string{
		"Шошуков Руслан 22000",
		"Милана 25000",
		"оплата через интернет-банк 5000",
		"Ахмед Каталов 10000",
	}
	for _, s := range cash {
		if !IsCash(s) {
			t.Errorf("IsCash(%q) = false, ожидали true (наличка)", s)
		}
	}
	for _, s := range notCash {
		if IsCash(s) {
			t.Errorf("IsCash(%q) = true, ожидали false (не наличка)", s)
		}
	}
}

// TestExtractAmount — сокращённые суммы, как их пишут в группе.
func TestExtractAmount(t *testing.T) {
	cases := map[string]float64{
		"Шошуков Руслан 22т":  22000,
		"5к":                  5000,
		"25 тыщ":              25000,
		"3 млн":               3000000,
		"Загалаев Адлан 130000": 130000,
		"за 2 месяца":         2, // нет суффикса, маленькое число — как есть
		"просто текст":        0,
	}
	for in, want := range cases {
		if got := ExtractAmount(in); got != want {
			t.Errorf("ExtractAmount(%q) = %.0f, ожидали %.0f", in, got, want)
		}
	}
}

// TestLooksMessyPayment — грязные форматы (наличка/кто-собрал/сокращения/сумма
// без имени) уходят в ИИ; чистый простой список — нет.
func TestLooksMessyPayment(t *testing.T) {
	messy := []string{
		"Джабраилов сулеман\nналичка\n72.600р\nмансур взял",
		"Оплата наличными:35.000₽\nАсхабов Ибрагим\nОтдал к Солтамурадову Адаму",
		"У Усумова Рауфа забрал 31 т\nНаличка",
		"Умхадижиев Рахман 170т",
		"Манаев Шамиль 120т",
	}
	for _, s := range messy {
		if !LooksMessyPayment(s, ParseMessage(s)) {
			t.Errorf("LooksMessyPayment(%q) = false, ожидали true", s)
		}
	}
	clean := []string{
		"Ахмед 5000",
		"Милана 25000\nРасул 30000",
	}
	for _, s := range clean {
		if LooksMessyPayment(s, ParseMessage(s)) {
			t.Errorf("LooksMessyPayment(%q) = true, ожидали false", s)
		}
	}
}
