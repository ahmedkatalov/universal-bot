package bot

import (
	"context"
	"time"

	"whatsapp-bot/internal/db"
)

// judgeDueWatches — судим чеки одного клиента так же, как сверка
// (cmf_check_receipts): одна оплата программы закрывает только один чек, вся
// картина клиента (другие его чеки и наличка вокруг) учитывается. Возвращает
// вердикт по id наблюдения; наблюдения без пары в учёте в ответ не попадают
// (их проверяют по-старому).
// dueLedger — чеки и наличка учёта вокруг напоминаемых чеков (одна выборка на
// весь круг напоминаний).
type dueLedger struct {
	from, to time.Time
	rawRecs  []db.ReconReceipt
	recs     []reconRec
	cash     []db.ReconCash
}

func (b *Bot) loadDueLedger(ctx context.Context, ws []db.CmfWatch) (*dueLedger, error) {
	if len(ws) == 0 {
		return nil, nil
	}
	minDate := ws[0].TxDate
	for _, w := range ws {
		if w.TxDate.Before(minDate) {
			minDate = w.TxDate
		}
	}
	l := &dueLedger{from: minDate.AddDate(0, 0, -reconContextDays), to: time.Now().AddDate(0, 0, 1)}
	var err error
	if l.rawRecs, err = b.db.ReceiptsForReconcile(ctx, l.from, l.to); err != nil {
		return nil, err
	}
	if l.cash, err = b.db.CashForReconcile(ctx, l.from, l.to); err != nil {
		return nil, err
	}
	l.recs = dedupeReconReceipts(l.rawRecs)
	return l, nil
}

func (b *Bot) judgeDueWatches(ctx context.Context, l *dueLedger, clientID string, ws []db.CmfWatch) (map[int]reconVerdict, error) {
	if l == nil || len(ws) == 0 {
		return nil, nil
	}
	rawRecs, recs, cash := l.rawRecs, l.recs, l.cash
	ctxFrom, ctxTo := l.from, l.to

	var names []string
	for _, w := range ws {
		names = append(names, w.ClientName, w.ClientText)
	}
	isHis := func(name string) bool {
		for _, n := range names {
			if n != "" && sameClientName(name, n) {
				return true
			}
		}
		return false
	}
	// Наблюдение ↔ чек учёта: по сообщению, а копия (пересылка) — как тот же чек.
	rawByWa := map[string]db.ReconReceipt{}
	for _, r := range rawRecs {
		if r.WaMessageID != "" {
			rawByWa[r.WaMessageID] = r
		}
	}
	itemOf := map[int]int{} // id наблюдения → индекс позиции
	var items []reconItem
	for _, r := range recs {
		if r.NeedsReview {
			continue
		}
		var mine []int
		for _, w := range ws {
			if raw, ok := rawByWa[w.WaMessageID]; ok && (raw.ID == r.ID || sameCheckCopy(raw, r.ReconReceipt)) {
				mine = append(mine, w.ID)
			}
		}
		if len(mine) == 0 && !isHis(r.Name) {
			continue
		}
		for _, id := range mine {
			itemOf[id] = len(items)
		}
		items = append(items, reconItem{ID: r.ID, Kind: "check", Amount: r.Amount, Date: r.TxDate, Report: len(mine) > 0})
	}
	for _, c := range cash {
		if isHis(c.Name) {
			items = append(items, reconItem{ID: -c.ID, Kind: "cash", Amount: c.Amount, Date: c.TxDate})
		}
	}
	if len(itemOf) == 0 {
		return nil, nil
	}
	pays, err := b.cmf.PaymentsBetween(ctx, clientID, ctxFrom.AddDate(0, 0, -10), ctxTo.AddDate(0, 0, 20))
	if err != nil {
		return nil, err
	}
	mode := unitsRubles
	if r, k := reconUnitTally(items, pays); k > r {
		mode = unitsKopecks
	}
	mr := humanMatchFull(items, pays, mode)
	out := map[int]reconVerdict{}
	for id, i := range itemOf {
		if i < len(mr.Verdicts) {
			out[id] = mr.Verdicts[i]
		}
	}
	return out, nil
}
