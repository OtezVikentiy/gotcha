package alert

import (
	"net"
	"net/mail"
	"net/url"
	"strings"
)

// DetailPolicy решает, можно ли раскрыть детали события конкретному получателю
// уведомления: текст ошибки, стектрейс, имя транзакции. Всё это может нести
// персональные данные, и его уход за пределы контура оператора — потенциально
// трансграничная передача (152-ФЗ ст. 12).
//
// Раньше решение принималось по ТРАНСПОРТУ: telegram и webhook считались
// внешними, email — своим. Обе половины этого правила неверны.
//
// Почтовый ящик на публичном сервисе — это чужая инфраструктура ровно в той же
// мере, что и Telegram: уведомление на @gmail.com уезжало с полным текстом
// ошибки, потому что «это же email». А вебхук на собственный сервер во
// внутренней сети деталей не получал, хотя не покидал контура вовсе — и чтобы
// их получить, оператору приходилось включать GOTCHA_EXTERNAL_CHANNEL_DETAILS
// глобально, открывая заодно и Telegram. Правило по транспорту одновременно
// пропускало то, что должно было задержать, и задерживало то, что могло
// пропустить.
//
// Решение принимается по ПОЛУЧАТЕЛЮ: домену почтового адреса, хосту вебхука.
// У Telegram получатель — chat_id, домена у него нет, а сервис по определению
// вне контура оператора: он остаётся внешним всегда.
type DetailPolicy struct {
	// trusted — нормализованные хосты своего контура. Совпадение суффиксом по
	// границе метки, а не по строке: «corp.example» покрывает
	// «mail.corp.example», но не «evilcorp.example».
	trusted []string
	// all — GOTCHA_EXTERNAL_CHANNEL_DETAILS: оператор заявил законное основание
	// и разрешил детали кому угодно, включая Telegram.
	all bool
}

// NewDetailPolicy собирает политику.
//
// baseURL — публичный адрес инстанса. Его ХОСТ (и поддомены этого хоста)
// доверенный по умолчанию: вебхук на соседний сервис того же инстанса — это
// заведомо свой контур, и настройки для этого требовать незачем.
//
// Родительский домен НЕ доверяется автоматически, хотя соблазн велик: инстанс
// на gotcha.corp.example почти наверняка шлёт почту на @corp.example. Но тот
// же подъём на уровень вверх от gotcha.github.io выдал бы доверие всему
// github.io, то есть любому чужому проекту на том же хостинге. Отличить
// организационный домен от публичного суффикса без списка суффиксов нельзя, а
// тащить и обновлять такой список ради одной проверки — цена выше пользы.
// Поэтому домен организации указывается явно, через trusted
// (GOTCHA_TRUSTED_RECIPIENTS).
//
// allowAll — глобальное разрешение (GOTCHA_EXTERNAL_CHANNEL_DETAILS).
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

// AllowsDetails сообщает, можно ли слать в этот канал полный payload.
func (p DetailPolicy) AllowsDetails(c Channel) bool {
	if p.all {
		return true
	}
	// Отметка на самом канале: оператор заявил, что этот получатель — его
	// собственный. Стоит ПЕРЕД разбором получателя, потому что смысл её в
	// том, что разобрать его нечем: chat_id Telegram не домен, и никакая
	// проверка адреса подтвердить принадлежность к контуру не может. Ставится
	// только руками, в форме канала, по одному каналу за раз — в отличие от
	// all, который открывает детали всему сразу.
	if c.Trusted {
		return true
	}
	switch c.Kind {
	case ChannelEmail:
		return p.trusts(emailDomain(c.Target))
	case ChannelWebhook:
		return p.trusts(hostOfURL(c.Target))
	default:
		// telegram и всё, что появится позже: получателя разобрать нечем,
		// значит и подтвердить, что он внутри контура, нечем. Fail-closed —
		// новый тип канала не должен молча начать возить ПДн наружу.
		return false
	}
}

// trusts — доверенный ли это хост. Пустой (адрес не разобрался) — нет.
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

// emailDomain — доменная часть адреса, нормализованная.
//
// Сначала пробуем разобрать адрес по RFC 5322: канал принимается валидатором
// через mail.ParseAddress, а тот допускает форму «Ops Team <a@corp.example>».
// Для неё наивный разбор дал бы домен «corp.example>» — с угловой скобкой,
// которая не совпадёт ни с чем, и доверенный получатель тихо перестал бы
// получать детали.
//
// Если адрес не разобрался, берём подстроку после ПОСЛЕДНЕГО '@': локальная
// часть по RFC может содержать его в кавычках, и разбор по первому отдал бы за
// домен кусок локальной части — то есть чужой адрес мог бы притвориться своим.
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

// hostOfURL — хост из URL без порта. Пустая строка, если это не разбирается в
// абсолютный http(s)-адрес: тогда доверять нечему.
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	// Схема проверяется, а не подразумевается: докблок обещает абсолютный
	// http(s), но url.Parse отдаёт хост и для «//evil.example/x», и для
	// «ftp://…». Здесь решается, доверенный ли получатель, и «хост непонятно
	// какого протокола» доверенным считаться не должен — fail-closed.
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := u.Hostname() // без порта, IPv6 без скобок
	return normalizeHost(host)
}

// normalizeHost приводит хост к сравнимому виду: нижний регистр, без
// обрамляющих пробелов и без корневой точки ("example.com." == "example.com").
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.Trim(h, "[]") // literal IPv6 из настройки
	return h
}

// localTLDs — доменные зоны, которые не маршрутизируются в публичный интернет.
var localTLDs = []string{".local", ".internal", ".localhost", ".home.arpa", ".lan"}

// isLocalHost — заведомо внутренний получатель: петля, приватная сеть или
// нероутируемая зона.
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
