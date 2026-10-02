package bot

import (
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
)

func aug(day int) time.Time { return time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC) }
func mon(m time.Month, day int) time.Time {
	return time.Date(2026, m, day, 12, 0, 0, 0, time.UTC)
}

func chk(amount float64, t time.Time) reconItem {
	return reconItem{Kind: "check", Amount: amount, Date: t, Report: true}
}
func pay(amount int64, t time.Time) cmf.Payment { return cmf.Payment{Amount: amount, PaidAt: t} }

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

// --- Инварианты старого сопоставителя (должны сохраниться) ---

func TestHumanMatchLegacyInvariants(t *testing.T) {
	// 1) чек 20.08, оплата 24.08 той же суммы → внесён.
	v, left, _ := humanMatch([]reconItem{chk(20000, aug(20))}, []cmf.Payment{pay(20000, aug(24))})
	wantStatuses(t, "одиночный", v, stEntered)
	if len(left) != 0 {
		t.Errorf("одиночный: лишние оплаты %v", left)
	}

	// 2) два чека 20.08 и 21.08 по 20000, оплата одна 24.08 → засчитан ближайший
	//    (21.08), 20.08 НЕ внесён с пометкой «сумма как у чека 21.08».
	v, _, _ = humanMatch([]reconItem{chk(20000, aug(20)), chk(20000, aug(21))}, []cmf.Payment{pay(20000, aug(24))})
	wantStatuses(t, "два чека/одна оплата", v, stNotEntered, stEntered)
	if v[0].SameAmountAs != 1 {
		t.Errorf("ожидали пометку «одна из двух» (SameAmountAs=1), получили %d", v[0].SameAmountAs)
	}

	// 3) копейки.
	v, _, kop := humanMatch([]reconItem{chk(15000, aug(5))}, []cmf.Payment{pay(1500000, aug(5))})
	wantStatuses(t, "копейки", v, stEntered)
	if !kop {
		t.Error("копейки: ожидали kopecks=true")
	}

	// 4) разные суммы не путаются.
	v, _, _ = humanMatch([]reconItem{chk(20000, aug(10)), chk(25000, aug(11))},
		[]cmf.Payment{pay(25000, aug(12)), pay(20000, aug(13))})
	wantStatuses(t, "разные суммы", v, stEntered, stEntered)

	// 5) оплата без чека остаётся лишней.
	v, left, _ = humanMatch([]reconItem{chk(20000, aug(10))}, []cmf.Payment{pay(20000, aug(10)), pay(5000, aug(10))})
	wantStatuses(t, "лишняя оплата", v, stEntered)
	if len(left) != 1 {
		t.Errorf("лишняя оплата: leftover=%v, ожидали одну (5000)", left)
	}

	// 6) единая единица: 100 ₽ не «крадёт» 10000.
	v, _, kop = humanMatch([]reconItem{chk(10000, aug(1)), chk(100, aug(2))},
		[]cmf.Payment{pay(10000, aug(3)), pay(100, aug(4))})
	wantStatuses(t, "единица", v, stEntered, stEntered)
	if kop {
		t.Error("единица: kopecks должно быть false")
	}

	// 7) оплата ПРОШЛОГО месяца той же суммы не закрывает чек этого месяца.
	v, _, _ = humanMatch([]reconItem{chk(20000, aug(20))}, []cmf.Payment{pay(20000, mon(7, 20))})
	wantStatuses(t, "прошлый месяц", v, stNotEntered)

	// 8) оплата на 2 дня раньше чека — засчитывается.
	v, _, _ = humanMatch([]reconItem{chk(30000, aug(10))}, []cmf.Payment{pay(30000, aug(8))})
	wantStatuses(t, "на 2 дня раньше", v, stEntered)

	// 9) максимум совпадений при неполном графе.
	v, _, _ = humanMatch([]reconItem{chk(15000, aug(10)), chk(15000, aug(1))},
		[]cmf.Payment{pay(15000, aug(10)), pay(15000, aug(25))})
	wantStatuses(t, "максимум", v, stEntered, stEntered)
}

// --- «Как человек» ---

// Пример владельца: чеки 10.08 (15000) и 12.08 (8000), в программе одна оплата
// 14.08 на сумму чека от 10-го → 10.08 внесён оплатой 14.08, 12.08 — НЕ внесён.
func TestHumanMatchOwnerExample(t *testing.T) {
	v, left, _ := humanMatch([]reconItem{chk(15000, aug(10)), chk(8000, aug(12))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "пример владельца", v, stEntered, stNotEntered)
	if v[1].SameAmountAs != -1 {
		t.Error("у 12.08 другая сумма — пометки «одна из двух» быть не должно")
	}
	if len(left) != 0 {
		t.Errorf("лишних оплат быть не должно: %v", left)
	}
}

// Фиксированный платёж: сентябрьский чек 28.09 внесли 02.10, октябрьский 05.10 —
// НЕТ. Оплата 02.10 принадлежит сентябрьскому (контекст), а не октябрьскому.
func TestHumanMatchNeighbourMonth(t *testing.T) {
	sep := reconItem{Kind: "check", Amount: 10000, Date: mon(9, 28)} // контекст
	oct := chk(10000, mon(10, 5))
	v, _, _ := humanMatch([]reconItem{oct, sep}, []cmf.Payment{pay(10000, mon(10, 2))})
	wantStatuses(t, "соседний месяц", v, stNotEntered, stEntered)
}

// Два чека внесли ОДНОЙ оплатой.
func TestHumanMatchCombined(t *testing.T) {
	v, left, _ := humanMatch([]reconItem{chk(10000, aug(10)), chk(5000, aug(11))}, []cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "одной оплатой", v, stCombined, stCombined)
	if len(left) != 0 {
		t.Errorf("оплата должна быть использована: %v", left)
	}
}

// Точное совпадение важнее «одной оплатой».
func TestHumanMatchExactBeatsCombined(t *testing.T) {
	v, _, _ := humanMatch([]reconItem{chk(10000, aug(10)), chk(5000, aug(11)), chk(15000, aug(12))},
		[]cmf.Payment{pay(15000, aug(14))})
	wantStatuses(t, "точное важнее", v, stNotEntered, stNotEntered, stEntered)
}

// Один чек внесли частями.
func TestHumanMatchSplit(t *testing.T) {
	v, _, _ := humanMatch([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(10000, aug(14)), pay(5000, aug(16))})
	wantStatuses(t, "частями", v, stSplit)
	if len(v[0].Pays) != 2 {
		t.Errorf("частями: ожидали 2 оплаты, получили %v", v[0].Pays)
	}
}

// Внесли поздно (через 29 дней).
func TestHumanMatchLate(t *testing.T) {
	v, _, _ := humanMatch([]reconItem{chk(15000, aug(1))}, []cmf.Payment{pay(15000, aug(30))})
	wantStatuses(t, "поздно", v, stLate)
}

// Ошибка в сумме при вводе.
func TestHumanMatchSlips(t *testing.T) {
	cases := []struct {
		got  int64
		want reconStatus
		note string
	}{
		{1500, stSuspicious, "потеряли ноль"},
		{150000, stSuspicious, "лишний ноль"},
		{16000, stSuspicious, "одна цифра не та"},
		{14800, stSuspicious, "внесли чуть меньше"},
		{5000, stNotEntered, ""}, // просто другая оплата — не выдумываем «ошибку»
	}
	for _, c := range cases {
		v, _, _ := humanMatch([]reconItem{chk(15000, aug(10))}, []cmf.Payment{pay(c.got, aug(12))})
		if v[0].Status != c.want || v[0].Note != c.note {
			t.Errorf("оплата %d: статус %v «%s», ожидали %v «%s»", c.got, v[0].Status, v[0].Note, c.want, c.note)
		}
	}
	if s := amountSlip(15000, 51000); s != "переставили цифры" && s != "" {
		t.Logf("перестановка: %q", s)
	}
	if s := amountSlip(15600, 16500); s != "переставили цифры" {
		t.Errorf("перестановка цифр: %q", s)
	}
}

// Наличка клиента тоже «забирает» свою оплату: оплата 05.08 — за наличку 05.08,
// а не за чек 07.08 той же суммы.
func TestHumanMatchCashConsumes(t *testing.T) {
	cash := reconItem{Kind: "cash", Amount: 10000, Date: aug(5), Report: true}
	v, _, _ := humanMatch([]reconItem{cash, chk(10000, aug(7))}, []cmf.Payment{pay(10000, aug(5))})
	wantStatuses(t, "наличка", v, stEntered, stNotEntered)
}

// Копии одного чека из разных групп склеиваются в одну позицию.
func TestDedupeReconReceipts(t *testing.T) {
	t0 := mon(10, 2)
	rs := []db.ReconReceipt{
		{ID: 1, Name: "Каталов Ахмед", Amount: 15000, TxDate: t0, GroupJID: "g1", WaMessageID: "M1"},
		{ID: 2, Name: "Хадижат Ибрагимова", Amount: 15000, TxDate: t0, GroupJID: "g2", WaMessageID: "M1-fwd-g2", NeedsReview: true},
		{ID: 3, Name: "Исаев", Amount: 9000, TxDate: t0, GroupJID: "g1", DocNumber: "777", WaMessageID: "M3"},
		{ID: 4, Name: "Другое имя", Amount: 9000, TxDate: t0.Add(time.Hour), GroupJID: "g3", DocNumber: "777", WaMessageID: "M4"},
	}
	out := dedupeReconReceipts(rs)
	if len(out) != 2 {
		t.Fatalf("ожидали 2 позиции после склейки, получили %d", len(out))
	}
	if out[0].Name != "Каталов Ахмед" || out[0].NeedsReview || !out[0].groups["g2"] {
		t.Errorf("пересылка: ожидали подтверждённое имя и обе группы, получили %+v", out[0])
	}
	if !out[1].groups["g3"] {
		t.Errorf("тот же номер документа в другой группе должен склеиться: %+v", out[1])
	}
}

func TestSameClientName(t *testing.T) {
	yes := [][2]string{
		{"Каталов Ахмед", "Ахмед Каталов Нажудович"},
		{"Каталова Ахмеда", "Каталов Ахмед"},
		{"Пияна", "пияна"},
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
	}
	for _, c := range no {
		if sameClientName(c[0], c[1]) {
			t.Errorf("sameClientName(%q,%q)=true, ожидали false", c[0], c[1])
		}
	}
}
