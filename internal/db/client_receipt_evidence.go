package db

import (
	"context"
	"strings"
	"time"
)

// ClientReceiptEvidence is a saved source mentioning the requested name. A body
// match proves only that the message exists, not who paid or whether it counts.
type ClientReceiptEvidence struct {
	RawID      int
	WaID       string
	GroupJID   string
	Sender     string
	PostedAt   time.Time
	HasMedia   bool
	Parsed     bool
	Deleted    bool
	HasReceipt bool
	Status     string // unread, review, parsed, excluded; never ledger membership
	MatchedBy  string // caption, body, client (confirmed receipt client only)
}

// FindClientReceiptEvidence audits saved sources in the posting period [from,to).
// It deliberately includes every reading tag, deleted messages and excluded
// receipts. Matching happens in Go: PostgreSQL's C locale must not hide Cyrillic
// names. No body is truncated and no row limit is applied before matching.
func (d *DB) FindClientReceiptEvidence(ctx context.Context, from, to time.Time, groups []string, client string) ([]ClientReceiptEvidence, error) {
	if strings.TrimSpace(client) == "" || contactKey(client) == "" {
		return nil, nil
	}
	if len(groups) == 0 {
		groups = []string{}
	}
	rows, err := d.pool.Query(ctx, `
		SELECT rm.id, COALESCE(rm.wa_message_id,''), rm.wa_group_jid,
		       COALESCE(NULLIF(rm.sender_name,''),NULLIF(rm.sender_phone,''),rm.sender_jid,''),
		       rm.received_at, rm.has_media, rm.parsed, rm.deleted,
		       COALESCE(rm.caption,''), COALESCE(rm.body,''),
		       receipts.has_receipt, COALESCE(receipts.ready,false), COALESCE(receipts.review,false),
		       COALESCE(receipts.excluded,false), COALESCE(receipts.clients,ARRAY[]::text[])
		FROM raw_messages rm
		LEFT JOIN LATERAL (
			SELECT COUNT(*)>0 AS has_receipt,
			       BOOL_OR(NOT br.ignored AND NOT br.is_duplicate AND br.client_confirmed AND NOT br.needs_review AND br.amount>0) AS ready,
			       BOOL_OR(NOT br.ignored AND NOT br.is_duplicate AND (NOT br.client_confirmed OR br.needs_review OR COALESCE(br.amount,0)<=0)) AS review,
			       BOOL_OR(br.ignored OR br.is_duplicate) AS excluded,
			       ARRAY_AGG(COALESCE(c.canonical_name,br.recipient_raw,'')) FILTER (WHERE br.client_confirmed) AS clients
			FROM bank_receipts br LEFT JOIN contacts c ON c.id=br.contact_id
			WHERE br.raw_message_id=rm.id
			  AND (cardinality($3::text[])=0 OR rm.wa_group_jid=ANY($3::text[]) OR br.group_jid=ANY($3::text[]))
		) receipts ON true
		WHERE rm.received_at >= $1 AND rm.received_at < $2
		  AND (cardinality($3::text[])=0 OR rm.wa_group_jid=ANY($3::text[]) OR receipts.has_receipt)
		ORDER BY rm.received_at,rm.id`, from, to, groups)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientReceiptEvidence
	for rows.Next() {
		var e ClientReceiptEvidence
		var caption, body string
		var ready, review, excluded bool
		var clients []string
		if err := rows.Scan(&e.RawID, &e.WaID, &e.GroupJID, &e.Sender, &e.PostedAt, &e.HasMedia, &e.Parsed, &e.Deleted, &caption, &body, &e.HasReceipt, &ready, &review, &excluded, &clients); err != nil {
			return nil, err
		}
		switch {
		case employeeClientMatches(client, caption):
			e.MatchedBy = "caption"
		case employeeClientMatches(client, body):
			e.MatchedBy = "body"
		default:
			for _, name := range clients {
				if employeeClientMatches(client, name) {
					e.MatchedBy = "client"
					break
				}
			}
		}
		if e.MatchedBy == "" {
			continue
		}
		switch {
		case e.Deleted:
			e.Status = "excluded"
		case ready:
			e.Status = "parsed"
		case review:
			e.Status = "review"
		case excluded:
			e.Status = "excluded"
		case e.Parsed:
			e.Status = "review"
		default:
			e.Status = "unread"
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
