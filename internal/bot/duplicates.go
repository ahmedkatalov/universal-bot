// Самостоятельная уборка памяти: бот находит контакты-дубликаты одного человека
// (разное написание ФИО, другой порядок слов, инициалы) и по команде владельца
// объединяет их. Детектор НАМЕРЕННО консервативен — кластеризует только имена с
// ОДИНАКОВЫМ набором значимых слов, поэтому разные однофамильцы/тёзки с разными
// фамилиями («Ахмед Катаев» и «Ахмед Висаев») в один кластер НЕ попадают.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/db"
)

// dupKey — ключ сравнения имён: нижний регистр, выброшены инициалы (1 буква) и
// знаки, слова отсортированы. «Хадаев Али», «Али Хадаев», «Хадаев Али К.» → один
// ключ «али хадаев». Разные фамилии дают разные ключи — не схлопнутся.
func dupKey(name string) string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(name)) {
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) })
		if len([]rune(w)) <= 1 {
			continue // инициал/мусор
		}
		words = append(words, w)
	}
	sort.Strings(words)
	return strings.Join(words, " ")
}

// duplicateClusters группирует контакты по dupKey и возвращает только группы с
// 2+ участниками. Внутри группы первым идёт тот, кого лучше ОСТАВИТЬ: больше
// привязанных записей, затем более полное (длинное) имя, затем по алфавиту.
func duplicateClusters(stats []db.ContactStat) [][]db.ContactStat {
	byKey := map[string][]db.ContactStat{}
	var order []string
	for _, s := range stats {
		k := dupKey(s.Name)
		if k == "" {
			continue
		}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], s)
	}
	var clusters [][]db.ContactStat
	for _, k := range order {
		group := byKey[k]
		if len(group) < 2 {
			continue
		}
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Records != group[j].Records {
				return group[i].Records > group[j].Records
			}
			ri, rj := []rune(group[i].Name), []rune(group[j].Name)
			if len(ri) != len(rj) {
				return len(ri) > len(rj)
			}
			return group[i].Name < group[j].Name
		})
		clusters = append(clusters, group)
	}
	return clusters
}

// reviewDuplicatesTool — «разберись с дублями / почисти память». Без apply —
// показывает найденные группы дублей и предлагает объединить. С apply=true —
// объединяет каждую группу сам (оставляя того, у кого записей больше). Только
// владельцу/админам.
func (b *Bot) reviewDuplicatesTool() ai.Tool {
	return ai.Tool{
		Name: "review_duplicates",
		Description: "Находит контакты-ДУБЛИКАТЫ одного человека (разное написание ФИО, порядок слов, инициалы) и " +
			"помогает навести порядок в памяти. Вызывай при 'проверь дубли', 'разберись с памятью', 'есть ли задвоенные " +
			"клиенты', 'почисти дубликаты'. По умолчанию только ПОКАЗЫВАЕТ группы и предлагает объединить. Если владелец " +
			"говорит 'объедини их сам', 'почисти дубли', 'слей все' — вызови с apply=true, и бот объединит каждую группу " +
			"(оставит того, у кого больше записей). Разные люди с разными фамилиями в группу не попадают.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"apply": map[string]any{"type": "boolean", "description": "true — сразу объединить найденные группы; false/пусто — только показать"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Apply bool `json:"apply"`
			}
			_ = json.Unmarshal(input, &args)

			stats, err := b.db.ContactsWithCounts(ctx)
			if err != nil {
				return "", fmt.Errorf("не удалось прочитать список клиентов: %w", err)
			}
			clusters := duplicateClusters(stats)
			if len(clusters) == 0 {
				return "Дубликатов среди клиентов не нашёл — в памяти чисто.", nil
			}

			if !args.Apply {
				var sb strings.Builder
				fmt.Fprintf(&sb, "Нашёл похожие на дубли группы (%d):\n", len(clusters))
				for _, g := range clusters {
					names := make([]string, 0, len(g))
					for _, c := range g {
						names = append(names, fmt.Sprintf("«%s» (%d зап.)", c.Name, c.Records))
					}
					fmt.Fprintf(&sb, "• %s → оставить «%s»\n", strings.Join(names, " = "), g[0].Name)
				}
				sb.WriteString("\nОбъединить их? Скажи «объедини» (или «почисти дубли сам») — сделаю.")
				return sb.String(), nil
			}

			merged, groups := 0, 0
			var report []string
			for _, g := range clusters {
				keep := g[0].Name
				combined := false
				for _, c := range g[1:] {
					moved, fromC, toC, err := b.db.MergeContacts(ctx, c.Name, keep)
					if err != nil {
						continue // не нашёлся/неоднозначно — пропускаем, не падаем
					}
					b.aliases.Add(fromC, toC)
					merged += moved
					combined = true
				}
				if combined {
					groups++
					report = append(report, "«"+keep+"»")
				}
			}
			if groups == 0 {
				return "Нашёл группы дублей, но объединить не вышло — проверь имена вручную.", nil
			}
			return fmt.Sprintf("Навёл порядок: объединил %d групп(ы) дублей, перенёс %d записей. Оставил: %s.",
				groups, merged, strings.Join(report, ", ")), nil
		},
	}
}
