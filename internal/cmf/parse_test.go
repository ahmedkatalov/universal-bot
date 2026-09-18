package cmf

import (
	"encoding/json"
	"testing"
)

// TestFlexibleParsing — толерантный разбор ответов программы: суммы строкой,
// альтернативные имена полей, дата без времени. Если реальный API отдаёт поля
// чуть иначе, сверка не должна МОЛЧА видеть нулевые суммы и пустые имена.
func TestFlexibleParsing(t *testing.T) {
	// Клиент: имя в поле "name", id в "client_id".
	var ci ClientInfo
	if err := json.Unmarshal([]byte(`{"client_id":"c-1","name":"Саралиева Милана","phone_number":"79380187222"}`), &ci); err != nil {
		t.Fatalf("ClientInfo: %v", err)
	}
	if ci.ID != "c-1" || ci.FullName != "Саралиева Милана" || ci.Phone != "79380187222" {
		t.Errorf("ClientInfo разобран неверно: %+v", ci)
	}

	// Платёж: сумма СТРОКОЙ "25000.00", дата без времени.
	var p Payment
	if err := json.Unmarshal([]byte(`{"sum":"25000.00","date":"2026-07-01"}`), &p); err != nil {
		t.Fatalf("Payment: %v", err)
	}
	if p.Amount != 25000 {
		t.Errorf("Payment.Amount = %d, ожидали 25000 (сумма строкой)", p.Amount)
	}
	if p.PaidAt.IsZero() {
		t.Errorf("Payment.PaidAt не разобрана из даты без времени")
	}

	// Платёж: сумма числом в стандартном поле — прежнее поведение сохраняется.
	var p2 Payment
	if err := json.Unmarshal([]byte(`{"amount":14000,"paid_at":"2026-07-01T18:25:00Z"}`), &p2); err != nil {
		t.Fatalf("Payment2: %v", err)
	}
	if p2.Amount != 14000 {
		t.Errorf("Payment2.Amount = %d, ожидали 14000", p2.Amount)
	}

	// Платёж: основной ключ null, значение в запасном — null НЕ должен коротить в 0.
	var p3 Payment
	if err := json.Unmarshal([]byte(`{"amount":null,"sum":"25000"}`), &p3); err != nil {
		t.Fatalf("Payment3: %v", err)
	}
	if p3.Amount != 25000 {
		t.Errorf("Payment3.Amount = %d, ожидали 25000 (null в amount -> берём sum)", p3.Amount)
	}

	// Договор: branchId (camelCase), номер строкой.
	var cr ContractRef
	if err := json.Unmarshal([]byte(`{"id":"ctr-1","branchId":"b-main","number":"200","product":"Ноутбук","remaining":"50000"}`), &cr); err != nil {
		t.Fatalf("ContractRef: %v", err)
	}
	if cr.ID != "ctr-1" || cr.BranchID != "b-main" || cr.Number != 200 || cr.Remaining != 50000 {
		t.Errorf("ContractRef разобран неверно: %+v", cr)
	}
}

// TestLooseItemsRobust — массив клиентов приходит и голым, и обёрнутым.
func TestLooseItemsRobust(t *testing.T) {
	for _, body := range []string{
		`[{"id":"a","full_name":"Иван"}]`,
		`{"items":[{"id":"a","full_name":"Иван"}]}`,
		`{"clients":[{"id":"a","full_name":"Иван"}]}`,
		`{"data":[{"id":"a","full_name":"Иван"}]}`,
	} {
		items := looseItems([]byte(body))
		if len(items) != 1 {
			t.Errorf("looseItems(%s) = %d элементов, ожидали 1", body, len(items))
		}
	}
}
