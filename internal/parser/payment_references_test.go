package parser

import "testing"

func TestPaymentReferencesAreNeverAmounts(t *testing.T) {
	for _, text := range []string{
		"Договор 983", "Договор № 983/2026", "Номер договора 983.2026", "дог.345", "д.345",
		"Мусаев Рахимжан\nДоговор 983", "телефон +7 (928) 123-45-67",
		"тел 8 928 123 45 67", "8 928 123 45 67",
		"89281234567", "Счёт 12345678901234567890",
	} {
		t.Run(text, func(t *testing.T) {
			if got := ExtractAmount(text); got != 0 {
				t.Errorf("номер принят за сумму: %v", got)
			}
			if got, amb := ExtractSingleAmount(text); got != 0 || amb {
				t.Errorf("номер принят за ответ о сумме: %v, %v", got, amb)
			}
			if got := ParseMessage(text).Transactions; len(got) != 0 {
				t.Errorf("служебная запись стала платежом: %+v", got)
			}
		})
	}
}

func TestPaymentReferencesKeepClientAndAmount(t *testing.T) {
	for _, text := range []string{
		"Ханжиев Нурид\nДоговор 941\n20.100 наличку взял Сафаи\nЗакрыт",
		"Ханжиев Нурид\nДоговор №941/2026\n20 100 наличными",
		"Ханжиев Нурид\nДоговор Закрыл\n20.100 нал",
		"Ханжиев Нурид\nЗакрыл договор 99999 20.100 нал",
		"Ханжиев Нурид\nДоговор 941\n20,1т нал",
		"Ханжиев Нурид\n20.100\nнал\nДоговор 941\nЗакрыт",
		"Ханжиев Нурид\nНомер договора 941.\n20.100 нал",
		"Ханжиев Нурид\nтел 8 928 123 45 67\n20.100 нал",
		"Ханжиев Нурид\nтелефон: +7 (928) 123-45-67\n20.100 нал",
		"Ханжиев Нурид\nНомер телефона: 8 928 123 45 67\n20.100 нал",
	} {
		t.Run(text, func(t *testing.T) {
			if got, amb := ExtractSingleAmount(text); got != 20100 || amb {
				t.Errorf("сумма: %v, %v; ожидали 20100 без неоднозначности", got, amb)
			}
			got := ParseMessage(text).Transactions
			if len(got) != 1 || got[0].RawName != "Ханжиев Нурид" || got[0].Amount != 20100 || !got[0].IsCash {
				t.Errorf("платёж искажён: %+v", got)
			}
		})
	}
}

func TestCashCollectorAfterAmount(t *testing.T) {
	for _, collector := range []string{"Мансур", "МБ", "Сафаи"} {
		got := ParseMessage("Керимов Турпал-Али\n36.800 нал взял " + collector).Transactions
		if len(got) != 1 || got[0].Amount != 36800 || !got[0].IsCash || got[0].Collector != collector {
			t.Errorf("ответственный %q не сохранён: %+v", collector, got)
		}
	}
}
