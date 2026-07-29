package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetupSnippetsUseRealSDKs фиксирует блокер онбординга: страница подключения
// раздавала сниппеты с пакетами, которых не существует
// (gitflic.ru/otezvikentiy/gotcha-go, @gotcha/browser, Gotcha\init) и без единой
// команды установки. Первый же шаг нового пользователя упирался в 404 от
// go get / npm install — притом что /docs/sdk прямо говорит обратное: своего
// протокола у Gotcha нет, ставится официальный Sentry SDK нужного языка.
func TestSetupSnippetsUseRealSDKs(t *testing.T) {
	const dsn = "https://pub@gotcha.example/7"
	snips := setupSnippets("go", dsn)

	if len(snips) != 4 {
		t.Fatalf("сниппетов %d, want 4 (go/php/javascript/python)", len(snips))
	}

	// Несуществующих пакетов не должно остаться ни в одном блоке.
	ghosts := []string{"gotcha-go", "@gotcha/browser", `Gotcha\init`, "gotcha.Init"}
	for _, sn := range snips {
		joined := sn.Install + "\n" + sn.Code
		for _, g := range ghosts {
			if strings.Contains(joined, g) {
				t.Errorf("сниппет %s ссылается на несуществующий пакет %q", sn.Lang, g)
			}
		}
		if sn.Install == "" {
			t.Errorf("у сниппета %s нет команды установки", sn.Lang)
		}
		if !strings.Contains(sn.Code, dsn) {
			t.Errorf("сниппет %s не содержит DSN проекта", sn.Lang)
		}
	}

	// Реальные пакеты на месте.
	want := map[string]string{
		"Go":         "github.com/getsentry/sentry-go",
		"PHP":        "sentry/sentry",
		"JavaScript": "@sentry/browser",
		"Python":     "sentry-sdk",
	}
	for _, sn := range snips {
		pkg, ok := want[sn.Lang]
		if !ok {
			t.Fatalf("неожиданный язык %q", sn.Lang)
		}
		if !strings.Contains(sn.Install, pkg) {
			t.Errorf("установка %s = %q, ожидался пакет %q", sn.Lang, sn.Install, pkg)
		}
	}
}

// TestSetupSnippetsPlatformFirst — платформа, выбранная при создании проекта,
// показывается первой. Раньше страница не знала о ней вовсе: проект на Python
// получал Go/PHP/JS и ни одного питоновского примера.
func TestSetupSnippetsPlatformFirst(t *testing.T) {
	for platform, wantLang := range map[string]string{
		"python":     "Python",
		"php":        "PHP",
		"javascript": "JavaScript",
		"go":         "Go",
	} {
		snips := setupSnippets(platform, "dsn")
		if len(snips) == 0 || snips[0].Lang != wantLang {
			t.Errorf("платформа %q: первый сниппет %v, want %s", platform, snips[0].Lang, wantLang)
		}
		if len(snips) != 4 {
			t.Errorf("платформа %q: сниппетов %d, want 4", platform, len(snips))
		}
	}

	// Неизвестная платформа («other») — показываем все, без дублей.
	snips := setupSnippets("other", "dsn")
	if len(snips) != 4 {
		t.Fatalf("платформа other: сниппетов %d, want 4", len(snips))
	}
	seen := map[string]bool{}
	for _, sn := range snips {
		if seen[sn.Lang] {
			t.Errorf("дубль языка %q", sn.Lang)
		}
		seen[sn.Lang] = true
	}
}

// TestGoSnippetCompiles — Go-сниппет вручается пользователю как есть, значит он
// обязан быть валидным Go. Проверяем, что все использованные пакеты
// импортированы: первая версия правки использовала time.Second без импорта
// "time", то есть выдавала заведомо несобирающийся код.
func TestGoSnippetCompiles(t *testing.T) {
	var code string
	for _, sn := range setupSnippets("go", "https://pub@host/1") {
		if sn.Lang == "Go" {
			code = sn.Code
		}
	}
	if code == "" {
		t.Fatal("Go-сниппет не найден")
	}
	// Каждый использованный пакет должен быть в блоке import.
	imports := code[strings.Index(code, "import ("):strings.Index(code, ")\n\n")]
	for _, pkg := range []string{"log", "time", "sentry-go"} {
		if !strings.Contains(imports, pkg) {
			t.Errorf("пакет %q используется, но не импортирован:\n%s", pkg, code)
		}
	}
	for _, use := range []string{"log.Fatal", "time.Second", "sentry.Init", "sentry.Flush"} {
		if !strings.Contains(code, use) {
			t.Errorf("сниппет не использует %q — импорт был бы лишним", use)
		}
	}
}

// TestSubjectPurgeVMFromQuery — итог удаления ПДн приезжает на страницу
// настроек через ?purged=N. Отличать «удалено N» от «не найдено ничего»
// обязательно: раньше оба исхода выглядели одинаковым молчаливым редиректом.
func TestSubjectPurgeVMFromQuery(t *testing.T) {
	h := &Handler{ScrubEmail: true, ScrubIP: true}

	cases := []struct {
		query      string
		wantDone   bool
		wantPurged int
	}{
		{"", false, 0},
		{"?purged=0", true, 0}, // ноль — тоже результат, показываем
		{"?purged=128", true, 128},
		{"?purged=abc", false, 0}, // мусор игнорируем
		{"?purged=-5", false, 0},  // отрицательное игнорируем
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/orgs/1/settings"+c.query, nil)
		vm := h.subjectPurgeVM(r)
		if vm.Done != c.wantDone || vm.Purged != c.wantPurged {
			t.Errorf("query %q: Done=%v Purged=%d, want Done=%v Purged=%d",
				c.query, vm.Done, vm.Purged, c.wantDone, c.wantPurged)
		}
		// Флаги обезличивания зеркалят конфиг приёма и от query не зависят.
		if !vm.InertEmail || !vm.InertIP {
			t.Errorf("query %q: флаги обезличивания потеряны: %+v", c.query, vm)
		}
	}

	// Скрубинг выключен — предупреждать не о чем.
	off := (&Handler{}).subjectPurgeVM(httptest.NewRequest(http.MethodGet, "/x", nil))
	if off.InertKey() != "" {
		t.Errorf("при выключенном скрубинге предупреждение не нужно: %q", off.InertKey())
	}
}
