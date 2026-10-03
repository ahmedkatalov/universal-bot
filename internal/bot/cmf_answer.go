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
// «уточню у …», «ни … ни …», «вариант» без номера — пусть решает ИИ.
var unclearWords = map[string]bool{
	"никто": true, "никого": true, "ни": true, "них": true, "из": true, "одна": true, "один": true,
	"вариант": true, "варианта": true, "новая": true, "новый": true, "клиентка": true,
	"салам": true, "салям": true, "ассаламу": true, "ассалам": true, "алейкум": true, "алейкуму": true,
	"привет": true, "здравствуйте": true, "уточню": true, "спрошу": true, "узнаю": true, "щас": true,
	"сейчас": true, "потом": true, "позже": true, "ок": true, "окей": true, "спасибо": true,
}

var yesWords = map[string]bool{
	"да": true, "ага": true, "угу": true, "он": true, "она": true, "верно": true, "точно": true,
	"это он": true, "это она": true, "да он": true, "да она": true, "да это он": true, "да это она": true,
	"да, он": true, "да, она": true, "правильно": true, "так": true, "именно": true, "да, это он": true, "да, это она": true,
}

var rejectWords = map[string]bool{
	"нет": true, "не он": true, "не она": true, "нет, не он": true, "нет, не она": true, "неа": true, "нет не он": true, "нет не она": true,
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
	// Номер варианта: «2», «№2», «второй», «2-й», «второй вариант», «вторая марха».
	if len(cands) > 0 {
		w := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(low, "№"), "номер "))
		if n, err := strconv.Atoi(w); err == nil {
			if n >= 1 && n <= len(cands) {
				return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
			}
			return cmfAnswer{Kind: ansOther}, true
		}
		if len(toks) > 0 {
			if n, ok := ordinalWords[toks[0]]; ok && n <= len(cands) {
				rest := toks[1:]
				okRest := true
				var named []string
				for _, t := range rest {
					switch t {
					case "вариант", "номер", "это", "он", "она":
					default:
						named = append(named, t)
					}
				}
				if len(named) > 0 {
					idx := matchCandidates(strings.Join(named, " "), cands)
					okRest = false
					for _, k := range idx {
						if k == n-1 {
							okRest = true
						}
					}
				}
				if okRest {
					return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
				}
				return cmfAnswer{Kind: ansOther}, false
			}
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
	// Повторили имя плательщика с чека («Марха») — это не выбор.
	if caption != "" && nameCovers(low, caption) {
		return cmfAnswer{Kind: ansOther}, false
	}
	// Короткий ответ словами одного из вариантов («сайдаевой», «мусиева марха»).
	if len(toks) <= 3 {
		if idx := matchCandidates(low, cands); len(idx) == 1 {
			return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
		} else if len(idx) > 1 {
			return cmfAnswer{Kind: ansClient, Name: strings.TrimSpace(text), Among: idx}, true
		}
	}
	name, amount, deferToAI := clarifyNameFromReply(text)
	if deferToAI || amount > 0 || name == "" {
		return cmfAnswer{Kind: ansOther}, false
	}
	if idx := matchCandidates(name, cands); len(idx) == 1 {
		return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
	} else if len(idx) > 1 {
		return cmfAnswer{Kind: ansClient, Name: name, Among: idx}, true
	}
	// Одно слово (имя без фамилии, реплика) — не уверены.
	if full, _ := nameParts(name); len(full) < 2 {
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
	system := "Бот спросил в рабочей группе, за какого клиента программы рассрочек этот платёж по чеку. " +
		"Сотрудник ответил. Пойми ответ по смыслу (бывают опечатки, сленг, чеченские слова, падежи). " +
		"Верни СТРОГО JSON {\"kind\":\"pick|client|not_in_program|unknown|other\",\"pick\":номер варианта или 0,\"name\":\"ФИО в именительном падеже или пусто\"}. " +
		"pick — выбрал вариант из списка (номером, именем, фамилией, «да/он» при одном варианте); " +
		"client — назвал ФИО клиента (в т.ч. не из списка, например родственника, за которого платят); " +
		"not_in_program — клиента нет в программе/новый клиент; unknown — не знает; " +
		"other — это не ответ на вопрос (приветствие, отмашка, вопрос боту, болтовня, «сейчас уточню»). " +
		"ФИО в ответе — это КЛИЕНТ, а не имя сотрудника. Отрицание важно: «не Мусиева» — НЕ выбор Мусиевой; " +
		"«нет такого, это Дудаева Разет» — client «Дудаева Разет». «никто из них» без ФИО — not_in_program."
	user := "Вопрос бота: " + question + "\nВарианты:\n" + strings.Join(list, "\n") + "\nОтвет сотрудника: " + text
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
	info := analyzeCands(clientText, cands)
	if len(info) > cmfAskMaxOptions {
		info = info[:cmfAskMaxOptions]
	}
	opts := make([]cmf.ClientInfo, len(info))
	for k, ci := range info {
		opts[k] = ci.c
	}
	labels := optionLabels(opts)
	allFull, anySurname, sameName := true, false, true
	for _, ci := range info {
		allFull = allFull && ci.fullMatch
		anySurname = anySurname || ci.surname
		sameName = sameName && normCyr(ci.c.FullName) == normCyr(info[0].c.FullName)
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
	case anySurname:
		sb.WriteString(": точно такого клиента в программе нет. Похожие:")
	default:
		sb.WriteString(": такого клиента в программе нет — совпадает только имя:")
	}
	for k, ci := range info {
		fmt.Fprintf(&sb, "\n%d. %s", k+1, labels[k])
		if !allFull && ci.surname && !ci.fullMatch {
			sb.WriteString(" (та же фамилия)")
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
		full, initials := nameParts(ans.Name)
		single := len(full) < 2 && len(initials) == 0
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
// его — и для ответа свайпом, и для ответа следом без свайпа.
func (b *Bot) askCmfWatch(ctx context.Context, chat types.JID, watchID int, waMsgID, senderJID, text string, opts []cmf.ClientInfo) {
	if len(opts) > 0 {
		if j, err := json.Marshal(opts); err == nil {
			_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", string(j), "")
		}
	}
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

// applyCmfWatchAnswer применяет ответ на вопрос «за кого платёж». Возвращает
// true, если сообщение обработано как ответ.
//   - свайп: понимаем любой ответ (неясное — ИИ); если ИИ решил, что это не
//     ответ, а разговор, — отдаём ассистенту; без ИИ переспрашиваем, НИЧЕГО не
//     привязывая;
//   - свайп с обращением по имени: только понятный ответ, остальное — ассистенту;
//   - без свайпа: только бесспорное (номер, вариант целиком, «нет в программе»,
//     а сразу после просьбы «напишите ФИО» — ФИО), чтобы не перехватить имя
//     перед новым чеком или новый платёж.
func (b *Bot) applyCmfWatchAnswer(ctx context.Context, chat types.JID, watchID int, text string, mode answerMode, wantName bool) bool {
	w, ok, err := b.db.CmfWatchByID(ctx, watchID)
	if err != nil || !ok {
		return false
	}
	var cands []cmf.ClientInfo
	if w.Candidates != "" {
		_ = json.Unmarshal([]byte(w.Candidates), &cands)
	}
	ans, confident := parseCmfAnswer(text, w.ClientText, cands)
	aiUsed := false
	if !confident && mode != modeContext {
		question, _ := cmfAskText(w.ClientText, w.Amount, cands)
		if a, ok := b.cmfAnswerByAI(ctx, question, text, cands); ok {
			ans, confident, aiUsed = a, true, true
		}
	}
	switch mode {
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
			full, _ := nameParts(text)
			if !wantName || len(ans.Among) > 0 || len(full) < 2 || parser.ExtractAmount(text) > 0 {
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
		if !confident {
			b.cmfFollowUp(ctx, chat, w, "Не понял ответ — напишите номер варианта или ФИО клиента (или «нет в программе»).", true)
			return true
		}
		if ans.Kind == ansOther {
			if aiUsed {
				return false // это не ответ, а разговор — пусть ответит ассистент
			}
			hint := "Напишите ФИО клиента, за кого этот платёж (или «нет в программе»)."
			if len(cands) > 1 {
				hint = fmt.Sprintf("Нет такого варианта — ответьте номером от 1 до %d или ФИО клиента.", len(cands))
			}
			b.cmfFollowUp(ctx, chat, w, hint, true)
			return true
		}
	}
	if w.Status == "found" {
		b.cmfSay(ctx, chat, w, "Этот платёж уже найден в программе — менять не стал.")
		return true
	}
	plan := planCmfAnswer(ans, cands, b.cmfLookupFunc(ctx))
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
		b.cmfFollowUp(ctx, chat, w, "Тогда напишите ФИО клиента, за кого этот платёж.", true)
	case actLookupError:
		b.cmfFollowUp(ctx, chat, w, "Не смог проверить в программе: "+cmf.Human(plan.Err)+". Ответьте ещё раз чуть позже.", false)
	}
	return true
}

func isPickToken(text string) bool {
	low := cleanAnswer(text)
	low = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(low, "№"), "номер "))
	if _, err := strconv.Atoi(low); err == nil {
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
		b.forgetPayer(ctx, w.ClientText)
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
			b.forgetPayer(ctx, w.ClientText)
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
	ID   string `json:"id"`
	Name string `json:"name"`
	Raw  string `json:"raw,omitempty"` // подпись плательщика как есть (с дефисами)
}

func payerKey(name string) string { return normCyr(name) }

// payerEligible — запоминать можно только плательщика с ФИО из 2+ слов.
func payerEligible(payer string) bool {
	full, _ := nameParts(payer)
	return len(full) >= 2
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

// rememberPayer — запоминает, что плательщик с чека платит за клиента c.
func (b *Bot) rememberPayer(ctx context.Context, payer string, c cmf.ClientInfo) bool {
	k := payerKey(payer)
	if k == "" || c.ID == "" || !payerEligible(payer) {
		return false
	}
	b.payerMu.Lock()
	defer b.payerMu.Unlock()
	m := b.loadPayerMap(ctx)
	if _, exists := m[k]; !exists && len(m) >= 1000 {
		return false // разрослось — не копим бесконечно
	}
	m[k] = payerRec{ID: c.ID, Name: c.FullName, Raw: strings.TrimSpace(payer)}
	return b.savePayerMap(ctx, m)
}

// forgetPayer — забыть плательщика (и записи, которые с ним совпадают).
func (b *Bot) forgetPayer(ctx context.Context, payer string) {
	b.payerMu.Lock()
	defer b.payerMu.Unlock()
	m := b.loadPayerMap(ctx)
	changed := false
	for k, r := range m {
		raw := r.Raw
		if raw == "" {
			raw = k
		}
		if k == payerKey(payer) || sameClientName(raw, payer) {
			delete(m, k)
			changed = true
		}
	}
	if changed {
		b.savePayerMap(ctx, m)
	}
}

// payerClient — клиент, за которого платит этот плательщик (если запомнен и
// однозначен: две подходящие записи на разных клиентов — не угадываем).
func (b *Bot) payerClient(ctx context.Context, payer string) (cmf.ClientInfo, bool) {
	if !payerEligible(payer) {
		return cmf.ClientInfo{}, false
	}
	m := b.loadPayerMap(ctx)
	if r, ok := m[payerKey(payer)]; ok && r.ID != "" {
		return cmf.ClientInfo{ID: r.ID, FullName: r.Name}, true
	}
	var hit *payerRec
	for k, r := range m {
		raw := r.Raw
		if raw == "" {
			raw = k
		}
		if r.ID == "" || !payerEligible(raw) || !sameClientName(raw, payer) {
			continue
		}
		if hit != nil && hit.ID != r.ID {
			return cmf.ClientInfo{}, false
		}
		rr := r
		hit = &rr
	}
	if hit == nil {
		return cmf.ClientInfo{}, false
	}
	return cmf.ClientInfo{ID: hit.ID, FullName: hit.Name}, true
}

// cmfReplyAnswer — свайп на сообщение бота по чеку («за кого платёж?», переспрос,
// «как в прошлый раз»): применяет ответ. Вызывается до маршрутизации реплая в
// ассистента.
func (b *Bot) cmfReplyAnswer(ctx context.Context, msg *events.Message, text string, mode answerMode) bool {
	quotedID := extractQuotedStanzaID(msg)
	if quotedID == "" {
		return false
	}
	watchID, ok := b.cmfWatchByAsk(ctx, quotedID)
	if !ok {
		return false
	}
	return b.applyCmfWatchAnswer(ctx, msg.Info.Chat, watchID, text, mode, false)
}

// reresolveWatchByCheck — свайп с ФИО на САМ чек: клиент чека поменялся —
// сверка тоже должна следить за новым клиентом.
func (b *Bot) reresolveWatchByCheck(ctx context.Context, chat types.JID, checkWaID, name string) bool {
	if b.cmf == nil || b.db == nil {
		return false
	}
	wid, ok, err := b.db.CmfWatchByCheckMessage(ctx, checkWaID)
	if err != nil || !ok {
		return false
	}
	w, ok, err := b.db.CmfWatchByID(ctx, wid)
	if err != nil || !ok || w.Status == "found" {
		return ok
	}
	if w.ClientID != "" {
		if pc, had := b.payerClient(ctx, w.ClientText); had && pc.ID == w.ClientID && !nameCovers(name, pc.FullName) {
			b.forgetPayer(ctx, w.ClientText) // «как в прошлый раз» оказалось не так
		}
	}
	_ = b.db.UpdateCmfWatch(ctx, wid, name, "", "", "", "lookup")
	go b.cmfResolveWatch(context.Background(), wid, chat, name, w.Amount)
	return true
}
