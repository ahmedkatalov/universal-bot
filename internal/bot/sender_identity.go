package bot

import (
	"context"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"whatsapp-bot/internal/db"
)

func sameAccountJID(a, b types.JID) bool {
	return !a.IsEmpty() && !b.IsEmpty() && a.ToNonAD() == b.ToNonAD()
}

// A relationship comes only from the delivered SenderAlt pair or WhatsApp's
// own persisted LID store. No name matching or comparison of bare LID digits.
func (b *Bot) rememberMessageSender(ctx context.Context, info types.MessageInfo) error {
	alternate := info.SenderAlt
	if alternate.IsEmpty() && info.Sender.Server == types.HiddenUserServer && b.client != nil && b.client.Store != nil && b.client.Store.LIDs != nil {
		if pn, err := b.client.Store.LIDs.GetPNForLID(ctx, info.Sender.ToNonAD()); err == nil && pn.Server == types.DefaultUserServer {
			alternate = pn
		}
	}
	if b.db == nil || alternate.IsEmpty() {
		return nil
	}
	return b.db.RememberSenderIdentity(ctx, info.Sender.String(), alternate.String())
}

func (b *Bot) senderActorKey(ctx context.Context, info types.MessageInfo) string {
	return b.senderActorJID(ctx, info.Sender.String())
}

func (b *Bot) senderActorJID(ctx context.Context, sender string) string {
	if b.db != nil {
		if actor, _, err := b.db.ResolveSenderIdentity(ctx, sender); err == nil && actor != "" {
			return actor
		}
	}
	if jid, err := types.ParseJID(sender); err == nil {
		return jid.ToNonAD().String()
	}
	return sender
}

func (b *Bot) trustedQuoteSource(ctx context.Context, msg *events.Message) (db.RawMessageRef, bool, error) {
	var empty db.RawMessageRef
	ci := contextInfoOf(msg)
	if ci == nil || ci.GetStanzaID() == "" || b.db == nil {
		return empty, false, nil
	}
	source := msg.Info.Chat.String()
	if ci.GetRemoteJID() != "" {
		source = ci.GetRemoteJID()
	}
	if msg.Info.IsGroup && source != msg.Info.Chat.String() {
		return empty, false, fmt.Errorf("цитата относится к другой группе")
	}
	ref, err := b.db.RawMessageByWaID(ctx, ci.GetStanzaID())
	if err != nil {
		return empty, false, err
	}
	if ref.Found && ref.GroupJID != source {
		return empty, false, fmt.Errorf("источник цитаты не совпадает с группой")
	}
	return ref, ref.Found, nil
}
