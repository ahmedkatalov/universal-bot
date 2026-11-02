package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type groupReadingQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// GroupReadingStatus describes the saved evidence, not the complete WhatsApp
// history or a reconciliation with the external accounting application.
type GroupReadingStatus struct {
	SavedMessages, DeletedMessages, SettledMessages, UnresolvedMessages int
	UnreadMedia, UnreadTexts, DisputedMessages                          int
	ReviewReceipts, UnknownReceiptAmounts, RecognitionPending           int
	HeldPayments                                                        int
	FirstSavedAt, LastSavedAt                                           time.Time
	Issues                                                              []GroupReadingIssue
	Truncated                                                           bool
}

type GroupReadingIssue struct {
	RawID                  int
	WaID, GroupJID, Sender string
	ReceivedAt             time.Time
	Reasons                []string
	UnknownAmounts         int
	KnownPendingAmount     float64 // evidence only; never add it to the collection total
	MediaAttempts          int
}

type groupReadingMessage struct {
	RawID                                       int
	WaID, GroupJID, Sender                      string
	ReceivedAt                                  time.Time
	Parsed, Deleted, HasMedia, Disputed         bool
	Handled, Body                               string
	MediaAvailable                              bool
	MediaAttempts                               int
	ReceiptRows, ReviewReceipts, UnknownAmounts int
	RecognitionPending, HeldPayments            int
	KnownPendingAmount                          float64
}

// GroupReading returns a snapshot over [from,to) by WhatsApp posting time.
// The issue limit affects only the samples; all counters cover the whole period.
func (d *DB) GroupReading(ctx context.Context, from, to time.Time, groupJIDs []string, limit int) (GroupReadingStatus, error) {
	return groupReading(ctx, d.pool, from, to, groupJIDs, limit)
}

// GroupReadingSnapshot keeps raw evidence and inbox events on the same MVCC
// snapshot. A worker may create raw rows between the two SELECTs; that event
// must appear in either raw coverage or the inbox, never disappear from both.
func (d *DB) GroupReadingSnapshot(ctx context.Context, from, to time.Time, groups []string, limit int) (GroupReadingStatus, GroupInboxCoverage, error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return GroupReadingStatus{}, GroupInboxCoverage{}, err
	}
	defer tx.Rollback(ctx)
	reading, err := groupReading(ctx, tx, from, to, groups, limit)
	if err != nil {
		return GroupReadingStatus{}, GroupInboxCoverage{}, err
	}
	inbox, err := groupInboxStatus(ctx, tx, from, to, groups, limit)
	if err != nil {
		return GroupReadingStatus{}, GroupInboxCoverage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GroupReadingStatus{}, GroupInboxCoverage{}, err
	}
	return reading, inbox, nil
}

func groupReading(ctx context.Context, q groupReadingQuerier, from, to time.Time, groupJIDs []string, limit int) (GroupReadingStatus, error) {
	if !to.After(from) {
		return GroupReadingStatus{}, fmt.Errorf("конец периода должен быть позже начала")
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
		WITH source AS MATERIALIZED (
			SELECT rm.id, COALESCE(rm.wa_message_id,'') AS wa_id, COALESCE(rm.wa_group_jid,'') AS group_jid,
			       COALESCE(NULLIF(po.name,''), NULLIF(rm.sender_name,''), split_part(COALESCE(NULLIF(rm.sender_phone,''),rm.sender_jid,''),'@',1)) AS sender,
			       rm.received_at, rm.parsed, COALESCE(rm.deleted,false) AS deleted, rm.has_media,
			       COALESCE(rm.ai_disputed,false) AS disputed, COALESCE(rm.handled_as,'') AS handled,
			       CASE WHEN btrim(COALESCE(rm.body,'')) <> '' THEN 'nonempty' ELSE '' END AS body,
			       (COALESCE(rm.media_path,'') <> '' OR rm.media_proto IS NOT NULL) AS media_available, rm.media_attempts
			FROM raw_messages rm
			LEFT JOIN phone_owners po ON po.phone = split_part(COALESCE(NULLIF(rm.sender_phone,''),rm.sender_jid,''),'@',1)
			WHERE COALESCE(rm.wa_group_jid,'') LIKE '%@g.us'
			  AND COALESCE(rm.sender_jid,'') <> 'bot-forward'
			  AND rm.received_at >= $1 AND rm.received_at < $2
			  AND ($3::text[] IS NULL OR rm.wa_group_jid = ANY($3))
		), br AS (
			SELECT raw_message_id, COUNT(*)::int AS total,
			       COUNT(*) FILTER (WHERE NOT ignored AND NOT is_duplicate AND
			         (needs_review OR NOT client_confirmed OR contact_id IS NULL OR COALESCE(amount,0) <= 0 OR recognition_status IN ('pending','failed')))::int AS review,
			       COUNT(*) FILTER (WHERE NOT ignored AND NOT is_duplicate AND COALESCE(amount,0) <= 0)::int AS unknown_amounts,
			       COUNT(*) FILTER (WHERE NOT ignored AND NOT is_duplicate AND recognition_status IN ('pending','failed'))::int AS pending_recognition,
			       COALESCE(SUM(amount) FILTER (WHERE NOT ignored AND NOT is_duplicate AND amount > 0 AND
			         (needs_review OR NOT client_confirmed OR contact_id IS NULL OR recognition_status IN ('pending','failed'))),0) AS known_pending
			FROM bank_receipts JOIN source ON source.id = raw_message_id
			GROUP BY raw_message_id
		), tx AS (
			SELECT raw_message_id, COUNT(*) FILTER (WHERE NOT ignored AND dup_pending)::int AS held,
			       COALESCE(SUM(amount) FILTER (WHERE NOT ignored AND dup_pending AND amount > 0),0) AS known_pending
			FROM transactions JOIN source ON source.id = raw_message_id
			GROUP BY raw_message_id
		)
		SELECT source.*, COALESCE(br.total,0), COALESCE(br.review,0), COALESCE(br.unknown_amounts,0), COALESCE(br.pending_recognition,0),
		       COALESCE(tx.held,0), (COALESCE(br.known_pending,0) + COALESCE(tx.known_pending,0))::float8
		FROM source
		LEFT JOIN br ON br.raw_message_id = source.id
		LEFT JOIN tx ON tx.raw_message_id = source.id
		ORDER BY source.received_at, source.id`, from, to, groupSliceArg(groupJIDs))
	if err != nil {
		return GroupReadingStatus{}, err
	}
	defer rows.Close()
	var status GroupReadingStatus
	for rows.Next() {
		var m groupReadingMessage
		if err := rows.Scan(&m.RawID, &m.WaID, &m.GroupJID, &m.Sender, &m.ReceivedAt, &m.Parsed, &m.Deleted, &m.HasMedia,
			&m.Disputed, &m.Handled, &m.Body, &m.MediaAvailable, &m.MediaAttempts, &m.ReceiptRows, &m.ReviewReceipts,
			&m.UnknownAmounts, &m.RecognitionPending, &m.HeldPayments, &m.KnownPendingAmount); err != nil {
			return GroupReadingStatus{}, err
		}
		status.add(m, limit)
	}
	if err := rows.Err(); err != nil {
		return GroupReadingStatus{}, err
	}
	return status, nil
}

func (s *GroupReadingStatus) add(m groupReadingMessage, limit int) {
	s.SavedMessages++
	if s.FirstSavedAt.IsZero() || m.ReceivedAt.Before(s.FirstSavedAt) {
		s.FirstSavedAt = m.ReceivedAt
	}
	if m.ReceivedAt.After(s.LastSavedAt) {
		s.LastSavedAt = m.ReceivedAt
	}
	if m.Deleted {
		s.DeletedMessages++
		return
	}
	issue := GroupReadingIssue{RawID: m.RawID, WaID: m.WaID, GroupJID: m.GroupJID, Sender: m.Sender, ReceivedAt: m.ReceivedAt,
		KnownPendingAmount: m.KnownPendingAmount, UnknownAmounts: m.UnknownAmounts, MediaAttempts: m.MediaAttempts}
	if m.HasMedia && !m.Parsed && m.ReceiptRows == 0 {
		s.UnreadMedia++
		reason := "вложение ещё не разобрано; неизвестно, является ли оно чеком"
		if !m.MediaAvailable {
			reason += "; файл и данные для повторного скачивания не сохранены"
		}
		issue.Reasons = append(issue.Reasons, reason)
		if m.Handled == "pdf_multiple_pages" {
			issue.Reasons = append(issue.Reasons, "многостраничный PDF: нужен отдельный файл для каждого чека")
		}
		if m.Handled == "pdf_pages_unknown" {
			issue.Reasons = append(issue.Reasons, "число страниц PDF не проверено: требуется отдельный читаемый чек")
		}
		if m.Handled == "receipt_candidate_other" {
			issue.Reasons = append(issue.Reasons, "подпись клиента или банковский текст противоречат распознаванию изображения; вложение на проверке, сумма не засчитана")
		}
	}
	if !m.HasMedia && !m.Parsed && m.Handled == "" && strings.TrimSpace(m.Body) != "" {
		s.UnreadTexts++
		issue.Reasons = append(issue.Reasons, "текст ещё не разобран полностью")
	}
	if m.Disputed {
		s.DisputedMessages++
		issue.Reasons = append(issue.Reasons, "разбор сообщения требует уточнения")
	}
	if m.ReviewReceipts > 0 {
		s.ReviewReceipts += m.ReviewReceipts
		issue.Reasons = append(issue.Reasons, fmt.Sprintf("чеков на проверке: %d", m.ReviewReceipts))
	}
	if m.UnknownAmounts > 0 {
		s.UnknownReceiptAmounts += m.UnknownAmounts
		issue.Reasons = append(issue.Reasons, fmt.Sprintf("сумма не прочитана у %d чеков; неизвестное не равно нулю", m.UnknownAmounts))
	}
	if m.RecognitionPending > 0 {
		s.RecognitionPending += m.RecognitionPending
		issue.Reasons = append(issue.Reasons, "распознавание чека не завершено или исчерпаны попытки")
	}
	if m.HeldPayments > 0 {
		s.HeldPayments += m.HeldPayments
		issue.Reasons = append(issue.Reasons, fmt.Sprintf("платежей, ожидающих решения о повторе: %d", m.HeldPayments))
	}
	if len(issue.Reasons) == 0 {
		s.SettledMessages++
		return
	}
	s.UnresolvedMessages++
	if len(s.Issues) < limit {
		s.Issues = append(s.Issues, issue)
	} else {
		s.Truncated = true
	}
}
