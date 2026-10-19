package parser

import (
	"testing"
	"time"
)

// Регрессии L5 (ИИ/тексты): дата операции словом месяца с «в»/запятой/«г.».

// TestL5ai_18_NamedMonthDateWithV — «18 октября 2026 в 14:05» (и «, 14:05»,
// «г. в 14:05») — это 14:05, а не полночь. Дата без времени — «только дата»
// (полдень или «Время операции» рядом), но не 00:00.
func TestL5ai_18_NamedMonthDateWithV(t *testing.T) {
	want := time.Date(2026, 10, 18, 14, 5, 0, 0, time.Local)
	for _, v := range []string{
		"18 октября 2026 в 14:05",
		"18 октября 2026, 14:05",
		"18 октября 2026 г. в 14:05",
		"18 октября 2026г., 14:05",
		"18 октября 2026 года в 14:05",
		"18 октября 2026, в 14:05",
		"18 октября 2026 14:05",
		"18 окт. 2026 в 14:05",
		"18 окт. 2026 г. в 14:05",
	} {
		text := "Перевод выполнен\nДата операции " + v + "\nПолучатель\nХадижат Ибрагимовна И.\nСумма перевода\n7 000 ₽"
		if rd := ParseReceipt(text); !rd.HasTxTime || !rd.TxTime.Equal(want) {
			t.Errorf("%q: время операции %v (найдено=%v), ожидали %v", v, rd.TxTime, rd.HasTxTime, want)
		}
	}
	// Дата словом без времени + отдельная строка «Время операции».
	text := "Перевод выполнен\nДата операции 18 октября 2026\nВремя операции 14:05\nСумма перевода\n7 000 ₽"
	if rd := ParseReceipt(text); !rd.TxTime.Equal(want) {
		t.Errorf("дата словом + «Время операции»: %v, ожидали %v", rd.TxTime, want)
	}
	// Дата словом без времени вовсе — полдень, как у «Дата операции 18.10.2026».
	text = "Перевод выполнен\nДата операции 18 октября 2026\nСумма перевода\n7 000 ₽"
	if rd := ParseReceipt(text); !rd.HasTxTime || !rd.TxTime.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)) {
		t.Errorf("дата словом без времени: %v (найдено=%v), ожидали 18.10.2026 12:00", rd.TxTime, rd.HasTxTime)
	}
	// Дата словом без подписи: дата с чека (время строкой ниже), а не «нет даты».
	text = "Квитанция от 18 октября 2026\n14:05\nСумма\n7 000 ₽"
	if rd := ParseReceipt(text); !rd.HasTxTime || !rd.TxTime.Equal(want) {
		t.Errorf("дата словом без подписи: %v (найдено=%v), ожидали %v", rd.TxTime, rd.HasTxTime, want)
	}
	text = "Квитанция от 18 октября 2026\nСумма\n7 000 ₽"
	if rd := ParseReceipt(text); !rd.HasTxTime || !rd.TxTime.Equal(time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)) {
		t.Errorf("дата словом без подписи и времени: %v (найдено=%v), ожидали 18.10.2026 12:00", rd.TxTime, rd.HasTxTime)
	}
	// Шапка с датой без времени не перебивает дату операции со временем ниже.
	text = "Квитанция от 18 октября 2026\nДата и время операции 18.10.2026 14:05\nСумма\n7 000 ₽"
	if rd := ParseReceipt(text); !rd.TxTime.Equal(want) {
		t.Errorf("шапка без времени: %v, ожидали %v", rd.TxTime, want)
	}
}

// TestL6ai_28_DateOnlyTimeDefaulted — на чеке одна дата операции без времени:
// полдень придуман (TimeDefaulted, TxTimeGuessed), дата — с чека. Время рядом
// или в строке «Время операции» — напечатанное.
func TestL6ai_28_DateOnlyTimeDefaulted(t *testing.T) {
	for _, c := range []struct {
		text      string
		want      time.Time
		defaulted bool
	}{
		{"Почта Банк\nКвитанция о переводе\nДата операции: 14.10.2026\nКому\nХадижат Ибрагимовна И.\nСумма перевода\n15 000 ₽",
			time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local), true},
		{"Перевод выполнен\nДата операции 14 октября 2026\nСумма перевода\n7 000 ₽",
			time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local), true},
		{"Перевод выполнен\nДата операции: 14.10.2026\nВремя операции: 08:41\nСумма перевода\n7 000 ₽",
			time.Date(2026, 10, 14, 8, 41, 0, 0, time.Local), false},
		{"Перевод выполнен\nДата операции: 14.10.2026 08:41:05\nСумма перевода\n7 000 ₽",
			time.Date(2026, 10, 14, 8, 41, 5, 0, time.Local), false},
	} {
		rd := ParseReceipt(c.text)
		if !rd.HasTxTime || !rd.TxTime.Equal(c.want) || rd.TimeDefaulted != c.defaulted || rd.TxTimeGuessed() != c.defaulted {
			t.Errorf("%q: %v (найдено=%v, придумано=%v), ожидали %v (придумано=%v)", c.text, rd.TxTime, rd.HasTxTime, rd.TimeDefaulted, c.want, c.defaulted)
		}
	}
	if rd := ParseReceipt("Перевод выполнен\nСумма перевода\n7 000 ₽"); rd.HasTxTime || !rd.TxTimeGuessed() {
		t.Errorf("без даты: найдено=%v угадано=%v, ожидали угаданное", rd.HasTxTime, rd.TxTimeGuessed())
	}
}
