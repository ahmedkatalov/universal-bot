// «Человеческая» сверка оплат клиента с программой рассрочек.
//
// Как рассуждает бухгалтер: у клиента есть чеки/наличка (по нашему учёту) и
// оплаты в программе (по всем его рассрочкам). Надо найти САМОЕ ПРАВДОПОДОБНОЕ
// объяснение всей картины сразу, а не хватать по одному «ближайшее»:
//
//   - та же сумма, внесли в день чека или после — естественно;
//   - внесли на пару дней РАНЬШЕ чека — бывает (опечатка в дате), но реже;
//   - внесли сильно позже (3–6 недель) — «внесён поздно»;
//   - та же сумма, но дата на 4–10 дней раньше чека — «проверь дату»;
//   - похоже на ошибку ввода суммы (потеряли/лишний ноль, цифра) — «проверь»;
//   - несколько чеков одной оплатой / один чек частями — реже, но бывает;
//   - чек без оплаты — «НЕ внесён»; оплата без чека — тоже странно.
//
// Каждому варианту — «цена неправдоподобия»; выбираем набор объяснений с
// минимальной суммарной ценой (1:1 — точно, венгерским алгоритмом; редкие
// «одной оплатой/частями» — перебором). Даты сравниваем по КАЛЕНДАРНЫМ дням по
// Москве: в программе дата без времени, а у чека есть часы. Результат не зависит
// от порядка входных данных.
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
	Note         string // пояснение («потеряли ноль», «в программе без даты»…)
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

func itemKop(a float64) int64 { return int64(math.Round(a * 100)) }

func payKop(p cmf.Payment, kopecks bool) int64 {
	if kopecks {
		return p.Amount
	}
	return p.Amount * 100
}

// sameMoney — суммы совпадают с точностью до рубля (копейки часто отбрасывают).
func sameMoney(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 100
}

// --- цены неправдоподобия ---

const (
	costUnexplainedItem = 150 // чек/наличка без оплаты — «НЕ внесён»
	costUnusedPayment   = 120 // оплата в программе без чека — тоже требует объяснения
	costUnusedUndated   = 60
	costCombo           = 150 // «одной оплатой» / «частями» — реже, чем 1:1
	costUndatedExact    = 70  // та же сумма, но у оплаты нет даты
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

// exactCost — цена совпадения ТОЙ ЖЕ суммы при сдвиге d дней.
func exactCost(d int) (int, reconStatus, bool) {
	if c, ok := normalCost(d); ok {
		return c, stEntered, true
	}
	switch {
	case d > 20 && d <= 45:
		return 60 + (d - 20), stLate, true
	case d < -3 && d >= -10:
		return 95 + 3*(-d), stDateCheck, true
	}
	return 0, 0, false
}

type edgeInfo struct {
	cost   int
	status reconStatus
	note   string
}

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
			return edgeInfo{costUndatedExact, stEntered, "в программе без даты"}, true
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
		// Дата ОПЛАТЫ в программе явно не та, но ВНЕСЛИ вовремя — это он, дату поправить.
		if !created.IsZero() && dayNum(created) != dayNum(paid) {
			if c, ok := normalCost(dayDelta(created, it.Date)); ok {
				try(edgeInfo{c + 25, stLate, "в программе дата оплаты " + p.PaidAt.In(reconLoc).Format("02.01") + ", а внесли " + created.In(reconLoc).Format("02.01") + " — поправь дату"})
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

// reconUnitTally — сколько позиций находят оплату той же суммы в обычный срок при
// трактовке «рубли» и «копейки». Суммируется по ВСЕМ клиентам сверки: единица у
// программы одна, и одна случайная оплата одного клиента не должна её переворачивать.
func reconUnitTally(items []reconItem, pays []cmf.Payment) (rub, kop int) {
	for _, it := range items {
		w := itemKop(it.Amount)
		hit := func(kopecks bool) bool {
			for _, p := range pays {
				if p.Amount <= 0 || !sameMoney(w, payKop(p, kopecks)) {
					continue
				}
				if p.PaidAt.IsZero() {
					return true
				}
				if _, ok := normalCost(dayDelta(p.PaidAt, it.Date)); ok {
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

// dropReversals убирает сторно: отрицательную оплату вместе с положительной той же
// суммы (того же договора, ближайшей по дате). Отменённая запись — не оплата.
func dropReversals(pays []cmf.Payment) []bool {
	valid := make([]bool, len(pays))
	for j, p := range pays {
		valid[j] = p.Amount > 0
	}
	for _, p := range pays {
		if p.Amount >= 0 {
			continue
		}
		best, bestDist := -1, math.MaxInt32
		for k, q := range pays {
			if !valid[k] || q.Amount != -p.Amount || (p.ContractID != "" && q.ContractID != "" && p.ContractID != q.ContractID) {
				continue
			}
			dist := 0
			if !p.PaidAt.IsZero() && !q.PaidAt.IsZero() {
				dist = dayDelta(p.PaidAt, q.PaidAt)
				if dist < 0 {
					dist = -dist
				}
			}
			if dist <= 60 && dist < bestDist {
				best, bestDist = k, dist
			}
		}
		if best >= 0 {
			valid[best] = false
		}
	}
	return valid
}

// hyper — объяснение «одной оплатой несколько позиций» или «одна позиция частями».
type hyper struct {
	items  []int // канонические индексы позиций
	pays   []int // канонические индексы оплат
	cost   int
	status reconStatus
}

const maxHypers = 12

// humanMatch объясняет позиции клиента его оплатами в программе.
// Возвращает вердикт по каждой позиции (по исходному индексу), неиспользованные
// оплаты (исходные индексы) и единицу сумм программы.
func humanMatch(items []reconItem, pays []cmf.Payment, mode unitMode) (verdicts []reconVerdict, leftover []int, kopecks bool) {
	valid := dropReversals(pays)
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
		return x.ID < y.ID
	})
	var pord []int
	for j := range pays {
		if valid[j] {
			pord = append(pord, j)
		}
	}
	sort.SliceStable(pord, func(a, b int) bool {
		x, y := pays[pord[a]], pays[pord[b]]
		if !x.PaidAt.Equal(y.PaidAt) {
			return x.PaidAt.Before(y.PaidAt)
		}
		if x.Amount != y.Amount {
			return x.Amount < y.Amount
		}
		return x.ContractID < y.ContractID
	})
	ci := make([]reconItem, len(iord))
	for k, i := range iord {
		ci[k] = items[i]
	}
	cp := make([]cmf.Payment, len(pord))
	for k, j := range pord {
		cp[k] = pays[j]
	}
	n, m := len(ci), len(cp)

	// Рёбра 1:1.
	edge := make([][]edgeInfo, n)
	has := make([][]bool, n)
	for i := 0; i < n; i++ {
		edge[i] = make([]edgeInfo, m)
		has[i] = make([]bool, m)
		for j := 0; j < m; j++ {
			edge[i][j], has[i][j] = pairEdge(ci[i], cp[j], kopecks)
		}
	}

	// Кандидаты «одной оплатой» и «частями».
	hypers := buildHypers(ci, cp, kopecks)

	unusedCost := func(j int) int {
		if cp[j].PaidAt.IsZero() && cp[j].CreatedAt.IsZero() {
			return costUnusedUndated
		}
		return costUnusedPayment
	}

	type solution struct {
		total  int
		chosen []int
		assign []int // позиция -> оплата (или -1)
	}
	best := solution{total: math.MaxInt32}
	usedI := make([]bool, n)
	usedP := make([]bool, m)

	solveLeaf := func(chosen []int, hcost int) {
		var rows, cols []int
		for i := 0; i < n; i++ {
			if !usedI[i] {
				rows = append(rows, i)
			}
		}
		for j := 0; j < m; j++ {
			if !usedP[j] {
				cols = append(cols, j)
			}
		}
		assign := make([]int, n)
		for i := range assign {
			assign[i] = -1
		}
		total := hcost
		if len(rows) > 0 {
			const big = 1 << 30
			w := len(cols) + len(rows)
			a := make([][]int, len(rows))
			for r, i := range rows {
				a[r] = make([]int, w)
				for c, j := range cols {
					if has[i][j] {
						a[r][c] = edge[i][j].cost - unusedCost(j)
					} else {
						a[r][c] = big
					}
				}
				for c := len(cols); c < w; c++ {
					a[r][c] = costUnexplainedItem
				}
			}
			res := hungarian(a)
			for r, c := range res {
				total += a[r][c]
				if c < len(cols) {
					assign[rows[r]] = cols[c]
				}
			}
		}
		for _, j := range cols {
			total += unusedCost(j)
		}
		if total < best.total {
			best = solution{total, append([]int(nil), chosen...), assign}
		}
	}

	var dfs func(k int, chosen []int, hcost int)
	dfs = func(k int, chosen []int, hcost int) {
		if hcost >= best.total {
			return
		}
		if k == len(hypers) {
			solveLeaf(chosen, hcost)
			return
		}
		dfs(k+1, chosen, hcost)
		h := hypers[k]
		for _, i := range h.items {
			if usedI[i] {
				return
			}
		}
		for _, j := range h.pays {
			if usedP[j] {
				return
			}
		}
		for _, i := range h.items {
			usedI[i] = true
		}
		for _, j := range h.pays {
			usedP[j] = true
		}
		dfs(k+1, append(chosen, k), hcost+h.cost)
		for _, i := range h.items {
			usedI[i] = false
		}
		for _, j := range h.pays {
			usedP[j] = false
		}
	}
	dfs(0, nil, 0)

	// Переводим решение в вердикты (канонические индексы -> исходные).
	cv := make([]reconVerdict, n)
	for i := range cv {
		cv[i].SameAmountAs = -1
	}
	payUsed := make([]bool, m)
	for _, k := range best.chosen {
		h := hypers[k]
		for _, j := range h.pays {
			payUsed[j] = true
		}
		for _, i := range h.items {
			v := reconVerdict{Status: h.status, SameAmountAs: -1}
			for _, j := range h.pays {
				v.Pays = append(v.Pays, pord[j])
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
	for i, j := range best.assign {
		if j < 0 {
			continue
		}
		payUsed[j] = true
		e := edge[i][j]
		cv[i] = reconVerdict{Status: e.status, Pays: []int{pord[j]}, Note: e.note, SameAmountAs: -1}
	}

	// Равные суммы: «НЕ внесён», но рядом внесённая позиция ТОЙ ЖЕ суммы, чья
	// оплата подошла бы и сюда — честно: внесена одна из двух.
	for i := range ci {
		if cv[i].Status != stNotEntered {
			continue
		}
		for k := range ci {
			if k == i || !cv[k].entered() || !sameMoney(itemKop(ci[k].Amount), itemKop(ci[i].Amount)) {
				continue
			}
			for _, oj := range cv[k].Pays {
				p := pays[oj]
				if p.PaidAt.IsZero() {
					continue
				}
				if _, ok := normalCost(dayDelta(p.PaidAt, ci[i].Date)); ok {
					cv[i].SameAmountAs = iord[k]
					break
				}
			}
			if cv[i].SameAmountAs >= 0 {
				break
			}
		}
	}

	verdicts = make([]reconVerdict, len(items))
	for k, i := range iord {
		verdicts[i] = cv[k]
	}
	for j := 0; j < m; j++ {
		if !payUsed[j] {
			leftover = append(leftover, pord[j])
		}
	}
	sort.Ints(leftover)
	return verdicts, leftover, kopecks
}

// buildHypers — кандидаты «несколько позиций одной оплатой» (2–3 позиции в пределах
// двух недель, оплата в обычный срок к каждой) и «одна позиция частями» (2–3 оплаты).
// Оставляем самые правдоподобные maxHypers штук.
func buildHypers(ci []reconItem, cp []cmf.Payment, kopecks bool) []hyper {
	var hs []hyper
	for j, p := range cp {
		if p.PaidAt.IsZero() {
			continue
		}
		total := payKop(p, kopecks)
		var cand []int
		for i, it := range ci {
			if _, ok := normalCost(dayDelta(p.PaidAt, it.Date)); ok && itemKop(it.Amount) < total {
				cand = append(cand, i)
			}
		}
		forSubsets(cand, 2, 3, func(sub []int) {
			var sum int64
			cost := costCombo
			first, last := ci[sub[0]].Date, ci[sub[0]].Date
			for _, i := range sub {
				sum += itemKop(ci[i].Amount)
				c, _ := normalCost(dayDelta(p.PaidAt, ci[i].Date))
				cost += c
				if ci[i].Date.Before(first) {
					first = ci[i].Date
				}
				if ci[i].Date.After(last) {
					last = ci[i].Date
				}
			}
			if sameMoney(sum, total) && dayNum(last)-dayNum(first) <= 14 {
				hs = append(hs, hyper{items: append([]int(nil), sub...), pays: []int{j}, cost: cost, status: stCombined})
			}
		})
	}
	for i, it := range ci {
		want := itemKop(it.Amount)
		var cand []int
		for j, p := range cp {
			if p.PaidAt.IsZero() {
				continue
			}
			if _, ok := normalCost(dayDelta(p.PaidAt, it.Date)); ok && payKop(p, kopecks) < want {
				cand = append(cand, j)
			}
		}
		forSubsets(cand, 2, 3, func(sub []int) {
			var sum int64
			cost := costCombo
			for _, j := range sub {
				sum += payKop(cp[j], kopecks)
				c, _ := normalCost(dayDelta(cp[j].PaidAt, it.Date))
				cost += c
			}
			if sameMoney(sum, want) {
				hs = append(hs, hyper{items: []int{i}, pays: append([]int(nil), sub...), cost: cost, status: stSplit})
			}
		})
	}
	sort.SliceStable(hs, func(a, b int) bool { return hs[a].cost < hs[b].cost })
	if len(hs) > maxHypers {
		hs = hs[:maxHypers]
	}
	return hs
}

// forSubsets вызывает f для каждого подмножества cand размера minK..maxK.
func forSubsets(cand []int, minK, maxK int, f func([]int)) {
	if len(cand) > 14 {
		cand = cand[:14] // предохранитель от перебора; у клиента столько не бывает
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
func amountSlip(want, got int64) string {
	if want <= 0 || got <= 0 || sameMoney(want, got) {
		return ""
	}
	switch {
	case sameMoney(got*10, want):
		return "похоже, потеряли ноль"
	case sameMoney(got, want*10):
		return "похоже, лишний ноль"
	}
	rel := math.Abs(float64(got-want)) / float64(want)
	if rel > 0.25 {
		return "" // разница слишком большая — это другая оплата, а не опечатка
	}
	ws, gs := strconv.FormatInt(want/100, 10), strconv.FormatInt(got/100, 10)
	if len(ws) == len(gs) && len(ws) >= 3 {
		diff, first := 0, -1
		for k := range ws {
			if ws[k] != gs[k] {
				if first < 0 {
					first = k
				}
				diff++
			}
		}
		if diff == 1 {
			return "похоже, ошиблись в одной цифре"
		}
		if diff == 2 && first+1 < len(ws) && ws[first] == gs[first+1] && ws[first+1] == gs[first] {
			return "похоже, переставили цифры"
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
