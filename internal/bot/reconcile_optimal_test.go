package bot

import (
	"math/rand"
	"testing"

	"whatsapp-bot/internal/cmf"
)

// Перекрёстная проверка: поиск находит тот же минимум, что и полный перебор
// всех наборов непересекающихся комбинаций.
func TestReconSearchOptimal(t *testing.T) {
	searchVsBrute(t, 42, []float64{1000, 2000, 3000, 4000, 5000, 6000, 8000, 10000}, 2, 5)
	searchVsBrute(t, 7, []float64{2000, 2000, 4000, 6000}, 3, 6) // ряды одинаковых сумм
}

func searchVsBrute(t *testing.T, seed int64, amts []float64, minN, spread int) {
	rng := rand.New(rand.NewSource(seed))
	worse, total := 0, 0
	for iter := 0; iter < 800; iter++ {
		ni, np := minN+rng.Intn(spread), minN+rng.Intn(spread)
		var ci []reconItem
		for k := 0; k < ni; k++ {
			ci = append(ci, chk(amts[rng.Intn(len(amts))], aug(1+rng.Intn(20))))
		}
		var cp []solverPay
		for k := 0; k < np; k++ {
			a := int64(amts[rng.Intn(len(amts))])
			if rng.Intn(3) == 0 {
				a = int64(amts[rng.Intn(len(amts))] + amts[rng.Intn(len(amts))])
			}
			cp = append(cp, solverPay{Payment: cmf.Payment{Amount: a, PaidAt: dOnly(8, 1+rng.Intn(25))}, orig: k})
		}
		s := newCaseSolver(ci, cp, nil, false)
		got := 0
		for _, c := range s.components() {
			got += s.search(c).total
		}
		// полный перебор
		s2 := newCaseSolver(ci, cp, nil, false)
		all := &compSet{}
		for i := range ci {
			all.items = append(all.items, i)
		}
		for j := range cp {
			all.pays = append(all.pays, j)
		}
		for h := range s2.hy {
			all.hypers = append(all.hypers, h)
		}
		best := s2.evaluate(all, nil).total
		usedI := map[int]bool{}
		usedP := map[int]bool{}
		var rec func(k int, chosen []int)
		rec = func(k int, chosen []int) {
			if k == len(s2.hy) {
				if st := s2.evaluate(all, append([]int(nil), chosen...)); st.total < best {
					best = st.total
				}
				return
			}
			rec(k+1, chosen)
			h := s2.hy[k]
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
			rec(k+1, append(chosen, k))
			for _, i := range h.items {
				usedI[i] = false
			}
			for _, j := range h.pays {
				usedP[j] = false
			}
		}
		if len(s2.hy) > 22 {
			continue
		}
		rec(0, nil)
		total++
		if got > best {
			worse++
			if worse <= 3 {
				t.Errorf("iter %d: поиск %d, перебор %d", iter, got, best)
			}
		}
	}
	t.Logf("проверено %d, хуже оптимума %d", total, worse)
}
