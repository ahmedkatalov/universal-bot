package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

func containsDigit(lines []string) bool {
	for _, l := range lines {
		for _, r := range l {
			if unicode.IsDigit(r) {
				return true
			}
		}
	}
	return false
}

func extractJSONBlock(s string) string {
	iArr := strings.Index(s, "[")
	iObj := strings.Index(s, "{")
	if iObj >= 0 && (iArr < 0 || iObj < iArr) {
		if end := strings.LastIndex(s, "}"); end > iObj {
			return s[iObj : end+1]
		}
	}
	if iArr >= 0 {
		if end := strings.LastIndex(s, "]"); end > iArr {
			return s[iArr : end+1]
		}
	}
	return ""
}

type aiPayment struct {
	Name      string  `json:"name"`
	Amount    float64 `json:"amount"`
	Note      string  `json:"note"`
	Card      string  `json:"card"`
	Cash      bool    `json:"cash"`      // наличка (наличными/нал/офис/«у ‹имя›»)
	Collector string  `json:"collector"` // кто ЗАБРАЛ наличку (ответственный), если указан
}

func (b *Bot) aiRescueUnparsed(ctx context.Context, chat types.JID, senderName string, lines []string, rawID int, txDate time.Time, cashHint bool) {
	// Помечаем сообщение разобранным ТОЛЬКО по завершении записи. Если горутина
	// упадёт с паникой ИЛИ платежи нашлись, но ни один не записался (БД
	// недоступна) — не помечаем, чтобы пересчёт (recount) переразобрал его и
	// платёж не потерялся.
	keepUnparsed := false
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("ИИ-доразбор: паника, оставляю сообщение на пересчёт:", r)
			return
		}
		if keepUnparsed {
			fmt.Printf("ИИ-доразбор: сообщение %d — платёж найден, но не записан; оставляю на пересчёт\n", rawID)
			return
		}
		_ = b.db.MarkMessageParsed(ctx, rawID)
	}()
	system := "Ты — модуль разбора платежей в WhatsApp-боте учёта финансов. " +
		"Тебе дают сообщение из рабочей группы — в нём могут быть платежи (переводы и/или наличка) в ЛЮБОМ формате. " +
		"Разбери их по СМЫСЛУ, как понял бы человек, а не по шаблону. " +
		"Известные люди: " + strings.Join(b.aliases.Canonicals(), ", ") + ".\n\n" +
		"Верни СТРОГО один JSON-объект вида " +
		`{"payments":[{"name":"ФИО клиента","amount":12345,"cash":false,"collector":"","note":"","card":"втб|сбер|наличные|"}],"clarify":""}` + ".\n\n" +
		"В payments включай ТОЛЬКО РЕАЛЬНО СОВЕРШЁННЫЕ операции — деньги фактически сдали/перевели/принесли. " +
		"НЕ включай обсуждения, планы, вопросы: «сказал взять 5000», «может нужно 10000», «сколько будет 5000?» — пустой payments.\n" +
		"РАЗБИРАЙ ГРЯЗНЫЕ ФОРМАТЫ — имя, сумма, пометка «наличка» и «кто забрал» часто на РАЗНЫХ строках или в одном сообщении " +
		"НЕСКОЛЬКО платежей. Примеры (каждый -> один платёж):\n" +
		"• «Джабраилов сулеман / наличка / 72.600р / мансур взял» -> name:«Джабраилов Сулейман», amount:72600, cash:true, collector:«Мансур».\n" +
		"• «Оплата наличными:35.000₽ / Асхабов Ибрагим / Отдал к Солтамурадову Адаму» -> name:«Асхабов Ибрагим», amount:35000, cash:true, collector:«Солтамурадов Адам».\n" +
		"• «У Усумова Рауфа забрал 31 т / Наличка» -> name:«Усумов Рауф», amount:31000, cash:true, collector:«» (кто забрал не назван — тот, кто пишет).\n" +
		"• «Умхадижиев Рахман 170т✅» -> name:«Умхадижиев Рахман», amount:170000, cash:false.\n" +
		"• «Манаев Шамиль 120т / Дебишов Хусен 60т» -> ДВА платежа.\n" +
		"cash — наличные ли это. Реши ПО СМЫСЛУ, как человек, а НЕ по конкретным словам: наличка = деньги передали/принесли/" +
		"забрали/сдали ФИЗИЧЕСКИ, из рук в руки (в офис, на руки, живыми, кэшем, деньгами занёс/принёс, «у ‹имя›», «отдал ‹имя›»). " +
		"Перевод = деньги ушли через банк / на карту / чеком. Слова «наличка/нал» — лишь частые подсказки, но их МОЖЕТ НЕ БЫТЬ: " +
		"если по контексту деньги отдали вживую — cash:true даже без слова «наличка». Явный банковский перевод/на карту — cash:false. " +
		"collector — кто ФИЗИЧЕСКИ забрал/принял наличку. Это может быть человек ИЛИ место (кафе, офис, точка). " +
		"Имя того, кто забрал, может стоять ПОСЛЕ или ПЕРЕД словом взял/забрал/отдал/принял: «мансур взял», «отдал Адаму», " +
		"«нур коффе отдал» -> collector:«Нур Коффе», «отнёс в офис» -> collector:«офис». " +
		"ЕСЛИ в сообщении назван тот, кто забрал деньги или кому их отдали — ОБЯЗАТЕЛЬНО заполни collector, НЕ оставляй пустым " +
		"(иначе бот лишний раз переспросит). Если действительно не указан — пустая строка. Имя приводи к именительному падежу " +
		"(«У Усумова Рауфа» -> «Усумов Рауф»).\n" +
		"Суммы и сокращения: 5к=5000, 25 тыщ=25000, 170т=170000, 31 т=31000, 120т=120000, лям=1000000, «косарь»=1000. " +
		"«72.600»/«35.000»/«100,000» — это тысячи (72600, 35000, 100000). Сумма — числом в рублях.\n" +
		"name — ФИО клиента (кто оплатил рассрочку), из списка известных если совпадает, иначе как в сообщении.\n\n" +
		"clarify — уточняющий вопрос ОДНОЙ короткой фразой, ТОЛЬКО если строка похожа на реальную операцию, " +
		"но непонятно ключевое (был ли платёж на самом деле, чьи это деньги, какая сумма). " +
		"ВАЖНО: строка либо в payments, либо в clarify — никогда одновременно. Если непонятно, от кого деньги, " +
		"НЕ записывай их на отправителя сообщения — задай clarify и оставь payments пустым. " +
		"Для болтовни и явных обсуждений clarify оставь пустым — не переспрашивай по пустякам."

	// Детерминированно вытаскиваем суммы из текста и даём их ИИ ЯКОРЕМ — модели
	// иногда ошибаются в цифрах («10.000р» прочитали как 5000). Парсер читает
	// форматы сумм надёжно, поэтому передаём точные значения и просим не пересчитывать.
	// Считаем ВСЕ денежные числа сообщения (не «первое в строке»): при «20000 +
	// 5000» подсказка должна содержать оба, иначе ИИ подталкивался к неверному итогу.
	joined := strings.Join(lines, "\n")
	tokens := moneyTokens(joined)
	var foundAmts []string
	seenAmt := map[float64]bool{}
	for _, a := range tokens {
		if !seenAmt[a] {
			seenAmt[a] = true
			foundAmts = append(foundAmts, fmt.Sprintf("%.0f", a))
		}
	}
	user := "Отправитель сообщения: " + senderName + "\nСтроки:\n" + joined
	if len(foundAmts) > 0 {
		user += "\n\nСуммы, которые ТОЧНО есть в тексте (бери их КАК ЕСТЬ, не пересчитывай и не дели): " + strings.Join(foundAmts, ", ") + " ₽."
	}

	// 3-ФАЗНАЯ ПРОВЕРКА (само-согласованность): читаем сообщение НЕСКОЛЬКО раз
	// независимо и берём согласованный результат. Так СУММА берётся медианой
	// (разовая ошибка вроде «10.000»→5000 отсекается), а НАЛ/ПЕРЕВОД — по
	// большинству голосов (одно случайное «наличка» не перепутает перевод).
	n := paymentReads()
	type runResult struct {
		payments []aiPayment
		clarify  string
		ok       bool
	}
	ch := make(chan runResult, n)
	for i := 0; i < n; i++ {
		go func() {
			out, err := b.assistant.Complete(ctx, system, user)
			if err != nil {
				ch <- runResult{}
				return
			}
			block := extractJSONBlock(out)
			if block == "" {
				ch <- runResult{}
				return
			}
			var p struct {
				Payments []aiPayment `json:"payments"`
				Clarify  string      `json:"clarify"`
			}
			if err := json.Unmarshal([]byte(block), &p); err != nil {
				ch <- runResult{}
				return
			}
			ch <- runResult{payments: p.Payments, clarify: strings.TrimSpace(p.Clarify), ok: true}
		}()
	}
	var runs [][]aiPayment
	var clarifies []string
	okRuns := 0
	for i := 0; i < n; i++ {
		r := <-ch
		if !r.ok {
			continue
		}
		okRuns++
		runs = append(runs, r.payments)
		if r.clarify != "" {
			clarifies = append(clarifies, r.clarify)
		}
	}
	if okRuns == 0 {
		// Ни одно чтение не удалось (ИИ недоступен) — вердикта НЕТ. Откат к парсеру,
		// но если он ничего не записал — оставляем сообщение на пересчёт (не помечаем
		// разобранным): реальный платёж не должен пропасть только потому, что ИИ лежал.
		fmt.Println("ИИ-доразбор: ни одно чтение не удалось, откат к обычному парсеру")
		saved, _ := b.recordDeterministicPayments(ctx, joined, rawID, txDate, cashHint, false)
		keepUnparsed = saved == 0
		return
	}

	payments := consensusPayments(runs, okRuns)

	// Страховка от ошибки ИИ в цифрах: если в сообщении РОВНО ОДНО денежное число
	// (не «одна строка с суммой» — при «20000 + 5000» подмена ломала бы итог) и на
	// выходе РОВНО один платёж с другой суммой — берём сумму из текста (парсер
	// читает её надёжно). Так «10.000р» точно не станет 5000.
	if len(tokens) == 1 && len(payments) == 1 {
		if det := tokens[0]; det > 0 && payments[0].Amount != det {
			fmt.Printf("ИИ-доразбор: сумма ИИ %.0f заменена на точную из текста %.0f\n", payments[0].Amount, det)
			payments[0].Amount = det
		}
	}

	// Вопрос-уточнение задаём, только если платежей нет и его задало БОЛЬШИНСТВО
	// чтений (иначе одно «неуверенное» чтение сыпало бы лишние вопросы).
	if len(payments) == 0 {
		if q := majorityString(clarifies, okRuns); q != "" {
			b.sendText(chat, "❓ "+q)
		}
	}

	saved := 0
	insertFailed := false
	for _, p := range payments {
		name := strings.TrimSpace(p.Name)
		if p.Amount <= 0 || name == "" {
			continue
		}
		canonical, _ := b.aliases.ResolveName(name)
		contactID, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			fmt.Println("ИИ-доразбор: ошибка контакта:", err)
			insertFailed = true
			continue
		}
		isCash := p.Cash || cashHint || parser.IsCash(strings.Join(lines, " ")+" "+p.Note+" "+p.Card)
		card := p.Card
		if isCash && card == "" {
			card = "наличные"
		}

		err = b.db.InsertTransaction(ctx, db.TransactionInput{
			ContactID:    contactID,
			RawName:      name,
			Amount:       p.Amount,
			Note:         p.Note,
			CardTo:       card,
			IsCash:       isCash,
			Collector:    strings.TrimSpace(p.Collector),
			RawMessageID: rawID,
			TxDate:       txDate,
			DupCheck:     b.cashDupCheckOn(),
		})
		if err != nil {
			fmt.Println("ИИ-доразбор: ошибка сохранения транзакции:", err)
			insertFailed = true
			continue
		}
		saved++
	}
	if saved > 0 {
		fmt.Printf("ИИ-доразбор: сообщение %d — извлечено и сохранено %d платеж(ей), которые не понял обычный парсер\n", rawID, saved)
	} else if insertFailed {
		// ИИ ПОНЯЛ платежи, но ни один не записался (БД недоступна). Не теряем:
		// оставляем на пересчёт. Детерминированно не дублируем — ИИ уже всё понял.
		keepUnparsed = true
	} else {
		// ИИ был ДОСТУПЕН и вернул ПУСТО — это осознанный вердикт «не платёж»
		// (обсуждение/план: «сказал взять 5000»). Доверяем ему и НЕ пишем мусор.
		// Подстраховка только для платежей ИЗВЕСТНЫМ клиентам (совпал алиас),
		// которых ИИ мог ошибочно пропустить: болтовню это не заденет.
		dsaved, dfailed := b.recordDeterministicPayments(ctx, joined, rawID, txDate, cashHint, true)
		keepUnparsed = dsaved == 0 && dfailed
	}
}

// reMoneyToken — числовые токены в тексте: «10.000», «72,600», «30 000», «31».
var reMoneyToken = regexp.MustCompile(`\d[\d.,]*(?:[ \x{00a0}]\d{3})*`)

// reDateLikeToken — «08.09», «8.9.26», «08.09.2026»: это дата, а не сумма.
var reDateLikeToken = regexp.MustCompile(`^\d{1,2}\.\d{1,2}(?:\.\d{2,4})?$`)

// reThousandsSuffix — суффикс тысяч/миллионов сразу после числа («31 т», «5к»,
// «3 млн», «25 тысяч», «2 ляма», «5 косарей»). Хвосты склонений [а-яё]* — как в
// parser.reShorthand (без них «тысяч/косарей/ляма» не распознавались). За
// суффиксом не должна идти буква («т.е.» — не тысячи).
var reThousandsSuffix = regexp.MustCompile(`(?i)^\s*(кк|к|тыщ[а-яё]*|тыс[а-яё]*|т|млн|лям[а-яё]*|косар[а-яё]*)(?:$|\s|[^\p{L}\d\s](?:[^\p{L}]|$))`)

// looksLikeClockToken — токен из 1–2 цифр вплотную к двоеточию (часы/минуты в
// «12:30»). Так «12»/«30» из времени не считаются суммой, а «35.000» из
// «наличными:35.000» (не 1–2 цифры) — считается.
func looksLikeClockToken(text string, start, end int, tok string) bool {
	if len(tok) < 1 || len(tok) > 2 {
		return false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	if end < len(text) && text[end] == ':' {
		return true
	}
	if start > 0 && text[start-1] == ':' {
		return true
	}
	return false
}

// moneyTokens — «денежные» числа сообщения: с суффиксом тысяч/миллионов ИЛИ не
// меньше 100 (и не похожие на дату/время). По их количеству решаем, ОДНА ли
// сумма в сообщении: только тогда её можно безопасно навязать ИИ и подменить ею
// результат; при нескольких числах («20000 + 5000») подмена ломала бы итог.
func moneyTokens(text string) []float64 {
	var out []float64
	for _, m := range reMoneyToken.FindAllStringIndex(text, -1) {
		tok := strings.Trim(text[m[0]:m[1]], ".,")
		if tok == "" {
			continue
		}
		// Суффикс проверяем ПЕРВЫМ: «1.5 млн» — сумма, а не дата; «22т» — тысячи.
		suffix := reThousandsSuffix.FindStringSubmatch(text[m[1]:])
		if suffix == nil && reDateLikeToken.MatchString(tok) {
			continue // «08.09» без суффикса — дата
		}
		if suffix == nil && looksLikeClockToken(text, m[0], m[1], tok) {
			continue // «12:30» — время
		}
		v := parser.ParseMoneyValue(tok)
		if v <= 0 {
			continue
		}
		if suffix != nil {
			switch s := strings.ToLower(suffix[1]); {
			case s == "кк" || strings.HasPrefix(s, "млн") || strings.HasPrefix(s, "лям"):
				v *= 1_000_000
			default: // к, т, тыс, тыщ, косар
				v *= 1000
			}
		} else if v < 100 {
			continue // мелкое число без суффикса — вряд ли сумма (номер, «за 2 месяца»)
		}
		out = append(out, v)
	}
	return out
}

// paymentReads — сколько независимых прочтений ТЕКСТОВОГО платежа делать
// (само-согласованность): по умолчанию 3. PAYMENT_READS=1 отключает консенсус.
func paymentReads() int {
	if v := strings.TrimSpace(os.Getenv("PAYMENT_READS")); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 1 && k <= 5 {
			return k
		}
	}
	return 3
}

// consensusPayments сводит несколько независимых прочтений платежей в один
// согласованный список. Платежи группируются по каноничному имени клиента;
// принимается платёж, встретившийся в БОЛЬШИНСТВЕ чтений (это отсеивает разовые
// галлюцинации), сумма берётся медианой (устойчива к одному выбросу вроде
// «10.000»→5000), признак наличка/перевод — строгим большинством голосов.
func consensusPayments(runs [][]aiPayment, okRuns int) []aiPayment {
	if okRuns <= 1 {
		if len(runs) > 0 {
			return runs[0]
		}
		return nil
	}
	type agg struct {
		name      string
		amounts   []float64
		cashVotes int
		total     int // в скольких ЧТЕНИЯХ встретился (ключ уникален в пределах чтения)
		collector map[string]int
		card      map[string]int
		note      string
	}
	byName := map[string]*agg{}
	var order []string
	for _, run := range runs {
		// Одно имя ДВАЖДЫ в одном чтении («Ахмед 5000 / Ахмед 10000») — это два
		// платежа: нумеруем их по возрастанию суммы, чтобы «#0» и «#1» совпадали
		// между чтениями. Раньше оба сливались в один и «встречались 2 раза» в
		// одном чтении — ложное большинство, и сумма — медиана двух РАЗНЫХ платежей.
		sorted := append([]aiPayment(nil), run...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Amount < sorted[j].Amount })
		ordinal := map[string]int{}
		for _, p := range sorted {
			name := strings.TrimSpace(p.Name)
			if p.Amount <= 0 || name == "" {
				continue
			}
			nameKey := strings.ToLower(strings.Join(strings.Fields(name), " "))
			key := nameKey + "#" + strconv.Itoa(ordinal[nameKey])
			ordinal[nameKey]++
			a := byName[key]
			if a == nil {
				a = &agg{name: name, collector: map[string]int{}, card: map[string]int{}}
				byName[key] = a
				order = append(order, key)
			}
			a.amounts = append(a.amounts, p.Amount)
			if p.Cash {
				a.cashVotes++
			}
			a.total++
			if c := strings.TrimSpace(p.Collector); c != "" {
				a.collector[c]++
			}
			if p.Card != "" {
				a.card[p.Card]++
			}
			if a.note == "" {
				a.note = p.Note
			}
		}
	}
	maj := okRuns/2 + 1 // строгое большинство прочтений
	var out []aiPayment
	for _, key := range order {
		a := byName[key]
		if a.total < maj {
			continue // встретился меньше чем в большинстве чтений — вероятно, ошибка
		}
		out = append(out, aiPayment{
			Name:      a.name,
			Amount:    medianFloat(a.amounts),
			Cash:      a.cashVotes*2 > a.total, // строгое большинство голосов «наличка»
			Collector: mostCommonKey(a.collector),
			Card:      mostCommonKey(a.card),
			Note:      a.note,
		})
	}
	return out
}

// medianFloat — медиана (нижняя середина), устойчивая к одному выбросу.
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[(len(s)-1)/2]
}

// mostCommonKey — самый частый ключ в счётчике ("" если пусто).
func mostCommonKey(m map[string]int) string {
	best, bestN := "", 0
	for k, c := range m {
		if c > bestN {
			best, bestN = k, c
		}
	}
	return best
}

// majorityString — строка, встретившаяся в БОЛЬШИНСТВЕ из total (иначе "").
func majorityString(vals []string, total int) string {
	counts := map[string]int{}
	for _, v := range vals {
		counts[v]++
	}
	for v, c := range counts {
		if c*2 > total {
			return v
		}
	}
	return ""
}

type aiReceipt struct {
	Kind           string  `json:"kind"`            // "receipt" (чек), "cash" (фото наличных), "other" (не чек)
	Bank           string  `json:"bank"`            // банк чека (сторона получателя)
	Recipient      string  `json:"recipient"`       // ФИО получателя
	RecipientBank  string  `json:"recipient_bank"`  // банк получателя, если указан отдельно
	RecipientPhone string  `json:"recipient_phone"` // телефон получателя
	Sender         string  `json:"sender"`          // ФИО отправителя (плательщик, напечатан на чеке)
	SenderBank     string  `json:"sender_bank"`     // банк отправителя
	SenderAccount  string  `json:"sender_account"`  // счёт/карта отправителя
	Amount         float64 `json:"amount"`
	Commission     float64 `json:"commission"`
	DocNumber      string  `json:"doc_number"`
	AuthCode       string  `json:"auth_code"`
	Status         string  `json:"status"`
	Datetime       string  `json:"datetime"` // "YYYY-MM-DD HH:MM:SS", "YYYY-MM-DD HH:MM" или ""
}

// receiptSchemaJSON — форма JSON для ИИ-разбора чека (полный набор полей).
const receiptSchemaJSON = `{"kind":"receipt","bank":"","recipient":"","recipient_bank":"","recipient_phone":"","sender":"","sender_bank":"","sender_account":"","amount":0,"commission":0,"doc_number":"","auth_code":"","status":"","datetime":""}`

// receiptExtractRules — общие правила извлечения полей чека для ИИ.
const receiptExtractRules = "kind — что на изображении: 'receipt' (банковский чек/квитанция перевода), 'cash' (ФОТО НАЛИЧНЫХ ДЕНЕГ — купюры в руке/на столе, а не чек), 'other' (что-то иное). " +
	"recipient — ФИО ПОЛУЧАТЕЛЯ перевода (кому/на чью карту пришли деньги — владелец карты). " +
	"sender — ФИО отправителя/плательщика, КАК НАПЕЧАТАНО НА ЧЕКЕ. ВАЖНО: напечатанный на чеке отправитель — это НЕ обязательно клиент рассрочки (часто платят с чужой карты). Клиента определяет бот по подписи рядом, а не ты — просто верни, что напечатано. " +
	"recipient_bank/sender_bank — банки сторон, если указаны (например 'Банк получателя: Т-Банк'). " +
	"recipient_phone — телефон получателя, sender_account — счёт/карта отправителя (последние цифры). " +
	"amount — ГЛАВНАЯ сумма перевода числом в рублях, БЕЗ комиссии. Это САМОЕ КРУПНОЕ, ВЫДЕЛЕННОЕ число вверху чека, " +
	"обычно у слов 'Итого', 'Сумма перевода', 'Сумма операции', 'Сумма'. Категорически НЕ бери: остаток/баланс на карте, " +
	"комиссию/'Без комиссии 0', номер карты/счёта, номер операции, телефон, дату. Если сомневаешься между двумя числами — " +
	"сумма перевода это та, что рядом с 'Итого/Сумма', а не мелкие числа. ПЕРЕПРОВЕРЬ каждую цифру суммы по картинке " +
	"(например «10 000», а не «5 000»; не теряй и не добавляй нули и разряды) — сумма самое важное, ошибаться в ней нельзя. " +
	"commission — комиссия числом. " +
	"datetime — дата и время операции с чека в формате YYYY-MM-DD HH:MM:SS (или YYYY-MM-DD HH:MM). " +
	"doc_number — номер документа/операции, auth_code — код авторизации, status — статус ('Выполнено' и т.п.). " +
	"Читай ВНИМАТЕЛЬНО, даже если фото размытое, под углом, тёмное или это скан — разбери, что можешь. " +
	"Кириллицу и цифры не путай (0/О, 3/З, 6/б, 1/l). Заполняй ВСЕ поля, которые видишь; чего не видно — оставь пустым " +
	"(числа 0). Лучше оставить поле пустым, чем выдумать. Не придумывай данные, которых нет на изображении."

// aiRescueReceipt отдаёт OCR-текст чека модели, когда обычный парсер не смог
// вытащить сумму или получателя (нестандартная вёрстка, кривой OCR).
func (b *Bot) aiRescueReceipt(ctx context.Context, ocrText string) (aiReceipt, bool) {
	system := "Ты — модуль разбора банковских чеков в WhatsApp-боте учёта финансов. " +
		"Тебе дают текст, распознанный OCR со скриншота банковского перевода (текст может быть с ошибками распознавания). " +
		"Верни СТРОГО один JSON-объект вида " + receiptSchemaJSON + ". " + receiptExtractRules

	out, err := b.assistant.Complete(ctx, system, ocrText)
	if err != nil {
		fmt.Println("ИИ-доразбор чека не удался:", err)
		return aiReceipt{}, false
	}
	block := extractJSONBlock(out)
	if block == "" {
		return aiReceipt{}, false
	}
	var rec aiReceipt
	if err := json.Unmarshal([]byte(block), &rec); err != nil {
		fmt.Printf("ИИ-доразбор чека: не удалось разобрать JSON (%v): %s\n", err, block)
		return aiReceipt{}, false
	}
	if rec.Amount <= 0 && strings.TrimSpace(rec.Recipient) == "" {
		return aiReceipt{}, false // модель тоже ничего не нашла
	}
	return rec, true
}

// aiVisionReceipt показывает файл чека (фото или PDF) модели "глазами" —
// последний рубеж распознавания, когда OCR выдал кашу или вообще ничего.
// Claude читает чек прямо с изображения: банк, получатель, сумма, дата.
// Возвращает (результат, ok, reachable). reachable=false означает, что модель
// зрения НЕ дала вердикт (нет ассистента, не отрендерился PDF, ошибка вызова) —
// тогда нельзя заключать «это не чек»: вызывающий не должен помечать сообщение
// обработанным, иначе реальный чек потеряется при сбое API. reachable=true —
// модель ответила (в т.ч. «это не чек / other»), вердикту можно верить.
func (b *Bot) aiVisionReceipt(ctx context.Context, media []byte, ext, hint string) (aiReceipt, bool, bool) {
	if b.assistant == nil || len(media) == 0 {
		return aiReceipt{}, false, false
	}

	img := media
	mime := "image/jpeg"
	if ext == ".pdf" {
		rendered, err := renderPDFFirstPage(ctx, media)
		if err != nil {
			fmt.Println("Вижн-разбор: не удалось отрендерить PDF:", err)
			return aiReceipt{}, false, false
		}
		img, mime = rendered, "image/png"
	} else if len(img) >= 8 && string(img[:4]) == "\x89PNG" {
		mime = "image/png"
	}

	system := "Ты — модуль разбора изображений в боте учёта финансов. Тебе показывают ИЗОБРАЖЕНИЕ: обычно это чек/скриншот " +
		"банковского перевода, но иногда — ФОТО НАЛИЧНЫХ ДЕНЕГ (пачка купюр в руке/на столе). " +
		"Внимательно посмотри и верни СТРОГО один JSON-объект вида " + receiptSchemaJSON + ". " + receiptExtractRules +
		" Если это фото наличных денег — kind='cash' (сумму заполни, только если она явно видна/подписана, иначе 0). " +
		"Если это банковский чек — kind='receipt'. Если ни то ни другое — kind='other' и остальные поля пустыми."

	userText := "Что на изображении? Верни JSON."
	if h := strings.TrimSpace(hint); h != "" {
		// OCR-текст этого же чека как ПОДСКАЗКА (может быть с ошибками — верь
		// картинке больше, но используй его, чтобы сверить цифры и ФИО).
		if len([]rune(h)) > 1500 {
			h = string([]rune(h)[:1500])
		}
		userText += "\n\nДля сверки — что распозналось OCR с этого чека (может быть с ошибками, доверяй изображению больше): " + h
	}
	out, err := b.assistant.CompleteWithImage(ctx, system, userText, img, mime)
	if err != nil {
		fmt.Println("Вижн-разбор чека не удался:", err)
		return aiReceipt{}, false, false // модель недоступна — вердикта нет
	}
	// Дальше модель ОТВЕТИЛА — вердикту можно верить (reachable=true), даже если
	// она сказала «это не чек» или вернула кашу.
	block := extractJSONBlock(out)
	if block == "" {
		return aiReceipt{}, false, true
	}
	var rec aiReceipt
	if err := json.Unmarshal([]byte(block), &rec); err != nil {
		fmt.Printf("Вижн-разбор: не удалось разобрать JSON (%v): %s\n", err, block)
		return aiReceipt{}, false, true
	}
	// Фото наличных — это валидный результат (наличка), даже без суммы/получателя.
	if rec.Kind == "cash" {
		fmt.Printf("Вижн-разбор: на фото НАЛИЧНЫЕ деньги (сумма с фото: %.0f)\n", rec.Amount)
		return rec, true, true
	}
	if rec.Amount <= 0 && strings.TrimSpace(rec.Recipient) == "" {
		return aiReceipt{}, false, true
	}
	fmt.Printf("Вижн-разбор: Claude прочитал чек с изображения (получатель %q, сумма %.0f)\n", rec.Recipient, rec.Amount)
	return rec, true, true
}

// receiptVisionReads — сколько независимых прочтений чека делать за один раз
// (само-согласованность): по умолчанию 3. Из них берётся согласованная сумма —
// это отсеивает разовые ошибки распознавания (напр. лишний ноль в одном чтении).
// RECEIPT_VISION_READS=1 отключает (одно чтение).
func receiptVisionReads() int {
	if v := strings.TrimSpace(os.Getenv("RECEIPT_VISION_READS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 5 {
			return n
		}
	}
	return 3
}

// aiVisionReceiptConsensus читает чек несколькими независимыми прогонами вижна
// ПАРАЛЛЕЛЬНО (задержка ≈ одного запроса) и выбирает согласованный результат:
// сумму, которая совпала в большинстве прочтений, а если все разные — медианную
// (устойчивую к одному выбросу). Так разовая ошибка распознавания не проходит.
func (b *Bot) aiVisionReceiptConsensus(ctx context.Context, media []byte, ext, hint string) (aiReceipt, bool, bool) {
	n := receiptVisionReads()
	if n <= 1 {
		return b.aiVisionReceipt(ctx, media, ext, hint)
	}

	type res struct {
		rec       aiReceipt
		ok        bool
		reachable bool
	}
	ch := make(chan res, n)
	for i := 0; i < n; i++ {
		go func() {
			rec, ok, reachable := b.aiVisionReceipt(ctx, media, ext, hint)
			ch <- res{rec, ok, reachable}
		}()
	}

	var recs []aiReceipt
	var cash *aiReceipt
	cashVotes := 0
	anyReachable := false
	for i := 0; i < n; i++ {
		r := <-ch
		if r.reachable {
			anyReachable = true
		}
		if !r.ok {
			continue
		}
		if r.rec.Kind == "cash" {
			c := r.rec
			cash = &c
			cashVotes++
			continue
		}
		recs = append(recs, r.rec)
	}

	// Фото наличных побеждает, только если «наличкой» его назвало СТРОГО больше
	// прочтений, чем «чеком» (и когда чеков не вышло вовсе — тогда len(recs)=0).
	// На РАВЕНСТВЕ голосов доверяем чеку: одно случайное «наличка»-прочтение не
	// должно выкинуть реальный банковский перевод.
	if cash != nil && cashVotes > len(recs) {
		return *cash, true, true
	}
	if len(recs) == 0 {
		return aiReceipt{}, false, anyReachable
	}

	pick := pickConsensusReceipt(recs)
	fmt.Printf("Вижн-консенсус (%d чтений): выбрана сумма %.0f ₽ (получатель %q)\n", n, pick.Amount, pick.Recipient)
	return pick, true, true
}

// pickConsensusReceipt выбирает из нескольких прочтений одно: по согласованной
// сумме (см. consensusAmount). Прочие поля берём из того прочтения, что дало
// выбранную сумму. Прочтения без суммы — как запасной вариант.
func pickConsensusReceipt(recs []aiReceipt) aiReceipt {
	var amounts []float64
	for _, r := range recs {
		if r.Amount > 0 {
			amounts = append(amounts, r.Amount)
		}
	}
	if len(amounts) == 0 {
		return recs[0] // суммы никто не прочитал — вернём первое (получатель и т.п.)
	}
	winner := consensusAmount(amounts)
	// Среди прочтений с выигравшей суммой берём САМОЕ полное (не первое попавшееся),
	// чтобы к верной сумме не прицепился пустой получатель/кривая дата из другого
	// прочтения, случайно совпавшего по сумме. Пустые поля добираем из остальных
	// прочтений с той же суммой.
	best := -1
	bestScore := -1
	for i := range recs {
		if recs[i].Amount != winner {
			continue
		}
		score := 0
		for _, s := range []string{recs[i].Recipient, recs[i].Datetime, recs[i].Sender, recs[i].Bank, recs[i].Status, recs[i].DocNumber} {
			if strings.TrimSpace(s) != "" {
				score++
			}
		}
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if best < 0 {
		return recs[0]
	}
	out := recs[best]
	for i := range recs {
		if recs[i].Amount != winner {
			continue
		}
		if strings.TrimSpace(out.Recipient) == "" {
			out.Recipient = recs[i].Recipient
		}
		if strings.TrimSpace(out.Datetime) == "" {
			out.Datetime = recs[i].Datetime
		}
		if strings.TrimSpace(out.Sender) == "" {
			out.Sender = recs[i].Sender
		}
		if strings.TrimSpace(out.Bank) == "" {
			out.Bank = recs[i].Bank
		}
		if strings.TrimSpace(out.RecipientBank) == "" {
			out.RecipientBank = recs[i].RecipientBank
		}
		if strings.TrimSpace(out.DocNumber) == "" {
			out.DocNumber = recs[i].DocNumber
		}
		if strings.TrimSpace(out.Status) == "" {
			out.Status = recs[i].Status
		}
	}
	return out
}

// consensusAmount возвращает согласованную сумму: если какая-то встречается 2+
// раз — её (большинство); иначе медиану (средняя из отсортированных — отсекает
// один резкий выброс вроде лишнего нуля).
func consensusAmount(amounts []float64) float64 {
	if len(amounts) == 0 {
		return 0
	}
	counts := map[float64]int{}
	for _, a := range amounts {
		counts[a]++
	}
	var best float64
	bestN := 0
	for a, cnt := range counts {
		if cnt > bestN || (cnt == bestN && a < best) {
			best, bestN = a, cnt
		}
	}
	if bestN >= 2 {
		return best
	}
	sorted := append([]float64(nil), amounts...)
	sort.Float64s(sorted)
	// нижне-средний элемент: устойчив к завышающему выбросу (лишний ноль).
	return sorted[(len(sorted)-1)/2]
}

// receiptVisionFirst — читать фото-чеки СРАЗУ глазами Claude, не полагаясь на
// Tesseract-OCR (тот на кириллице часто выдаёт «правдоподобный мусор» с неверной
// суммой, и тогда зрение как фолбэк не включалось). По умолчанию ВКЛЮЧЕНО —
// это заметно точнее. Отключить: RECEIPT_VISION_FIRST=0.
func receiptVisionFirst() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("RECEIPT_VISION_FIRST")))
	if v == "" {
		return true
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// applyAIReceiptAuthoritative заполняет ReceiptData из ответа ИИ КАК ОСНОВНОЙ
// источник (перезаписывает поля) — когда чек прочитан вижном первым.
func applyAIReceiptAuthoritative(rd *parser.ReceiptData, rec aiReceipt) {
	if v := strings.TrimSpace(rec.Bank); v != "" {
		rd.Bank = v
	}
	if v := strings.TrimSpace(rec.Recipient); v != "" {
		rd.Recipient = v
	}
	if v := strings.TrimSpace(rec.RecipientBank); v != "" {
		rd.RecipientBank = v
	}
	if v := strings.TrimSpace(rec.RecipientPhone); v != "" {
		rd.RecipientPhone = v
	}
	if v := strings.TrimSpace(rec.Sender); v != "" {
		rd.Sender = v
	}
	if v := strings.TrimSpace(rec.SenderBank); v != "" {
		rd.SenderBank = v
	}
	if v := strings.TrimSpace(rec.SenderAccount); v != "" {
		rd.SenderAccount = v
	}
	if rec.Amount > 0 {
		rd.Amount = rec.Amount
	}
	if rec.Commission > 0 {
		rd.Commission = rec.Commission
	}
	if v := strings.TrimSpace(rec.DocNumber); v != "" {
		rd.DocNumber = v
	}
	if v := strings.TrimSpace(rec.AuthCode); v != "" {
		rd.AuthCode = v
	}
	if v := strings.TrimSpace(rec.Status); v != "" {
		rd.Status = v
	}
	if t, ok := parseAIDatetime(rec.Datetime); ok {
		rd.TxTime = t
		rd.HasTxTime = true
	}
}

// mergeAIReceipt дополняет ReceiptData недостающими полями из ответа ИИ.
func mergeAIReceipt(rd *parser.ReceiptData, rec aiReceipt) {
	if rd.Bank == "" {
		rd.Bank = rec.Bank
	}
	if rd.Recipient == "" {
		rd.Recipient = strings.TrimSpace(rec.Recipient)
	}
	if rd.Sender == "" {
		rd.Sender = strings.TrimSpace(rec.Sender)
	}
	if rd.RecipientBank == "" {
		rd.RecipientBank = strings.TrimSpace(rec.RecipientBank)
	}
	if rd.RecipientPhone == "" {
		rd.RecipientPhone = strings.TrimSpace(rec.RecipientPhone)
	}
	if rd.SenderBank == "" {
		rd.SenderBank = strings.TrimSpace(rec.SenderBank)
	}
	if rd.SenderAccount == "" {
		rd.SenderAccount = strings.TrimSpace(rec.SenderAccount)
	}
	if rd.Amount == 0 {
		rd.Amount = rec.Amount
	}
	if rd.Commission == 0 {
		rd.Commission = rec.Commission
	}
	if rd.DocNumber == "" {
		rd.DocNumber = rec.DocNumber
	}
	if rd.AuthCode == "" {
		rd.AuthCode = rec.AuthCode
	}
	if rd.Status == "" {
		rd.Status = rec.Status
	}
	if !rd.HasTxTime {
		if t, ok := parseAIDatetime(rec.Datetime); ok {
			rd.TxTime = t
			rd.HasTxTime = true
		}
	}
}

// parseAIDatetime разбирает дату/время из ответа модели.
func parseAIDatetime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Принимаем не только пробел-формат, но и ISO с «T», со смещением/зоной и
	// точечный дд.мм.гггг — модель/OCR часто отдают именно так. Иначе верно
	// прочитанная дата операции отбрасывалась, и чек датировался временем
	// присылки в WhatsApp (уезжал не в тот день/период).
	for _, layout := range []string{
		"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02",
		"2006-01-02T15:04:05", "2006-01-02T15:04",
		time.RFC3339, "2006-01-02T15:04:05Z07:00",
		"02.01.2006 15:04:05", "02.01.2006 15:04", "02.01.2006",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
