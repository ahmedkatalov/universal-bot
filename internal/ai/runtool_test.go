package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestRunToolReportsFailure — упавший инструмент помечается для модели как НЕ
// ВЫПОЛНЕННЫЙ (чтобы она не выдала неуспех за успех); успешный проходит как есть.
func TestRunToolReportsFailure(t *testing.T) {
	failing := []Tool{{
		Name: "record_payment",
		Handle: func(ctx context.Context, in json.RawMessage) (string, error) {
			return "", errors.New("не удалось записать платёж")
		},
	}}
	out := runTool(context.Background(), failing, toolCall{Function: toolCallFunc{Name: "record_payment", Arguments: "{}"}})
	if !strings.Contains(out, "НЕ ВЫПОЛНЕН") || !strings.Contains(out, "не удалось записать платёж") {
		t.Errorf("упавший инструмент должен быть явно помечен как невыполненный: %q", out)
	}

	okTool := []Tool{{
		Name:   "record_payment",
		Handle: func(ctx context.Context, in json.RawMessage) (string, error) { return "Записал: 5000 ₽", nil },
	}}
	if got := runTool(context.Background(), okTool, toolCall{Function: toolCallFunc{Name: "record_payment", Arguments: "{}"}}); got != "Записал: 5000 ₽" {
		t.Errorf("успешный результат должен проходить как есть: %q", got)
	}
}
