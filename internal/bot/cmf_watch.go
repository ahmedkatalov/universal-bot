// Сверка чеков с программой рассрочек (cmf): каждый чек из рабочей группы
// становится "наблюдением". Бот определяет клиента (имя пишут в подписи к
// чеку или отдельным сообщением), ищет его в программе с учётом опечаток,
// при неоднозначности переспрашивает прямо в группе, а затем следит, внесли
// ли платёж в программу — и напоминает, если забыли.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/parser"
)

const settingUnmatchedBranch = "cmf_unmatched_branch"

// cmfRemindAfter — сколько ждать внесения платежа в программу, прежде чем
// напомнить в группу. Настраивается CMF_REMIND_HOURS (по умолчанию 24).
func cmfRemindAfter() time.Duration {
	if s := os.Getenv("CMF_REMIND_HOURS"); s != "" {
		if h, err := strconv.Atoi(s); err == nil && h > 0 {
			return time.Duration(h) * time.Hour
		}
	}
	return 24 * time.Hour
}

// cmfCapture заводит наблюдение за чеком из группы. caption — подпись к чеку
// (там обычно пишут, чей это платёж: "с карты Пияна, Ахмед оплатил рассрочку").
func (b *Bot) cmfCapture(ctx context.Context, chat types.JID, senderJID, caption, text string, rawID int, ts time.Time) {
	if b.cmf == nil {
		return
	}
	rd := parser.ParseReceipt(text)
	if rd.Amount == 0 {
		return // без суммы сверять нечего
	}
	txDate := ts
	if rd.HasTxTime {
		txDate = rd.TxTime
	}

	clientText := ""
	if strings.TrimSpace(caption) != "" {
		clientText = b.cmfExtractClientName(ctx, caption)
	}

	status := "noname"
	if clientText != "" {
		status = "lookup" // временный, сразу уточним ниже
	}
	watchID, err := b.db.InsertCmfWatch(ctx, rawID, chat.String(), senderJID, clientText, rd.Amount, txDate, status)
	if err != nil {
		fmt.Println("cmf: не удалось создать наблюдение:", err)
		return
	}
	if clientText != "" {
		b.cmfResolveWatch(ctx, watchID, chat, clientText, rd.Amount)
	}
}

// cmfAttachName привязывает имя клиента, присланное отдельным сообщением
// сразу после чека тем же отправителем ("Саралиева Милана").
func (b *Bot) cmfAttachName(ctx context.Context, chat types.JID, senderJID, text string) bool {
	if b.cmf == nil {
		return false
	}
	name := strings.TrimSpace(text)
	// Имя — короткая строка без команд и вопросов.
	if name == "" || len([]rune(name)) > 60 || strings.ContainsAny(name, "?!/") {
		return false
	}
	words := strings.Fields(name)
	if len(words) > 5 {
		return false
	}
	watchID, ok, err := b.db.LatestNonameWatch(ctx, chat.String(), senderJID, time.Now().Add(-3*time.Minute))
	if err != nil || !ok {
		return false
	}
	// Убираем возможную сумму из хвоста ("Касумова марям 25.000").
	var nameWords []string
	for _, w := range words {
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		nameWords = append(nameWords, w)
	}
	if len(nameWords) == 0 {
		return false
	}
	name = strings.Join(nameWords, " ")

	if err := b.db.UpdateCmfWatch(ctx, watchID, name, "", "", "", "lookup"); err != nil {
		fmt.Println("cmf: не удалось привязать имя:", err)
		return false
	}
	var amount float64
	if ws, err := b.db.ListCmfWatches(ctx, []string{"lookup"}, 50); err == nil {
		for _, w := range ws {
			if w.ID == watchID {
				amount = w.Amount
			}
		}
	}
	go b.cmfResolveWatch(context.Background(), watchID, chat, name, amount)
	return true
}

// cmfResolveWatch ищет клиента в программе по имени (ILIKE-подстрока; для
// опечаток — повторный поиск по отдельным словам). 0 совпадений -> unmatched,
// 1 -> watch, несколько -> вопрос в группу.
func (b *Bot) cmfResolveWatch(ctx context.Context, watchID int, chat types.JID, clientText string, amount float64) {
	clients, err := b.cmf.LookupClients(ctx, clientText)
	if err != nil {
		fmt.Println("cmf lookup:", err)
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", "", "noname")
		return
	}
	// Опечатки: полная строка не нашлась — ищем по каждому слову имени
	// и собираем пересечение кандидатов.
	if len(clients) == 0 {
		seen := map[string]cmf.ClientInfo{}
		for _, word := range strings.Fields(clientText) {
			if len([]rune(word)) < 3 {
				continue
			}
			if found, err := b.cmf.LookupClients(ctx, word); err == nil {
				for _, c := range found {
					seen[c.ID] = c
				}
			}
		}
		for _, c := range seen {
			clients = append(clients, c)
		}
	}

	switch len(clients) {
	case 0:
		branch, _ := b.db.SettingGet(ctx, settingUnmatchedBranch)
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", "", "unmatched")
		note := ""
		if branch != "" {
			note = " Отнесла к точке «" + branch + "» (как договаривались для чеков, которых нет в программе)."
		}
		b.sendText(chat, fmt.Sprintf("🔎 Клиента %q в программе не нашла (чек на %.0f ₽).%s", clientText, amount, note))
	case 1:
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", clients[0].ID, clients[0].FullName, "", "watch")
		fmt.Printf("cmf: чек на %.0f ₽ привязан к клиенту %s, ждём платёж в программе\n", amount, clients[0].FullName)
	default:
		names := make([]string, 0, len(clients))
		for _, c := range clients {
			names = append(names, c.FullName)
		}
		candJSON, _ := json.Marshal(clients)
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", string(candJSON), "ambiguous")
		b.sendText(chat, fmt.Sprintf(
			"🔎 По чеку на %.0f ₽ (%s) в программе нашлось несколько клиентов:\n- %s\nКому относится платёж? Ответьте на это сообщение полным именем.",
			amount, clientText, strings.Join(names, "\n- ")))
	}
}

// cmfWatcherLoop — фоновая сверка: раз в полчаса проверяет наблюдения старше
// CMF_REMIND_HOURS — внесён ли платёж в программу; если нет, напоминает в группу.
func (b *Bot) cmfWatcherLoop() {
	if b.cmf == nil {
		return
	}
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		b.cmfCheckDue(ctx)
		cancel()
	}
}

func (b *Bot) cmfCheckDue(ctx context.Context) {
	due, err := b.db.DueCmfWatches(ctx, time.Now().Add(-cmfRemindAfter()), 30)
	if err != nil {
		fmt.Println("cmf: ошибка выборки наблюдений:", err)
		return
	}
	for _, w := range due {
		found, err := b.cmf.HasPaymentAround(ctx, w.ClientID, w.Amount, w.TxDate, 5)
		if err != nil {
			fmt.Println("cmf: ошибка проверки платежа:", err)
			continue
		}
		if found {
			_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "found")
			fmt.Printf("cmf: платёж %s на %.0f ₽ найден в программе\n", w.ClientName, w.Amount)
			continue
		}
		_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "reminded")
		if jid, err := types.ParseJID(w.GroupJID); err == nil {
			b.sendText(jid, fmt.Sprintf(
				"⏰ Напоминание: чек от %s на %.0f ₽ (клиент %s) до сих пор НЕ добавлен в программу к рассрочке клиента. Не забудьте внести.",
				w.TxDate.Format("02.01.2006"), w.Amount, w.ClientName))
		}
	}
}

// cmfExtractClientName вытаскивает имя плательщика из подписи к чеку через ИИ.
func (b *Bot) cmfExtractClientName(ctx context.Context, caption string) string {
	if b.assistant == nil {
		return strings.TrimSpace(caption)
	}
	system := "Из подписи к банковскому чеку выдели имя КЛИЕНТА, который сделал платёж по своей рассрочке. " +
		"Внимание: в подписи могут упоминаться посторонние имена (чья карта использовалась, кто переслал) — " +
		"нужен именно плательщик рассрочки. Пример: 'с карты Пияна Ахмед сделал оплату своей рассрочки' -> Ахмед. " +
		"'Саралиева Милана' -> Саралиева Милана. " +
		"Имя приведи в именительный падеж (кто?): 'брат Догаева Магомеда скинул' -> Догаев Магомед. " +
		"Суммы и лишние слова не включай. Верни СТРОГО JSON {\"client\":\"имя или пусто\"}."
	out, err := b.assistant.Complete(ctx, system, caption)
	if err != nil {
		fmt.Println("cmf: извлечение имени из подписи не удалось:", err)
		return ""
	}
	var parsed struct {
		Client string `json:"client"`
	}
	if block := extractJSONBlock(out); block != "" {
		_ = json.Unmarshal([]byte(block), &parsed)
	}
	return strings.TrimSpace(parsed.Client)
}

// ---- Инструменты ассистента ----

// cmfStatusTool — состояние сверки с программой.
func (b *Bot) cmfStatusTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_status",
		Description: "Показывает состояние сверки чеков с программой рассрочек: какие чеки ждут внесения, " +
			"какие не внесены (напомнено), по каким непонятно, чей клиент. Вызывай при вопросах " +
			"'какие чеки не внесены в программу', 'что по сверке с программой', 'какие чеки без клиента'.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			ws, err := b.db.ListCmfWatches(ctx, []string{"watch", "reminded", "ambiguous", "unmatched", "noname"}, 30)
			if err != nil {
				return "", err
			}
			if len(ws) == 0 {
				return "Все чеки сверены с программой — открытых вопросов нет.", nil
			}
			branch, _ := b.db.SettingGet(ctx, settingUnmatchedBranch)
			var sb strings.Builder
			for _, w := range ws {
				label := w.ClientName
				if label == "" {
					label = w.ClientText
				}
				if label == "" {
					label = "(имя не указано)"
				}
				switch w.Status {
				case "watch":
					fmt.Fprintf(&sb, "- [#%d] ждём внесения: %s, %.0f ₽, чек от %s\n", w.ID, label, w.Amount, w.TxDate.Format("02.01"))
				case "reminded":
					fmt.Fprintf(&sb, "- [#%d] НЕ ВНЕСЁН (напомнено): %s, %.0f ₽, чек от %s\n", w.ID, label, w.Amount, w.TxDate.Format("02.01"))
				case "ambiguous":
					fmt.Fprintf(&sb, "- [#%d] неоднозначно, кандидаты: %s, %.0f ₽ — нужен ответ, чей чек\n", w.ID, w.Candidates, w.Amount)
				case "unmatched":
					extra := ""
					if branch != "" {
						extra = " (точка: " + branch + ")"
					}
					fmt.Fprintf(&sb, "- [#%d] клиента нет в программе: %s, %.0f ₽%s\n", w.ID, label, w.Amount, extra)
				case "noname":
					fmt.Fprintf(&sb, "- [#%d] чек без имени плательщика, %.0f ₽ от %s\n", w.ID, w.Amount, w.TxDate.Format("02.01 15:04"))
				}
			}
			return sb.String(), nil
		},
	}
}

// cmfResolveTool — вручную указать, чей чек (ответ на вопрос бота или команда).
func (b *Bot) cmfResolveTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_resolve",
		Description: "Указывает, какому клиенту программы относится чек из сверки. Вызывай, когда владелец отвечает " +
			"на вопрос 'кому относится платёж' или говорит 'чек #5 — это Ахмед Каталов Нажудович'. " +
			"watch_id — номер из cmf_status или из вопроса бота (если не указан — последний неоднозначный). " +
			"client_name — полное имя клиента как в программе.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"watch_id":    map[string]any{"type": "integer", "description": "Номер наблюдения (0 = последний неоднозначный)"},
				"client_name": map[string]any{"type": "string", "description": "Полное имя клиента в программе"},
			},
			"required": []string{"client_name"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				WatchID    int    `json:"watch_id"`
				ClientName string `json:"client_name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if b.cmf == nil {
				return "", fmt.Errorf("интеграция с программой не настроена (CMF_API_URL/CMF_EMAIL/CMF_PASSWORD)")
			}
			watchID := args.WatchID
			if watchID == 0 {
				ws, err := b.db.ListCmfWatches(ctx, []string{"ambiguous", "noname", "unmatched"}, 1)
				if err != nil || len(ws) == 0 {
					return "", fmt.Errorf("нет наблюдений, ожидающих уточнения — укажи watch_id из cmf_status")
				}
				watchID = ws[0].ID
			}
			clients, err := b.cmf.LookupClients(ctx, strings.TrimSpace(args.ClientName))
			if err != nil {
				return "", err
			}
			switch len(clients) {
			case 0:
				return fmt.Sprintf("Клиента %q в программе не нашла — проверь написание.", args.ClientName), nil
			case 1:
				if err := b.db.UpdateCmfWatch(ctx, watchID, "", clients[0].ID, clients[0].FullName, "", "watch"); err != nil {
					return "", err
				}
				return fmt.Sprintf("Чек #%d привязан к клиенту %s — слежу, чтобы платёж внесли в программу.", watchID, clients[0].FullName), nil
			default:
				var names []string
				for _, c := range clients {
					names = append(names, c.FullName)
				}
				return "Под это имя подходит несколько клиентов: " + strings.Join(names, "; ") + " — уточни полное имя.", nil
			}
		},
	}
}

// cmfBranchTool — "запомни: чеки, которых нет в программе, относятся к точке X".
func (b *Bot) cmfBranchTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_set_unmatched_branch",
		Description: "Запоминает, к какой точке (филиалу) относить чеки, клиентов которых нет в программе. " +
			"Вызывай при 'запомни: чеки которых нет в программе относятся к главной точке'. " +
			"Пустое название = показать текущую настройку.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"branch": map[string]any{"type": "string", "description": "Название точки (пусто = показать текущую)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Branch string `json:"branch"`
			}
			_ = json.Unmarshal(input, &args)
			if strings.TrimSpace(args.Branch) == "" {
				cur, err := b.db.SettingGet(ctx, settingUnmatchedBranch)
				if err != nil {
					return "", err
				}
				if cur == "" {
					return "Точка для чеков вне программы пока не задана.", nil
				}
				return "Чеки, которых нет в программе, относятся к точке «" + cur + "».", nil
			}
			if err := b.db.SettingSet(ctx, settingUnmatchedBranch, strings.TrimSpace(args.Branch)); err != nil {
				return "", err
			}
			return "Запомнила: чеки, которых нет в программе, относятся к точке «" + strings.TrimSpace(args.Branch) + "».", nil
		},
	}
}
