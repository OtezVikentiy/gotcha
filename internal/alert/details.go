package alert

import (
	"net"
	"net/mail"
	"net/url"
	"strings"
)

// Решение — по получателю (домену/хосту), не по транспорту: у Telegram
// получатель — chat_id без домена, и канал остаётся внешним всегда.
type DetailPolicy struct {
	// Совпадение суффиксом по границе метки: «corp.example» покрывает
	// «mail.corp.example», но не «evilcorp.example».
	trusted []string
	// GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED — оператор разрешил детали всем
	// каналам, включая Telegram.
	all bool
}

// baseURL хост доверен автоматически, родительский домен — нет: подъём на
// уровень вверх дал бы доверие всему github.io при инстансе на *.github.io.
func NewDetailPolicy(baseURL string, trusted []string, allowAll bool) DetailPolicy {
	p := DetailPolicy{all: allowAll}
	if h := hostOfURL(baseURL); h != "" {
		p.trusted = append(p.trusted, h)
	}
	for _, t := range trusted {
		if n := normalizeHost(t); n != "" {
			p.trusted = append(p.trusted, n)
		}
	}
	return p
}

func (p DetailPolicy) AllowsDetails(c Channel) bool {
	if p.all {
		return true
	}
	// Перед разбором получателя: chat_id Telegram не домен, и без этой
	// отметки его контур никакой проверкой адреса не подтвердить.
	if c.Trusted {
		return true
	}
	switch c.Kind {
	case ChannelEmail:
		return p.trusts(emailDomain(c.Target))
	case ChannelWebhook:
		return p.trusts(hostOfURL(c.Target))
	default:
		// Получателя разобрать нечем — fail-closed, новый тип канала не должен
		// молча возить ПДн наружу.
		return false
	}
}

// Пустой хост (адрес не разобрался) считается недоверенным.
func (p DetailPolicy) trusts(host string) bool {
	if host == "" {
		return false
	}
	if isLocalHost(host) {
		// Петля, приватная сеть, нероутируемый спец-домен: получатель заведомо
		// внутри инфраструктуры оператора, наружу это не уезжает физически.
		return true
	}
	for _, t := range p.trusted {
		if host == t || strings.HasSuffix(host, "."+t) {
			return true
		}
	}
	return false
}

// mail.ParseAddress сначала; при неудаче — домен после ПОСЛЕДНЕГО '@', не
// первого: локальная часть в кавычках может содержать '@' сама.
func emailDomain(addr string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(addr)); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			return normalizeHost(a.Address[i+1:])
		}
	}
	i := strings.LastIndex(addr, "@")
	if i < 0 {
		return ""
	}
	return normalizeHost(addr[i+1:])
}

// Пустая строка, если это не абсолютный http(s)-адрес — тогда доверять нечему.
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	// url.Parse отдаёт хост и для «//evil.example/x», и для «ftp://…» — здесь
	// схема проверяется, а не только наличие хоста (fail-closed).
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := u.Hostname() // без порта, IPv6 без скобок
	return normalizeHost(host)
}

// Нижний регистр, без обрамляющих пробелов и без корневой точки
// ("example.com." == "example.com").
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.Trim(h, "[]") // literal IPv6 из настройки
	return h
}

// Доменные зоны, не маршрутизируемые в публичный интернет.
var localTLDs = []string{".local", ".internal", ".localhost", ".home.arpa", ".lan"}

func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	for _, tld := range localTLDs {
		if strings.HasSuffix(host, tld) {
			return true
		}
	}
	return false
}
