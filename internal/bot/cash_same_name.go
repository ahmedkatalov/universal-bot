package bot

import (
	"context"
	"fmt"

	"whatsapp-bot/internal/db"
)

// resolveCashDup — ответ «новый / тот же» на придержанную наличку txID. «Тот
// же» оставляет в сборе один платёж — и под точным именем клиента (см.
// keepCorrectedCashName).
func (b *Bot) resolveCashDup(ctx context.Context, txID int, isNew bool) (found bool, amount float64, client string, err error) {
	found, amount, client, err = b.db.ResolveCashDup(ctx, txID, isNew)
	if err == nil && found && !isNew {
		b.keepCorrectedCashName(ctx, txID)
	}
	return found, amount, client, err
}

// keepCorrectedCashName — наличку heldTxID признали повтором («тот же»): в
// сборе остаётся прежняя запись, но наличку часто переотправляют именно ради
// имени — дописали («Исаев» → «Исаев Харон») или исправили опечатку («Исаев
// Харн» → «Исаев Харон»). Тогда оставшийся платёж переходит на точное имя
// (дата и сумма — прежние). Контакты не склеиваются: меняется клиент одной
// записи.
func (b *Bot) keepCorrectedCashName(ctx context.Context, heldTxID int) {
	p, ok, err := b.db.CashSamePair(ctx, heldTxID)
	if err != nil {
		fmt.Println("Повтор налички: ошибка поиска пары:", err)
		return
	}
	if !ok || p.HeldContact == 0 || p.HeldContact == p.KeptContact || whoPlaceholders[p.HeldName] || !heldCashNameBetter(p) {
		return
	}
	if err := b.db.SetTxClient(ctx, p.KeptID, p.HeldContact, p.HeldName); err != nil {
		fmt.Println("Повтор налички: ошибка смены клиента:", err)
		return
	}
	fmt.Printf("Повтор налички: платёж %d записан на %q вместо %q (имя исправили)\n", p.KeptID, p.HeldName, p.KeptName)
}

// heldCashNameBetter — имя повторно присланной налички точнее прежнего:
// полнее (больше слов ФИО); при равном — прежнее не известный уже клиент
// (у него нет других записей, а у нового есть) или оба новые: переотправили,
// чтобы исправить.
func heldCashNameBetter(p db.CashSamePair) bool {
	if kw, hw := fullWordCount(p.KeptName), fullWordCount(p.HeldName); kw != hw {
		return hw > kw
	}
	if (p.KeptOther > 0) != (p.HeldOther > 0) {
		return p.HeldOther > 0
	}
	return true
}
