// Инструмент записи платежа со слов владельца: когда он диктует/пересылает
// платёж боту (в личку или обращаясь к нему), а не бот сам поймал его из
// группы. Без этого ассистент мог только «говорить», что записал, но реально
// ничего не сохранял — отсюда бесконечные переспросы и «пересчёт даёт то же».
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
)

const settingWorkingGroup = "working_group"

// recordPaymentTool записывает наличный/безналичный платёж, продиктованный
// владельцем, прямо в учёт (в рабочую группу).
func (b *Bot) recordPaymentTool() ai.Tool {
	return ai.Tool{
		Name: "record_payment",
		Description: "ЗАПИСАТЬ платёж в учёт со слов владельца — когда он диктует или пересылает платёж, а не бот сам " +
			"поймал его из группы. Вызывай на сообщения-платежи владельца: 'Джабраилов Мовсар оплатил наличными 22400', " +
			"'Асхабов Ибрагим наличка 35000 отдал Адаму', 'Умхадижиев Рахман 170т', 'запиши перевод Милана 25000'. " +
			"Если владелец прислал СРАЗУ НЕСКОЛЬКО платежей — вызови инструмент по КАЖДОМУ. client и amount обязательны. " +
			"kind: 'cash' если явно наличка/наличными/нал, иначе 'transfer'. collector — кто ЗАБРАЛ наличку (ответственный, " +
			"'отдал Адаму'/'Мансур взял'). group — в какую группу; если не указана, берётся ЗАПОМНЕННАЯ рабочая группа. " +
			"НЕ переспрашивай группу на каждый платёж — спроси один раз, запомни и применяй ко всем. date — YYYY-MM-DD, " +
			"по умолчанию сегодня.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client":    map[string]any{"type": "string", "description": "ФИО клиента (кто оплатил)"},
				"amount":    map[string]any{"type": "number", "description": "Сумма в рублях"},
				"kind":      map[string]any{"type": "string", "enum": []string{"cash", "transfer"}, "description": "cash — наличка; transfer — перевод/чек"},
				"group":     map[string]any{"type": "string", "description": "Группа учёта (пусто — рабочая группа из памяти)"},
				"collector": map[string]any{"type": "string", "description": "Кто забрал наличку (ответственный), если указан"},
				"date":      map[string]any{"type": "string", "description": "Дата платежа YYYY-MM-DD (по умолчанию сегодня)"},
			},
			"required": []string{"client", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Client    string  `json:"client"`
				Amount    float64 `json:"amount"`
				Kind      string  `json:"kind"`
				Group     string  `json:"group"`
				Collector string  `json:"collector"`
				Date      string  `json:"date"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			client := strings.TrimSpace(args.Client)
			if client == "" || args.Amount <= 0 {
				return "", fmt.Errorf("нужны и клиент, и сумма (>0)")
			}

			// Группа: указана -> используем и ЗАПОМИНАЕМ как рабочую; иначе берём
			// запомненную. Если нет ни там, ни там — просим назвать один раз.
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
				_ = b.db.SettingSet(ctx, settingWorkingGroup, groupJID)
			} else {
				groupJID, _ = b.db.SettingGet(ctx, settingWorkingGroup)
				if groupJID == "" {
					return "Не знаю, в какую группу записывать платежи. Назови рабочую группу один раз " +
						"(например: «работаем с Оплата КЛНТ»), дальше буду использовать её автоматически.", nil
				}
			}

			txDate := time.Now()
			if s := strings.TrimSpace(args.Date); s != "" {
				if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
					txDate = t
				}
			}

			isCash := strings.EqualFold(args.Kind, "cash")
			collector := strings.TrimSpace(args.Collector)
			senderName := collector
			if senderName == "" {
				senderName = "владелец"
			}

			// Синтетическое сырое сообщение, чтобы платёж лёг в нужную группу и
			// «кто собрал» засчитался ответственному (по sender_name).
			waMsgID := fmt.Sprintf("dictated-%d", time.Now().UnixNano())
			body := fmt.Sprintf("%s %.0f", client, args.Amount)
			if isCash {
				body += " наличка"
			}
			if collector != "" {
				body += " (собрал " + collector + ")"
			}
			rawID, err := b.db.SaveRawMessage(ctx, waMsgID, groupJID, "dictated@c.us", senderName, body, false, "", txDate)
			if err != nil {
				return "", fmt.Errorf("не удалось сохранить: %w", err)
			}

			canonical := client
			if resolved, ok := b.aliases.ResolveName(client); ok {
				canonical = resolved
			}
			contactID, err := b.db.GetOrCreateContact(ctx, canonical)
			if err != nil {
				return "", fmt.Errorf("не удалось создать контакт: %w", err)
			}
			cardTo := ""
			if isCash {
				cardTo = "наличные"
			}
			if err := b.db.InsertTransaction(ctx, db.TransactionInput{
				ContactID:    contactID,
				RawName:      canonical,
				Amount:       args.Amount,
				CardTo:       cardTo,
				IsCash:       isCash,
				Collector:    collector,
				RawMessageID: rawID,
				TxDate:       txDate,
			}); err != nil {
				return "", fmt.Errorf("не удалось записать платёж: %w", err)
			}

			kindLabel := "перевод"
			if isCash {
				kindLabel = "наличка"
			}
			groupName := groupJID
			if jid, err := types.ParseJID(groupJID); err == nil {
				if name, ok := b.joinedGroups(ctx)[jid]; ok {
					groupName = name
				}
			}
			out := fmt.Sprintf("Записал: %s — %.0f ₽ (%s), группа %s, %s.", canonical, args.Amount, kindLabel, groupName, txDate.Format("02.01.2006"))
			if collector != "" {
				out += " Собрал: " + collector + "."
			}
			return out, nil
		},
	}
}
