package bot

import (
	"math"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/ledger"
)

func TestMoneyIdentityDoesNotTargetAnotherKopeck(t *testing.T) {
	for _, amounts := range [][2]float64{{1000.10, 1000.20}, {1000, 1000.01}, {0, 0.01}, {math.NaN(), math.NaN()}, {math.Inf(1), math.Inf(1)}} {
		if sameMoneyCents(amounts[0], amounts[1]) || nearAmount(amounts[0], amounts[1]) {
			t.Fatalf("different money treated as identical: %v", amounts)
		}
	}
	if !nearAmount(0.1+0.2, 0.30) {
		t.Fatal("binary arithmetic noise broke identical kopecks")
	}
}

func TestMoneyExtractionRejectsFractionsOfKopecks(t *testing.T) {
	for _, amount := range []string{"1.005", "1000.101", "10000000000"} {
		if _, _, valid := decodePaymentExtraction(`{"payments":[{"name":"Клиент","amount":` + amount + `}]}`); valid {
			t.Fatalf("unrepresentable payment accepted: %s", amount)
		}
		if _, valid := decodeReceiptExtraction(`{"kind":"receipt","amount":` + amount + `}`); valid {
			t.Fatalf("unrepresentable receipt accepted: %s", amount)
		}
	}
	for _, amount := range []float64{0.01, 1000.10, 9999999999.99} {
		if !validExtractedMoney(amount, false) {
			t.Fatalf("valid amount rejected: %.2f", amount)
		}
	}
}

func TestMoneySuspicionDoesNotRoundDistinctPaymentsToRubles(t *testing.T) {
	ps := []*ledger.Payment{
		{Client: "Клиент", Amount: 1000.10, Cash: true, OpDate: time.Now()},
		{Client: "Клиент", Amount: 1000.20, Cash: true, OpDate: time.Now()},
	}
	if got := suspicious(ps, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("different payments flagged as the same ruble amount: %v", got)
	}
	ps[1].Amount = ps[0].Amount
	if got := suspicious(ps, func(string) bool { return true }); len(got) != 1 || !strings.Contains(got[0], "1 000,10") {
		t.Fatalf("duplicate warning lost exact amount: %v", got)
	}
}

func TestMoneyDictatedPaymentKeepsKopecksInAcknowledgementAndLedger(t *testing.T) {
	grp := e2eGroup("dictated-kopecks")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ"}})
	out := e.tool("record_payment", grp, `{"client":"Исаев Харон","amount":0.25,"kind":"cash","group":"СБ"}`)
	if !strings.Contains(out, "0.25 ₽") {
		t.Fatalf("acknowledgement rounded real money: %s", out)
	}
	if total := sum(dayTotals(e, grp)); total != 0.25 {
		t.Fatalf("recorded amount differs from acknowledgement: %v", total)
	}
}
