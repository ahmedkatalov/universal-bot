package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/report"
)

type senderReportArgs struct {
	FromDate    string   `json:"from_date"`
	ToDate      string   `json:"to_date"`
	Group       string   `json:"group"`
	Sender      string   `json:"sender"`
	Format      string   `json:"format"`
	DateBasis   string   `json:"date_basis"`
	Attribution string   `json:"attribution"`
	Kind        string   `json:"kind"`
	View        string   `json:"view"`
	Columns     []string `json:"columns"`
	SortBy      string   `json:"sort_by"`
	Descending  bool     `json:"descending"`
	Limit       int      `json:"limit"`
	Offset      int      `json:"offset"`
}

var employeeColumnLabels = map[string]string{
	"date": "Прислан в чат", "operation_date": "Дата операции", "client": "ФИО клиента",
	"amount": "Сумма, ₽", "employee": "Сотрудник", "kind": "Вид платежа",
	"bank_payer": "Плательщик на чеке", "collector": "Кто забрал", "group": "Группа", "status": "Статус",
}

func (b *Bot) sendersTool(_ context.Context, chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "senders_report",
		Description: "Отчёт ПО СОТРУДНИКАМ: кто сколько уникальных чеков прислал, их сумма, наличка отдельно, " +
			"а также ПОЛНЫЙ СПИСОК платежей сотрудника с ФИО клиентов и суммами. На «сколько чеков закинул Мансур, " +
			"сумму и ФИО плательщиков» используй sender=Мансур, kind=checks, view=payments. " +
			"По умолчанию период считается ПО ДАТЕ ПРИСЫЛКИ В ЧАТ, включая присланные старые чеки. " +
			"Дубли, удалённые и исключённые платежи не увеличивают сбор. Непроверенные чеки видны отдельно: " +
			"их количество и известная сумма не скрываются и не подмешиваются в подтверждённый сбор. " +
			"Клиент — ФИО из подписи/сообщения рядом с чеком; банковский отправитель — отдельное поле bank_payer. " +
			"sender — имя сотрудника или его телефон в любом формате; несколько совпадений показываются раздельно. " +
			"PDF содержит весь список; текст — страницы с полными итогами и next_offset. " +
			"Для произвольной таблицы выбери columns; для группировки по плательщикам используй результаты в make_custom_pdf.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date":   map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD включительно; за всё время — 2000-01-01"},
				"to_date":     map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD включительно"},
				"group":       map[string]any{"type": "string", "description": "Группа или несколько через запятую; пусто — все рабочие группы"},
				"sender":      map[string]any{"type": "string", "description": "Имя сотрудника в именительном падеже (Мансур) или телефон. Пусто — все сотрудники"},
				"format":      map[string]any{"type": "string", "enum": []string{"text", "pdf"}},
				"date_basis":  map[string]any{"type": "string", "enum": []string{"chat", "operation"}, "description": "chat по умолчанию — когда прислал в чат; operation только если просят по дате банковской операции"},
				"attribution": map[string]any{"type": "string", "enum": []string{"submitter", "collector"}, "description": "submitter по умолчанию — кто прислал. collector — кто забрал деньги (по явной пометке, иначе отправитель)"},
				"kind":        map[string]any{"type": "string", "enum": []string{"all", "checks", "cash", "transfers"}, "description": "checks — только чеки; cash — только наличка; all (по умолчанию) — все виды с отдельными количествами и суммами"},
				"view":        map[string]any{"type": "string", "enum": []string{"summary", "payments"}, "description": "summary — сводка по сотрудникам. payments — сводка И подробный список ФИО/сумм. По умолчанию для конкретного сотрудника payments, для всех summary"},
				"columns":     map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"date", "operation_date", "client", "amount", "employee", "kind", "bank_payer", "collector", "group", "status"}}, "description": "Колонки подробного списка в нужном порядке; по умолчанию дата, ФИО клиента, сумма, вид, сотрудник, статус"},
				"sort_by":     map[string]any{"type": "string", "enum": []string{"date", "operation_date", "amount", "client", "employee"}},
				"descending":  map[string]any{"type": "boolean", "description": "Сортировать по убыванию; по умолчанию по возрастанию даты"},
				"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Строк на странице текста (по умолчанию 50); PDF всегда полный"},
				"offset":      map[string]any{"type": "integer", "minimum": 0, "description": "Пропустить столько строк: используй next_offset предыдущей страницы, сохраняя остальные фильтры"},
			},
			"required": []string{"from_date", "to_date"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args senderReportArgs
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("не удалось разобрать аргументы: %w", err)
			}
			from, toDay, err := args.validate()
			if err != nil {
				return "", err
			}
			groups, groupLabel, err := b.resolveGroups(ctx, args.Group)
			if err != nil {
				return "", err
			}
			data, err := b.db.EmployeePayments(ctx, from, toDay.AddDate(0, 0, 1), db.EmployeeReportOptions{
				Groups: groups, Sender: strings.TrimSpace(args.Sender), DateBasis: args.DateBasis, Attribution: args.Attribution, Kind: args.Kind,
			})
			if err != nil {
				return "", fmt.Errorf("ошибка выборки: %w", err)
			}
			args.sortRows(data.Rows)
			period := from.Format("02.01.2006") + " — " + toDay.Format("02.01.2006")
			basis := "по дате присылки в чат"
			if args.DateBasis == "operation" {
				basis = "по дате операции"
			}
			var out strings.Builder
			fmt.Fprintf(&out, "Отчёт по сотрудникам: %s (%s; %s).\n", period, groupLabel, basis)
			if args.Attribution == "collector" {
				out.WriteString("Ответственный — кто забрал деньги; без пометки — отправитель.\n")
			} else {
				out.WriteString("Сотрудник — кто прислал сообщение в WhatsApp.\n")
			}
			if len(data.Rows) == 0 {
				out.WriteString("За выбранный период и фильтры платежей не нашёл.")
				return out.String(), nil
			}
			for _, s := range data.Stats {
				fmt.Fprintf(&out, "• %s: %s\n", employeeLabel(s.Name, s.Phone), employeeStatText(s))
			}
			total := employeeTotals(data.Stats)
			fmt.Fprintf(&out, "ИТОГО за ВЕСЬ период: %s\n", employeeStatText(total))
			out.WriteString("Один чек считается один раз. Удалённые и исключённые платежи не учитываются.\n")
			if args.Sender != "" && len(data.Stats) > 1 {
				out.WriteString("По имени найдены несколько сотрудников — показаны раздельно; для одного укажи телефон.\n")
			}
			if args.Format == "pdf" {
				if err := b.sendEmployeePDF(ctx, chat, args, data, period, groupLabel, basis); err != nil {
					return "", err
				}
				out.WriteString("Полный PDF отправлен: все строки выбранной выборки, без обрезки.")
				return out.String(), nil
			}
			if args.View != "payments" {
				return out.String(), nil
			}
			start := min(args.Offset, len(data.Rows))
			end := min(start+args.Limit, len(data.Rows))
			fmt.Fprintf(&out, "\nСписок платежей: строки %d–%d из %d.\n", min(start+1, end), end, len(data.Rows))
			var headings []string
			for _, c := range args.Columns {
				headings = append(headings, employeeColumnLabels[c])
			}
			out.WriteString(strings.Join(headings, " | ") + "\n")
			for i := start; i < end; i++ {
				cells := b.employeeCells(ctx, data.Rows[i], args.Columns)
				fmt.Fprintf(&out, "%d. %s\n", i+1, strings.Join(cells, " | "))
			}
			if end < len(data.Rows) {
				fmt.Fprintf(&out, "Список НЕ полный. next_offset=%d; остальные фильтры сохрани. Для всего списка одним файлом вызови format=pdf. Итоги выше включают все %d строк.", end, len(data.Rows))
			} else {
				out.WriteString("Конец списка.")
			}
			return out.String(), nil
		},
	}
}

func (a *senderReportArgs) validate() (time.Time, time.Time, error) {
	from, err := time.ParseInLocation("2006-01-02", a.FromDate, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("неверная дата начала %q, нужен YYYY-MM-DD", a.FromDate)
	}
	to, err := time.ParseInLocation("2006-01-02", a.ToDate, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("неверная дата конца %q, нужен YYYY-MM-DD", a.ToDate)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("дата конца раньше начала")
	}
	if a.DateBasis == "" {
		a.DateBasis = "chat"
	}
	if a.Kind == "" {
		a.Kind = "all"
	}
	if a.Attribution == "" {
		a.Attribution = "submitter"
	}
	if a.Format == "" {
		a.Format = "text"
	}
	if a.View == "" {
		a.View = "summary"
		if strings.TrimSpace(a.Sender) != "" || len(a.Columns) > 0 {
			a.View = "payments"
		}
	}
	for _, p := range []struct {
		value   string
		allowed []string
	}{
		{a.DateBasis, []string{"chat", "operation"}}, {a.Attribution, []string{"submitter", "collector"}}, {a.Kind, []string{"all", "checks", "cash", "transfers"}},
		{a.View, []string{"summary", "payments"}}, {a.Format, []string{"text", "pdf"}}, {a.SortBy, []string{"", "date", "operation_date", "amount", "client", "employee"}},
	} {
		valid := false
		for _, s := range p.allowed {
			if s == p.value {
				valid = true
			}
		}
		if !valid {
			return time.Time{}, time.Time{}, fmt.Errorf("неизвестный параметр отчёта %q", p.value)
		}
	}
	if a.Offset < 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("offset не может быть отрицательным")
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	if a.Limit > 200 {
		a.Limit = 200
	}
	if len(a.Columns) == 0 {
		a.Columns = []string{"date", "client", "amount", "kind", "employee", "status"}
		if a.DateBasis == "operation" {
			a.Columns[0] = "operation_date"
		}
	}
	for _, c := range a.Columns {
		if _, ok := employeeColumnLabels[c]; !ok {
			return time.Time{}, time.Time{}, fmt.Errorf("неизвестная колонка %q", c)
		}
	}
	return from, to, nil
}

func employeeMoney(v float64) string {
	s := strconv.FormatFloat(math.Round(v*100)/100, 'f', 2, 64)
	return strings.TrimSuffix(s, ".00")
}

func employeeLabel(name, phone string) string {
	if name == "" || name == phone {
		if phone != "" {
			return "+" + phone
		}
		return "неизвестный сотрудник"
	}
	if phone != "" {
		return name + " (+" + phone + ")"
	}
	return name
}

func employeeStatText(s db.EmployeeStat) string {
	text := fmt.Sprintf("чеки %d шт. на %s ₽", s.Checks, employeeMoney(s.ChecksSum))
	if s.Cash > 0 {
		text += fmt.Sprintf("; наличка %d платежей на %s ₽", s.Cash, employeeMoney(s.CashSum))
	}
	if s.Transfers > 0 {
		text += fmt.Sprintf("; переводы текстом %d на %s ₽", s.Transfers, employeeMoney(s.TransfersSum))
	}
	text += fmt.Sprintf("; учтено всего %s ₽ (%d платежей)", employeeMoney(s.Total), s.Count)
	if s.PendingChecks+s.PendingCash+s.PendingTransfers > 0 {
		text += fmt.Sprintf(". НЕ вошло в подтверждённый сбор: ждут уточнения — чеки %d, наличка %d, переводы %d; известная сумма %s ₽", s.PendingChecks, s.PendingCash, s.PendingTransfers, employeeMoney(s.PendingSum))
		if s.UnknownAmounts > 0 {
			text += fmt.Sprintf("; сумма не прочитана у %d", s.UnknownAmounts)
		}
		text += fmt.Sprintf(". Прислано уникальных чеков, включая ожидающие: %d", s.Checks+s.PendingChecks)
	}
	return text
}

func employeeTotals(stats []db.EmployeeStat) db.EmployeeStat {
	var t db.EmployeeStat
	for _, s := range stats {
		t.Count += s.Count
		t.Checks += s.Checks
		t.Cash += s.Cash
		t.Transfers += s.Transfers
		t.Total += s.Total
		t.ChecksSum += s.ChecksSum
		t.CashSum += s.CashSum
		t.TransfersSum += s.TransfersSum
		t.PendingChecks += s.PendingChecks
		t.PendingCash += s.PendingCash
		t.PendingTransfers += s.PendingTransfers
		t.PendingSum += s.PendingSum
		t.UnknownAmounts += s.UnknownAmounts
	}
	return t
}

func (a senderReportArgs) sortRows(rows []db.EmployeePayment) {
	sort.SliceStable(rows, func(i, j int) bool {
		x, y := rows[i], rows[j]
		cmp := 0
		switch a.SortBy {
		case "client":
			cmp = strings.Compare(strings.ToLower(x.Client), strings.ToLower(y.Client))
		case "employee":
			cmp = strings.Compare(strings.ToLower(x.Employee), strings.ToLower(y.Employee))
		case "amount":
			if x.Amount < y.Amount {
				cmp = -1
			} else if x.Amount > y.Amount {
				cmp = 1
			}
		case "operation_date":
			cmp = x.OperationAt.Compare(y.OperationAt)
		default:
			cmp = x.PostedAt.Compare(y.PostedAt)
		}
		if cmp == 0 {
			cmp = strings.Compare(x.Key, y.Key)
		}
		if a.Descending {
			return cmp > 0
		}
		return cmp < 0
	})
}

func (b *Bot) employeeCells(ctx context.Context, r db.EmployeePayment, columns []string) []string {
	client := r.Client
	if client == "" {
		client = "ФИО клиента не указано"
	}
	kind := map[string]string{"checks": "чек", "cash": "наличка", "transfers": "перевод"}[r.Kind]
	status := map[string]string{"counted": "учтён", "review": "ждёт уточнения клиента/чека", "no_amount": "сумма не прочитана", "held": "возможный повтор, ждёт уточнения"}[r.Status]
	amount := employeeMoney(r.Amount)
	if r.Amount <= 0 {
		amount = "не прочитана"
	}
	values := map[string]string{
		"date": r.PostedAt.In(time.Local).Format("02.01.2006 15:04"), "operation_date": r.OperationAt.In(time.Local).Format("02.01.2006 15:04"),
		"client": client, "bank_payer": r.BankPayer, "amount": amount, "employee": employeeLabel(r.Employee, r.EmployeePhone), "kind": kind, "collector": r.Collector, "group": r.GroupJID, "status": status,
	}
	var out []string
	for _, c := range columns {
		value := values[c]
		if c == "group" {
			if jid, err := types.ParseJID(r.GroupJID); err == nil {
				if name := b.joinedGroups(ctx)[jid]; name != "" {
					value = name
				}
			}
		}
		if value == "" {
			value = "—"
		}
		out = append(out, value)
	}
	return out
}

func (b *Bot) sendEmployeePDF(ctx context.Context, chat types.JID, args senderReportArgs, data db.EmployeeReport, period, groupLabel, basis string) error {
	section := report.Section{Title: "Сводка по сотрудникам. Ожидающие не входят в учтённый сбор.", Columns: []string{"Сотрудник", "Чеки, шт.", "Чеки, ₽", "Нал., ₽", "Перев., ₽", "Учтено, ₽", "Ждут, шт.", "Ждут, ₽"}}
	for _, s := range data.Stats {
		section.Rows = append(section.Rows, []string{employeeLabel(s.Name, s.Phone), strconv.Itoa(s.Checks), employeeMoney(s.ChecksSum), employeeMoney(s.CashSum), employeeMoney(s.TransfersSum), employeeMoney(s.Total), strconv.Itoa(s.PendingChecks + s.PendingCash + s.PendingTransfers), employeeMoney(s.PendingSum)})
	}
	t := employeeTotals(data.Stats)
	section.TotalRow = []string{"ИТОГО", strconv.Itoa(t.Checks), employeeMoney(t.ChecksSum), employeeMoney(t.CashSum), employeeMoney(t.TransfersSum), employeeMoney(t.Total), strconv.Itoa(t.PendingChecks + t.PendingCash + t.PendingTransfers), employeeMoney(t.PendingSum)}
	sections := []report.Section{section}
	if args.View == "payments" {
		detail := report.Section{Title: fmt.Sprintf("Полный список: %d платежей. Ожидающие не входят в учтённый сбор.", len(data.Rows))}
		for _, c := range args.Columns {
			detail.Columns = append(detail.Columns, employeeColumnLabels[c])
		}
		for _, r := range data.Rows {
			detail.Rows = append(detail.Rows, b.employeeCells(ctx, r, args.Columns))
		}
		sections = append(sections, detail)
	}
	path := filepath.Join(b.reportDir, fmt.Sprintf("employees_%d.pdf", time.Now().UnixNano()))
	if err := report.GenerateTables("Отчёт по сотрудникам", period+" | "+groupLabel+" | "+basis, sections, b.fontDir, path); err != nil {
		return fmt.Errorf("ошибка генерации PDF: %w", err)
	}
	if err := b.sendDocument(chat, path, "Чеки_по_сотрудникам_"+args.FromDate+".pdf"); err != nil {
		return fmt.Errorf("не удалось отправить PDF: %w", err)
	}
	return nil
}
