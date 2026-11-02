package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestCMFAccessOwnerLIDKeepsVerifiedPermissionsWithoutSenderAlt(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, Owners: []string{ownerPhone}, CMFClients: katalov})
	lid := types.NewJID("330000000000001", types.HiddenUserServer)
	if err := e.db.RememberSenderIdentity(context.Background(), lid.String(), user(ownerPhone).String()); err != nil {
		t.Fatal(err)
	}
	msg := privateText(lid, "Владелец", "Проверь, есть ли доступ к программе")
	if !e.isReportAdmin(msg.Info) || !e.verifiedSenderInSet(e.ownerNumbers, msg.Info) {
		t.Fatal("persisted WhatsApp identity lost owner permissions without SenderAlt")
	}
	e.ai = func(c aiCall) aiReply {
		if c.hasToolResult() {
			return aiReply{Text: strings.Join(c.toolResults(), " ")}
		}
		if !strings.Contains(strings.Join(c.Tools, " "), "cmf_connection_check") {
			t.Error("verified owner was denied CMF tools")
			return aiReply{Text: "Ошибка доступа"}
		}
		if !strings.Contains(c.System, "обязательно вызови cmf_connection_check") {
			t.Error("assistant was not instructed to verify access before denying it")
		}
		return aiReply{ToolCalls: []aiToolCall{{Name: "cmf_connection_check", Args: `{}`}}}
	}
	e.handlePrivateMessage(context.Background(), msg)
	if out := e.last().Text; !strings.Contains(out, "вход и поиск клиентов работают") {
		t.Fatalf("CMF access through private owner LID failed: %s", out)
	}
	stranger := privateText(types.NewJID(ownerPhone, types.HiddenUserServer), "Владелец", "Покажи программу")
	if e.isReportAdmin(stranger.Info) || e.verifiedSenderInSet(e.ownerNumbers, stranger.Info) {
		t.Fatal("unverified LID digits or display name granted access")
	}
}

func TestCMFAccessCheckExplainsIncompleteConfiguration(t *testing.T) {
	t.Setenv("CMF_API_URL", "https://cmf.invalid")
	t.Setenv("CMF_EMAIL", "bot@example.invalid")
	t.Setenv("CMF_PASSWORD", "")
	b := &Bot{}
	out, err := b.cmfConnectionTool().Handle(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "CMF_PASSWORD") || strings.Contains(out, "bot@example.invalid") || strings.Contains(out, "вход и поиск клиентов работают") {
		t.Fatalf("missing configuration was hidden or secrets exposed: %q err=%v", out, err)
	}
}

func TestCMFAccessCheckDetectsStaleProcessConfiguration(t *testing.T) {
	t.Setenv("CMF_API_URL", "https://cmf.invalid")
	t.Setenv("CMF_EMAIL", "bot@example.invalid")
	t.Setenv("CMF_PASSWORD", "test-only-password")
	b := &Bot{}
	out, err := b.cmfConnectionTool().Handle(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, "Пересоздай контейнер") || strings.Contains(out, "test-only-password") || strings.Contains(out, "вход и поиск клиентов работают") {
		t.Fatalf("stale process claimed working access or leaked a password: %q err=%v", out, err)
	}
}
