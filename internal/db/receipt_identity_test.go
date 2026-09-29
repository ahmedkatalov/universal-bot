package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestFindReceiptOccurrences — тот ЖЕ чек, отправленный в две группы, находится
// как два появления; при этом ДРУГОЙ платёж с той же суммой (но другим номером
// документа) НЕ считается тем же чеком.
func TestFindReceiptOccurrences(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	doc := "DOC-" + tag
	g1, g2 := "grpA-"+tag, "grpB-"+tag
	txd := time.Now().Add(-time.Hour).Truncate(time.Second)

	insert := func(grp, waid, docNum string, amount float64) {
		t.Helper()
		var rid int
		if err := d.pool.QueryRow(ctx, `
			INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_name, received_at)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			waid, grp, "s-"+tag+"@s.whatsapp.net", "Работник", txd).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO bank_receipts (raw_message_id, bank, recipient_raw, amount, doc_number, group_jid, tx_date)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			rid, "Т-Банк", "Клиент Тест", 21500, docNum, grp, txd); err != nil {
			t.Fatal(err)
		}
		_ = amount
	}

	// Один и тот же чек (один doc) в двух группах.
	insert(g1, "wa-"+tag+"-1", doc, 21500)
	insert(g2, "wa-"+tag+"-2", doc, 21500)
	// Контроль: ДРУГОЙ платёж, та же сумма, но другой номер документа.
	insert(g1, "wa-"+tag+"-3", "OTHER-"+tag, 21500)

	ident, err := d.ReceiptByWaMessageID(ctx, "wa-"+tag+"-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ident.Found || ident.DocNumber != doc {
		t.Fatalf("чек по сообщению не найден корректно: %+v", ident)
	}

	occ, err := d.FindReceiptOccurrences(ctx, ident)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 2 {
		t.Fatalf("ожидали 2 появления одного чека, получили %d: %+v", len(occ), occ)
	}
	seen := map[string]bool{}
	for _, o := range occ {
		seen[o.GroupJID] = true
		if o.Method != "номер документа" {
			t.Errorf("совпадение должно быть по номеру документа, а не %q", o.Method)
		}
	}
	if !seen[g1] || !seen[g2] {
		t.Errorf("ожидали группы %s и %s, получили %+v", g1, g2, occ)
	}
}
