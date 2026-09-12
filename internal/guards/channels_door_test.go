package guards

import (
	"regexp"
	"strings"
	"testing"
)

// Разрешает по имени функции, где найден вызов, а не по строке текста —
// в отличие от большинства сторожей пакета.
var channelsDoorFuncHeaderRe = regexp.MustCompile(`^func \(h \*Handler\) (\w+)\(`)

var channelsDoorAllowlist = []Exemption{
	{Value: "channelsForView", Why: "сама дверь — читает сырые каналы из БД и маскирует Target/зануляет Secret для !canManage, прежде чем отдать их дальше", Finding: "B1"},
	{Value: "alertsChannelUpdate", Why: "admin channel-CRUD: ищет канал по channel_id из формы, чтобы взять его текущий Kind (тип каналом не редактируется) — requireProjectRole выше уже требует owner/admin, санировать для оператора нечего", Finding: "B1"},
	{Value: "alertsChannelDelete", Why: "admin channel-CRUD: проверяет принадлежность channel_id проекту перед удалением — requireProjectRole выше уже требует owner/admin", Finding: "B1"},
	{Value: "alertsChannelTest", Why: "admin channel-CRUD: ищет канал по channel_id, чтобы отправить тестовое сообщение на его сырой Target — requireProjectRole выше уже требует owner/admin", Finding: "B1"},
	{Value: "gettingStarted", Why: "issues.go:182 — результат используется только как len(channels) > 0 для чек-листа онбординга; ни Target, ни Secret не покидают эту функцию, санировать нечего (count-only)", Finding: "B1"},
}

// Расти должен только вместе с осознанным добавлением новой легитимной причины.
const maxChannelsDoorAllowlist = 5

func TestChannelsGoThroughOneDoor(t *testing.T) {
	tree := Load(t)
	allowed := ExemptedValues(channelsDoorAllowlist)
	seen := map[string]bool{}

	for _, f := range tree.GoFiles {
		if !strings.HasPrefix(f.Path, "internal/web/") || f.Generated || strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		currentFunc := ""
		for i, line := range strings.Split(f.Body, "\n") {
			if m := channelsDoorFuncHeaderRe.FindStringSubmatch(line); m != nil {
				currentFunc = m[1]
			}
			checked := stripTrailingComment(line)
			if !strings.Contains(checked, "h.Alerts.Channels(") {
				continue
			}
			seen[currentFunc] = true
			if allowed[currentFunc] {
				continue
			}
			t.Errorf("%s:%d: h.Alerts.Channels(...) вызван напрямую внутри %s, в обход channelsForView — "+
				"либо переведите вызов на дверь (channelsForView), либо добавьте функцию в "+
				"channelsDoorAllowlist с обоснованием, почему ей нужен сырой список (находка B1)",
				f.Path, i+1, currentFunc)
		}
	}

	CheckExemptions(t, "TestChannelsGoThroughOneDoor", channelsDoorAllowlist, maxChannelsDoorAllowlist, seen)
}
