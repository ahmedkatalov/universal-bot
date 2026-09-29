package bot

import "testing"

func TestParseHM(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"10:00", 10, 0, true},
		{"9:30", 9, 30, true},
		{"10", 10, 0, true},
		{"10.15", 10, 15, true},
		{"23:59", 23, 59, true},
		{"24:00", 0, 0, false},
		{"10:99", 0, 0, false},
		{"вечером", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		h, m, ok := parseHM(c.in)
		if ok != c.ok || (ok && (h != c.h || m != c.m)) {
			t.Errorf("parseHM(%q) = (%d,%d,%v), ожидали (%d,%d,%v)", c.in, h, m, ok, c.h, c.m, c.ok)
		}
	}
}

func TestParseWeekday(t *testing.T) {
	cases := map[string]struct {
		wd int
		ok bool
	}{
		"пн":          {1, true},
		"понедельник": {1, true},
		"вт":          {2, true},
		"пятницу":     {5, true},
		"вс":          {0, true},
		"6":           {6, true},
		"0":           {0, true},
		"7":           {0, false},
		"завтра":      {0, false},
	}
	for in, want := range cases {
		wd, ok := parseWeekday(in)
		if ok != want.ok || (ok && wd != want.wd) {
			t.Errorf("parseWeekday(%q) = (%d,%v), ожидали (%d,%v)", in, wd, ok, want.wd, want.ok)
		}
	}
}
