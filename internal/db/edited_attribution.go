package db

import (
	"math"
	"strings"
)

func sameMoneyCents(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && !math.IsInf(a, 0) && !math.IsInf(b, 0) && math.Round(a*100) == math.Round(b*100)
}

func validMoneyValue(amount float64, allowZero bool) bool {
	return !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0 && amount < 1e10 && (allowZero || amount > 0) && math.Abs(amount*100-math.Round(amount*100)) < 0.001
}

// An edit can replace or reorder every payment in a list. Position and equal
// list length are not evidence that a later client/collector clarification
// still describes the same payment. Only transfer metadata on a unique match
// in both directions, preserving cash/transfer identity.
func matchEditedTxStates(old, current []TxState) map[int]TxState {
	matched := make(map[int]TxState)
	used := make(map[int]bool)
	for pass := 0; pass < 3; pass++ {
		candidate := func(o, n TxState) bool {
			if o.Cash != n.Cash {
				return false
			}
			sameClient := o.ContactID > 0 && o.ContactID == n.ContactID || contactKey(o.RawName) != "" && contactKey(o.RawName) == contactKey(n.RawName)
			switch pass {
			case 0:
				return sameClient && sameMoneyCents(o.Amount, n.Amount)
			case 1:
				return sameClient
			default:
				// A reply may have expanded "Ахмед" to his full name. Retain
				// that explicit clarification on an amount-only edit, but do
				// not overwrite a different full name typed in the edit.
				if o.SourceRawID == 0 {
					return false
				}
				short, full := strings.Fields(contactKey(n.RawName)), strings.Fields(contactKey(o.RawName))
				if len(short) == 0 || len(short) >= len(full) {
					return false
				}
				words := make(map[string]bool, len(full))
				for _, w := range full {
					words[w] = true
				}
				for _, w := range short {
					if !words[w] {
						return false
					}
				}
				return true
			}
		}
		type pair struct{ old, current int }
		var pairs []pair
		for ni, n := range current {
			if _, ok := matched[n.ID]; ok {
				continue
			}
			oi, count := -1, 0
			for i, o := range old {
				if !used[i] && candidate(o, n) {
					oi, count = i, count+1
				}
			}
			if count != 1 {
				continue
			}
			reverse := 0
			for _, other := range current {
				if _, ok := matched[other.ID]; !ok && candidate(old[oi], other) {
					reverse++
				}
			}
			if reverse == 1 {
				pairs = append(pairs, pair{oi, ni})
			}
		}
		for _, p := range pairs {
			matched[current[p.current].ID] = old[p.old]
			used[p.old] = true
		}
	}
	return matched
}
