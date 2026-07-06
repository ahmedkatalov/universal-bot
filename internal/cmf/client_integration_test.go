package cmf

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mockCMF поднимает фейковый бэкенд рассрочек, повторяющий реальные эндпоинты,
// чтобы прогнать через него настоящий код клиента: логин с выбором профиля,
// поиск клиента, проверку внесён/не внесён платёж, запись платежа.
func mockCMF(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var addedPayments []string

	mux := http.NewServeMux()

	// Логин отвечает режимом select_profile (у аккаунта бота — бухгалтера — один профиль).
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"mode": "select_profile",
			"user": map[string]any{"id": "user-1"},
			"profiles": []map[string]any{
				{"id": "prof-1", "user_id": "user-1", "is_primary": true},
			},
		})
	})
	mux.HandleFunc("/api/auth/select-profile", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "jwt-token-123"})
	})

	// Поиск клиента по ФИО.
	mux.HandleFunc("/api/clients/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-token-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := strings.ToLower(r.URL.Query().Get("full_name"))
		var items []map[string]any
		if strings.Contains(name, "милана") || strings.Contains(name, "саралиева") {
			items = append(items, map[string]any{"id": "cli-milana", "full_name": "Саралиева Милана", "phone": "79380187222"})
		}
		if strings.Contains(name, "ахмед") {
			items = append(items, map[string]any{"id": "cli-ahmed", "full_name": "Каталов Ахмед Нажудович", "phone": "79280000000"})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	})

	// Договоры клиента (contracts-summary).
	mux.HandleFunc("/api/contracts/client/cli-milana/contracts-summary", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "contract-milana-1", "branch_id": "branch-grozny", "number": 142, "product_name": "iPhone", "remaining": 30000},
		})
	})
	mux.HandleFunc("/api/contracts/client/cli-ahmed/contracts-summary", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "contract-ahmed-1", "branch_id": "branch-main", "number": 200, "product_name": "Ноутбук", "remaining": 50000},
		})
	})

	// Платежи по договору. Требует X-Branch-ID (как в реальном роутере).
	mux.HandleFunc("/api/contract-payments/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("X-Branch-ID") == "" {
				http.Error(w, "branch required", http.StatusBadRequest)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			addedPayments = append(addedPayments, r.Header.Get("X-Branch-ID")+"|"+
				body["contract_id"].(string)+"|"+jsonNum(body["amount"]))
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Header.Get("X-Branch-ID") == "" {
			http.Error(w, "branch required", http.StatusBadRequest)
			return
		}
		contract := r.URL.Query().Get("contract_id")
		var payments []map[string]any
		// У Миланы платёж на 25000 уже ВНЕСЁН, у Ахмеда — платежей нет.
		if contract == "contract-milana-1" {
			payments = append(payments, map[string]any{"amount": 25000, "paid_at": "2026-07-01T18:25:00Z"})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": payments})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &addedPayments
}

func jsonNum(v any) string {
	if n, ok := v.(float64); ok {
		return strconv.FormatInt(int64(n), 10)
	}
	return "?"
}

func newTestClient(base string) *Client {
	return &Client{baseURL: base, email: "bot@x.ru", password: "pass", http: http.DefaultClient}
}

func TestCMFFullFlow(t *testing.T) {
	srv, added := mockCMF(t)
	c := newTestClient(srv.URL)
	ctx := t.Context()
	july1 := time.Date(2026, 7, 1, 18, 25, 0, 0, time.UTC)

	// 1. Поиск клиента (логин с select_profile проходит внутри).
	clients, err := c.LookupClients(ctx, "Саралиева Милана")
	if err != nil {
		t.Fatalf("LookupClients: %v", err)
	}
	if len(clients) != 1 || clients[0].ID != "cli-milana" {
		t.Fatalf("ожидали 1 клиента Милана, получили %+v", clients)
	}
	t.Logf("✓ клиент найден: %s (%s)", clients[0].FullName, clients[0].ID)

	// 2. Платёж Миланы на 25000 УЖЕ внесён -> HasPaymentAround = true.
	found, err := c.HasPaymentAround(ctx, "cli-milana", 25000, july1, 5)
	if err != nil || !found {
		t.Fatalf("Милана 25000 должен быть найден как внесённый: found=%v err=%v", found, err)
	}
	t.Log("✓ внесённый платёж (Милана 25000) определяется как ВНЕСЁН")

	// 3. Платёж Миланы на 40000 НЕ внесён -> HasPaymentAround = false.
	found, err = c.HasPaymentAround(ctx, "cli-milana", 40000, july1, 5)
	if err != nil || found {
		t.Fatalf("Милана 40000 не должен находиться: found=%v err=%v", found, err)
	}
	t.Log("✓ НЕвнесённый платёж (Милана 40000) определяется как НЕ ВНЕСЁН")

	// 4. Клиент Ахмед — платежей нет вообще -> не внесён.
	ahmed, _ := c.LookupClients(ctx, "Ахмед")
	if len(ahmed) != 1 {
		t.Fatalf("ожидали найти Ахмеда")
	}
	found, err = c.HasPaymentAround(ctx, "cli-ahmed", 14000, july1, 5)
	if err != nil || found {
		t.Fatalf("Ахмед 14000 не внесён: found=%v err=%v", found, err)
	}
	t.Log("✓ клиент без платежей -> чек НЕ ВНЕСЁН")

	// 5. Внесение платежа в программу (с X-Branch-ID из договора).
	contracts, err := c.ClientContracts(ctx, "cli-ahmed")
	if err != nil || len(contracts) != 1 {
		t.Fatalf("договоры Ахмеда: %+v err=%v", contracts, err)
	}
	if err := c.AddPayment(ctx, contracts[0].ID, contracts[0].BranchID, 14000, july1); err != nil {
		t.Fatalf("AddPayment: %v", err)
	}
	if len(*added) != 1 || (*added)[0] != "branch-main|contract-ahmed-1|14000" {
		t.Fatalf("платёж записан неверно: %+v", *added)
	}
	t.Log("✓ платёж внесён в программу: branch=branch-main contract=contract-ahmed-1 amount=14000")
}
