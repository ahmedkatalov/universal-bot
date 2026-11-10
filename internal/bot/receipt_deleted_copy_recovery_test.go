package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
)

// A deleted original plus a surviving stored duplicate is a historical state
// that an upgrade must repair, rather than requiring a new receipt upload.
func TestRereadGroupRestoresSurvivingReceiptCopyOnce(t *testing.T) {
	for _, rawOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_raw_only=%v", rawOnly), func(t *testing.T) {
			testRereadGroupRestoresSurvivingReceiptCopyOnce(t, rawOnly)
		})
	}
}

func TestDurableRevokeResumesCopyRecoveryAfterDeletionFlag(t *testing.T) {
	ctx := context.Background()
	group, worker := e2eGroup("revoke-copy-replay"), user("79280003996")
	e := newE2E(t, e2eOpts{})
	at := time.Now().Add(-time.Hour)
	original, copy := e2eID("REVOKE-ORIGINAL"), e2eID("REVOKE-COPY")
	var originalRaw int
	for _, waID := range []string{original, copy} {
		rawID, _, err := e.db.SaveRawMessage(ctx, waID, group.String(), worker.String(), worker.String(), "Мансур", "", true, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: rawID, GroupJID: group.String(), Amount: 30000, TxDate: at, IsDuplicate: waID == copy, SupersededByRaw: originalRaw}); err != nil {
			t.Fatal(err)
		}
		if waID == original {
			originalRaw = rawID
			// Simulate a prior attempt writing the flag but failing before
			// the copy existed or could be promoted.
			if _, _, err := e.db.MarkMessageDeleted(ctx, waID); err != nil {
				t.Fatal(err)
			}
		}
	}
	msg := groupText(group, worker, "Мансур", "")
	p := &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(), Key: &waProto.MessageKey{ID: proto.String(original), RemoteJID: proto.String(group.String())}}
	for range 2 {
		if err := e.applyDurableRevoke(ctx, msg, p); err != nil {
			t.Fatal(err)
		}
		if r := e.db.TestReceiptByMessage(ctx, copy); !r.Found || r.Duplicate || r.Amount != 30000 {
			t.Fatalf("completed revoke left the remaining copy suppressed: %+v", r)
		}
	}
}

func testRereadGroupRestoresSurvivingReceiptCopyOnce(t *testing.T, rawOnly bool) {
	t.Helper()
	ctx := context.Background()
	grp := e2eGroup("deleted-receipt-recovery")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Сбор СБ ЧЕКИ"}})
	day := time.Now().AddDate(0, 0, -1)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	var originalRaw int
	var originalWa string
	for i, row := range []struct {
		client string
		amount float64
	}{
		{"Эстамиров Ибрагим Муслимович", 50000},
		{"Хасуев Ислам", 30000},
		{"Мусаев Рахман", 65000},
		{"Ханатаев Акраман", 30000},
	} {
		wa := e2eID("DELETED-ORIGINAL")
		phone := fmt.Sprintf("7928000390%d", i)
		raw, _, err := e.db.SaveRawMessage(ctx, wa, grp.String(), user(phone).String(), phone, "Первый сотрудник", row.client, true, "", day.Add(time.Duration(i+1)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		cid, err := e.db.GetOrCreateContact(ctx, row.client)
		if err != nil {
			t.Fatal(err)
		}
		doc := fmt.Sprintf("receipt-operation-%d", i)
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, GroupJID: grp.String(), ContactID: &cid, RecipientRaw: row.client, ClientConfirmed: true, Amount: row.amount, DocNumber: doc, TxDate: day.Add(time.Duration(i+1) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			originalRaw, originalWa = raw, wa
		}
	}
	if _, _, err := e.db.MarkMessageDeleted(ctx, originalWa); err != nil {
		t.Fatal(err)
	}
	// Importing a copy after the deletion also models a missed old promotion.
	copyWa := e2eID("LIVE-COPY")
	body := "Ханатаев Акраман"
	if rawOnly {
		body = "СБЕРБАНК Чек по операции 30000 ₽"
	}
	copyRaw, _, err := e.db.SaveRawMessage(ctx, copyWa, grp.String(), user("79280003999").String(), "79280003999", "Оставшийся сотрудник", body, true, "", day.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cid, err := e.db.GetOrCreateContact(ctx, "Ханатаев Акраман")
	if err != nil {
		t.Fatal(err)
	}
	if rawOnly {
		for _, raw := range []int{originalRaw, copyRaw} {
			if err := e.db.SetMediaSha(ctx, raw, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: copyRaw, GroupJID: grp.String(), ContactID: &cid, RecipientRaw: "Ханатаев Акраман", ClientConfirmed: true, Amount: 30000, DocNumber: "receipt-operation-3", TxDate: day.Add(4 * time.Hour), IsDuplicate: true, SupersededByRaw: originalRaw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.db.MarkMessageParsed(ctx, copyRaw); err != nil {
		t.Fatal(err)
	}
	args := fmt.Sprintf(`{"from_date":%q,"to_date":%q,"group":"Сбор СБ ЧЕКИ","kind":"checks","view":"payments"}`, day.Format("2006-01-02"), day.Format("2006-01-02"))
	report := func() string {
		t.Helper()
		out, err := e.sendersTool(ctx, user(ownerPhone)).Handle(ctx, []byte(args))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if before := report(); !strings.Contains(before, "чеки 3 шт. на 145000 ₽") {
		t.Fatalf("fixture does not reproduce the missing payment: %s", before)
	}
	for i := 0; i < 2; i++ {
		out, err := e.rereadGroupTool(user(ownerPhone)).Handle(ctx, []byte(`{"group":"Сбор СБ ЧЕКИ","days":7,"client":"Ханатаев Акраман"}`))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !strings.Contains(out, "Живых копий выведено из дублей после удаления оригинала: 1") {
			t.Fatalf("reread did not disclose recovery: %s", out)
		}
		if after := report(); !strings.Contains(after, "чеки 4 шт. на 175000 ₽") || !strings.Contains(after, "Оставшийся сотрудник") {
			t.Fatalf("surviving copy not counted once under its own sender: %s", after)
		}
	}
	if source, err := e.db.RawMessageByWaID(ctx, originalWa); err != nil || !source.Deleted {
		t.Fatalf("recovery resurrected the deleted original: %+v / %v", source, err)
	}
}

func TestRereadGroupKeepsDeletedCopyConflictOnReview(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("deleted-copy-conflict-recovery")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Сбор СБ ЧЕКИ"}})
	e.aliases.Add("Ханатаев Акраман", "Ханатаев Акраман")
	at := time.Now().Add(-time.Hour)
	cid, err := e.db.GetOrCreateContact(ctx, "Ханатаев Акраман")
	if err != nil {
		t.Fatal(err)
	}
	var original int
	var copyWa string
	for i, amount := range []float64{30000, 31000} {
		wa := e2eID("COPY-AMOUNT-CONFLICT")
		raw, _, err := e.db.SaveRawMessage(ctx, wa, grp.String(), user("79280003888").String(), "79280003888", "Сотрудник", "Ханатаев Акраман", true, "", at.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: raw, GroupJID: grp.String(), ContactID: &cid, RecipientRaw: "Ханатаев Акраман", ClientConfirmed: true, Amount: amount, DocNumber: "1000000006086701079", TxDate: at, IsDuplicate: i == 1, SupersededByRaw: original}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.MarkMessageParsed(ctx, raw); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			original = raw
			if _, _, err := e.db.MarkMessageDeleted(ctx, wa); err != nil {
				t.Fatal(err)
			}
		} else {
			copyWa = wa
		}
	}
	out, err := e.rereadGroupTool(user(ownerPhone)).Handle(ctx, []byte(`{"group":"Сбор СБ ЧЕКИ","days":7,"client":"Ханатаев Акраман"}`))
	if err != nil {
		t.Fatal(err)
	}
	if receipt := e.db.TestReceiptByMessage(ctx, copyWa); !receipt.Found || receipt.Duplicate || !receipt.NeedsReview {
		t.Fatalf("recount cleared the copy conflict merely because the name was known: %+v / %s", receipt, out)
	}
	if !strings.Contains(out, "Отчёт предварительный") || !strings.Contains(out, "чеков на проверке: 1") {
		t.Fatalf("recovery did not disclose unresolved conflict: %s", out)
	}
}
