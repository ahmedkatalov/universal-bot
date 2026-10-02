package bot

import "testing"

func TestNormCyr(t *testing.T) {
	cases := map[string]string{
		"Ахмед Каталов!":   "ахмед каталов",
		"  Каталов,  Ахмед": "каталов ахмед",
		"Катёлов":          "кателов",       // ё→е
		"Kаталов":          "каталов",        // латинская K→к
		" Cаралиева ":      "саралиева",      // латинская c→с
	}
	for in, want := range cases {
		if got := normCyr(in); got != want {
			t.Errorf("normCyr(%q) = %q, ожидали %q", in, got, want)
		}
	}
}

func TestWordSimilar(t *testing.T) {
	similar := [][2]string{
		{"каталов", "каталова"},   // склонение
		{"каталова", "каталов"},   // и наоборот
		{"катал", "каталов"},      // основа
		{"каталов", "котолов"},    // 2 опечатки, длинное слово
		{"ахмед", "ахмад"},        // 1 опечатка
		{"сулейманов", "сулейманова"},
	}
	for _, c := range similar {
		if !wordSimilar(c[0], c[1]) {
			t.Errorf("wordSimilar(%q,%q) = false, ожидали true", c[0], c[1])
		}
	}
	different := [][2]string{
		{"каталов", "сулейманов"},
		{"ахмед", "рамзан"},
		{"иван", "петр"},
		{"али", "иса"}, // короткие разные
	}
	for _, c := range different {
		if wordSimilar(c[0], c[1]) {
			t.Errorf("wordSimilar(%q,%q) = true, ожидали false", c[0], c[1])
		}
	}
}

func TestScoreCandidate(t *testing.T) {
	// Склонение в обоих словах — совпадают оба.
	if s := scoreCandidate(normWords("Ахмеда Каталова"), normWords("Ахмед Каталов")); s != 2 {
		t.Errorf("склонение: score=%d, ожидали 2", s)
	}
	// Опечатка в фамилии — всё равно оба слова совпадают (имя ввело в пул, фамилия по буквам).
	if s := scoreCandidate(normWords("Котолов Ахмед"), normWords("Ахмед Каталов")); s != 2 {
		t.Errorf("опечатка: score=%d, ожидали 2", s)
	}
	// Другой однофамилец-тёзка по фамилии — совпало только одно слово.
	if s := scoreCandidate(normWords("Ахмед Каталов"), normWords("Ахмед Висаев")); s != 1 {
		t.Errorf("тёзка: score=%d, ожидали 1", s)
	}
	// Совсем другой — ноль.
	if s := scoreCandidate(normWords("Ахмед Каталов"), normWords("Милана Саралиева")); s != 0 {
		t.Errorf("чужой: score=%d, ожидали 0", s)
	}
}
