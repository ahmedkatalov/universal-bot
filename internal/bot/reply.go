// Ответы-цитаты: бот "отмечает" конкретное сообщение (как свайп-ответ),
// когда пишет о нём — вопрос "чей это чек", предупреждение о дубле и т.п.
package bot

import (
	"context"
	"fmt"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// sendReply отправляет текст ЦИТАТОЙ (reply) на сообщение quotedID от
// quotedSender. Так в чате видно, к какому именно чеку относится ответ.
// Возвращает id отправленного сообщения. Если данных для цитаты нет —
// отправляет обычным текстом.
func (b *Bot) sendReply(chat types.JID, text, quotedID, quotedSender string) string {
	if quotedID == "" {
		return b.sendTextReturnID(chat, text)
	}
	ctxInfo := &waProto.ContextInfo{
		StanzaID:      proto.String(quotedID),
		QuotedMessage: &waProto.Message{Conversation: proto.String("")},
	}
	if quotedSender != "" {
		ctxInfo.Participant = proto.String(quotedSender)
	}
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: ctxInfo,
		},
	}
	resp, err := b.client.SendMessage(context.Background(), chat, msg)
	if err != nil {
		fmt.Println("Ошибка отправки цитаты:", err)
		return b.sendTextReturnID(chat, text)
	}
	b.rememberSent(chat, resp.ID)
	return resp.ID
}
