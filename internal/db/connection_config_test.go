package db

import (
	"net/url"
	"strings"
	"testing"
)

func TestConnectionEnvironmentPreservesPassword(t *testing.T) {
	password := "тестовый пароль:@/#%?'\"$\\end"
	for key, value := range map[string]string{
		"PGHOST": "postgres", "PGPORT": "5432", "PGDATABASE": "finance",
		"PGUSER": "financebot", "PGPASSWORD": password, "PGSSLMODE": "disable",
	} {
		t.Setenv(key, value)
	}
	cfg, err := connectionConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Password != password {
		t.Error("password was changed during configuration parsing")
	}
	if cfg.ConnConfig.Host != "postgres" || cfg.ConnConfig.User != "financebot" || cfg.ConnConfig.Database != "finance" {
		t.Error("wrong database configuration")
	}
}

func TestConnectionURLCompatibilityAndErrorPrivacy(t *testing.T) {
	password := "пример@/#:password"
	u := url.URL{Scheme: "postgres", Host: "localhost:5432", Path: "/finance", User: url.UserPassword("financebot", password)}
	cfg, err := connectionConfig(u.String())
	if err != nil || cfg.ConnConfig.Password != password {
		t.Fatalf("encoded DATABASE_URL stopped working: %v", err)
	}
	_, err = connectionConfig("postgres://financebot:" + password + "@localhost/finance")
	if err == nil {
		t.Fatal("malformed URL must be rejected")
	}
	if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "пример") {
		t.Error("password leaked into the error")
	}
}
