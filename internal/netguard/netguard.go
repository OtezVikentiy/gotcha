package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

var ErrBlockedTarget = errors.New("netguard: target resolves to a blocked (private/loopback/link-local) address")

// cgnatRange — RFC 6598 Carrier-Grade NAT / shared address space (100.64.0.0/10).
// net.IP.IsPrivate() этот диапазон НЕ покрывает, а часть облаков отдаёт оттуда
// метадату (например Alibaba/Oracle — 100.100.100.200), поэтому режем явно,
// иначе остаётся SSRF-обход фильтра приватных адресов.
var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// nat64Range — well-known NAT64-префикс (RFC 6052, 64:ff9b::/96). Встраивает
// IPv4 в последние 4 байта адреса: 64:ff9b::10.0.0.1 через NAT64-шлюз
// маршрутизируется в приватный 10.0.0.1. Без явной проверки встроенного адреса
// это обход SSRF-фильтра приватных диапазонов через NAT64.
var nat64Range = &net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}

// sixToFourRange — 6to4 (RFC 3056, 2002::/16). Встраивает IPv4 в байты 2..5:
// 2002:7f00:1:: == 6to4 для 127.0.0.1. Через 6to4-релей это тот же вектор
// обхода фильтра приватных/служебных адресов, что и NAT64.
var sixToFourRange = &net.IPNet{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)}

// ipv4CompatRange — устаревшие IPv4-compatible IPv6-адреса (RFC 4291 §2.5.5.1,
// формально deprecated), ::a.b.c.d = ::/96 с IPv4 в младших 4 байтах:
// ::7f00:1 == 127.0.0.1. Первые 12 байт нулевые. Исторически такие адреса
// автоматически туннелировались в IPv4, поэтому встроенный адрес может быть
// приватным/loopback — тот же вектор обхода SSRF-фильтра, что и NAT64/6to4.
// Особые случаи ::(unspecified) и ::1(loopback) в этот диапазон формально
// попадают, но извлечённый из них IPv4 (0.0.0.0 и 0.0.0.1) — мусор, поэтому
// их исключаем и оставляем штатным проверкам ниже (IsUnspecified/IsLoopback).
var ipv4CompatRange = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(96, 128)}

// IsBlockedIP — адрес во внутреннем/служебном диапазоне, запросы к которому из
// мультитенантных чекеров/вебхуков недопустимы (SSRF к метадате облака,
// внутренним сервисам, loopback).
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	// Переходные IPv6-префиксы, встраивающие IPv4: адрес выглядит как публичный
	// IPv6, но через шлюз/релей уходит на встроенный IPv4, который может быть
	// приватным/loopback. Извлекаем встроенный IPv4 и рекурсивно прогоняем его
	// через тот же фильтр — иначе NAT64/6to4 остаётся обходом SSRF-защиты.
	// (IPv4-mapped ::ffff:0:0/96 отдельно не обрабатываем: ip.To4() выше уже
	// нормализует такие адреса в 4-байтовые, поэтому они покрыты обычными
	// проверками ниже.)
	if ip.To4() == nil {
		if ip16 := ip.To16(); ip16 != nil {
			if nat64Range.Contains(ip16) {
				return IsBlockedIP(net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]))
			}
			if sixToFourRange.Contains(ip16) {
				return IsBlockedIP(net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5]))
			}
			// IPv4-compatible ::a.b.c.d (::/96). Исключаем ::(unspecified) и
			// ::1(loopback) — их встроенный «IPv4» бессмыслен, а сами адреса
			// и так режутся ниже. У остальных извлекаем IPv4 и рекурсивно
			// прогоняем через фильтр (иначе ::7f00:1 == 127.0.0.1 проходит).
			if ipv4CompatRange.Contains(ip16) && !ip.IsUnspecified() && !ip.IsLoopback() {
				return IsBlockedIP(net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]))
			}
		}
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		cgnatRange.Contains(ip)
}

// control для net.Dialer.Control: вызывается ПОСЛЕ резолва, до соединения, на
// каждый фактический адрес — устойчиво к DNS-rebind и к редиректам.
func control(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if IsBlockedIP(ip) {
		return ErrBlockedTarget
	}
	return nil
}

// Dialer возвращает *net.Dialer, который при allowPrivate=false режет
// соединения к приватным/служебным адресам через Control (проверка на
// фактический IP после резолва, до коннекта). Таймаут соединения намеренно не
// задан — его контролирует вызывающий через ctx (так делает TCP-чекер, где
// дедлайн приходит из таймаута монитора).
func Dialer(allowPrivate bool) *net.Dialer {
	d := &net.Dialer{}
	if !allowPrivate {
		d.Control = control
	}
	return d
}

func DialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		d.Control = control
	}
	return d.DialContext
}

func SafeHTTPClient(allowPrivate bool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       DialContext(allowPrivate),
		},
	}
}
