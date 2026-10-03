// «Человеческая» сверка оплат клиента с программой рассрочек.
//
// Как рассуждает бухгалтер: у клиента есть чеки/наличка (по нашему учёту) и
// оплаты в программе (по всем его рассрочкам). Ищем САМОЕ ПРАВДОПОДОБНОЕ
// объяснение всей картины сразу, а не хватаем по одному «ближайшее»:
//
//   - та же сумма, внесли в день чека или после — естественно;
//   - внесли на пару дней РАНЬШЕ чека — бывает (опечатка в дате), но реже;
//   - внесли сильно позже (3–6 недель) — «внесён поздно»;
//   - та же сумма, но дата на 4–10 дней раньше чека — «проверь дату»;
//   - похоже на ошибку ввода суммы (ноль, цифра, запятая) — «проверь сумму»;
//   - несколько чеков одной оплатой / один чек частями — реже; и такое
//     объяснение должно выигрывать ПЛОТНОСТЬЮ по датам, а не числом позиций;
//   - чек без оплаты — «НЕ внесён»; оплата без чека — тоже требует объяснения;
//   - сторно отменяет запись, сделанную ДО него, — какую именно, решает та же
//     оценка правдоподобия (дубль, запись с ошибочной датой).
//
// Каждому варианту — «цена неправдоподобия»; минимизируем суммарную цену:
// 1:1 — точно (венгерский алгоритм, по независимым группам), «одной оплатой/
// частями» — итеративным улучшением с точным пересчётом. Даты — календарные дни
// по Москве. Ответ не зависит от порядка входных данных.
package bot

import (
	"math"
	"sort"
	"strconv"
	"time"

	"whatsapp-bot/internal/cmf"
)

// reconLoc — календарь бизнеса (Москва, без перехода на летнее время).
var reconLoc = time.FixedZone("MSK", 3*3600)

// reconStatus — вердикт по позиции.
type reconStatus int

const (
	stNotEntered reconStatus = iota
	stEntered                // та же сумма, внесли в обычный срок
	stCombined               // внесён вместе с другими одной оплатой
	stSplit                  // внесён частями
	stLate                   // внесён, но поздно / с ошибкой в дате оплаты
	stSuspicious             // похоже, ошиблись в сумме — проверить
	stDateCheck              // та же сумма, но дата заметно РАНЬШЕ чека — проверить
)

// reconItem — то, что клиент заплатил по нашему учёту (чек или наличка).
type reconItem struct {
	ID     int
	Kind   string // "check" | "cash"
	Amount float64
	Date   time.Time
	Report bool // показать в ответе; false — только контекст сопоставления
}

// reconVerdict — объяснение по одной позиции.
type reconVerdict struct {
	Status       reconStatus
	Pays         []int  // индексы оплат (в исходном срезе), закрывших позицию
	With         []int  // для stCombined — другие позиции той же оплаты
	SameAmountAs int    // для «НЕ внесён»: позиция с той же суммой, чья оплата подходит и сюда
	Note         string // пояснение («потеряли ноль», «поправь дату», «внесли поздно»…)
}

func (v reconVerdict) entered() bool {
	return v.Status == stEntered || v.Status == stCombined || v.Status == stSplit || v.Status == stLate
}

func (v reconVerdict) needsCheck() bool {
	return v.Status == stSuspicious || v.Status == stDateCheck
}

// unitMode — в каких единицах программа хранит суммы.
type unitMode int

const (
	unitsAuto unitMode = iota
	unitsRubles
	unitsKopecks
)

// --- даты и деньги ---

func dayNum(t time.Time) int {
	y, m, d := t.In(reconLoc).Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// dayDelta — сколько календарных дней оплата позже позиции (отрицательно — раньше).
func dayDelta(pay, item time.Time) int { return dayNum(pay) - dayNum(item) }

// effDate — дата оплаты для сверки: дата оплаты, а если её нет — дата внесения.
func effDate(p cmf.Payment) time.Time {
	if !p.PaidAt.IsZero() {
		return p.PaidAt
	}
	return p.CreatedAt
}

func itemKop(a float64) int64 { return int64(math.Round(a * 100)) }

func payKop(p cmf.Payment, kopecks bool) int64 {
	if kopecks {
		return p.Amount
	}
	return p.Amount * 100
}

// moneyClose — суммы совпадают с точностью до рубля на каждую из k складываемых
// позиций (копейки при вводе часто отбрасывают).
func moneyClose(a, b int64, k int) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < int64(100*k)
}

func sameMoney(a, b int64) bool { return moneyClose(a, b, 1) }

// --- цены неправдоподобия ---

const (
	costUnexplainedItem = 150 // чек/наличка без оплаты — «НЕ внесён»
	costUnusedPayment   = 120 // оплата в программе без чека — тоже требует объяснения
	costUnusedUndated   = 60
	costCombo           = 150 // «одной оплатой» / «частями» — реже, чем 1:1
	costUndatedExact    = 70  // та же сумма, но у оплаты нет даты
	costDateFix         = 25  // совпало только по дате ВНЕСЕНИЯ — дату оплаты надо поправить
)

// normalCost — цена «та же сумма в обычный срок» (−3…+20 дней).
func normalCost(d int) (int, bool) {
	switch {
	case d >= 0 && d <= 7:
		return d, true
	case d >= 8 && d <= 20:
		return 7 + 2*(d-7), true
	case d >= -3 && d < 0:
		return 40 + 5*(-d), true // до чека — только если «после» ничего нет
	}
	return 0, false
}

// lateCost — цена «внесли поздно» (+21…+45 дней).
func lateCost(d int) (int, bool) {
	if d > 20 && d <= 45 {
		return 60 + (d - 20), true
	}
	return 0, false
}

// exactCost — цена совпадения ТОЙ ЖЕ суммы при сдвиге d дней.
func exactCost(d int) (int, reconStatus, bool) {
	if c, ok := normalCost(d); ok {
		return c, stEntered, true
	}
	if c, ok := lateCost(d); ok {
		return c, stLate, true
	}
	if d < -3 && d >= -10 {
		return 95 + 3*(-d), stDateCheck, true
	}
	return 0, 0, false
}

// memberCost — цена одной части «одной оплатой/частями»: обычный срок или поздно.
func memberCost(d int) (cost int, late, ok bool) {
	if c, ok := normalCost(d); ok {
		return c, false, true
	}
	if c, ok := lateCost(d); ok {
		return c, true, true
	}
	return 0, false, false
}

type edgeInfo struct {
	cost   int
	status reconStatus
	note   string
}

func fmtDay(t time.Time) string { return t.In(reconLoc).Format("02.01") }

// pairEdge — можно ли объяснить позицию одной оплатой и во что это обойдётся.
func pairEdge(it reconItem, p cmf.Payment, kopecks bool) (edgeInfo, bool) {
	want, got := itemKop(it.Amount), payKop(p, kopecks)
	exact := sameMoney(want, got)
	paid, created := p.PaidAt, p.CreatedAt
	if paid.IsZero() {
		paid, created = created, time.Time{}
	}
	if paid.IsZero() {
		if exact {
			return edgeInfo{costUndatedExact, stEntered, ""}, true
		}
		return edgeInfo{}, false
	}
	best, found := edgeInfo{}, false
	try := func(e edgeInfo) {
		if !found || e.cost < best.cost {
			best, found = e, true
		}
	}
	if exact {
		if c, st, ok := exactCost(dayDelta(paid, it.Date)); ok {
			try(edgeInfo{c, st, ""})
		}
		// Дата ОПЛАТЫ в программе явно не та, а по дате ВНЕСЕНИЯ всё сходится —
		// это он, дату надо поправить.
		if !created.IsZero() && dayNum(created) != dayNum(paid) {
			if c, st, ok := exactCost(dayDelta(created, it.Date)); ok {
				note := "в программе дата оплаты " + fmtDay(paid) + ", а внесли " + fmtDay(created) + " — поправь дату"
				if st == stEntered {
					st = stLate
				}
				try(edgeInfo{c + costDateFix, st, note})
			}
		}
	} else if d := dayDelta(paid, it.Date); d >= -3 && d <= 20 {
		if note := amountSlip(want, got); note != "" {
			ad := d
			if ad < 0 {
				ad = -ad
			}
			try(edgeInfo{100 + ad, stSuspicious, note})
		}
	}
	return best, found
}

// reconUnitTally — сколько ОПЛАТ находят позицию той же суммы в обычный срок при
// трактовке «рубли» и «копейки» (считаем оплаты, а не позиции: пара мелких
// позиций не должна переворачивать единицу). Суммируется по всем клиентам сверки.
func reconUnitTally(items []reconItem, pays []cmf.Payment) (rub, kop int) {
	for _, p := range pays {
		if p.Amount <= 0 {
			continue
		}
		hit := func(kopecks bool) bool {
			got := payKop(p, kopecks)
			for _, it := range items {
				if !sameMoney(itemKop(it.Amount), got) {
					continue
				}
				d := effDate(p)
				if d.IsZero() {
					return true
				}
				if _, ok := normalCost(dayDelta(d, it.Date)); ok {
					return true
				}
			}
			return false
		}
		if hit(false) {
			rub++
		}
		if hit(true) {
			kop++
		}
	}
	return rub, kop
}

// --- сторно ---

// revPlan — вариант трактовки сторно: какие положительные записи отменены и какие
// уменьшены частичной корректировкой.
type revPlan struct {
	cancel map[int]bool
	adjust map[int]int64
}

func contractsCompatible(a, b cmf.Payment) bool {
	if a.ContractID != "" && b.ContractID != "" {
		return a.ContractID == b.ContractID
	}
	if a.ContractNumber != 0 && b.ContractNumber != 0 {
		return a.ContractNumber == b.ContractNumber
	}
	return true
}

// planReversals перечисляет правдоподобные трактовки отрицательных записей.
// Сторно отменяет запись той же суммы того же договора, сделанную НЕ ПОЗЖЕ него;
// какую именно (дубль? запись с ошибочной датой?) — выберет общая оценка, поэтому
// возвращаем все варианты (не больше 32). Отрицательная запись без пары той же
// суммы — частичная корректировка: уменьшает ближайшую предыдущую запись договора.
func planReversals(pays []cmf.Payment, porder []int) []revPlan {
	var negs []int
	for _, j := range porder {
		if pays[j].Amount < 0 {
			negs = append(negs, j)
		}
	}
	base := revPlan{cancel: map[int]bool{}, adjust: map[int]int64{}}
	if len(negs) == 0 {
		return []revPlan{base}
	}
	priorOK := func(p, r cmf.Payment) bool {
		pd, rd := effDate(p), effDate(r)
		if pd.IsZero() || rd.IsZero() {
			return true
		}
		gap := dayNum(rd) - dayNum(pd)
		return gap >= 0 && gap <= 365
	}
	cands := make([][]int, len(negs))
	for k, r := range negs {
		for _, j := range porder {
			p := pays[j]
			if p.Amount > 0 && p.Amount == -pays[r].Amount && contractsCompatible(p, pays[r]) && priorOK(p, pays[r]) {
				cands[k] = append(cands[k], j)
			}
		}
		if len(cands[k]) > 6 {
			cands[k] = cands[k][len(cands[k])-6:] // самые поздние из предшествующих
		}
	}
	plans := []revPlan{base}
	for k, r := range negs {
		if len(cands[k]) == 0 {
			// Частичная корректировка: уменьшаем ближайшую предыдущую запись договора.
			best := -1
			for _, j := range porder {
				p := pays[j]
				if p.Amount > -pays[r].Amount && contractsCompatible(p, pays[r]) && priorOK(p, pays[r]) {
					best = j // porder по возрастанию даты — последняя подходящая и есть ближайшая
				}
			}
			for _, pl := range plans {
				if best >= 0 && !pl.cancel[best] {
					pl.adjust[best] += pays[r].Amount
				}
			}
			continue
		}
		var next []revPlan
		for _, pl := range plans {
			for _, j := range cands[k] {
				if pl.cancel[j] {
					continue
				}
				np := revPlan{cancel: map[int]bool{}, adjust: map[int]int64{}}
				for x := range pl.cancel {
					np.cancel[x] = true
				}
				for x, v := range pl.adjust {
					np.adjust[x] = v
				}
				np.cancel[j] = true
				next = append(next, np)
				if len(next) >= 32 {
					break
				}
			}
			if len(next) >= 32 {
				break
			}
		}
		if len(next) > 0 {
			plans = next
		}
	}
	return plans
}

// --- решатель ---

// hyper — объяснение «одной оплатой несколько позиций» или «одна позиция частями».
type hyper struct {
	items  []int // канонические индексы позиций
	pays   []int // канонические индексы оплат
	cost   int
	status reconStatus
	note   string
}

type caseSolver struct {
	ci     []reconItem
	cp     []cmf.Payment
	kop    bool
	edge   [][]edgeInfo
	has    [][]bool
	unused []int
}

func newCaseSolver(ci []reconItem, cp []cmf.Payment, kop bool) *caseSolver {
	s := &caseSolver{ci: ci, cp: cp, kop: kop}
	n, m := len(ci), len(cp)
	s.edge = make([][]edgeInfo, n)
	s.has = make([][]bool, n)
	for i := 0; i < n; i++ {
		s.edge[i] = make([]edgeInfo, m)
		s.has[i] = make([]bool, m)
		for j := 0; j < m; j++ {
			s.edge[i][j], s.has[i][j] = pairEdge(ci[i], cp[j], kop)
		}
	}
	s.unused = make([]int, m)
	for j, p := range cp {
		if effDate(p).IsZero() {
			s.unused[j] = costUnusedUndated
		} else {
			s.unused[j] = costUnusedPayment
		}
	}
	return s
}

// solve — лучшее 1:1 для свободных позиций/оплат. Задача распадается на
// независимые группы (позиции и оплаты, связанные возможными парами), каждая —
// венгерским алгоритмом. Возвращает цену и назначение позиция→оплата (или −1).
func (s *caseSolver) solve(freeI, freeP []bool) (int, []int) {
	n, m := len(s.ci), len(s.cp)
	parent := make([]int, n+m)
	for x := range parent {
		parent[x] = x
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i := 0; i < n; i++ {
		if !freeI[i] {
			continue
		}
		for j := 0; j < m; j++ {
			if freeP[j] && s.has[i][j] {
				a, b := find(i), find(n+j)
				if a != b {
					if a < b {
						parent[b] = a
					} else {
						parent[a] = b
					}
				}
			}
		}
	}
	type comp struct{ rows, cols []int }
	comps := map[int]*comp{}
	var roots []int
	get := func(r int) *comp {
		c := comps[r]
		if c == nil {
			c = &comp{}
			comps[r] = c
			roots = append(roots, r)
		}
		return c
	}
	for i := 0; i < n; i++ {
		if freeI[i] {
			c := get(find(i))
			c.rows = append(c.rows, i)
		}
	}
	for j := 0; j < m; j++ {
		if freeP[j] {
			c := get(find(n + j))
			c.cols = append(c.cols, j)
		}
	}
	sort.Ints(roots)
	assign := make([]int, n)
	for i := range assign {
		assign[i] = -1
	}
	total := 0
	const big = 1 << 30
	for _, r := range roots {
		c := comps[r]
		for _, j := range c.cols {
			total += s.unused[j]
		}
		if len(c.rows) == 0 {
			continue
		}
		if len(c.cols) == 0 {
			total += costUnexplainedItem * len(c.rows)
			continue
		}
		w := len(c.cols) + len(c.rows)
		a := make([][]int, len(c.rows))
		for ri, i := range c.rows {
			a[ri] = make([]int, w)
			for cj, j := range c.cols {
				if s.has[i][j] {
					a[ri][cj] = s.edge[i][j].cost - s.unused[j]
				} else {
					a[ri][cj] = big
				}
			}
			for cj := len(c.cols); cj < w; cj++ {
				a[ri][cj] = costUnexplainedItem
			}
		}
		res := hungarian(a)
		for ri, cj := range res {
			total += a[ri][cj]
			if cj < len(c.cols) {
				assign[c.rows[ri]] = c.cols[cj]
			}
		}
	}
	return total, assign
}

// payDateAlts — даты, по которым можно сопоставить оплату: дата оплаты и (если
// отличается) дата внесения — с отметкой, что дату оплаты придётся поправить.
type payDateAlt struct {
	t       time.Time
	viaFix  bool
	paidDay time.Time
}

func payDateAlts(p cmf.Payment) []payDateAlt {
	paid, created := p.PaidAt, p.CreatedAt
	if paid.IsZero() {
		paid, created = created, time.Time{}
	}
	if paid.IsZero() {
		return nil
	}
	alts := []payDateAlt{{t: paid}}
	if !created.IsZero() && dayNum(created) != dayNum(paid) {
		alts = append(alts, payDateAlt{t: created, viaFix: true, paidDay: paid})
	}
	return alts
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

const (
	hyperCandidates = 10 // сколько САМЫХ правдоподобных кандидатов на якорь перебирать
	hypersPerAnchor = 2  // сколько лучших комбинаций на одну оплату/позицию оставлять
)

// buildHypers — кандидаты «несколько позиций одной оплатой» (2–4 позиции в
// пределах двух недель друг от друга) и «одна позиция частями» (2–3 оплаты в
// пределах 20 дней). Для каждой оплаты/позиции берём ближайших по датам
// кандидатов и оставляем лучшие варианты. Цена не даёт бонуса за размер:
// k-комбинация дороже на (k−2) «необъяснённых», поэтому выигрывает только
// плотностью дат, а не тем, что «объясняет больше».
func (s *caseSolver) buildHypers() []hyper {
	var hs []hyper
	type mc struct {
		x    int
		cost int
		late bool
	}
	// Одной оплатой.
	for j, p := range s.cp {
		total := payKop(p, s.kop)
		var local []hyper
		for _, alt := range payDateAlts(p) {
			var cands []mc
			for i, it := range s.ci {
				if itemKop(it.Amount) >= total {
					continue
				}
				if c, late, ok := memberCost(dayDelta(alt.t, it.Date)); ok {
					cands = append(cands, mc{i, c, late})
				}
			}
			sort.SliceStable(cands, func(a, b int) bool {
				if cands[a].cost != cands[b].cost {
					return cands[a].cost < cands[b].cost
				}
				return cands[a].x < cands[b].x
			})
			if len(cands) > hyperCandidates {
				cands = cands[:hyperCandidates]
			}
			idx := make([]int, len(cands))
			for k := range idx {
				idx[k] = k
			}
			forSubsets(idx, 2, 4, func(sub []int) {
				k := len(sub)
				var sum int64
				cost, late := costCombo+(k-2)*costUnexplainedItem, false
				first, last := s.ci[cands[sub[0]].x].Date, s.ci[cands[sub[0]].x].Date
				items := make([]int, 0, k)
				for _, q := range sub {
					c := cands[q]
					it := s.ci[c.x]
					sum += itemKop(it.Amount)
					cost += c.cost
					late = late || c.late
					if it.Date.Before(first) {
						first = it.Date
					}
					if it.Date.After(last) {
						last = it.Date
					}
					items = append(items, c.x)
				}
				if !moneyClose(sum, total, k) || dayNum(last)-dayNum(first) > 14 {
					return
				}
				note := ""
				if alt.viaFix {
					cost += costDateFix
					note = "в программе дата оплаты " + fmtDay(alt.paidDay) + ", а внесли " + fmtDay(alt.t) + " — поправь дату"
				}
				if late {
					note = joinNote(note, "внесли поздно")
				}
				sort.Ints(items)
				local = append(local, hyper{items: items, pays: []int{j}, cost: cost, status: stCombined, note: note})
			})
		}
		hs = append(hs, bestHypers(local)...)
	}
	// Частями.
	for i, it := range s.ci {
		want := itemKop(it.Amount)
		type pc struct {
			j    int
			t    time.Time
			cost int
			late bool
			fix  bool
		}
		var cands []pc
		for j, p := range s.cp {
			if payKop(p, s.kop) >= want {
				continue
			}
			var best *pc
			for _, alt := range payDateAlts(p) {
				if c, late, ok := memberCost(dayDelta(alt.t, it.Date)); ok {
					if alt.viaFix {
						c += costDateFix
					}
					if best == nil || c < best.cost {
						best = &pc{j, alt.t, c, late, alt.viaFix}
					}
				}
			}
			if best != nil {
				cands = append(cands, *best)
			}
		}
		sort.SliceStable(cands, func(a, b int) bool {
			if cands[a].cost != cands[b].cost {
				return cands[a].cost < cands[b].cost
			}
			return cands[a].j < cands[b].j
		})
		if len(cands) > hyperCandidates {
			cands = cands[:hyperCandidates]
		}
		idx := make([]int, len(cands))
		for k := range idx {
			idx[k] = k
		}
		var local []hyper
		forSubsets(idx, 2, 3, func(sub []int) {
			k := len(sub)
			var sum int64
			cost, late, fix := costCombo+(k-2)*costUnusedPayment, false, false
			first, last := cands[sub[0]].t, cands[sub[0]].t
			pays := make([]int, 0, k)
			for _, q := range sub {
				c := cands[q]
				sum += payKop(s.cp[c.j], s.kop)
				cost += c.cost
				late = late || c.late
				fix = fix || c.fix
				if c.t.Before(first) {
					first = c.t
				}
				if c.t.After(last) {
					last = c.t
				}
				pays = append(pays, c.j)
			}
			if !moneyClose(sum, want, k) || dayNum(last)-dayNum(first) > 20 {
				return
			}
			note := ""
			if fix {
				note = "у части оплат в программе неверная дата — поправь"
			}
			if late {
				note = joinNote(note, "внесли поздно")
			}
			sort.Ints(pays)
			local = append(local, hyper{items: []int{i}, pays: pays, cost: cost, status: stSplit, note: note})
		})
		hs = append(hs, bestHypers(local)...)
	}
	// Без повторов, в детерминированном порядке.
	sort.SliceStable(hs, func(a, b int) bool { return hyperLess(hs[a], hs[b]) })
	var out []hyper
	for _, h := range hs {
		if len(out) > 0 && sameHyper(out[len(out)-1], h) {
			continue
		}
		out = append(out, h)
	}
	return out
}

func bestHypers(local []hyper) []hyper {
	sort.SliceStable(local, func(a, b int) bool { return hyperLess(local[a], local[b]) })
	if len(local) > hypersPerAnchor {
		local = local[:hypersPerAnchor]
	}
	return local
}

func hyperLess(a, b hyper) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if c := compareInts(a.items, b.items); c != 0 {
		return c < 0
	}
	return compareInts(a.pays, b.pays) < 0
}

func sameHyper(a, b hyper) bool {
	return a.status == b.status && compareInts(a.items, b.items) == 0 && compareInts(a.pays, b.pays) == 0
}

func compareInts(a, b []int) int {
	for k := 0; k < len(a) && k < len(b); k++ {
		if a[k] != b[k] {
			if a[k] < b[k] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// run — итеративно добавляет «одной оплатой/частями», пока это снижает общую
// цену (каждый раз — точный пересчёт 1:1). Возвращает цену, выбранные
// комбинации и назначение 1:1.
func (s *caseSolver) run() (int, []hyper, []int) {
	n, m := len(s.ci), len(s.cp)
	freeI := make([]bool, n)
	freeP := make([]bool, m)
	for i := range freeI {
		freeI[i] = true
	}
	for j := range freeP {
		freeP[j] = true
	}
	hypers := s.buildHypers()
	var chosen []hyper
	hcost := 0
	curTotal, curAssign := s.solve(freeI, freeP)

	conflicts := func(h hyper) bool {
		for _, i := range h.items {
			if !freeI[i] {
				return true
			}
		}
		for _, j := range h.pays {
			if !freeP[j] {
				return true
			}
		}
		return false
	}
	setFree := func(h hyper, v bool) {
		for _, i := range h.items {
			freeI[i] = v
		}
		for _, j := range h.pays {
			freeP[j] = v
		}
	}
	const maxEvals = 600
	evals := 0
	for round := 0; round < 60 && evals < maxEvals; round++ {
		payUsed := make([]bool, m)
		for _, j := range curAssign {
			if j >= 0 {
				payUsed[j] = true
			}
		}
		// Есть смысл проверять комбинацию, только если она трогает «слабину»:
		// необъяснённую позицию или неиспользованную оплату.
		slack := func(h hyper) bool {
			for _, i := range h.items {
				if curAssign[i] < 0 {
					return true
				}
			}
			for _, j := range h.pays {
				if !payUsed[j] {
					return true
				}
			}
			return false
		}
		type gain struct {
			k     int
			total int
		}
		var gains []gain
		for k, h := range hypers {
			if evals >= maxEvals {
				break
			}
			if conflicts(h) || !slack(h) {
				continue
			}
			evals++
			setFree(h, false)
			t, _ := s.solve(freeI, freeP)
			setFree(h, true)
			t += hcost + h.cost
			if t < curTotal {
				gains = append(gains, gain{k, t})
			}
		}
		if len(gains) == 0 {
			break
		}
		sort.SliceStable(gains, func(a, b int) bool {
			if gains[a].total != gains[b].total {
				return gains[a].total < gains[b].total
			}
			return gains[a].k < gains[b].k
		})
		// Сначала пробуем принять пачку непересекающихся улучшений разом.
		var batch []int
		batchCost := 0
		for _, g := range gains {
			if conflicts(hypers[g.k]) {
				continue
			}
			setFree(hypers[g.k], false)
			batch = append(batch, g.k)
			batchCost += hypers[g.k].cost
		}
		t, a := s.solve(freeI, freeP)
		t += hcost + batchCost
		if t <= gains[0].total {
			for _, k := range batch {
				chosen = append(chosen, hypers[k])
			}
			hcost += batchCost
			curTotal, curAssign = t, a
			continue
		}
		for _, k := range batch {
			setFree(hypers[k], true)
		}
		h := hypers[gains[0].k]
		setFree(h, false)
		chosen = append(chosen, h)
		hcost += h.cost
		curTotal, curAssign = s.solve(freeI, freeP)
		curTotal += hcost
	}
	return curTotal, chosen, curAssign
}

// humanMatch объясняет позиции клиента его оплатами в программе.
// Возвращает вердикт по каждой позиции (по исходному индексу), неиспользованные
// оплаты (исходные индексы, без отменённых сторно) и единицу сумм программы.
func humanMatch(items []reconItem, pays []cmf.Payment, mode unitMode) (verdicts []reconVerdict, leftover []int, kopecks bool) {
	verdicts, leftover, kopecks, _ = humanMatchNet(items, pays, mode)
	return
}

// humanMatchNet — то же, плюс суммы оплат после частичных корректировок
// (исходный индекс → сумма в единицах программы), чтобы показать их честно.
func humanMatchNet(items []reconItem, pays []cmf.Payment, mode unitMode) (verdicts []reconVerdict, leftover []int, kopecks bool, net map[int]int64) {
	switch mode {
	case unitsKopecks:
		kopecks = true
	case unitsAuto:
		rub, kop := reconUnitTally(items, pays)
		kopecks = kop > rub
	}

	// Канонический порядок — чтобы ответ не зависел от порядка входа.
	iord := make([]int, len(items))
	for i := range iord {
		iord[i] = i
	}
	sort.SliceStable(iord, func(a, b int) bool {
		x, y := items[iord[a]], items[iord[b]]
		if !x.Date.Equal(y.Date) {
			return x.Date.Before(y.Date)
		}
		if x.Amount != y.Amount {
			return x.Amount < y.Amount
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		if x.Report != y.Report {
			return x.Report
		}
		return x.ID < y.ID
	})
	porder := make([]int, len(pays))
	for j := range porder {
		porder[j] = j
	}
	sort.SliceStable(porder, func(a, b int) bool {
		x, y := pays[porder[a]], pays[porder[b]]
		if dx, dy := effDate(x), effDate(y); !dx.Equal(dy) {
			return dx.Before(dy)
		}
		if x.Amount != y.Amount {
			return x.Amount < y.Amount
		}
		if x.ContractID != y.ContractID {
			return x.ContractID < y.ContractID
		}
		if x.ContractNumber != y.ContractNumber {
			return x.ContractNumber < y.ContractNumber
		}
		return !x.CreatedAt.After(y.CreatedAt)
	})
	ci := make([]reconItem, len(iord))
	for k, i := range iord {
		ci[k] = items[i]
	}

	type outcome struct {
		total  int
		chosen []hyper
		assign []int
		pord   []int
		cp     []cmf.Payment
		adjust map[int]int64
	}
	var best *outcome
	for _, plan := range planReversals(pays, porder) {
		var pord []int
		var cp []cmf.Payment
		for _, j := range porder {
			p := pays[j]
			if p.Amount <= 0 || plan.cancel[j] {
				continue
			}
			if adj, ok := plan.adjust[j]; ok {
				p.Amount += adj
				if p.Amount <= 0 {
					continue
				}
			}
			pord = append(pord, j)
			cp = append(cp, p)
		}
		s := newCaseSolver(ci, cp, kopecks)
		total, chosen, assign := s.run()
		if best == nil || total < best.total {
			best = &outcome{total, chosen, assign, pord, cp, plan.adjust}
		}
	}

	n := len(ci)
	cv := make([]reconVerdict, n)
	for i := range cv {
		cv[i].SameAmountAs = -1
	}
	payUsed := make([]bool, len(best.cp))
	for _, h := range best.chosen {
		for _, j := range h.pays {
			payUsed[j] = true
		}
		for _, i := range h.items {
			v := reconVerdict{Status: h.status, Note: h.note, SameAmountAs: -1}
			for _, j := range h.pays {
				v.Pays = append(v.Pays, best.pord[j])
			}
			if h.status == stCombined {
				for _, o := range h.items {
					if o != i {
						v.With = append(v.With, iord[o])
					}
				}
			}
			cv[i] = v
		}
	}
	s := newCaseSolver(ci, best.cp, kopecks) // для пояснений по рёбрам
	for i, j := range best.assign {
		if j < 0 {
			continue
		}
		payUsed[j] = true
		e := s.edge[i][j]
		cv[i] = reconVerdict{Status: e.status, Pays: []int{best.pord[j]}, Note: e.note, SameAmountAs: -1}
	}

	// Равные суммы: «НЕ внесён», но рядом внесённая позиция ТОЙ ЖЕ суммы, чья
	// оплата подошла бы и сюда — честно: внесена одна из двух.
	for i := range ci {
		if cv[i].Status != stNotEntered {
			continue
		}
	outer:
		for k := range ci {
			if k == i || !cv[k].entered() || !sameMoney(itemKop(ci[k].Amount), itemKop(ci[i].Amount)) {
				continue
			}
			for _, oj := range cv[k].Pays {
				d := effDate(pays[oj])
				if d.IsZero() {
					continue
				}
				if _, ok := normalCost(dayDelta(d, ci[i].Date)); ok {
					cv[i].SameAmountAs = iord[k]
					break outer
				}
			}
		}
	}

	verdicts = make([]reconVerdict, len(items))
	for k, i := range iord {
		verdicts[i] = cv[k]
	}
	for j := range best.cp {
		if !payUsed[j] {
			leftover = append(leftover, best.pord[j])
		}
	}
	sort.Ints(leftover)
	net = map[int]int64{}
	for j, adj := range best.adjust {
		net[j] = pays[j].Amount + adj
	}
	return verdicts, leftover, kopecks, net
}

// forSubsets вызывает f для каждого подмножества cand размера minK..maxK.
func forSubsets(cand []int, minK, maxK int, f func([]int)) {
	if len(cand) > 16 {
		cand = cand[:16] // предохранитель от перебора
	}
	var rec func(start int, chosen []int)
	rec = func(start int, chosen []int) {
		if len(chosen) >= minK {
			f(chosen)
		}
		if len(chosen) == maxK {
			return
		}
		for x := start; x < len(cand); x++ {
			rec(x+1, append(chosen, cand[x]))
		}
	}
	rec(0, nil)
}

// hungarian — назначение минимальной стоимости для прямоугольной матрицы n×m
// (n ≤ m): каждой строке — свой столбец. Возвращает столбец для каждой строки.
func hungarian(a [][]int) []int {
	n := len(a)
	if n == 0 {
		return nil
	}
	m := len(a[0])
	const inf = math.MaxInt64 / 4
	u := make([]int, n+1)
	v := make([]int, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]int, m+1)
		for j := range minv {
			minv[j] = inf
		}
		used := make([]bool, m+1)
		for {
			used[j0] = true
			i0, delta, j1 := p[j0], inf, 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j], way[j] = cur, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	ans := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			ans[p[j]-1] = j - 1
		}
	}
	return ans
}

// amountSlip — похожа ли сумма оплаты на ОШИБКУ ввода суммы позиции (обе — в
// копейках). Пояснение или "" (не похоже — значит, просто другая оплата).
// Сравниваем целые рубли: копейки при вводе часто отбрасывают.
func amountSlip(want, got int64) string {
	if want <= 0 || got <= 0 || sameMoney(want, got) {
		return ""
	}
	wr, gr := want/100, got/100
	switch {
	case wr > 0 && gr == wr*10:
		return "похоже, лишний ноль"
	case gr > 0 && wr == gr*10:
		return "похоже, потеряли ноль"
	case want%100 != 0 && gr == want:
		return "похоже, потеряли запятую (копейки записали рублями)"
	}
	ws, gs := strconv.FormatInt(wr, 10), strconv.FormatInt(gr, 10)
	if zeroInsDel(ws, gs) {
		if len(gs) < len(ws) {
			return "похоже, потеряли ноль"
		}
		return "похоже, лишний ноль"
	}
	if adjacentSwap(ws, gs) {
		return "похоже, переставили цифры"
	}
	rel := math.Abs(float64(got-want)) / float64(want)
	if rel > 0.25 {
		return "" // разница слишком большая — это другая оплата, а не опечатка
	}
	if len(ws) == len(gs) && len(ws) >= 3 {
		diff := 0
		for k := range ws {
			if ws[k] != gs[k] {
				diff++
			}
		}
		if diff == 1 {
			return "похоже, ошиблись в одной цифре"
		}
	}
	if rel <= 0.05 {
		if got < want {
			return "внесли чуть меньше"
		}
		return "внесли чуть больше"
	}
	return ""
}

// zeroInsDel — строки отличаются ровно одним вставленным/потерянным нулём
// («10500» ↔ «1500», «2500» ↔ «20500»).
func zeroInsDel(a, b string) bool {
	if len(a) == len(b) {
		return false
	}
	long, short := a, b
	if len(b) > len(a) {
		long, short = b, a
	}
	if len(long) != len(short)+1 {
		return false
	}
	for k := 0; k < len(long); k++ {
		if long[k] == '0' && long[:k]+long[k+1:] == short {
			return true
		}
	}
	return false
}

// adjacentSwap — строки отличаются перестановкой двух соседних разных цифр.
func adjacentSwap(a, b string) bool {
	if len(a) != len(b) || len(a) < 2 {
		return false
	}
	first := -1
	diff := 0
	for k := range a {
		if a[k] != b[k] {
			if first < 0 {
				first = k
			}
			diff++
		}
	}
	return diff == 2 && first+1 < len(a) && a[first] == b[first+1] && a[first+1] == b[first]
}
