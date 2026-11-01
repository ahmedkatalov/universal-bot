package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestOldUnreadMediaRemainsRecoverable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	id := fmt.Sprintf("old-media-%d", time.Now().UnixNano())
	group := id + "@g.us"
	raw, _, err := d.SaveRawMessage(ctx, id, group, "79280000991@s.whatsapp.net", "79280000991", "Мансур", "", true, "/tmp/old-pending.jpg", time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	items, err := d.DueMediaRetries(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range items {
		if i.RawID == raw {
			found = true
		}
	}
	if !found {
		t.Fatal("old unread media silently expired")
	}
	if _, err := d.HoldMediaForReview(ctx, raw, "pdf_multiple_pages"); err != nil {
		t.Fatal(err)
	}
	items, err = d.DueMediaRetries(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range items {
		if i.RawID == raw {
			t.Fatal("manual PDF review loop retried first page")
		}
	}
}
