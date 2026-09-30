package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestReceiptRetryLifecycle — жизненный цикл автоповтора распознавания:
// поставить в очередь -> выбрать по сроку -> +попытка -> заполнить -> снять с очереди.
func TestReceiptRetryLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	txd := time.Now().Add(-time.Hour).Truncate(time.Second)

	var rid, brid int
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_name, media_path, received_at, has_media)
		VALUES ($1,$2,$3,$4,$5,$6,true) RETURNING id`,
		"wa-"+tag, "grp-"+tag, "s-"+tag+"@s.whatsapp.net", "Работник", "/tmp/nope-"+tag+".jpg", txd).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO bank_receipts (raw_message_id, amount, needs_review, group_jid, tx_date)
		VALUES ($1,0,true,$2,$3) RETURNING id`, rid, "grp-"+tag, txd).Scan(&brid); err != nil {
		t.Fatal(err)
	}

	// Ставим на повтор — срок «сейчас».
	if err := d.MarkReceiptForRetry(ctx, rid, "vision unavailable", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	due, err := d.DueReceiptRetries(ctx, time.Now(), 6, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range due {
		if r.ID == brid {
			found = true
			if r.MediaPath == "" {
				t.Errorf("в очереди должен быть путь к файлу")
			}
		}
	}
	if !found {
		t.Fatalf("чек #%d должен попасть в очередь повтора", brid)
	}

	// Неудачная попытка: +1, всё ещё pending (лимит 6), но срок в будущем.
	if err := d.BumpReceiptRetry(ctx, brid, "still down", time.Now().Add(5*time.Minute), 6); err != nil {
		t.Fatal(err)
	}
	due2, _ := d.DueReceiptRetries(ctx, time.Now(), 6, 10)
	for _, r := range due2 {
		if r.ID == brid {
			t.Errorf("после Bump со сроком в будущем чек не должен быть due")
		}
	}

	// Успех: заполняем данными — снимается с очереди.
	if err := d.FillReceiptFromRetry(ctx, brid, RecognizedReceipt{
		Bank: "Т-Банк", Recipient: "Иванов Иван", Amount: 21500, DocNumber: "DOC1", TxDate: txd, HasTxDate: true,
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	var amount float64
	if err := d.pool.QueryRow(ctx, `SELECT recognition_status, amount::float8 FROM bank_receipts WHERE id=$1`, brid).Scan(&status, &amount); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || amount != 21500 {
		t.Errorf("после заполнения: status=%q amount=%.0f, ожидали ok/21500", status, amount)
	}
	// Больше не в очереди (даже с прошедшим сроком — статус уже ok).
	due3, _ := d.DueReceiptRetries(ctx, time.Now().Add(time.Hour), 6, 10)
	for _, r := range due3 {
		if r.ID == brid {
			t.Errorf("заполненный чек не должен оставаться в очереди")
		}
	}
}

// TestFillReceiptFromRetryKeepsExistingAmount — регрессионный: автоповтор НЕ
// перезаписывает уже известную сумму (ручную правку владельца).
func TestFillReceiptFromRetryKeepsExistingAmount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	txd := time.Now().Truncate(time.Second)
	var rid, brid int
	_ = d.pool.QueryRow(ctx, `INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, received_at) VALUES ($1,$2,$3,$4) RETURNING id`,
		"wa3-"+tag, "grp3-"+tag, "s3-"+tag+"@s.whatsapp.net", txd).Scan(&rid)
	// Владелец уже поправил сумму на 21500 и подтвердил клиента.
	_ = d.pool.QueryRow(ctx, `INSERT INTO bank_receipts (raw_message_id, amount, needs_review, client_confirmed, recipient_raw, tx_date, recognition_status, next_retry_at) VALUES ($1,21500,false,true,'Клиент',$2,'pending',$3) RETURNING id`,
		rid, txd, time.Now()).Scan(&brid)

	// Автоповтор «перечитал» сумму как 215000 — НЕ должен затирать 21500.
	if err := d.FillReceiptFromRetry(ctx, brid, RecognizedReceipt{Bank: "Т-Банк", Recipient: "Другой", Amount: 215000, TxDate: txd, HasTxDate: true}); err != nil {
		t.Fatal(err)
	}
	var amount float64
	var recipient string
	_ = d.pool.QueryRow(ctx, `SELECT amount::float8, recipient_raw FROM bank_receipts WHERE id=$1`, brid).Scan(&amount, &recipient)
	if amount != 21500 {
		t.Errorf("сумма затёрта автоповтором: %.0f (ожидали 21500)", amount)
	}
	if recipient != "Клиент" {
		t.Errorf("подтверждённый клиент затёрт: %q (ожидали Клиент)", recipient)
	}
}

// TestBumpReceiptRetryGivesUp — при исчерпании лимита статус становится 'failed'.
func TestBumpReceiptRetryGivesUp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	txd := time.Now().Truncate(time.Second)
	var rid, brid int
	_ = d.pool.QueryRow(ctx, `INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, received_at) VALUES ($1,$2,$3,$4) RETURNING id`,
		"wa2-"+tag, "grp2-"+tag, "s2-"+tag+"@s.whatsapp.net", txd).Scan(&rid)
	_ = d.pool.QueryRow(ctx, `INSERT INTO bank_receipts (raw_message_id, amount, needs_review, tx_date, recognition_status, recognition_attempts, next_retry_at) VALUES ($1,0,true,$2,'pending',0,$3) RETURNING id`,
		rid, txd, time.Now()).Scan(&brid)
	// Лимит 1 -> первая же неудача переводит в 'failed'.
	if err := d.BumpReceiptRetry(ctx, brid, "down", time.Now().Add(time.Minute), 1); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = d.pool.QueryRow(ctx, `SELECT recognition_status FROM bank_receipts WHERE id=$1`, brid).Scan(&status)
	if status != "failed" {
		t.Errorf("после исчерпания лимита статус=%q, ожидали failed", status)
	}
}
