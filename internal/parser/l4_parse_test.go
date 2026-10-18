package parser

import (
	"strings"
	"testing"
	"time"
)

// Регрессии L4: дата операции на PDF-чеке и разбор сумм в ответах.

// TestL4parse_18_DetectTxTimeSkipsFormationDate — «Дата формирования/печати»
// (когда квитанцию скачали) не должна побеждать «Дату операции».
func TestL4parse_18_DetectTxTimeSkipsFormationDate(t *testing.T) {
	op := time.Date(2026, 10, 17, 23, 40, 0, 0, time.Local)
	cases := map[string]string{
		"formation datetime first, op date+time on separate lines": "Почта Банк\nКвитанция о переводе\nДата формирования: 18.10.2026 09:15:02\n" +
			"Дата операции: 17.10.2026\nВремя операции: 23:40\nСумма перевода\n11 000 ₽",
		"formation value on next line": "Квитанция\nДата формирования\n18.10.2026 09:15:02\nДата операции\n17.10.2026 23:40:00\nСумма\n11 000 ₽",
		"print date":                   "Дата печати: 18.10.2026 09:15\nДата и время операции: 17.10.2026 23:40\nСумма\n11 000 ₽",
		"formation after op date":      "Дата операции: 17.10.2026\nДата формирования: 18.10.2026 09:15:02\nВремя операции: 23:40\nСумма\n11 000 ₽",
		"standalone formation stamp":   "Сформировано 18.10.2026 09:15:02\nДата операции 17.10.2026\nВремя 23:40\nСумма\n11 000 ₽",
	}
	for name, text := range cases {
		rd := ParseReceipt(text)
		if !rd.HasTxTime || !rd.TxTime.Equal(op) {
			t.Errorf("%s: дата чека %v (найдена=%v), ожидали дату операции %v", name, rd.TxTime, rd.HasTxTime, op)
		}
	}
	// Обычные чеки не сломаны.
	plain := map[string]time.Time{
		"Чек по операции\nДата операции 18.10.2026 12:00:00\nСумма перевода\n5 000 ₽":              time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local),
		"Дата рождения 01.01.1990\nДата операции: 17.10.2026\nВремя операции: 23:40\nСумма\n500 ₽": op,
		"27.06.2026 11:43:33\nСумма\n500 ₽":                               time.Date(2026, 6, 27, 11, 43, 33, 0, time.Local),
		"Дата и время перевода 26 июня 2026 22:58:35 (МСК)\nСумма\n500 ₽": time.Date(2026, 6, 26, 22, 58, 35, 0, time.Local),
		// Даты операции нет совсем — дата формирования лучше времени присылки.
		"Квитанция\nДата формирования: 17.10.2026 23:41:05\nСумма\n500 ₽": time.Date(2026, 10, 17, 23, 41, 5, 0, time.Local),
	}
	for text, want := range plain {
		if rd := ParseReceipt(text); !rd.TxTime.Equal(want) {
			t.Errorf("%q: дата %v, ожидали %v", text, rd.TxTime, want)
		}
	}
}

// TestL4parse_21_ExtractAmountSuffixes — «15тр», «15 т.р.», «15 тыс.руб» — это 15 000.
func TestL4parse_21_ExtractAmountSuffixes(t *testing.T) {
	cases := map[string]float64{
		"Исаев Харон 15тр":          15000,
		"Исаев Харон 15 т.р.":       15000,
		"Исаев Харон 15 т.р":        15000,
		"Исаев Харон 15т.р.":        15000,
		"Исаев Харон 15 тыс.руб":    15000,
		"Исаев Харон 15 тыс. р.":    15000,
		"Исаев Харон 15 тыс руб":    15000,
		"Исаев Харон 15 тысяч руб.": 15000,
		"Исаев Харон 15к руб":       15000,
		"Исаев Харон 15 т р":        15000,
		"15 тренировок":             15, // «тр» — начало слова, не тысячи
		"Шошуков Руслан 22т":        22000,
		"1.5 млн":                   1500000,
		"2,5к":                      2500,
	}
	for in, want := range cases {
		if got := ExtractAmount(in); got != want {
			t.Errorf("ExtractAmount(%q) = %v, ожидали %v", in, got, want)
		}
	}
	if got := ExtractAmount("осталось 22 т.е. потом"); got == 22000 {
		t.Errorf("ExtractAmount(«22 т.е.») = 22000 — «т.е.» принято за тысячи")
	}
	if got := ExtractAmount("Отдал 15000 к Адаму"); got != 15000 {
		t.Errorf("ExtractAmount(«15000 к Адаму») = %v — предлог «к» принят за тысячи", got)
	}
}

// TestL4parse_21_NoJoinOfSeparateNumbers — разные числа через пробел или перенос
// строки не склеиваются в одну сумму; дата после суммы — не сумма.
func TestL4parse_21_NoJoinOfSeparateNumbers(t *testing.T) {
	extract := map[string]float64{
		"Исаев Харон 15000 18.10":       15000,
		"Исаев Харон 15000 18.10.2026":  15000,
		"Исаев Харон 15000 18.10.26":    15000,
		"18.10 Исаев Харон 15000":       15000,
		"Исаев Харон 15 000 18.10":      15000,
		"Исаев Харон 15000 в 12:30":     15000,
		"Исаев Харон за 2 месяца 15000": 15000,
		"договор 903 35.000":            35000,
		"Исаев Харон 15000\n20000":      15000,
	}
	for in, want := range extract {
		if got := ExtractAmount(in); got != want {
			t.Errorf("ExtractAmount(%q) = %v, ожидали %v", in, got, want)
		}
	}
	money := map[string]float64{
		"15000 18.10":  15000,
		"15000\n20000": 15000,
		"15 000\n200":  15000,
		"5000 300":     5000,
		"1 15000":      1,
		"15000 200":    15000,
	}
	for in, want := range money {
		if got := ParseMoneyValue(in); got != want {
			t.Errorf("ParseMoneyValue(%q) = %v, ожидали %v", in, got, want)
		}
	}
}

// TestL4parse_21_SingleAmountAmbiguous — две РАЗНЫЕ суммы в ответе — не угадываем.
func TestL4parse_21_SingleAmountAmbiguous(t *testing.T) {
	type res struct {
		v   float64
		amb bool
	}
	cases := map[string]res{
		"Исаев Харон 15000 20000":       {0, true},
		"Исаев Харон 15000\n20000":      {0, true},
		"Исаев Харон 15000 + 5000":      {0, true},
		"Исаев Харон 15000 18.10":       {15000, false},
		"Исаев Харон 15000 за 2 месяца": {15000, false},
		"Исаев Харон 15000 (15т)":       {15000, false},
		"Исаев Харон 15тр":              {15000, false},
		"Исаев Харон 15000 договор 903": {15000, false},
		"Исаев Харон":                   {0, false},
		"за 2 месяца":                   {2, false},
	}
	for in, want := range cases {
		v, amb := ExtractSingleAmount(in)
		if v != want.v || amb != want.amb {
			t.Errorf("ExtractSingleAmount(%q) = (%v, %v), ожидали (%v, %v)", in, v, amb, want.v, want.amb)
		}
	}
}

// TestL4parse_21_ReceiptAmountsStillParse — суммы с чеков (OCR) читаются как раньше.
func TestL4parse_21_ReceiptAmountsStillParse(t *testing.T) {
	cases := map[string]float64{
		"15 000,00 ₽":      15000,
		"15\u00a0000,00 ₽": 15000,
		"15\u202f000 ₽":    15000,
		"1 500 000":        1500000,
		"1 500 000,50 ₽":   1500000.5,
		"15.000":           15000,
		"5 000 ₽":          5000,
		"30 000 Р":         30000,
		"10000.00 ₽":       10000,
		"79 650,00 ₽":      79650,
		"2.500.000 ₽":      2500000,
		"1.234,56":         1234.56,
		"844,03":           844.03,
		"0 ₽":              0,
	}
	for in, want := range cases {
		if got := ParseMoneyValue(in); got != want {
			t.Errorf("ParseMoneyValue(%q) = %v, ожидали %v", in, got, want)
		}
	}
	receipts := map[string]float64{
		"СберБанк\nЧек по операции\nДата операции 18.10.2026 12:00:00\nСумма перевода\n15 000,00 ₽\nКомиссия\n0,00 ₽": 15000,
		"Квитанция\nСумма перевода: 1 500 000 ₽\nКомиссия: 0 ₽":                                                       1500000,
		"Альфа-Банк\nСколько\n15.000 ₽\nСтатус\nВыполнен":                                                             15000,
		"РСХБ\nПеревод\n5 000 ₽\n18.10.2026 12:00":                                                                    5000,
	}
	for text, want := range receipts {
		if rd := ParseReceipt(text); rd.Amount != want {
			t.Errorf("ParseReceipt(%q).Amount = %v, ожидали %v", strings.ReplaceAll(text, "\n", " | "), rd.Amount, want)
		}
	}
}

// TestL4parse_21_ParseMessageThousandSuffix — текстовый платёж «ФИО 15 т.р.».
func TestL4parse_21_ParseMessageThousandSuffix(t *testing.T) {
	for _, in := range []string{"Исаев Харон 15тр", "Исаев Харон 15 т.р.", "Исаев Харон 15 тыс.руб", "Исаев Харон 15 тыс. р."} {
		res := ParseMessage(in)
		if len(res.Transactions) != 1 || res.Transactions[0].Amount != 15000 || res.Transactions[0].RawName != "Исаев Харон" {
			t.Errorf("ParseMessage(%q) = %+v, ожидали Исаев Харон 15000", in, res.Transactions)
		}
	}
}
