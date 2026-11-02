package parser

import (
	"testing"
	"time"
)

// Fields transcribed from the forwarded Sber receipt in the November 1 report.
// Preserve the printed line breaks and the client caption above the bank text.
func TestForwardedSberReceiptNovember1(t *testing.T) {
	text := `Ханатаев акраман
СБЕРБАНК
Чек по операции
1 ноября 2026 14:23:06(МСК)

ФИО получателя
Милана Нажудовна К.

ФИО отправителя
Элина Зилаудиевна Г.

Сумма перевода
30000.00 ₽

Комиссия
0.00 ₽

Номер документа
1000000006086701079

Код авторизации
024307`
	if !LooksLikeBankReceipt(text) {
		t.Fatal("the complete Sber receipt was not recognized as bank evidence")
	}
	rd := ParseReceipt(text)
	if rd.Amount != 30000 || rd.Commission != 0 || rd.Bank != "Сбербанк" {
		t.Fatalf("wrong bank or money fields: %+v", rd)
	}
	if rd.Recipient != "Милана Нажудовна К." || rd.Sender != "Элина Зилаудиевна Г." {
		t.Fatalf("caption replaced printed bank parties: %+v", rd)
	}
	if rd.DocNumber != "1000000006086701079" || rd.AuthCode != "024307" {
		t.Fatalf("operation identity was lost: %+v", rd)
	}
	if !rd.HasTxTime || rd.TimeDefaulted || rd.TxTime.Format("2006-01-02 15:04:05") != "2026-11-01 14:23:06" {
		t.Fatalf("wrong printed transaction time: %+v", rd)
	}
	_, offset := rd.TxTime.Zone()
	if offset != 3*int(time.Hour/time.Second) {
		t.Fatalf("printed Moscow timezone was lost: %v", rd.TxTime)
	}
}
