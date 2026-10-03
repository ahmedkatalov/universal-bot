package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

// Регрессии второго раунда враждебной проверки сверки.

func TestReconR2Tradeoffs(t *testing.T) {
	// 1. Комбинация из трёх не должна побеждать точное совпадение в тот же день.
	v, left, _ := hm([]reconItem{chk(5000, aug(1)), chk(5000, aug(2)), chk(5000, aug(3)), chk(15000, aug(20))},
		[]cmf.Payment{pay(15000, aug(20))})
	wantStatuses(t, "#1a", v, stNotEntered, stNotEntered, stNotEntered, stEntered)
	wantLeft(t, "#1a", left)
	v, _, _ = hm([]reconItem{ctxChk(5000, mon(7, 25)), ctxChk(5000, mon(7, 27)), ctxChk(5000, mon(7, 29)), chk(15000, aug(12))},
		[]cmf.Payment{pay(15000, aug(12))})
	wantStatuses(t, "#1b", v, stNotEntered, stNotEntered, stNotEntered, stEntered)

	// 2. Занятой клиент: дважды в неделю чек 7000 вносят двумя оплатами 5000+2000.
	var items []reconItem
	var pays []cmf.Payment
	for d := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Monday && d.Weekday() != time.Thursday {
			continue
		}
		items = append(items, reconItem{Kind: "check", Amount: 7000, Date: d, Report: d.Month() == time.September})
		pd := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		pays = append(pays, cmf.Payment{Amount: 5000, PaidAt: pd, ContractNumber: 1}, cmf.Payment{Amount: 2000, PaidAt: pd, ContractNumber: 2})
	}
	start := time.Now()
	v, left, _ = hm(items, pays)
	for i, x := range v {
		if x.Status != stSplit {
			t.Errorf("#2: чек %s статус %v, ожидали частями", items[i].Date.Format("02.01"), x.Status)
		}
	}
	wantLeft(t, "#2", left)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("#2: сверка заняла %v", el)
	}

	// 3. Плотная пара ближе к оплате, чем далёкая тройка июльских чеков.
	v, _, _ = hm([]reconItem{ctxChk(5000, mon(7, 20)), ctxChk(5000, mon(7, 23)), ctxChk(5000, mon(7, 26)), chk(10000, aug(12)), chk(5000, aug(12))},
		[]cmf.Payment{pay(15000, aug(13))})
	wantStatuses(t, "#3", v, stNotEntered, stNotEntered, stNotEntered, stCombined, stCombined)

	// 4. Кандидаты на «одной оплатой» — ближайшие по дате, а не самые старые.
	items, pays = nil, nil
	for d := 0; d < 14; d++ {
		day := mon(7, 28).AddDate(0, 0, d)
		items = append(items, chk(1000, day))
		pays = append(pays, pay(1000, day.AddDate(0, 0, 1)))
	}
	items = append(items, chk(1200, aug(11)), chk(800, aug(12)))
	pays = append(pays, pay(2000, aug(13)))
	v, left, _ = hm(items, pays)
	if v[14].Status != stCombined || v[15].Status != stCombined {
		t.Errorf("#4: %v", statuses(v))
	}
	wantLeft(t, "#4", left)

	// 5. Поздние части не забирают точное совпадение в тот же день.
	v, _, _ = hm([]reconItem{chk(15000, aug(1)), ctxChk(5000, aug(20))},
		[]cmf.Payment{pay(5000, aug(18)), pay(5000, aug(19)), pay(5000, aug(20))})
	wantStatuses(t, "#5", v, stNotEntered, stEntered)

	// 6. Четыре чека одной оплатой.
	v, left, _ = hm([]reconItem{chk(5000, aug(1)), chk(5000, aug(4)), chk(5000, aug(8)), chk(5000, aug(12))},
		[]cmf.Payment{pay(20000, aug(13))})
	wantStatuses(t, "#6", v, stCombined, stCombined, stCombined, stCombined)
	wantLeft(t, "#6", left)
}

func TestReconR2Dates(t *testing.T) {
	// 7. Нет даты оплаты, есть дата внесения — описание по дате внесения, без чепухи.
	items := []reconItem{chk(15000, at(8, 1, 12))}
	pays := []cmf.Payment{{Amount: 15000, CreatedAt: time.Date(2026, 9, 5, 10, 0, 0, 0, msk)}}
	v, _, k := hm(items, pays)
	wantStatuses(t, "#7a", v, stLate)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "05.09") || !strings.Contains(s, "через 35 дн.") {
		t.Errorf("#7a: %q", s)
	}
	items = []reconItem{chk(15000, at(8, 10, 12))}
	pays = []cmf.Payment{{Amount: 15000, CreatedAt: time.Date(2026, 8, 3, 10, 0, 0, 0, msk)}}
	v, _, k = hm(items, pays)
	wantStatuses(t, "#7b", v, stDateCheck)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "03.08") || !strings.Contains(s, "на 7 дн.") {
		t.Errorf("#7b: %q", s)
	}
	pays = []cmf.Payment{{Amount: 1500, CreatedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, msk)}}
	v, _, k = hm(items, pays)
	wantStatuses(t, "#7c", v, stSuspicious)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "12.08") || strings.Contains(s, "01.01") {
		t.Errorf("#7c: %q", s)
	}

	// 8. Дата внесения через 21 день — всё равно «внесён поздно, поправь дату».
	items = []reconItem{chk(15000, at(8, 10, 12))}
	pays = []cmf.Payment{{Amount: 15000, PaidAt: dOnly(7, 10), CreatedAt: time.Date(2026, 8, 31, 10, 0, 0, 0, msk)}}
	v, left, _ := hm(items, pays)
	wantStatuses(t, "#8", v, stLate)
	wantLeft(t, "#8", left)
	if !strings.Contains(v[0].Note, "поправь дату") {
		t.Errorf("#8: note %q", v[0].Note)
	}

	// 9. Дата внесения работает и для «одной оплатой» / «частями».
	items = []reconItem{chk(10000, at(8, 10, 12)), chk(5000, at(8, 11, 12))}
	pays = []cmf.Payment{{Amount: 15000, PaidAt: dOnly(7, 11), CreatedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, msk)}}
	v, left, _ = hm(items, pays)
	wantStatuses(t, "#9a", v, stCombined, stCombined)
	wantLeft(t, "#9a", left)
	if !strings.Contains(v[0].Note, "поправь дату") {
		t.Errorf("#9a: note %q", v[0].Note)
	}
	items = []reconItem{chk(15000, at(8, 10, 12))}
	pays = []cmf.Payment{{Amount: 10000, CreatedAt: time.Date(2026, 8, 11, 10, 0, 0, 0, msk)}, {Amount: 5000, CreatedAt: time.Date(2026, 8, 11, 10, 0, 0, 0, msk)}}
	v, left, _ = hm(items, pays)
	wantStatuses(t, "#9b", v, stSplit)
	wantLeft(t, "#9b", left)

	// 10. «Одной оплатой» / «частями» — тоже бывает поздно.
	v, left, _ = hm([]reconItem{chk(10000, at(8, 1, 12)), chk(5000, at(8, 10, 12))}, []cmf.Payment{pay(15000, dOnly(8, 22))})
	wantStatuses(t, "#10a", v, stCombined, stCombined)
	wantLeft(t, "#10a", left)
	v, left, _ = hm([]reconItem{chk(15000, at(8, 1, 12))}, []cmf.Payment{pay(10000, dOnly(8, 22)), pay(5000, dOnly(8, 26))})
	wantStatuses(t, "#10b", v, stSplit)
	wantLeft(t, "#10b", left)
	if !strings.Contains(v[0].Note, "поздно") {
		t.Errorf("#10b: note %q", v[0].Note)
	}

	// 11. Время без пояса из программы — московское.
	var p cmf.Payment
	if err := json.Unmarshal([]byte(`{"amount":10000,"paid_at":"2026-08-21 22:00:00"}`), &p); err != nil {
		t.Fatal(err)
	}
	items = []reconItem{chk(10000, at(8, 1, 12))}
	v, _, k = hm(items, []cmf.Payment{p})
	wantStatuses(t, "#11", v, stEntered)
	if s := describeVerdict(items, []cmf.Payment{p}, v, 0, k); !strings.Contains(s, "21.08") {
		t.Errorf("#11: %q", s)
	}

	// 12. Формулировки: «как у чека», наличка — «внесена».
	items = []reconItem{chk(10000, at(8, 10, 12)), chk(10000, at(8, 11, 12))}
	pays = []cmf.Payment{pay(10000, dOnly(8, 12))}
	v, _, k = hm(items, pays)
	for i := range items {
		s := describeVerdict(items, pays, v, i, k)
		if strings.Contains(s, "как у чек ") || strings.Contains(s, "как у наличка") {
			t.Errorf("#12a: %q", s)
		}
	}
	items = []reconItem{cashIt(5000, at(8, 10, 12), true)}
	pays = []cmf.Payment{pay(5000, dOnly(8, 10))}
	v, _, k = hm(items, pays)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "внесена") || strings.Contains(s, "внесён") {
		t.Errorf("#12b: %q", s)
	}
}

func TestReconR2Units(t *testing.T) {
	// 13. Единицу решают оплаты, а не позиции: пара мелких налички её не переворачивает.
	v, _, kop := hm([]reconItem{cashIt(500, aug(3), true), chk(50000, aug(8)), cashIt(500, aug(10), true)},
		[]cmf.Payment{pay(50000, aug(9))})
	if kop {
		t.Error("#13a: kopecks")
	}
	wantStatuses(t, "#13a", v, stNotEntered, stEntered, stNotEntered)
	v, _, kop = hm([]reconItem{chk(15000, aug(10)), cashIt(150, aug(10), true), cashIt(150, aug(11), true)},
		[]cmf.Payment{pay(15000, aug(12))})
	if kop {
		t.Error("#13b: kopecks")
	}
	wantStatuses(t, "#13b", v, stEntered, stNotEntered, stNotEntered)

	// 14. Допуск по копейкам растёт с числом чеков в одной оплате.
	v, left, _ := humanMatch([]reconItem{chk(5000.70, aug(10)), chk(5000.70, aug(11))}, []cmf.Payment{pay(10000, aug(12))}, unitsRubles)
	wantStatuses(t, "#14a", v, stCombined, stCombined)
	wantLeft(t, "#14a", left)
	v, _, _ = humanMatch([]reconItem{chk(5000.50, aug(10)), chk(5000.50, aug(11)), chk(5000.50, aug(12))}, []cmf.Payment{pay(15000, aug(13))}, unitsRubles)
	wantStatuses(t, "#14b", v, stCombined, stCombined, stCombined)
}

func TestReconR2Slips(t *testing.T) {
	// 15. Лишний ноль, когда у чека копейки, а их не внесли.
	v, _, _ := humanMatch([]reconItem{chk(1500.50, aug(10))}, []cmf.Payment{pay(15000, aug(12))}, unitsRubles)
	wantStatuses(t, "#15", v, stSuspicious)
	if v[0].Note != "похоже, лишний ноль" {
		t.Errorf("#15: note %q", v[0].Note)
	}
	// 16. Ноль потерян/добавлен в середине суммы.
	v, _, _ = humanMatch([]reconItem{chk(10500, aug(10))}, []cmf.Payment{pay(1500, aug(12))}, unitsRubles)
	wantStatuses(t, "#16a", v, stSuspicious)
	v, _, _ = humanMatch([]reconItem{chk(2500, aug(10))}, []cmf.Payment{pay(20500, aug(12))}, unitsRubles)
	wantStatuses(t, "#16b", v, stSuspicious)
	// 17. В сообщении «проверь сумму» видны настоящие суммы с копейками.
	items := []reconItem{chk(15000.50, aug(10))}
	pays := []cmf.Payment{pay(1499950, aug(11))}
	v, _, k := humanMatch(items, pays, unitsKopecks)
	wantStatuses(t, "#17", v, stSuspicious)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "15 000,50") || !strings.Contains(s, "14 999,50") {
		t.Errorf("#17: %q", s)
	}
	// 18. Переставленные первые цифры.
	v, _, _ = humanMatch([]reconItem{chk(23000, aug(10))}, []cmf.Payment{pay(32000, aug(12))}, unitsRubles)
	wantStatuses(t, "#18", v, stSuspicious)
	if v[0].Note != "похоже, переставили цифры" {
		t.Errorf("#18: note %q", v[0].Note)
	}
}

func TestReconR2Reversals(t *testing.T) {
	// 19. Сторно отменяет только запись, сделанную до него.
	v, left, _ := hm([]reconItem{ctxChk(10000, mon(7, 5)), chk(10000, aug(5))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(7, 6), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 3), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 6), ContractID: "c1"}})
	wantStatuses(t, "#19a", v, stNotEntered, stEntered)
	wantLeft(t, "#19a", left)
	v, _, _ = hm([]reconItem{chk(10000, aug(1)), chk(10000, aug(20))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(8, 2), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 12), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 21), ContractID: "c1"}})
	wantStatuses(t, "#19b", v, stNotEntered, stEntered)

	// 20. Исправили дату: сторно ошибочной записи и ввод заново в тот же день.
	v, left, _ = hm([]reconItem{chk(10000, aug(10))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(7, 10), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 11), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 11), ContractID: "c1"}})
	wantStatuses(t, "#20", v, stEntered)
	wantLeft(t, "#20", left)
	if v[0].Note != "" {
		t.Errorf("#20: note %q", v[0].Note)
	}

	// 21. Сторно дубля не отменяет законный платёж.
	v, left, _ = hm([]reconItem{ctxChk(10000, mon(7, 5)), chk(10000, aug(5))}, []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(7, 6), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(7, 6), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 6), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 10), ContractID: "c1"}})
	wantStatuses(t, "#21", v, stEntered, stEntered)
	wantLeft(t, "#21", left)

	// 22. Частичная корректировка уменьшает запись.
	pays := []cmf.Payment{{Amount: 15000, PaidAt: dOnly(8, 11), ContractID: "c1"}, {Amount: -5000, PaidAt: dOnly(8, 12), ContractID: "c1"}}
	v, _, _ = hm([]reconItem{chk(15000, aug(10))}, pays)
	if v[0].Status == stEntered && v[0].Note == "" {
		t.Errorf("#22a: внесён без оговорок, а в программе нетто 10 000")
	}
	v, left, _ = hm([]reconItem{chk(10000, aug(10))}, pays)
	if !v[0].entered() {
		t.Errorf("#22b: %v", statuses(v))
	}
	wantLeft(t, "#22b", left)

	// 23. Длинная история клиента не вытесняет настоящее «частями».
	var items []reconItem
	pays = nil
	for d := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 7) {
		items = append(items, reconItem{Kind: "check", Amount: 5000, Date: d, Report: d.Month() == time.August})
		pays = append(pays, cmf.Payment{Amount: 5000, PaidAt: time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, time.UTC), ContractID: "c2"})
	}
	for _, m := range []time.Month{6, 7, 9, 10} {
		items = append(items, ctxChk(10000, mon(m, 10)))
		pays = append(pays, cmf.Payment{Amount: 10000, PaidAt: dOnly(m, 11), ContractID: "c1"})
	}
	items = append(items, chk(15000, aug(10)))
	pays = append(pays, cmf.Payment{Amount: 10000, PaidAt: dOnly(8, 28), ContractID: "c1"}, cmf.Payment{Amount: 5000, PaidAt: dOnly(8, 28), ContractID: "c2"})
	v, left, _ = hm(items, pays)
	if s := v[len(v)-1].Status; s != stSplit {
		t.Errorf("#23: статус %v, ожидали частями", s)
	}
	wantLeft(t, "#23", left)

	// 24. Сторно больше чем через 60 дней — всё равно сторно.
	v, _, _ = hm([]reconItem{chk(15000, aug(1))}, []cmf.Payment{
		{Amount: 15000, PaidAt: dOnly(8, 2), ContractID: "c1"},
		{Amount: -15000, PaidAt: dOnly(10, 5), ContractID: "c1"}})
	wantStatuses(t, "#24", v, stNotEntered)

	// 25. Ответ не зависит от порядка оплат.
	pays = []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(8, 6), ContractID: "c1"},
		{Amount: 10000, PaidAt: dOnly(8, 14), ContractID: "c1"},
		{Amount: -10000, PaidAt: dOnly(8, 10), ContractID: "c1"}}
	v1, _, _ := hm([]reconItem{chk(10000, aug(13))}, pays)
	v2, _, _ := hm([]reconItem{chk(10000, aug(13))}, []cmf.Payment{pays[1], pays[0], pays[2]})
	wantStatuses(t, "#25a", v1, stEntered)
	wantStatuses(t, "#25b", v2, stEntered)

	// 26. «Частями» по двум договорам — оба номера.
	items = []reconItem{chk(13000, aug(5))}
	pays = []cmf.Payment{
		{Amount: 10000, PaidAt: dOnly(8, 6), ContractID: "c1", ContractNumber: 101},
		{Amount: 3000, PaidAt: dOnly(8, 6), ContractID: "c2", ContractNumber: 202}}
	v, _, k := hm(items, pays)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "101") || !strings.Contains(s, "202") {
		t.Errorf("#26: %q", s)
	}
}

func TestReconR2Copies(t *testing.T) {
	t0 := time.Date(2026, 8, 10, 11, 32, 0, 0, time.UTC)
	perms := func(rs []db.ReconReceipt) [][]db.ReconReceipt {
		var out [][]db.ReconReceipt
		var rec func(k int)
		rec = func(k int) {
			if k == len(rs) {
				out = append(out, append([]db.ReconReceipt(nil), rs...))
				return
			}
			for x := k; x < len(rs); x++ {
				rs[k], rs[x] = rs[x], rs[k]
				rec(k + 1)
				rs[k], rs[x] = rs[x], rs[k]
			}
		}
		rec(0)
		return out
	}
	check := func(name string, rs []db.ReconReceipt, want int) {
		t.Helper()
		for _, p := range perms(rs) {
			if out := dedupeReconReceipts(p); len(out) != want {
				t.Errorf("%s: позиций %d, ожидали %d", name, len(out), want)
				return
			}
		}
	}
	// 27. Группа, куда переслан оригинал, уже занята его копией.
	check("#27", []db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "M1"},
		{ID: 2, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g2", WaMessageID: "M1-fwd-g2", NeedsReview: true},
		{ID: 3, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0.Add(50 * time.Second), GroupJID: "g2", WaMessageID: "M2"},
	}, 2)
	// 28. Разные люди с похожими именами.
	check("#28", []db.ReconReceipt{
		{ID: 1, Name: "Магомедов Рустам", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Магомедов Руслан", Amount: 5000, TxDate: t0.Add(20 * time.Second), GroupJID: "g2", WaMessageID: "B"},
	}, 2)
	// 29. Номера документов всех копий помнятся; порядок строк не важен.
	check("#29a", []db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g2", DocNumber: "777", WaMessageID: "B"},
		{ID: 3, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g3", DocNumber: "888", WaMessageID: "C"},
	}, 2)
	check("#29b", []db.ReconReceipt{
		{ID: 1, Name: "Хадижат Ибрагимова", Amount: 15000, TxDate: t0, GroupJID: "g1", DocNumber: "777", WaMessageID: "A", NeedsReview: true},
		{ID: 2, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g2", WaMessageID: "B"},
		{ID: 3, Name: "Хадижат Ибрагимова", Amount: 15000, TxDate: t0.Add(3 * time.Hour), GroupJID: "g3", DocNumber: "777", WaMessageID: "C", NeedsReview: true},
	}, 1)
	// 32. Номер документа латиницей и кириллицей — один чек.
	check("#32", []db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g1", DocNumber: "A62231132045670B0000050011570501", WaMessageID: "A"},
		{ID: 2, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g2", DocNumber: "А62231132045670В0000050011570501", WaMessageID: "B"},
	}, 1)
	// 33. «Каталов А.Н.» и «Каталов Ахмед Н.» — один человек.
	check("#33", []db.ReconReceipt{
		{ID: 1, Name: "Каталов А.Н.", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
		{ID: 2, Name: "Каталов Ахмед Н.", Amount: 5000, TxDate: t0.Add(30 * time.Second), GroupJID: "g2", WaMessageID: "B"},
	}, 1)
	// 34. Две неподтверждённые копии с разными получателями — разные чеки.
	check("#34", []db.ReconReceipt{
		{ID: 1, Name: "Ибрагимова Хадижат", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A", NeedsReview: true},
		{ID: 2, Name: "Петров Сергей", Amount: 5000, TxDate: t0.Add(40 * time.Second), GroupJID: "g2", WaMessageID: "B", NeedsReview: true},
	}, 2)
	check("#34b", []db.ReconReceipt{
		{ID: 1, Name: "Хадижат Имрановна С.", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A", NeedsReview: true},
		{ID: 2, Name: "Хадижат С.", Amount: 5000, TxDate: t0.Add(40 * time.Second), GroupJID: "g2", WaMessageID: "B", NeedsReview: true},
	}, 1)
}

func TestReconR2Names(t *testing.T) {
	yes := [][2]string{
		{"Магомедов Хаджи-Мурат", "Магомедов Хаджимурат Ахмедович"}, // 30
		{"Абдул-Керим Алиев", "Алиев Абдулкерим"},
		{"Каталов А.Н.", "Каталов Ахмед Н."}, // 33
		{"Каталов А. Н.", "Каталов АН"},
		{"Каталов А.Н.", "Каталов Ахмед"},
		{"Магомедов Магомед Р.", "Магомедов Магомед"},
		{"Каталов Ахмед", "Котолов Ахмед"}, // опечатка в гласных
		{"Ахмад Каталов", "Ахмед Каталов"},
		{"Шамиль Исаев", "Шамил Исаев"},
		{"Абдуллаев Руслан", "Абдулаев Руслан"},
	}
	for _, c := range yes {
		if !sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=false, ожидали true", c[0], c[1])
		}
	}
	no := [][2]string{
		{"Магомедов Рустам", "Магомедов Руслан"}, // 28
		{"Алиева Мадина", "Алиева Марина"},
		{"Ахмедов Рамиль", "Ахмедов Камиль"},
		{"Ахмедов Руслан", "Ахматов Руслан"},
		{"Исаев Иса", "Исаева Ира"},
		{"Каталов Ахмед Н.", "Каталов Ахмед Р."}, // 31
		{"Каталов Ахмед Нажудович", "Каталов Ахмед Р."},
		{"Магомедов Магомед Р.", "Магомедов Магомед Абдулаевич"},
		{"Каталов АН", "Каталов Ахмед Русланович"}, // 33
		{"Каталов А.Н.", "Каталов Ахмед Русланович"},
		{"Каталов А.Р.", "Каталов Ахмед Н."},
		{"Каталов А.", "Каталов Б.Н."},
	}
	for _, c := range no {
		if sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=true, ожидали false", c[0], c[1])
		}
	}
}

func TestReconR2Perf(t *testing.T) {
	do := func(t time.Time) time.Time { y, m, d := t.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, msk)

	// 35. Длинная история «частями» — каждая неделя своя.
	var items []reconItem
	var pays []cmf.Payment
	for i := 0; i < 16; i++ {
		d := base.AddDate(0, 0, 7*i)
		items = append(items, reconItem{ID: i + 1, Kind: "check", Amount: 10000, Date: d, Report: i >= 12})
		pd := d
		if i >= 12 {
			pd = pd.AddDate(0, 0, 1)
		}
		pays = append(pays, cmf.Payment{Amount: 5000, PaidAt: do(pd), ContractID: "A"}, cmf.Payment{Amount: 5000, PaidAt: do(pd), ContractID: "B"})
	}
	v, left, _ := humanMatch(items, pays, unitsRubles)
	for i, x := range v {
		if x.Status != stSplit {
			t.Errorf("#35: неделя %d статус %v", i, x.Status)
		}
	}
	wantLeft(t, "#35", left)

	// 36. Случайные комбинации не вытесняют настоящую.
	items, pays = nil, nil
	id := 0
	for w := 0; w < 6; w++ {
		d := base.AddDate(0, 0, 7*w)
		for _, a := range []float64{5000, 5000, 10000} {
			id++
			items = append(items, reconItem{ID: id, Kind: "check", Amount: a, Date: d})
		}
		pays = append(pays, cmf.Payment{Amount: 5000, PaidAt: do(d), ContractID: "A"}, cmf.Payment{Amount: 5000, PaidAt: do(d), ContractID: "B"}, cmf.Payment{Amount: 10000, PaidAt: do(d), ContractID: "C"})
	}
	items = append(items, reconItem{ID: 100, Kind: "check", Amount: 8000, Date: base.AddDate(0, 0, 22), Report: true}, reconItem{ID: 101, Kind: "check", Amount: 7000, Date: base.AddDate(0, 0, 23), Report: true})
	pays = append(pays, cmf.Payment{Amount: 15000, PaidAt: do(base.AddDate(0, 0, 25)), ContractID: "D"})
	v, left, _ = humanMatch(items, pays, unitsRubles)
	for i := 0; i < 18; i++ {
		if v[i].Status != stEntered {
			t.Errorf("#36: позиция %d статус %v", i, v[i].Status)
		}
	}
	if v[18].Status != stCombined || v[19].Status != stCombined {
		t.Errorf("#36: %v %v", v[18].Status, v[19].Status)
	}
	wantLeft(t, "#36", left)

	// 37. Большая история считается быстро.
	items, pays = nil, nil
	for i := 0; i < 12; i++ {
		d := base.AddDate(0, 0, 7*i)
		items = append(items, reconItem{ID: i + 1, Kind: "check", Amount: 10000, Date: d})
		pays = append(pays, cmf.Payment{Amount: 5000, PaidAt: do(d), ContractID: "A"}, cmf.Payment{Amount: 5000, PaidAt: do(d), ContractID: "B"})
	}
	for i := 0; i < 20; i++ {
		for c, cid := range []string{"C", "D", "E"} {
			d := base.AddDate(0, 0, 7*i+c)
			items = append(items, reconItem{ID: 100 + 3*i + c, Kind: "check", Amount: 3000, Date: d})
			pays = append(pays, cmf.Payment{Amount: 3000, PaidAt: do(d.AddDate(0, 0, (i+c)%4)), ContractID: cid})
		}
	}
	start := time.Now()
	v, left, _ = humanMatch(items, pays, unitsRubles)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("#37: %v", el)
	}
	for i := 0; i < 12; i++ {
		if v[i].Status != stSplit {
			t.Errorf("#37: неделя %d статус %v", i, v[i].Status)
		}
	}
	wantLeft(t, "#37", left)

	// 38. Ежедневная мелкая наличка не вытесняет чеки прямо перед оплатой.
	items, pays = nil, nil
	for d := 0; d < 40; d++ {
		day := base.AddDate(0, 0, d)
		items = append(items, reconItem{ID: -(d + 1), Kind: "cash", Amount: 500, Date: day})
		pays = append(pays, cmf.Payment{Amount: 500, PaidAt: do(day), ContractID: "A"})
	}
	items = append(items, reconItem{ID: 1, Kind: "check", Amount: 7000, Date: base.AddDate(0, 0, 29), Report: true}, reconItem{ID: 2, Kind: "check", Amount: 8000, Date: base.AddDate(0, 0, 30), Report: true})
	pays = append(pays, cmf.Payment{Amount: 15000, PaidAt: do(base.AddDate(0, 0, 31)), ContractID: "B"})
	v, left, _ = humanMatch(items, pays, unitsRubles)
	if v[40].Status != stCombined || v[41].Status != stCombined {
		t.Errorf("#38: %v %v", v[40].Status, v[41].Status)
	}
	wantLeft(t, "#38", left)
	// вариант «частями»
	items = items[:40]
	items = append(items, reconItem{ID: 3, Kind: "check", Amount: 15000, Date: base.AddDate(0, 0, 30), Report: true})
	pays = pays[:40]
	pays = append(pays, cmf.Payment{Amount: 7000, PaidAt: do(base.AddDate(0, 0, 31)), ContractID: "B"}, cmf.Payment{Amount: 8000, PaidAt: do(base.AddDate(0, 0, 31)), ContractID: "B"})
	v, left, _ = humanMatch(items, pays, unitsRubles)
	if v[40].Status != stSplit {
		t.Errorf("#38 частями: %v", v[40].Status)
	}
	wantLeft(t, "#38 частями", left)
}
