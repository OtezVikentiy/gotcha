package netguard

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // loopback IPv4
		"::1",             // loopback IPv6
		"10.0.0.1",        // private
		"192.168.1.1",     // private
		"169.254.169.254", // link-local (метадата облака)
		"fe80::1",         // link-local IPv6
		"fc00::1",         // ULA
		"0.0.0.0",         // unspecified
		"100.64.0.1",      // CGNAT нижняя граница (RFC 6598)
		"100.100.100.200", // метадата Alibaba/Oracle (CGNAT)
		"100.127.255.255", // CGNAT верхняя граница
		"64:ff9b::7f00:1", // NAT64 → 127.0.0.1 (loopback), обход SSRF-фильтра
		"64:ff9b::a00:1",  // NAT64 → 10.0.0.1 (private)
		"2002:7f00:1::",   // 6to4 → 127.0.0.1 (loopback)
		"2002:a00:1::",    // 6to4 → 10.0.0.1 (private)
		"::7f00:1",        // IPv4-compatible ::a.b.c.d → 127.0.0.1 (loopback)
		"::a00:1",         // IPv4-compatible → 10.0.0.1 (private)
		"::",              // unspecified IPv6 (регресс: остаётся заблокирован)
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false, ожидалось true", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"100.63.255.255",       // сразу под CGNAT — публичный, не блокируем
		"100.128.0.0",          // сразу над CGNAT — публичный, не блокируем
		"64:ff9b::808:808",     // NAT64 → 8.8.8.8 (публичный) — не блокируем
		"2002:808:808::",       // 6to4 → 8.8.8.8 (публичный) — не блокируем
		"::808:808",            // IPv4-compatible → 8.8.8.8 (публичный) — не блокируем
		"2606:4700:4700::1111", // обычный публичный IPv6 (Cloudflare)
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true, ожидалось false", s)
		}
	}

	// nil-адрес тоже блокируется.
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) = false, ожидалось true")
	}
}

func TestDialerBlocksLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	addr := ln.Addr().String()

	blocked := Dialer(false)
	if conn, err := blocked.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Error("Dialer(false).Dial(loopback) = nil error, ожидалась блокировка")
	} else if !errors.Is(err, ErrBlockedTarget) {
		t.Errorf("ожидалась ошибка ErrBlockedTarget, получено: %v", err)
	}

	allowed := Dialer(true)
	conn, err := allowed.Dial("tcp", addr)
	if err != nil {
		t.Errorf("Dialer(true).Dial(loopback) = %v, ожидался проход", err)
	} else {
		conn.Close()
	}
}

func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	// Принимаем и сразу закрываем соединения, чтобы allowPrivate=true не завис.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	url := "http://" + ln.Addr().String() + "/"

	blocked := SafeHTTPClient(false, 5*time.Second)
	if _, err := blocked.Get(url); err == nil {
		t.Error("SafeHTTPClient(false).Get(loopback) = nil error, ожидалась блокировка")
	} else if !errors.Is(err, ErrBlockedTarget) {
		t.Errorf("ожидалась ошибка ErrBlockedTarget, получено: %v", err)
	}

	// Ошибка уровня HTTP допустима (соединение закрывается сервером сразу), но
	// это НЕ ErrBlockedTarget — фильтр не должен был сработать.
	allowed := SafeHTTPClient(true, 5*time.Second)
	if _, err := allowed.Get(url); errors.Is(err, ErrBlockedTarget) {
		t.Error("SafeHTTPClient(true).Get(loopback) заблокирован, ожидался проход фильтра")
	}
}
