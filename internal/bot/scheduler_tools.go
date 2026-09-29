package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
)

// parseHM разбирает "ЧЧ:ММ" / "ЧЧ.ММ" / "ЧЧ". Возвращает часы, минуты, ok.
func parseHM(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(".", ":", " ", ":", "-", ":").Replace(s)
	parts := strings.SplitN(s, ":", 2)
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	m := 0
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		m, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || m < 0 || m > 59 {
			return 0, 0, false
		}
	}
	return h, m, true
}

// parseWeekday принимает 0-6 (0=вс) или русское название дня.
func parseWeekday(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 6 {
		return n, true
	}
	names := map[string]int{
		"вс": 0, "воскресенье": 0, "вск": 0,
		"пн": 1, "понедельник": 1, "пон": 1,
		"вт": 2, "вторник": 2,
		"ср": 3, "среда": 3, "среду": 3,
		"чт": 4, "четверг": 4,
		"пт": 5, "пятница": 5, "пятницу": 5,
		"сб": 6, "суббота": 6, "субботу": 6,
	}
	for k, v := range names {
		if strings.HasPrefix(s, k) {
			return v, true
		}
	}
	return 0, false
}

var weekdayDative = [7]string{"воскресеньям", "понедельникам", "вторникам", "средам", "четвергам", "пятницам", "субботам"}

// describeSchedule — человеческое описание расписания для подтверждений/списка.
func describeSchedule(kind string, next time.Time, hour, minute, weekday int) string {
	switch kind {
	case "daily":
		return fmt.Sprintf("каждый день в %02d:%02d", hour, minute)
	case "weekly":
		wd := "по расписанию"
		if weekday >= 0 && weekday <= 6 {
			wd = "по " + weekdayDative[weekday]
		}
		return fmt.Sprintf("%s в %02d:%02d", wd, hour, minute)
	default: // once
		return "один раз " + next.Format("02.01 в 15:04")
	}
}

// scheduleReminderTool — создать напоминание/сообщение по расписанию. Владельцу.
func (b *Bot) scheduleReminderTool(chat types.JID, ownerJID types.JID) ai.Tool {
	return ai.Tool{
		Name: "schedule_reminder",
		Description: "Создаёт напоминание или сообщение по расписанию (переживает перезапуск бота). Вызывай, когда " +
			"владелец просит «напоминай», «каждый день в 10 отправляй мне …», «через час напомни …», «в пятницу в 9 …», " +
			"«отправь Мухаммаду в 18:00 …». kind: once (разово), daily (каждый день), weekly (раз в неделю). Время в " +
			"формате ЧЧ:ММ (time) или через in_minutes (через сколько минут, для разового). Для weekly укажи weekday " +
			"(пн/вт/…/вс). Кому: target = me (владельцу лично), here (в этот чат), group (укажи group=название), " +
			"person (укажи number или name). message — точный текст, который отправлять.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":       map[string]any{"type": "string", "enum": []string{"once", "daily", "weekly"}, "description": "once/daily/weekly"},
				"time":       map[string]any{"type": "string", "description": "Время ЧЧ:ММ (для daily/weekly и разового с датой)"},
				"in_minutes": map[string]any{"type": "integer", "description": "Через сколько минут (разовое; заменяет time/date)"},
				"date":       map[string]any{"type": "string", "description": "Дата ГГГГ-ММ-ДД для разового (по умолчанию сегодня/завтра)"},
				"weekday":    map[string]any{"type": "string", "description": "День недели для weekly: пн/вт/ср/чт/пт/сб/вс или 0-6"},
				"target":     map[string]any{"type": "string", "enum": []string{"me", "here", "group", "person"}, "description": "Кому отправлять"},
				"group":      map[string]any{"type": "string", "description": "Название группы (если target=group)"},
				"number":     map[string]any{"type": "string", "description": "Номер человека (если target=person)"},
				"name":       map[string]any{"type": "string", "description": "Имя человека (если target=person)"},
				"message":    map[string]any{"type": "string", "description": "Текст, который отправить"},
			},
			"required": []string{"kind", "message"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var a struct {
				Kind      string `json:"kind"`
				Time      string `json:"time"`
				InMinutes int    `json:"in_minutes"`
				Date      string `json:"date"`
				Weekday   string `json:"weekday"`
				Target    string `json:"target"`
				Group     string `json:"group"`
				Number    string `json:"number"`
				Name      string `json:"name"`
				Message   string `json:"message"`
			}
			if err := json.Unmarshal(input, &a); err != nil {
				return "", err
			}
			msg := strings.TrimSpace(a.Message)
			if msg == "" {
				return "Что именно отправлять по расписанию? Укажи текст.", nil
			}

			// Получатель -> JID + подпись.
			targetKind := strings.TrimSpace(a.Target)
			if targetKind == "" {
				targetKind = "me"
			}
			var targetJID, label string
			switch targetKind {
			case "me":
				targetJID, label = ownerJID.String(), "вам лично"
			case "here":
				targetJID, label = chat.String(), "в этот чат"
			case "group":
				if strings.TrimSpace(a.Group) == "" {
					return "В какую группу отправлять по расписанию? Назови группу.", nil
				}
				jid, name, err := b.resolveTargetGroup(ctx, a.Group)
				if err != nil {
					return "", err
				}
				targetJID, label = jid.String(), "в группу «"+name+"»"
			case "person":
				phone, ask, err := b.resolvePersonPhone(ctx, a.Number, a.Name)
				if err != nil {
					return "", err
				}
				if ask != "" {
					return ask, nil
				}
				targetJID, label = types.NewJID(phone, types.DefaultUserServer).String(), "лично +"+phone
			default:
				targetJID, label = ownerJID.String(), "вам лично"
			}

			// Расписание.
			kind := strings.TrimSpace(a.Kind)
			var hour, minute, weekday int = 0, 0, -1
			var onceAt time.Time
			now := time.Now()
			if a.InMinutes > 0 {
				kind = "once"
				onceAt = now.Add(time.Duration(a.InMinutes) * time.Minute)
			} else {
				h, m, ok := parseHM(a.Time)
				if !ok {
					return "Во сколько отправлять? Укажи время в формате ЧЧ:ММ (например 10:00) или «через N минут».", nil
				}
				hour, minute = h, m
				switch kind {
				case "weekly":
					wd, ok := parseWeekday(a.Weekday)
					if !ok {
						return "В какой день недели? Укажи пн/вт/ср/чт/пт/сб/вс.", nil
					}
					weekday = wd
				case "once":
					day := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.Local)
					if s := strings.TrimSpace(a.Date); s != "" {
						d, err := time.ParseInLocation("2006-01-02", s, time.Local)
						if err != nil {
							return "Не понял дату, нужен формат ГГГГ-ММ-ДД.", nil
						}
						day = time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, time.Local)
					}
					if !day.After(now) {
						day = day.AddDate(0, 0, 1) // время сегодня уже прошло -> завтра
					}
					onceAt = day
				}
			}

			job, err := b.db.CreateScheduledJob(ctx, ownerJID.User, kind, targetKind, targetJID, label, msg, hour, minute, weekday, onceAt)
			if err != nil {
				return "", fmt.Errorf("не получилось создать напоминание: %w", err)
			}
			return fmt.Sprintf("Готово, напоминание #%d: %s, %s — «%s». Первый раз: %s.",
				job.ID, describeSchedule(kind, job.NextRunAt, hour, minute, weekday), label, msg, job.NextRunAt.Format("02.01 15:04")), nil
		},
	}
}

// resolvePersonPhone определяет полный номер получателя (личка). ask != "" —
// нужно уточнение (несколько кандидатов/не найден), его и вернуть пользователю.
func (b *Bot) resolvePersonPhone(ctx context.Context, number, name string) (phone, ask string, err error) {
	if full := extractPhone(number); full != "" && len(full) >= 11 {
		return full, "", nil
	}
	digits := onlyDigits(number)
	name = strings.TrimSpace(name)
	if digits == "" && name == "" {
		return "", "Кому отправлять? Назови номер (полный или последние цифры) или имя.", nil
	}
	cands, err := b.db.SenderCandidates(ctx, digits, name, 10)
	if err != nil {
		return "", "", fmt.Errorf("поиск получателя: %w", err)
	}
	switch len(cands) {
	case 0:
		return "", "Не нашёл, кому писать. Дай полный номер получателя (с кодом страны).", nil
	case 1:
		return cands[0].Phone, "", nil
	default:
		var lines []string
		for _, c := range cands {
			l := c.Name
			if l == "" {
				l = "без имени"
			}
			lines = append(lines, fmt.Sprintf("+%s — %s", c.Phone, l))
		}
		return "", "Под это подходит несколько человек — уточни, кому (полный номер):\n- " + strings.Join(lines, "\n- "), nil
	}
}

// listRemindersTool — показать активные напоминания. Владельцу.
func (b *Bot) listRemindersTool() ai.Tool {
	return ai.Tool{
		Name:        "list_reminders",
		Description: "Показывает активные напоминания/задания по расписанию. Вызывай на «какие у меня напоминания», «покажи напоминания», «что запланировано».",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			jobs, err := b.db.ListScheduledJobs(ctx, 50)
			if err != nil {
				return "", err
			}
			if len(jobs) == 0 {
				return "Активных напоминаний нет.", nil
			}
			var sb strings.Builder
			sb.WriteString("Активные напоминания:\n")
			for _, j := range jobs {
				fmt.Fprintf(&sb, "#%d — %s, %s: «%s» (ближайшее %s)\n",
					j.ID, describeSchedule(j.Kind, j.NextRunAt, j.AtHour, j.AtMinute, j.Weekday),
					j.TargetLabel, j.Message, j.NextRunAt.Format("02.01 15:04"))
			}
			return sb.String(), nil
		},
	}
}

// cancelReminderTool — отменить напоминание по номеру. Владельцу.
func (b *Bot) cancelReminderTool() ai.Tool {
	return ai.Tool{
		Name:        "cancel_reminder",
		Description: "Отменяет напоминание по его номеру (#id из list_reminders). Вызывай на «отмени напоминание 3», «убери напоминание про …».",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "integer", "description": "Номер напоминания (#id)"}},
			"required":   []string{"id"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var a struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(input, &a); err != nil {
				return "", err
			}
			if a.ID <= 0 {
				return "Укажи номер напоминания (#id из списка).", nil
			}
			label, found, err := b.db.CancelScheduledJob(ctx, a.ID)
			if err != nil {
				return "", err
			}
			if !found {
				return fmt.Sprintf("Напоминание #%d не найдено или уже неактивно.", a.ID), nil
			}
			return fmt.Sprintf("Отменил напоминание #%d («%s»).", a.ID, label), nil
		},
	}
}
