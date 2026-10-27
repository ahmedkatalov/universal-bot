package bot

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// Чистая подпись-ФИО к чеку распознаётся без запроса к ИИ (быстрее), а шумная
// подпись по-прежнему идёт в ИИ.
func TestL9_CleanCaptionSkipsAI(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{WithAI: true})
	var nameCalls int32
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			atomic.AddInt32(&nameCalls, 1)
			u := c.lastUserText()
			// вернём «плательщика» из шумной подписи
			if strings.Contains(u, "Пияна") {
				return aiReply{Text: `{"client":"Ахмед"}`}
			}
			return aiReply{Text: `{"client":"` + strings.TrimSpace(u) + `"}`}
		}
		return aiReply{Text: ""}
	}

	// Чистая подпись — без ИИ.
	name, _ := e.payerFromCaption(ctx, "Магомедов Ахмед")
	if name == "" {
		t.Fatalf("чистую подпись «Магомедов Ахмед» не распознали")
	}
	if n := atomic.LoadInt32(&nameCalls); n != 0 {
		t.Errorf("чистая подпись зря дёрнула ИИ %d раз", n)
	}

	// Чистая подпись с суммой — тоже без ИИ.
	if name, amt := e.payerFromCaption(ctx, "Исаев Харон 15000"); name == "" || amt != 15000 {
		t.Errorf("«Исаев Харон 15000»: имя %q сумма %v", name, amt)
	}
	if n := atomic.LoadInt32(&nameCalls); n != 0 {
		t.Errorf("подпись с суммой зря дёрнула ИИ %d раз", n)
	}

	// Шумная подпись — через ИИ.
	if name, _ := e.payerFromCaption(ctx, "с карты Пияна Ахмед оплатил"); name == "" {
		t.Errorf("шумную подпись не разобрали")
	}
	if n := atomic.LoadInt32(&nameCalls); n != 1 {
		t.Errorf("шумная подпись должна была один раз вызвать ИИ, а вызвала %d", n)
	}
}
