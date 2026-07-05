// Память номеров: телефон отправителя чека -> кто "забрал" деньги
// (ответственный). Владелец редактирует память прямо в чате словами.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/ai"
)

// extractPhone нормализует номер из произвольного текста: оставляет цифры,
// приводит российскую 8XXXXXXXXXX к 7XXXXXXXXXX (формат WhatsApp). Возвращает
// пустую строку, если это не похоже на телефон.
func extractPhone(s string) string {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if len(d) == 11 && strings.HasPrefix(d, "8") {
		d = "7" + d[1:]
	}
	if len(d) < 10 || len(d) > 15 {
		return ""
	}
	return d
}

// phoneMemoryTool — "открой память номеров", "добавь этот номер Расула",
// "этот номер теперь Майр-Эли", "убери номер".
func (b *Bot) phoneMemoryTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "phone_memory",
		Description: "Память номеров: какой телефон принадлежит кому (кто 'забрал' деньги, ответственный). " +
			"action=list — показать всю память ('открой память номеров'). " +
			"action=set — привязать/переназначить номер к человеку ('добавь этот номер Расула', " +
			"'этот номер теперь не Расула, а Майр-Эли') — сохраняется навсегда. " +
			"action=remove — убрать номер. " +
			"Номер бери из текста владельца; если он говорит 'этот номер' про чек в ответе (свайп) — используй " +
			"номер отправителя того чека (он будет в контексте). Имена в чеках/отчётах у таких номеров " +
			"показываются как имя из памяти.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"list", "set", "remove"}},
				"phone":  map[string]any{"type": "string", "description": "Телефон (любой формат; для set/remove)"},
				"name":   map[string]any{"type": "string", "description": "Имя ответственного (для set)"},
			},
			"required": []string{"action"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Action string `json:"action"`
				Phone  string `json:"phone"`
				Name   string `json:"name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			switch args.Action {
			case "list":
				owners, err := b.db.ListPhoneOwners(ctx)
				if err != nil {
					return "", err
				}
				if len(owners) == 0 {
					return "Память номеров пуста. Скажи «запомни номер ... это ...», и я запишу.", nil
				}
				var sb strings.Builder
				sb.WriteString("Память номеров:\n")
				for _, o := range owners {
					fmt.Fprintf(&sb, "- +%s — %s\n", o.Phone, o.Name)
				}
				return sb.String(), nil

			case "set":
				phone := extractPhone(args.Phone)
				if phone == "" {
					return "", fmt.Errorf("не понял номер телефона — укажи его цифрами")
				}
				if strings.TrimSpace(args.Name) == "" {
					return "", fmt.Errorf("нужно имя, кому принадлежит номер")
				}
				if err := b.db.SetPhoneOwner(ctx, phone, strings.TrimSpace(args.Name)); err != nil {
					return "", err
				}
				return fmt.Sprintf("Запомнил навсегда: +%s — это %s.", phone, strings.TrimSpace(args.Name)), nil

			case "remove":
				phone := extractPhone(args.Phone)
				if phone == "" {
					return "", fmt.Errorf("не понял номер телефона")
				}
				ok, err := b.db.RemovePhoneOwner(ctx, phone)
				if err != nil {
					return "", err
				}
				if !ok {
					return fmt.Sprintf("Номера +%s в памяти не было.", phone), nil
				}
				return fmt.Sprintf("Убрал номер +%s из памяти.", phone), nil

			default:
				return "", fmt.Errorf("неизвестное действие %q", args.Action)
			}
		},
	}
}

// quotedSenderPhoneNote — если владелец ответил (свайпом) на чужое сообщение,
// возвращает подсказку с номером его отправителя и id сообщения, чтобы
// "этот номер"/"этот чек" сработали по контексту. Пусто, если это не ответ.
func quotedSenderPhoneNote(msg *events.Message) string {
	ext := msg.Message.GetExtendedTextMessage()
	if ext == nil {
		return ""
	}
	ci := ext.GetContextInfo()
	if ci == nil {
		return ""
	}
	var parts []string
	if participant := ci.GetParticipant(); participant != "" {
		if jid, err := types.ParseJID(participant); err == nil && jid.User != "" {
			parts = append(parts, "номер отправителя +"+jid.User)
		}
	}
	if id := ci.GetStanzaID(); id != "" {
		parts = append(parts, "id сообщения "+id)
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n[Контекст ответа: " + strings.Join(parts, ", ") + "]"
}

// assignReceiptTool — "запиши этот чек на Майр-Эли": привязывает чек к
// ответственному, который забрал деньги.
func (b *Bot) assignReceiptTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "assign_receipt_collector",
		Description: "Привязывает чек к человеку, который ЗАБРАЛ деньги (ответственный), — 'запиши этот чек на " +
			"Майр-Эли', 'этот чек забрал Расул'. Передавай message_id ТОЛЬКО если в ТЕКУЩЕМ сообщении владельца " +
			"есть [Контекст ответа: ... id сообщения XXX]; не бери id из прошлых сообщений. Без id привязывается " +
			"последний чек в группе (можно уточнить сумму). Это влияет на отчёт по ответственным.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"collector":  map[string]any{"type": "string", "description": "Имя того, кто забрал деньги"},
				"message_id": map[string]any{"type": "string", "description": "id сообщения-чека из [Контекст ответа], если есть"},
				"amount":     map[string]any{"type": "number", "description": "Сумма чека, если надо уточнить (иначе 0)"},
			},
			"required": []string{"collector"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Collector string  `json:"collector"`
				MessageID string  `json:"message_id"`
				Amount    float64 `json:"amount"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			collector := strings.TrimSpace(args.Collector)
			if collector == "" {
				return "", fmt.Errorf("нужно имя, кто забрал деньги")
			}
			if strings.TrimSpace(args.MessageID) != "" {
				found, amount, recipient, err := b.db.SetReceiptCollectorByMessage(ctx, strings.TrimSpace(args.MessageID), collector)
				if err != nil {
					return "", err
				}
				if found {
					return fmt.Sprintf("Записал: чек %s на %.0f ₽ забрал %s.", recipient, amount, collector), nil
				}
			}
			found, amount, recipient, err := b.db.SetReceiptCollectorLatest(ctx, chat.String(), collector, args.Amount)
			if err != nil {
				return "", err
			}
			if !found {
				return "Подходящего чека не нашла — уточни сумму или ответь на нужный чек.", nil
			}
			return fmt.Sprintf("Записал: чек %s на %.0f ₽ забрал %s.", recipient, amount, collector), nil
		},
	}
}
