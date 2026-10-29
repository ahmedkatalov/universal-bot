package bot

import (
	"fmt"
	"strings"
	"testing"
)

// Реальные последовательности из группы «Оплата КЛНТ» (скриншоты владельца,
// 27.10.2026): один отправитель пересылает подряд ФОТО ЧЕКА, а ПОТОМ имя
// клиента; между ними — наличные заметки «ФИО 21,400 Нал взял Мансур».
// Бот должен по порядку понять, какой чек какого клиента.
func TestREAL_ScreenshotSequences(t *testing.T) {
	for _, withAI := range []bool{false, true} {
		t.Run(fmt.Sprintf("ai=%v", withAI), func(t *testing.T) {
			e := newE2E(t, e2eOpts{WithAI: withAI, Admins: []string{ownerPhone}})
			if withAI {
				e.ai = aiNamesAndPayments("Мусаев Рахимжан", "Альмурзаева Марха Абдулмажидовна", "Матаев Алихан",
					"Ибрагимов Аюб", "Керимов Турпал-Али Рашидович", "Шыйтуркаев Тамерлан", "Ханжиев Нурид")
			}
			grp := e2eGroup("klnt")
			me := user(ownerPhone) // «Вы» — владелец пересылает

			// --- скриншот 1 (вечер) ---
			e.sendReceipt(grp, me, "Вы", sberCheck("20000.00", "19:18:40"), "")
			e.sendGroup(groupText(grp, me, "Вы", "Мусаев Рахимжан\nДоговор 983"))
			e.sendReceipt(grp, me, "Вы", sberCheck("10000.00", "18:25:19"), "")
			e.sendGroup(groupText(grp, me, "Вы", "Альмурзаева Марха Абдулмажидовна"))
			e.sendGroup(groupText(grp, me, "Вы", "Матаев Алихан\n21,400 Нал взял Мансур"))

			// --- скриншот 2 (день) ---
			e.sendReceipt(grp, me, "Вы", sberCheck("15000.00", "13:57:10"), "")
			e.sendGroup(groupText(grp, me, "Вы", "ибрагимов аюб"))
			e.sendGroup(groupText(grp, me, "Вы", "Керимов Турпал-Али Рашидович\n36.800 нал взял МБ\nДоговор Закрыл"))
			e.sendReceipt(grp, me, "Вы", sberCheck("25000.00", "21:36:09"), "")
			e.sendGroup(groupText(grp, me, "Вы", "Шыйтуркаев Тамерлан 25т"))
			e.sendGroup(groupText(grp, me, "Вы", "Ханжиев Нурид\nДоговор 941\n20.100 наличку взял Сафаи\nЗакрыт"))
			e.waitIdle()

			want := map[string]float64{
				"Мусаев Рахимжан":                  20000,
				"Альмурзаева Марха Абдулмажидовна": 10000,
				"Матаев Алихан":                    21400,
				"Ибрагимов Аюб":                    15000,
				"Керимов Турпал-Али Рашидович":     36800,
				"Шыйтуркаев Тамерлан":              25000,
				"Ханжиев Нурид":                    20100,
			}
			got := dayTotals(e, grp)
			for name, amt := range want {
				found := false
				for g, v := range got {
					if strings.EqualFold(strings.TrimSpace(g), name) && v == amt {
						found = true
					}
				}
				if !found {
					t.Errorf("ОШИБКА: %s должен быть %v, по факту сбор: %v", name, amt, got)
				}
			}
			if s := sum(got); s != 148300 {
				t.Errorf("итог сбора %v, хочу 148300 (без двойного счёта/потерь): %v", s, got)
			}
			t.Logf("sent by bot (%d):", len(e.sent()))
			for _, m := range e.sent() {
				t.Logf("  BOT> %s", strings.ReplaceAll(m.Text, "\n", " | "))
			}
		})
	}
}
