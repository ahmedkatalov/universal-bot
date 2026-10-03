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
//   - несколько чеков одной оплатой / один чек частями — реже; такое
//     объяснение выигрывает ПЛОТНОСТЬЮ по датам, а не числом позиций;
//   - чек без оплаты — «НЕ внесён»; оплата без чека — тоже требует объяснения;
//   - сторно отменяет запись той же суммы, сделанную ДО него (какую именно —
//     решает общая оценка), списание на сумму нескольких записей подряд
//     отменяет их, частичная корректировка уменьшает одну из записей.
//
// Каждому варианту — «цена неправдоподобия»; минимизируем суммарную цену.
// 1:1 и выбор отменяемых сторно записей — точно (венгерский алгоритм с
// двойственными оценками), «одной оплатой/частями» — точным перебором для
// небольших групп и локальным поиском (добавить/заменить/убрать/пара) с
// отсечением по двойственной оценке для больших. Независимые группы позиций
// и оплат решаются отдельно. Даты — календарные дни по Москве. Ответ не
// зависит от порядка входных данных.
package bot

import (
	"math"
	"sort"
	"strconv"
	"strings"
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
	Pays         []int     // индексы оплат (в исходном срезе), закрывших позицию
	With         []int     // для stCombined — другие позиции той же оплаты
	SameAmountAs int       // для «НЕ внесён»: позиция с той же суммой, чья оплата подходит и сюда
	Note         string    // пояснение («потеряли ноль», «поправь дату», «внесли поздно»…)
	At           time.Time // дата, по которой сошлось (дата оплаты или дата внесения)
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

func fmtDay(t time.Time) string { return t.In(reconLoc).Format("02.01") }

func dayOrDash(t time.Time) string {
	if t.IsZero() {
		return "без даты"
	}
	return fmtDay(t)
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

// --- цены неправдоподобия ---

const (
	costUnexplainedItem = 150 // чек/наличка без оплаты — «НЕ внесён»
	costUnusedPayment   = 120 // оплата в программе без чека — тоже требует объяснения
	costUnusedUndated   = 60
	costCombo           = 150 // «одной оплатой» / «частями» — реже, чем 1:1
	costUndatedExact    = 70  // та же сумма, но у оплаты нет даты
	costDateFix         = 25  // совпало только по дате ВНЕСЕНИЯ — дату оплаты надо поправить
	costStrayNegative   = 110 // списание, которое не удалось отнести ни к одной записи
	costRevUnmatched    = 400 // сторно, которому не досталось записи той же суммы
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

// gapCost — небольшая надбавка за «давность» отменяемой записи: при прочих
// равных сторно отменяет ближайшую предыдущую запись.
func gapCost(g int) int {
	if g < 0 {
		g = 0
	}
	if g > 60 {
		g = 60
	}
	return g / 6
}

// --- оплаты для решателя ---

// solverPay — положительная запись программы после учёта сторно/корректировок.
type solverPay struct {
	cmf.Payment
	orig  int    // индекс в исходном срезе
	gross int64  // сумма ДО частичной корректировки (0 — корректировки не было)
	corr  string // пояснение корректировки
}

type edgeInfo struct {
	cost   int
	status reconStatus
	note   string
	at     time.Time
}

// pairEdge — можно ли объяснить позицию одной оплатой и во что это обойдётся.
func pairEdge(it reconItem, sp solverPay, kopecks bool) (edgeInfo, bool) {
	e, ok := pairEdgeAmount(it, sp.Payment, payKop(sp.Payment, kopecks))
	if sp.gross > 0 {
		g := sp.Payment
		g.Amount = sp.gross
		if sameMoney(itemKop(it.Amount), payKop(g, kopecks)) {
			// Сумма совпала с записью ДО корректировки — внесли, но потом уменьшили.
			if ge, gok := pairEdgeAmount(it, g, payKop(g, kopecks)); gok {
				cand := edgeInfo{100 + ge.cost, stSuspicious, joinNote(sp.corr, ge.note), ge.at}
				if !ok || cand.cost < e.cost {
					e, ok = cand, true
				}
			}
		}
	}
	return e, ok
}

func pairEdgeAmount(it reconItem, p cmf.Payment, got int64) (edgeInfo, bool) {
	want := itemKop(it.Amount)
	exact := sameMoney(want, got)
	paid, created := p.PaidAt, p.CreatedAt
	if paid.IsZero() {
		paid, created = created, time.Time{}
	}
	if paid.IsZero() {
		if exact && !p.DateUnknown {
			return edgeInfo{costUndatedExact, stEntered, "", time.Time{}}, true
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
			try(edgeInfo{c, st, "", paid})
		}
		// Дата ОПЛАТЫ в программе явно не та, а по дате ВНЕСЕНИЯ сходится.
		if !created.IsZero() && dayNum(created) != dayNum(paid) {
			dc := dayDelta(created, it.Date)
			switch {
			case dc >= 0:
				if c, st, ok := exactCost(dc); ok {
					if st == stEntered {
						st = stLate
					}
					try(edgeInfo{c + costDateFix, st, "в программе дата оплаты " + fmtDay(paid) + ", а внесли " + fmtDay(created) + " — поправь дату", created})
				}
			case dc >= -10:
				// Запись сделали ДО чека — это не его запись наверняка, только «проверь».
				try(edgeInfo{95 + 3*(-dc) + costDateFix, stDateCheck, "внесли в программу " + fmtDay(created) + ", до чека; дата оплаты там " + fmtDay(paid), created})
			}
		}
	} else if d := dayDelta(paid, it.Date); d >= -3 && d <= 20 {
		if note := amountSlip(want, got); note != "" {
			ad := d
			if ad < 0 {
				ad = -ad
			}
			try(edgeInfo{100 + ad, stSuspicious, note, paid})
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

// --- сторно и корректировки ---

func contractsCompatible(a, b cmf.Payment) bool {
	if a.ContractID != "" && b.ContractID != "" {
		return a.ContractID == b.ContractID
	}
	if a.ContractNumber != 0 && b.ContractNumber != 0 {
		return a.ContractNumber == b.ContractNumber
	}
	return true
}

// madeBefore — сделана ли запись p не позже отрицательной записи r, и сколько
// дней между ними. Если у обеих есть время внесения — сравниваем его (дата
// оплаты бывает с опечаткой), иначе — даты оплаты.
func madeBefore(p, r cmf.Payment) (int, bool) {
	if !p.CreatedAt.IsZero() && !r.CreatedAt.IsZero() {
		if p.CreatedAt.After(r.CreatedAt) {
			return 0, false
		}
		g := dayNum(r.CreatedAt) - dayNum(p.CreatedAt)
		return g, g <= 365
	}
	pd, rd := effDate(p), effDate(r)
	if pd.IsZero() || rd.IsZero() {
		return 30, true
	}
	g := dayNum(rd) - dayNum(pd)
	return g, g >= 0 && g <= 365
}

// corrPlan — одна трактовка отрицательных записей.
type corrPlan struct {
	cancel map[int]bool   // записи, отменённые списанием на их общую сумму
	adjust map[int]int64  // частичные корректировки: запись → изменение суммы
	notes  map[int]string // пояснения к скорректированным записям
	revs   []int          // сторно (решатель сам выберет, какую запись отменить)
	stray  []int          // списания, не отнесённые ни к чему
	prior  int            // цена выбора корректировок
	key    string
}

func (pl corrPlan) clone() corrPlan {
	np := corrPlan{cancel: map[int]bool{}, adjust: map[int]int64{}, notes: map[int]string{}, prior: pl.prior}
	for k, v := range pl.cancel {
		np.cancel[k] = v
	}
	for k, v := range pl.adjust {
		np.adjust[k] = v
	}
	for k, v := range pl.notes {
		np.notes[k] = v
	}
	np.revs = append([]int(nil), pl.revs...)
	np.stray = append([]int(nil), pl.stray...)
	return np
}

func (pl corrPlan) makeKey() string {
	var ks []int
	for k := range pl.adjust {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(strconv.Itoa(k) + ":" + strconv.FormatInt(pl.adjust[k], 10) + ",")
	}
	b.WriteString("|" + setKey(pl.stray))
	return b.String()
}

// planCorrections разбирает отрицательные записи так, как это сделал бы
// бухгалтер:
//  1. есть запись той же суммы того же договора, сделанная раньше, — это сторно;
//     какую именно запись оно отменяет, решит общая оценка;
//  2. иначе есть 2–3 записи того же договора за 0–3 дня до списания на его
//     сумму — их отменили одним списанием;
//  3. иначе это частичная корректировка одной из ближайших предыдущих записей
//     (варианты — до трёх самых свежих) или списание, которое не к чему отнести.
func planCorrections(pays []cmf.Payment, porder []int, kopecks bool) []corrPlan {
	base := corrPlan{cancel: map[int]bool{}, adjust: map[int]int64{}, notes: map[int]string{}}
	var partial []int
	for _, r := range porder {
		neg := pays[r]
		if neg.Amount >= 0 {
			continue
		}
		amt := -neg.Amount
		full := false
		for _, j := range porder {
			p := pays[j]
			if p.Amount == amt && contractsCompatible(p, neg) {
				if _, ok := madeBefore(p, neg); ok {
					full = true
					break
				}
			}
		}
		if full {
			base.revs = append(base.revs, r)
			continue
		}
		if set := cancelSet(pays, porder, r, base.cancel); set != nil {
			for _, j := range set {
				base.cancel[j] = true
			}
			continue
		}
		partial = append(partial, r)
	}
	plans := []corrPlan{base}
	for _, r := range partial {
		neg := pays[r]
		amt := -neg.Amount
		var next []corrPlan
		for _, pl := range plans {
			type tgt struct{ j, g int }
			var ts []tgt
			for k := len(porder) - 1; k >= 0; k-- {
				j := porder[k]
				p := pays[j]
				if p.Amount <= 0 || pl.cancel[j] || !contractsCompatible(p, neg) || p.Amount+pl.adjust[j] <= amt {
					continue
				}
				if g, ok := madeBefore(p, neg); ok && g <= 60 {
					ts = append(ts, tgt{j, g})
				}
			}
			sort.SliceStable(ts, func(a, b int) bool { return ts[a].g < ts[b].g })
			if len(ts) > 3 {
				ts = ts[:3]
			}
			for _, t := range ts {
				np := pl.clone()
				np.adjust[t.j] -= amt
				np.notes[t.j] = joinNote(np.notes[t.j], "её уменьшили корректировкой −"+
					formatMoney(float64(payKop(cmf.Payment{Amount: amt}, kopecks))/100)+" ₽ от "+dayOrDash(effDate(neg)))
				np.prior += gapCost(t.g)
				next = append(next, np)
			}
			np := pl.clone()
			np.stray = append(np.stray, r)
			np.prior += costStrayNegative
			next = append(next, np)
		}
		// Луч: не больше 12 различных вариантов, самые правдоподобные.
		for k := range next {
			next[k].key = next[k].makeKey()
		}
		sort.SliceStable(next, func(a, b int) bool {
			if next[a].prior != next[b].prior {
				return next[a].prior < next[b].prior
			}
			return next[a].key < next[b].key
		})
		plans = nil
		seen := map[string]bool{}
		for _, pl := range next {
			if seen[pl.key] {
				continue
			}
			seen[pl.key] = true
			plans = append(plans, pl)
			if len(plans) >= 12 {
				break
			}
		}
	}
	return plans
}

// cancelSet — 2–3 записи того же договора за 0–3 дня до списания r, которые в
// сумме дают ровно его сумму (самые близкие по датам), или nil.
func cancelSet(pays []cmf.Payment, porder []int, r int, taken map[int]bool) []int {
	neg := pays[r]
	amt := -neg.Amount
	type c struct{ j, g int }
	var cs []c
	for _, j := range porder {
		p := pays[j]
		if p.Amount <= 0 || p.Amount >= amt || taken[j] || !contractsCompatible(p, neg) {
			continue
		}
		if g, ok := madeBefore(p, neg); ok && g <= 3 {
			cs = append(cs, c{j, g})
		}
	}
	if len(cs) > 14 {
		return nil
	}
	var best []int
	bestG := 1 << 30
	var rec func(start int, sum int64, g int, chosen []int)
	rec = func(start int, sum int64, g int, chosen []int) {
		if len(chosen) >= 2 && sum == amt {
			if g < bestG {
				bestG, best = g, append([]int(nil), chosen...)
			}
			return
		}
		if len(chosen) == 3 || sum >= amt {
			return
		}
		for x := start; x < len(cs); x++ {
			rec(x+1, sum+pays[cs[x].j].Amount, g+cs[x].g, append(chosen, cs[x].j))
		}
	}
	rec(0, 0, 0, nil)
	return best
}

// --- решатель ---

// hyper — объяснение «одной оплатой несколько позиций» или «одна позиция частями».
type hyper struct {
	items  []int // канонические индексы позиций
	pays   []int // индексы оплат решателя
	cost   int
	status reconStatus
	note   string
	at     time.Time
}

// revRow — сторно: отменяет одну из записей-кандидатов.
type revRow struct {
	orig  int
	cands []int
	cost  []int
}

// compSet — независимая группа: позиции, оплаты, сторно и комбинации, связанные
// хоть каким-то возможным объяснением.
type compSet struct {
	items, pays, revs, hypers []int
}

type subSol struct {
	cost   int
	rowPay []int // для каждой строки (позиции, затем сторно) — оплата или −1
	u, vd  []int // двойственные строк и их «личных» фиктивных столбцов
	vp     []int // двойственные столбцов-оплат
}

// evalState — решение группы при заданном наборе комбинаций.
type evalState struct {
	total, hcost, base, matrix int
	chosen                     []int
	lockedI, lockedP           []bool
	assign                     []int // позиция → оплата (1:1) или −1
	revPay                     []int // сторно → оплата или −1
	uI, vdI, uR, vP            []int
}

type caseSolver struct {
	ci  []reconItem
	cp  []solverPay
	rv  []revRow
	kop bool

	edge    [][]edgeInfo
	has     [][]bool
	itemAdj [][]int
	payAdjI [][]int
	payAdjR [][]int
	revCost []map[int]int
	unused  []int

	hy      []hyper
	hyByI   [][]int
	hyByP   [][]int
	memo    map[string]*subSol
	evals   int
	work    int
	exhaust bool
}

const (
	evalBudget  = 2500  // точных пересчётов на одного клиента
	workBudget  = 3e8   // «операций» венгерского алгоритма на одного клиента
	exactHypers = 10    // до стольких комбинаций в группе — точный перебор
	enumBudget  = 20000 // узлов перебора сумм на одну оплату/позицию
	maxSubsets  = 4096
	bigCost     = 1 << 40
)

func newCaseSolver(ci []reconItem, cp []solverPay, rv []revRow, kop bool) *caseSolver {
	s := &caseSolver{ci: ci, cp: cp, rv: rv, kop: kop, memo: map[string]*subSol{}}
	n, m := len(ci), len(cp)
	s.edge = make([][]edgeInfo, n)
	s.has = make([][]bool, n)
	s.itemAdj = make([][]int, n)
	s.payAdjI = make([][]int, m)
	s.payAdjR = make([][]int, m)
	for i := 0; i < n; i++ {
		s.edge[i] = make([]edgeInfo, m)
		s.has[i] = make([]bool, m)
		for j := 0; j < m; j++ {
			s.edge[i][j], s.has[i][j] = pairEdge(ci[i], cp[j], kop)
			if s.has[i][j] {
				s.itemAdj[i] = append(s.itemAdj[i], j)
				s.payAdjI[j] = append(s.payAdjI[j], i)
			}
		}
	}
	s.revCost = make([]map[int]int, len(rv))
	for r, row := range rv {
		s.revCost[r] = map[int]int{}
		for k, j := range row.cands {
			s.revCost[r][j] = row.cost[k]
			s.payAdjR[j] = append(s.payAdjR[j], r)
		}
	}
	s.unused = make([]int, m)
	for j, p := range cp {
		if effDate(p.Payment).IsZero() {
			s.unused[j] = costUnusedUndated
		} else {
			s.unused[j] = costUnusedPayment
		}
	}
	s.hy = s.buildHypers()
	s.hyByI = make([][]int, n)
	s.hyByP = make([][]int, m)
	for k, h := range s.hy {
		for _, i := range h.items {
			s.hyByI[i] = append(s.hyByI[i], k)
		}
		for _, j := range h.pays {
			s.hyByP[j] = append(s.hyByP[j], k)
		}
	}
	return s
}

type unionFind struct{ p []int }

func newUF(n int) *unionFind {
	u := &unionFind{p: make([]int, n)}
	for i := range u.p {
		u.p[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	for u.p[x] != x {
		u.p[x] = u.p[u.p[x]]
		x = u.p[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	a, b = u.find(a), u.find(b)
	if a == b {
		return
	}
	if a < b {
		u.p[b] = a
	} else {
		u.p[a] = b
	}
}

// components — независимые группы (по возможным парам, сторно и комбинациям).
func (s *caseSolver) components() []*compSet {
	n, m, R := len(s.ci), len(s.cp), len(s.rv)
	uf := newUF(n + m + R)
	for i := 0; i < n; i++ {
		for _, j := range s.itemAdj[i] {
			uf.union(i, n+j)
		}
	}
	for r, row := range s.rv {
		for _, j := range row.cands {
			uf.union(n+m+r, n+j)
		}
	}
	for _, h := range s.hy {
		first := -1
		for _, i := range h.items {
			if first < 0 {
				first = i
			}
			uf.union(first, i)
		}
		for _, j := range h.pays {
			if first < 0 {
				first = n + j
			}
			uf.union(first, n+j)
		}
	}
	byRoot := map[int]*compSet{}
	var out []*compSet
	get := func(x int) *compSet {
		r := uf.find(x)
		c := byRoot[r]
		if c == nil {
			c = &compSet{}
			byRoot[r] = c
			out = append(out, c)
		}
		return c
	}
	for i := 0; i < n; i++ {
		c := get(i)
		c.items = append(c.items, i)
	}
	for j := 0; j < m; j++ {
		c := get(n + j)
		c.pays = append(c.pays, j)
	}
	for r := 0; r < R; r++ {
		c := get(n + m + r)
		c.revs = append(c.revs, r)
	}
	for k, h := range s.hy {
		var c *compSet
		if len(h.items) > 0 {
			c = get(h.items[0])
		} else {
			c = get(n + h.pays[0])
		}
		c.hypers = append(c.hypers, k)
	}
	return out
}

// evaluate — точная цена группы при выбранных комбинациях: оставшиеся позиции,
// сторно и оплаты сопоставляются 1:1 оптимально (по независимым подгруппам,
// с запоминанием).
func (s *caseSolver) evaluate(c *compSet, chosen []int) evalState {
	n, m, R := len(s.ci), len(s.cp), len(s.rv)
	st := evalState{chosen: chosen}
	st.lockedI = make([]bool, n)
	st.lockedP = make([]bool, m)
	for _, h := range chosen {
		st.hcost += s.hy[h].cost
		for _, i := range s.hy[h].items {
			st.lockedI[i] = true
		}
		for _, j := range s.hy[h].pays {
			st.lockedP[j] = true
		}
	}
	st.assign = make([]int, n)
	st.uI = make([]int, n)
	st.vdI = make([]int, n)
	st.vP = make([]int, m)
	st.revPay = make([]int, R)
	st.uR = make([]int, R)
	for i := range st.assign {
		st.assign[i] = -1
	}
	for r := range st.revPay {
		st.revPay[r] = -1
	}
	for _, j := range c.pays {
		if !st.lockedP[j] {
			st.base += s.unused[j]
		}
	}
	// Подгруппы свободных элементов.
	uf := newUF(n + m + R)
	for _, i := range c.items {
		if st.lockedI[i] {
			continue
		}
		for _, j := range s.itemAdj[i] {
			if !st.lockedP[j] {
				uf.union(i, n+j)
			}
		}
	}
	for _, r := range c.revs {
		for _, j := range s.rv[r].cands {
			if !st.lockedP[j] {
				uf.union(n+m+r, n+j)
			}
		}
	}
	type sub struct{ items, revs, pays []int }
	subs := map[int]*sub{}
	var roots []int
	get := func(x int) *sub {
		r := uf.find(x)
		g := subs[r]
		if g == nil {
			g = &sub{}
			subs[r] = g
			roots = append(roots, r)
		}
		return g
	}
	for _, i := range c.items {
		if !st.lockedI[i] {
			g := get(i)
			g.items = append(g.items, i)
		}
	}
	for _, r := range c.revs {
		g := get(n + m + r)
		g.revs = append(g.revs, r)
	}
	for _, j := range c.pays {
		if !st.lockedP[j] {
			g := get(n + j)
			g.pays = append(g.pays, j)
		}
	}
	for _, root := range roots {
		g := subs[root]
		if len(g.items)+len(g.revs) == 0 {
			continue
		}
		sol := s.solveSub(g.items, g.revs, g.pays)
		st.matrix += sol.cost
		for k, i := range g.items {
			st.assign[i] = sol.rowPay[k]
			st.uI[i], st.vdI[i] = sol.u[k], sol.vd[k]
		}
		for k, r := range g.revs {
			x := len(g.items) + k
			st.revPay[r] = sol.rowPay[x]
			st.uR[r] = sol.u[x]
		}
		for k, j := range g.pays {
			st.vP[j] = sol.vp[k]
		}
	}
	st.total = st.hcost + st.base + st.matrix
	s.evals++
	return st
}

// solveSub — оптимальное 1:1 для подгруппы: строки — позиции и сторно, столбцы —
// оплаты и «личный» фиктивный столбец каждой строки («не объяснена»).
func (s *caseSolver) solveSub(items, revs, pays []int) *subSol {
	key := setKey(items) + "|" + setKey(revs) + "|" + setKey(pays)
	if sol, ok := s.memo[key]; ok {
		return sol
	}
	rows := len(items) + len(revs)
	cols := len(pays) + rows
	a := make([][]int, rows)
	for x := 0; x < rows; x++ {
		a[x] = make([]int, cols)
		for y := range a[x] {
			a[x][y] = bigCost
		}
		if x < len(items) {
			i := items[x]
			for y, j := range pays {
				if s.has[i][j] {
					a[x][y] = s.edge[i][j].cost - s.unused[j]
				}
			}
			a[x][len(pays)+x] = costUnexplainedItem
		} else {
			r := revs[x-len(items)]
			for y, j := range pays {
				if c, ok := s.revCost[r][j]; ok {
					a[x][y] = c - s.unused[j]
				}
			}
			a[x][len(pays)+x] = costRevUnmatched
		}
	}
	s.work += rows * rows * cols
	ans, u, v := hungarian(a)
	sol := &subSol{rowPay: make([]int, rows), u: u, vd: make([]int, rows), vp: make([]int, len(pays))}
	for x, y := range ans {
		sol.cost += a[x][y]
		if y < len(pays) {
			sol.rowPay[x] = pays[y]
		} else {
			sol.rowPay[x] = -1
		}
		sol.vd[x] = v[len(pays)+x]
	}
	copy(sol.vp, v[:len(pays)])
	s.memo[key] = sol
	return sol
}

// lowerBound — нижняя оценка цены группы, если вместо cur.chosen выбрать next:
// двойственные оценки текущего решения остаются допустимыми, если убрать
// занятые строки/столбцы, а освободившимся дать наибольшие допустимые оценки.
func (s *caseSolver) lowerBound(cur *evalState, next []int) int {
	lockI := map[int]bool{}
	lockP := map[int]bool{}
	hcost := 0
	for _, h := range next {
		hcost += s.hy[h].cost
		for _, i := range s.hy[h].items {
			lockI[i] = true
		}
		for _, j := range s.hy[h].pays {
			lockP[j] = true
		}
	}
	matrix, base := cur.matrix, cur.base
	var freedI, freedP []int
	for _, h := range cur.chosen {
		for _, i := range s.hy[h].items {
			if !lockI[i] {
				freedI = append(freedI, i)
			}
		}
		for _, j := range s.hy[h].pays {
			if !lockP[j] {
				freedP = append(freedP, j)
			}
		}
	}
	for i := range lockI {
		if !cur.lockedI[i] {
			matrix -= cur.uI[i] + cur.vdI[i]
		}
	}
	for j := range lockP {
		if !cur.lockedP[j] {
			matrix -= cur.vP[j]
			base -= s.unused[j]
		}
	}
	vNew := map[int]int{}
	for _, j := range freedP {
		v := 0
		for _, i := range s.payAdjI[j] {
			if !lockI[i] && !cur.lockedI[i] {
				if x := s.edge[i][j].cost - s.unused[j] - cur.uI[i]; x < v {
					v = x
				}
			}
		}
		for _, r := range s.payAdjR[j] {
			if x := s.revCost[r][j] - s.unused[j] - cur.uR[r]; x < v {
				v = x
			}
		}
		vNew[j] = v
		matrix += v
		base += s.unused[j]
	}
	for _, i := range freedI {
		u := costUnexplainedItem
		for _, j := range s.itemAdj[i] {
			if lockP[j] {
				continue
			}
			vj, ok := vNew[j]
			if !ok {
				if cur.lockedP[j] {
					continue
				}
				vj = cur.vP[j]
			}
			if x := s.edge[i][j].cost - s.unused[j] - vj; x < u {
				u = x
			}
		}
		matrix += u
	}
	return hcost + base + matrix
}

func (s *caseSolver) outOfBudget() bool {
	if s.evals >= evalBudget || s.work >= workBudget {
		s.exhaust = true
		return true
	}
	return false
}

func setKey(xs []int) string {
	var b strings.Builder
	for _, x := range xs {
		b.WriteString(strconv.Itoa(x))
		b.WriteByte(',')
	}
	return b.String()
}

type scoredSet struct {
	set []int
	lb  int
	key string
}

func (s *caseSolver) scoreSets(cur *evalState, sets [][]int) []scoredSet {
	out := make([]scoredSet, 0, len(sets))
	for _, set := range sets {
		out = append(out, scoredSet{set, s.lowerBound(cur, set), setKey(set)})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].lb != out[b].lb {
			return out[a].lb < out[b].lb
		}
		return out[a].key < out[b].key
	})
	return out
}

// search — лучшее объяснение группы.
func (s *caseSolver) search(c *compSet) evalState {
	cur := s.evaluate(c, nil)
	if len(c.hypers) == 0 {
		return cur
	}
	if len(c.hypers) <= exactHypers {
		if subsets := s.disjointSubsets(c.hypers); subsets != nil {
			best, done := cur, true
			for _, x := range s.scoreSets(&cur, subsets) {
				if x.lb >= best.total {
					break
				}
				if s.outOfBudget() {
					done = false
					break
				}
				if st := s.evaluate(c, x.set); st.total < best.total {
					best = st
				}
			}
			if done {
				return best
			}
			cur = best
		}
	}
	// Локальный поиск: на каждом шаге — лучший из ходов «добавить (вытеснив
	// пересекающиеся)», «убрать», «добавить пару», с отсечением по оценке.
	for round := 0; round < 300; round++ {
		var best *evalState
		bestTotal := cur.total
		for _, x := range s.scoreSets(&cur, s.moves(c, &cur)) {
			if x.lb >= bestTotal || s.outOfBudget() {
				break
			}
			if st := s.evaluate(c, x.set); st.total < bestTotal {
				bestTotal = st.total
				b := st
				best = &b
			}
		}
		if best == nil {
			break
		}
		cur = *best
	}
	// Точный перебор с отсечением (по каждой оплате/позиции — какая из её
	// комбинаций или никакой) — находит и длинные цепочки перестановок; лучшее
	// из локального поиска служит начальной планкой.
	return s.branchAndBound(c, cur)
}

const bbNodeBudget = 40000

// branchAndBound — перебор «якорей» (оплата для «одной оплатой», позиция для
// «частями») с нижней оценкой: каждая свободная позиция берёт самую дешёвую
// свою долю (1:1, комбинация или «не внесена»), каждая оплата — «без чека».
func (s *caseSolver) branchAndBound(c *compSet, best evalState) evalState {
	type anchor struct{ hs []int }
	byKey := map[int]*anchor{}
	var anchors []*anchor
	for _, h := range c.hypers {
		key := 0
		if s.hy[h].status == stCombined {
			key = -1 - s.hy[h].pays[0]
		} else {
			key = s.hy[h].items[0]
		}
		a := byKey[key]
		if a == nil {
			a = &anchor{}
			byKey[key] = a
			anchors = append(anchors, a)
		}
		a.hs = append(a.hs, h)
	}
	n, m := len(s.ci), len(s.cp)
	usedI := make([]bool, n)
	usedP := make([]bool, m)
	share := make([]int, n)
	relax := func(from int) int {
		sum := 0
		for _, j := range c.pays {
			if !usedP[j] {
				sum += s.unused[j]
			}
		}
		for _, r := range c.revs {
			b := costRevUnmatched
			for k, j := range s.rv[r].cands {
				if !usedP[j] {
					if x := s.rv[r].cost[k] - s.unused[j]; x < b {
						b = x
					}
				}
			}
			sum += b
		}
		for _, i := range c.items {
			if usedI[i] {
				continue
			}
			b := costUnexplainedItem
			for _, j := range s.itemAdj[i] {
				if !usedP[j] {
					if x := s.edge[i][j].cost - s.unused[j]; x < b {
						b = x
					}
				}
			}
			share[i] = b
		}
		for _, a := range anchors[from:] {
		next:
			for _, h := range a.hs {
				hh := s.hy[h]
				for _, i := range hh.items {
					if usedI[i] {
						continue next
					}
				}
				for _, j := range hh.pays {
					if usedP[j] {
						continue next
					}
				}
				x := hh.cost
				for _, j := range hh.pays {
					x -= s.unused[j]
				}
				per := floorDiv(x, len(hh.items))
				for _, i := range hh.items {
					if per < share[i] {
						share[i] = per
					}
				}
			}
		}
		for _, i := range c.items {
			if !usedI[i] {
				sum += share[i]
			}
		}
		return sum
	}
	var chosen []int
	hcost, nodes := 0, 0
	var rec func(k int)
	rec = func(k int) {
		nodes++
		if nodes > bbNodeBudget || s.outOfBudget() {
			s.exhaust = true
			return
		}
		if hcost+relax(k) >= best.total {
			return
		}
		if k == len(anchors) {
			set := append([]int(nil), chosen...)
			sort.Ints(set)
			if st := s.evaluate(c, set); st.total < best.total {
				best = st
			}
			return
		}
	opts:
		for _, h := range anchors[k].hs {
			hh := s.hy[h]
			for _, i := range hh.items {
				if usedI[i] {
					continue opts
				}
			}
			for _, j := range hh.pays {
				if usedP[j] {
					continue opts
				}
			}
			for _, i := range hh.items {
				usedI[i] = true
			}
			for _, j := range hh.pays {
				usedP[j] = true
			}
			chosen = append(chosen, h)
			hcost += hh.cost
			rec(k + 1)
			hcost -= hh.cost
			chosen = chosen[:len(chosen)-1]
			for _, i := range hh.items {
				usedI[i] = false
			}
			for _, j := range hh.pays {
				usedP[j] = false
			}
		}
		rec(k + 1)
	}
	rec(0)
	return best
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// disjointSubsets — все наборы непересекающихся комбинаций (или nil, если их много).
func (s *caseSolver) disjointSubsets(hs []int) [][]int {
	var out [][]int
	usedI := map[int]bool{}
	usedP := map[int]bool{}
	tooMany := false
	var rec func(k int, chosen []int)
	rec = func(k int, chosen []int) {
		if tooMany {
			return
		}
		if k == len(hs) {
			if len(chosen) > 0 {
				out = append(out, append([]int(nil), chosen...))
				if len(out) > maxSubsets {
					tooMany = true
				}
			}
			return
		}
		rec(k+1, chosen)
		h := s.hy[hs[k]]
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
		rec(k+1, append(chosen, hs[k]))
		for _, i := range h.items {
			usedI[i] = false
		}
		for _, j := range h.pays {
			usedP[j] = false
		}
	}
	rec(0, nil)
	if tooMany {
		return nil
	}
	return out
}

func (s *caseSolver) hyDisjoint(a, b int) bool {
	for _, x := range s.hy[a].items {
		for _, y := range s.hy[b].items {
			if x == y {
				return false
			}
		}
	}
	for _, x := range s.hy[a].pays {
		for _, y := range s.hy[b].pays {
			if x == y {
				return false
			}
		}
	}
	return true
}

// moves — соседние наборы комбинаций для локального поиска.
func (s *caseSolver) moves(c *compSet, cur *evalState) [][]int {
	inCur := map[int]bool{}
	ownerI := map[int]int{}
	ownerP := map[int]int{}
	for _, h := range cur.chosen {
		inCur[h] = true
		for _, i := range s.hy[h].items {
			ownerI[i] = h
		}
		for _, j := range s.hy[h].pays {
			ownerP[j] = h
		}
	}
	payItem := map[int]int{}
	for _, i := range c.items {
		if j := cur.assign[i]; j >= 0 && !cur.lockedI[i] {
			payItem[j] = i
		}
	}
	seen := map[string]bool{}
	var out [][]int
	add := func(next []int) {
		sort.Ints(next)
		if k := setKey(next); !seen[k] {
			seen[k] = true
			out = append(out, next)
		}
	}
	with := func(adds ...int) {
		drop := map[int]bool{}
		for _, a := range adds {
			for _, i := range s.hy[a].items {
				if o, ok := ownerI[i]; ok {
					drop[o] = true
				}
			}
			for _, j := range s.hy[a].pays {
				if o, ok := ownerP[j]; ok {
					drop[o] = true
				}
			}
		}
		var next []int
		for _, h := range cur.chosen {
			if !drop[h] {
				next = append(next, h)
			}
		}
		add(append(next, adds...))
	}
	for _, h := range c.hypers {
		if !inCur[h] {
			with(h)
		}
	}
	for _, h := range cur.chosen {
		var next []int
		for _, x := range cur.chosen {
			if x != h {
				next = append(next, x)
			}
		}
		add(next)
	}
	// Пары: вторая комбинация забирает то, что сейчас занято 1:1 «партнёром»
	// элемента первой.
	for _, h1 := range c.hypers {
		if inCur[h1] {
			continue
		}
		var lists [][]int
		for _, i := range s.hy[h1].items {
			if j := cur.assign[i]; j >= 0 && !cur.lockedI[i] {
				lists = append(lists, s.hyByP[j])
			}
		}
		for _, j := range s.hy[h1].pays {
			if i, ok := payItem[j]; ok {
				lists = append(lists, s.hyByI[i])
			}
		}
		// Элементы вытесняемых комбинаций, которые h1 не покрывает, — им нужна своя.
		inH1I, inH1P := map[int]bool{}, map[int]bool{}
		for _, i := range s.hy[h1].items {
			inH1I[i] = true
		}
		for _, j := range s.hy[h1].pays {
			inH1P[j] = true
		}
		evicted := map[int]bool{}
		for _, i := range s.hy[h1].items {
			if o, ok := ownerI[i]; ok {
				evicted[o] = true
			}
		}
		for _, j := range s.hy[h1].pays {
			if o, ok := ownerP[j]; ok {
				evicted[o] = true
			}
		}
		for _, o := range cur.chosen {
			if !evicted[o] {
				continue
			}
			for _, i := range s.hy[o].items {
				if !inH1I[i] {
					lists = append(lists, s.hyByI[i])
				}
			}
			for _, j := range s.hy[o].pays {
				if !inH1P[j] {
					lists = append(lists, s.hyByP[j])
				}
			}
		}
		cnt := 0
		for _, list := range lists {
			for _, h2 := range list {
				if cnt >= 12 {
					break
				}
				if h2 == h1 || inCur[h2] || !s.hyDisjoint(h1, h2) {
					continue
				}
				with(h1, h2)
				cnt++
			}
		}
	}
	return out
}

// payDateAlt — дата, по которой можно сопоставить оплату: дата оплаты и (если
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

const (
	hypersPerAnchor = 3  // лучших комбинаций на оплату/позицию
	hypersDiverse   = 3  // плюс непересекающиеся с ними (для рядов одинаковых сумм)
	hypersCap       = 12 // всего на оплату/позицию
)

type hcand struct {
	x    int
	amt  int64
	cost int
	late bool
	fix  bool
	t    time.Time
}

// enumSums перебирает наборы из 2..maxK кандидатов (отсортированных по сумме по
// убыванию), сумма которых совпадает с target с допуском в рубль на часть, а
// даты укладываются в span дней. Ветки, которые заведомо не дотянут или уже
// перебрали, отсекаются.
func enumSums(cs []hcand, target int64, maxK, span int, f func(sub []int)) {
	nodes := 0
	tol := int64(100 * maxK)
	var rec func(start int, sum int64, lo, hi int, chosen []int)
	rec = func(start int, sum int64, lo, hi int, chosen []int) {
		nodes++
		if nodes > enumBudget {
			return
		}
		if len(chosen) >= 2 && moneyClose(sum, target, len(chosen)) {
			f(chosen)
		}
		if len(chosen) == maxK || sum >= target+tol {
			return
		}
		left := int64(maxK - len(chosen))
		for x := start; x < len(cs); x++ {
			if sum+left*cs[x].amt < target-tol {
				return // дальше суммы только меньше
			}
			d := dayNum(cs[x].t)
			nlo, nhi := d, d
			if len(chosen) > 0 {
				nlo, nhi = lo, hi
				if d < nlo {
					nlo = d
				}
				if d > nhi {
					nhi = d
				}
			}
			if nhi-nlo > span {
				continue
			}
			rec(x+1, sum+cs[x].amt, nlo, nhi, append(chosen, x))
		}
	}
	rec(0, 0, 0, 0, nil)
}

func sortHCands(cs []hcand) {
	sort.SliceStable(cs, func(a, b int) bool {
		if cs[a].amt != cs[b].amt {
			return cs[a].amt > cs[b].amt
		}
		if cs[a].cost != cs[b].cost {
			return cs[a].cost < cs[b].cost
		}
		return cs[a].x < cs[b].x
	})
}

// buildHypers — кандидаты «несколько позиций одной оплатой» (2–4 позиции в
// пределах двух недель) и «одна позиция частями» (2–3 оплаты в пределах 20
// дней). Цена не даёт бонуса за размер: k-комбинация дороже на (k−2)
// «необъяснённых», поэтому выигрывает только плотностью дат.
func (s *caseSolver) buildHypers() []hyper {
	var hs []hyper
	// Одной оплатой.
	for j := range s.cp {
		p := s.cp[j].Payment
		total := payKop(p, s.kop)
		var local []hyper
		for _, alt := range payDateAlts(p) {
			var cs []hcand
			for i, it := range s.ci {
				a := itemKop(it.Amount)
				if a <= 0 || a >= total {
					continue
				}
				d := dayDelta(alt.t, it.Date)
				if alt.viaFix && d < 0 {
					continue
				}
				if c, late, ok := memberCost(d); ok {
					cs = append(cs, hcand{x: i, amt: a, cost: c, late: late, t: it.Date})
				}
			}
			sortHCands(cs)
			enumSums(cs, total, 4, 14, func(sub []int) {
				k := len(sub)
				cost, late := costCombo+(k-2)*costUnexplainedItem, false
				items := make([]int, 0, k)
				for _, q := range sub {
					cost += cs[q].cost
					late = late || cs[q].late
					items = append(items, cs[q].x)
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
				local = append(local, hyper{items: items, pays: []int{j}, cost: cost, status: stCombined, note: note, at: alt.t})
			})
		}
		hs = append(hs, selectHypers(local, false)...)
	}
	// Частями.
	for i, it := range s.ci {
		want := itemKop(it.Amount)
		var cs []hcand
		for j := range s.cp {
			p := s.cp[j].Payment
			a := payKop(p, s.kop)
			if a <= 0 || a >= want {
				continue
			}
			var best *hcand
			for _, alt := range payDateAlts(p) {
				d := dayDelta(alt.t, it.Date)
				if alt.viaFix && d < 0 {
					continue
				}
				if c, late, ok := memberCost(d); ok {
					if alt.viaFix {
						c += costDateFix
					}
					if best == nil || c < best.cost {
						best = &hcand{x: j, amt: a, cost: c, late: late, fix: alt.viaFix, t: alt.t}
					}
				}
			}
			if best != nil {
				cs = append(cs, *best)
			}
		}
		sortHCands(cs)
		var local []hyper
		enumSums(cs, want, 3, 20, func(sub []int) {
			k := len(sub)
			cost, late, fix := costCombo+(k-2)*costUnusedPayment, false, false
			pays := make([]int, 0, k)
			for _, q := range sub {
				cost += cs[q].cost
				late = late || cs[q].late
				fix = fix || cs[q].fix
				pays = append(pays, cs[q].x)
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
		hs = append(hs, selectHypers(local, true)...)
	}
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

// selectHypers — какие комбинации одной оплаты/позиции оставить: лучшие, плюс
// непересекающиеся с ними (ряды одинаковых сумм), плюс лучшая для каждого
// участника — чтобы у каждого был свой вариант.
func selectHypers(local []hyper, split bool) []hyper {
	sort.SliceStable(local, func(a, b int) bool { return hyperLess(local[a], local[b]) })
	var uniq []hyper
	seen := map[string]bool{}
	for _, h := range local {
		k := setKey(h.items) + "|" + setKey(h.pays)
		if seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, h)
	}
	side := func(h hyper) []int {
		if split {
			return h.pays
		}
		return h.items
	}
	kept := map[int]bool{}
	covered := map[int]bool{}
	var out []hyper
	keep := func(k int) {
		kept[k] = true
		out = append(out, uniq[k])
		for _, e := range side(uniq[k]) {
			covered[e] = true
		}
	}
	for k := range uniq {
		if len(out) >= hypersPerAnchor {
			break
		}
		keep(k)
	}
	div := 0
	for k, h := range uniq {
		if div >= hypersDiverse || len(out) >= hypersCap {
			break
		}
		if kept[k] {
			continue
		}
		free := true
		for _, e := range side(h) {
			if covered[e] {
				free = false
				break
			}
		}
		if free {
			keep(k)
			div++
		}
	}
	for k, h := range uniq {
		if len(out) >= hypersCap {
			break
		}
		if kept[k] {
			continue
		}
		for _, e := range side(h) {
			if !covered[e] {
				keep(k)
				break
			}
		}
	}
	return out
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

// matchResult — итог сверки клиента.
type matchResult struct {
	Verdicts []reconVerdict
	Leftover []int          // оплаты без пары (исходные индексы)
	Kopecks  bool           // программа хранит суммы в копейках
	Net      map[int]int64  // суммы записей после частичных корректировок
	Notes    map[int]string // пояснения к скорректированным записям
	Stray    []int          // списания/сторно, которые не к чему отнести
	Partial  bool           // поиск упёрся в лимит — ответ может быть не лучшим
}

// humanMatch объясняет позиции клиента его оплатами в программе.
// Возвращает вердикт по каждой позиции (по исходному индексу), неиспользованные
// оплаты (исходные индексы, без отменённых сторно) и единицу сумм программы.
func humanMatch(items []reconItem, pays []cmf.Payment, mode unitMode) (verdicts []reconVerdict, leftover []int, kopecks bool) {
	r := humanMatchFull(items, pays, mode)
	return r.Verdicts, r.Leftover, r.Kopecks
}

// humanMatchNet — то же, плюс суммы оплат после частичных корректировок.
func humanMatchNet(items []reconItem, pays []cmf.Payment, mode unitMode) (verdicts []reconVerdict, leftover []int, kopecks bool, net map[int]int64) {
	r := humanMatchFull(items, pays, mode)
	return r.Verdicts, r.Leftover, r.Kopecks, r.Net
}

func humanMatchFull(items []reconItem, pays []cmf.Payment, mode unitMode) matchResult {
	var res matchResult
	switch mode {
	case unitsKopecks:
		res.Kopecks = true
	case unitsAuto:
		rub, kop := reconUnitTally(items, pays)
		res.Kopecks = kop > rub
	}
	kopecks := res.Kopecks

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
		s      *caseSolver
		plan   corrPlan
		states []evalState
		comps  []*compSet
	}
	var best *outcome
	for _, plan := range planCorrections(pays, porder, kopecks) {
		var cp []solverPay
		pos := map[int]int{}
		for _, j := range porder {
			p := pays[j]
			if p.Amount <= 0 || plan.cancel[j] {
				continue
			}
			sp := solverPay{Payment: p, orig: j}
			if adj, ok := plan.adjust[j]; ok {
				sp.gross = p.Amount
				sp.Amount += adj
				sp.corr = plan.notes[j]
			}
			pos[j] = len(cp)
			cp = append(cp, sp)
		}
		var rv []revRow
		for _, r := range plan.revs {
			row := revRow{orig: r}
			for _, j := range porder {
				p := pays[j]
				k, ok := pos[j]
				if !ok || cp[k].gross > 0 || p.Amount != -pays[r].Amount || !contractsCompatible(p, pays[r]) {
					continue
				}
				if g, ok := madeBefore(p, pays[r]); ok {
					row.cands = append(row.cands, k)
					row.cost = append(row.cost, gapCost(g))
				}
			}
			rv = append(rv, row)
		}
		s := newCaseSolver(ci, cp, rv, kopecks)
		comps := s.components()
		states := make([]evalState, len(comps))
		total := plan.prior
		for k, c := range comps {
			states[k] = s.search(c)
			total += states[k].total
		}
		if best == nil || total < best.total {
			best = &outcome{total, s, plan, states, comps}
		}
	}

	s := best.s
	n := len(ci)
	cv := make([]reconVerdict, n)
	for i := range cv {
		cv[i].SameAmountAs = -1
	}
	payUsed := make([]bool, len(s.cp))
	for k, c := range best.comps {
		st := best.states[k]
		for _, hk := range st.chosen {
			h := s.hy[hk]
			for _, j := range h.pays {
				payUsed[j] = true
			}
			for _, i := range h.items {
				v := reconVerdict{Status: h.status, Note: h.note, SameAmountAs: -1, At: h.at}
				for _, j := range h.pays {
					v.Pays = append(v.Pays, s.cp[j].orig)
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
		for _, i := range c.items {
			j := st.assign[i]
			if j < 0 || st.lockedI[i] {
				continue
			}
			payUsed[j] = true
			e := s.edge[i][j]
			cv[i] = reconVerdict{Status: e.status, Pays: []int{s.cp[j].orig}, Note: e.note, SameAmountAs: -1, At: e.at}
		}
		for _, r := range c.revs {
			if j := st.revPay[r]; j >= 0 {
				payUsed[j] = true
			} else {
				res.Stray = append(res.Stray, s.rv[r].orig)
			}
		}
	}
	res.Stray = append(res.Stray, best.plan.stray...)
	sort.Ints(res.Stray)
	res.Partial = s.exhaust

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

	res.Verdicts = make([]reconVerdict, len(items))
	for k, i := range iord {
		res.Verdicts[i] = cv[k]
	}
	for j := range s.cp {
		if !payUsed[j] {
			res.Leftover = append(res.Leftover, s.cp[j].orig)
		}
	}
	sort.Ints(res.Leftover)
	res.Net = map[int]int64{}
	res.Notes = map[int]string{}
	for _, sp := range s.cp {
		if sp.gross > 0 {
			res.Net[sp.orig] = sp.Amount
			res.Notes[sp.orig] = sp.corr
		}
	}
	return res
}

// hungarian — назначение минимальной стоимости для прямоугольной матрицы n×m
// (n ≤ m): каждой строке — свой столбец. Возвращает столбец для каждой строки и
// двойственные оценки строк (u) и столбцов (v): u[i]+v[j] ≤ a[i][j], на
// выбранных парах — равенство, у незанятых столбцов v = 0.
func hungarian(a [][]int) (ans, uRows, vCols []int) {
	n := len(a)
	if n == 0 {
		return nil, nil, nil
	}
	m := len(a[0])
	const inf = math.MaxInt64 / 4
	u := make([]int, n+1)
	v := make([]int, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	minv := make([]int, m+1)
	used := make([]bool, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		for j := range minv {
			minv[j] = inf
			used[j] = false
		}
		for {
			used[j0] = true
			i0, delta, j1 := p[j0], inf, 0
			row := a[i0-1]
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := row[j-1] - u[i0] - v[j]
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
	ans = make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			ans[p[j]-1] = j - 1
		}
	}
	return ans, u[1:], v[1:]
}

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
