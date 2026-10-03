package bot

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

// Регрессии третьего раунда враждебной проверки сверки.

func TestReconR3Search(t *testing.T) {
	// 1. Две комбинации, которые помогают только вместе.
	v, left, _ := hm([]reconItem{chk(4000, aug(4)), chk(6000, aug(10))},
		[]cmf.Payment{pay(2000, dOnly(8, 5)), pay(2000, dOnly(8, 5)), pay(2000, dOnly(8, 11)), pay(4000, dOnly(8, 11))})
	wantStatuses(t, "#1 пара", v, stSplit, stSplit)
	wantLeft(t, "#1 пара", left)
	v, left, _ = hm([]reconItem{chk(10000, aug(1)), chk(5000, aug(1)), chk(15000, aug(20))},
		[]cmf.Payment{pay(15000, dOnly(8, 2)), pay(10000, dOnly(8, 21)), pay(5000, dOnly(8, 21))})
	wantStatuses(t, "#1 P01", v, stCombined, stCombined, stSplit)
	wantLeft(t, "#1 P01", left)

	// 2. Ряд одинаковых чеков, каждый внесён двумя частями.
	v, left, _ = hm([]reconItem{chk(10000, aug(15)), chk(10000, aug(16)), chk(10000, aug(17))},
		[]cmf.Payment{pay(2000, dOnly(8, 18)), pay(8000, dOnly(8, 18)), pay(2000, dOnly(8, 19)), pay(8000, dOnly(8, 19)), pay(2000, dOnly(8, 20)), pay(8000, dOnly(8, 20))})
	wantStatuses(t, "#2", v, stSplit, stSplit, stSplit)
	wantLeft(t, "#2", left)
	v, left, _ = hm([]reconItem{chk(10000, aug(15)), chk(10000, aug(17))},
		[]cmf.Payment{pay(2000, dOnly(8, 18)), pay(8000, dOnly(8, 18)), pay(2000, dOnly(8, 22)), pay(8000, dOnly(8, 22))})
	wantStatuses(t, "#2b", v, stSplit, stSplit)
	wantLeft(t, "#2b", left)

	// 3. Раньше выбранная комбинация не мешает двум лучшим.
	v, left, _ = hm([]reconItem{chk(8000, aug(3)), chk(8000, aug(6)), chk(8000, aug(8)), chk(2000, aug(15))},
		[]cmf.Payment{pay(16000, dOnly(8, 9)), pay(10000, dOnly(8, 17))})
	for i, x := range v {
		if !x.entered() {
			t.Errorf("#3: позиция %d статус %v", i, x.Status)
		}
	}
	wantLeft(t, "#3", left)
	v, left, _ = hm([]reconItem{chk(10000, aug(1)), chk(5000, aug(3)), chk(10000, aug(5))},
		[]cmf.Payment{pay(7000, dOnly(8, 1)), pay(3000, dOnly(8, 1)), pay(15000, dOnly(8, 3))})
	wantStatuses(t, "#3 U01", v, stSplit, stCombined, stCombined)
	wantLeft(t, "#3 U01", left)

	// 4. Одна и та же комбинация по двум датам не вытесняет нужную.
	v, left, _ = hm([]reconItem{chk(12000, aug(1)), chk(3000, aug(1)), chk(10000, aug(12)), chk(5000, aug(12))},
		[]cmf.Payment{pay(10000, dOnly(8, 12)), pay(5000, dOnly(8, 12)), {Amount: 15000, PaidAt: dOnly(8, 12), CreatedAt: at(8, 13, 10)}})
	wantStatuses(t, "#4", v, stCombined, stCombined, stEntered, stEntered)
	wantLeft(t, "#4", left)

	// 5. Комбинация, все участники которой уже заняты 1:1, тоже рассматривается.
	v, left, _ = hm([]reconItem{chk(2000, mon(8, 11)), chk(2000, mon(8, 11)), chk(10000, mon(8, 12)), chk(2000, mon(8, 13))},
		[]cmf.Payment{pay(2000, dOnly(8, 11)), pay(2000, dOnly(8, 13)), pay(12000, dOnly(8, 13))})
	for i, x := range v {
		if !x.entered() {
			t.Errorf("#5: позиция %d статус %v", i, x.Status)
		}
	}
	wantLeft(t, "#5", left)
}

func TestReconR3Dates(t *testing.T) {
	// 6. «Проверь дату» по дате внесения — правильные дата и число дней, без «поправь».
	items := []reconItem{chk(15000, at(8, 10, 12))}
	pays := []cmf.Payment{{Amount: 15000, PaidAt: dOnly(9, 29), CreatedAt: time.Date(2026, 8, 5, 10, 0, 0, 0, msk)}}
	v, _, k := hm(items, pays)
	wantStatuses(t, "#6", v, stDateCheck)
	if s := describeVerdict(items, pays, v, 0, k); strings.Contains(s, "на -") || !strings.Contains(s, "05.08") || !strings.Contains(s, "на 5 дн.") || strings.Contains(s, "поправь") {
		t.Errorf("#6: %q", s)
	}

	// 7. Даты со смещением и в миллисекундах читаются; нечитаемая дата — не «любая».
	var p cmf.Payment
	if err := json.Unmarshal([]byte(`{"amount":10000,"paid_at":"2026-06-20 12:00:00+03:00","created_at":"2026-06-20 12:05:00+03:00"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.PaidAt.IsZero() {
		t.Error("#7: дата со смещением не прочитана")
	}
	v, _, _ = hm([]reconItem{chk(10000, at(8, 10, 12))}, []cmf.Payment{p})
	if v[0].entered() {
		t.Errorf("#7: июньская оплата закрыла августовский чек: %v", v[0].Status)
	}
	var pm cmf.Payment
	_ = json.Unmarshal([]byte(`{"amount":10000,"paid_at":1786311000000}`), &pm)
	if pm.PaidAt.Year() != 2026 {
		t.Errorf("#7: миллисекунды: %v", pm.PaidAt)
	}
	var pu cmf.Payment
	_ = json.Unmarshal([]byte(`{"amount":10000,"paid_at":"вчера"}`), &pu)
	v, _, _ = hm([]reconItem{chk(10000, at(8, 10, 12))}, []cmf.Payment{pu})
	if !pu.DateUnknown || v[0].entered() {
		t.Errorf("#7: нечитаемая дата: unknown=%v статус %v", pu.DateUnknown, v[0].Status)
	}

	// 8. Запись внесли в программу ДО чека — это не «внесён».
	items = []reconItem{chk(15000, at(8, 10, 12))}
	pays = []cmf.Payment{{Amount: 15000, PaidAt: dOnly(7, 1), CreatedAt: time.Date(2026, 8, 8, 10, 0, 0, 0, msk)}}
	v, _, _ = hm(items, pays)
	if v[0].entered() {
		t.Errorf("#8: %v", v[0].Status)
	}
}

func TestReconR3Reversals(t *testing.T) {
	// 9. Сторно отменяет запись прямо перед ним, а не двухмесячной давности.
	v, left, _ := hm(
		[]reconItem{ctxChk(12000, mon(6, 3)), ctxChk(12000, mon(7, 3)), chk(12000, aug(3))},
		[]cmf.Payment{
			{Amount: 12000, PaidAt: dOnly(6, 4), ContractID: "c1"},
			{Amount: 12000, PaidAt: dOnly(7, 4), ContractID: "c1"},
			{Amount: 12000, PaidAt: dOnly(8, 4), ContractID: "c1"},
			{Amount: -12000, PaidAt: dOnly(8, 5), ContractID: "c1"}})
	wantStatuses(t, "#9", v, stEntered, stEntered, stNotEntered)
	wantLeft(t, "#9", left)

	// 10. Частичная корректировка не пропадает, если сторно отменило другую запись.
	v, _, _, net := humanMatchNet([]reconItem{chk(15000, aug(4))}, []cmf.Payment{
		{Amount: 15000, PaidAt: dOnly(8, 5), ContractID: "c1"},
		{Amount: 15000, PaidAt: dOnly(8, 6), ContractID: "c1"},
		{Amount: -15000, PaidAt: dOnly(8, 7), ContractID: "c1"},
		{Amount: -5000, PaidAt: dOnly(8, 8), ContractID: "c1"}}, unitsAuto)
	if v[0].Status == stEntered && v[0].Note == "" {
		t.Errorf("#10: в программе нетто 10 000, а «внесён»; net=%v", net)
	}

	// 11. Списание на сумму двух записей подряд отменяет обе.
	pays := []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(8, 11), ContractID: "c1"},
		{Amount: 5000, PaidAt: dOnly(8, 11), ContractID: "c1"},
		{Amount: -15000, PaidAt: dOnly(8, 12), ContractID: "c1"}}
	v, _, _ = hm([]reconItem{chk(10000, aug(10)), chk(5000, aug(10))}, pays)
	wantStatuses(t, "#11", v, stNotEntered, stNotEntered)
	v, _, _ = hm([]reconItem{ctxChk(20000, mon(7, 19)), chk(10000, aug(10)), chk(5000, aug(10))},
		append([]cmf.Payment{{Amount: 20000, PaidAt: dOnly(7, 20), ContractID: "c1"}}, pays...))
	wantStatuses(t, "#11b", v, stEntered, stNotEntered, stNotEntered)
	// Списание, которое не к чему отнести, видно отдельно.
	r := humanMatchFull([]reconItem{chk(10000, aug(10))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(8, 11), ContractID: "c1"},
		{Amount: -15000, PaidAt: dOnly(8, 12), ContractID: "c1"}}, unitsAuto)
	if len(r.Stray) != 1 || r.Stray[0] != 1 {
		t.Errorf("#11c: stray %v", r.Stray)
	}

	// 12. Три сторно — никакой лимит вариантов не теряет правильный.
	c := func(a int64, d time.Time) cmf.Payment { return cmf.Payment{Amount: a, PaidAt: d, ContractID: "c1"} }
	v, left, _ = hm([]reconItem{chk(10000, mon(5, 20)), ctxChk(10000, mon(6, 20)), ctxChk(10000, aug(5))}, []cmf.Payment{
		c(10000, dOnly(5, 21)), c(10000, dOnly(6, 21)),
		c(10000, dOnly(8, 6)), c(10000, dOnly(8, 6)), c(10000, dOnly(8, 6)), c(10000, dOnly(8, 6)),
		c(-10000, dOnly(8, 7)), c(-10000, dOnly(8, 7)), c(-10000, dOnly(8, 7))})
	wantStatuses(t, "#12", v, stEntered, stEntered, stEntered)
	wantLeft(t, "#12", left)

	// 13. Корректировка относится к той записи, при которой всё сходится.
	v, left, _ = hm([]reconItem{chk(10000, aug(10)), chk(20000, aug(12))}, []cmf.Payment{
		pay(15000, dOnly(8, 11)), pay(20000, dOnly(8, 12)), pay(-5000, dOnly(8, 13))})
	wantStatuses(t, "#13", v, stEntered, stEntered)
	wantLeft(t, "#13", left)

	// 14. «Сделана раньше» — по времени внесения: сторно отменяет запись с опечаткой.
	v, left, _ = hm([]reconItem{chk(10000, aug(10))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(9, 11), CreatedAt: at(8, 11, 10), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 12), CreatedAt: at(8, 12, 10), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 11), CreatedAt: at(8, 12, 9), ContractID: "c1"}})
	wantStatuses(t, "#14", v, stEntered)
	wantLeft(t, "#14", left)
	if v[0].Note != "" || len(v[0].Pays) != 1 || v[0].Pays[0] != 2 {
		t.Errorf("#14: %+v", v[0])
	}
}

func TestReconR3Names(t *testing.T) {
	t0 := time.Date(2026, 8, 10, 11, 32, 0, 0, time.UTC)
	// 15. Неподтверждённая копия не склеивает двух разных подтверждённых клиентов.
	rs := []db.ReconReceipt{
		{ID: 1, Name: "Ахмед Нажудович К.", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A", NeedsReview: true},
		{ID: 2, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0.Add(20 * time.Second), GroupJID: "g2", WaMessageID: "B"},
		{ID: 3, Name: "Магомедов Руслан", Amount: 5000, TxDate: t0.Add(60 * time.Second), GroupJID: "g3", WaMessageID: "C"},
	}
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}} {
		var in []db.ReconReceipt
		for _, k := range order {
			in = append(in, rs[k])
		}
		if out := dedupeReconReceipts(in); len(out) != 2 {
			t.Errorf("#15 %v: позиций %d, ожидали 2", order, len(out))
		}
	}
	// 16. «Магомед-Расул» — не «Магомед».
	no := [][2]string{
		{"Исаев Магомед-Расул Ахмедович", "Исаев Магомед Ахмедович"},
		{"Исаев Магомед-Расул", "Исаев Магомед"},
		{"Магомедов Магомед-Али", "Магомедов Магомед"},
		{"Алиев Абдул-Керим", "Алиев Абдул"},
		{"Исаев Камиль", "Исаева Камила"},
	}
	for _, c := range no {
		if sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=true, ожидали false", c[0], c[1])
		}
	}
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Исаев Магомед", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Исаев Магомед-Расул", Amount: 5000, TxDate: t0.Add(30 * time.Second), GroupJID: "g2", WaMessageID: "B"},
	}); len(out) != 2 {
		t.Errorf("#16: позиций %d, ожидали 2", len(out))
	}
	yes := [][2]string{
		// 17. Составное имя через пробел.
		{"Магомедов Хаджимурат", "Магомедов Хаджи Мурат"},
		{"Хаджи Мурат Магомедов", "Магомедов Хаджимурат Ахмедович"},
		{"Алиев Абдулкерим", "Абдул Керим Алиев"},
		{"Магомедов Хаджи-Мурат", "Магомедов Хаджи Мурат"},
		// 18. й/и.
		{"Гусейнов Хусейн", "Гусеинов Хусеин"},
		{"Магомедова Айшат", "Магомедова Аишат"},
		{"Майоров Сергей", "Маиоров Сергей"},
		// 19. Падежи имён на «ь».
		{"Исаева Шамиля", "Исаев Шамиль"},
		{"Исаеву Шамилю", "Исаев Шамиль Магомедович"},
		{"Исаевым Шамилем", "Исаев Шамиль"},
		{"Петрова Игоря", "Петров Игорь"},
	}
	for _, c := range yes {
		if !sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=false, ожидали true", c[0], c[1])
		}
	}
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Магомедов Хаджимурат", Amount: 7000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Магомедов Хаджи Мурат", Amount: 7000, TxDate: t0.Add(30 * time.Second), GroupJID: "g2", WaMessageID: "B"},
	}); len(out) != 1 {
		t.Errorf("#17: позиций %d, ожидали 1", len(out))
	}
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Гусейнов Хусейн", Amount: 7000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Гусеинов Хусеин", Amount: 7000, TxDate: t0, GroupJID: "g2", WaMessageID: "B"},
	}); len(out) != 1 {
		t.Errorf("#18: позиций %d, ожидали 1", len(out))
	}
}

func TestReconR3LockIn(t *testing.T) {
	// 20. Ложное «частями» не забирает точное совпадение в тот же день.
	items := []reconItem{
		chk(5000, mon(8, 27)), chk(5000, mon(9, 3)), chk(7000, mon(7, 7)),
		chk(7000, mon(9, 1)), chk(3000, mon(9, 6)),
	}
	pays := []cmf.Payment{
		{Amount: 2500, PaidAt: dOnly(8, 28), ContractID: "A"}, {Amount: 2500, PaidAt: dOnly(8, 28), ContractID: "A"},
		{Amount: 5000, PaidAt: dOnly(9, 3), ContractID: "A"},
		{Amount: 3500, PaidAt: dOnly(7, 8), ContractID: "B"}, {Amount: 3500, PaidAt: dOnly(7, 9), ContractID: "B"},
		{Amount: 3500, PaidAt: dOnly(9, 6), ContractID: "B"}, {Amount: 3500, PaidAt: dOnly(9, 7), ContractID: "B"},
		{Amount: 1500, PaidAt: dOnly(9, 14), ContractID: "C"}, {Amount: 1500, PaidAt: dOnly(9, 15), ContractID: "C"},
	}
	v, left, _ := hm(items, pays)
	wantStatuses(t, "#20", v, stSplit, stEntered, stSplit, stSplit, stSplit)
	wantLeft(t, "#20", left)

	// 21. Ежедневная мелкая наличка не мешает найти 7000+8000, внесённые через 8–9 дней.
	for _, pd := range []int{19, 20} {
		items, pays = nil, nil
		for d := 1; d <= 31; d++ {
			items = append(items, cashIt(500, mon(8, d), false))
			pays = append(pays, pay(500, dOnly(8, d)))
		}
		items = append(items, chk(7000, mon(8, 10)), chk(8000, mon(8, 11)))
		v, left, _ = hm(items, append(pays, pay(15000, dOnly(8, pd))))
		if v[31].Status != stCombined || v[32].Status != stCombined {
			t.Errorf("#21 %d: %v %v", pd, v[31].Status, v[32].Status)
		}
		wantLeft(t, "#21", left)
		items[31] = chk(15000, mon(8, 10))
		items = items[:32]
		v, left, _ = hm(items, append(pays, pay(7000, dOnly(8, pd)), pay(8000, dOnly(8, pd))))
		if v[31].Status != stSplit {
			t.Errorf("#21 частями %d: %v", pd, v[31].Status)
		}
		wantLeft(t, "#21 частями", left)
	}

	// 23. Цепочка «одной оплатой».
	v, left, _ = hm([]reconItem{cashIt(1000, mon(8, 1), true), cashIt(1000, mon(8, 2), true), cashIt(1000, mon(8, 3), true), cashIt(1000, mon(8, 4), true)},
		[]cmf.Payment{pay(2000, dOnly(8, 3)), pay(2000, dOnly(8, 5))})
	wantStatuses(t, "#23", v, stCombined, stCombined, stCombined, stCombined)
	wantLeft(t, "#23", left)
	items, pays = nil, nil
	for d := 1; d <= 10; d++ {
		items = append(items, cashIt(1000, mon(8, d), true))
		if d%2 == 0 {
			pays = append(pays, pay(2000, dOnly(8, d+1)))
		}
	}
	v, left, _ = hm(items, pays)
	for i, x := range v {
		if x.Status != stCombined {
			t.Errorf("#23 10 дней: позиция %d статус %v", i, x.Status)
		}
	}
	wantLeft(t, "#23 10 дней", left)

	// 24. Поздние наличные комбинации не ломают точное 1000 = 1000.
	items = []reconItem{
		cashIt(500, mon(8, 17), true), cashIt(700, mon(9, 3), true), cashIt(300, mon(8, 30), true),
		cashIt(300, mon(8, 31), true), cashIt(1000, mon(9, 4), true),
		cashIt(500, mon(9, 21), true), cashIt(500, mon(9, 22), true), cashIt(700, mon(9, 26), true), cashIt(700, mon(9, 27), true),
	}
	pays = []cmf.Payment{pay(600, dOnly(9, 2)), pay(1000, dOnly(9, 6)), pay(1000, dOnly(9, 22)), pay(1400, dOnly(9, 27))}
	short, _, _ := hm(items[:5], pays[:2])
	wantStatuses(t, "#24 коротко", short, stNotEntered, stNotEntered, stCombined, stCombined, stEntered)
	v, left, _ = hm(items, pays)
	wantStatuses(t, "#24", v, stNotEntered, stNotEntered, stCombined, stCombined, stEntered, stCombined, stCombined, stCombined, stCombined)
	wantLeft(t, "#24", left)
}

func TestReconR3Perf(t *testing.T) {
	// 22. Сторно не умножают время работы.
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, msk)
	do := func(d int) time.Time {
		y, m, dd := base.AddDate(0, 0, d).Date()
		return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
	}
	rng := rand.New(rand.NewSource(7))
	var items []reconItem
	var pays []cmf.Payment
	for d := 0; d < 150; d++ {
		items = append(items, reconItem{ID: -(d + 1), Kind: "cash", Amount: 500, Date: base.AddDate(0, 0, d), Report: d >= 45 && d < 75})
	}
	for d := 0; d < 150; d++ {
		switch r := rng.Intn(10); {
		case r == 0:
		case r <= 2 && d+1 < 150:
			pays = append(pays, cmf.Payment{Amount: 1000, PaidAt: do(d + 1 + rng.Intn(3)), ContractID: "A"})
			d++
		default:
			pays = append(pays, cmf.Payment{Amount: 500, PaidAt: do(d + rng.Intn(4)), ContractID: "A"})
		}
	}
	pays = append(pays, cmf.Payment{Amount: -500, PaidAt: do(20), ContractID: "A"}, cmf.Payment{Amount: -500, PaidAt: do(45), ContractID: "A"})
	start := time.Now()
	humanMatch(items, pays, unitsRubles)
	if el := time.Since(start); el > time.Second {
		t.Errorf("#22: %v", el)
	}
	t.Logf("#22: %v", time.Since(start))

	// Ответ не зависит от порядка оплат и позиций.
	v1, l1, _ := humanMatch(items, pays, unitsRubles)
	pr := append([]cmf.Payment(nil), pays...)
	rng.Shuffle(len(pr), func(a, b int) { pr[a], pr[b] = pr[b], pr[a] })
	v2, l2, _ := humanMatch(items, pr, unitsRubles)
	if len(l1) != len(l2) {
		t.Errorf("перестановка: лишних оплат %d vs %d", len(l1), len(l2))
	}
	n1, n2 := 0, 0
	for i := range v1 {
		if v1[i].entered() {
			n1++
		}
		if v2[i].entered() {
			n2++
		}
	}
	if n1 != n2 {
		t.Errorf("перестановка: внесено %d vs %d", n1, n2)
	}
}
