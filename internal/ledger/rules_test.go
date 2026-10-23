package ledger

import (
	"testing"
	"time"
)

// Правила склейки, добавленные после проверки движка (2026-10-18).

func inGroup(g string) Filter {
	f := all
	f.Groups = map[string]bool{g: true}
	return f
}

// Тот же чек и тот же текст в двух группах — один платёж везде.
func TestSameCheckAndTextInTwoGroups(t *testing.T) {
	b := Build([]Check{
		chk(1, "sb", "Магомедов Ахмед", 1, 15000, t0, "D123456789"),
		chk(2, "main", "Магомедов Ахмед", 1, 15000, t0, "D123456789"),
	}, []Text{
		txt(1, "sb", "Магомедов Ахмед", 1, 15000, t0.Add(2*time.Minute), false),
		txt(2, "main", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false),
	})
	for _, f := range []Filter{all, inGroup("sb"), inGroup("main")} {
		if got := total(b.Counted(f)); got != 15000 {
			t.Fatalf("группы %v: %v, хочу 15000", f.Groups, got)
		}
	}
}

// Текст в другой группе (без чека там) — тот же платёж, что чек.
func TestTextReportedInSecondGroup(t *testing.T) {
	b := Build([]Check{chk(1, "sb", "Магомедов Ахмед", 1, 15000, t0, "")},
		[]Text{
			txt(1, "sb", "Магомедов Ахмед", 1, 15000, t0.Add(-time.Minute), false),
			txt(2, "main", "Магомедов Ахмед", 1, 15000, t0.Add(10*time.Minute), false),
		})
	if got := total(b.Counted(all)); got != 15000 {
		t.Fatalf("все группы %v, хочу 15000", got)
	}
}

// Ближайший текст берёт чек: вчерашний перевод без чека остаётся вчерашним.
func TestPairingByCloseness(t *testing.T) {
	yesterday := t0.Add(-15 * time.Hour)
	b := Build([]Check{chk(1, "g", "Магомедов Ахмед", 1, 5000, t0, "")},
		[]Text{
			txt(1, "g", "Магомедов Ахмед", 1, 5000, yesterday, false),
			txt(2, "g", "Магомедов Ахмед", 1, 5000, t0.Add(2*time.Minute), false),
		})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("всего %v, хочу 10000", got)
	}
	for _, p := range b.Payments {
		if p.Main() == nil && !p.OpDate.Equal(yesterday) {
			t.Fatalf("отдельным платежом должен остаться вчерашний текст, а остался %v", p.OpDate)
		}
	}
}

// Чек прислали через выходные: дата операции рядом с текстом — тот же платёж.
func TestLateCheckPairsByOperationDate(t *testing.T) {
	c := chk(1, "g", "Магомедов Ахмед", 1, 15000, t0, "")
	c.PostedAt = t0.Add(72 * time.Hour)
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(10*time.Minute), false)})
	if got := total(b.Counted(all)); got != 15000 {
		t.Fatalf("%v, хочу 15000", got)
	}
}

// Платёж частями: два чека и текст-итог — сумма чеков.
func TestSplitPaymentTotalText(t *testing.T) {
	b := Build([]Check{
		chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, ""),
		chk(2, "g", "Магомедов Ахмед", 1, 5000, t0.Add(time.Minute), ""),
	}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false)})
	if got := total(b.Counted(all)); got != 15000 {
		t.Fatalf("%v, хочу 15000", got)
	}
	left := b.Left(all)
	if len(left) != 1 || left[0].Reason != ReasonSplit || len(left[0].Parts) != 2 {
		t.Fatalf("текст должен быть итогом двух чеков: %+v", left)
	}
}

// Итог частями не склеивается с чеками другого клиента и с наличкой.
func TestSplitNeedsSameClientAndTransfer(t *testing.T) {
	b := Build([]Check{
		chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, ""),
		chk(2, "g", "Исаев Харон", 2, 5000, t0.Add(time.Minute), ""),
	}, []Text{
		txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false),
		txt(2, "g", "Магомедов Ахмед", 1, 15000, t0.Add(6*time.Minute), true),
	})
	if got := total(b.Counted(all)); got != 45000 {
		t.Fatalf("%v, хочу 45000", got)
	}
}

// «Ахмед 10000» от того же сотрудника рядом с чеком «Магомедов Ахмед» — тот же платёж.
func TestShortNameTextPairs(t *testing.T) {
	c := chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, "")
	c.Phone = "7900"
	tx := txt(1, "g", "Ахмед", 2, 10000, t0.Add(3*time.Minute), false)
	tx.Phone = "7900"
	b := Build([]Check{c}, []Text{tx})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("%v, хочу 10000", got)
	}
	// Через сутки в другой группе от другого человека — другой платёж.
	tx2 := txt(2, "other", "Ахмед", 2, 10000, t0.Add(20*time.Hour), false)
	tx2.Phone = "7911"
	b = Build([]Check{c}, []Text{tx2})
	if got := total(b.Counted(all)); got != 20000 {
		t.Fatalf("далёкий текст «Ахмед» не должен склеиться: %v", got)
	}
}

// Один номер документа, сумму в одной группе поправили — один платёж с исправленной суммой.
func TestSameDocCorrectedAmount(t *testing.T) {
	a := chk(1, "sb", "Исаев Харон", 1, 13000, t0, "1234567890")
	a.Fixed = true
	m := chk(2, "main", "Исаев Харон", 1, 15000, t0, "1234567890")
	m.PostedAt = a.PostedAt.Add(-time.Minute) // в основной прислали раньше
	b := Build([]Check{a, m}, nil)
	if got := total(b.Counted(all)); got != 13000 {
		t.Fatalf("%v, хочу 13000", got)
	}
}

// Без номера: разные получатели или плательщики — разные переводы, даже если
// клиентов можно спутать.
func TestNoDocDifferentPrintedFields(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 5000, t0, "")
	a.Printed, a.Payer = "Хадижат Ибрагимовна И.", "Зарема Б."
	c := chk(2, "b", "Магомедов Ахмед", 1, 5000, t0.Add(time.Minute), "")
	c.Printed, c.Payer = "Мурад Исаевич К.", "Луиза Ю."
	b := Build([]Check{a, c}, nil)
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("%v, хочу 10000", got)
	}
}

// Тот же чек в другой группе ждёт «чей чек?» — он часть посчитанного платежа и
// виден в отчёте той группы; два непроверенных экземпляра — один в «не вошло».
func TestReviewCopyOfCountedCheck(t *testing.T) {
	a := chk(1, "sb", "Магомедов Ахмед", 1, 15000, t0, "9876543210")
	r := chk(2, "main", "Хадижат Ибрагимовна И.", 0, 15000, t0, "9876543210")
	r.Review, r.Confirmed = true, false
	b := Build([]Check{a, r}, nil)
	if got := total(b.Counted(inGroup("main"))); got != 15000 {
		t.Fatalf("основная группа: %v, хочу 15000", got)
	}
	if len(b.Left(all)) != 0 {
		t.Fatalf("непроверенная копия посчитанного чека не должна висеть в «не вошло»: %+v", b.Left(all))
	}

	x := chk(3, "sb", "", 0, 20000, t0, "5555555555")
	x.Review = true
	y := chk(4, "main", "", 0, 20000, t0, "5555555555")
	y.Review = true
	b = Build([]Check{x, y}, nil)
	roots := map[*Check]bool{}
	for _, e := range b.Left(all) {
		roots[e.Root()] = true
	}
	if len(b.Left(all)) != 2 || len(roots) != 1 {
		t.Fatalf("два экземпляра одного непроверенного чека — один корень: %+v", b.Left(all))
	}
}

// Месяц большой сети: 20 тыс. чеков и 30 тыс. текстов собираются быстро.
func TestBuildIsFastOnBigVolume(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	names := []string{"Магомедов Ахмед", "Исаев Харон", "Омаров Магомед", "Умаров Магомед", "Алиев Руслан", "Гаджиев Шамиль"}
	var amounts []float64
	for a := 1000.0; a <= 30000; a += 1000 {
		amounts = append(amounts, a)
	}
	var cs []Check
	var ts []Text
	for i := 0; i < 20000; i++ {
		at := t0.Add(time.Duration(i) * 2 * time.Minute)
		c := chk(i+1, "g"+itoa(i%5), names[i%6]+" "+itoa(i%400), i%2400+1, amounts[(i*7)%len(amounts)], at, "")
		if i%3 == 0 {
			c.Doc = "D" + itoa(1000000+i)
		}
		cs = append(cs, c)
	}
	for i := 0; i < 30000; i++ {
		at := t0.Add(time.Duration(i)*80*time.Second + 30*time.Second)
		ts = append(ts, txt(i+1, "g"+itoa(i%5), names[i%6]+" "+itoa(i%400), i%2400+1, amounts[(i*11)%len(amounts)], at, i%4 == 0))
	}
	start := time.Now()
	b := Build(cs, ts)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Build 20k чеков + 30k текстов: %v", d)
	}
	if len(b.Payments) == 0 {
		t.Fatal("пустая книга")
	}
}

// Текст, написанный задолго до операции на чеке, — про другой платёж.
func TestTextLongBeforeOperationIsAnotherPayment(t *testing.T) {
	op := t0.Add(7 * time.Minute) // не ровно 12:00 — время прочитано с чека
	c := chk(1, "g", "Магомедов Ахмед", 1, 5000, op, "")
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 5000, op.Add(-15*time.Hour), false)})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("%v, хочу 10000", got)
	}
	// Время на чеке не прочитано — сравниваем только по времени присылки.
	c.TimeGuessed = true
	c.PostedAt = op.Add(-14 * time.Hour)
	b = Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 5000, op.Add(-15*time.Hour), false)})
	if got := total(b.Counted(all)); got != 5000 {
		t.Fatalf("время чека не прочитано, текст за час до присылки: %v, хочу 5000", got)
	}
}

// Итог частями, когда одна часть ждёт имени: итог сверх чеков не считается.
func TestSplitTotalWithWaitingPart(t *testing.T) {
	a := chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, "")
	a.Phone = "7901"
	w := chk(2, "g", "Хадижат Ибрагимовна И.", 0, 5000, t0.Add(time.Minute), "")
	w.Review, w.Confirmed, w.Phone = true, false, "7901"
	tx := txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false)
	tx.Phone = "7901"
	b := Build([]Check{a, w}, []Text{tx})
	if got := total(b.Counted(all)); got != 10000 {
		t.Fatalf("%v, хочу 10000 (5000 ждёт имени)", got)
	}
	var split *Excluded
	for i, e := range b.Excluded {
		if e.Reason == ReasonSplit {
			split = &b.Excluded[i]
		}
	}
	if split == nil || len(split.Parts) != 1 || len(split.Missing) != 1 {
		t.Fatalf("итог должен ссылаться на посчитанный и ждущий чек: %+v", b.Excluded)
	}
}

// Тот же итог частями во второй группе — тот же платёж.
func TestSplitTotalInSecondGroup(t *testing.T) {
	b := Build([]Check{
		chk(1, "sb", "Магомедов Ахмед", 1, 10000, t0, ""),
		chk(2, "sb", "Магомедов Ахмед", 1, 5000, t0.Add(time.Minute), ""),
	}, []Text{
		txt(1, "sb", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false),
		txt(2, "main", "Магомедов Ахмед", 1, 15000, t0.Add(20*time.Minute), false),
	})
	for _, f := range []Filter{all, inGroup("sb"), inGroup("main")} {
		if got := total(b.Counted(f)); got != 15000 {
			t.Fatalf("группы %v: %v, хочу 15000", f.Groups, got)
		}
	}
}

// Непроверенный чек без номера с другой минутой операции — другой перевод.
func TestReviewCopyNeedsExactTime(t *testing.T) {
	a := chk(1, "a", "Магомедов Ахмед", 1, 5000, t0, "")
	a.Printed, a.Payer = "Хадижат Ибрагимовна И.", "Зарема Б."
	r := chk(2, "b", "", 0, 5000, t0.Add(time.Minute), "")
	r.Review, r.Confirmed, r.Printed, r.Payer = true, false, "Хадижат Ибрагимовна И.", "Зарема Б."
	b := Build([]Check{a, r}, nil)
	if len(b.Left(all)) != 1 || b.Left(all)[0].Reason != ReasonReview {
		t.Fatalf("чек с другой минутой — в «не вошло»: %+v", b.Left(all))
	}
	r.OpDate = t0
	b = Build([]Check{a, r}, nil)
	if len(b.Left(all)) != 0 || total(b.Counted(inGroup("b"))) != 5000 {
		t.Fatalf("тот же чек (всё совпало) — часть платежа: %+v", b.Left(all))
	}
}

// Один номер документа, разные клиенты в подписях — один платёж с пометкой.
func TestSameDocDifferentClientsConflict(t *testing.T) {
	b := Build([]Check{
		chk(1, "sb", "Магомедов Ахмед", 1, 15000, t0, "1234567890"),
		chk(2, "main", "Исаев Харон", 2, 15000, t0, "1234567890"),
	}, nil)
	if len(b.Payments) != 1 || len(b.Payments[0].Conflicts) != 1 {
		t.Fatalf("один платёж с конфликтом подписей: %+v", b.Payments)
	}
}

// По дате в чате платёж датируется самой ранней уликой в месяце операции: текст
// про платёж — днём текста, даже если чек прислали позже; если в месяце
// операции улик нет — присылкой чека (это «старый чек»).
func TestByChatDatesByEarliestEvidenceInMonth(t *testing.T) {
	c := chk(1, "g", "Магомедов Ахмед", 1, 15000, t0, "")
	c.PostedAt = t0.Add(6 * 24 * time.Hour)
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false)})
	day := func(d time.Time) Filter {
		from := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
		return Filter{From: from, To: from.AddDate(0, 0, 1), ByChat: true}
	}
	if got := total(b.Counted(day(t0))); got != 15000 {
		t.Fatalf("день текста по дате чата: %v, хочу 15000", got)
	}
	if got := total(b.Counted(day(c.PostedAt))); got != 0 {
		t.Fatalf("день присылки чека: %v, хочу 0", got)
	}
	// Текст 30.09 23:58, операция 30.09 23:57, чек прислан 01.10 00:03 — сентябрь.
	op := time.Date(2026, 9, 30, 23, 57, 0, 0, time.Local)
	c2 := chk(2, "g", "Магомедов Ахмед", 1, 15000, op, "")
	c2.PostedAt = time.Date(2026, 10, 1, 0, 3, 0, 0, time.Local)
	b = Build([]Check{c2}, []Text{txt(2, "g", "Магомедов Ахмед", 1, 15000, op.Add(time.Minute), false)})
	sep := Filter{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), To: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), ByChat: true,
		MonthStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), MonthEnd: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)}
	if got := total(b.Counted(sep)); got != 15000 {
		t.Fatalf("сентябрь по дате чата: %v, хочу 15000", got)
	}
}

// Итог частями: итог в группе без чеков (или написан там раньше) — тот же платёж.
func TestSplitTotalOrderIndependent(t *testing.T) {
	cs := []Check{
		chk(1, "sb", "Магомедов Ахмед", 1, 10000, t0, ""),
		chk(2, "sb", "Магомедов Ахмед", 1, 5000, t0.Add(time.Minute), ""),
	}
	for _, ts := range [][]Text{
		{txt(1, "main", "Магомедов Ахмед", 1, 15000, t0.Add(4*time.Minute), false)},
		{txt(1, "main", "Магомедов Ахмед", 1, 15000, t0.Add(4*time.Minute), false), txt(2, "sb", "Магомедов Ахмед", 1, 15000, t0.Add(6*time.Minute), false)},
	} {
		b := Build(append([]Check(nil), cs...), ts)
		if got := total(b.Counted(all)); got != 15000 {
			t.Fatalf("тексты %d: %v, хочу 15000", len(ts), got)
		}
	}
}

// Недостающая часть итога — только от того же работника: чужой чек без имени
// не «съедает» отдельный перевод.
func TestSplitMissingPartSameSenderOnly(t *testing.T) {
	a := chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, "")
	a.Phone = "7901"
	w := chk(2, "g", "", 0, 5000, t0.Add(40*time.Minute), "")
	w.Review, w.Confirmed, w.Phone = true, false, "7902"
	tx := txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(90*time.Minute), false)
	tx.Phone = "7901"
	b := Build([]Check{a, w}, []Text{tx})
	if got := total(b.Counted(all)); got != 25000 {
		t.Fatalf("%v, хочу 25000 (чек 10000 + отдельный перевод 15000)", got)
	}
}

// Вторая часть подписана только фамилией — тот же клиент для итога.
func TestSplitShortNamePart(t *testing.T) {
	a := chk(1, "g", "Магомедов Ахмед", 1, 10000, t0, "")
	c := chk(2, "g", "Магомедов", 2, 5000, t0.Add(time.Minute), "")
	b := Build([]Check{a, c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 15000, t0.Add(5*time.Minute), false)})
	if got := total(b.Counted(all)); got != 15000 {
		t.Fatalf("%v, хочу 15000", got)
	}
}

// Чек с одной датой (время подставлено 12:00) не мешает паре с утренним текстом.
func TestDateOnlyCheckPairsWithMorningText(t *testing.T) {
	op := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	c := chk(1, "g", "Магомедов Ахмед", 1, 15000, op, "")
	c.PostedAt = time.Date(2026, 10, 14, 13, 0, 0, 0, time.Local)
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 15000, time.Date(2026, 10, 14, 8, 40, 0, 0, time.Local), false)})
	if got := total(b.Counted(Filter{From: op.AddDate(0, 0, -1), To: op.AddDate(0, 0, 1)})); got != 15000 {
		t.Fatalf("%v, хочу 15000", got)
	}
}

// Время операции на чеке позже его присылки (ошибка чтения, другой часовой
// пояс) — правилу «текст задолго до операции» не мешает.
func TestFutureOperationTimeIgnored(t *testing.T) {
	c := chk(1, "g", "Магомедов Ахмед", 1, 10000, t0.Add(9*time.Hour), "")
	c.PostedAt = t0
	b := Build([]Check{c}, []Text{txt(1, "g", "Магомедов Ахмед", 1, 10000, t0.Add(2*time.Minute), false)})
	if got := total(b.Counted(Filter{From: t0.AddDate(0, 0, -10), To: t0.AddDate(0, 0, 10)})); got != 10000 {
		t.Fatalf("%v, хочу 10000", got)
	}
}

// По дате в чате сбор за период (даже через границу месяца) равен сумме по
// дням: «старый чек» решается по самому платежу, а не по месяцу начала запроса.
func TestByChatRangeEqualsDayByDay(t *testing.T) {
	// операция 25.09, прислали 05.10 — для октября это «старый чек».
	c := chk(1, "g", "Исаев Харон", 2, 20000, time.Date(2026, 9, 25, 11, 0, 0, 0, time.Local), "DOC87654321")
	c.PostedAt = time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local)
	// текст того же дня, что операция (сентябрь) — обычный платёж по дате чата.
	tx := txt(2, "g", "Магомедов Ахмед", 1, 7000, time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local), false)
	b := Build([]Check{c}, []Text{tx})

	chatF := func(from, toExcl time.Time) Filter {
		last := toExcl.AddDate(0, 0, -1)
		return Filter{From: from, To: toExcl, ByChat: true,
			MonthStart: time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location()),
			MonthEnd:   time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, last.Location()).AddDate(0, 1, 0)}
	}
	from, toExcl := time.Date(2026, 9, 15, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 16, 0, 0, 0, 0, time.Local)
	rng := total(b.Counted(chatF(from, toExcl)))
	var day float64
	for d := from; d.Before(toExcl); d = d.AddDate(0, 0, 1) {
		day += total(b.Counted(chatF(d, d.AddDate(0, 0, 1))))
	}
	if rng != day {
		t.Fatalf("по дате в чате: период %v, по дням %v", rng, day)
	}
	if rng != 7000 {
		t.Fatalf("в сумме только сентябрьский текст 7000 (чек 25.09 прислан 05.10 — старый), а получили %v", rng)
	}
}
