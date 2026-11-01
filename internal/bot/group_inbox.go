package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
)

const groupEventLease = 7 * time.Minute

func (b *Bot) captureGroupMessage(ctx context.Context, msg *events.Message) {
	if msg == nil || msg.Message == nil || msg.Info.IsFromMe || !msg.Info.IsGroup || !b.isAllowedGroup(msg.Info.Chat) {
		return
	}
	meta, err := json.Marshal(msg.Info)
	if err != nil {
		fmt.Println("Журнал группы: метаданные не сохранены:", err)
		return
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg.Message)
	if err != nil {
		fmt.Println("Журнал группы: событие не сохранено:", err)
		return
	}
	kind := "message"
	if p := msg.Message.GetProtocolMessage(); p != nil {
		switch p.GetType() {
		case waProto.ProtocolMessage_MESSAGE_EDIT:
			kind = "edit"
		case waProto.ProtocolMessage_REVOKE:
			kind = "revoke"
		}
	}
	// Delivery metadata can change (LID/phone/name), but the original event is
	// stable. Different edits of one message must each have their own identity.
	hash := sha256.Sum256(append([]byte(msg.Info.Chat.String()+"|"+msg.Info.ID+"|"+kind+"|"), payload...))
	command := b.ownerAssistantFastPath(msg)
	if kind == "message" && b.assistant != nil && b.isReportAdmin(msg.Info) && b.isReplyToBot(msg) {
		command = true
	}
	e := db.GroupInboxEvent{Key: hex.EncodeToString(hash[:]), WaID: msg.Info.ID, GroupJID: msg.Info.Chat.String(), Kind: kind, Command: command, Metadata: meta, Payload: payload, ReceivedAt: msg.Info.Timestamp}
	b.inboxCaptureMu.Lock()
	var ready []capturedSpoolEvent
	var retryCapture func()
	defer func() {
		b.inboxCaptureMu.Unlock()
		for _, r := range ready {
			b.dispatchCapturedGroupEvent(r.event, r.inserted)
		}
		if retryCapture != nil {
			b.enqueueChat(e.GroupJID, retryCapture)
		}
	}()
	if b.inboxSpoolDir != "" {
		if err := b.persistGroupSpool(e); err == nil {
			committed, err := b.flushGroupSpoolLocked(ctx)
			ready = append(ready, committed...)
			if err != nil {
				fmt.Println("Журнал группы: событие сохранено локально, PostgreSQL подхватит позже:", err)
			}
			return
		} else {
			fmt.Println("Журнал группы: локальная запись не удалась, пробую PostgreSQL:", err)
			// Do not let this event overtake older local events.
			committed, err := b.flushGroupSpoolLocked(ctx)
			ready = append(ready, committed...)
			if err != nil {
				fmt.Println("Журнал группы: новое событие НЕ сохранено; проверьте диск и PostgreSQL:", err)
				return
			}
		}
	}
	captureCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	saved, inserted, err := b.db.CaptureGroupEvent(captureCtx, e)
	cancel()
	if err != nil {
		// A failed write must not be marked as seen. Keep the event in the chat
		// queue and retry persistence before dispatching any financial action.
		fmt.Println("Журнал группы: база временно недоступна, повтор сохранения:", err)
		retryCapture = func() {
			for n := 0; n < 5; n++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				saved, inserted, err = b.db.CaptureGroupEvent(ctx, e)
				cancel()
				if err == nil {
					if inserted || saved.State == "pending" {
						b.runGroupEvent(saved.ID)
					}
					return
				}
				time.Sleep(time.Duration(n+1) * time.Second)
			}
			fmt.Println("Журнал группы: событие не удалось записать, требуется проверка подключения БД")
		}
		return
	}
	ready = append(ready, capturedSpoolEvent{saved, inserted})
}

func (b *Bot) runGroupEvent(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	e, claimed, err := b.db.ClaimGroupEvent(ctx, id, groupEventLease)
	if err != nil {
		fmt.Println("Журнал группы: не удалось взять событие:", err)
		return
	}
	if !claimed {
		return
	}
	state, note := "done", "dispatched"
	defer func() {
		if r := recover(); r != nil {
			state, note = "review", "interrupted_recognition"
			if e.Kind != "message" {
				note = "interrupted_" + e.Kind
			}
			fmt.Println("Журнал группы: прерван разбор:", r)
		}
		if e.Command && state != "done" {
			state, note = "review", "interrupted_command"
		}
		if state == "pending" && e.Attempts >= 8 {
			state, note = "review", "processing_failed"
		}
		finishCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := b.db.FinishGroupEvent(finishCtx, e, state, note); err != nil {
			fmt.Println("Журнал группы: не удалось завершить событие:", err)
		}
	}()
	var info types.MessageInfo
	var message waProto.Message
	if json.Unmarshal(e.Metadata, &info) != nil || proto.Unmarshal(e.Payload, &message) != nil {
		state, note = "review", "invalid_saved_event"
		return
	}
	if info.Chat.String() != e.GroupJID || info.ID != e.WaID || !info.IsGroup || info.IsFromMe || !b.isAllowedGroup(info.Chat) {
		state, note = "review", "group_not_available"
		return
	}
	msg := &events.Message{Info: info, Message: &message}
	if err := b.rememberMessageSender(ctx, info); err != nil {
		state, note = "pending", "sender_identity_unavailable"
		if errors.Is(err, db.ErrSenderIdentityConflict) {
			state, note = "review", "sender_identity_conflict"
		}
		return
	}
	if _, known := senderPhoneJID(info); !known && info.Sender.Server == types.HiddenUserServer {
		if actor, phone, err := b.db.ResolveSenderIdentity(ctx, info.Sender.String()); err == nil && phone != "" {
			if jid, err := types.ParseJID(actor); err == nil {
				msg.Info.SenderAlt = jid
			}
		}
	}
	if e.Kind == "edit" {
		complete, err := b.applyDurableEdit(ctx, msg, message.GetProtocolMessage())
		if err != nil {
			state, note = "pending", "edit_failed"
		} else if !complete {
			state, note = "review", "edit_not_confirmed"
		}
		return
	}
	if e.Kind == "revoke" {
		if err := b.applyDurableRevoke(ctx, msg, message.GetProtocolMessage()); err != nil {
			state, note = "pending", "revoke_failed"
			if errors.Is(err, errGroupProtocolReview) {
				state, note = "review", "revoke_cleanup_not_confirmed"
			}
		}
		return
	}
	before, err := b.db.GroupEventRawState(ctx, e.WaID)
	if err != nil {
		state, note = "pending", "database_unavailable"
		return
	}
	if before.Exists {
		if before.GroupJID != e.GroupJID {
			state, note = "review", "message_identity_conflict"
			return
		}
		if !before.Parsed && !before.Deleted && before.HandledAs == "" {
			// Recognition owns the durable raw-message retry. Replaying the full
			// message could repeat a correction, reply or partially written cash.
			state, note = "review", "interrupted_recognition"
		}
		return
	}
	b.processGroupMessage(ctx, msg)
	after, err := b.db.GroupEventRawState(ctx, e.WaID)
	if err != nil {
		state, note = "pending", "database_unavailable"
		return
	}
	if after.Exists {
		return
	}
	if unsupportedGroupContent(&message) {
		state, note = "review", "unsupported_content"
		return
	}
	if strings.TrimSpace(extractText(&message)) != "" {
		state, note = "pending", "raw_message_not_saved"
	}
}

func unsupportedGroupContent(m *waProto.Message) bool {
	return m.GetAudioMessage() != nil || m.GetVideoMessage() != nil || m.GetDocumentMessage() != nil ||
		m.GetImageMessage() != nil || m.GetContactMessage() != nil || m.GetContactsArrayMessage() != nil
}

func (b *Bot) recoverGroupInbox(ctx context.Context) {
	b.inboxCaptureMu.Lock()
	ready, err := b.flushGroupSpoolLocked(ctx)
	b.inboxCaptureMu.Unlock()
	for _, r := range ready {
		b.dispatchCapturedGroupEvent(r.event, r.inserted)
	}
	if err != nil {
		fmt.Println("Журнал группы: локальные события ждут восстановления:", err)
	}
	items, err := b.db.DueGroupEvents(ctx, 100)
	if err != nil {
		fmt.Println("Журнал группы: ошибка восстановления:", err)
		return
	}
	for _, e := range items {
		id := e.ID
		if e.Command {
			go b.runGroupEvent(id)
		} else {
			b.enqueueChat(e.GroupJID, func() { b.runGroupEvent(id) })
		}
	}
}

func (b *Bot) groupInboxLoop() {
	b.recoverGroupInbox(context.Background())
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		b.recoverGroupInbox(context.Background())
	}
}
