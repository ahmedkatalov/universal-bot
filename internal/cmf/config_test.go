package cmf

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckedConfigurationDistinguishesDisabledAndMissingFields(t *testing.T) {
	for _, key := range []string{"CMF_API_URL", "CMF_EMAIL", "CMF_PASSWORD"} {
		t.Setenv(key, "")
	}
	if c, err := NewFromEnvChecked(); c != nil || err != nil {
		t.Fatalf("disabled integration: client=%v err=%v", c, err)
	}
	t.Setenv("CMF_API_URL", "https://example.invalid/api/")
	t.Setenv("CMF_PASSWORD", "private-test-password")
	c, err := NewFromEnvChecked()
	var cfg *ConfigurationError
	if c != nil || !errors.As(err, &cfg) || len(cfg.Missing) != 1 || cfg.Missing[0] != "CMF_EMAIL" {
		t.Fatalf("missing field not diagnosed: client=%v err=%v", c, err)
	}
	if strings.Contains(err.Error(), "private-test-password") || Human(err) != err.Error() {
		t.Fatal("configuration diagnosis leaks a value or loses its precise reason")
	}
}

func TestCheckedConfigurationRejectsWrongURLsWithoutLeakingValues(t *testing.T) {
	t.Setenv("CMF_EMAIL", "private-test-email")
	t.Setenv("CMF_PASSWORD", "private-test-password")
	for _, bad := range []string{
		"[https://example.invalid](https://example.invalid)",
		"example.invalid", "file:///private-test-password", "https://user:private-test-password@example.invalid",
		"https://example.invalid?token=private-test-password", "https://example.invalid/#private-test-password",
	} {
		t.Setenv("CMF_API_URL", bad)
		c, err := NewFromEnvChecked()
		if c != nil || err == nil || !strings.Contains(Human(err), "CMF_API_URL") {
			t.Errorf("invalid URL accepted or undiagnosed: client=%v err=%v", c, err)
			continue
		}
		if strings.Contains(err.Error(), "private-test-password") || strings.Contains(err.Error(), "private-test-email") {
			t.Fatal("configuration diagnosis leaked a secret")
		}
	}
}

func TestCheckedConfigurationNormalizesAPIPathAndKeepsPassword(t *testing.T) {
	t.Setenv("CMF_API_URL", " https://example.invalid/backend/api/ ")
	t.Setenv("CMF_EMAIL", "bot@example.invalid")
	t.Setenv("CMF_PASSWORD", "пароль-@+#")
	c, err := NewFromEnvChecked()
	if err != nil || c == nil || c.baseURL != "https://example.invalid/backend" || c.password != "пароль-@+#" {
		t.Fatalf("valid configuration was rejected or changed: error=%v", err)
	}
}
