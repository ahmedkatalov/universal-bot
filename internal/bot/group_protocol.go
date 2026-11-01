package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	"whatsapp-bot/internal/db"
)

var errGroupProtocolReview = errors.New("изменение учёта требует проверки")

func (b *Bot) verifyDeletedDependencies(ctx context.Context, waID string) error {
	corrections, err := b.db.CorrectionsByMessage(ctx, waID)
	if err != nil {
		return err
	}
	labeled, err := b.db.CountLabeledBy(ctx, waID)
	if err != nil {
		return err
	}
	if corrections+labeled > 0 {
		return fmt.Errorf("%w: не сняты поправки/привязки удалённого сообщения", errGroupProtocolReview)
	}
	return nil
}

// Durable protocol events require a verified database outcome. Legacy handlers
// deliberately log errors for live UI, so a void return cannot acknowledge an
// inbox event on its own.
func (b *Bot) applyDurableRevoke(ctx context.Context, msg *events.Message, p *waProto.ProtocolMessage) error {
	waID := p.GetKey().GetID()
	if waID == "" {
		return fmt.Errorf("удаление без id исходного сообщения")
	}
	if group := p.GetKey().GetRemoteJID(); group != "" && group != msg.Info.Chat.String() {
		return fmt.Errorf("удаление из другой группы")
	}
	if copyID, ok, err := b.db.CopyBySentID(ctx, waID); err != nil {
		return err
	} else if ok {
		waID = copyID
	}
	before, err := b.db.RawMessageByWaID(ctx, waID)
	if err != nil {
		return err
	}
	if before.Found && before.GroupJID != msg.Info.Chat.String() {
		return fmt.Errorf("удаление затрагивает другую группу")
	}
	if before.Deleted {
		return b.verifyDeletedDependencies(ctx, waID)
	}
	if !before.Found {
		if !b.importRevokeQuiet(ctx, msg.Info.Chat, p, msg.Info.Timestamp) {
			return fmt.Errorf("не удалось сохранить удаление до получения оригинала")
		}
	} else {
		corrections, err := b.db.CorrectionsByMessage(ctx, waID)
		if err != nil {
			return err
		}
		labeled, err := b.db.CountLabeledBy(ctx, waID)
		if err != nil {
			return err
		}
		txs, receipts, err := b.db.MarkMessageDeleted(ctx, waID)
		if err != nil {
			return err
		}
		if err := b.verifyDeletedDependencies(ctx, waID); err != nil {
			return err
		}
		// A success notification is allowed only after cleanup is verified.
		if !b.groupSilent(msg.Info.Chat) {
			if txs+receipts > 0 {
				b.sendText(msg.Info.Chat, fmt.Sprintf("🗑 Сообщение удалено — убрал из учёта связанные записи (платежей: %d, чеков: %d). Отчёты уже без них.", txs, receipts))
			}
			if corrections > 0 {
				b.sendText(msg.Info.Chat, "🗑 Поправку суммы удалили — вернул прежнюю сумму.")
			}
			if labeled > 0 && txs+receipts == 0 {
				b.sendText(msg.Info.Chat, "🗑 Сообщение с ФИО удалили — чек снова без клиента, спрошу, чей он.")
			}
		}
		go b.ensureMissingWatches(context.Background())
	}
	after, err := b.db.RawMessageByWaID(ctx, waID)
	if err != nil {
		return err
	}
	if !after.Found || !after.Deleted {
		return fmt.Errorf("удаление не подтверждено в базе")
	}
	return b.verifyDeletedDependencies(ctx, waID)
}

func (b *Bot) applyCaptionEditVerified(ctx context.Context, msg *events.Message, p *waProto.ProtocolMessage, ref db.RawMessageRef) (bool, error) {
	caption := strings.TrimSpace(extractText(p.GetEditedMessage()))
	name := ""
	if n, ok := looksLikeName(caption); ok {
		name = n
	} else if n, ok := firstNameLine(caption); ok {
		name = n
	}
	if name == "" {
		return false, nil
	}
	canonical, ok := b.confirmClientName(ctx, name)
	if !ok {
		return false, nil
	}
	cid, err := b.db.GetOrCreateContact(ctx, canonical)
	if err != nil {
		return false, err
	}
	found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, p.GetKey().GetID(), canonical, &cid, ref.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("чек для правки подписи не найден")
	}
	b.reresolveWatchByCheck(ctx, msg.Info.Chat, p.GetKey().GetID(), canonical)
	if !b.groupSilent(msg.Info.Chat) {
		b.sendText(msg.Info.Chat, "✏️ Подпись к чеку изменили — чек теперь на клиента "+canonical+".")
	}
	return true, nil
}

func (b *Bot) applyDurableEdit(ctx context.Context, msg *events.Message, p *waProto.ProtocolMessage) (bool, error) {
	waID := p.GetKey().GetID()
	if waID == "" || p.GetEditedMessage() == nil {
		return false, fmt.Errorf("правка без исходного сообщения/содержимого")
	}
	if group := p.GetKey().GetRemoteJID(); group != "" && group != msg.Info.Chat.String() {
		return false, fmt.Errorf("правка из другой группы")
	}
	before, err := b.db.RawMessageByWaID(ctx, waID)
	if err != nil {
		return false, err
	}
	if !before.Found {
		return false, fmt.Errorf("исходное сообщение правки ещё не получено")
	}
	if before.GroupJID != msg.Info.Chat.String() {
		return false, fmt.Errorf("правка затрагивает другую группу")
	}
	if before.Deleted || before.HandledAs == "assistant" {
		return true, nil
	}
	if before.HasMedia {
		return b.applyCaptionEditVerified(ctx, msg, p, before)
	}
	newText := strings.TrimSpace(extractText(p.GetEditedMessage()))
	if stripped, addressed := b.stripBotName(newText); addressed {
		if normalized, ok := b.employeeAccountingText(ctx, msg, stripped); ok {
			newText = normalized
		} else if before.HandledAs == "answer" {
			newText = stripped
		} else {
			return true, nil
		}
	} else if b.isMentionedText(newText) {
		newText = b.stripBotMention(newText)
	}
	if newText == "" {
		return true, nil
	}
	if newText == strings.TrimSpace(before.Body) {
		state, err := b.db.GroupEventRawState(ctx, waID)
		return err == nil && (state.Parsed || state.Deleted || state.HandledAs != ""), err
	}
	beforeEdits, err := b.db.MessageEditCount(ctx, before.ID)
	if err != nil {
		return false, err
	}
	applied := b.applyEdit(ctx, msg, p, editQuiet{})
	after, err := b.db.RawMessageByWaID(ctx, waID)
	if err != nil {
		return false, err
	}
	afterEdits, err := b.db.MessageEditCount(ctx, before.ID)
	if err != nil {
		return false, err
	}
	if !applied && afterEdits == beforeEdits && after.Body == before.Body {
		return false, fmt.Errorf("правка не сохранена")
	}
	state, err := b.db.GroupEventRawState(ctx, waID)
	if err != nil {
		return false, err
	}
	// A changed source that has not settled may already have partial effects.
	// Keep it for review/raw recognition, never blindly apply the edit twice.
	return state.Parsed || state.Deleted || state.HandledAs != "", nil
}
