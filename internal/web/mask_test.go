package web

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// TestMaskChannelTarget — таблица масок цели канала для не-админа (спека
// 2026-08-08, решения владельца 2026-08-10; дискриминатор — находка C3,
// 2026-08-11): email — первая руна + ***@домен; telegram — последние 2
// цифры chat_id; webhook — только scheme://host, путь и query часто несут
// токены; неразбираемое — "****". К каждой маске добавлен суффикс
// " ·xxxx" — first4(hex(sha256(target))), детерминирован для фиксированных
// ожиданий ниже.
func TestMaskChannelTarget(t *testing.T) {
	cases := []struct{ kind, target, want string }{
		{alert.ChannelEmail, "oleg@example.com", "o***@example.com ·7953"},
		{alert.ChannelEmail, "a@b.c", "***@b.c ·d648"}, // локальная часть из 1 руны не палится
		{alert.ChannelEmail, "кириллица@почта.рф", "к***@почта.рф ·697b"},
		{alert.ChannelTelegram, "123456789", "****89 ·15e2"},
		{alert.ChannelTelegram, "9", "**** ·1958"}, // короче 3 рун — целиком
		{alert.ChannelWebhook, "https://hooks.example.com/T000/B000/secret", "https://hooks.example.com/… ·e043"},
		{alert.ChannelWebhook, "https://user:pass@hooks.example.com/x", "https://hooks.example.com/… ·45f2"}, // userinfo не в u.Host — не палится, но и не выводится
		{alert.ChannelWebhook, "не-URL", "**** ·633b"},
		{"unknown", "whatever", "**** ·8573"},
	}
	for _, c := range cases {
		if got := maskChannelTarget(c.kind, c.target); got != c.want {
			t.Errorf("maskChannelTarget(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
		}
	}
}

// TestMaskChannelTargetBoundaries — граничные случаи, которых не было в
// основной таблице (находка D1): telegram ровно на пороге "<3 рун" в обе
// стороны, webhook с портом/IPv6-хостом/без хоста, email без "@" и пустая
// цель. Каждый кейс проверяет, что маска не палит исходное значение — не
// только совпадает со снятым руками ожиданием.
func TestMaskChannelTargetBoundaries(t *testing.T) {
	cases := []struct {
		name, kind, target, want string
	}{
		// telegram: len(chat_id)==2 ещё короче порога — маскируется целиком
		// (как len==1 в основной таблице); len==3 — уже достаточно, видны
		// последние 2 цифры.
		{"telegram 2 runes", alert.ChannelTelegram, "12", "**** ·6b51"},
		{"telegram 3 runes", alert.ChannelTelegram, "123", "****23 ·a665"},
		// webhook: порт — часть u.Host, попадает в открытую часть маски как
		// есть (не секрет сам по себе, в отличие от пути/query).
		{"webhook with port", alert.ChannelWebhook, "https://host:8443/x", "https://host:8443/… ·84bc"},
		// webhook: IPv6-хост в квадратных скобках — url.Parse кладёt его в
		// u.Host целиком со скобками, маска показывает как есть.
		{"webhook IPv6 host", alert.ChannelWebhook, "https://[::1]/x", "https://[::1]/… ·11e4"},
		// webhook: строка парсится без ошибки (net/url лоялен к относительным
		// путям и mailto:), но u.Host пуст — тот же безопасный дефолт "****",
		// что и для строк, которые вообще не распарсились.
		{"webhook host-less path parses ok", alert.ChannelWebhook, "/just/a/path", "**** ·3d13"},
		{"webhook host-less mailto parses ok", alert.ChannelWebhook, "mailto:ops@example.com", "**** ·d360"},
		// email: нет "@" вообще — та же ветка at<=0, что и у пустой строки
		// ниже, но target непустой, так что дискриминатор добавляется.
		{"email no at sign", alert.ChannelEmail, "not-an-email", "**** ·eba0"},
		// email: пустая цель — ранний выход в maskChannelTarget до
		// дискриминатора (target == "" в самом начале функции), поэтому суффикса
		// нет вовсе, в отличие от остальных "****"-случаев этого теста.
		{"email empty target", alert.ChannelEmail, "", "****"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := maskChannelTarget(c.kind, c.target)
			if got != c.want {
				t.Errorf("maskChannelTarget(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
			}
			if c.target != "" && strings.Contains(got, c.target) {
				t.Errorf("maskChannelTarget(%q, %q) leaks raw target verbatim: %q", c.kind, c.target, got)
			}
		})
	}
}

// TestMaskChannelTargetDisambiguatesSameHost — находка C3: два webhook-канала
// на общем хосте (типично для Slack/Discord — секрет в пути, не в хосте)
// раньше схлопывались в идентичную маску "scheme://host/…" — оператор не мог
// отличить каналы друг от друга ни в таблице, ни в пикере формы монитора.
// Дискриминатор различает их, оставаясь одинаковым по длине/формату для
// обоих (16 бит от sha256 полного target — коллизия косметическая, не
// секьюрити-риск).
func TestMaskChannelTargetDisambiguatesSameHost(t *testing.T) {
	a := maskChannelTarget(alert.ChannelWebhook, "https://hooks.slack.com/services/T1/B1/xxx")
	b := maskChannelTarget(alert.ChannelWebhook, "https://hooks.slack.com/services/T2/B2/yyy")
	if a == b {
		t.Fatalf("two distinct same-host webhook targets masked identically: %q", a)
	}
	// Хост-часть маски (до дискриминатора) у обоих одинакова — расходятся
	// только суффиксом.
	if a[:len("https://hooks.slack.com/…")] != "https://hooks.slack.com/…" ||
		b[:len("https://hooks.slack.com/…")] != "https://hooks.slack.com/…" {
		t.Fatalf("unexpected mask prefix: %q / %q", a, b)
	}
	if a != "https://hooks.slack.com/… ·4c65" {
		t.Errorf("target 1: got %q, want %q", a, "https://hooks.slack.com/… ·4c65")
	}
	if b != "https://hooks.slack.com/… ·c432" {
		t.Errorf("target 2: got %q, want %q", b, "https://hooks.slack.com/… ·c432")
	}
}

// TestMaskDiscriminatorDeterministicAndOneWay — дискриминатор детерминирован
// (тот же target → та же метка, важно для тестов и для оператора при
// перезагрузке страницы) и не раскрывает исходное значение по построению
// (SHA-256 — однонаправленная функция; 4 hex-символа — лишь его усечённый
// хвост).
// TestAlertDeliveriesRedactionOrderMatters — пин порядка операций в
// alertDeliveriesPage (alerts.go:174-175, находка A1): LastError редактируется
// RedactToken(..., СЫРОЙ target) ДО того, как Target маскируется для
// отображения (maskChannelTarget). Порядок важен буквально: RedactToken ищет
// точное совпадение подстроки, а маска меняет target на другую строку —
// если бы маскировка шла первой, второй вызов искал бы уже замаскированную
// (другую) строку внутри LastError и ничего не находил, оставляя сырой
// адрес/URL в тексте ошибки. Тест прогоняет оба порядка на одних и тех же
// входных данных: «правильный» обязан вычистить target из LastError,
// «обратный» — обязан НЕ вычистить (иначе сам тест не отличает порядки, и
// разворот прошёл бы незамеченным).
func TestAlertDeliveriesRedactionOrderMatters(t *testing.T) {
	const rawTarget = "https://hooks.example.com/T000/B000/order-pin-secret"
	lastErr := "upstream rejected POST to " + rawTarget + ": connection reset"

	// Правильный порядок, как в alertDeliveriesPage: redact по сырому target,
	// маска — потом (и только для отображения, в сам RedactToken не идёт).
	redacted := notify.RedactToken(lastErr, rawTarget)
	if strings.Contains(redacted, rawTarget) {
		t.Fatalf("correct order (redact-then-mask) left target in LastError: %q", redacted)
	}

	// Обратный порядок: маска сначала. RedactToken получает уже
	// замаскированную строку, не сырой target, и не находит его в LastError —
	// секрет остаётся. Это доказывает, что порядок в alerts.go не случаен.
	masked := maskChannelTarget(alert.ChannelWebhook, rawTarget)
	reversed := notify.RedactToken(lastErr, masked)
	if !strings.Contains(reversed, rawTarget) {
		t.Fatalf("test invalid: reversed order (mask-then-redact) unexpectedly stripped the target too — order is no longer distinguishable by this test, adjust fixtures")
	}
}

func TestMaskDiscriminatorDeterministicAndOneWay(t *testing.T) {
	got1 := maskDiscriminator("https://hooks.example.com/T000/B000/secret-abc")
	got2 := maskDiscriminator("https://hooks.example.com/T000/B000/secret-abc")
	if got1 != got2 {
		t.Fatalf("maskDiscriminator not deterministic: %q != %q", got1, got2)
	}
	if len(got1) != len("·")+4 {
		t.Errorf("maskDiscriminator length = %d, want %d (·+4 hex)", len(got1), len("·")+4)
	}
	if strings.Contains(got1, "secret-abc") {
		t.Errorf("maskDiscriminator leaked raw target: %q", got1)
	}
}
