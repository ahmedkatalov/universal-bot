// «Молчи в группе»: владелец может по-человечески сказать боту замолчать в
// конкретной группе или во всех. Бот перестаёт писать САМ (вопросы «чей чек»,
// «у кого наличка», предупреждения, проактивные реплики), но продолжает молча
// вести учёт и отвечает, когда к нему обращаются напрямую. Настройка переживает
// рестарт (bot_settings). Это и есть «стоп-кран» на болтовню, которого раньше не
// было — полностью выключить бота можно только снаружи, но заткнуть его в чатах
// владелец теперь может сам.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/ai"
)

const mutedGroupsSettingKey = "muted_groups"
const muteAllSentinel = "*"

// loadMutedGroups восстанавливает список «молчаливых» групп из настроек.
func (b *Bot) loadMutedGroups(ctx context.Context) {
	raw, err := b.db.SettingGet(ctx, mutedGroupsSettingKey)
	if err != nil {
		fmt.Println("Не удалось прочитать список молчаливых групп:", err)
		return
	}
	b.mutedMu.Lock()
	defer b.mutedMu.Unlock()
	b.muted = map[string]bool{}
	for _, j := range strings.Split(raw, ",") {
		if j = strings.TrimSpace(j); j != "" {
			b.muted[j] = true
		}
	}
}

// groupSilent — должен ли бот МОЛЧАТЬ (не писать сам) в этой группе.
func (b *Bot) groupSilent(jid types.JID) bool {
	b.mutedMu.Lock()
	defer b.mutedMu.Unlock()
	return b.muted[muteAllSentinel] || b.muted[jid.String()]
}

// setMuted включает/выключает молчание для группы (или для всех: key="*") и
// сохраняет в настройки.
func (b *Bot) setMuted(ctx context.Context, key string, mute bool) error {
	b.mutedMu.Lock()
	if mute {
		b.muted[key] = true
	} else {
		delete(b.muted, key)
	}
	keys := make([]string, 0, len(b.muted))
	for k := range b.muted {
		keys = append(keys, k)
	}
	b.mutedMu.Unlock()
	sort.Strings(keys)
	return b.db.SettingSet(ctx, mutedGroupsSettingKey, strings.Join(keys, ","))
}

// muteGroupTool — «молчи в группе Отче / не пиши сюда / помолчи везде» и обратно.
func (b *Bot) muteGroupTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "set_group_silence",
		Description: "Включает или выключает МОЛЧАНИЕ бота в группе: перестать писать САМ (вопросы «чей чек», " +
			"«у кого наличка», предупреждения о суммах, проактивные реплики). Учёт продолжит идти молча, на прямое " +
			"обращение бот всё равно ответит. Вызывай, когда владелец говорит «молчи в группе Отче», «не пиши сюда/в эту " +
			"группу», «хватит писать в …», «помолчи везде», «остановись» (silence=true), либо «можешь снова писать в …», " +
			"«верни вопросы», «говори» (silence=false). group — название группы; пусто = ЭТА группа; «все»/«везде» = все группы.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"silence": map[string]any{"type": "boolean", "description": "true — замолчать; false — снова можно писать"},
				"group":   map[string]any{"type": "string", "description": "Название группы; пусто = текущая; «все»/«везде» = все группы"},
			},
			"required": []string{"silence"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Silence bool   `json:"silence"`
				Group   string `json:"group"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			g := strings.ToLower(strings.TrimSpace(args.Group))

			// Все группы.
			if g == "все" || g == "всех" || g == "везде" || g == "all" || g == "*" {
				if err := b.setMuted(ctx, muteAllSentinel, args.Silence); err != nil {
					return "", err
				}
				if args.Silence {
					return "Молчу во ВСЕХ группах — сам писать не буду (учёт идёт, на обращение отвечу). Скажи «можешь снова писать», когда вернуть.", nil
				}
				return "Снова могу писать в группах (вопросы и предупреждения вернул).", nil
			}

			// Конкретная группа: по имени или текущая.
			var target types.JID
			if g != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				target = jid
			} else if chat.Server == types.GroupServer {
				target = chat
			} else {
				return "В какой группе молчать? Назови группу (или скажи «везде»).", nil
			}
			if err := b.setMuted(ctx, target.String(), args.Silence); err != nil {
				return "", err
			}
			name := target.String()
			if groups := b.joinedGroups(ctx); groups[target] != "" {
				name = groups[target]
			}
			if args.Silence {
				return fmt.Sprintf("Молчу в «%s» — сам туда ничего не пишу. Учёт продолжаю, на прямой вопрос отвечу. Вернуть — «можешь снова писать в %s».", name, name), nil
			}
			return fmt.Sprintf("Снова могу писать в «%s».", name), nil
		},
	}
}
