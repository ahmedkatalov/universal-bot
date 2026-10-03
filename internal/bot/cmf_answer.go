// Вопрос бота «за кого этот платёж?» по сверке с программой и ответ на него.
//
// Когда клиента по чеку нельзя уверенно найти в программе, бот спрашивает в
// группе (ответом на сам чек). Ответить может ЛЮБОЙ участник — бот спросил
// группу, и чаще всего знает ответ сотрудник, который прислал чек. Ответ —
// номер варианта, ФИО одного из вариантов, ФИО ДРУГОГО клиента (платит
// родственник: «Альмурзаева Марха» за «Альмурзаева Разет»), «нет в программе»
// или «не знаю». Бот понимает это сам (правилами, а если не уверен — ИИ),
// находит клиента в программе и отвечает коротко. Это НЕ разговор с
// ассистентом: ответ на вопрос бота никогда не уходит в болтовню.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

const settingPayerMap = "cmf_payer_map"

// cmfAnswerKind — что человек ответил на вопрос «за кого платёж».
type cmfAnswerKind int

const (
	ansOther        cmfAnswerKind = iota // не ответ на вопрос (болтовня/вопрос боту)
	ansPick                              // выбрал вариант из списка
	ansClient                            // назвал клиента (возможно, не из списка)
	ansNotInProgram                      // клиента нет в программе
	ansUnknown                           // не знает
	ansReject                            // «нет» на единственный вариант
)

type cmfAnswer struct {
	Kind cmfAnswerKind
	Pick int    // для ansPick — индекс в списке вариантов (с 0)
	Name string // для ansClient — названное ФИО
	// Among — для ansClient, если названное подходит к нескольким вариантам.
	Among []int
}

var ordinalWords = map[string]int{
	"первый": 1, "первая": 1, "первое": 1, "первого": 1, "первую": 1, "1й": 1, "1-й": 1, "1-я": 1,
	"второй": 2, "вторая": 2, "второе": 2, "второго": 2, "вторую": 2, "2й": 2, "2-й": 2, "2-я": 2,
	"третий": 3, "третья": 3, "третье": 3, "третьего": 3, "третью": 3, "3й": 3, "3-й": 3, "3-я": 3,
	"четвёртый": 4, "четвертый": 4, "четвёртая": 4, "четвертая": 4, "4й": 4, "4-й": 4, "4-я": 4,
	"пятый": 5, "пятая": 5, "5й": 5, "5-й": 5, "5-я": 5,
}

// Явные «нет в программе» — понимаем, даже если вокруг пара слов («его нет в
// программе», «её нет в базе»), но только в коротком ответе без цифр и без ФИО.
var notInProgramStrong = []string{
	"нет в программе", "нету в программе", "не в программе", "нет в базе", "нету в базе",
	"не внесен в программу", "не внесён в программу", "нет такого клиента", "нету такого клиента",
	"не наш клиент", "нет в списке", "нету в списке",
}

// Короткие «нет такого» — только если это ВЕСЬ ответ (иначе «его нет на месте»,
// «Новый клиент Хасанов Ислам 5000» приняли бы за ответ).
var notInProgramWhole = map[string]bool{
	"нет такого": true, "нету такого": true, "нет такой": true, "нету такой": true,
	"его нет": true, "её нет": true, "ее нет": true, "нету его": true, "нету её": true, "нету ее": true,
	"новый клиент": true, "новая клиентка": true, "не клиент": true, "нету": true, "нет его": true, "нет её": true,
	"никто из них": true, "ни один из них": true, "ни одна из них": true, "никто": true,
	"нет его в программе": true, "нет её в программе": true,
}

// fillerWords — слова, которые в ответе не делают его ФИО («это», «за», «её»…).
var fillerWords = map[string]bool{
	"это": true, "его": true, "её": true, "ее": true, "в": true, "у": true, "нас": true, "тут": true,
	"такой": true, "такого": true, "клиент": true, "клиента": true, "клиентка": true, "программе": true,
	"базе": true, "списке": true, "нет": true, "нету": true, "не": true, "наш": true, "внесен": true,
	"внесён": true, "программу": true, "он": true, "она": true, "там": true,
}

// unclearWords — если они есть в ответе, правила не берутся решать: приветствие,
// «уточню у …», обращение к коллеге, «ни … ни …», «вариант» без номера — пусть
// решает ИИ (а без ИИ — переспросим, ничего не привязывая).
var unclearWords = map[string]bool{
	"никто": true, "никого": true, "ни": true, "них": true, "из": true, "одна": true, "один": true,
	"вариант": true, "варианта": true, "новая": true, "новый": true, "клиентка": true,
	"салам": true, "салям": true, "ассаламу": true, "ассалам": true, "алейкум": true, "алейкуму": true,
	"привет": true, "здравствуйте": true, "добрый": true, "доброе": true, "день": true, "утро": true, "вечер": true,
	"уточню": true, "спрошу": true, "узнаю": true, "щас": true, "сейчас": true, "потом": true, "позже": true,
	"ок": true, "окей": true, "спасибо": true, "пожалуйста": true, "подождите": true, "подожди": true,
	"минуту": true, "минутку": true, "секунду": true, "дай": true, "дайте": true,
	"посмотри": true, "посмотрите": true, "глянь": true, "гляньте": true, "смотри": true, "спроси": true,
	"спросите": true, "скажи": true, "скажите": true, "знает": true, "знаю": true, "ответь": true,
	"ответьте": true, "проверь": true, "проверьте": true, "уточни": true, "уточните": true,
	"подскажи": true, "подскажите": true, "напиши": true, "напишите": true, "кинь": true, "позвони": true,
	"видела": true, "видел": true, "есть": true, "фамилия": true, "другая": true, "другой": true,
	"баркалла": true, "дела": true, "реза": true, "хуьлда": true, "хьуна": true, "хьо": true, "мила": true, "ву": true,
	"номер": true, "платит": true, "платила": true, "платил": true,
}

// ackWords — подтверждение/спасибо/отмашка: на сообщение бота это не ответ, а
// «принято» — бот молчит.
var ackWords = map[string]bool{
	"ок": true, "окей": true, "ok": true, "спасибо": true, "спс": true, "благодарю": true, "понял": true,
	"поняла": true, "хорошо": true, "хор": true, "ясно": true, "принято": true, "принял": true, "приняла": true,
	"отлично": true, "супер": true, "+": true, "баркалла": true, "дела реза хуьлда": true, "баркалла хьуна": true,
	"ага": true, "угу": true, "да": true, "верно": true, "точно": true, "правильно": true, "давай": true, "ладно": true,
}

var yesWords = map[string]bool{
	"да": true, "ага": true, "угу": true, "он": true, "она": true, "верно": true, "точно": true,
	"это он": true, "это она": true, "да он": true, "да она": true, "да это он": true, "да это она": true,
	"да, он": true, "да, она": true, "правильно": true, "так": true, "именно": true, "да, это он": true, "да, это она": true,
}

var rejectWords = map[string]bool{
	"нет": true, "не он": true, "не она": true, "нет, не он": true, "нет, не она": true, "неа": true, "нет не он": true,
	"нет не она": true, "не за неё": true, "не за нее": true, "не за него": true, "нет не за неё": true,
	"нет не за нее": true, "нет не за него": true, "нет, не за неё": true, "нет, не за него": true, "не так": true,
}

// cardinalWords — номер варианта словом («номер два», «два»).
var cardinalWords = map[string]int{
	"один": 1, "одна": 1, "одну": 1, "два": 2, "две": 2, "три": 3, "четыре": 4, "пять": 5,
}

// keycapDigits — «2️⃣» → «2».
var keycapRepl = strings.NewReplacer("\uFE0F", "", "\u20E3", "")

func cleanAnswer(text string) string {
	low := strings.ToLower(strings.TrimSpace(keycapRepl.Replace(text)))
	low = strings.Trim(low, ".,!)👍✅ \t")
	return strings.Join(strings.Fields(low), " ")
}

func answerTokens(low string) []string {
	return strings.FieldsFunc(low, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' })
}

func hasNegation(tokens []string) bool {
	for _, t := range tokens {
		switch t {
		case "не", "нет", "неа", "ни", "нету":
			return true
		}
	}
	return false
}

// isAck — ответ-«принято» (спасибо, ок, 👍, «+», «да») или пустой без букв и цифр.
func isAck(text string) bool {
	low := cleanAnswer(text)
	if ackWords[low] {
		return true
	}
	return strings.IndexFunc(low, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0
}

// pickNumber — номер варианта из ответа: «2», «№2», «(2)», «2🙏», «номер два»,
// «под номером 2», «2 вариант». ok=false — номера нет; n может быть вне списка.
func pickNumber(low string) (n int, ok bool) {
	toks := answerTokens(low)
	if strings.IndexFunc(low, unicode.IsLetter) < 0 {
		var digits []string
		for _, t := range toks {
			if _, err := strconv.Atoi(t); err == nil {
				digits = append(digits, t)
			}
		}
		if len(digits) == 1 {
			n, _ := strconv.Atoi(digits[0])
			return n, true
		}
		return 0, false
	}
	for len(toks) > 0 {
		switch toks[0] {
		case "номер", "под", "номером", "вариант":
			toks = toks[1:]
			continue
		}
		break
	}
	if len(toks) == 2 && toks[1] == "вариант" {
		toks = toks[:1]
	}
	if len(toks) != 1 {
		return 0, false
	}
	if n, err := strconv.Atoi(toks[0]); err == nil {
		return n, true
	}
	if n, ok := cardinalWords[toks[0]]; ok {
		return n, true
	}
	return 0, false
}

// surnameTails — окончания фамилий (и их падежей): по ним видно, что в ответе ФИО.
var surnameTails = []string{"ов", "ев", "ёв", "ин", "ын", "ова", "ева", "ёва", "ина", "ына", "ову", "еву", "ину",
	"овой", "евой", "иной", "ым", "ым", "ого", "ой", "ая", "ий", "ский", "ская", "цкий", "цкая", "ян", "дзе", "швили",
	"ко", "ук", "юк", "ых", "их", "аев", "иев", "аева", "иева", "аеву", "иеву", "аевой", "иевой"}

// looksLikeFIO — похоже ли на ФИО человека (а не фразу): есть слово с
// окончанием фамилии или слово из имени одного из вариантов, и нет служебных слов.
func looksLikeFIO(name string, cands []cmf.ClientInfo) bool {
	full, _ := nameParts(name)
	if len(full) == 0 {
		return false
	}
	for _, w := range full {
		if unclearWords[w] || fillerWords[w] || nameStopwords[w] {
			return false
		}
	}
	for _, w := range full {
		if len([]rune(w)) >= 5 {
			for _, t := range surnameTails {
				if strings.HasSuffix(w, t) {
					return true
				}
			}
		}
		for _, c := range cands {
			for _, v := range normWords(c.FullName) {
				if nameWordSame(w, v) {
					return true
				}
			}
		}
	}
	return false
}

// stripFillers — ответ без служебных слов и предлогов («это Марха», «за Марху»).
func stripFillers(low string) string {
	var out []string
	for _, t := range answerTokens(low) {
		switch t {
		case "за", "для", "это", "его", "её", "ее", "клиент", "клиентка", "клиента", "платёж", "платеж", "оплата", "оплату":
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

// parseCmfAnswer — разбор ответа правилами. confident=false — правила не
// уверены (фраза, вопрос, отрицание, незнакомое слово): пусть решит ИИ, а без
// ИИ — переспросим, но НИЧЕГО не привяжем. caption — имя плательщика с чека:
// повторить его — не значит выбрать вариант.
func parseCmfAnswer(text, caption string, cands []cmf.ClientInfo) (ans cmfAnswer, confident bool) {
	low := cleanAnswer(text)
	if low == "" {
		return cmfAnswer{Kind: ansOther}, true
	}
	if strings.Contains(text, "?") {
		return cmfAnswer{Kind: ansOther}, false
	}
	toks := answerTokens(low)
	hasDigit := strings.IndexFunc(low, unicode.IsDigit) >= 0
	// «нет в программе».
	if notInProgramWhole[low] {
		return cmfAnswer{Kind: ansNotInProgram}, true
	}
	for _, m := range notInProgramStrong {
		if !strings.Contains(low, m) {
			continue
		}
		if hasDigit || len(toks) > 6 {
			return cmfAnswer{Kind: ansOther}, false
		}
		for _, t := range toks {
			if !fillerWords[t] && len([]rune(t)) >= 3 && !strings.Contains(m, t) {
				return cmfAnswer{Kind: ansOther}, false // рядом названо что-то ещё (ФИО?) — пусть решит ИИ
			}
		}
		return cmfAnswer{Kind: ansNotInProgram}, true
	}
	if isUnknownReply(low) {
		// «не знаю» — только если больше ничего не названо («не знаю их, это за X» — нет).
		for _, t := range toks {
			switch t {
			case "не", "знаю", "незнаю", "хз", "помню", "понял", "в", "курсе", "без", "понятия", "их", "его", "её", "ее",
				"кто", "это", "пока", "я", "тоже":
			default:
				return cmfAnswer{Kind: ansOther}, false
			}
		}
		return cmfAnswer{Kind: ansUnknown}, true
	}
	if len(cands) == 1 {
		if yesWords[low] {
			return cmfAnswer{Kind: ansPick, Pick: 0}, true
		}
		if rejectWords[low] {
			return cmfAnswer{Kind: ansReject}, true
		}
	}
	// Номер варианта.
	if n, ok := pickNumber(low); ok {
		if n >= 1 && n <= len(cands) {
			return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
		}
		return cmfAnswer{Kind: ansOther}, true
	}
	otoks := toks
	for len(otoks) > 1 && (otoks[0] == "номер" || otoks[0] == "под" || otoks[0] == "номером") {
		otoks = otoks[1:]
	}
	if len(cands) > 0 && len(otoks) > 0 {
		if n, ok := ordinalWords[otoks[0]]; ok && n <= len(cands) {
			var named []string
			for _, t := range otoks[1:] {
				switch t {
				case "вариант", "номер", "это", "он", "она":
				default:
					named = append(named, t)
				}
			}
			if len(named) == 0 {
				return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
			}
			for _, k := range matchCandidates(strings.Join(named, " "), cands) {
				if k == n-1 {
					return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
				}
			}
			return cmfAnswer{Kind: ansOther}, false
		}
	}
	// Голые цифры/сумма — это не ФИО.
	if strings.IndexFunc(low, unicode.IsLetter) < 0 {
		return cmfAnswer{Kind: ansOther}, true
	}
	// Отрицание («не Мусиева», «нет, это Альмурзаева») и фразы — решает ИИ.
	if hasNegation(toks) {
		return cmfAnswer{Kind: ansOther}, false
	}
	for _, t := range toks {
		if unclearWords[t] {
			return cmfAnswer{Kind: ansOther}, false
		}
	}
	if hasDigit {
		return cmfAnswer{Kind: ansOther}, false
	}
	core := stripFillers(low)
	if core == "" {
		return cmfAnswer{Kind: ansOther}, false
	}
	// Повторили имя плательщика с чека («Марха», «это Марха», «за Марху») — не выбор.
	if caption != "" && nameCovers(core, caption) {
		return cmfAnswer{Kind: ansOther}, false
	}
	// Короткий ответ словами одного из вариантов («сайдаевой», «мусиева марха»).
	if len(answerTokens(core)) <= 3 {
		if idx := matchCandidates(core, cands); len(idx) == 1 {
			return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
		} else if len(idx) > 1 {
			return cmfAnswer{Kind: ansClient, Name: strings.TrimSpace(text), Among: idx}, true
		}
	}
	name, amount, deferToAI := clarifyNameFromReply(text)
	if deferToAI || amount > 0 || name == "" {
		return cmfAnswer{Kind: ansOther}, false
	}
	if caption != "" && nameCovers(stripFillers(strings.ToLower(name)), caption) {
		return cmfAnswer{Kind: ansOther}, false
	}
	if idx := matchCandidates(name, cands); len(idx) == 1 {
		return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
	} else if len(idx) > 1 {
		return cmfAnswer{Kind: ansClient, Name: name, Among: idx}, true
	}
	// Уверенно — только полноценное ФИО (2+ слова, похоже на имя человека).
	if fullWordCount(name) < 2 || !looksLikeFIO(name, cands) {
		return cmfAnswer{Kind: ansClient, Name: name}, false
	}
	return cmfAnswer{Kind: ansClient, Name: name}, true
}

// matchCandidates — варианты, которые покрывают названное имя (все слова
// ответа есть в имени варианта, инициалы не спорят): «Сайдаева» → «Сайдаева
// Марха …», «Альмурзаева Р.» → «Альмурзаева Разет …».
func matchCandidates(name string, cands []cmf.ClientInfo) []int {
	var out []int
	for k, c := range cands {
		if nameCovers(name, c.FullName) {
			out = append(out, k)
		}
	}
	return out
}

// cmfAnswerByAI — ИИ решает по смыслу, что ответили на вопрос бота.
func (b *Bot) cmfAnswerByAI(ctx context.Context, question, text string, cands []cmf.ClientInfo) (cmfAnswer, bool) {
	if b.assistant == nil {
		return cmfAnswer{}, false
	}
	var list []string
	for k, l := range optionLabels(cands) {
		list = append(list, fmt.Sprintf("%d. %s", k+1, l))
	}
	if len(list) == 0 {
		list = []string{"(вариантов не было — бот просил написать ФИО)"}
	}
	system := "Бот спросил в рабочей группе, за какого клиента программы рассрочек этот платёж по чеку. " +
		"Сотрудник ответил. Пойми ответ по смыслу (бывают опечатки, сленг, чеченские слова, падежи). " +
		"Верни СТРОГО JSON {\"kind\":\"pick|client|not_in_program|unknown|other\",\"pick\":номер варианта или 0,\"name\":\"ФИО в именительном падеже или пусто\"}. " +
		"pick — выбрал вариант из списка (номером, именем, фамилией, «да/он» при одном варианте); " +
		"client — назвал ФИО клиента (в т.ч. не из списка, например родственника, за которого платят); " +
		"not_in_program — клиента нет в программе/новый клиент; unknown — не знает; " +
		"other — это не ответ на вопрос (приветствие, отмашка, вопрос боту, болтовня, «сейчас уточню»). " +
		"ФИО в ответе — это КЛИЕНТ, а не имя сотрудника. Отрицание важно: «не Мусиева» — НЕ выбор Мусиевой; " +
		"«нет такого, это Дудаева Разет» — client «Дудаева Разет». «никто из них» без ФИО — not_in_program."
	user := question + "\nВарианты (нумерация как в группе):\n" + strings.Join(list, "\n") + "\nОтвет сотрудника: " + text
	out, err := b.assistant.Complete(ctx, system, user)
	if err != nil {
		fmt.Println("cmf: разбор ответа ИИ не удался:", err)
		return cmfAnswer{}, false
	}
	var parsed struct {
		Kind string `json:"kind"`
		Pick int    `json:"pick"`
		Name string `json:"name"`
	}
	block := extractJSONBlock(out)
	if block == "" || json.Unmarshal([]byte(block), &parsed) != nil {
		return cmfAnswer{}, false
	}
	switch parsed.Kind {
	case "pick":
		if parsed.Pick >= 1 && parsed.Pick <= len(cands) {
			return cmfAnswer{Kind: ansPick, Pick: parsed.Pick - 1}, true
		}
	case "client":
		if n := strings.TrimSpace(parsed.Name); n != "" {
			if idx := matchCandidates(n, cands); len(idx) == 1 {
				return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
			}
			return cmfAnswer{Kind: ansClient, Name: n}, true
		}
	case "not_in_program":
		return cmfAnswer{Kind: ansNotInProgram}, true
	case "unknown":
		return cmfAnswer{Kind: ansUnknown}, true
	case "other":
		return cmfAnswer{Kind: ansOther}, true
	}
	return cmfAnswer{}, false
}

// --- текст вопроса ---

// candInfo — вариант и то, чем он похож на имя с чека.
type candInfo struct {
	c         cmf.ClientInfo
	matched   int  // сколько слов имени с чека нашлось у варианта
	surname   bool // совпала ФАМИЛИЯ варианта (первое слово в программе) — он сам или родственник
	fullMatch bool // вариант покрывает всё имя с чека (и инициалы не спорят)
	initialOK bool // инициалы с чека не противоречат варианту
}

func analyzeCands(query string, cands []cmf.ClientInfo) []candInfo {
	fq, iq := nameParts(query)
	out := make([]candInfo, 0, len(cands))
	for _, c := range cands {
		fc, ic := nameParts(c.FullName)
		score, _, uc := matchNameWords(fq, fc)
		ci := candInfo{c: c, matched: score}
		ci.surname = len(uc) > 0 && uc[0]
		ci.initialOK = initialsAgree(iq, leftovers(fc, uc, ic))
		ci.fullMatch = len(fq) > 0 && score == len(fq) && ci.initialOK
		out = append(out, ci)
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.fullMatch != y.fullMatch {
			return x.fullMatch
		}
		if x.surname != y.surname {
			return x.surname
		}
		if x.initialOK != y.initialOK {
			return x.initialOK
		}
		if x.matched != y.matched {
			return x.matched > y.matched
		}
		if x.c.FullName != y.c.FullName {
			return x.c.FullName < y.c.FullName
		}
		return x.c.ID < y.c.ID
	})
	return out
}

func rub0(v float64) string { return formatMoney(v) + " ₽" }

const cmfAskMaxOptions = 5

// optionLabels — как показать варианты: ФИО, а у одинаковых ФИО — хвост
// телефона (или номер карточки), чтобы их можно было различить.
func optionLabels(opts []cmf.ClientInfo) []string {
	count := map[string]int{}
	for _, c := range opts {
		count[normCyr(c.FullName)]++
	}
	out := make([]string, len(opts))
	for k, c := range opts {
		out[k] = c.FullName
		if count[normCyr(c.FullName)] > 1 {
			digits := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, c.Phone)
			if len(digits) >= 4 {
				out[k] += " (тел. …" + digits[len(digits)-4:] + ")"
			} else {
				out[k] += " (карточка " + c.ID + ")"
			}
		}
	}
	return out
}

// cmfAskText — вопрос в группу, когда клиента по чеку нельзя уверенно найти.
// Возвращает текст и варианты в том порядке, в каком они перечислены.
func cmfAskText(clientText string, amount float64, cands []cmf.ClientInfo) (string, []cmf.ClientInfo) {
	head := fmt.Sprintf("🔎 Чек на %s («%s»)", rub0(amount), clientText)
	if amount <= 0 {
		head = fmt.Sprintf("🔎 «%s»", clientText)
	}
	if len(cands) == 0 {
		return head + ": такого клиента в программе не нашёл. За кого этот платёж? Ответьте на это сообщение ФИО клиента (или «нет в программе»).", nil
	}
	all := analyzeCands(clientText, cands)
	// Кандидаты, у которых не совпало ни одно слово (а «похожесть» была только
	// в отчестве), — не варианты: их не предлагаем.
	var info []candInfo
	for _, ci := range all {
		if ci.matched > 0 || ci.surname {
			info = append(info, ci)
		}
	}
	if len(info) == 0 {
		return head + ": такого клиента в программе не нашёл. За кого этот платёж? Ответьте на это сообщение ФИО клиента (или «нет в программе»).", nil
	}
	if len(info) > cmfAskMaxOptions {
		info = info[:cmfAskMaxOptions]
	}
	opts := make([]cmf.ClientInfo, len(info))
	for k, ci := range info {
		opts[k] = ci.c
	}
	labels := optionLabels(opts)
	allFull, anySurname, sameName, nFull := true, false, true, 0
	for _, ci := range info {
		allFull = allFull && ci.fullMatch
		anySurname = anySurname || ci.surname
		sameName = sameName && normCyr(ci.c.FullName) == normCyr(info[0].c.FullName)
		if ci.fullMatch {
			nFull++
		}
	}
	if len(info) == 1 {
		ci := info[0]
		switch {
		case ci.fullMatch:
			return head + fmt.Sprintf(": в программе похоже на «%s». Если это он(а) — ответьте «да»; если нет — напишите ФИО клиента.", labels[0]), opts
		case ci.surname:
			return head + fmt.Sprintf(": такого клиента в программе нет, есть с той же фамилией — «%s». Платёж за него(неё)? Ответьте «да» или ФИО клиента.", labels[0]), opts
		default:
			return head + fmt.Sprintf(": такого клиента в программе нет (только похожее имя — «%s»). За кого этот платёж? Ответьте ФИО клиента (или «да», если за него/неё).", labels[0]), opts
		}
	}
	var sb strings.Builder
	sb.WriteString(head)
	switch {
	case allFull && sameName:
		fmt.Fprintf(&sb, ": в программе %d карточки с таким ФИО:", len(info))
	case allFull:
		sb.WriteString(": в программе несколько подходящих клиентов:")
	case nFull > 0:
		fmt.Fprintf(&sb, ": в программе несколько клиентов «%s»:", clientText)
	case anySurname:
		sb.WriteString(": точно такого клиента в программе нет. Похожие:")
	default:
		sb.WriteString(": такого клиента в программе нет — совпадает только имя:")
	}
	for k, ci := range info {
		fmt.Fprintf(&sb, "\n%d. %s", k+1, labels[k])
		switch {
		case allFull || ci.fullMatch:
		case ci.surname:
			sb.WriteString(" (та же фамилия)")
		case nFull > 0:
			sb.WriteString(" (похожее имя)")
		}
	}
	sb.WriteString("\nЗа кого этот платёж? Ответьте номером или ФИО клиента.")
	return sb.String(), opts
}

// --- решение по ответу ---

type cmfAnswerAction int

const (
	actNone         cmfAnswerAction = iota // не ответ — пусть идёт дальше
	actBind                                // привязать к клиенту
	actAskAgain                            // переспросить с новыми вариантами
	actNotFound                            // названного нет в программе — переспросить
	actNotInProgram                        // клиента нет в программе — закрыть
	actUnknown                             // не знают — оставить
	actReject                              // «нет» на единственный вариант
	actLookupError                         // программа не ответила
)

type cmfAnswerPlan struct {
	Action  cmfAnswerAction
	Client  cmf.ClientInfo
	Options []cmf.ClientInfo
	Name    string
	Err     error
}

// planCmfAnswer — что делать с разобранным ответом (без побочных эффектов).
// Привязываем, только если названное имя ДЕЙСТВИТЕЛЬНО покрывает ровно одного
// клиента программы (все слова, инициалы); одно слово («Разет», «Магомед») —
// только с подтверждением.
func planCmfAnswer(ans cmfAnswer, cands []cmf.ClientInfo, lookup func(string) ([]cmf.ClientInfo, cmfMatchKind, error)) cmfAnswerPlan {
	switch ans.Kind {
	case ansPick:
		if ans.Pick >= 0 && ans.Pick < len(cands) {
			return cmfAnswerPlan{Action: actBind, Client: cands[ans.Pick]}
		}
		return cmfAnswerPlan{Action: actNone}
	case ansNotInProgram:
		return cmfAnswerPlan{Action: actNotInProgram}
	case ansUnknown:
		return cmfAnswerPlan{Action: actUnknown}
	case ansReject:
		return cmfAnswerPlan{Action: actReject}
	case ansClient:
		if len(ans.Among) > 1 {
			var opts []cmf.ClientInfo
			for _, k := range ans.Among {
				if k >= 0 && k < len(cands) {
					opts = append(opts, cands[k])
				}
			}
			return cmfAnswerPlan{Action: actAskAgain, Options: opts, Name: ans.Name}
		}
		if lookup == nil {
			return cmfAnswerPlan{Action: actNone}
		}
		found, _, err := lookup(ans.Name)
		if err != nil {
			return cmfAnswerPlan{Action: actLookupError, Name: ans.Name, Err: err}
		}
		var covering []cmf.ClientInfo
		for _, k := range matchCandidates(ans.Name, found) {
			covering = append(covering, found[k])
		}
		_, initials := nameParts(ans.Name)
		single := fullWordCount(ans.Name) < 2 && len(initials) == 0
		switch {
		case len(covering) == 1 && !single:
			return cmfAnswerPlan{Action: actBind, Client: covering[0], Name: ans.Name}
		case len(covering) >= 1:
			return cmfAnswerPlan{Action: actAskAgain, Options: covering, Name: ans.Name}
		case len(found) > 0 && !single:
			return cmfAnswerPlan{Action: actAskAgain, Options: found, Name: ans.Name}
		default:
			return cmfAnswerPlan{Action: actNotFound, Name: ans.Name}
		}
	}
	return cmfAnswerPlan{Action: actNone}
}

// --- применение ---

// answerMode — как пришёл ответ.
type answerMode int

const (
	modeSwipe     answerMode = iota // свайп на сообщение бота по этому чеку
	modeAddressed                   // свайп + обращение по имени («Джарвис, это …»)
	modeContext                     // без свайпа, следом за вопросом
)

// cmfWatchByAsk — наблюдение, на сообщение бота по которому ответили свайпом.
func (b *Bot) cmfWatchByAsk(ctx context.Context, quotedID string) (int, bool) {
	if b.db == nil || quotedID == "" {
		return 0, false
	}
	id, ok, err := b.db.CmfWatchByAsk(ctx, quotedID)
	if err != nil {
		fmt.Println("cmf: поиск вопроса:", err)
		return 0, false
	}
	return id, ok
}

// cmfSay — сообщение по чеку: ответом на сам чек, и свайп на него тоже
// считается ответом по этому чеку.
func (b *Bot) cmfSay(ctx context.Context, chat types.JID, w db.CmfWatchFull, text string) {
	if b.groupSilent(chat) {
		return
	}
	id := b.sendReply(chat, text, w.WaMessageID, w.SenderJID)
	if id == "" {
		return
	}
	if err := b.db.SetCmfWatchAsk(ctx, w.ID, id); err != nil {
		fmt.Println("cmf: не сохранил сообщение по чеку:", err)
	}
}

// cmfFollowUp — переспрос по чеку: ответить можно свайпом, а если просили
// ФИО — и просто следом (недолго).
func (b *Bot) cmfFollowUp(ctx context.Context, chat types.JID, w db.CmfWatchFull, text string, wantName bool) {
	if b.groupSilent(chat) {
		return
	}
	b.cmfSay(ctx, chat, w, text)
	b.setOpenAskIfLatest(chat.String(), openAsk{kind: "cmf_watch", watchID: w.ID, wantName: wantName})
}

// askCmfWatch задаёт вопрос «за кого платёж» ответом на сам чек и запоминает
// его — и для ответа свайпом, и для ответа следом без свайпа. Варианты вопроса
// сохраняются всегда (и пустые): «1» относится к тому списку, что видела группа.
func (b *Bot) askCmfWatch(ctx context.Context, chat types.JID, watchID int, waMsgID, senderJID, text string, opts []cmf.ClientInfo) {
	candJSON := "[]"
	if len(opts) > 0 {
		if j, err := json.Marshal(opts); err == nil {
			candJSON = string(j)
		}
	}
	_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", candJSON, "")
	if b.groupSilent(chat) {
		return
	}
	askID := b.sendReply(chat, text, waMsgID, senderJID)
	if askID == "" {
		return
	}
	if err := b.db.SetCmfWatchAsk(ctx, watchID, askID); err != nil {
		fmt.Println("cmf: не сохранил вопрос:", err)
	}
	b.setOpenAsk(chat.String(), openAsk{kind: "cmf_watch", watchID: watchID})
}

func sameOptionIDs(a, b []cmf.ClientInfo) bool {
	if len(a) != len(b) {
		return false
	}
	ids := map[string]int{}
	for _, c := range a {
		ids[c.ID]++
	}
	for _, c := range b {
		ids[c.ID]--
	}
	for _, v := range ids {
		if v != 0 {
			return false
		}
	}
	return true
}

func (b *Bot) cmfLookupFunc(ctx context.Context) func(string) ([]cmf.ClientInfo, cmfMatchKind, error) {
	if b.cmf == nil {
		return nil
	}
	return func(name string) ([]cmf.ClientInfo, cmfMatchKind, error) {
		return b.cmfLookupWithTypos(ctx, name)
	}
}

// watchResolved — по чеку уже есть решение (клиент привязан, «нет в
// программе», платёж найден, чек удалён).
func watchResolved(w db.CmfWatchFull) bool {
	switch w.Status {
	case "found", "unmatched", "deleted", "reminded":
		return true
	case "watch":
		return w.ClientID != ""
	}
	return false
}

// answerOpts — как пришёл ответ: режим, просил ли бот ФИО, от владельца ли.
type answerOpts struct {
	mode     answerMode
	wantName bool
	admin    bool
}

// applyCmfWatchAnswer применяет ответ на вопрос «за кого платёж» (для тестов и
// простых вызовов — как от сотрудника).
func (b *Bot) applyCmfWatchAnswer(ctx context.Context, chat types.JID, watchID int, text string, mode answerMode, wantName bool) bool {
	return b.applyCmfWatchAnswerOpts(ctx, chat, watchID, text, answerOpts{mode: mode, wantName: wantName})
}

// applyCmfWatchAnswerOpts применяет ответ на вопрос «за кого платёж». Возвращает
// true, если сообщение обработано (в т.ч. молча — «спасибо» на «✅ Понял»).
//   - свайп: понимаем любой ответ (неясное — ИИ); если ИИ решил, что это не
//     ответ, а разговор: владельцу отвечает ассистент, сотруднику — тишина;
//     без ИИ переспрашиваем, НИЧЕГО не привязывая;
//   - обращение по имени: только понятный ответ, остальное — ассистенту;
//   - без свайпа: только бесспорное (номер, вариант целиком, «нет в программе»,
//     а сразу после просьбы «напишите ФИО» — ФИО, которое однозначно нашлось),
//     чтобы не перехватить имя перед новым чеком или новый платёж;
//   - чек уже решён: «спасибо/ок/👍» — молча; меняем решение только на явную
//     поправку (другой клиент, «нет», «нет в программе»).
func (b *Bot) applyCmfWatchAnswerOpts(ctx context.Context, chat types.JID, watchID int, text string, o answerOpts) bool {
	w, ok, err := b.db.CmfWatchByID(ctx, watchID)
	if err != nil || !ok {
		return false
	}
	if w.Deleted || w.Status == "deleted" {
		b.clearOpenAskFor(chat.String(), w.ID)
		if o.mode == modeContext {
			return false
		}
		return true // чек удалён — по нему ничего не делаем и не пишем
	}
	resolved := watchResolved(w)
	if resolved && o.mode == modeContext {
		return false // решённый чек без свайпа не трогаем
	}
	var cands []cmf.ClientInfo
	if w.Candidates != "" {
		_ = json.Unmarshal([]byte(w.Candidates), &cands)
	}
	bound := w.ClientID != "" && (w.Status == "watch" || w.Status == "reminded" || w.Status == "found")
	if bound {
		// Привязанный клиент — единственный «вариант»: «нет», «не она», «не Разет»
		// понимаются как отказ от него.
		cands = []cmf.ClientInfo{{ID: w.ClientID, FullName: w.ClientName}}
	}
	if resolved && isAck(text) {
		return true // «принято» на решённый чек — молчим
	}
	ans, confident := parseCmfAnswer(text, w.ClientText, cands)
	if bound && ans.Kind == ansOther {
		// «не Разет» / «нет, не за неё» по привязанному клиенту — отказ.
		toks := answerTokens(cleanAnswer(text))
		if hasNegation(toks) && len(toks) <= 4 {
			named := stripFillers(strings.Join(dropNegations(toks), " "))
			if named == "" || len(matchCandidates(named, cands)) == 1 {
				ans, confident = cmfAnswer{Kind: ansReject}, true
			}
		}
	}
	aiUsed := false
	if !confident && o.mode != modeContext {
		question := fmt.Sprintf("Бот спросил, за кого чек на %s (подпись к чеку: «%s»).", rub0(w.Amount), w.ClientText)
		if bound {
			question = fmt.Sprintf("Бот написал по чеку на %s: «за «%s»» и попросил поправить, если это не так.", rub0(w.Amount), w.ClientName)
		}
		if a, ok := b.cmfAnswerByAI(ctx, question, text, cands); ok {
			ans, confident, aiUsed = a, true, true
		}
	}
	if resolved {
		// Решённый чек меняем только по явной поправке.
		switch ans.Kind {
		case ansClient, ansNotInProgram, ansReject:
		case ansPick:
			if bound {
				return true // «да/он» про того же клиента — это подтверждение
			}
		default:
			if o.mode == modeAddressed || (aiUsed && o.admin) {
				return false // владелец спрашивает о чём-то — пусть ответит ассистент
			}
			return true
		}
		if !confident {
			return true
		}
	}
	switch o.mode {
	case modeContext:
		if !confident {
			return false
		}
		switch ans.Kind {
		case ansPick:
			low := cleanAnswer(text)
			if yesWords[low] || !(isPickToken(text) || len(matchCandidates(text, cands)) == 1) {
				return false
			}
		case ansNotInProgram:
		case ansClient:
			if !o.wantName || len(ans.Among) > 0 || fullWordCount(text) < 2 || parser.ExtractAmount(text) > 0 {
				return false
			}
			// Без свайпа — только если ФИО однозначно нашлось в программе.
			if plan := planCmfAnswer(ans, cands, b.cmfLookupFunc(ctx)); plan.Action != actBind {
				return false
			}
		default:
			return false
		}
	case modeAddressed:
		if !confident || ans.Kind == ansOther {
			return false
		}
	default: // modeSwipe
		if isAck(text) {
			return true // «спасибо»/«👍» на вопрос — не ответ, молчим
		}
		if !confident {
			b.cmfFollowUp(ctx, chat, w, "Не понял ответ — напишите ФИО клиента"+numberHint(cands)+" (или «нет в программе»).", true)
			return true
		}
		if ans.Kind == ansOther {
			if aiUsed {
				return !o.admin // сотруднику — тишина; владельцу ответит ассистент
			}
			hint := "Напишите ФИО клиента, за кого этот платёж (или «нет в программе»)."
			if len(cands) > 1 {
				hint = fmt.Sprintf("Нет такого варианта — ответьте номером от 1 до %d или ФИО клиента.", len(cands))
			}
			b.cmfFollowUp(ctx, chat, w, hint, true)
			return true
		}
	}
	if w.Status == "found" && ans.Kind != ansClient {
		return true
	}
	plan := planCmfAnswer(ans, cands, b.cmfLookupFunc(ctx))
	if bound && plan.Action == actBind && plan.Client.ID == w.ClientID {
		return true // тот же клиент — ничего не меняем
	}
	switch plan.Action {
	case actNone:
		return false
	case actBind:
		b.bindCmfWatch(ctx, chat, w, plan.Client, true)
	case actAskAgain:
		if sameOptionIDs(plan.Options, cands) {
			b.cmfFollowUp(ctx, chat, w, "Под это ФИО подходит не один вариант — ответьте номером из списка.", false)
			break
		}
		q, opts := cmfAskText(plan.Name, w.Amount, plan.Options)
		b.askCmfWatch(ctx, chat, w.ID, w.WaMessageID, w.SenderJID, q, opts)
	case actNotFound:
		b.cmfFollowUp(ctx, chat, w, fmt.Sprintf("Клиента «%s» в программе не нашёл — проверьте, как он записан там, и ответьте ещё раз (или «нет в программе»).", plan.Name), true)
	case actNotInProgram:
		b.unbindToUnmatched(ctx, w)
		b.clearOpenAskFor(chat.String(), w.ID)
		msg := "Понял, клиента нет в программе — по этому чеку напоминать не буду."
		if branch, _ := b.db.SettingGet(ctx, settingUnmatchedBranch); branch != "" {
			msg += " Отнёс к точке «" + branch + "»."
		}
		b.cmfSay(ctx, chat, w, msg)
	case actUnknown:
		b.clearOpenAskFor(chat.String(), w.ID)
		b.cmfSay(ctx, chat, w, "Хорошо, оставлю пока без клиента.")
	case actReject:
		if bound {
			b.unbindRejected(ctx, w)
			_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "[]", "")
			b.cmfFollowUp(ctx, chat, w, fmt.Sprintf("Понял, не «%s». За кого тогда этот платёж? Напишите ФИО клиента.", w.ClientName), true)
			break
		}
		b.cmfFollowUp(ctx, chat, w, "Тогда напишите ФИО клиента, за кого этот платёж.", true)
	case actLookupError:
		b.cmfFollowUp(ctx, chat, w, "Не смог проверить в программе: "+cmf.Human(plan.Err)+". Ответьте ещё раз чуть позже.", false)
	}
	return true
}

func numberHint(cands []cmf.ClientInfo) string {
	if len(cands) > 1 {
		return " или номер варианта"
	}
	return ""
}

func dropNegations(toks []string) []string {
	var out []string
	for _, t := range toks {
		switch t {
		case "не", "нет", "неа", "ни", "нету", "за", "неё", "нее", "него", "она", "он":
			continue
		}
		out = append(out, t)
	}
	return out
}

// unbindRejected — «это не он»: снимаем привязку к клиенту (клиент чека —
// снова плательщик с подписи), а плательщика помечаем как «платит за разных».
func (b *Bot) unbindRejected(ctx context.Context, w db.CmfWatchFull) {
	_ = b.db.ClearCmfWatchClient(ctx, w.ID, "ambiguous")
	if pc, ok := b.payerClient(ctx, w.ClientText); ok && pc.ID == w.ClientID {
		b.markPayerConflict(ctx, w.ClientText)
	}
	if w.WaMessageID != "" && w.ClientText != "" {
		canonical, _ := b.aliases.ResolveName(w.ClientText)
		var cidPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			cidPtr = &cid
		}
		_, _ = b.db.SetReceiptClientByMessage(ctx, w.WaMessageID, canonical, cidPtr)
	}
}

func isPickToken(text string) bool {
	low := cleanAnswer(text)
	if _, ok := pickNumber(low); ok {
		return true
	}
	toks := answerTokens(low)
	if len(toks) == 0 {
		return false
	}
	_, ok := ordinalWords[toks[0]]
	return ok && (len(toks) == 1 || (len(toks) == 2 && toks[1] == "вариант"))
}

// unbindToUnmatched — «клиента нет в программе»: если чек уже был привязан,
// откатываем привязку (клиент чека — снова плательщик с подписи, связь
// «плательщик → клиент» забываем).
func (b *Bot) unbindToUnmatched(ctx context.Context, w db.CmfWatchFull) {
	_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "unmatched")
	if w.ClientID == "" {
		return
	}
	if pc, ok := b.payerClient(ctx, w.ClientText); ok && pc.ID == w.ClientID {
		b.markPayerConflict(ctx, w.ClientText)
	}
	if w.WaMessageID != "" && w.ClientText != "" {
		canonical, _ := b.aliases.ResolveName(w.ClientText)
		var cidPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			cidPtr = &cid
		}
		_, _ = b.db.SetReceiptClientByMessage(ctx, w.WaMessageID, canonical, cidPtr)
	}
}

// bindCmfWatch привязывает чек к клиенту программы: сверка ждёт его платёж,
// в учёте чек записывается на этого клиента, а связь «плательщик с чека →
// клиент» запоминается — только для настоящего плательщика (ФИО из 2+ слов,
// не сам клиент) и только если она не спорит с прежней.
func (b *Bot) bindCmfWatch(ctx context.Context, chat types.JID, w db.CmfWatchFull, c cmf.ClientInfo, announce bool) {
	_ = b.db.UpdateCmfWatch(ctx, w.ID, "", c.ID, c.FullName, "", "watch")
	b.clearOpenAskFor(chat.String(), w.ID)
	if w.WaMessageID != "" {
		canonical, _ := b.aliases.ResolveName(c.FullName)
		var cidPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			cidPtr = &cid
		}
		if _, err := b.db.SetReceiptClientByMessage(ctx, w.WaMessageID, canonical, cidPtr); err != nil {
			fmt.Println("cmf: не записал клиента чеку:", err)
		}
	}
	note := ""
	if w.ClientText != "" {
		prev, had := b.payerClient(ctx, w.ClientText)
		switch {
		case nameCovers(w.ClientText, c.FullName):
			// Подпись — это сам клиент: никакой связи «плательщик → клиент».
			if had {
				b.forgetPayer(ctx, w.ClientText)
			}
		case !payerEligible(w.ClientText):
			// «Марха» / «Альмурзаева» — это сокращённое имя, а не плательщик.
		case had && prev.ID != c.ID:
			// Платит за разных людей — больше не угадываем, будем спрашивать.
			b.markPayerConflict(ctx, w.ClientText)
		case had:
		default:
			if b.rememberPayer(ctx, w.ClientText, c) {
				note = fmt.Sprintf(" Запомнил: «%s» платит за этого клиента.", w.ClientText)
			}
		}
	}
	if announce {
		b.cmfSay(ctx, chat, w, fmt.Sprintf("✅ Понял: чек на %s — оплата за «%s». Слежу, чтобы внесли в программу.%s", rub0(w.Amount), c.FullName, note))
	}
	fmt.Printf("cmf: чек %d на %.0f ₽ привязан к клиенту %s\n", w.ID, w.Amount, c.FullName)
}

// --- «плательщик → клиент» ---

type payerRec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Raw      string `json:"raw,omitempty"`      // подпись плательщика как есть (с дефисами)
	Conflict bool   `json:"conflict,omitempty"` // платит за разных клиентов — не угадываем
}

func payerKey(name string) string { return normCyr(name) }

// nonNameWords — слова, с которыми подпись к чеку — не ФИО плательщика
// («Первый взнос», «Досрочное погашение», «Доброе утро»).
var nonNameWords = map[string]bool{
	"взнос": true, "первый": true, "погашение": true, "досрочное": true, "оплата": true, "долг": true,
	"остаток": true, "рассрочка": true, "доброе": true, "добрый": true, "утро": true, "день": true,
	"вечер": true, "платёж": true, "платеж": true, "перевод": true, "за": true, "месяц": true,
}

// payerEligible — запоминать можно только плательщика с настоящим ФИО: 2+
// слова (составное через дефис — одно слово), без служебных слов.
func payerEligible(payer string) bool {
	if fullWordCount(payer) < 2 {
		return false
	}
	full, _ := nameParts(payer)
	for _, w := range full {
		if nonNameWords[w] || unclearWords[w] || nameStopwords[w] {
			return false
		}
	}
	return true
}

func (b *Bot) loadPayerMap(ctx context.Context) map[string]payerRec {
	m := map[string]payerRec{}
	if b.db == nil {
		return m
	}
	if s, err := b.db.SettingGet(ctx, settingPayerMap); err == nil && s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

func (b *Bot) savePayerMap(ctx context.Context, m map[string]payerRec) bool {
	j, err := json.Marshal(m)
	if err != nil {
		return false
	}
	return b.db.SettingSet(ctx, settingPayerMap, string(j)) == nil
}

func recRaw(k string, r payerRec) string {
	if r.Raw != "" {
		return r.Raw
	}
	return k
}

// rememberPayer — запоминает, что плательщик с чека платит за клиента c. Не
// перезаписывает отметку «платит за разных».
func (b *Bot) rememberPayer(ctx context.Context, payer string, c cmf.ClientInfo) bool {
	k := payerKey(payer)
	if k == "" || c.ID == "" || !payerEligible(payer) {
		return false
	}
	b.payerMu.Lock()
	defer b.payerMu.Unlock()
	m := b.loadPayerMap(ctx)
	for kk, r := range m {
		if r.Conflict && (kk == k || sameClientName(recRaw(kk, r), payer)) {
			return false
		}
	}
	if _, exists := m[k]; !exists && len(m) >= 1000 {
		return false // разрослось — не копим бесконечно
	}
	m[k] = payerRec{ID: c.ID, Name: c.FullName, Raw: strings.TrimSpace(payer)}
	return b.savePayerMap(ctx, m)
}

// markPayerConflict — плательщик платит за разных клиентов: дальше спрашиваем.
func (b *Bot) markPayerConflict(ctx context.Context, payer string) {
	if !payerEligible(payer) {
		return
	}
	b.payerMu.Lock()
	defer b.payerMu.Unlock()
	m := b.loadPayerMap(ctx)
	for kk, r := range m {
		if kk == payerKey(payer) || sameClientName(recRaw(kk, r), payer) {
			delete(m, kk)
		}
	}
	m[payerKey(payer)] = payerRec{Raw: strings.TrimSpace(payer), Conflict: true}
	b.savePayerMap(ctx, m)
}

// forgetPayer — забыть плательщика (подпись оказалась самим клиентом).
func (b *Bot) forgetPayer(ctx context.Context, payer string) {
	b.payerMu.Lock()
	defer b.payerMu.Unlock()
	m := b.loadPayerMap(ctx)
	changed := false
	for k, r := range m {
		if r.Conflict {
			continue
		}
		if k == payerKey(payer) || sameClientName(recRaw(k, r), payer) {
			delete(m, k)
			changed = true
		}
	}
	if changed {
		b.savePayerMap(ctx, m)
	}
}

// payerClient — клиент, за которого платит этот плательщик (если запомнен и
// однозначен: две подходящие записи на разных клиентов или отметка «платит за
// разных» — не угадываем).
func (b *Bot) payerClient(ctx context.Context, payer string) (cmf.ClientInfo, bool) {
	if !payerEligible(payer) {
		return cmf.ClientInfo{}, false
	}
	m := b.loadPayerMap(ctx)
	if r, ok := m[payerKey(payer)]; ok {
		if r.Conflict || r.ID == "" {
			return cmf.ClientInfo{}, false
		}
		return cmf.ClientInfo{ID: r.ID, FullName: r.Name}, true
	}
	var hit *payerRec
	for k, r := range m {
		raw := recRaw(k, r)
		if !payerEligible(raw) || !sameClientName(raw, payer) {
			continue
		}
		if r.Conflict || (hit != nil && hit.ID != r.ID) {
			return cmf.ClientInfo{}, false
		}
		rr := r
		hit = &rr
	}
	if hit == nil || hit.ID == "" {
		return cmf.ClientInfo{}, false
	}
	return cmf.ClientInfo{ID: hit.ID, FullName: hit.Name}, true
}

// payerFor — клиент по памяти о плательщике, но только если в программе нет
// клиента, который и есть этот человек (иначе спрашиваем — общий путь для
// сверки чека и для отчёта).
func (b *Bot) payerFor(ctx context.Context, name string, clients []cmf.ClientInfo) (cmf.ClientInfo, bool) {
	if len(matchCandidates(name, clients)) > 0 {
		return cmf.ClientInfo{}, false
	}
	return b.payerClient(ctx, name)
}

// cmfReplyAnswer — свайп на сообщение бота по чеку («за кого платёж?», переспрос,
// «как в прошлый раз», «✅ Понял»): применяет ответ. Вызывается до маршрутизации
// реплая в ассистента.
func (b *Bot) cmfReplyAnswer(ctx context.Context, msg *events.Message, text string, mode answerMode) bool {
	quotedID := extractQuotedStanzaID(msg)
	if quotedID == "" {
		return false
	}
	watchID, ok := b.cmfWatchByAsk(ctx, quotedID)
	if !ok {
		return false
	}
	return b.applyCmfWatchAnswerOpts(ctx, msg.Info.Chat, watchID, text, answerOpts{mode: mode, admin: b.isReportAdmin(msg.Info)})
}

// cmfAddressedContext — «Джарвис, это Альмурзаева Разет» / «Джарвис, 2» БЕЗ
// свайпа сразу после вопроса бота: понимаем как ответ на открытый вопрос.
func (b *Bot) cmfAddressedContext(ctx context.Context, msg *events.Message, text string) bool {
	a, ok := b.recentOpenAsk(msg.Info.Chat.String())
	if !ok || a.kind != "cmf_watch" {
		return false
	}
	return b.applyCmfWatchAnswerOpts(ctx, msg.Info.Chat, a.watchID, text,
		answerOpts{mode: modeAddressed, wantName: a.wantName, admin: b.isReportAdmin(msg.Info)})
}

// reresolveWatchByCheck — свайп с ФИО на САМ чек: клиент чека поменялся —
// сверка тоже должна следить за новым клиентом. Подпись-плательщик остаётся
// прежней (её и запомним как плательщика за этого клиента).
func (b *Bot) reresolveWatchByCheck(ctx context.Context, chat types.JID, checkWaID, name string) bool {
	if b.cmf == nil || b.db == nil {
		return false
	}
	wid, ok, err := b.db.CmfWatchByCheckMessage(ctx, checkWaID)
	if err != nil || !ok {
		return false
	}
	w, ok, err := b.db.CmfWatchByID(ctx, wid)
	if err != nil || !ok {
		return false
	}
	if w.Status == "found" || w.Status == "deleted" || w.Deleted {
		return true
	}
	b.clearOpenAskFor(chat.String(), wid)
	if w.ClientID != "" {
		if pc, had := b.payerClient(ctx, w.ClientText); had && pc.ID == w.ClientID && !nameCovers(name, pc.FullName) {
			b.markPayerConflict(ctx, w.ClientText) // «как в прошлый раз» оказалось не так
		}
	}
	go func() {
		ctx := context.Background()
		plan := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: name}, nil, b.cmfLookupFunc(ctx))
		w, ok, err := b.db.CmfWatchByID(ctx, wid)
		if err != nil || !ok {
			return
		}
		switch plan.Action {
		case actBind:
			b.bindCmfWatch(ctx, chat, w, plan.Client, false)
		case actLookupError:
			_ = b.db.UpdateCmfWatch(ctx, wid, "", "", "", "", "lookup")
		default:
			if w.ClientID != "" {
				_ = b.db.ClearCmfWatchClient(ctx, wid, "ambiguous")
			}
			q, opts := cmfAskText(name, w.Amount, plan.Options)
			b.askCmfWatch(ctx, chat, wid, w.WaMessageID, w.SenderJID, q, opts)
		}
	}()
	return true
}
