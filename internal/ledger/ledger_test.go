package ledger

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)

func chk(id int, group, client string, cid int, amount float64, at time.Time, doc string) Check {
	return Check{ID: id, RawID: id, WaID: "w" + itoa(id), Group: group, ContactID: cid, Client: client, Confirmed: true,
		Amount: amount, OpDate: at, PostedAt: at.Add(time.Minute), Doc: doc}
}

func txt(id int, group, client string, cid int, amount float64, at time.Time, cash bool) Text {
	return Text{ID: id, RawID: 100 + id, Group: group, ContactID: cid, Client: client, Amount: amount, Cash: cash, OpDate: at, PostedAt: at}
}

func total(ps []*Payment) float64 {
	var s float64
	for _, p := range ps {
		s += p.Amount
	}
	return s
}

var all = Filter{From: t0.AddDate(0, 0, -10), To: t0.AddDate(0, 0, 10)}

func TestCheckAndTextAreOnePayment(t *testing.T) {
	b := Build([]Check{chk(1, "g", "Магомедов Ахмед", 1, 15000, t0, "D1")},
		[]Text{txt(1, "g", "Ахмед Магомедов", 0, 15000, t0.Add(-time.Hour), false)})
	if got := total(b.Counted(all)); got != 15000 {
		t.Fatalf("чек + текст = %v, хочу 15000", got)
	}
	if len(b.Left(all)) != 1 || b.Left(all)[0].Reason != ReasonTwin {
		t.Fatalf("текст должен быть дублем чека: %+v", b.Left(all))
	}
}

func TestTwoTextsOneCheckIsTwoPayments(t *testing.T) {
	b := Build([]Check{chk(1, "g", "Магомедов Ахмед", 1, 5000, t0, "")},
		[]Text{txt(1, "g", "Магомедов Ахмед", 1, 5000, t0, false), txt(2, "g", "Магомедов Ахмед", 1, 5000, t0.Add(time.Hour), false)})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("один чек и два текста = %v, хочу 10000 (второй текст — другой платёж)", got)
	}
}

func TestCashNeverTwin(t *testing.T) {
	b := Build([]Check{chk(1, "g", "Исаев Харон", 2, 7000, t0, "")},
		[]Text{txt(1, "g", "Исаев Харон", 2, 7000, t0, true)})
	if got := total(b.Counted(all)); got != 14000 {
		t.Fatalf("чек + наличка = %v, хочу 14000", got)
	}
}

func TestSameCheckTwoGroups(t *testing.T) {
	b := Build([]Check{chk(1, "sb", "Каталов", 3, 9000, t0, "OP-1"), chk(2, "main", "Каталов", 3, 9000, t0, "op1")}, nil)
	if got := total(b.Counted(all)); got != 9000 {
		t.Fatalf("тот же чек в двух группах = %v, хочу 9000", got)
	}
	if got := total(b.Counted(Filter{From: all.From, To: all.To, Groups: map[string]bool{"main": true}})); got != 9000 {
		t.Fatalf("в группе main = %v, хочу 9000", got)
	}
}

func TestDifferentChecksSameAmountSameGroup(t *testing.T) {
	b := Build([]Check{chk(1, "g", "Каталов", 3, 5000, t0, ""), chk(2, "g", "Каталов", 3, 5000, t0.Add(time.Minute), "")}, nil)
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("два чека одной группы = %v, хочу 10000", got)
	}
}

func TestForwardCopy(t *testing.T) {
	src := chk(1, "src", "Каталов", 3, 13000, t0, "")
	cp := chk(2, "dst", "Каталов", 3, 13000, t0, "")
	cp.FwdOrigin = src.WaID
	b := Build([]Check{src, cp}, nil)
	if got := total(b.Counted(all)); got != 13000 {
		t.Fatalf("исходный + копия = %v", got)
	}
	if got := total(b.Counted(Filter{From: all.From, To: all.To, Groups: map[string]bool{"dst": true}})); got != 13000 {
		t.Fatalf("в группе копии = %v", got)
	}
	src.Deleted = true
	b = Build([]Check{src, cp}, nil)
	if got := total(b.Counted(all)); got != 0 {
		t.Fatalf("исходный удалён — копия не считается: %v", got)
	}
}

func TestExcludedStates(t *testing.T) {
	a := chk(1, "g", "А", 1, 1000, t0, "")
	a.Deleted = true
	b := chk(2, "g", "Б", 2, 2000, t0, "")
	b.Ignored = true
	c := chk(3, "g", "В", 3, 3000, t0, "")
	c.Duplicate = true
	d := chk(4, "g", "Хадижат И.", 0, 4000, t0, "")
	d.Review = true
	held := txt(1, "g", "Г", 4, 5000, t0, true)
	held.Held = true
	book := Build([]Check{a, b, c, d}, []Text{held})
	if got := total(book.Counted(all)); got != 0 {
		t.Fatalf("ничего не должно считаться: %v", got)
	}
	reasons := map[Reason]int{}
	for _, e := range book.Left(all) {
		reasons[e.Reason]++
	}
	for _, r := range []Reason{ReasonDeleted, ReasonIgnored, ReasonDuplicate, ReasonReview, ReasonHeld} {
		if reasons[r] != 1 {
			t.Errorf("нет причины %q: %v", r, reasons)
		}
	}
}

func TestDeletedCheckReleasesText(t *testing.T) {
	c := chk(1, "g", "Магомедов", 1, 5000, t0, "")
	c.Deleted = true
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов", 1, 5000, t0, false)})
	if got := total(b.Counted(all)); got != 5000 {
		t.Fatalf("чек удалён — текст считается: %v", got)
	}
}

func TestChatDateBasisAndOldChecks(t *testing.T) {
	old := chk(1, "g", "Каталов", 3, 9000, t0.AddDate(0, -1, 0), "")
	old.PostedAt = t0
	b := Build([]Check{old}, nil)
	f := Filter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour), ByChat: true,
		MonthStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), MonthEnd: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)}
	if got := total(b.Counted(f)); got != 0 {
		t.Fatalf("старый чек не в сумме: %v", got)
	}
	if len(b.OldChecks(f)) != 1 {
		t.Fatalf("старый чек должен быть отдельно")
	}
}

func TestTwinWindow(t *testing.T) {
	b := Build([]Check{chk(1, "g", "Магомедов", 1, 5000, t0, "")},
		[]Text{txt(1, "g", "Магомедов", 1, 5000, t0.Add(-3*24*time.Hour), false)})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("текст за 3 дня до чека — другой платёж: %v", got)
	}
}
