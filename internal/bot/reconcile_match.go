// «Человеческая» сверка оплат клиента с программой рассрочек. Смотрим на ВСЮ
// картину клиента (чеки и наличку вокруг периода, все его рассрочки) и
// объясняем каждый чек так, как объяснил бы бухгалтер:
//
//  1. та же сумма в обычном окне дат (оплату вносят в день чека или позже) —
//     «внесён оплатой 14.08»; при равных суммах — ближайшая оплата ПОСЛЕ чека;
//  2. одна оплата закрывает несколько чеков («внесли одной суммой») или один чек
//     внесён частями;
//  3. та же сумма, но внесли сильно позже/раньше — «внесён поздно»;
//  4. рядом есть оплата, похожая на ошибку в сумме (лишний/потерянный ноль,
//     одна цифра, разница в пару процентов) — «проверь, похоже ошиблись»;
//  5. иначе — «НЕ внесён».
//
// Контекстные позиции (чеки/наличка соседних дней и других групп) участвуют в
// сопоставлении, но в ответ не выводятся: они «забирают» свои оплаты, и оплата
// сентябрьского чека, внесённая 2 октября, не засчитает октябрьский чек.
package bot

import (
	"math"
	"sort"
	"strconv"
	"time"

	"whatsapp-bot/internal/cmf"
)

const day = 24 * time.Hour

const (
	reconWinBack  = 3 * day  // оплата раньше чека — допуск на перекос дат
	reconWinFwd   = 20 * day // обычно вносят в течение пары недель после чека
	reconLateBack = 10 * day // «внесли с датой раньше чека» — уже странно
	reconLateFwd  = 45 * day // «внесли поздно» — но это всё ещё он
)

// reconStatus — вердикт по позиции.
type reconStatus int

const (
	stNotEntered reconStatus = iota
	stEntered                // та же сумма в обычном окне
	stCombined               // внесён вместе с другими одной оплатой
	stSplit                  // внесён частями (несколько оплат)
	stLate                   // та же сумма, но сильно позже/раньше
	stSuspicious             // похоже, внесли с ошибкой в сумме — проверить
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
	Status reconStatus
	Pays   []int // индексы оплат, закрывших позицию (или похожей оплаты для stSuspicious)
	With   []int // для stCombined — другие позиции в той же оплате
	// SameAmountAs — для «НЕ внесён»: индекс внесённой позиции с той же суммой,
	// чья оплата подходит и сюда (суммы одинаковые — внесена одна из двух).
	SameAmountAs int
	Note         string // «лишний ноль», «внесли меньше на 2%» и т.п.
}

func (v reconVerdict) entered() bool {
	return v.Status == stEntered || v.Status == stCombined || v.Status == stSplit || v.Status == stLate
}

// reconPaymentsInKopecks — в каких единицах программа хранит суммы (по всему
// набору): иначе одна оплата совпала бы с двумя позициями, отличающимися в 100 раз.
func reconPaymentsInKopecks(items []reconItem, pays []cmf.Payment) bool {
	rub, kop := 0, 0
	for _, it := range items {
		wr, wk := int64(it.Amount+0.5), int64(it.Amount*100+0.5)
		for _, p := range pays {
			if p.Amount == wr {
				rub++
				break
			}
		}
		for _, p := range pays {
			if p.Amount == wk {
				kop++
				break
			}
		}
	}
	return kop > rub
}

// prefKey — чем меньше, тем вероятнее, что оплата именно за эту позицию.
// Оплата ПОСЛЕ позиции естественна; ДО — бывает (перекос дат), но реже.
func prefKey(delta time.Duration) time.Duration {
	if delta >= 0 {
		return delta
	}
	return -3*delta + 12*time.Hour
}

// humanMatch сопоставляет позиции клиента с его оплатами в программе.
// Возвращает вердикт по каждой позиции (по индексу), неиспользованные оплаты и
// единицу сумм программы.
func humanMatch(items []reconItem, pays []cmf.Payment) (verdicts []reconVerdict, leftover []int, kopecks bool) {
	kopecks = reconPaymentsInKopecks(items, pays)
	units := func(a float64) int64 {
		if kopecks {
			return int64(math.Round(a * 100))
		}
		return int64(math.Round(a))
	}
	verdicts = make([]reconVerdict, len(items))
	for i := range verdicts {
		verdicts[i].SameAmountAs = -1
	}
	payUsed := make([]bool, len(pays))
	itemDone := func(i int) bool { return verdicts[i].Status != stNotEntered }

	// --- Этап 1: та же сумма в обычном окне, максимум совпадений (Кун). ---
	const noDate = time.Duration(1) << 62
	adj := make([][]int, len(items))
	type edge struct {
		i, j int
		key  time.Duration
	}
	var edges []edge
	for i, it := range items {
		w := units(it.Amount)
		type cand struct {
			j   int
			key time.Duration
		}
		var cs []cand
		for j, p := range pays {
			if p.Amount != w {
				continue
			}
			key := noDate
			if !p.PaidAt.IsZero() {
				delta := p.PaidAt.Sub(it.Date)
				if delta > reconWinFwd || delta < -reconWinBack {
					continue
				}
				key = prefKey(delta)
			}
			cs = append(cs, cand{j, key})
		}
		sort.SliceStable(cs, func(a, b int) bool { return cs[a].key < cs[b].key })
		for _, c := range cs {
			adj[i] = append(adj[i], c.j)
			edges = append(edges, edge{i, c.j, c.key})
		}
	}
	matchPay := make([]int, len(pays))
	for j := range matchPay {
		matchPay[j] = -1
	}
	matchItem := make([]int, len(items))
	for i := range matchItem {
		matchItem[i] = -1
	}
	sort.SliceStable(edges, func(a, b int) bool { return edges[a].key < edges[b].key })
	for _, e := range edges {
		if matchItem[e.i] == -1 && matchPay[e.j] == -1 {
			matchItem[e.i], matchPay[e.j] = e.j, e.i
		}
	}
	var augment func(i int, seen []bool) bool
	augment = func(i int, seen []bool) bool {
		for _, j := range adj[i] {
			if seen[j] {
				continue
			}
			seen[j] = true
			if matchPay[j] == -1 || augment(matchPay[j], seen) {
				matchPay[j], matchItem[i] = i, j
				return true
			}
		}
		return false
	}
	for i := range items {
		if matchItem[i] == -1 {
			augment(i, make([]bool, len(pays)))
		}
	}
	for i := range items {
		if j := matchItem[i]; j >= 0 {
			verdicts[i] = reconVerdict{Status: stEntered, Pays: []int{j}, SameAmountAs: -1}
			payUsed[j] = true
		}
	}

	inWindow := func(it reconItem, p cmf.Payment) bool {
		if p.PaidAt.IsZero() {
			return false
		}
		delta := p.PaidAt.Sub(it.Date)
		return delta <= reconWinFwd && delta >= -reconWinBack
	}

	// --- Этап 2а: одна оплата = сумма 2–3 позиций («внесли одной суммой»). ---
	payOrder := make([]int, 0, len(pays))
	for j := range pays {
		payOrder = append(payOrder, j)
	}
	sort.SliceStable(payOrder, func(a, b int) bool { return pays[payOrder[a]].PaidAt.Before(pays[payOrder[b]].PaidAt) })
	for _, j := range payOrder {
		if payUsed[j] {
			continue
		}
		p := pays[j]
		var cand []int
		for i, it := range items {
			if !itemDone(i) && inWindow(it, p) {
				cand = append(cand, i)
			}
		}
		if best := bestSubset(cand, 2, 3, p.Amount, func(i int) int64 { return units(items[i].Amount) },
			func(i int) time.Duration { return prefKey(p.PaidAt.Sub(items[i].Date)) }); best != nil {
			payUsed[j] = true
			for _, i := range best {
				var others []int
				for _, k := range best {
					if k != i {
						others = append(others, k)
					}
				}
				verdicts[i] = reconVerdict{Status: stCombined, Pays: []int{j}, With: others, SameAmountAs: -1}
			}
		}
	}

	// --- Этап 2б: одна позиция = сумма 2–3 оплат («внесли частями»). ---
	for i, it := range items {
		if itemDone(i) {
			continue
		}
		var cand []int
		for j, p := range pays {
			if !payUsed[j] && inWindow(it, p) {
				cand = append(cand, j)
			}
		}
		if best := bestSubset(cand, 2, 3, units(it.Amount), func(j int) int64 { return pays[j].Amount },
			func(j int) time.Duration { return prefKey(pays[j].PaidAt.Sub(it.Date)) }); best != nil {
			for _, j := range best {
				payUsed[j] = true
			}
			sort.Slice(best, func(a, b int) bool { return pays[best[a]].PaidAt.Before(pays[best[b]].PaidAt) })
			verdicts[i] = reconVerdict{Status: stSplit, Pays: best, SameAmountAs: -1}
		}
	}

	// --- Этап 3: та же сумма, но внесли сильно позже/раньше обычного. ---
	type lateEdge struct {
		i, j int
		key  time.Duration
	}
	var late []lateEdge
	for i, it := range items {
		if itemDone(i) {
			continue
		}
		w := units(it.Amount)
		for j, p := range pays {
			if payUsed[j] || p.Amount != w || p.PaidAt.IsZero() {
				continue
			}
			delta := p.PaidAt.Sub(it.Date)
			if (delta > reconWinFwd && delta <= reconLateFwd) || (delta < -reconWinBack && delta >= -reconLateBack) {
				late = append(late, lateEdge{i, j, prefKey(delta)})
			}
		}
	}
	sort.SliceStable(late, func(a, b int) bool { return late[a].key < late[b].key })
	for _, e := range late {
		if itemDone(e.i) || payUsed[e.j] {
			continue
		}
		payUsed[e.j] = true
		verdicts[e.i] = reconVerdict{Status: stLate, Pays: []int{e.j}, SameAmountAs: -1}
	}

	// --- Этап 4: похоже на ошибку в сумме при вводе в программу. ---
	type susEdge struct {
		i, j int
		key  time.Duration
		note string
	}
	var sus []susEdge
	for i, it := range items {
		if itemDone(i) {
			continue
		}
		w := units(it.Amount)
		for j, p := range pays {
			if payUsed[j] || !inWindow(it, p) {
				continue
			}
			if note := amountSlip(w, p.Amount); note != "" {
				sus = append(sus, susEdge{i, j, prefKey(p.PaidAt.Sub(it.Date)), note})
			}
		}
	}
	sort.SliceStable(sus, func(a, b int) bool { return sus[a].key < sus[b].key })
	for _, e := range sus {
		if itemDone(e.i) || payUsed[e.j] {
			continue
		}
		payUsed[e.j] = true
		verdicts[e.i] = reconVerdict{Status: stSuspicious, Pays: []int{e.j}, Note: e.note, SameAmountAs: -1}
	}

	// Равные суммы: «НЕ внесён», но рядом внесённая позиция с ТОЙ ЖЕ суммой, чья
	// оплата подошла бы и сюда — честно говорим, что внесена одна из двух.
	for i, it := range items {
		if verdicts[i].Status != stNotEntered {
			continue
		}
		for k, other := range items {
			if k == i || verdicts[k].Status != stEntered || units(other.Amount) != units(it.Amount) {
				continue
			}
			if p := pays[verdicts[k].Pays[0]]; !p.PaidAt.IsZero() && inWindow(it, p) {
				verdicts[i].SameAmountAs = k
				break
			}
		}
	}

	for j := range pays {
		if !payUsed[j] {
			leftover = append(leftover, j)
		}
	}
	return verdicts, leftover, kopecks
}

// bestSubset ищет среди cand подмножество размера minK..maxK с суммой target,
// предпочитая МЕНЬШИЙ размер и меньшую суммарную «дальность» по датам. nil — нет.
func bestSubset(cand []int, minK, maxK int, target int64, val func(int) int64, cost func(int) time.Duration) []int {
	if len(cand) < minK || target <= 0 {
		return nil
	}
	if len(cand) > 12 {
		cand = cand[:12] // предохранитель от перебора; у клиента столько не бывает
	}
	for k := minK; k <= maxK; k++ {
		var best []int
		bestCost := time.Duration(math.MaxInt64)
		var rec func(start int, chosen []int, sum int64, c time.Duration)
		rec = func(start int, chosen []int, sum int64, c time.Duration) {
			if len(chosen) == k {
				if sum == target && c < bestCost {
					bestCost, best = c, append([]int(nil), chosen...)
				}
				return
			}
			for x := start; x < len(cand); x++ {
				v := val(cand[x])
				if v <= 0 || sum+v > target {
					continue
				}
				rec(x+1, append(chosen, cand[x]), sum+v, c+cost(cand[x]))
			}
		}
		rec(0, nil, 0, 0)
		if best != nil {
			return best
		}
	}
	return nil
}

// amountSlip — похожа ли сумма оплаты на ОШИБКУ ввода суммы позиции. Возвращает
// пояснение или "" (не похоже — тогда это просто другая оплата).
func amountSlip(want, got int64) string {
	if want <= 0 || got <= 0 || want == got {
		return ""
	}
	switch {
	case got*10 == want:
		return "потеряли ноль"
	case got == want*10:
		return "лишний ноль"
	}
	ws, gs := strconv.FormatInt(want, 10), strconv.FormatInt(got, 10)
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
			return "одна цифра не та"
		}
		if diff == 2 && first+1 < len(ws) && ws[first] == gs[first+1] && ws[first+1] == gs[first] {
			return "переставили цифры"
		}
	}
	rel := math.Abs(float64(got-want)) / float64(want)
	if rel <= 0.05 {
		if got < want {
			return "внесли чуть меньше"
		}
		return "внесли чуть больше"
	}
	return ""
}
