package bot

import (
	"context"
	"fmt"
	"time"

	"whatsapp-bot/internal/db"
)

// Чек с одной ДАТОЙ операции (без времени: Почта Банк и др.). Его дата — с
// чека (дата отчёта), а время неизвестно; полдень, который ставит парсер,
// сбивал сверку с текстом того же платежа: «Магомедов Ахмед 15000» в 08:40 и
// чек, присланный в 13:00, — «текст написан за 3+ часа до операции», то есть
// другой платёж, и сбор задваивался. А совсем «угаданное» время (как у чека
// без даты) теряло дату: текст пятницы и чек пятницы, присланный в
// понедельник, расходились на трое суток, а копии чека в двух группах не
// узнавались. Поэтому время такому чеку выбираем так, чтобы он сходился со
// своими копиями и с текстом этого платежа: время уже записанной копии того
// же напечатанного чека, иначе время самого раннего текста этого клиента на
// эту сумму, написанного в день операции до присылки чека (клиент ещё не
// назван — любого текста на эту сумму), иначе полдень.

// dateOnlyOpTime — время операции для чека с одной датой. p — напечатанные
// поля (p.TxDate — дата с чека, полдень); client — клиент, названный рядом с
// чеком (пусто — неизвестен); postedAt — когда чек прислали; excludeID — сам
// чек, если он уже записан.
func (b *Bot) dateOnlyOpTime(ctx context.Context, excludeID int, p db.PrintedReceipt, client string, postedAt time.Time) time.Time {
	if b.db == nil {
		return p.TxDate
	}
	if t, ok, err := b.db.DateOnlyTwinTime(ctx, excludeID, p); err != nil {
		fmt.Println("Чек с одной датой: ошибка поиска копии:", err)
	} else if ok {
		return t
	}
	texts, err := b.db.DayTransferTexts(ctx, p.Amount, p.TxDate, postedAt)
	if err != nil {
		fmt.Println("Чек с одной датой: ошибка поиска текста платежа:", err)
		return p.TxDate
	}
	if len(normWords(client)) == 0 {
		// Клиента назовут потом («чек, потом ФИО»): самый ранний текст на эту
		// сумму в тот день — операция была не позже, и ни один текст этого дня
		// не окажется «написанным задолго до операции».
		if len(texts) > 0 {
			return texts[0].PostedAt
		}
		return p.TxDate
	}
	for _, t := range texts {
		if dayTextSameClient(client, t.Client) {
			return t.PostedAt
		}
	}
	return p.TxDate
}

// dayTextSameClient — клиент текста — клиент чека: то же ФИО (опечатки,
// падежи, порядок слов) или одно имя — часть другого («Ахмед» и «Магомедов
// Ахмед»).
func dayTextSameClient(check, text string) bool {
	if len(normWords(check)) == 0 || len(normWords(text)) == 0 || whoPlaceholders[text] {
		return false
	}
	return sameClientMoney(check, text) || corrNameMatches(text, check) || corrNameMatches(check, text)
}

// anchorStoredDateOnly — чек id с одной датой уже записан (повторное чтение):
// выбираем его время так же, как при приёме.
func (b *Bot) anchorStoredDateOnly(ctx context.Context, id int) {
	a, err := b.db.ReceiptAnchor(ctx, id)
	if err != nil || !a.DateOnly {
		return
	}
	if t := b.dateOnlyOpTime(ctx, id, a.Printed, a.Client, a.PostedAt); !t.Equal(a.Printed.TxDate) {
		if err := b.db.SetReceiptOpTime(ctx, id, t); err != nil {
			fmt.Println("Чек с одной датой: ошибка записи времени:", err)
		}
	}
}
