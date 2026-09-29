package db

import (
	"testing"
	"time"
)

// TestNextDailyRun — ближайшее ЧЧ:ММ строго после now (сегодня, если ещё не
// прошло; иначе завтра).
func TestNextDailyRun(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 30, 9, 0, 0, 0, loc) // 09:00
	// 10:00 сегодня — ещё впереди.
	if got := nextDailyRun(now, 10, 0); !got.Equal(time.Date(2026, 9, 30, 10, 0, 0, 0, loc)) {
		t.Errorf("10:00 при now=09:00 должно быть сегодня, got %v", got)
	}
	// 08:00 уже прошло — завтра.
	if got := nextDailyRun(now, 8, 0); !got.Equal(time.Date(2026, 10, 1, 8, 0, 0, 0, loc)) {
		t.Errorf("08:00 при now=09:00 должно быть завтра, got %v", got)
	}
	// Ровно текущее время считается прошедшим -> завтра.
	if got := nextDailyRun(now, 9, 0); !got.Equal(time.Date(2026, 10, 1, 9, 0, 0, 0, loc)) {
		t.Errorf("09:00 при now=09:00 должно быть завтра, got %v", got)
	}
}

// TestNextWeeklyRun — ближайший день недели в ЧЧ:ММ строго после now.
func TestNextWeeklyRun(t *testing.T) {
	loc := time.UTC
	// 2026-09-30 — среда (Weekday()=3).
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, loc)
	// Пятница (5) на этой неделе -> 2026-10-02.
	if got := nextWeeklyRun(now, 5, 9, 0); !got.Equal(time.Date(2026, 10, 2, 9, 0, 0, 0, loc)) {
		t.Errorf("пятница 09:00 -> 02.10, got %v", got)
	}
	// Та же среда, но время уже прошло (10:00 < 12:00) -> через неделю.
	if got := nextWeeklyRun(now, 3, 10, 0); !got.Equal(time.Date(2026, 10, 7, 10, 0, 0, 0, loc)) {
		t.Errorf("среда 10:00 при now=среда 12:00 -> +неделя (07.10), got %v", got)
	}
	// Та же среда, время ещё впереди (15:00) -> сегодня.
	if got := nextWeeklyRun(now, 3, 15, 0); !got.Equal(time.Date(2026, 9, 30, 15, 0, 0, 0, loc)) {
		t.Errorf("среда 15:00 при now=среда 12:00 -> сегодня, got %v", got)
	}
}

// TestFirstRun — разовое в прошлом отвергается, в будущем принимается.
func TestFirstRun(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if _, err := firstRun(now, "once", 0, 0, -1, now.Add(-time.Hour)); err == nil {
		t.Errorf("разовое в прошлом должно отвергаться")
	}
	if got, err := firstRun(now, "once", 0, 0, -1, now.Add(time.Hour)); err != nil || !got.Equal(now.Add(time.Hour)) {
		t.Errorf("разовое в будущем: got %v err %v", got, err)
	}
	if _, err := firstRun(now, "хрень", 0, 0, -1, time.Time{}); err == nil {
		t.Errorf("неизвестный kind должен давать ошибку")
	}
}
