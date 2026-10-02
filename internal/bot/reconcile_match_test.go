package bot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

var msk = time.FixedZone("MSK", 3*3600)

func aug(day int) time.Time { return time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC) }
func mon(m time.Month, day int) time.Time {
	return time.Date(2026, m, day, 12, 0, 0, 0, time.UTC)
}

// dOnly — дата из программы без времени («2026-08-10» → 00:00 UTC).
func dOnly(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

// at — время чека по Москве.
func at(m time.Month, day, hour int) time.Time { return time.Date(2026, m, day, hour, 0, 0, 0, msk) }

func chk(amount float64, t time.Time) reconItem {
	return reconItem{Kind: "check", Amount: amount, Date: t, Report: true}
}
func ctxChk(amount float64, t time.Time) reconItem {
	return reconItem{Kind: "check", Amount: amount, Date: t}
}
func cashIt(amount float64, t time.Time, report bool) reconItem {
	return reconItem{Kind: "cash", Amount: amount, Date: t, Report: report}
}
func pay(amount int64, t time.Time) cmf.Payment { return cmf.Payment{Amount: amount, PaidAt: t} }

func hm(items []reconItem, pays []cmf.Payment) ([]reconVerdict, []int, bool) {
	return humanMatch(items, pays, unitsAuto)
}

func statuses(v []reconVerdict) []reconStatus {
	out := make([]reconStatus, len(v))
	for i := range v {
		out[i] = v[i].Status
	}
	return out
}

func wantStatuses(t *testing.T, name string, v []reconVerdict, want ...reconStatus) {
	t.Helper()
	got := statuses(v)
	if len(got) != len(want) {
		t.Fatalf("%s: статусов %d, ожидали %d (%v)", name, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: позиция %d статус %v, ожидали %v (все: %v)", name, i, got[i], want[i], got)
		}
	}
}

func wantLeft(t *testing.T, name string, left []int, want ...int) {
	t.Helper()
	if len(left) != len(want) {
		t.Errorf("%s: лишние оплаты %v, ожидали %v", name, left, want)
		return
	}
	for i := range want {
		if left[i] != want[i] {
			t.Errorf("%s: лишние оплаты %v, ожидали %v", name, left, want)
			return
		}
	}
}

// --- Инварианты прежнего сопоставителя ---

func TestHumanMatchLegacyInvariants(t *testing.T) {
	v, left, _ := hm([]reconItem{chk(20000, aug(20))}, []cmf.Payment{pay(20000, aug(24))})
	wantStatuses(t, "одиночный", v, stEntered)
	wantLeft(t, "одиночный", left)

	v, _, _ = hm([]reconItem{chk(20000, aug(20)), chk(20000, aug(21))}, []cmf.Payment{pay(20000, aug(24))})
	wantStatuses(t, "два чека/одна оплата", v, stNotEntered, stEntered)
	if v[0].SameAmountAs != 1 {
		t.Errorf("ожидали пометку «одна из двух» (SameAmountAs=1), получили %d", v[0].SameAmountAs)
	}

	v, _, kop := hm([]reconItem{chk(15000, aug(5))}, []cmf.Payment{pay(1500000, aug(5))})
	wantStatuses(t, "копейки", v, stEntered)
	if !kop {
		t.Error("копейки: ожидали kopecks=true")
	}

	v, _, _ = hm([]reconItem{chk(20000, aug(10)), chk(25000, aug(11))}, []cmf.Payment{pay(25000, aug(12)), pay(20000, aug(13))})
	wantStatuses(t, "разные суммы", v, stEntered, stEntered)

	v, left, _ = hm([]reconItem{chk(20000, aug(10))}, []cmf.Payment{pay(20000, aug(10)), pay(5000, aug(10))})
	wantStatuses(t, "лишняя оплата", v, stEntered)
	wantLeft(t, "лишняя оплата", left, 1)

	v, _, kop = hm([]reconItem{chk(10000, aug(1)), chk(100, aug(2))}, []cmf.Payment{pay(10000, aug(3)), pay(100, aug(4))})
	wantStatuses(t, "единица", v, stEntered, stEntered)
	if kop {
		t.Error("единица: kopecks должно быть false")
	}

	v, _, _ = hm([]reconItem{chk(20000, aug(20))}, []cmf.Payment{pay(20000, mon(7, 20))})
	wantStatuses(t, "прошлый месяц", v, stNotEntered)

	v, _, _ = hm([]reconItem{chk(30000, aug(10))}, []cmf.Payment{pay(30000, aug(8))})
	wantStatuses(t, "на 2 дня раньше", v, stEntered)

	v, _, _ = hm([]reconItem{chk(15000, aug(10)), chk(15000, aug(1))}, []cmf.Payment{pay(15000, aug(10)), pay(15000, aug(25))})
	wantStatuses(t, "максимум", v, stEntered, stEntered)
}

// --- Сценарии «как человек» ---

func TestHumanMatchOwnerExample(t *testing.T) {
	v, left, _ := hm([]reconItem{chk(15000, aug(10)), chk(8000, aug(12))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "пример владельца", v, stEntered, stNotEntered)
	if v[1].SameAmountAs != -1 {
		t.Error("у 12.08 другая сумма — пометки «одна из двух» быть не должно")
	}
	wantLeft(t, "пример владельца", left)
}

func TestHumanMatchNeighbourMonth(t *testing.T) {
	v, _, _ := hm([]reconItem{chk(10000, mon(10, 5)), ctxChk(10000, mon(9, 28))}, []cmf.Payment{pay(10000, mon(10, 2))})
	wantStatuses(t, "соседний месяц", v, stNotEntered, stEntered)
}

func TestHumanMatchCombinedSplitLate(t *testing.T) {
	v, left, _ := hm([]reconItem{chk(10000, aug(10)), chk(5000, aug(11))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "одной оплатой", v, stCombined, stCombined)
	wantLeft(t, "одной оплатой", left)

	v, _, _ = hm([]reconItem{chk(10000, aug(10)), chk(5000, aug(11)), chk(15000, aug(12))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "точное рядом важнее", v, stNotEntered, stNotEntered, stEntered)

	v, _, _ = hm([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(10000, aug(14)), pay(5000, aug(16))})
	wantStatuses(t, "частями", v, stSplit)

	v, _, _ = hm([]reconItem{chk(15000, aug(1))}, []cmf.Payment{pay(15000, aug(30))})
	wantStatuses(t, "поздно", v, stLate)
}

func TestHumanMatchSlips(t *testing.T) {
	cases := []struct {
		got  int64
		want reconStatus
		note string
	}{
		{1500, stSuspicious, "похоже, потеряли ноль"},
		{150000, stSuspicious, "похоже, лишний ноль"},
		{16000, stSuspicious, "похоже, ошиблись в одной цифре"},
		{14800, stSuspicious, "внесли чуть меньше"},
		{5000, stNotEntered, ""},
	}
	for _, c := range cases {
		v, _, _ := humanMatch([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(c.got, aug(12))}, unitsRubles)
		if v[0].Status != c.want || v[0].Note != c.note {
			t.Errorf("оплата %d: статус %v «%s», ожидали %v «%s»", c.got, v[0].Status, v[0].Note, c.want, c.note)
		}
	}
	if s := amountSlip(1560000, 1650000); s != "похоже, переставили цифры" {
		t.Errorf("перестановка цифр: %q", s)
	}
}

func TestHumanMatchCashConsumes(t *testing.T) {
	v, _, _ := hm([]reconItem{cashIt(10000, aug(5), true), chk(10000, aug(7))}, []cmf.Payment{pay(10000, aug(5))})
	wantStatuses(t, "наличка", v, stEntered, stNotEntered)
}

// --- Регрессии из враждебной проверки (номер = находка) ---

func TestReconRegressionDates(t *testing.T) {
	// 1. Вечерний чек и оплата того же дня (дата без времени); второй чек частями.
	v, left, _ := hm([]reconItem{chk(10000, time.Date(2026, 8, 10, 22, 0, 0, 0, msk)), chk(15000, at(8, 13, 15))},
		[]cmf.Payment{pay(10000, dOnly(8, 10)), pay(10000, dOnly(8, 13)), pay(5000, dOnly(8, 14))})
	wantStatuses(t, "#1", v, stEntered, stSplit)
	if len(v[0].Pays) != 1 || v[0].Pays[0] != 0 {
		t.Errorf("#1: вечерний чек должен закрыться оплатой того же дня, а закрыт %v", v[0].Pays)
	}
	wantLeft(t, "#1", left)

	// 2. «−3 дня» — календарные дни, а не часы.
	v, _, _ = hm([]reconItem{chk(15000, at(8, 13, 15))}, []cmf.Payment{pay(10000, dOnly(8, 10)), pay(5000, dOnly(8, 10))})
	wantStatuses(t, "#2 частями", v, stSplit)
	v, _, _ = hm([]reconItem{chk(10000, at(8, 12, 10)), chk(5000, at(8, 13, 15))}, []cmf.Payment{pay(15000, dOnly(8, 10))})
	wantStatuses(t, "#2 одной оплатой", v, stCombined, stCombined)
	v, _, _ = hm([]reconItem{chk(10000, at(8, 13, 15))}, []cmf.Payment{pay(10000, dOnly(8, 10))})
	wantStatuses(t, "#2 точное", v, stEntered)

	// 3. Два поздних чека и две поздние оплаты — оба «поздно».
	v, left, _ = hm([]reconItem{chk(10000, at(8, 1, 15)), chk(10000, at(8, 20, 15))},
		[]cmf.Payment{pay(10000, dOnly(9, 10)), pay(10000, dOnly(9, 25))})
	wantStatuses(t, "#3", v, stLate, stLate)
	wantLeft(t, "#3", left)

	// 4. Границы окон не зависят от часа чека.
	night := time.Date(2026, 8, 2, 1, 0, 0, 0, msk)
	v, _, _ = hm([]reconItem{chk(15000, night)}, []cmf.Payment{pay(10000, dOnly(8, 15)), pay(5000, dOnly(8, 22))})
	wantStatuses(t, "#4 частями (+20)", v, stSplit)
	v, _, _ = hm([]reconItem{chk(10000, night)}, []cmf.Payment{pay(10000, dOnly(9, 16))})
	wantStatuses(t, "#4 поздно (+45)", v, stLate)
	v, _, _ = hm([]reconItem{chk(10000, at(8, 20, 15))}, []cmf.Payment{pay(10000, dOnly(8, 10))})
	wantStatuses(t, "#4 −10 дней", v, stDateCheck)

	// 5. Дата оплаты в программе с ошибкой, но внесли вовремя (CreatedAt).
	v, left, _ = hm([]reconItem{chk(15000, at(8, 10, 12))},
		[]cmf.Payment{{Amount: 15000, PaidAt: dOnly(7, 10), CreatedAt: time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)}})
	wantStatuses(t, "#5", v, stLate)
	if v[0].Note == "" {
		t.Error("#5: ожидали пояснение «поправь дату»")
	}
	wantLeft(t, "#5", left)

	// 7. Равные суммы подряд — без перекрёстных пар.
	v, _, _ = hm([]reconItem{chk(10000, at(8, 10, 15)), chk(10000, at(8, 11, 15))},
		[]cmf.Payment{pay(10000, dOnly(8, 10)), pay(10000, dOnly(8, 11))})
	if v[0].Pays[0] != 0 || v[1].Pays[0] != 1 {
		t.Errorf("#7: перекрёстные пары %v / %v", v[0].Pays, v[1].Pays)
	}

	// 8. Количество дней в пояснении — календарное.
	items := []reconItem{chk(10000, at(8, 14, 15))}
	pays := []cmf.Payment{pay(10000, dOnly(8, 10))}
	v, _, k := hm(items, pays)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "на 4 дн.") || !strings.Contains(s, "проверь") {
		t.Errorf("#8: %q", s)
	}
	items = []reconItem{chk(10000, at(8, 1, 15))}
	pays = []cmf.Payment{pay(10000, dOnly(8, 30))}
	v, _, k = hm(items, pays)
	if s := describeVerdict(items, pays, v, 0, k); !strings.Contains(s, "через 29 дн.") {
		t.Errorf("#8 поздно: %q", s)
	}
}

func TestReconRegressionAmounts(t *testing.T) {
	// 9. Единица сумм решается по всей сверке (здесь — рубли), одна оплата ×100 её не переворачивает.
	v, left, kop := humanMatch([]reconItem{chk(500, aug(10))}, []cmf.Payment{pay(50000, aug(10))}, unitsRubles)
	wantStatuses(t, "#9a", v, stNotEntered)
	wantLeft(t, "#9a", left, 0)
	if kop {
		t.Error("#9a: kopecks должно быть false")
	}
	v, _, _ = hm([]reconItem{chk(15000, aug(10)), chk(500, aug(11)), chk(500, aug(20))}, []cmf.Payment{pay(15000, aug(12)), pay(50000, aug(13))})
	wantStatuses(t, "#9b", v, stEntered, stNotEntered, stNotEntered)
	v, _, _ = hm([]reconItem{chk(1000, aug(5)), ctxChk(1000, mon(9, 5))}, []cmf.Payment{pay(100000, aug(1))})
	wantStatuses(t, "#9c", v, stNotEntered, stNotEntered)

	// 10. Копейки отбросили при вводе — это внесён, а не опечатка.
	v, _, _ = hm([]reconItem{chk(15000.50, aug(10))}, []cmf.Payment{pay(15000, aug(11))})
	wantStatuses(t, "#10a", v, stEntered)
	v, _, _ = hm([]reconItem{chk(14999.99, aug(10))}, []cmf.Payment{pay(14999, aug(11))})
	wantStatuses(t, "#10b", v, stEntered)

	// 11. «Одна цифра» не для сумм, отличающихся в разы.
	v, left, _ = hm([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(95000, aug(12))})
	wantStatuses(t, "#11a", v, stNotEntered)
	wantLeft(t, "#11a", left, 0)
	v, left, _ = hm([]reconItem{chk(15000, aug(10)), ctxChk(15000, mon(7, 10)), ctxChk(25000, mon(7, 12))},
		[]cmf.Payment{pay(15000, mon(7, 11)), pay(25000, mon(7, 13)), pay(25000, aug(13))})
	wantStatuses(t, "#11b", v, stNotEntered, stEntered, stEntered)
	wantLeft(t, "#11b", left, 2)

	// 12, 23. Суммы с копейками складываются точно.
	v, left, _ = hm([]reconItem{chk(10000.50, aug(10)), chk(4999.50, aug(11))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "#12a", v, stCombined, stCombined)
	wantLeft(t, "#12a", left)
	v, _, _ = hm([]reconItem{chk(300.50, aug(10)), chk(199.50, aug(11))}, []cmf.Payment{pay(500, aug(12))})
	wantStatuses(t, "#12b", v, stCombined, stCombined)
	v, _, _ = hm([]reconItem{chk(3333.50, aug(10)), chk(6666.50, aug(10))}, []cmf.Payment{pay(10000, aug(11))})
	wantStatuses(t, "#23", v, stCombined, stCombined)

	// 13. Сторно отменяет запись.
	v, _, _ = hm([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(15000, aug(12)), pay(-15000, aug(13))})
	wantStatuses(t, "#13", v, stNotEntered)
	var p cmf.Payment
	if err := json.Unmarshal([]byte(`{"amount":-15000}`), &p); err != nil || p.Amount != -15000 {
		t.Errorf("#13: отрицательная сумма разобрана как %d", p.Amount)
	}
}

func TestReconRegressionFixed(t *testing.T) {
	// 14. Раз в две недели, вносят через 12–13 дней; 31.08 не внесён.
	v, _, _ := hm([]reconItem{ctxChk(5000, mon(7, 20)), chk(5000, aug(3)), chk(5000, aug(17)), chk(5000, aug(31))},
		[]cmf.Payment{pay(5000, aug(1)), pay(5000, aug(16)), pay(5000, aug(30))})
	wantStatuses(t, "#14 раз в две недели", v, stEntered, stEntered, stEntered, stNotEntered)
	v, _, _ = hm([]reconItem{ctxChk(10000, at(7, 28, 12)), chk(10000, at(8, 4, 12))}, []cmf.Payment{pay(10000, dOnly(8, 3))})
	wantStatuses(t, "#14 ежемесячно", v, stEntered, stNotEntered)

	// 15. Поздняя оплата прошлого месяца — его, а не этого.
	v, _, _ = hm([]reconItem{ctxChk(10000, mon(7, 5)), chk(10000, aug(5))}, []cmf.Payment{pay(10000, mon(7, 30))})
	wantStatuses(t, "#15", v, stLate, stNotEntered)

	// 16. Ответ не зависит от порядка позиций.
	pays := []cmf.Payment{pay(10000, aug(16)), pay(10000, mon(9, 4))}
	R, X, C := chk(10000, aug(1)), ctxChk(10000, aug(15)), ctxChk(10000, aug(14))
	v1, _, _ := hm([]reconItem{R, X, C}, pays)
	v2, _, _ := hm([]reconItem{X, C, R}, pays)
	if v1[0].Status != stNotEntered || v2[2].Status != stNotEntered {
		t.Errorf("#16: R должен быть НЕ внесён при любом порядке: %v / %v", statuses(v1), statuses(v2))
	}

	// 17. Два разных чека в одной группе в одну минуту — две позиции.
	t0 := time.Date(2026, 8, 5, 10, 3, 0, 0, msk)
	out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Исаев Магомед", Amount: 5000, TxDate: t0, GroupJID: "g1", WaMessageID: "A1"},
		{ID: 2, Name: "Исаев Магомед", Amount: 5000, TxDate: t0.Add(25 * time.Second), GroupJID: "g1", WaMessageID: "A2"},
	})
	if len(out) != 2 {
		t.Errorf("#17: позиций %d, ожидали 2", len(out))
	}
}

func TestReconRegressionCombo(t *testing.T) {
	// 18. Три близких чека одной оплатой важнее пары с далёким чеком.
	v, _, _ := hm([]reconItem{chk(5000, aug(10)), chk(5000, aug(11)), chk(5000, aug(12)), chk(10000, mon(7, 26))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "#18", v, stCombined, stCombined, stCombined, stNotEntered)

	// 19. Две «пачки» подряд — обе одной оплатой.
	v, left, _ := hm([]reconItem{chk(10000, aug(1)), cashIt(5000, aug(1), true), chk(10000, aug(15)), cashIt(5000, aug(15), true)},
		[]cmf.Payment{pay(15000, aug(14)), pay(15000, aug(30))})
	wantStatuses(t, "#19", v, stCombined, stCombined, stCombined, stCombined)
	wantLeft(t, "#19", left)

	// 20. Тесная комбинация сразу после чеков важнее далёкой точной суммы.
	v, left, _ = hm([]reconItem{chk(5000, aug(1)), chk(5000, aug(2)), chk(5000, aug(3))}, []cmf.Payment{pay(15000, aug(4)), pay(5000, aug(20))})
	wantStatuses(t, "#20", v, stCombined, stCombined, stCombined)
	wantLeft(t, "#20", left, 1)

	// 21. Части — тому, после кого их внесли.
	v, left, _ = hm([]reconItem{chk(15000, aug(15)), cashIt(15000, aug(1), true)},
		[]cmf.Payment{pay(10000, aug(14)), pay(5000, aug(14)), pay(10000, aug(30)), pay(5000, aug(30))})
	wantStatuses(t, "#21", v, stSplit, stSplit)
	wantLeft(t, "#21", left)

	// 22. Чеки в 22 днях друг от друга — не «одной оплатой».
	v, _, _ = hm([]reconItem{chk(5000, mon(7, 30)), chk(10000, aug(21))}, []cmf.Payment{pay(15000, aug(19))})
	for i, x := range v {
		if x.Status == stCombined {
			t.Errorf("#22: позиция %d — ложное «одной оплатой»", i)
		}
	}
}

func TestReconRegressionContracts(t *testing.T) {
	// 24. Ровный сплит ранней позиции важнее «точной» оплаты, датированной ДО чека.
	v, left, _ := hm([]reconItem{chk(10000, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)), cashIt(15000, dOnly(8, 3), false)},
		[]cmf.Payment{{Amount: 10000, PaidAt: dOnly(8, 4), ContractNumber: 1}, {Amount: 5000, PaidAt: dOnly(8, 4), ContractNumber: 2}})
	wantStatuses(t, "#24 наличка", v, stNotEntered, stSplit)
	wantLeft(t, "#24 наличка", left)
	v, _, _ = hm([]reconItem{chk(15000, aug(10)), chk(5000, aug(12))}, []cmf.Payment{pay(10000, aug(11)), pay(5000, aug(11))})
	wantStatuses(t, "#24 чеки", v, stSplit, stNotEntered)

	// 25. Части, внесённые после контекстной налички, — её, а не чека.
	v, _, _ = hm([]reconItem{chk(10000, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)), cashIt(10000, dOnly(8, 8), false)},
		[]cmf.Payment{{Amount: 5000, PaidAt: dOnly(8, 9), ContractNumber: 1}, {Amount: 5000, PaidAt: dOnly(8, 9), ContractNumber: 2}})
	wantStatuses(t, "#25", v, stNotEntered, stSplit)

	// 26. Та же сумма на 10 дней РАНЬШЕ — «проверь», а не «внесён».
	v, _, _ = hm([]reconItem{chk(10000, aug(20))}, []cmf.Payment{pay(10000, aug(10))})
	wantStatuses(t, "#26", v, stDateCheck)
	if v[0].entered() || !v[0].needsCheck() {
		t.Error("#26: не должно считаться внесённым")
	}

	// 27. Лишняя оплата (внесли дважды) остаётся видимой.
	v, left, _ = hm([]reconItem{chk(10000, aug(10))}, []cmf.Payment{{Amount: 10000, PaidAt: aug(11), ContractNumber: 1}, {Amount: 10000, PaidAt: aug(11), ContractNumber: 2}})
	wantStatuses(t, "#27", v, stEntered)
	wantLeft(t, "#27", left, 1)

	// 28. Равные суммы и сплит: части — ближайшему, второму — подсказка.
	v, _, _ = hm([]reconItem{chk(15000, aug(10)), chk(15000, aug(20))}, []cmf.Payment{pay(10000, aug(21)), pay(5000, aug(21))})
	wantStatuses(t, "#28", v, stNotEntered, stSplit)
	if v[0].SameAmountAs != 1 {
		t.Errorf("#28: ожидали подсказку «одна из двух», SameAmountAs=%d", v[0].SameAmountAs)
	}
}

func TestReconRegressionData(t *testing.T) {
	t0 := time.Date(2026, 8, 10, 11, 32, 0, 0, time.UTC)
	// 29. Номер документа прочитан только на одной копии.
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g1", DocNumber: "1000123456", WaMessageID: "A"},
		{ID: 2, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g2", WaMessageID: "B"},
	}); len(out) != 1 {
		t.Errorf("#29: позиций %d, ожидали 1", len(out))
	}
	// 30. Другой порядок слов / падеж в другой группе.
	for _, other := range []string{"Ахмед Каталов", "Каталова Ахмеда"} {
		if out := dedupeReconReceipts([]db.ReconReceipt{
			{ID: 1, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g1", WaMessageID: "A"},
			{ID: 2, Name: other, Amount: 15000, TxDate: t0.Add(20 * time.Second), GroupJID: "g2", WaMessageID: "B"},
		}); len(out) != 1 {
			t.Errorf("#30 %q: позиций %d, ожидали 1", other, len(out))
		}
	}
	// 31. Одинаковый номер документа у разных клиентов с разницей в месяц — разные чеки.
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 5000, TxDate: t0, GroupJID: "g1", DocNumber: "142", WaMessageID: "A"},
		{ID: 2, Name: "Магомедов Руслан", Amount: 5000, TxDate: t0.AddDate(0, 1, 0), GroupJID: "g2", DocNumber: "142", WaMessageID: "B"},
	}); len(out) != 2 {
		t.Errorf("#31: позиций %d, ожидали 2", len(out))
	}
	// 35. Две пересылки одного оригинала без самого оригинала.
	if out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Иванова Хадижат", Amount: 15000, TxDate: t0, GroupJID: "g2", DocNumber: "555", WaMessageID: "M1-fwd-g2", NeedsReview: true},
		{ID: 2, Name: "Иванова Хадижат", Amount: 15000, TxDate: t0, GroupJID: "g3", WaMessageID: "M1-fwd-g3", NeedsReview: true},
	}); len(out) != 1 {
		t.Errorf("#35: позиций %d, ожидали 1", len(out))
	}
	// Пересылка с неподтверждённым именем склеивается с подтверждённым оригиналом.
	out := dedupeReconReceipts([]db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g1", WaMessageID: "M1"},
		{ID: 2, Name: "Хадижат Ибрагимова", Amount: 15000, TxDate: t0, GroupJID: "g2", WaMessageID: "M1-fwd-g2", NeedsReview: true},
	})
	if len(out) != 1 || out[0].Name != "Каталов Ахмед" || out[0].NeedsReview || !out[0].groups["g2"] {
		t.Errorf("пересылка: %+v", out)
	}
}

func TestSameClientName(t *testing.T) {
	yes := [][2]string{
		{"Каталов Ахмед", "Ахмед Каталов Нажудович"},
		{"Каталова Ахмеда", "Каталов Ахмед"},
		{"Пияна", "пияна"},
		{"Каталов А.", "Каталов Ахмед Русланович"}, // #32
		{"Сергей Майоров", "Сергей Майоров"},     // #36
	}
	for _, c := range yes {
		if !sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=false, ожидали true", c[0], c[1])
		}
	}
	no := [][2]string{
		{"Ахмед", "Ахмед Висаев"},
		{"Ахмед Катаев", "Ахмед Висаев"},
		{"Пияна", "Милана"},
		{"Каталов Б.", "Каталов Ахмед"},
		{"Ахмедов Магомед", "Магомедов Ахмед Русланович"}, // #33
		{"Магомедов Магомед", "Магомедов Магомедали"},     // #34
		{"Алиев Али", "Алиев Алибек"},                     // #34
	}
	for _, c := range no {
		if sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=true, ожидали false", c[0], c[1])
		}
	}
}
