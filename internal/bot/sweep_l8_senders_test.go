package bot

import (
	"context"
	"testing"
	"time"
)

// Отчёт «кто сколько скинул»: отправитель находится по номеру в ЛЮБОМ формате
// (8928… против хранимого 7928…), сумма разбита на чеки/наличку, имя — из
// памяти номеров, если номер сохранён.
func TestL8_SendersByPhoneAndBreakdown(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("who")
	phone := "79287836800"
	u := user(phone)
	if err := e.db.SetPhoneOwner(ctx, phone, "Ахмед Сборщик"); err != nil {
		t.Fatal(err)
	}
	// Два чека по 20000 (с подписью клиента — попадают в сбор) и наличка 5000.
	e.sendReceipt(grp, u, "Ахмед Сборщик", sberCheck("20000.00", "10:00:00"), "Магомедов Ахмед")
	e.sendReceipt(grp, u, "Ахмед Сборщик", sberCheck("20000.00", "11:00:00"), "Исаев Харон")
	e.sendGroup(groupText(grp, u, "Ахмед Сборщик", "Нал 5000"))
	e.waitIdle()

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 0, 1)

	// Номер в «народном» формате 8928… — должен найти контакт 7928…
	for _, q := range []string{"89287836800", "+7 928 783 68 00", "9287836800"} {
		stats, err := e.db.SenderStats(ctx, from, to, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(stats) != 1 {
			t.Fatalf("запрос %q: строк %d, ожидали 1 (%+v)", q, len(stats), stats)
		}
		s := stats[0]
		if s.Name != "Ахмед Сборщик" {
			t.Errorf("запрос %q: имя %q, ожидали «Ахмед Сборщик» (из памяти номеров)", q, s.Name)
		}
		if s.Total != 45000 || s.ChecksSum != 40000 || s.Checks != 2 || s.CashSum != 5000 || s.Cash != 1 {
			t.Errorf("запрос %q: всего %.0f (чеки %.0f×%d, наличка %.0f×%d), ожидали 45000 (чеки 40000×2, наличка 5000×1)",
				q, s.Total, s.ChecksSum, s.Checks, s.CashSum, s.Cash)
		}
	}

	// По имени тоже находит.
	if st, err := e.db.SenderStats(ctx, from, to, nil, "ахмед сборщик"); err != nil || len(st) != 1 || st[0].Total != 45000 {
		t.Errorf("поиск по имени: %v / %+v", err, st)
	}
}
