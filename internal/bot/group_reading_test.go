package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"whatsapp-bot/internal/db"
)

func TestGroupReadingStatusExplainsLimitedCoverage(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	text := formatGroupReadingStatus(db.GroupReadingStatus{
		SavedMessages: 10, SettledMessages: 6, UnresolvedMessages: 4, UnreadMedia: 2, UnreadTexts: 1, UnknownReceiptAmounts: 1,
		Issues: []db.GroupReadingIssue{{RawID: 42, WaID: "wa-example", Sender: "Мансур", GroupJID: "g@g.us", ReceivedAt: day, Reasons: []string{"сумма не прочитана"}}}, Truncated: true,
	}, day, day, "СБ")
	for _, want := range []string{"Неразобранные вложения: 2", "с неизвестной суммой: 1", "raw_id=42", "wa_message_id=wa-example", "Показано 1 из 4", "неизвестные суммы", "CMF"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	empty := formatGroupReadingStatus(db.GroupReadingStatus{}, day, day, "СБ")
	if !strings.Contains(empty, "Это не означает, что в WhatsApp группа была пуста") {
		t.Fatalf("absence incorrectly proves complete history: %s", empty)
	}
	complete := formatGroupReadingStatus(db.GroupReadingStatus{SavedMessages: 2, SettledMessages: 2}, day, day, "СБ")
	if !strings.Contains(complete, "не подтверждает полноту всей истории WhatsApp") {
		t.Fatalf("unwarranted completeness claim: %s", complete)
	}
}

func TestGroupReadingReportsConflictsEvenWhenEverySourceWasParsed(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("reading-conflict")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "СБ"}})
	posted := time.Now()
	doc := e2eID("CONFLICT-DOC")
	for _, client := range []string{"Исаев Харон", "Магомедов Ахмед"} {
		raw, _, err := e.db.SaveRawMessage(ctx, e2eID("CHECK"), grp.String(), user("79280000101").String(), "", "Мансур", client, true, "", posted)
		if err != nil {
			t.Fatal(err)
		}
		cid, err := e.db.GetOrCreateContact(ctx, client)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, GroupJID: grp.String(), ContactID: &cid, RecipientRaw: client, Amount: 1000.10, ClientConfirmed: true, DocNumber: doc, TxDate: posted}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	note := e.groupReadingNote(ctx, posted.Add(-time.Minute), posted.Add(time.Minute), []string{grp.String()})
	if !strings.Contains(note, "у 1 платежей") || !strings.Contains(note, "не включены в сбор") {
		t.Fatalf("parsed sources hid a financial conflict: %s", note)
	}
}

func TestGroupReadingToolCombinesRawAndInboxWithoutLosingEdits(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("reading-status")
	e := newE2E(t, e2eOpts{Groups: map[string]string{group.String(): "СБ"}})
	waID := e2eID("UNREAD")
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), user("79280000101").String(), "79280000101", "Мансур", "", true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ wa, kind string }{{waID, "message"}, {waID, "edit"}, {e2eID("NEW"), "message"}} {
		_, _, err = e.db.CaptureGroupEvent(ctx, db.GroupInboxEvent{Key: e2eID("EVENT"), WaID: event.wa, GroupJID: group.String(), Kind: event.kind, Metadata: []byte(`{}`), Payload: []byte("test"), ReceivedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}
	day := time.Now().Format("2006-01-02")
	out, err := e.groupReadingTool().Handle(ctx, json.RawMessage(fmt.Sprintf(`{"from_date":%q,"to_date":%q,"group":"СБ"}`, day, day)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Сохранено сообщений: 1", "Неразобранные вложения: 1", fmt.Sprintf("raw_id=%d", rawID), "ждут обработки 2", "вид=edit", "не дополнительные чеки"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	from, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	note := e.groupReadingNote(ctx, from, from.AddDate(0, 0, 1), []string{group.String()})
	if !strings.Contains(note, "предварительный") || !strings.Contains(note, "2 событий") {
		t.Fatalf("money report hides incomplete reading: %s", note)
	}
}

func TestGroupReadingNoteIncludesUnfinishedInboxWithoutAddingMoney(t *testing.T) {
	note := groupReadingIncompleteNote(db.GroupReadingStatus{UnreadMedia: 1, UnknownReceiptAmounts: 2}, db.GroupInboxCoverage{Pending: 1, Processing: 1, Review: 1})
	for _, want := range []string{"предварительный", "не все обязательно чеки", "с неизвестной суммой: 2", "3 событий", "не равны нулю", "CMF"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q: %s", want, note)
		}
	}
	if note := groupReadingIncompleteNote(db.GroupReadingStatus{}, db.GroupInboxCoverage{}); note != "" {
		t.Fatalf("settled evidence requires no incomplete-reading note: %s", note)
	}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	text := formatGroupReadingStatus(db.GroupReadingStatus{SavedMessages: 1, SettledMessages: 1}, day, day, "СБ", db.GroupInboxCoverage{Pending: 1})
	if strings.Contains(text, "нерешённых вопросов чтения не найдено") || !strings.Contains(text, "В очереди приёма") {
		t.Fatalf("pending inbox falsely reported complete: %s", text)
	}
}
