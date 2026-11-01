package db

import (
	"context"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"testing"
	"time"
)

func TestSenderIdentityRequiresObservedAliasAndRejectsRemapping(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	n := time.Now().UnixNano()
	lid := fmt.Sprintf("%d@lid", n)
	phone := fmt.Sprintf("7928%07d@s.whatsapp.net", n%10000000)
	actor, p, err := d.ResolveSenderIdentity(ctx, lid)
	if err != nil || actor != lid || p != "" {
		t.Fatalf("unknown LID guessed as telephone: %q %q %v", actor, p, err)
	}
	raw, _, err := d.SaveRawMessage(ctx, fmt.Sprintf("identity-%d", n), "group-identity@g.us", lid, "", "Мансур", "", false, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RememberSenderIdentity(ctx, lid, phone); err != nil {
		t.Fatal(err)
	}
	actor, p, err = d.ResolveSenderIdentity(ctx, lid)
	if err != nil || actor != phone || p != fmt.Sprintf("7928%07d", n%10000000) {
		t.Fatalf("observed mapping lost: %q %q %v", actor, p, err)
	}
	all, err := d.RawSenderIdentities(ctx, []int{raw})
	if err != nil || all[raw].ActorJID != phone || all[raw].Phone != p {
		t.Fatalf("raw identity backfill: %+v / %v", all, err)
	}
	other := fmt.Sprintf("7928%07d@s.whatsapp.net", (n+1)%10000000)
	if err := d.RememberSenderIdentity(ctx, lid, other); !errors.Is(err, ErrSenderIdentityConflict) {
		t.Fatalf("conflicting remapping accepted: %v", err)
	}
	actor, _, _ = d.ResolveSenderIdentity(ctx, lid)
	if actor != phone {
		t.Fatal("a conflict rewrote the historical actor")
	}
}

func TestSenderIdentityPairingUsesPNAndLIDWithoutNameGuess(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	n := time.Now().UnixNano()
	group := fmt.Sprintf("identity-%d@g.us", n)
	lid := fmt.Sprintf("%d@lid", n)
	phone := fmt.Sprintf("7928%07d@s.whatsapp.net", n%10000000)
	other := fmt.Sprintf("7928%07d@s.whatsapp.net", (n+1)%10000000)
	raw, _, err := d.SaveRawMessage(ctx, fmt.Sprintf("name-%d", n), group, lid, "", "Мансур", "Каталов Ахмед", false, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnqueuePendingName(ctx, group, lid, "Каталов Ахмед", 0, raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.RememberSenderIdentity(ctx, lid, phone); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := d.TakePendingName(ctx, group, other, time.Now().Add(-time.Hour)); err != nil || ok {
		t.Fatalf("other employee stole the name: %v %v", ok, err)
	}
	name, _, ok, err := d.TakePendingName(ctx, group, phone, time.Now().Add(-time.Hour))
	if err != nil || !ok || name != "Каталов Ахмед" {
		t.Fatalf("PN/LID split lost pending name: %q %v %v", name, ok, err)
	}
}

func TestSenderIdentityNeverTreatsBareLIDDigitsAsPhone(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	n := time.Now().UnixNano()
	digits := fmt.Sprintf("7928%07d", n%10000000)
	lid := digits + "@lid"
	actor, phone, err := d.ResolveSenderIdentity(ctx, lid)
	if err != nil || actor != lid || phone != "" {
		t.Fatalf("LID digits became a telephone: %q %q %v", actor, phone, err)
	}
	if _, ok := parsedSenderJID(types.NewJID(digits, types.GroupServer).String()); ok {
		t.Fatal("group was accepted as employee")
	}
}
