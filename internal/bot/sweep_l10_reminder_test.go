package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// «Завтра напомни мне в это время»: разовое напоминание без явного времени
// ставится на то же время следующего дня и бот сам пишет владельцу.
func TestL10_ReminderTomorrowSameTimeToOwner(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	grp := e2eGroup("rem")
	owner := user(ownerPhone)
	tool := e.scheduleReminderTool(grp, owner)

	out, err := tool.Handle(ctx, json.RawMessage(`{"kind":"once","date":"завтра","target":"me","message":"Проверь остатки по рассрочкам"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "напоминание") {
		t.Fatalf("не создалось напоминание: %s", out)
	}

	// До срока — молчит.
	n := len(e.sent())
	e.runDueJobsAt(time.Now().Add(1 * time.Hour))
	if got := e.sentSince(n); len(got) != 0 {
		t.Fatalf("сработало раньше срока: %+v", got)
	}

	// Завтра в это же время — бот пишет владельцу.
	n = len(e.sent())
	e.runDueJobsAt(time.Now().Add(24*time.Hour + 2*time.Minute))
	got := e.sentSince(n)
	if len(got) != 1 || !strings.Contains(got[0].Chat, owner.User) || !strings.Contains(got[0].Text, "Проверь остатки") {
		t.Fatalf("бот не напомнил владельцу вовремя: %+v", got)
	}
}

// «Напомни ему через N минут»: разовое напоминание конкретному человеку по
// номеру — бот пишет именно ему.
func TestL10_ReminderToPerson(t *testing.T) {
	ctx := context.Background()
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	grp := e2eGroup("rem2")
	owner := user(ownerPhone)
	tool := e.scheduleReminderTool(grp, owner)

	out, err := tool.Handle(ctx, json.RawMessage(`{"kind":"once","in_minutes":1,"target":"person","number":"79991234567","message":"Скинь чеки за сегодня"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "+79991234567") && !strings.Contains(out, "79991234567") {
		t.Fatalf("получатель не тот: %s", out)
	}
	n := len(e.sent())
	e.runDueJobsAt(time.Now().Add(2 * time.Minute))
	got := e.sentSince(n)
	if len(got) != 1 || !strings.Contains(got[0].Chat, "79991234567") || !strings.Contains(got[0].Text, "Скинь чеки") {
		t.Fatalf("бот не написал человеку: %+v (ожидали JID 79991234567)", got)
	}
}
