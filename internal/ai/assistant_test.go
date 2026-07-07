package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRetryOnTransient проверяет, что клиент переживает временные сбои
// провайдера: сервер дважды отвечает 500, на третий раз — нормальный ответ.
// Без ретраев один такой сбой рушил бы весь ответ бота.
func TestRetryOnTransient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "overloaded", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "готово"}, "finish_reason": "stop"},
			},
		})
	}))
	defer srv.Close()

	a := New("key", "test-model", "", srv.URL)
	out, err := a.Complete(context.Background(), "sys", "посчитай")
	if err != nil {
		t.Fatalf("Complete после ретраев вернул ошибку: %v", err)
	}
	if out != "готово" {
		t.Fatalf("ожидали 'готово', получили %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("ожидали 3 попытки (2 сбоя + успех), было %d", got)
	}
}

// TestReplyToolLoop проверяет полный цикл tool use: модель сначала зовёт
// инструмент, бот выполняет его и возвращает результат, модель отдаёт финал.
func TestReplyToolLoop(t *testing.T) {
	var step int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&step, 1) == 1 {
			// Первый ответ: просим вызвать инструмент.
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call-1", "type": "function",
							"function": map[string]any{"name": "sbor", "arguments": `{"month":"июнь"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		// Второй ответ: финальный текст (после результата инструмента).
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "сбор за июнь: 100000"}, "finish_reason": "stop"},
			},
		})
	}))
	defer srv.Close()

	var toolRan bool
	tools := []Tool{{
		Name:        "sbor",
		Description: "сбор за период",
		InputSchema: map[string]any{"type": "object"},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			toolRan = true
			return "100000", nil
		},
	}}

	a := New("key", "test-model", "", srv.URL)
	out, err := a.Reply(context.Background(), "static", "dynamic", tools, nil, "сколько собрали в июне")
	if err != nil {
		t.Fatalf("Reply вернул ошибку: %v", err)
	}
	if !toolRan {
		t.Fatal("инструмент sbor не был выполнен")
	}
	if out != "сбор за июнь: 100000" {
		t.Fatalf("неожиданный финальный ответ: %q", out)
	}
}
