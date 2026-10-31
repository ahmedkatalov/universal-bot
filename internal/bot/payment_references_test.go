package bot

import (
	"context"
	"reflect"
	"testing"

	"whatsapp-bot/internal/parser"
)

func TestCleanReceiptNameRejectsPhrases(t *testing.T) {
	b := &Bot{aliases: parser.NewAliasMap(), botName: "Джарвис"}
	for _, text := range []string{"Доброе утро!", "Добрый день всем", "Салам алейкум брат", "Карта Хадижат", "с карты Пияна перевёл"} {
		if name, _ := b.payerFromCaption(context.Background(), text); name != "" {
			t.Errorf("фраза %q принята за клиента %q", text, name)
		}
	}
	if name, _ := b.payerFromCaption(context.Background(), "Умаров Салам"); name != "Умаров Салам" {
		t.Errorf("настоящее имя ошибочно отброшено: %q", name)
	}
	for _, text := range []string{"Шошуков Руслан 22т ✅ За 2 месяца", "Шошуков Руслан\nДоговор закрыт\n22.000 ₽"} {
		if name, amt := b.payerFromCaption(context.Background(), text); name != "Шошуков Руслан" || amt != 22000 {
			t.Errorf("подпись %q: клиент %q, сумма %v", text, name, amt)
		}
	}
	if name, _ := b.payerFromCaption(context.Background(), "Умаров Салам\nИсаев Харон"); name != "" {
		t.Errorf("два разных ФИО склеены в клиента %q", name)
	}
}

func TestPaymentReferenceAnchors(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []float64
	}{
		{"Договор 983", nil},
		{"Договор №983/2026", nil},
		{"Договор 941\n20.100 наличку взял Сафаи", []float64{20100}},
		{"договор 99999 5000", []float64{5000}},
		{"тел. +7 (928) 123-45-67, оплата 15000", []float64{15000}},
		{"тел. 8 928 123 45 67, оплата 15000", []float64{15000}},
		{"Исаев Харон Д. 15000", []float64{15000}},
	} {
		if got := moneyTokens(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("moneyTokens(%q) = %v, ожидали %v", tc.text, got, tc.want)
		}
	}
}

func TestReceiptNameWithReferenceWithoutAI(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, sender := e2eGroup("ref-name"), user("79281119901")
	waID := e.sendReceipt(grp, sender, "Сафаи", sberCheck("20000.00", "19:18:40"), "")
	for _, text := range []string{"Доброе утро", "Новый клиент", "Карта Хадижат", "с карты Пияна перевёл"} {
		e.sendGroup(groupText(grp, sender, "Сафаи", text))
		if r := e.db.TestReceiptByMessage(context.Background(), waID); r.Confirmed {
			t.Fatalf("фраза %q стала клиентом чека: %q", text, r.Recipient)
		}
	}
	e.sendGroup(groupText(grp, sender, "Сафаи", "Мусаев Рахимжан\nНомер договора №983/2026"))
	r := e.db.TestReceiptByMessage(context.Background(), waID)
	if !r.Confirmed || r.Recipient != "Мусаев Рахимжан" || r.Amount != 20000 {
		t.Errorf("подпись с договором не привязалась к чеку: %+v", r)
	}
	if got := dayTotals(e, grp); len(got) != 1 || got["Мусаев Рахимжан"] != 20000 {
		t.Errorf("номер договора изменил сбор: %v", got)
	}
}
