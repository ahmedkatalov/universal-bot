package bot

import (
	"crypto/sha256"
	"strings"
	"testing"
)

// TestMatchesSecretCode — код сверяется по SHA-256; пробелы по краям игнорируются,
// любой другой текст (в т.ч. похожий) не подходит. Код в тесте — вымышленный.
func TestMatchesSecretCode(t *testing.T) {
	const code = "DUMMY-TEST-CODE-XYZ-123"
	b := &Bot{secretCodeSHA: sha256.Sum256([]byte(code))}
	if !b.matchesSecretCode(code) {
		t.Error("точный код должен совпасть")
	}
	if !b.matchesSecretCode("  " + code + "  ") {
		t.Error("код с пробелами по краям должен совпасть")
	}
	for _, wrong := range []string{"", "да", strings.ToLower(code), code + "X", code[:len(code)-1]} {
		if b.matchesSecretCode(wrong) {
			t.Errorf("неверный ввод %q не должен совпасть с кодом", wrong)
		}
	}
}

// TestSecretYesNo — распознавание подтверждения и отказа.
func TestSecretYesNo(t *testing.T) {
	yes := []string{"да", "Да.", "да, отправляй", "давай", "ок", "конечно"}
	no := []string{"нет", "Нет.", "не надо", "отмена", "стоп"}
	neither := []string{"а что это", "проверяю", "15000", "почему"}
	for _, s := range yes {
		if !secretIsYes(strings.ToLower(s)) {
			t.Errorf("secretIsYes(%q) = false, ожидали true", s)
		}
	}
	for _, s := range no {
		if !secretIsNo(strings.ToLower(s)) {
			t.Errorf("secretIsNo(%q) = false, ожидали true", s)
		}
	}
	for _, s := range neither {
		if secretIsYes(strings.ToLower(s)) || secretIsNo(strings.ToLower(s)) {
			t.Errorf("%q не должно быть ни да, ни нет", s)
		}
	}
}
