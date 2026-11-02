package bot

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/types"
	"whatsapp-bot/internal/parser"
)

const receiptCandidateOtherHold = "receipt_candidate_other"

// A model's "other" verdict cannot silently close an attachment submitted
// with a client name or bank-receipt text. These are candidates for review,
// not confirmed bank receipts or income.
func (b *Bot) holdContradictoryNonReceipt(ctx context.Context, chat types.JID, sender, waID string, rawID int, text, payer string, quiet bool) bool {
	body, caption, err := b.db.MediaReviewEvidence(ctx, rawID)
	if err != nil {
		fmt.Println("Проверка неоднозначного вложения:", err)
		return true // do not close an attachment when its evidence is unavailable
	}
	candidate := parser.LooksLikeBankReceipt(text) || parser.LooksLikeBankReceipt(body)
	for _, name := range []string{caption, payer, body} {
		if _, clean := cleanNameCaption(name); clean {
			candidate = true
		}
	}
	if !candidate {
		return false
	}
	changed, err := b.db.HoldMediaForReview(ctx, rawID, receiptCandidateOtherHold)
	if err != nil {
		fmt.Println("Не удалось сохранить вложение на проверку:", err)
		return true
	}
	if changed && !quiet && !b.groupSilent(chat) {
		b.sendReply(chat, "📎 Не смог уверенно распознать вложение с данными платежа. Оставил его на проверке; сумму не засчитываю. Если это чек, пришли чёткое фото целиком или отдельный PDF.", waID, sender)
	}
	return true
}
