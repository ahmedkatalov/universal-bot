package bot

import (
	"math"
	"strconv"
	"strings"
)

func moneyText(amount float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(math.Round(amount*100)/100, 'f', 2, 64), ".00")
}

// Monetary equality is equality of kopecks, never a one-ruble proximity
// heuristic. Used by attribution, reply targeting and amount corrections.
func sameMoneyCents(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && !math.IsInf(a, 0) && !math.IsInf(b, 0) && math.Round(a*100) == math.Round(b*100)
}

func validExtractedMoney(amount float64, allowZero bool) bool {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount >= 1e10 || !allowZero && amount == 0 {
		return false
	}
	// PostgreSQL stores NUMERIC(12,2). Reject invented fractions of a kopeck
	// instead of allowing Go and PostgreSQL to round them differently.
	return math.Abs(amount*100-math.Round(amount*100)) < 0.001
}
