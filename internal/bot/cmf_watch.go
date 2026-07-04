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
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
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

// looksLikeName проверяет, похожа ли строка на ФИО (короткая, без команд,
// вопросов и лишних цифр). Возвращает очищенное имя (без суммы в хвосте).
func looksLikeName(text string) (string, bool) {
	name := strings.TrimSpace(text)
	if name == "" || len([]rune(name)) > 60 || strings.ContainsAny(name, "?/\n") {
		return "", false
	}
	var nameWords []string
	for _, w := range strings.Fields(name) {
		// слова с цифрами (суммы "25.000", "20т") в имя не берём
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		nameWords = append(nameWords, w)
	}
	if len(nameWords) == 0 || len(nameWords) > 5 {
		return "", false
	}
	return strings.Join(nameWords, " "), true
}

// resolveReceiptPayer определяет ФИО клиента, написанное РЯДОМ с чеком:
// подпись к фото, текст-ответ (свайп на чек) или отдельное имя, присланное
// прямо перед чеком. Это и есть "чей чек" — важнее получателя на чеке.
func (b *Bot) resolveReceiptPayer(ctx context.Context, msg *events.Message, caption string) string {
	// 1. Подпись к чеку.
	if strings.TrimSpace(caption) != "" {
		if name := b.cmfExtractClientName(ctx, caption); name != "" {
			return name
		}
	}
	// 2. Чек — это ответ (свайп) на сообщение с именем.
	if quoted := extractQuotedText(msg); strings.TrimSpace(quoted) != "" {
		if name := b.cmfExtractClientName(ctx, quoted); name != "" {
			return name
		}
	}
	// 3. Имя, присланное прямо перед чеком тем же человеком.
	key := msg.Info.Chat.String() + "|" + msg.Info.Sender.String()
	b.pendingNameMu.Lock()
	pn, ok := b.pendingNames[key]
	if ok && time.Since(pn.at) < 4*time.Minute {
		delete(b.pendingNames, key)
		b.pendingNameMu.Unlock()
		return pn.name
	}
	b.pendingNameMu.Unlock()
	return ""
}

// rememberPendingName запоминает имя, присланное отдельным сообщением, —
// вдруг следом придёт чек ("Ахмед Каталов" -> фото чека).
func (b *Bot) rememberPendingName(msg *events.Message, text string) {
	name, ok := looksLikeName(text)
	if !ok {
		return
	}
	key := msg.Info.Chat.String() + "|" + msg.Info.Sender.String()
	b.pendingNameMu.Lock()
	b.pendingNames[key] = pendingName{name: name, at: time.Now()}
	b.pendingNameMu.Unlock()
}

// attachNameToReceipt привязывает ФИО, присланное ПОСЛЕ чека: либо ответом
// (свайп) на конкретный чек, либо отдельным сообщением сразу за чеком.
func (b *Bot) attachNameToReceipt(ctx context.Context, msg *events.Message, text string) bool {
	if b.cmf == nil {
		return false
	}
	name, ok := looksLikeName(text)
	if !ok {
		return false
	}
	chat := msg.Info.Chat
	watchID, wok, err := b.db.LatestNonameWatch(ctx, chat.String(), msg.Info.Sender.String(), time.Now().Add(-4*time.Minute))
	if err != nil || !wok {
		return false
	}
	if err := b.db.UpdateCmfWatch(ctx, watchID, name, "", "", "", "lookup"); err != nil {
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

// cmfWatchReceipt заводит наблюдение сверки за уже разобранным чеком (с учётом
// вижна) и сразу пытается сопоставить клиента. clientText — ФИО, написанное
// рядом с чеком; если пусто, берётся получатель с чека.
func (b *Bot) cmfWatchReceipt(ctx context.Context, chat types.JID, senderJID, clientText string, amount float64, txDate time.Time, rawID int) {
	if b.cmf == nil || amount == 0 {
		return
	}
	status := "noname"
	if clientText != "" {
		status = "lookup"
	}
	watchID, err := b.db.InsertCmfWatch(ctx, rawID, chat.String(), senderJID, clientText, amount, txDate, status)
	if err != nil {
		fmt.Println("cmf: не удалось создать наблюдение:", err)
		return
	}
	if clientText != "" {
		b.cmfResolveWatch(ctx, watchID, chat, clientText, amount)
	}
}

// extractQuotedText возвращает текст сообщения, на которое ответили (свайп).
func extractQuotedText(msg *events.Message) string {
	ext := msg.Message.GetExtendedTextMessage()
	if ext == nil {
		return ""
	}
	ci := ext.GetContextInfo()
	if ci == nil {
		return ""
	}
	q := ci.GetQuotedMessage()
	if q == nil {
		return ""
	}
	if c := q.GetConversation(); c != "" {
		return c
	}
	if e := q.GetExtendedTextMessage(); e != nil {
		return e.GetText()
	}
	if img := q.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	return ""
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

// cmfStatusTool — ЖИВАЯ сверка распознанных чеков из учёта с программой:
// какие внесены, какие нет, по каким клиент не найден. Работает по всему
// учёту (в т.ч. по чекам, сохранённым через "запомни"), а не по отдельной
// таблице наблюдений — поэтому реально отвечает "какие чеки не внесены".
func (b *Bot) cmfStatusTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_check_receipts",
		Description: "Живая сверка чеков с программой рассрочек: берёт распознанные чеки из учёта за период " +
			"(по умолчанию сегодня) и проверяет по каждому, внесён ли платёж в программу. Показывает: внесённые, " +
			"НЕ внесённые (их надо добавить), и чеки, клиента которых нет в программе. Вызывай ВСЕГДА при вопросах " +
			"'какие чеки не внесены', 'какие сегодняшние чеки добавлены', 'чеки каких клиентов внесены', " +
			"'проверь сверку'. Не отвечай про сверку по памяти — всегда вызывай этот инструмент.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD (пусто = сегодня)"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD (пусто = сегодня)"},
				"group":     map[string]any{"type": "string", "description": "Название группы (пусто = все)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
			}
			_ = json.Unmarshal(input, &args)
			now := time.Now()
			from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			to := from.AddDate(0, 0, 1)
			if args.FromDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local); err == nil {
					from = t
				}
			}
			if args.ToDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local); err == nil {
					to = t.AddDate(0, 0, 1)
				}
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}
			return b.cmfReconcile(ctx, from, to, groupJID)
		},
	}
}

// cmfReconcile — живая сверка чеков учёта за период с программой.
func (b *Bot) cmfReconcile(ctx context.Context, from, to time.Time, groupJID string) (string, error) {
	receipts, err := b.db.ReceiptsForPeriod(ctx, from, to, groupJID)
	if err != nil {
		return "", err
	}
	periodLabel := from.Format("02.01.2006")
	if to.AddDate(0, 0, -1).Format("2006-01-02") != from.Format("2006-01-02") {
		periodLabel += " — " + to.AddDate(0, 0, -1).Format("02.01.2006")
	}
	if len(receipts) == 0 {
		return "За " + periodLabel + " распознанных чеков в учёте нет.", nil
	}

	var added, missing, noClient []string
	for _, r := range receipts {
		clients, err := b.cmfLookupWithTypos(ctx, r.Name)
		if err != nil {
			noClient = append(noClient, fmt.Sprintf("%s — %.0f ₽ (ошибка поиска в программе)", r.Name, r.Amount))
			continue
		}
		switch len(clients) {
		case 0:
			noClient = append(noClient, fmt.Sprintf("%s — %.0f ₽ (клиента нет в программе)", r.Name, r.Amount))
		case 1:
			found, err := b.cmf.HasPaymentAround(ctx, clients[0].ID, r.Amount, r.TxDate, 5)
			if err != nil {
				noClient = append(noClient, fmt.Sprintf("%s — %.0f ₽ (ошибка проверки платежа)", clients[0].FullName, r.Amount))
				continue
			}
			line := fmt.Sprintf("%s — %.0f ₽ (чек от %s)", clients[0].FullName, r.Amount, r.TxDate.Format("02.01"))
			if found {
				added = append(added, line)
			} else {
				missing = append(missing, line)
			}
		default:
			var names []string
			for _, c := range clients {
				names = append(names, c.FullName)
			}
			noClient = append(noClient, fmt.Sprintf("%s — %.0f ₽ (несколько клиентов: %s)", r.Name, r.Amount, strings.Join(names, ", ")))
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Сверка с программой за %s (чеков: %d):\n\n", periodLabel, len(receipts))
	if len(missing) > 0 {
		fmt.Fprintf(&sb, "❌ НЕ внесены в программу (%d):\n- %s\n\n", len(missing), strings.Join(missing, "\n- "))
	}
	if len(noClient) > 0 {
		fmt.Fprintf(&sb, "⚠️ Требуют внимания (%d):\n- %s\n\n", len(noClient), strings.Join(noClient, "\n- "))
	}
	if len(added) > 0 {
		fmt.Fprintf(&sb, "✅ Уже внесены (%d):\n- %s\n", len(added), strings.Join(added, "\n- "))
	}
	if len(missing) == 0 && len(noClient) == 0 {
		sb.WriteString("Все чеки внесены в программу ✅")
	}
	return sb.String(), nil
}

// cmfLookupWithTypos ищет клиента с допуском на опечатки (полная строка,
// затем по отдельным словам имени).
func (b *Bot) cmfLookupWithTypos(ctx context.Context, name string) ([]cmf.ClientInfo, error) {
	clients, err := b.cmf.LookupClients(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(clients) > 0 {
		return clients, nil
	}
	seen := map[string]cmf.ClientInfo{}
	for _, word := range strings.Fields(name) {
		if len([]rune(word)) < 3 {
			continue
		}
		if found, err := b.cmf.LookupClients(ctx, word); err == nil {
			for _, c := range found {
				seen[c.ID] = c
			}
		}
	}
	var out []cmf.ClientInfo
	for _, c := range seen {
		out = append(out, c)
	}
	return out, nil
}

// cmfAddPaymentTool — внести платёж по чеку в программу рассрочек.
func (b *Bot) cmfAddPaymentTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_add_payment",
		Description: "Вносит платёж клиента в программу рассрочек (записывает оплату по договору). Вызывай, когда " +
			"владелец просит внести/добавить платёж в программу: 'внеси чек Миланы на 25000 в программу', " +
			"'добавь платёж Ахмеда Каталова 14000'. Находит клиента и его договор; если договоров несколько — " +
			"вернёт список, тогда переспроси, по какому вносить (укажи contract_id). ЭТО ЗАПИСЬ В ПРОГРАММУ — " +
			"вызывай только по явной просьбе внести, не по своей инициативе.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client_name": map[string]any{"type": "string", "description": "Имя клиента в программе"},
				"amount":      map[string]any{"type": "number", "description": "Сумма платежа в рублях"},
				"date":        map[string]any{"type": "string", "description": "Дата платежа YYYY-MM-DD (пусто = сегодня)"},
				"contract_id": map[string]any{"type": "string", "description": "ID договора, если у клиента их несколько (из предыдущего ответа инструмента)"},
			},
			"required": []string{"client_name", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				ClientName string  `json:"client_name"`
				Amount     float64 `json:"amount"`
				Date       string  `json:"date"`
				ContractID string  `json:"contract_id"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if b.cmf == nil {
				return "", fmt.Errorf("интеграция с программой не настроена")
			}
			if args.Amount <= 0 {
				return "", fmt.Errorf("нужна сумма платежа")
			}
			paidAt := time.Now()
			if args.Date != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.Date, time.Local); err == nil {
					paidAt = t
				}
			}

			// Определяем клиента и договор.
			var branchID, contractID string
			if strings.TrimSpace(args.ContractID) != "" {
				contractID = strings.TrimSpace(args.ContractID)
				// branch неизвестен по id — найдём среди договоров клиента ниже
			}
			clients, err := b.cmfLookupWithTypos(ctx, strings.TrimSpace(args.ClientName))
			if err != nil {
				return "", err
			}
			if len(clients) == 0 {
				return fmt.Sprintf("Клиента %q в программе не нашла.", args.ClientName), nil
			}
			if len(clients) > 1 && contractID == "" {
				var names []string
				for _, c := range clients {
					names = append(names, c.FullName)
				}
				return "Под это имя подходит несколько клиентов: " + strings.Join(names, "; ") + ". Уточни полное имя.", nil
			}
			contracts, err := b.cmf.ClientContracts(ctx, clients[0].ID)
			if err != nil {
				return "", err
			}
			if len(contracts) == 0 {
				return fmt.Sprintf("У клиента %s нет договоров в программе.", clients[0].FullName), nil
			}
			var chosen *cmf.ContractRef
			if contractID != "" {
				for i := range contracts {
					if contracts[i].ID == contractID {
						chosen = &contracts[i]
						break
					}
				}
				if chosen == nil {
					return "", fmt.Errorf("договор %s у клиента не найден", contractID)
				}
			} else if len(contracts) == 1 {
				chosen = &contracts[0]
			} else {
				var lines []string
				for _, c := range contracts {
					lines = append(lines, fmt.Sprintf("договор №%d (%s, остаток %d ₽) — contract_id=%s", c.Number, c.ProductName, c.Remaining, c.ID))
				}
				return fmt.Sprintf("У клиента %s несколько договоров — уточни, по какому вносить:\n- %s", clients[0].FullName, strings.Join(lines, "\n- ")), nil
			}
			branchID = chosen.BranchID

			if err := b.cmf.AddPayment(ctx, chosen.ID, branchID, int64(args.Amount+0.5), paidAt); err != nil {
				return "", err
			}
			return fmt.Sprintf("✅ Внёс платёж в программу: %s, %.0f ₽, договор №%d, дата %s.",
				clients[0].FullName, args.Amount, chosen.Number, paidAt.Format("02.01.2006")), nil
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
