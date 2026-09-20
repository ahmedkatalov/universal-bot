package bot

import (
	"testing"
	"time"

	"whatsapp-bot/internal/cmf"
)

func d(day int) time.Time { return time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC) }

// TestMatchChecksToPayments — сценарий владельца: чек 20.08, оплата внесена 24.08
// той же суммой → чек ЗАСЧИТАН (даты приблизительные). Два чека той же суммы, а
// оплата одна → один засчитан, второй НЕ внесён.
func TestMatchChecksToPayments(t *testing.T) {
	// 1) Один чек 20.08 · 20000, оплата 24.08 · 20000 → внесён.
	m, un, left, _ := matchChecksToPayments(
		[]recCheck{{amount: 20000, date: d(20)}},
		[]cmf.Payment{{Amount: 20000, PaidAt: d(24)}},
	)
	if len(m) != 1 || len(un) != 0 || len(left) != 0 {
		t.Fatalf("одиночный чек+оплата: matches=%d unmatched=%d leftover=%d, ожидали 1/0/0", len(m), len(un), len(left))
	}

	// 2) Два чека 20.08 и 21.08 по 20000, но оплата ОДНА (24.08) → один внесён,
	//    второй НЕ внесён. Засчитаться должен ближайший по дате (21.08).
	m, un, left, _ = matchChecksToPayments(
		[]recCheck{{amount: 20000, date: d(20)}, {amount: 20000, date: d(21)}},
		[]cmf.Payment{{Amount: 20000, PaidAt: d(24)}},
	)
	if len(m) != 1 || len(un) != 1 || len(left) != 0 {
		t.Fatalf("два чека/одна оплата: matches=%d unmatched=%d leftover=%d, ожидали 1/1/0", len(m), len(un), len(left))
	}
	if !m[0].check.date.Equal(d(21)) {
		t.Errorf("засчитаться должен ближайший к оплате (24.08) чек 21.08, а засчитан %s", m[0].check.date.Format("02.01"))
	}
	if !un[0].date.Equal(d(20)) {
		t.Errorf("невнесённым должен остаться дальний чек 20.08, а остался %s", un[0].date.Format("02.01"))
	}

	// 3) Суммы программы в КОПЕЙКАХ — тоже совпадают.
	m, un, _, kop := matchChecksToPayments(
		[]recCheck{{amount: 15000, date: d(5)}},
		[]cmf.Payment{{Amount: 1500000, PaidAt: d(5)}},
	)
	if len(m) != 1 || len(un) != 0 || !kop {
		t.Fatalf("копейки: matches=%d unmatched=%d kopecks=%v, ожидали 1/0/true", len(m), len(un), kop)
	}

	// 4) Разные суммы не путаются: 20000 и 25000, оплаты 25000 и 20000 →
	//    оба чека внесены (сопоставление по сумме, не по порядку).
	m, un, left, _ = matchChecksToPayments(
		[]recCheck{{amount: 20000, date: d(10)}, {amount: 25000, date: d(11)}},
		[]cmf.Payment{{Amount: 25000, PaidAt: d(12)}, {Amount: 20000, PaidAt: d(13)}},
	)
	if len(m) != 2 || len(un) != 0 || len(left) != 0 {
		t.Fatalf("разные суммы: matches=%d unmatched=%d leftover=%d, ожидали 2/0/0", len(m), len(un), len(left))
	}

	// 5) Оплата без чека остаётся в leftover.
	_, un, left, _ = matchChecksToPayments(
		[]recCheck{{amount: 20000, date: d(10)}},
		[]cmf.Payment{{Amount: 20000, PaidAt: d(10)}, {Amount: 5000, PaidAt: d(10)}},
	)
	if len(un) != 0 || len(left) != 1 || left[0].Amount != 5000 {
		t.Fatalf("leftover: unmatched=%d leftover=%d, ожидали 0/1 (оплата 5000 без чека)", len(un), len(left))
	}
}
