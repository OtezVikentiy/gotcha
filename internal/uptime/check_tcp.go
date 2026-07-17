package uptime

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/netguard"
)

// TCPChecker — TCP connect-чекер: успешен, если удаётся установить
// соединение в пределах таймаута монитора.
//
// SSRF: по умолчанию (AllowPrivate=false) коннекты к приватным/служебным
// адресам режутся через netguard (по фактическому IP после резолва).
type TCPChecker struct {
	// AllowPrivate=true отключает SSRF-фильтр приватных целей.
	AllowPrivate bool
}

func NewTCPChecker(allowPrivate bool) *TCPChecker {
	return &TCPChecker{AllowPrivate: allowPrivate}
}

func (c *TCPChecker) Check(ctx context.Context, m Monitor) Result {
	var cfg TCPConfig
	if err := strictUnmarshal(m.Config, &cfg); err != nil {
		return Result{Error: fmt.Sprintf("invalid tcp config: %v", err)}
	}

	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := netguard.Dialer(c.AllowPrivate)

	start := time.Now()
	conn, err := dialer.DialContext(dialCtx, "tcp", addr)
	elapsed := time.Since(start)
	if err != nil {
		return Result{Error: errMessage(err, m.TimeoutSeconds), TotalMs: msToUint32(elapsed)}
	}
	defer conn.Close()

	ms := msToUint32(elapsed)
	return Result{
		OK:        true,
		ConnectMs: ms,
		TotalMs:   ms,
	}
}
