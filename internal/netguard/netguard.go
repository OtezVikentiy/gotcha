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
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
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
