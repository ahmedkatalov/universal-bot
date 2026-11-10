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

func TestGroupReadingNoteDoesNotHideReviewHeldOrLegacySources(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		status       db.GroupReadingStatus
	}{
		{"known-amount-receipt-review", "чеков на проверке: 2", db.GroupReadingStatus{UnresolvedMessages: 1, ReviewReceipts: 2}},
		{"held-payment", "платежей ждут решения о повторе: 1", db.GroupReadingStatus{UnresolvedMessages: 1, HeldPayments: 1}},
		{"legacy-parsed-media", "без записи чека или платежа: 1", db.GroupReadingStatus{UnresolvedMessages: 1, LegacyMediaCandidates: 1}},
		{"future-source-reason", "сообщений требуют проверки: 1", db.GroupReadingStatus{UnresolvedMessages: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := groupReadingIncompleteNote(tc.status, db.GroupInboxCoverage{})
			if !strings.Contains(note, "⚠️ Отчёт предварительный") || !strings.Contains(note, tc.detail) {
				t.Errorf("unresolved evidence produced a clean money report: %s", note)
			}
			if tc.status.LegacyMediaCandidates > 0 && !strings.Contains(note, "сумма не установлена") {
				t.Errorf("legacy source was treated as a receipt with a known amount: %s", note)
			}
		})
	}
}

func TestGroupReadingLegacyNamedPhotoWarningAppearsInMoneyReport(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("reading-legacy-hanataev")
	e := newE2E(t, e2eOpts{Groups: map[string]string{group.String(): "Сбор СБ ЧЕКИ"}})
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	raw, _, err := e.db.SaveRawMessage(ctx, e2eID("LEGACY-PHOTO"), group.String(), user("79280000003").String(), "79280000003", "~ M", "Ханатаев Акраман", true, "", day.Add(14*time.Hour+24*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
		t.Fatal(err)
	}
	for i, receipt := range []struct {
		client string
		amount float64
	}{
		{"Эстамиров Ибрагим Муслимович", 50000},
		{"Хасуев Ислам", 30000},
		{"Мусаев Рахман", 65000},
	} {
		posted := day.Add(time.Duration(i+1) * time.Hour)
		waID := e2eID("PARSED-CHECK")
		checkRaw, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), user("79280000004").String(), "79280000004", "Сотрудник", receipt.client, true, "", posted)
		if err != nil {
			t.Fatal(err)
		}
		contact, err := e.db.GetOrCreateContact(ctx, receipt.client)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: checkRaw, GroupJID: group.String(), ContactID: &contact, RecipientRaw: receipt.client, Amount: receipt.amount, ClientConfirmed: true, DocNumber: waID, TxDate: posted}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(ctx, checkRaw); err != nil {
			t.Fatal(err)
		}
	}
	note := e.groupReadingNote(ctx, day, day.AddDate(0, 0, 1), []string{group.String()})
	for _, want := range []string{"⚠️ Отчёт предварительный", "без записи чека или платежа: 1", "сумма не установлена"} {
		if !strings.Contains(note, want) {
			t.Errorf("parsed legacy photo disappeared from financial report warning %q: %s", want, note)
		}
	}
	status, _, err := e.db.GroupReadingSnapshot(ctx, day, day.AddDate(0, 0, 1), []string{group.String()}, 30)
	if err != nil {
		t.Fatal(err)
	}
	formatted := formatGroupReadingStatus(status, day, day, "Сбор СБ ЧЕКИ")
	if !strings.Contains(formatted, "помеченные разобранными без записи чека или платежа: 1") || strings.Contains(formatted, "нерешённых вопросов чтения не найдено") {
		t.Errorf("detailed coverage claimed that named legacy photo was settled: %s", formatted)
	}
	report := e.tool("senders_report", group, `{"from_date":"2026-11-01","to_date":"2026-11-01","group":"Сбор СБ ЧЕКИ","kind":"checks","view":"payments"}`)
	for _, want := range []string{"чеки 3 шт. на 145000 ₽", "⚠️ Отчёт предварительный", "без записи чека или платежа: 1", "сумма не установлена"} {
		if !strings.Contains(report, want) {
			t.Errorf("145,000 report silently omitted the named legacy source %q: %s", want, report)
		}
	}
	if strings.Contains(report, "175000") {
		t.Errorf("coverage warning invented the missing receipt's amount: %s", report)
	}
}

func TestGroupReadingWarnsMoneyReportAboutCopyOfDeletedReceipt(t *testing.T) {
	ctx := context.Background()
	group := e2eGroup("reading-deleted-receipt")
	e := newE2E(t, e2eOpts{Groups: map[string]string{group.String(): "СБ"}})
	posted := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	client := "Ханатаев Акраман"
	contact, err := e.db.GetOrCreateContact(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	doc := e2eID("RECEIPT-DOC")
	add := func(waID string, at time.Time, duplicate bool) {
		t.Helper()
		raw, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), user("79280000004").String(), "79280000004", "Сотрудник", client, true, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, GroupJID: group.String(), ContactID: &contact, RecipientRaw: client, Amount: 30000, ClientConfirmed: true, DocNumber: doc, TxDate: posted, IsDuplicate: duplicate}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	original := e2eID("ORIGINAL")
	add(original, posted.AddDate(0, 0, -1), false)
	if _, _, err := e.db.MarkMessageDeleted(ctx, original); err != nil {
		t.Fatal(err)
	}
	// A historically suppressed copy can appear after the original's revoke.
	add(e2eID("COPY"), posted, true)
	report := e.tool("senders_report", group, `{"from_date":"2026-11-08","to_date":"2026-11-08","group":"СБ","kind":"checks","view":"payments"}`)
	for _, want := range []string{"⚠️ Отчёт предварительный", "копий чеков без действующей записи после удаления оригиналов: 1"} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q: %s", want, report)
		}
	}
	if strings.Contains(report, "30000") {
		t.Errorf("report treated suppressed evidence as a payment: %s", report)
	}
}
