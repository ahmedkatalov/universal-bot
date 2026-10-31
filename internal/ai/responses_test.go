package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func responseText(text string) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text}}}
}

func responseCall(id, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": args}
}

func TestResponsesToolLoopPreservesReasoningAndContext(t *testing.T) {
	step, executed := 0, 0
	reasoning := map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "encrypted-state"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("wrong endpoint/auth")
			w.WriteHeader(400)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		step++
		if req["store"] != false || req["parallel_tool_calls"] != false {
			t.Error("unexpected storage/parallel settings")
		}
		if _, ok := req["max_tokens"]; ok {
			t.Error("legacy token parameter in Responses")
		}
		if req["max_output_tokens"] != float64(defaultChatMaxTokens) {
			t.Error("missing reasoning token budget")
		}
		input := req["input"].([]any)
		first := input[0].(map[string]any)
		if first["role"] != "developer" || first["content"] != "static\n\ndynamic" {
			t.Errorf("system prompt lost: %v", first)
		}
		if req["reasoning"].(map[string]any)["effort"] != "medium" {
			t.Error("wrong chat reasoning")
		}
		tool := req["tools"].([]any)[0].(map[string]any)
		if tool["name"] != "save_payment" || tool["strict"] != false || tool["function"] != nil {
			t.Errorf("wrong tool shape: %v", tool)
		}
		if step == 1 {
			if len(input) != 4 || input[1].(map[string]any)["content"] != "предыдущее сообщение" || input[2].(map[string]any)["role"] != "assistant" {
				t.Error("conversation history lost")
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{reasoning, responseText("Проверяю платёж"), responseCall("call_1", "save_payment", `{"amount":20100}`)}})
			return
		}
		if len(input) != 8 {
			t.Errorf("unexpected replay size: %d", len(input))
		}
		if !reflect.DeepEqual(input[4], reasoning) {
			t.Error("encrypted reasoning not replayed")
		}
		last := input[len(input)-1].(map[string]any)
		if last["type"] != "function_call_output" || last["call_id"] != "call_1" || last["output"] != "saved:20100" {
			t.Errorf("wrong result linkage: %v", last)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{responseText("Записал 20 100 ₽")}})
	}))
	defer srv.Close()
	a := New("test-key", defaultModel, "", srv.URL)
	tools := []Tool{{Name: "save_payment", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"amount": map[string]any{"type": "number"}}}, Handle: func(_ context.Context, b json.RawMessage) (string, error) { executed++; return "saved:20100", nil }}}
	out, err := a.Reply(t.Context(), "static", "dynamic", tools, []Turn{{FromUser: true, Text: "предыдущее сообщение"}, {Text: "понял"}}, "запиши 20100")
	if err != nil || out != "Записал 20 100 ₽" || executed != 1 || step != 2 {
		t.Fatalf("reply=%q err=%v executions=%d steps=%d", out, err, executed, step)
	}
}

func TestResponsesImageAndExtraction(t *testing.T) {
	withImage := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["reasoning"].(map[string]any)["effort"] != "low" {
			t.Error("extraction should use low reasoning")
		}
		if withImage {
			parts := req["input"].([]any)[1].(map[string]any)["content"].([]any)
			img := parts[1].(map[string]any)
			if img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,AQID" || img["detail"] != "high" {
				t.Errorf("wrong image: %v", img)
			}
			if req["max_output_tokens"] != float64(defaultVisionMaxTokens) {
				t.Error("wrong image token budget")
			}
		}
		// Шлюз может не возвращать status.
		json.NewEncoder(w).Encode(map[string]any{"output": []any{responseText(`{"amount":20100}`)}})
	}))
	defer srv.Close()
	a := New("test-key", defaultModel, "", srv.URL)
	if out, err := a.Complete(t.Context(), "JSON", "платёж"); err != nil || out != `{"amount":20100}` {
		t.Fatalf("Complete: %q %v", out, err)
	}
	withImage = true
	if out, err := a.CompleteWithImage(t.Context(), "JSON", "чек", []byte{1, 2, 3}, "image/png"); err != nil || out != `{"amount":20100}` {
		t.Fatalf("Image: %q %v", out, err)
	}
}

func TestResponsesRejectsIncompleteAndInvalidCalls(t *testing.T) {
	for _, body := range []string{
		`{"status":"incomplete","output":[{"type":"function_call","call_id":"c","name":"save","arguments":"{}"}]}`,
		`{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"save","arguments":"{"}]}`,
		`{"output":[{"type":"function_call","call_id":"c","name":"save","arguments":"null"}]}`,
		`{"output":[{"type":"function_call","name":"save","arguments":"{}"}]}`,
		`{"output":[{"type":"reasoning","summary":[]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer srv.Close()
			a := New("key", defaultModel, "", srv.URL)
			ran := false
			_, err := a.Reply(t.Context(), "", "", []Tool{{Name: "save", Handle: func(context.Context, json.RawMessage) (string, error) { ran = true; return "saved", nil }}}, nil, "save")
			if err == nil || ran {
				t.Errorf("invalid response executed tool: ran=%v err=%v", ran, err)
			}
		})
	}
}

func TestResponsesRepeatedCallDoesNotRepeatWrite(t *testing.T) {
	steps, ran := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		var output any = responseCall("same_call", "save", `{}`)
		if steps == 3 {
			output = responseText("готово")
		}
		json.NewEncoder(w).Encode(map[string]any{"output": []any{output}})
	}))
	defer srv.Close()
	a := New("key", defaultModel, "", srv.URL)
	_, err := a.Reply(t.Context(), "", "", []Tool{{Name: "save", Handle: func(context.Context, json.RawMessage) (string, error) { ran++; return "saved", nil }}}, nil, "save")
	if err != nil || ran != 1 {
		t.Errorf("write executed %d times: %v", ran, err)
	}
}

func TestResponsesFallbackAndKeyRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] == "gpt-primary" {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "missing model for private-key"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"output": []any{responseText("ok")}})
	}))
	defer srv.Close()
	a := New("private-key", "gpt-primary", "", srv.URL)
	if _, err := a.Complete(t.Context(), "", "test"); err == nil || strings.Contains(err.Error(), "private-key") {
		t.Errorf("credential leaked: %v", err)
	}
	a.ResetBreaker()
	a.fallbackModels = []string{"gpt-backup"}
	if out, err := a.Complete(t.Context(), "", "test"); err != nil || out != "ok" {
		t.Errorf("fallback: %q %v", out, err)
	}
}

func TestResponsesConnectionCheck(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		var output any = responseText(`{"ok":true}`)
		if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
			hasResult := false
			for _, item := range req["input"].([]any) {
				if item.(map[string]any)["type"] == "function_call_output" {
					hasResult = true
				}
			}
			if hasResult {
				output = responseText("ок")
			} else {
				output = responseCall("probe", "connection_check", `{}`)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"output": []any{output}})
	}))
	defer srv.Close()
	a := New("test-key", defaultModel, "", srv.URL)
	if err := a.CheckConnection(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != 4 {
		t.Errorf("проверки текста/фото/инструмента: %d запросов вместо 4", requests)
	}
}
