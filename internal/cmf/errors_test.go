package cmf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newErrTestClient(base string) *Client {
	return &Client{baseURL: base, email: "bot@x", password: "p", http: &http.Client{Timeout: 5 * time.Second}}
}

// Неверный пароль → понятная причина «неверный логин или пароль», а не «ошибка поиска».
func TestHumanBadLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, err := newErrTestClient(srv.URL).LookupClients(context.Background(), "Ахмед")
	if err == nil {
		t.Fatal("ожидали ошибку входа")
	}
	if h := Human(err); !strings.Contains(h, "логин или пароль") {
		t.Errorf("Human = %q, ожидали про логин/пароль", h)
	}
}

// Неверный путь (404) → подсказка проверить CMF_API_URL.
func TestHumanNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	err := newErrTestClient(srv.URL).Ping(context.Background())
	if err == nil || !strings.Contains(Human(err), "CMF_API_URL") {
		t.Errorf("Human = %q, ожидали подсказку про CMF_API_URL", Human(err))
	}
}

// Сеть недоступна → «не отвечает по сети».
func TestHumanNetwork(t *testing.T) {
	c := newErrTestClient("http://127.0.0.1:1") // порт закрыт
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Ping(ctx)
	if err == nil || !strings.Contains(Human(err), "по сети") {
		t.Errorf("Human = %q, ожидали про сеть", Human(err))
	}
}

// Временный сбой программы (503) повторяется и в итоге проходит.
func TestRetryOnServerError(t *testing.T) {
	var lookups int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
		case "/api/clients/lookup":
			if atomic.AddInt32(&lookups, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable) // первый раз — сбой
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "c1", "full_name": "Каталов Ахмед"}}})
		}
	}))
	defer srv.Close()
	got, err := newErrTestClient(srv.URL).LookupClients(context.Background(), "Ахмед")
	if err != nil || len(got) != 1 {
		t.Fatalf("после повтора ожидали 1 клиента, получили %v, err=%v", got, err)
	}
}

// Повторный поиск того же имени берётся из кэша, а не новым запросом.
func TestLookupCache(t *testing.T) {
	var lookups int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
		case "/api/clients/lookup":
			atomic.AddInt32(&lookups, 1)
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "c1", "full_name": "Каталов Ахмед"}}})
		}
	}))
	defer srv.Close()
	c := newErrTestClient(srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.LookupClients(context.Background(), "Ахмед"); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt32(&lookups); n != 1 {
		t.Errorf("ожидали 1 запрос к программе (кэш), было %d", n)
	}
}

// CMF_API_URL с хвостом «/api» не должен давать /api/api/... (404 на всё).
func TestNewFromEnvStripsAPISuffix(t *testing.T) {
	t.Setenv("CMF_API_URL", "https://cmf.example.com/api/")
	t.Setenv("CMF_EMAIL", "a@b")
	t.Setenv("CMF_PASSWORD", "x")
	c := NewFromEnv()
	if c == nil || c.baseURL != "https://cmf.example.com" {
		t.Errorf("baseURL = %q, ожидали без /api", c.baseURL)
	}
}
