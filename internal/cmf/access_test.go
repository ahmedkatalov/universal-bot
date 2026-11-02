package cmf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionCheckRequiresWorkingJSONSearch(t *testing.T) {
	for _, response := range []struct {
		name, body string
		status     int
	}{
		{"html-login-page", "<html>Login</html>", 200},
		{"wrong-api-envelope", `{"error":"permission denied"}`, 200},
		{"null", `null`, 200},
		{"null-data", `{"data":null}`, 200},
		{"error-with-empty-data", `{"success":false,"data":[]}`, 200},
		{"error-with-null-data", `{"success":false,"data":null}`, 200},
		{"null-item", `[null]`, 200},
		{"string-item", `["denied"]`, 200},
		{"error-item", `[{"error":"denied"}]`, 200},
		{"missing-client-id", `[{"full_name":"Клиент"}]`, 200},
		{"bad-query", `{"error":"invalid query"}`, 400},
		{"unprocessable-query", `{"error":"validation failed"}`, 422},
	} {
		t.Run(response.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/auth/login" {
					_, _ = w.Write([]byte(`{"token":"test-token"}`))
					return
				}
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer srv.Close()
			err := newErrTestClient(srv.URL).Ping(context.Background())
			var api *APIError
			if !errors.As(err, &api) || api.Op != "поиск клиента" {
				t.Fatalf("a non-working search was reported ready: err=%v", err)
			}
			if response.status == 200 && !IsOutage(err) {
				t.Fatal("invalid API schema did not trigger program-health diagnostics")
			}
		})
	}
}

func TestUnexpectedProgramResponsesAreErrorsNotEmptyAccountingLists(t *testing.T) {
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
			return
		}
		if r.URL.Path == "/api/clients/lookup" && lookups.Add(1) > 1 {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"not authorized"}`))
	}))
	defer srv.Close()
	c := newErrTestClient(srv.URL)
	ctx := context.Background()
	if clients, err := c.LookupClients(ctx, "Клиент"); err == nil {
		t.Fatalf("invalid response became a cached empty client list: %+v", clients)
	}
	if clients, err := c.LookupClients(ctx, "Клиент"); err != nil || len(clients) != 0 || lookups.Load() != 2 {
		t.Fatalf("invalid response poisoned cache or valid empty list rejected: clients=%+v requests=%d err=%v", clients, lookups.Load(), err)
	}
	if contracts, err := c.ClientContracts(ctx, "client-1"); err == nil {
		t.Fatalf("failed permission reply became no contracts: %+v", contracts)
	}
	if payments, err := c.ContractPayments(ctx, "contract-1", "branch-1", time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatalf("failed permission reply became no payments: %+v", payments)
	}
}
