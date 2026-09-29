// Планировщик напоминаний/сообщений: разовые и повторяющиеся задания живут в
// Postgres (таблица scheduled_jobs), переживают рестарт и выполняются идемпотентно
// (задание сдвигается на следующий срок атомарно перед отправкой). Инструменты
// ассистента (создать/показать/отменить) доступны только владельцу.
package bot

import (
	"context"
	"fmt"
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

func (b *Bot) runDueJobs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	jobs, err := b.db.ClaimDueJobs(ctx, time.Now())
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
		b.sendText(jid, j.Message)
		fmt.Printf("Планировщик: задание %d выполнено (%s -> %s)\n", j.ID, j.Kind, j.TargetKind)
	}
}
