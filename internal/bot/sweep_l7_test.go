package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/db"

	"go.mau.fi/whatsmeow/types"
)

var _ = db.BankReceiptInput{}
var _ = types.JID{}

// Регрессии 7-го раунда (2026-10-23): чтение настоящих чеков и «по дате в чате».

func TestL7_00_BalanceNotCountedAsAmount(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, snd := e2eGroup("realocr-bal"), user("79990001111")
	ocr := "Т-Банк\n" +
		"Квитанция\n" +
		"Сумма списания\n" +
		"15 000 ₽\n" +
		"Доступно на счёте\n" +
		"248 500 ₽\n" +
		"Получатель\n" +
		"Иса Джабраилов\n" +
		"Статус\n" +
		"Выполнено\n" +
		"15.10.2026 14:05"
	ck := e.sendReceipt(grp, snd, "Сафаи", ocr, "Ахмед Каталов")
	r := e.db.TestReceiptByMessage(context.Background(), ck)
	if !r.Found {
		t.Fatalf("чек не сохранён")
	}
	if r.Amount != 15000 {
		t.Fatalf("в сбор ушло %.0f ₽ вместо 15000 — взят баланс/списание, а не сумма перевода (recip=%q confirmed=%v)",
			r.Amount, r.Recipient, r.Confirmed)
	}
}

func TestL7_01_DeclinedBannerNotCounted(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, snd := e2eGroup("realocr-dec"), user("79990002222")
	ocr := "СберБанк\n" +
		"Операция отклонена\n" +
		"Перевод клиенту СберБанка\n" +
		"Сумма перевода\n" +
		"50 000 ₽\n" +
		"ФИО получателя\n" +
		"Муса Камилов\n" +
		"Номер документа 987654321\n" +
		"15.10.2026 10:00:00"
	e.sendReceipt(grp, snd, "Сафаи", ocr, "Ахмед Каталов")
	from := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.Local)
	ss, err := e.db.SummaryForPeriod(context.Background(), from, from.AddDate(0, 0, 1), []string{grp.String()})
	if err != nil {
		t.Fatal(err)
	}
	var got float64
	for _, x := range ss {
		got += x.Total
	}
	if got != 0 {
		t.Fatalf("отклонённый перевод засчитан в сбор: %.0f ₽, ожидали 0", got)
	}
}

func TestL7_02_TwoTransfersOneScreenshotAsked(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp, snd := e2eGroup("realocr-two"), user("79990004444")
	ocr := "СберБанк\nИстория операций\nПеревод по СБП\nСумма перевода\n15 000 ₽\nФИО получателя\nИса Джабраилов\n15.10.2026 10:00:00\n" +
		"Перевод по СБП\nСумма перевода\n20 000 ₽\nФИО получателя\nИса Джабраилов\n15.10.2026 10:05:00"
	n := len(e.sent())
	e.sendReceipt(grp, snd, "Сафаи", ocr, "Ахмед Каталов")
	from := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.Local)
	ss, err := e.db.SummaryForPeriod(context.Background(), from, from.AddDate(0, 0, 1), []string{grp.String()})
	if err != nil {
		t.Fatal(err)
	}
	var got float64
	for _, x := range ss {
		got += x.Total
	}
	if got != 0 {
		t.Fatalf("скриншот с двумя переводами молча посчитан (%.0f ₽) — должен уйти на проверку с вопросом", got)
	}
	asked := false
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "несколько переводов") || strings.Contains(m.Text, "каждый перевод отдельным") {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("бот не попросил прислать переводы по одному: %v", e.sentSince(n))
	}
}

func TestL7_04_ByChatNoDocCopyNotDoubleDaily(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	g1, g2 := e2eGroup("g1"), e2eGroup("g2")
	ctx := context.Background()

	noDoc := func(grp types.JID, sender, client string, amount float64, opDate, postedAt time.Time) {
		cid, err := e.db.GetOrCreateContact(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		rawID, _, err := e.db.SaveRawMessage(ctx, e2eID("CHK"), grp.String(), user(sender).String(), sender, client, "", true, "", postedAt)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{
			RawMessageID: rawID, ContactID: &cid, Amount: amount, GroupJID: grp.String(),
			TxDate: opDate, Status: "ok", ClientConfirmed: true, RecipientRaw: client,
			Bank: "сбер", CardOwner: "Хадижат Ибрагимовна И.", SenderRaw: "Плательщик Петров П.",
		}); err != nil {
			t.Fatal(err)
		}
	}
	byChat := func(fromDay, toDay time.Time) float64 {
		to := toDay.AddDate(0, 0, 1)
		ms := time.Date(fromDay.Year(), fromDay.Month(), 1, 0, 0, 0, 0, time.Local)
		me := time.Date(toDay.Year(), toDay.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, 1, 0)
		ss, err := e.db.SummaryForPeriodByChat(ctx, fromDay, to, ms, me, nil)
		if err != nil {
			t.Fatal(err)
		}
		var s float64
		for _, x := range ss {
			s += x.Total
		}
		return s
	}

	opTime := time.Date(2026, 10, 2, 12, 34, 0, 0, time.Local)
	noDoc(g1, "79001110001", "Магомедов Ахмед", 15000, opTime, time.Date(2026, 10, 3, 9, 0, 0, 0, time.Local))
	noDoc(g2, "79001110002", "Магомедов Ахмед", 15000, opTime, time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local))

	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	oct31 := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local)

	if ss, err := e.db.SummaryForPeriod(ctx, oct1, oct31.AddDate(0, 0, 1), nil); err != nil {
		t.Fatal(err)
	} else {
		var s float64
		for _, x := range ss {
			s += x.Total
		}
		if s != 15000 {
			t.Fatalf("operation-mode control: expected one 15000 payment, got %.0f", s)
		}
	}

	rngSum := byChat(oct1, oct31)
	var byDay float64
	for d := 0; d < 31; d++ {
		day := oct1.AddDate(0, 0, d)
		byDay += byChat(day, day)
	}
	if byDay != rngSum {
		t.Errorf("«по дате в чате»: month-range сбор %.0f, but summing daily сборы gives %.0f — the no-doc check re-posted to a second group is counted twice day-by-day", rngSum, byDay)
	}
}
