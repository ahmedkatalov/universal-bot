package bot

import "testing"

import "whatsapp-bot/internal/db"

func TestDupKey(t *testing.T) {
	// Разный порядок слов, регистр и инициалы — один ключ; разные фамилии — разные.
	same := []string{"Хадаев Али", "Али Хадаев", "хадаев али", "Хадаев Али К."}
	k0 := dupKey(same[0])
	for _, s := range same[1:] {
		if dupKey(s) != k0 {
			t.Errorf("dupKey(%q)=%q, ожидали как у %q (%q)", s, dupKey(s), same[0], k0)
		}
	}
	if dupKey("Ахмед Катаев") == dupKey("Ахмед Висаев") {
		t.Errorf("разные фамилии не должны давать один ключ")
	}
}

func TestDuplicateClusters(t *testing.T) {
	stats := []db.ContactStat{
		{Name: "Хадаев Али", Records: 5},
		{Name: "Али Хадаев", Records: 1},
		{Name: "Ахмед Катаев", Records: 3}, // одиночка — не дубль
		{Name: "Сулейманов Иса К.", Records: 2},
		{Name: "иса сулейманов", Records: 4},
	}
	clusters := duplicateClusters(stats)
	if len(clusters) != 2 {
		t.Fatalf("ожидали 2 группы дублей, получили %d: %v", len(clusters), clusters)
	}
	// В каждой группе первым идёт тот, у кого больше записей (его оставляем).
	for _, g := range clusters {
		if len(g) < 2 {
			t.Errorf("группа дублей должна быть из 2+: %v", g)
		}
		for i := 1; i < len(g); i++ {
			if g[0].Records < g[i].Records {
				t.Errorf("первым должен идти контакт с бОльшим числом записей: %v", g)
			}
		}
	}
}
