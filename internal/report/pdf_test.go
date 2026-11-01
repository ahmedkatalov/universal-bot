package report

import "testing"

func TestFormatMoneyPreservesKopecks(t *testing.T) {
	for _, c := range []struct {
		amount float64
		want   string
	}{
		{0, "0"}, {0.01, "0,01"}, {1000.10, "1 000,10"},
		{50000.25, "50 000,25"}, {9999999999.99, "9 999 999 999,99"}, {-1000.05, "-1 000,05"},
	} {
		if got := formatMoney(c.amount); got != c.want {
			t.Errorf("formatMoney(%v)=%q, want %q", c.amount, got, c.want)
		}
	}
}
