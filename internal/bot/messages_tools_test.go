package bot

import "testing"

func TestOnlyDigits(t *testing.T) {
	cases := map[string]string{
		"9292":          "9292",
		"конец 92 92":   "9292",
		"+7 928 123-45": "792812345",
		"нет цифр":      "",
	}
	for in, want := range cases {
		if got := onlyDigits(in); got != want {
			t.Errorf("onlyDigits(%q) = %q, ожидали %q", in, got, want)
		}
	}
}

func TestLastDigits(t *testing.T) {
	if got := lastDigits("79281234592", 4); got != "4592" {
		t.Errorf("lastDigits = %q, ожидали 4592", got)
	}
	if got := lastDigits("92", 4); got != "92" {
		t.Errorf("lastDigits short = %q, ожидали 92", got)
	}
}
