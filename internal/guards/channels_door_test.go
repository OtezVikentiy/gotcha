package guards

import (
	"regexp"
	"strings"
	"testing"
)

// channelsDoorFuncHeaderRe — заголовок метода Handler вида
// "func (h *Handler) Name(" в начале строки: используется, чтобы знать, В
// КАКОЙ функции найден вызов h.Alerts.Channels(...) — allowlist сторожа
// ниже разрешает конкретные ИМЕНА функций, а не строки текста (в отличие от
// большинства сторожей пакета, которые сверяются построчно).
var channelsDoorFuncHeaderRe = regexp.MustCompile(`^func \(h \*Handler\) (\w+)\(`)

// channelsDoorAllowlist — единственные функции internal/web, которым
// разрешено звать h.Alerts.Channels(...) напрямую, в обход
// channelsForView (internal/web/operate.go, находка B1). До находки B1
// маскировка Target/Secret для не-admin была продублирована по каждому
// сайту рендера отдельно — новый сайт, забывший про маску, тихо показал бы
// оператору сырой адрес или секрет канала. Теперь дверь одна, и этот
// список — единственные легитимные исключения:
var channelsDoorAllowlist = []Exemption{
	{Value: "channelsForView", Why: "сама дверь — читает сырые каналы из БД и маскирует Target/зануляет Secret для !canManage, прежде чем отдать их дальше", Finding: "B1"},
	{Value: "alertsChannelUpdate", Why: "admin channel-CRUD: ищет канал по channel_id из формы, чтобы взять его текущий Kind (тип каналом не редактируется) — requireProjectRole выше уже требует owner/admin, санировать для оператора нечего", Finding: "B1"},
	{Value: "alertsChannelDelete", Why: "admin channel-CRUD: проверяет принадлежность channel_id проекту перед удалением — requireProjectRole выше уже требует owner/admin", Finding: "B1"},
	{Value: "alertsChannelTest", Why: "admin channel-CRUD: ищет канал по channel_id, чтобы отправить тестовое сообщение на его сырой Target — requireProjectRole выше уже требует owner/admin", Finding: "B1"},
	{Value: "gettingStarted", Why: "issues.go:182 — результат используется только как len(channels) > 0 для чек-листа онбординга; ни Target, ни Secret не покидают эту функцию, санировать нечего (count-only)", Finding: "B1"},
}

// maxChannelsDoorAllowlist — потолок списка: пять записей на момент
// закрытия находки B1 (дверь сама + три admin-CRUD хендлера, которым нужен
// сырой канал по ID, + один count-only вызов). Расти должен только вместе с
// осознанным добавлением новой легитимной причины читать каналы напрямую.
const maxChannelsDoorAllowlist = 5

// TestChannelsGoThroughOneDoor — сорс-guard находки B1: любой вызов
// h.Alerts.Channels(...) в internal/web вне channelsDoorAllowlist —
// нарушение. Раньше "следующая страница, которая отрендерит канал" могла
// незаметно забыть про маскировку — теперь это красный тест, а не находка
// аудита постфактум.
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
