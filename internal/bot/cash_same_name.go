package bot

import (
	"context"
	"fmt"
	"time"

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

// cashCorrectionWindow — наличку, переотправленную тем же человеком не позже
// этого, считаем исправлением имени (а не просто повтором).
const cashCorrectionWindow = 30 * time.Minute

// heldCashNameBetter — платёж переходит на имя повторно присланной налички,
// только когда оно точнее прежнего. Известный клиент (у него есть другие
// записи) платёж не теряет ради нового имени без истории — ни ради полного
// («Исаев Харон Магомедович»), ни ради опечатки; и наоборот, новое имя
// уступает известному. Иначе (оба новые) — полное имя вместо неполного, а при
// равной полноте — лишь когда тот же человек вскоре переотправил (исправил
// опечатку); повтор другим сотрудником с опечаткой прежнее имя не меняет.
func heldCashNameBetter(p db.CashSamePair) bool {
	if p.KeptOther > 0 || p.HeldOther > 0 {
		return p.KeptOther == 0
	}
	if kw, hw := fullWordCount(p.KeptName), fullWordCount(p.HeldName); kw != hw {
		return hw > kw
	}
	return p.SameSender && p.Gap <= cashCorrectionWindow
}
