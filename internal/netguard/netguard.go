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

// RFC 6598 CGNAT: IsPrivate() его не покрывает, а часть облаков отдаёт оттуда
// метадату (напр. Alibaba/Oracle) — без среза это обход SSRF-фильтра.
var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// RFC 6052 NAT64 (64:ff9b::/96) встраивает IPv4 в младшие 4 байта — через
// шлюз адрес маршрутизируется на встроенный IPv4, который может быть приватным.
var nat64Range = &net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}

// RFC 3056 6to4 (2002::/16) встраивает IPv4 в байты 2..5 — тот же вектор
// обхода фильтра приватных адресов через релей, что и NAT64.
var sixToFourRange = &net.IPNet{IP: net.ParseIP("2002::"), Mask: net.CIDRMask(16, 128)}

// RFC 4291 IPv4-compatible (::/96, deprecated): встраивает IPv4 в младшие 4
// байта. ::/::1 исключены — их «встроенный IPv4» бессмыслен, их режут проверки ниже.
var ipv4CompatRange = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(96, 128)}

// Внутренние/служебные диапазоны — недопустимые цели для чекеров/вебхуков
// (SSRF к метадате облака, внутренним сервисам, loopback).
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	// Переходные IPv6-префиксы встраивают IPv4: адрес выглядит публичным IPv6, но
	// уходит на встроенный IPv4, который проверяем рекурсивно тем же фильтром.
	if ip.To4() == nil {
		if ip16 := ip.To16(); ip16 != nil {
			if nat64Range.Contains(ip16) {
				return IsBlockedIP(net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]))
			}
			if sixToFourRange.Contains(ip16) {
				return IsBlockedIP(net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5]))
			}
			// ::(unspecified)/::1(loopback) исключены — сами режутся ниже штатно.
			if ipv4CompatRange.Contains(ip16) && !ip.IsUnspecified() && !ip.IsLoopback() {
				return IsBlockedIP(net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]))
			}
		}
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		cgnatRange.Contains(ip)
}

// Вызывается для net.Dialer.Control после резолва, до соединения — на каждый
// фактический адрес, что даёт устойчивость к DNS-rebind и редиректам.
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

// allowPrivate=false режет через Control (проверка на фактический IP после
// резолва). Таймаут не задан — его контролирует вызывающий через ctx.
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
