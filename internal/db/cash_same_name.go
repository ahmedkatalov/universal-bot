package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CashSamePair — наличка, которую признали повтором («тот же»), и та, что
// осталась в сборе (повтором которой её признали). *Other — сколько ещё живых
// записей (платежей и чеков) у клиента каждой, кроме этих двух: по ним видно,
// какое из двух имён — уже известный клиент.
type CashSamePair struct {
	KeptID      int
	KeptContact int
	KeptName    string
	KeptOther   int
	HeldContact int
	HeldName    string
	HeldOther   int
}

// CashSamePair — пара для налички heldTxID, признанной повтором. found=false —
// она не повтор или той налички в сборе уже нет.
func (d *DB) CashSamePair(ctx context.Context, heldTxID int) (CashSamePair, bool, error) {
	var p CashSamePair
	err := d.pool.QueryRow(ctx, `
		SELECT k.id, COALESCE(k.contact_id, 0), COALESCE(kc.canonical_name, k.raw_name, ''),
		       COALESCE(h.contact_id, 0), COALESCE(hc.canonical_name, h.raw_name, '')
		FROM transactions h
		JOIN transactions k ON k.id = h.dup_same_as
		LEFT JOIN contacts kc ON kc.id = k.contact_id
		LEFT JOIN contacts hc ON hc.id = h.contact_id
		WHERE h.id = $1 AND h.ignored = true AND k.ignored = false AND k.is_cash = true`, heldTxID).
		Scan(&p.KeptID, &p.KeptContact, &p.KeptName, &p.HeldContact, &p.HeldName)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if p.KeptOther, err = d.contactOtherRecords(ctx, p.KeptContact, p.KeptID, heldTxID); err != nil {
		return p, false, err
	}
	if p.HeldOther, err = d.contactOtherRecords(ctx, p.HeldContact, p.KeptID, heldTxID); err != nil {
		return p, false, err
	}
	return p, true, nil
}

// contactOtherRecords — живые платежи и чеки клиента, кроме платежей a и b.
func (d *DB) contactOtherRecords(ctx context.Context, contactID, a, b int) (int, error) {
	if contactID == 0 {
		return 0, nil
	}
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM transactions t
		        LEFT JOIN raw_messages rm ON rm.id = t.raw_message_id
		        WHERE t.contact_id = $1 AND t.id <> $2 AND t.id <> $3 AND t.ignored = false
		          AND COALESCE(rm.deleted, false) = false)
		     + (SELECT COUNT(*) FROM bank_receipts br
		        LEFT JOIN raw_messages rm ON rm.id = br.raw_message_id
		        WHERE br.contact_id = $1 AND br.ignored = false AND br.is_duplicate = false
		          AND COALESCE(rm.deleted, false) = false)`, contactID, a, b).Scan(&n)
	return n, err
}
