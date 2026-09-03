package guards

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// webhookGoldenFiles — золотые файлы тела issue-алерта (E3, заморозка
// контракта вебхука): внутри internal/notify/testdata, потому что их
// снимает internal/notify/webhook_golden_test.go с РЕАЛЬНОГО пути
// (escalation.Dispatch -> notify.WebhookSender.Send). Порядок здесь ОБЯЗАН
// совпадать с порядком ```json-блоков в internal/docs/{ru,en}/alerts.md,
// раздел «Формат тела вебхука» / «Webhook body format»: сначала пример «с
// деталями», затем «без деталей».
var webhookGoldenFiles = []string{
	filepath.Join("internal", "notify", "testdata", "webhook_body_details.json"),
	filepath.Join("internal", "notify", "testdata", "webhook_body_redacted.json"),
}

// jsonFenceRe находит содержимое ```json-блоков markdown. (?s) — точка ловит
// перевод строки внутри блока; нежадный (.*?) — чтобы не схватить сразу все
// блоки страницы одним совпадением.
var jsonFenceRe = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// TestWebhookBodyDocsMatchGolden — E3 (заморозка контракта): пример JSON в
// разделе «Формат тела вебхука» обеих локалей alerts.md обязан побайтово (по
// каноническому представлению) совпадать с золотым файлом, который реально
// проходит через escalation.Dispatch и notify.WebhookSender.Send
// (internal/notify/webhook_golden_test.go). Без этого сторожа доки и код
// расходятся молча: тест в internal/notify замораживает то, что ОТПРАВЛЯЕТСЯ,
// а не то, что НАПИСАНО в документации — пример мог быть подправлен от руки
// и разъехаться с реальным контрактом.
func TestWebhookBodyDocsMatchGolden(t *testing.T) {
	tree := Load(t)

	golden := make([]string, len(webhookGoldenFiles))
	for i, rel := range webhookGoldenFiles {
		raw, err := os.ReadFile(filepath.Join(tree.Root, rel))
		if err != nil {
			t.Fatalf("blind guard: golden file %s not found — %v", rel, err)
		}
		golden[i] = canonicalizeJSONForTest(t, rel, raw)
	}

	for _, lang := range []string{"ru", "en"} {
		docPath := filepath.Join(tree.Root, "internal", "docs", lang, "alerts.md")
		doc, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatal(err)
		}
		matches := jsonFenceRe.FindAllStringSubmatch(string(doc), -1)
		if len(matches) != len(webhookGoldenFiles) {
			t.Fatalf("%s/alerts.md: found %d ```json block(s), want %d (details, then redacted) — раздел «Формат тела вебхука» разъехался с золотыми файлами",
				lang, len(matches), len(webhookGoldenFiles))
		}
		for i, m := range matches {
			docCanon := canonicalizeJSONForTest(t, docPath, []byte(m[1]))
			if docCanon != golden[i] {
				t.Errorf("%s/alerts.md: json block #%d != %s\n--- doc ---\n%s\n--- golden ---\n%s",
					lang, i+1, webhookGoldenFiles[i], docCanon, golden[i])
			}
		}
	}
}

// canonicalizeJSONForTest перепечатывает JSON с отсортированными ключами и
// отступами (json.Marshal сам сортирует ключи map[string]any), чтобы
// сравнение не зависело от форматирования исходника и печатало читаемую
// дельту при расхождении.
func canonicalizeJSONForTest(t *testing.T, source string, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: invalid json: %v\n%s", source, err, raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", source, err)
	}
	return string(b)
}
