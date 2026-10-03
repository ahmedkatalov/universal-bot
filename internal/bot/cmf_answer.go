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

var notInProgramMarks = []string{
	"нет в программе", "нету в программе", "не в программе", "нет в базе", "нету в базе",
	"не внесен в программу", "не внесён в программу", "новый клиент", "нет такого клиента",
	"нету такого клиента", "нет такого", "нету такого", "не наш клиент", "не клиент",
	"его нет", "её нет", "ее нет", "нету его", "нету её", "нету ее",
}

var yesWords = map[string]bool{
	"да": true, "ага": true, "угу": true, "он": true, "она": true, "верно": true, "точно": true,
	"это он": true, "это она": true, "да он": true, "да она": true, "да это он": true, "да это она": true,
	"да, он": true, "да, она": true, "правильно": true, "так": true, "именно": true,
}

func cleanAnswer(text string) string {
	low := strings.ToLower(strings.TrimSpace(text))
	low = strings.Trim(low, ".,!)👍✅ \t")
	return strings.Join(strings.Fields(low), " ")
}

// parseCmfAnswer — разбор ответа правилами. confident=false — правила не
// уверены (длинная фраза, вопрос, незнакомое слово): пусть решит ИИ.
func parseCmfAnswer(text string, cands []cmf.ClientInfo) (ans cmfAnswer, confident bool) {
	low := cleanAnswer(text)
	if low == "" {
		return cmfAnswer{Kind: ansOther}, true
	}
	if strings.Contains(text, "?") {
		return cmfAnswer{Kind: ansOther}, false
	}
	for _, m := range notInProgramMarks {
		if strings.Contains(low, m) {
			return cmfAnswer{Kind: ansNotInProgram}, true
		}
	}
	if isUnknownReply(low) {
		return cmfAnswer{Kind: ansUnknown}, true
	}
	// Номер варианта: «2», «№2», «второй», «2-й».
	if len(cands) > 0 {
		w := strings.TrimPrefix(strings.TrimPrefix(low, "№"), "номер ")
		w = strings.TrimSpace(w)
		if n, err := strconv.Atoi(w); err == nil && n >= 1 && n <= len(cands) {
			return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
		}
		if n, ok := ordinalWords[w]; ok && n <= len(cands) {
			return cmfAnswer{Kind: ansPick, Pick: n - 1}, true
		}
	}
	if len(cands) == 1 {
		if yesWords[low] {
			return cmfAnswer{Kind: ansPick, Pick: 0}, true
		}
		if low == "нет" || low == "не он" || low == "не она" || low == "нет, не он" || low == "нет, не она" {
			return cmfAnswer{Kind: ansReject}, true
		}
	}
	// Голые цифры/сумма — это не ФИО (а номер вне списка — тоже не ответ).
	if strings.IndexFunc(low, unicode.IsLetter) < 0 {
		return cmfAnswer{Kind: ansOther}, true
	}
	// Короткий ответ словами одного из вариантов, как угодно написанный
	// («сайдаевой», «мусиева марха») — выбор этого варианта.
	if ws := strings.Fields(low); len(ws) <= 3 && strings.IndexFunc(low, unicode.IsDigit) < 0 {
		if idx := matchCandidates(low, cands); len(idx) == 1 {
			return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
		}
	}
	name, amount, deferToAI := clarifyNameFromReply(text)
	if deferToAI || amount > 0 {
		return cmfAnswer{Kind: ansOther}, false
	}
	if name == "" {
		// Одно незнакомое слово («давай», «Д1авала») — не уверены, что это имя.
		return cmfAnswer{Kind: ansOther}, false
	}
	// Названо ФИО — сначала среди вариантов.
	if idx := matchCandidates(name, cands); len(idx) == 1 {
		return cmfAnswer{Kind: ansPick, Pick: idx[0]}, true
	} else if len(idx) > 1 {
		return cmfAnswer{Kind: ansClient, Name: name, Among: idx}, true
	}
	// Одно слово, которого нет среди вариантов (обычное имя/реплика) — не уверены.
	if len(normWords(name)) < 2 {
		return cmfAnswer{Kind: ansClient, Name: name}, false
	}
	return cmfAnswer{Kind: ansClient, Name: name}, true
}

// matchCandidates — варианты, к которым подходит названное имя: все значимые
// слова ответа нашлись в имени варианта («Сайдаева» → «Сайдаева Марха …»).
func matchCandidates(name string, cands []cmf.ClientInfo) []int {
	q := normWords(name)
	if len(q) == 0 {
		return nil
	}
	var out []int
	for k, c := range cands {
		cw := normWords(c.FullName)
		hit := 0
		used := make([]bool, len(cw))
		for _, w := range q {
			for x, v := range cw {
				if !used[x] && nameWordSame(w, v) {
					used[x] = true
					hit++
					break
				}
			}
		}
		if hit == len(q) {
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
	for k, c := range cands {
		list = append(list, fmt.Sprintf("%d. %s", k+1, c.FullName))
	}
	system := "Бот спросил в рабочей группе, за какого клиента программы рассрочек этот платёж по чеку. " +
		"Сотрудник ответил. Пойми ответ по смыслу (бывают опечатки, сленг, чеченские слова, падежи). " +
		"Верни СТРОГО JSON {\"kind\":\"pick|client|not_in_program|unknown|other\",\"pick\":номер варианта или 0,\"name\":\"ФИО в именительном падеже или пусто\"}. " +
		"pick — выбрал вариант из списка (номером, именем, фамилией, «да/он» при одном варианте); " +
		"client — назвал ФИО клиента (в т.ч. не из списка, например родственника, за которого платят); " +
		"not_in_program — клиента нет в программе/новый клиент; unknown — не знает; " +
		"other — это не ответ на вопрос (приветствие, отмашка, вопрос боту, болтовня). " +
		"ФИО в ответе — это КЛИЕНТ, а не имя сотрудника."
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

var surnameEndings = []string{"ова", "ева", "ёва", "ина", "ына", "ская", "цкая", "ов", "ев", "ёв", "ин", "ын", "ский", "цкий", "ая", "ий", "ой", "ко", "ук", "юк", "ян", "янц", "дзе", "швили"}

// looksLikeSurname — слово похоже на фамилию (по окончанию).
func looksLikeSurname(w string) bool {
	r := []rune(w)
	if len(r) < 5 {
		return false
	}
	for _, e := range surnameEndings {
		if strings.HasSuffix(w, e) {
			return true
		}
	}
	return false
}

// candInfo — вариант и то, чем он похож на имя с чека.
type candInfo struct {
	c         cmf.ClientInfo
	matched   []string // слова имени с чека, найденные у варианта
	surname   bool     // совпала фамилия (родственник/он сам)
	fullMatch bool     // совпали все значимые слова
}

func analyzeCands(query string, cands []cmf.ClientInfo) []candInfo {
	q := normWords(query)
	out := make([]candInfo, 0, len(cands))
	for _, c := range cands {
		cw := normWords(c.FullName)
		used := make([]bool, len(cw))
		ci := candInfo{c: c}
		for _, w := range q {
			for x, v := range cw {
				if !used[x] && wordSimilar(w, v) {
					used[x] = true
					ci.matched = append(ci.matched, w)
					if looksLikeSurname(w) {
						ci.surname = true
					}
					break
				}
			}
		}
		ci.fullMatch = len(q) > 0 && len(ci.matched) == len(q)
		out = append(out, ci)
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.fullMatch != y.fullMatch {
			return x.fullMatch
		}
		if len(x.matched) != len(y.matched) {
			return len(x.matched) > len(y.matched)
		}
		if x.surname != y.surname {
			return x.surname
		}
		return x.c.FullName < y.c.FullName
	})
	return out
}

func rub0(v float64) string { return formatMoney(v) + " ₽" }

const cmfAskMaxOptions = 5

// cmfAskText — вопрос в группу, когда клиента по чеку нельзя уверенно найти.
// Возвращает текст и варианты в том порядке, в каком они перечислены.
func cmfAskText(clientText string, amount float64, cands []cmf.ClientInfo) (string, []cmf.ClientInfo) {
	head := fmt.Sprintf("🔎 Чек на %s («%s»)", rub0(amount), clientText)
	if len(cands) == 0 {
		return head + ": такого клиента в программе не нашла. За кого этот платёж? Ответьте на это сообщение ФИО клиента (или «нет в программе»).", nil
	}
	info := analyzeCands(clientText, cands)
	if len(info) > cmfAskMaxOptions {
		info = info[:cmfAskMaxOptions]
	}
	opts := make([]cmf.ClientInfo, len(info))
	for k, ci := range info {
		opts[k] = ci.c
	}
	allFull := true
	anySurname := false
	for _, ci := range info {
		allFull = allFull && ci.fullMatch
		anySurname = anySurname || ci.surname
	}
	if len(info) == 1 {
		ci := info[0]
		switch {
		case ci.fullMatch:
			return head + fmt.Sprintf(": в программе похоже на «%s». Если это он(а) — ответьте «да»; если нет — напишите ФИО клиента.", ci.c.FullName), opts
		case ci.surname:
			return head + fmt.Sprintf(": такого клиента в программе нет, есть с той же фамилией — «%s». Платёж за него(неё)? Ответьте «да» или ФИО клиента.", ci.c.FullName), opts
		default:
			return head + fmt.Sprintf(": такого клиента в программе нет (есть только тёзка — «%s»). За кого этот платёж? Ответьте ФИО клиента (или «да», если за неё/него).", ci.c.FullName), opts
		}
	}
	var sb strings.Builder
	sb.WriteString(head)
	switch {
	case allFull:
		sb.WriteString(": в программе несколько подходящих клиентов:")
	case anySurname:
		sb.WriteString(": точно такого клиента в программе нет. Похожие:")
	default:
		sb.WriteString(": такого клиента в программе нет — совпадает только имя:")
	}
	for k, ci := range info {
		fmt.Fprintf(&sb, "\n%d. %s", k+1, ci.c.FullName)
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
		found, kind, err := lookup(ans.Name)
		if err != nil {
			return cmfAnswerPlan{Action: actLookupError, Name: ans.Name, Err: err}
		}
		switch {
		case (kind == cmfExact || kind == cmfStrong) && len(found) > 0:
			return cmfAnswerPlan{Action: actBind, Client: found[0], Name: ans.Name}
		case len(found) == 0:
			return cmfAnswerPlan{Action: actNotFound, Name: ans.Name}
		default:
			if len(found) == 1 {
				// Одно нечёткое совпадение на ФИО, которое человек сам назвал в ответ на
				// вопрос — это он (опечатка/склонение), если совпали все слова.
				if idx := matchCandidates(ans.Name, found); len(idx) == 1 {
					return cmfAnswerPlan{Action: actBind, Client: found[0], Name: ans.Name}
				}
			}
			return cmfAnswerPlan{Action: actAskAgain, Options: found, Name: ans.Name}
		}
	}
	return cmfAnswerPlan{Action: actNone}
}

// --- применение ---

// cmfWatchByAsk — наблюдение, на вопрос по которому ответили свайпом.
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

// askCmfWatch задаёт вопрос «за кого платёж» ответом на сам чек и запоминает
// его — и для ответа свайпом, и для ответа следом без свайпа.
func (b *Bot) askCmfWatch(ctx context.Context, chat types.JID, watchID int, waMsgID, senderJID, text string, opts []cmf.ClientInfo) {
	if b.groupSilent(chat) {
		return
	}
	candJSON := ""
	if len(opts) > 0 {
		if j, err := json.Marshal(opts); err == nil {
			candJSON = string(j)
		}
	}
	if candJSON != "" {
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", candJSON, "")
	}
	askID := b.sendReply(chat, text, waMsgID, senderJID)
	if askID == "" {
		return
	}
	if err := b.db.SetCmfWatchAsk(ctx, watchID, askID); err != nil {
		fmt.Println("cmf: не сохранила вопрос:", err)
	}
	b.setOpenAsk(chat.String(), openAsk{kind: "cmf_watch", watchID: watchID})
}

// applyCmfWatchAnswer применяет ответ на вопрос «за кого платёж». swipe —
// ответили свайпом на вопрос (тогда понимаем любой ответ, в т.ч. ИИ); без
// свайпа принимаем только бесспорное (номер/вариант/«нет в программе»), чтобы
// не перехватить обычное сообщение-имя перед новым чеком. Возвращает true,
// если сообщение — ответ и обработано.
func (b *Bot) applyCmfWatchAnswer(ctx context.Context, chat types.JID, watchID int, text string, swipe bool) bool {
	w, ok, err := b.db.CmfWatchByID(ctx, watchID)
	if err != nil || !ok {
		return false
	}
	var cands []cmf.ClientInfo
	if w.Candidates != "" {
		_ = json.Unmarshal([]byte(w.Candidates), &cands)
	}
	ans, confident := parseCmfAnswer(text, cands)
	if !swipe {
		// Без свайпа — только бесспорное.
		if !confident || !(ans.Kind == ansPick || ans.Kind == ansNotInProgram) {
			return false
		}
		if ans.Kind == ansPick && len(cands) > 1 && !isPickToken(text) && len(matchCandidates(text, cands)) != 1 {
			return false
		}
	} else if !confident {
		question, _ := cmfAskText(w.ClientText, w.Amount, cands)
		if a, ok := b.cmfAnswerByAI(ctx, question, text, cands); ok {
			ans = a
		}
	}
	var lookup func(string) ([]cmf.ClientInfo, cmfMatchKind, error)
	if b.cmf != nil {
		lookup = func(name string) ([]cmf.ClientInfo, cmfMatchKind, error) {
			return b.cmfLookupWithTypos(ctx, name)
		}
	}
	plan := planCmfAnswer(ans, cands, lookup)
	switch plan.Action {
	case actNone:
		return false
	case actBind:
		b.bindCmfWatch(ctx, chat, w, plan.Client)
	case actAskAgain:
		q, opts := cmfAskText(plan.Name, w.Amount, plan.Options)
		b.askCmfWatch(ctx, chat, w.ID, w.WaMessageID, w.SenderJID, q, opts)
	case actNotFound:
		b.sendText(chat, fmt.Sprintf("Клиента «%s» в программе не нашла — проверьте, как он записан там, и ответьте ещё раз (или «нет в программе»).", plan.Name))
		b.setOpenAsk(chat.String(), openAsk{kind: "cmf_watch", watchID: w.ID})
	case actNotInProgram:
		_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "unmatched")
		b.clearOpenAsk(chat.String())
		msg := "Понял, клиента нет в программе — по этому чеку напоминать не буду."
		if branch, _ := b.db.SettingGet(ctx, settingUnmatchedBranch); branch != "" {
			msg += " Отнесла к точке «" + branch + "»."
		}
		b.sendText(chat, msg)
	case actUnknown:
		b.clearOpenAsk(chat.String())
		b.sendText(chat, "Хорошо, оставлю пока без клиента.")
	case actReject:
		b.sendText(chat, "Тогда напишите ФИО клиента, за кого этот платёж.")
		b.setOpenAsk(chat.String(), openAsk{kind: "cmf_watch", watchID: w.ID})
	case actLookupError:
		b.sendText(chat, "Не смогла проверить в программе: "+cmf.Human(plan.Err)+". Ответьте ещё раз чуть позже.")
	}
	return true
}

func isPickToken(text string) bool {
	low := cleanAnswer(text)
	low = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(low, "№"), "номер "))
	if _, err := strconv.Atoi(low); err == nil {
		return true
	}
	_, ok := ordinalWords[low]
	return ok
}

// bindCmfWatch привязывает чек к клиенту программы: сверка ждёт его платёж,
// в учёте чек записывается на этого клиента, а связь «плательщик с чека →
// клиент» запоминается на будущее.
func (b *Bot) bindCmfWatch(ctx context.Context, chat types.JID, w db.CmfWatchFull, c cmf.ClientInfo) {
	_ = b.db.UpdateCmfWatch(ctx, w.ID, "", c.ID, c.FullName, "", "watch")
	b.clearOpenAsk(chat.String())
	if w.WaMessageID != "" {
		canonical, _ := b.aliases.ResolveName(c.FullName)
		var cidPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			cidPtr = &cid
		}
		if _, err := b.db.SetReceiptClientByMessage(ctx, w.WaMessageID, canonical, cidPtr); err != nil {
			fmt.Println("cmf: не записала клиента чеку:", err)
		}
	}
	remembered := false
	if w.ClientText != "" && !sameClientName(w.ClientText, c.FullName) {
		remembered = b.rememberPayer(ctx, w.ClientText, c)
	}
	msg := fmt.Sprintf("✅ Понял: чек на %s — оплата за «%s». Слежу, чтобы внесли в программу.", rub0(w.Amount), c.FullName)
	if remembered {
		msg += fmt.Sprintf(" Запомнила: «%s» платит за этого клиента.", w.ClientText)
	}
	b.sendText(chat, msg)
	fmt.Printf("cmf: чек %d на %.0f ₽ привязан к клиенту %s по ответу в группе\n", w.ID, w.Amount, c.FullName)
}

// --- «плательщик → клиент» ---

type payerRec struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func payerKey(name string) string { return normCyr(name) }

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

// rememberPayer — запоминает, что плательщик с чека платит за клиента c.
func (b *Bot) rememberPayer(ctx context.Context, payer string, c cmf.ClientInfo) bool {
	k := payerKey(payer)
	if k == "" || c.ID == "" {
		return false
	}
	m := b.loadPayerMap(ctx)
	m[k] = payerRec{ID: c.ID, Name: c.FullName}
	if len(m) > 1000 {
		return false // разрослось — не копим бесконечно
	}
	j, err := json.Marshal(m)
	if err != nil {
		return false
	}
	return b.db.SettingSet(ctx, settingPayerMap, string(j)) == nil
}

// payerClient — клиент, за которого обычно платит этот плательщик (если запомнен).
func (b *Bot) payerClient(ctx context.Context, payer string) (cmf.ClientInfo, bool) {
	m := b.loadPayerMap(ctx)
	if r, ok := m[payerKey(payer)]; ok && r.ID != "" {
		return cmf.ClientInfo{ID: r.ID, FullName: r.Name}, true
	}
	for k, r := range m {
		if r.ID != "" && sameClientName(k, payer) {
			return cmf.ClientInfo{ID: r.ID, FullName: r.Name}, true
		}
	}
	return cmf.ClientInfo{}, false
}

// cmfReplyAnswer — ответ свайпом на сообщение бота: если это его вопрос «за кого
// платёж», применяет ответ. Вызывается до маршрутизации реплая в ассистента.
func (b *Bot) cmfReplyAnswer(ctx context.Context, msg *events.Message, text string) bool {
	quotedID := extractQuotedStanzaID(msg)
	if quotedID == "" {
		return false
	}
	watchID, ok := b.cmfWatchByAsk(ctx, quotedID)
	if !ok {
		return false
	}
	return b.applyCmfWatchAnswer(ctx, msg.Info.Chat, watchID, text, true)
}
