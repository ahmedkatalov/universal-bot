package bot

import (
	"context"
	"fmt"
	"time"

	"whatsapp-bot/internal/cmf"

	"go.mau.fi/whatsmeow/types"
)

// Здоровье связи с программой рассрочек. Раньше сбой (сменили пароль бота,
// программа лежит) был виден только в журнале сервера: бот просто переставал
// напоминать «занесите в программу», и владелец не знал почему.

const cmfAlertEvery = 6 * time.Hour

// cmfNoteResult — учесть результат обращения к программе. Сбой всей программы
// — сообщить владельцу в личку (не чаще раза в 6 часов); связь вернулась —
// сказать об этом и сразу перепроверить отложенные чеки.
func (b *Bot) cmfNoteResult(err error, fallback ...types.JID) {
	if b.cmf == nil {
		return
	}
	b.cmfHealthMu.Lock()
	if err == nil || !cmf.IsOutage(err) {
		wasDown := !b.cmfDownSince.IsZero()
		b.cmfDownSince = time.Time{}
		b.cmfHealthMu.Unlock()
		if wasDown && err == nil {
			b.notifyOwners("✅ Связь с программой рассрочек восстановилась — проверяю чеки, которые ждали, пока она не отвечала.")
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				b.cmfRetryLookupsAll(ctx)
			}()
		}
		return
	}
	now := time.Now()
	if b.cmfDownSince.IsZero() {
		b.cmfDownSince = now
	}
	alert := now.Sub(b.cmfAlertAt) >= cmfAlertEvery
	if alert {
		b.cmfAlertAt = now
	}
	since := b.cmfDownSince
	b.cmfHealthMu.Unlock()
	fmt.Printf("cmf: программа недоступна с %s: %v\n", since.Format("02.01 15:04"), err)
	if alert {
		text := fmt.Sprintf("⚠️ Не могу проверить чеки в программе рассрочек: %s. Пока связи нет, напоминаний «занесите в программу» не будет — "+
			"чеки за это время я перепроверю, как только связь появится.", cmf.Human(err))
		if !b.notifyOwners(text) {
			// Владелец не настроен — скажем там, где чек.
			for _, j := range fallback {
				if !j.IsEmpty() && !b.groupSilent(j) {
					b.sendText(j, text)
					break
				}
			}
		}
	}
}

// cmfDown — программа сейчас недоступна (по последним обращениям).
func (b *Bot) cmfDown() bool {
	b.cmfHealthMu.Lock()
	defer b.cmfHealthMu.Unlock()
	return !b.cmfDownSince.IsZero()
}

// notifyOwners — личное сообщение админам отчётности (или владельцам).
func (b *Bot) notifyOwners(text string) bool {
	set := b.reportAdmins
	if len(set) == 0 {
		set = b.ownerNumbers
	}
	sent := false
	for phone := range set {
		if phone == "" {
			continue
		}
		b.sendText(types.NewJID(phone, types.DefaultUserServer), text)
		sent = true
	}
	if !sent {
		fmt.Println("Владельцу некуда написать (нет REPORT_ADMINS / OWNER_NUMBERS):", text)
	}
	return sent
}
