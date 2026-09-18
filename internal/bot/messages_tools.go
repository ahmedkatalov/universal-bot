// Инструменты «память сообщений»: поднять, что писал конкретный номер/человек
// (в т.ч. удалённые в WhatsApp сообщения — бот сохраняет тело сразу при
// получении), и отправить личное сообщение человеку по команде владельца.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
)

// onlyDigits оставляет из строки только цифры (для «последних цифр номера»).
func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteString(string(r))
		}
	}
	return b.String()
}

// findMessagesTool — «что писал номер …92 / покажи сообщения от Расула».
// Показывает сохранённые сообщения, ВКЛЮЧАЯ удалённые в WhatsApp. Только владельцу.
func (b *Bot) findMessagesTool() ai.Tool {
	return ai.Tool{
		Name: "find_messages",
		Description: "Показывает, ЧТО присылал конкретный номер или человек в группах — ДАЖЕ если сообщение потом " +
			"УДАЛИЛИ в WhatsApp (бот сохраняет каждое сообщение сразу при получении и не стирает при удалении). " +
			"Вызывай при 'что тебе написал номер …92', 'покажи сообщения от этого номера', 'что писал Расул вчера', " +
			"'какие сообщения удалил такой-то'. Можно задать последние цифры номера (number), имя (name), период и группу.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"number":    map[string]any{"type": "string", "description": "Номер или его ПОСЛЕДНИЕ цифры (например «9292»)"},
				"name":      map[string]any{"type": "string", "description": "Имя отправителя (из памяти номеров или как он подписан)"},
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD (пусто = без ограничения)"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD включительно (пусто = до сегодня)"},
				"group":     map[string]any{"type": "string", "description": "Название группы (пусто = все группы)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Number   string `json:"number"`
				Name     string `json:"name"`
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
			}
			_ = json.Unmarshal(input, &args)
			digits := onlyDigits(args.Number)
			name := strings.TrimSpace(args.Name)
			if digits == "" && name == "" {
				return "Укажи номер (хотя бы последние цифры) или имя — по кому поднять сообщения.", nil
			}

			var fromPtr, toPtr *time.Time
			if s := strings.TrimSpace(args.FromDate); s != "" {
				t, err := time.ParseInLocation("2006-01-02", s, time.Local)
				if err != nil {
					return "", fmt.Errorf("неверная дата %q, нужен формат ГГГГ-ММ-ДД", s)
				}
				fromPtr = &t
			}
			if s := strings.TrimSpace(args.ToDate); s != "" {
				t, err := time.ParseInLocation("2006-01-02", s, time.Local)
				if err != nil {
					return "", fmt.Errorf("неверная дата %q, нужен формат ГГГГ-ММ-ДД", s)
				}
				next := t.AddDate(0, 0, 1)
				toPtr = &next
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}

			msgs, err := b.db.MessagesFrom(ctx, digits, name, fromPtr, toPtr, groupJID, 50)
			if err != nil {
				return "", fmt.Errorf("ошибка выборки сообщений: %w", err)
			}
			who := name
			if who == "" {
				who = "номера …" + lastDigits(digits, 4)
			}
			if len(msgs) == 0 {
				return fmt.Sprintf("Сохранённых сообщений от %s не нашёл. Возможно, этот номер ничего не писал в группах, где я состою, "+
					"или писал, когда меня не было онлайн (что я не получил — того у меня нет).", who), nil
			}
			groups := b.joinedGroups(ctx)
			var sb strings.Builder
			fmt.Fprintf(&sb, "Сообщения от %s (найдено %d, свежие сверху):\n\n", who, len(msgs))
			for _, m := range msgs {
				gname := m.GroupJID
				if jid, err := types.ParseJID(m.GroupJID); err == nil {
					if n, ok := groups[jid]; ok && n != "" {
						gname = n
					}
				}
				label := m.Who
				if label == "" {
					label = "+" + m.Phone
				}
				body := strings.TrimSpace(m.Body)
				if body == "" {
					if m.HasMedia {
						body = "[фото/медиа без текста]"
					} else {
						body = "[пустое сообщение]"
					}
				} else if m.HasMedia {
					body = "[медиа] " + body
				}
				tags := ""
				if m.Deleted {
					tags = " 🗑удалено в WhatsApp"
				}
				fmt.Fprintf(&sb, "• %s — %s (+%s) в «%s»%s:\n  %s\n",
					m.ReceivedAt.Format("02.01 15:04"), label, m.Phone, gname, tags, body)
			}
			return sb.String(), nil
		},
	}
}

// lastDigits возвращает последние n цифр строки (для короткой подписи номера).
func lastDigits(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// sendToPersonTool — отправить продиктованный владельцем текст ЛИЧНО человеку
// (в личку), а не в группу. Только владельцу и только по явной просьбе.
func (b *Bot) sendToPersonTool() ai.Tool {
	return ai.Tool{
		Name: "send_to_person",
		Description: "Отправляет ПРОИЗВОЛЬНЫЙ текст, продиктованный владельцем, ЛИЧНО человеку (в личный чат), НЕ в группу. " +
			"Вызывай при 'напиши номеру … в личку …', 'отправь Расулу лично …', 'напиши этому номеру …'. " +
			"Укажи number (полный номер с кодом страны ИЛИ последние цифры) ИЛИ name (имя), и text — РОВНО тот текст без " +
			"своих добавлений. Если по частичному номеру/имени подходит несколько людей — инструмент вернёт список, тогда " +
			"уточни. Отправляй только то, что владелец явно просил передать.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"number": map[string]any{"type": "string", "description": "Номер получателя: полный (с кодом) или последние цифры"},
				"name":   map[string]any{"type": "string", "description": "Имя получателя (если номер не назвали)"},
				"text":   map[string]any{"type": "string", "description": "Текст сообщения ТОЧНО как его надо отправить"},
			},
			"required": []string{"text"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Number string `json:"number"`
				Name   string `json:"name"`
				Text   string `json:"text"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			text := strings.TrimSpace(args.Text)
			if text == "" {
				return "", fmt.Errorf("нечего отправлять — пустой текст")
			}
			digits := onlyDigits(args.Number)
			name := strings.TrimSpace(args.Name)

			phone := ""
			full := extractPhone(args.Number)
			if full != "" && len(full) >= 11 {
				// Полный МЕЖДУНАРОДНЫЙ номер (с кодом страны, напр. +7…) — напрямую.
				// Требуем ≥11 цифр: голый 10-значный номер без кода ушёл бы не туда
				// (9281234592@s.whatsapp.net вместо 79281234592@…), поэтому его
				// отправляем ниже через поиск проверенного полного номера.
				phone = full
			} else if digits != "" || name != "" {
				// Частичный номер (без кода) или имя — ищем ВЕРИФИЦИРОВАННЫЙ полный
				// номер среди тех, кто реально писал (по последним цифрам «9281234592»
				// найдётся «79281234592»). Так не отправим на неполный/неверный JID.
				cands, err := b.db.SenderCandidates(ctx, digits, name, 10)
				if err != nil {
					return "", fmt.Errorf("ошибка поиска получателя: %w", err)
				}
				switch len(cands) {
				case 0:
					return "Не нашёл проверенного номера по этим данным. Дай ПОЛНЫЙ номер получателя с кодом страны (например +7928…) — по нескольким цифрам новый номер я не восстановлю.", nil
				case 1:
					phone = cands[0].Phone
				default:
					var lines []string
					for _, c := range cands {
						label := c.Name
						if label == "" {
							label = "без имени"
						}
						lines = append(lines, fmt.Sprintf("+%s — %s", c.Phone, label))
					}
					return "Под это подходит несколько человек — уточни, кому именно (назови полный номер):\n- " + strings.Join(lines, "\n- "), nil
				}
			} else {
				return "Кому отправить? Назови номер (полный или последние цифры) или имя.", nil
			}

			jid := types.NewJID(phone, types.DefaultUserServer)
			if id := b.sendTextReturnID(jid, text); id == "" {
				return "", fmt.Errorf("не удалось отправить личное сообщение на +%s — попробуй ещё раз", phone)
			}
			return fmt.Sprintf("Отправил лично +%s:\n%s", phone, text), nil
		},
	}
}
