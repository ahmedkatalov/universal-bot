package parser

import (
	"testing"
	"time"
)

func TestParseAlfaBankReceipt(t *testing.T) {
	// Текст воссоздан по структуре скриншота Альфа-Банка, который прислал Ахмед.
	text := `Альфа-Банк

Получатель
Хадижат Имрановна С.

Сколько
30 000 Р

Комиссия
0 Р

Номер документа
20260630151235bbbbef30812e14c5c9fd5b

Код авторизации
866004`

	if !LooksLikeBankReceipt(text) {
		t.Fatal("ожидали, что текст распознается как банковский чек")
	}

	rd := ParseReceipt(text)

	if rd.Bank != "Альфа-Банк" {
		t.Errorf("Bank: ожидали Альфа-Банк, получили %q", rd.Bank)
	}
	if rd.Recipient != "Хадижат Имрановна С." {
		t.Errorf("Recipient: ожидали 'Хадижат Имрановна С.', получили %q", rd.Recipient)
	}
	if rd.Amount != 30000 {
		t.Errorf("Amount: ожидали 30000, получили %.0f", rd.Amount)
	}
	if rd.Commission != 0 {
		t.Errorf("Commission: ожидали 0, получили %.0f", rd.Commission)
	}
	if rd.AuthCode != "866004" {
		t.Errorf("AuthCode: ожидали 866004, получили %q", rd.AuthCode)
	}
}

func TestParseVTBReceipt(t *testing.T) {
	text := `Исходящий перевод СБП
Ахмед Нажудович К.

Статус
Выполнено`

	rd := ParseReceipt(text)

	if rd.Bank != "ВТБ" {
		t.Errorf("Bank: ожидали ВТБ, получили %q", rd.Bank)
	}
	if rd.Recipient != "Ахмед Нажудович К." {
		t.Errorf("Recipient: ожидали 'Ахмед Нажудович К.', получили %q", rd.Recipient)
	}
	if rd.Status != "Выполнено" {
		t.Errorf("Status: ожидали Выполнено, получили %q", rd.Status)
	}
}

func TestNonReceiptTextNotFlagged(t *testing.T) {
	text := `Нал
27000
40500`
	if LooksLikeBankReceipt(text) {
		t.Error("обычное сообщение с суммами не должно определяться как банковский чек")
	}
}

func TestParseSberSBPReceipt(t *testing.T) {
	// Воссоздано по скриншоту Сбера (СБП) из группы "Оплата КЛНТ".
	text := `СБЕРБАНК
Чек по операции
26 июня 2026 22:58:35 (МСК)

Операция
Перевод по СБП

ФИО получателя перевода
Ахмед Нажудович К

Номер телефона получателя
+7 928 783-68-00

Банк получателя
Т-Банк

ФИО отправителя
Аслан Сулиманович Б.

Карта отправителя
•• 2289

Сумма перевода
10000.00 ₽

Комиссия
0.00 ₽

Номер операции в СБП
A6177195834050030G10040011791103`

	if !LooksLikeBankReceipt(text) {
		t.Fatal("Сбер-чек должен определяться как банковский чек")
	}

	rd := ParseReceipt(text)

	if rd.Recipient != "Ахмед Нажудович К" {
		t.Errorf("Recipient: ожидали 'Ахмед Нажудович К', получили %q", rd.Recipient)
	}
	if rd.Sender != "Аслан Сулиманович Б." {
		t.Errorf("Sender: ожидали 'Аслан Сулиманович Б.', получили %q", rd.Sender)
	}
	if rd.Amount != 10000 {
		t.Errorf("Amount: ожидали 10000, получили %.2f", rd.Amount)
	}
	if rd.Commission != 0 {
		t.Errorf("Commission: ожидали 0, получили %.2f", rd.Commission)
	}
}

func TestParseSberClientTransferReceipt(t *testing.T) {
	// Второй формат Сбера: перевод клиенту, запятая как десятичный разделитель.
	text := `Операция
Перевод клиенту СберБанка
ФИО получателя
Сафаи Асланович Н.
Телефон получателя
+7(928) 003-24-62
Номер счёта получателя
**** 2288

ФИО отправителя
Анзор Зелимханович Б.
Счёт отправителя
**** 0557
Сумма перевода
79 650,00 ₽
Комиссия
844,03 ₽

Номер документа
1000000005123962668`

	rd := ParseReceipt(text)

	if rd.Recipient != "Сафаи Асланович Н." {
		t.Errorf("Recipient: ожидали 'Сафаи Асланович Н.', получили %q", rd.Recipient)
	}
	if rd.Sender != "Анзор Зелимханович Б." {
		t.Errorf("Sender: ожидали 'Анзор Зелимханович Б.', получили %q", rd.Sender)
	}
	if rd.Amount != 79650 {
		t.Errorf("Amount: ожидали 79650, получили %.2f", rd.Amount)
	}
	if rd.Commission != 844.03 {
		t.Errorf("Commission: ожидали 844.03, получили %.2f", rd.Commission)
	}
	if rd.DocNumber != "1000000005123962668" {
		t.Errorf("DocNumber: ожидали 1000000005123962668, получили %q", rd.DocNumber)
	}
}

func TestParseMoneyValueFormats(t *testing.T) {
	cases := map[string]float64{
		"30 000 Р":    30000,
		"10000.00 ₽":  10000,
		"79 650,00 ₽": 79650,
		"35.000":      35000, // точка + 3 цифры = разделитель тысяч!
		"20.000р":     20000,
		"844,03":      844.03,
		"0.00":        0,
		"5000":        5000,
	}
	for input, want := range cases {
		if got := ParseMoneyValue(input); got != want {
			t.Errorf("ParseMoneyValue(%q): ожидали %.2f, получили %.2f", input, want, got)
		}
	}
}

func TestCaptionWithContractNumber(t *testing.T) {
	// Подпись к чеку: номер договора (903) НЕ должен приниматься за сумму.
	msg := `Сафаи
Идразов Имран договор 903 35.000 закрыл`

	res := ParseMessage(msg)

	if len(res.Transactions) != 1 {
		t.Fatalf("ожидали 1 транзакцию, получили %d: %+v", len(res.Transactions), res.Transactions)
	}
	if res.Transactions[0].Amount != 35000 {
		t.Errorf("ожидали сумму 35000 (не номер договора 903), получили %.0f", res.Transactions[0].Amount)
	}
}

func TestCaptionClosedContract(t *testing.T) {
	msg := `Батаев Анзор
Закрыл просрочку 79.650`

	res := ParseMessage(msg)

	if len(res.Transactions) != 1 {
		t.Fatalf("ожидали 1 транзакцию, получили %d: %+v", len(res.Transactions), res.Transactions)
	}
	tr := res.Transactions[0]
	if tr.RawName != "Батаев Анзор" {
		t.Errorf("имя: ожидали 'Батаев Анзор', получили %q", tr.RawName)
	}
	if tr.Amount != 79650 {
		t.Errorf("сумма: ожидали 79650, получили %.0f", tr.Amount)
	}
}

// ВНИМАНИЕ: тесты ниже — синтетические, построены по ТИПОВЫМ лейблам полей
// чеков российских банков, а не по реальным скриншотам. Реальный чек банка
// может отличаться — тогда он попадёт в needs_review, и формат нужно будет
// добавить в fieldRules по живому примеру.

func TestParseGenericTBankReceipt(t *testing.T) {
	text := `Т-Банк
Перевод выполнен

Отправитель
Иван Иванович И.

Получатель
Ахмед Нажудович К.

Итого
15 000 ₽

Комиссия
0 ₽`

	rd := ParseReceipt(text)
	if rd.Bank != "Т-Банк" {
		t.Errorf("Bank: ожидали Т-Банк, получили %q", rd.Bank)
	}
	if rd.Recipient != "Ахмед Нажудович К." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Sender != "Иван Иванович И." {
		t.Errorf("Sender: получили %q", rd.Sender)
	}
	if rd.Amount != 15000 {
		t.Errorf("Amount: ожидали 15000, получили %.0f", rd.Amount)
	}
}

func TestParseGenericOzonReceipt(t *testing.T) {
	text := `Озон Банк
Сумма операции
5 500,00 ₽
Получатель перевода
Милана Ахмедовна А.
Статус операции
Исполнено
Номер операции
987654321`

	rd := ParseReceipt(text)
	if rd.Bank != "Озон Банк" {
		t.Errorf("Bank: ожидали Озон Банк, получили %q", rd.Bank)
	}
	if rd.Recipient != "Милана Ахмедовна А." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Amount != 5500 {
		t.Errorf("Amount: ожидали 5500, получили %.0f", rd.Amount)
	}
	if rd.Status != "Исполнено" {
		t.Errorf("Status: получили %q", rd.Status)
	}
	if rd.DocNumber != "987654321" {
		t.Errorf("DocNumber: получили %q", rd.DocNumber)
	}
}

func TestParseGenericRSHBReceipt(t *testing.T) {
	text := `Россельхозбанк
Перевод по СБП
Кому
Сафаи Асланович Н.
Сумма
25 000 руб.
Сумма комиссии
0 руб.`

	rd := ParseReceipt(text)
	if rd.Bank != "Россельхозбанк" {
		t.Errorf("Bank: ожидали Россельхозбанк, получили %q", rd.Bank)
	}
	if rd.Recipient != "Сафаи Асланович Н." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Amount != 25000 {
		t.Errorf("Amount: ожидали 25000, получили %.0f", rd.Amount)
	}
	if rd.Commission != 0 {
		t.Errorf("Commission: ожидали 0, получили %.2f", rd.Commission)
	}
}

func TestSumKomissiiNotConfusedWithAmount(t *testing.T) {
	// "Сумма комиссии" должна попасть в комиссию, а не перезаписать сумму.
	text := `Получатель
Тест Тестович Т.
Сумма комиссии
150 руб.
Сумма
10 000 руб.`

	rd := ParseReceipt(text)
	if rd.Amount != 10000 {
		t.Errorf("Amount: ожидали 10000, получили %.0f", rd.Amount)
	}
	if rd.Commission != 150 {
		t.Errorf("Commission: ожидали 150, получили %.0f", rd.Commission)
	}
}

// ==== Тесты по РЕАЛЬНЫМ чекам со скриншотов из группы "Оплата КЛНТ" ====
// Двухколоночная вёрстка ("Получатель ...  Ахмед Нажудович К.") воспроизведена
// так, как её обычно склеивает OCR — лейбл и значение на одной строке.

func TestParseRealPSBReceipt(t *testing.T) {
	text := `ПСБ
Чек по операции
Итого 3 000 р.
Получатель перевода Ахмед Нажудович К.
Телефон получателя +7 (928) 783-68-00
Банк получателя Т-Банк
Сумма 3 000 р.
Комиссия 0 р.
Карта списания ** 8089
Счет списания ** 0529
ID операции СБП А61780843301900G0G10020011791103
Дата операции 27.06.2026 11:43:33
ПАО «Банк ПСБ»
УСПЕШНО`

	rd := ParseReceipt(text)
	if rd.Bank != "ПСБ" {
		t.Errorf("Bank: ожидали ПСБ, получили %q", rd.Bank)
	}
	if rd.Recipient != "Ахмед Нажудович К." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Amount != 3000 {
		t.Errorf("Amount: ожидали 3000, получили %.0f", rd.Amount)
	}
	if rd.Commission != 0 {
		t.Errorf("Commission: ожидали 0, получили %.0f", rd.Commission)
	}
}

func TestParseRealRSHBReceipt(t *testing.T) {
	// Ключевая особенность РСХБ: сумма "18000 ₽" стоит БЕЗ лейбла в шапке.
	text := `РСХБ
Перевод по номеру телефона через СБП
Исполнен
18000 ₽
02.06.2026, 14:12
Комиссия
0 ₽
Счёт списания
40817...8527
Получатель
Ахмед Нажудович К
Номер телефона получателя
79287836800
Банк получателя
Сбербанк
Идентификатор операции в СБП`

	rd := ParseReceipt(text)
	if rd.Bank != "Россельхозбанк" {
		t.Errorf("Bank: ожидали Россельхозбанк (не Сбербанк из 'банк получателя'!), получили %q", rd.Bank)
	}
	if rd.Recipient != "Ахмед Нажудович К" {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Amount != 18000 {
		t.Errorf("Amount (без лейбла): ожидали 18000, получили %.0f", rd.Amount)
	}
}

func TestParseRealOzonReceipt(t *testing.T) {
	text := `ozon банк
Перевод 26.06.2026 12:51
Итого 40 000 ₽
Статус Успешно
Счёт списания Основной счёт
Сумма 40 000 ₽
Комиссия Без комиссии
Получатель Ахмед Нажудович К.
Телефон получателя +7 (928) 783-68-00
Банк получателя Т-Банк
Отправитель Хазбулат Шамильевич Б.
ID операции А6177095125937290G10080011791103
ООО «ОЗОН БАНК»`

	rd := ParseReceipt(text)
	if rd.Bank != "Озон Банк" {
		t.Errorf("Bank: ожидали Озон Банк, получили %q", rd.Bank)
	}
	if rd.Recipient != "Ахмед Нажудович К." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Sender != "Хазбулат Шамильевич Б." {
		t.Errorf("Sender: получили %q", rd.Sender)
	}
	if rd.Amount != 40000 {
		t.Errorf("Amount: ожидали 40000, получили %.0f", rd.Amount)
	}
	if rd.Status != "Успешно" {
		t.Errorf("Status: получили %q", rd.Status)
	}
}

func TestParseRealTBankReceipt(t *testing.T) {
	// Особенность: логотип Т-Банка — картинка, текстом банк виден только
	// в штампе "АО «ТБАНК»" внизу (слитно, без дефиса).
	text := `Квитанция
22.06.2026 23:03:45
Итого 19 000 ₽
Перевод По номеру телефона
Статус Успешно
Сумма 19 000 ₽
Комиссия 0 ₽
Отправитель Абдурахман Мусаев
Телефон получателя +7 (988) 217-15-56
Получатель Яхит К.
АО «ТБАНК»
Квитанция № 1-118-358-271-976`

	rd := ParseReceipt(text)
	if rd.Bank != "Т-Банк" {
		t.Errorf("Bank: ожидали Т-Банк (по штампу ТБАНК), получили %q", rd.Bank)
	}
	if rd.Recipient != "Яхит К." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Sender != "Абдурахман Мусаев" {
		t.Errorf("Sender: получили %q", rd.Sender)
	}
	if rd.Amount != 19000 {
		t.Errorf("Amount: ожидали 19000, получили %.0f", rd.Amount)
	}
}

func TestParseRealVTBReceipt(t *testing.T) {
	// Особенности ВТБ: "Имя плательщика" (не ФИО), "Сумма операции" внизу,
	// и "Банк получателя: Т-Банк" — банк-эмитент должен определиться по шапке.
	text := `ВТБ
Исходящий перевод СБП
Яхит К.
Статус Выполнено
Дата операции 20.06.2026, 16:04
Счет списания *5064
Имя плательщика Луиза Рамзановна Ю.
Сообщение Долг
Получатель Яхит К.
Телефон получателя +7 (988) 217-15-56
Банк получателя Т-Банк
ID операции в СБП А6171130440642120В1006001
Сумма операции 20 500 ₽
Банк ВТБ (ПАО)
Операция выполнена`

	rd := ParseReceipt(text)
	if rd.Bank != "ВТБ" {
		t.Errorf("Bank: ожидали ВТБ (по шапке, не Т-Банк из 'банк получателя'!), получили %q", rd.Bank)
	}
	if rd.Recipient != "Яхит К." {
		t.Errorf("Recipient: получили %q", rd.Recipient)
	}
	if rd.Sender != "Луиза Рамзановна Ю." {
		t.Errorf("Sender: получили %q", rd.Sender)
	}
	if rd.Amount != 20500 {
		t.Errorf("Amount: ожидали 20500, получили %.0f", rd.Amount)
	}
}

// ==== Извлечение реального времени операции с чека (не времени получения
// сообщения в WhatsApp) — нужно, чтобы отчёт "скинь чеки за 2 июля" смотрел
// на дату/время самой операции, а не на то, когда фото переслали в чат.

func TestParseTxTimeSberSBP(t *testing.T) {
	text := `СБЕРБАНК
Чек по операции
26 июня 2026 22:58:35 (МСК)

Операция
Перевод по СБП

ФИО получателя перевода
Ахмед Нажудович К`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 26, 22, 58, 35, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeRealVTB(t *testing.T) {
	text := `ВТБ
Исходящий перевод СБП
Яхит К.
Статус Выполнено
Дата операции 20.06.2026, 16:04
Получатель Яхит К.
Сумма операции 20 500 ₽`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 20, 16, 4, 0, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeRealPSB(t *testing.T) {
	text := `ПСБ
Чек по операции
Итого 3 000 р.
Получатель перевода Ахмед Нажудович К.
Дата операции 27.06.2026 11:43:33
ПАО «Банк ПСБ»`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 27, 11, 43, 33, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeRealRSHB(t *testing.T) {
	text := `РСХБ
Перевод по номеру телефона через СБП
Исполнен
18000 ₽
02.06.2026, 14:12
Получатель
Ахмед Нажудович К`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 2, 14, 12, 0, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeRealOzon(t *testing.T) {
	text := `ozon банк
Перевод 26.06.2026 12:51
Итого 40 000 ₽
Получатель Ахмед Нажудович К.`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 26, 12, 51, 0, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeRealTBank(t *testing.T) {
	text := `Квитанция
22.06.2026 23:03:45
Итого 19 000 ₽
Получатель Яхит К.`

	rd := ParseReceipt(text)
	if !rd.HasTxTime {
		t.Fatal("ожидали, что время операции будет распознано")
	}
	want := time.Date(2026, time.June, 22, 23, 3, 45, 0, time.Local)
	if !rd.TxTime.Equal(want) {
		t.Errorf("TxTime: ожидали %v, получили %v", want, rd.TxTime)
	}
}

func TestParseTxTimeAbsentWhenNoDateOnReceipt(t *testing.T) {
	// Альфа-Банк и часть чеков вообще не печатают дату/время на скриншоте —
	// в этом случае HasTxTime должен остаться false, а не подхватить
	// случайное число похожее на дату.
	text := `Альфа-Банк

Получатель
Хадижат Имрановна С.

Сколько
30 000 Р

Номер документа
20260630151235bbbbef30812e14c5c9fd5b`

	rd := ParseReceipt(text)
	if rd.HasTxTime {
		t.Errorf("не ожидали найти время операции, получили %v", rd.TxTime)
	}
}

// TestBankOrderMatchesMarkers защищает инвариант: каждый банк из bankOrder
// есть в bankMarkers. Иначе detectBank обратится к nil-регекспу и упадёт в
// проде на первом же чеке. Дешёвый предохранитель от опечатки в списке.
func TestBankOrderMatchesMarkers(t *testing.T) {
	for _, b := range bankOrder {
		if bankMarkers[b] == nil {
			t.Fatalf("банк %q есть в bankOrder, но нет в bankMarkers — detectBank упадёт", b)
		}
	}
	if len(bankOrder) != len(bankMarkers) {
		t.Fatalf("bankOrder(%d) и bankMarkers(%d) рассинхронизированы", len(bankOrder), len(bankMarkers))
	}
}

// TestDetectRussianBanks проверяет распознавание расширенного списка банков —
// в т.ч. новых (МКБ, ОТП, Точка, ЮMoney) и латинских написаний.
func TestDetectRussianBanks(t *testing.T) {
	cases := map[string]string{
		"Перевод через Сбербанк":         "Сбербанк",
		"SberBank Online":                "Сбербанк",
		"Московский кредитный банк":      "МКБ",
		"МКБ, перевод":                   "МКБ",
		"ОТП Банк":                       "ОТП Банк",
		"Банк Точка":                     "Точка",
		"Перевод ЮMoney":                 "ЮMoney",
		"Alfa-Bank transfer":            "Альфа-Банк",
		"VTB":                            "ВТБ",
		"Газпромбанк":                    "Газпромбанк",
		"Райффайзен банк":                "Райффайзен",
	}
	for text, want := range cases {
		if got := detectBank(text); got != want {
			t.Errorf("detectBank(%q) = %q, ожидали %q", text, got, want)
		}
	}
}

// TestGenericBankCatch — любой банк «…банк», даже не из списка, распознаётся,
// а служебные слова вроде «интернет-банк» — нет.
func TestGenericBankCatch(t *testing.T) {
	if got := detectBank("Перевод через Мособлбанк на карту"); got != "Мособлбанк" {
		t.Errorf("Мособлбанк не распознан: %q", got)
	}
	if got := detectBank("ПАО «Тимербанк», квитанция"); got != "Тимербанк" {
		t.Errorf("Тимербанк не распознан: %q", got)
	}
	if got := detectBank("Оплата через интернет-банк"); got != "" {
		t.Errorf("интернет-банк не банк, а вернулось %q", got)
	}
	// Явно перечисленный банк всё равно даёт каноничное имя из списка.
	if got := detectBank("Сбербанк Онлайн"); got != "Сбербанк" {
		t.Errorf("Сбербанк: %q", got)
	}
}
