package templates

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

const longKey = "abc123" + "xxxxxxxxxxxxxxxxxxxx" + "wxyz"

func TestKeyDisplayID(t *testing.T) {
	live := ProjectKeyView{Key: org.Key{PublicKey: longKey, Revoked: false}, DSN: "https://" + longKey + "@host/1"}
	got := keyDisplayID(live)
	if !strings.HasPrefix(got, "abc123") {
		t.Errorf("keyDisplayID(live) = %q, хочет префикс %q", got, "abc123")
	}
	if !strings.HasSuffix(got, "wxyz") {
		t.Errorf("keyDisplayID(live) = %q, хочет суффикс %q", got, "wxyz")
	}
	if strings.Contains(got, longKey) {
		t.Errorf("keyDisplayID(live) = %q не должен содержать полный ключ %q", got, longKey)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("keyDisplayID(live) = %q должен содержать многоточие U+2026", got)
	}

	revoked := ProjectKeyView{Key: org.Key{PublicKey: longKey, Revoked: true}, DSN: "https://" + longKey + "@host/1"}
	if got := keyDisplayID(revoked); got != longKey {
		t.Errorf("keyDisplayID(revoked) = %q, want полный ключ %q", got, longKey)
	}

	noDSN := ProjectKeyView{Key: org.Key{PublicKey: longKey, Revoked: false}, DSN: ""}
	if got := keyDisplayID(noDSN); got != longKey {
		t.Errorf("keyDisplayID(no DSN) = %q, want полный ключ %q", got, longKey)
	}

	short := ProjectKeyView{Key: org.Key{PublicKey: "shortpk12345", Revoked: false}, DSN: "https://shortpk12345@host/1"}
	if got := keyDisplayID(short); got != "shortpk12345" {
		t.Errorf("keyDisplayID(short) = %q, want %q (без изменений)", got, "shortpk12345")
	}
}

func TestHasLiveKey(t *testing.T) {
	cases := []struct {
		name string
		keys []ProjectKeyView
		want bool
	}{
		{"пусто", nil, false},
		{"только отозванные", []ProjectKeyView{
			{Key: org.Key{ID: 1, Revoked: true}},
			{Key: org.Key{ID: 2, Revoked: true}},
		}, false},
		{"один живой", []ProjectKeyView{{Key: org.Key{ID: 1, Revoked: false}}}, true},
		{"смесь", []ProjectKeyView{
			{Key: org.Key{ID: 1, Revoked: true}},
			{Key: org.Key{ID: 2, Revoked: false}},
		}, true},
	}
	for _, c := range cases {
		if got := hasLiveKey(c.keys); got != c.want {
			t.Errorf("hasLiveKey(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func renderKeyCard(t *testing.T, v ProjectKeyView) string {
	t.Helper()
	var sb strings.Builder
	if err := keyCard(1, v).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func keyCardHead(t *testing.T, card string) string {
	t.Helper()
	start := strings.Index(card, `<div class="key-card-head">`)
	end := strings.Index(card, "</div>")
	if start == -1 || end == -1 || end < start {
		t.Fatalf("malformed key-card-head: %s", card)
	}
	return card[start:end]
}

func TestKeyCardLive(t *testing.T) {
	v := ProjectKeyView{
		Key: org.Key{ID: 42, PublicKey: longKey, Kind: org.KindServer, Revoked: false},
		DSN: "https://" + longKey + "@host/1",
	}
	out := renderKeyCard(t, v)

	if !strings.Contains(out, `class="key-card"`) {
		t.Errorf("нет .key-card: %s", out)
	}
	if !strings.Contains(out, i18nT(t, "project.settings.dsn.copy")) {
		t.Errorf("нет подписи кнопки копирования %q: %s", i18nT(t, "project.settings.dsn.copy"), out)
	}
	if !strings.Contains(out, `class="copy-preview"`) || !strings.Contains(out, v.DSN) {
		t.Errorf("DSN не напечатан видимым текстом: %s", out)
	}
	if !strings.Contains(out, `class="key-revoke-form"`) {
		t.Errorf("нет формы отзыва у живого ключа: %s", out)
	}
	if !strings.Contains(out, "dsn-"+strconv.FormatInt(v.Key.ID, 10)) {
		t.Errorf("id copy-блока не привязан к ключу: %s", out)
	}
	head := keyCardHead(t, out)
	if strings.Contains(head, longKey) {
		t.Errorf("шапка живого ключа с DSN не должна нести полный ключ: %s", head)
	}
}

func TestKeyCardRevoked(t *testing.T) {
	v := ProjectKeyView{
		Key: org.Key{ID: 7, PublicKey: longKey, Kind: org.KindServer, Revoked: true},
		DSN: "https://" + longKey + "@host/1",
	}
	out := renderKeyCard(t, v)

	if !strings.Contains(out, "key-card--revoked") {
		t.Errorf("нет модификатора key-card--revoked: %s", out)
	}
	if !strings.Contains(out, i18nT(t, "project.settings.keys.revoked.note")) {
		t.Errorf("нет пояснения про отозванный ключ: %s", out)
	}
	if strings.Contains(out, "copy-llm") {
		t.Errorf("у отозванного ключа не должно быть кнопки копирования DSN: %s", out)
	}
	if strings.Contains(out, `class="key-revoke-form"`) {
		t.Errorf("у отозванного ключа не должно быть формы отзыва: %s", out)
	}
	if !strings.Contains(out, longKey) {
		t.Errorf("полный ключ отозванной записи должен присутствовать: %s", out)
	}
}

func TestKeyCardLegacy(t *testing.T) {
	v := ProjectKeyView{
		Key: org.Key{ID: 3, PublicKey: longKey, Kind: org.KindLegacy, Revoked: false},
		DSN: "https://" + longKey + "@host/1",
	}
	out := renderKeyCard(t, v)

	if !strings.Contains(out, "/docs/keys") {
		t.Fatalf("нет ссылки /docs/keys: %s", out)
	}
	head := keyCardHead(t, out)
	if strings.Contains(head, "/docs/keys") {
		t.Errorf("ссылка «Что это значит» не должна жить в шапке: %s", head)
	}
	if !strings.Contains(out, `class="key-card-note"`) {
		t.Errorf("ссылка должна жить в отдельном key-card-note: %s", out)
	}
}

func TestKeyCreateFormSegmented(t *testing.T) {
	project := org.Project{ID: 9, OrgID: 1, Slug: "seg", Name: "Seg", Platform: "go"}
	perf := PerfSettingsForm{SampleRate: "1", ApdexMS: "500", NPlusOneMin: "5", SlowDBMs: "300"}
	reg := RegressionSettingsForm{ThresholdPct: "20", RecoveryPct: "10", WindowMinutes: "60", MinSamples: "100", Enabled: true}
	out := renderTo(t, ProjectSettings(project, nil, "", "u@e.com", perf, reg, 30, nil))

	formStart := strings.Index(out, `class="key-create-form"`)
	if formStart == -1 {
		t.Fatalf("форма выпуска ключа не найдена: %s", out)
	}
	formEndRel := strings.Index(out[formStart:], "</form>")
	if formEndRel == -1 {
		t.Fatalf("не закрыта форма выпуска ключа: %s", out)
	}
	form := out[formStart : formStart+formEndRel]

	if !strings.Contains(form, `class="segmented"`) {
		t.Fatalf("нет .segmented: %s", form)
	}
	for _, kind := range []string{"browser", "server", "agent"} {
		if !strings.Contains(form, `value="`+kind+`" required`) {
			t.Errorf("нет radio kind=%s: %s", kind, form)
		}
	}
	for _, cls := range []string{"key-kind-hint--none", "key-kind-hint--browser", "key-kind-hint--server", "key-kind-hint--agent"} {
		if !strings.Contains(form, cls) {
			t.Errorf("нет абзаца %s: %s", cls, form)
		}
	}
	for _, hint := range []string{
		i18nT(t, "project.settings.keys.kind.browser.hint"),
		i18nT(t, "project.settings.keys.kind.server.hint"),
		i18nT(t, "project.settings.keys.kind.agent.hint"),
		i18nT(t, "project.settings.keys.kind.none.hint"),
	} {
		idx := strings.Index(form, hint)
		if idx == -1 {
			t.Fatalf("описание %q не найдено в форме: %s", hint, form)
		}
		labelStart := strings.LastIndex(form[:idx], "<label>")
		labelEnd := strings.LastIndex(form[:idx], "</label>")
		if labelStart != -1 && labelStart > labelEnd {
			t.Errorf("описание %q сидит внутри <label>: %s", hint, form)
		}
	}
}
