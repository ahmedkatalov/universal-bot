// Планировщик напоминаний/сообщений: разовые и повторяющиеся задания живут в
// Postgres (таблица scheduled_jobs), переживают рестарт и выполняются идемпотентно
// (задание сдвигается на следующий срок атомарно перед отправкой). Инструменты
// ассистента (создать/показать/отменить) доступны только владельцу.
package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// schedulerLoop раз в 30 секунд проверяет и выполняет задания, которым пора.
func (b *Bot) schedulerLoop() {
	time.Sleep(15 * time.Second) // дать клиенту подключиться после старта
	b.runDueJobs()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		b.runDueJobs()
	}
}

func (b *Bot) runDueJobs() { b.runDueJobsAt(time.Now()) }

func (b *Bot) runDueJobsAt(now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	jobs, err := b.db.ClaimDueJobs(ctx, now)
	if err != nil {
		fmt.Println("Планировщик: ошибка выборки заданий:", err)
		return
	}
	for _, j := range jobs {
		jid, perr := types.ParseJID(j.Target)
		if perr != nil || jid.User == "" {
			fmt.Printf("Планировщик: неверный получатель %q у задания %d — пропуск\n", j.Target, j.ID)
			continue
		}
		text := j.Message
		if strings.HasPrefix(text, reportJobPrefix) {
			text = b.dayCollectionText(ctx, jid, strings.TrimPrefix(text, reportJobPrefix))
		}
		b.sendText(jid, text)
		fmt.Printf("Планировщик: задание %d выполнено (%s -> %s)\n", j.ID, j.Kind, j.TargetKind)
	}
}

// reportJobPrefix — задание присылает не текст, а сбор за день (дальше — группы).
const reportJobPrefix = "@сбор_за_день:"

// jobLabel — как показать задание владельцу.
func jobLabel(msg string) string {
	if strings.HasPrefix(msg, reportJobPrefix) {
		if g := strings.TrimPrefix(msg, reportJobPrefix); g != "" {
			return "сбор за день с цифрами (" + g + ")"
		}
		return "сбор за день с цифрами (все группы)"
	}
	return msg
}

// dayCollectionText — сбор за сегодня, теми же расчётами, что send_finance_report.
func (b *Bot) dayCollectionText(ctx context.Context, chat types.JID, spec string) string {
	groupJIDs, label, err := b.resolveGroups(ctx, spec)
	if err != nil {
		return "Сбор за день: не нашёл группу «" + spec + "» — " + err.Error()
	}
	day := time.Now().Format("2006-01-02")
	out, err := b.buildReportForAssistant(ctx, chat, day, day, "text", groupJIDs, label, "")
	if err != nil {
		return "Сбор за день: не получилось посчитать — " + err.Error()
	}
	return "📊 Сбор за " + time.Now().Format("02.01.2006") + " (" + label + "):\n" + out
}
