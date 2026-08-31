package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestLiveKeyFor — правило выбора ключа для сценария (§7 спеки): первый живой
// ключ нужного типа → иначе первый живой legacy → иначе пусто.
//
// Фолбэк на legacy — это и есть переход без простоя: проект, у которого типов
// ещё нет, продолжает видеть рабочий DSN везде, где видел.
func TestLiveKeyFor(t *testing.T) {
	keys := []org.Key{
		{ID: 1, PublicKey: "revoked-agent", Kind: org.KindAgent, Revoked: true},
		{ID: 2, PublicKey: "legacy", Kind: org.KindLegacy},
		{ID: 3, PublicKey: "browser", Kind: org.KindBrowser},
		{ID: 4, PublicKey: "browser2", Kind: org.KindBrowser},
	}
	cases := []struct {
		kind org.KeyKind
		want string
	}{
		{org.KindBrowser, "browser"}, // первый живой нужного типа
		{org.KindAgent, "legacy"},    // нужного типа живого нет → legacy
		{org.KindServer, "legacy"},   // и здесь
	}
	for _, c := range cases {
		if got := liveKeyFor(keys, c.kind); got != c.want {
			t.Errorf("liveKeyFor(%q) = %q, ожидался %q", c.kind, got, c.want)
		}
	}
	// Отозванный legacy не спасает: выдавать мёртвый ключ хуже, чем показать
	// пустое состояние с кнопкой «создать».
	dead := []org.Key{{ID: 1, PublicKey: "legacy", Kind: org.KindLegacy, Revoked: true}}
	if got := liveKeyFor(dead, org.KindAgent); got != "" {
		t.Errorf("liveKeyFor по отозванным = %q, ожидалась пустая строка", got)
	}
	// Ключ с незаданным типом трактуется как legacy: столбец с дефолтом
	// legacy — то же самое состояние, что и строка, вставленная старым кодом.
	untyped := []org.Key{{ID: 1, PublicKey: "old"}}
	if got := liveKeyFor(untyped, org.KindServer); got != "old" {
		t.Errorf("liveKeyFor по ключу без типа = %q, ожидался old", got)
	}
}

// TestSetupSnippetsPerPlatformDSN — JS получает браузерный DSN, серверные
// языки — серверный. Иначе онбординг сам учит ставить серверный ключ в
// браузер.
func TestSetupSnippetsPerPlatformDSN(t *testing.T) {
	sn := setupSnippets("go", "dsn-browser", "dsn-server")
	byLang := map[string]string{}
	for _, s := range sn {
		byLang[s.Lang] = s.Code
	}
	if !strings.Contains(byLang["JavaScript"], "dsn-browser") {
		t.Error("JS-сниппет получил не браузерный DSN")
	}
	for _, lang := range []string{"Go", "PHP", "Python"} {
		if !strings.Contains(byLang[lang], "dsn-server") {
			t.Errorf("%s-сниппет получил не серверный DSN", lang)
		}
	}
}

// TestSetupSnippetsUseRealSDKs фиксирует блокер онбординга: страница подключения
// раздавала сниппеты с пакетами, которых не существует
// (gitflic.ru/otezvikentiy/gotcha-go, @gotcha/browser, Gotcha\init) и без единой
// команды установки. Первый же шаг нового пользователя упирался в 404 от
// go get / npm install — притом что /docs/sdk прямо говорит обратное: своего
// протокола у Gotcha нет, ставится официальный Sentry SDK нужного языка.
func TestSetupSnippetsUseRealSDKs(t *testing.T) {
	const dsn = "https://pub@gotcha.example/7"
	snips := setupSnippets("go", dsn, dsn)

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
		snips := setupSnippets(platform, "dsn", "dsn")
		if len(snips) == 0 || snips[0].Lang != wantLang {
			t.Errorf("платформа %q: первый сниппет %v, want %s", platform, snips[0].Lang, wantLang)
		}
		if len(snips) != 4 {
			t.Errorf("платформа %q: сниппетов %d, want 4", platform, len(snips))
		}
	}

	// Неизвестная платформа («other») — показываем все, без дублей.
	snips := setupSnippets("other", "dsn", "dsn")
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
	for _, sn := range setupSnippets("go", "https://pub@host/1", "https://pub@host/1") {
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

// TestSubjectPurgeVMWarnsAboutInertCriteria — форма удаления ПДн обязана
// предупреждать, что при включённом обезличивании поиск по email и IP не найдёт
// ничего: иначе владелец орга вводит email субъекта, получает «не найдено» и не
// понимает почему.
//
// Итог самого удаления сюда больше не входит — он показывается общим
// сообщением о результате действия (flash), а не query-параметром: параметр
// залипал в адресе при F5, и ссылку вида ?purged=9999 можно было подсунуть
// владельцу, показав ему выдуманное число.
func TestSubjectPurgeVMWarnsAboutInertCriteria(t *testing.T) {
	ctx := context.Background()

	both := (&Handler{ScrubEmail: true, ScrubIP: true}).subjectPurgeVM(ctx, 1)
	if both.InertKey() != "org.gdpr.purge.inert" {
		t.Errorf("оба критерия мертвы: ключ %q", both.InertKey())
	}
	onlyEmail := (&Handler{ScrubEmail: true}).subjectPurgeVM(ctx, 1)
	if onlyEmail.InertKey() != "org.gdpr.purge.inert_email" {
		t.Errorf("мёртв только email: ключ %q", onlyEmail.InertKey())
	}
	// Скрубинг выключен — предупреждать не о чем.
	off := (&Handler{}).subjectPurgeVM(ctx, 1)
	if off.InertKey() != "" {
		t.Errorf("при выключенном скрубинге предупреждение не нужно: %q", off.InertKey())
	}
}
