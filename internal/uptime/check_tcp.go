package uptime

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

// TCPChecker — TCP connect-чекер: успешен, если удаётся установить
// соединение в пределах таймаута монитора.
type TCPChecker struct{}

func NewTCPChecker() *TCPChecker {
	return &TCPChecker{}
}

func (c *TCPChecker) Check(ctx context.Context, m Monitor) Result {
	var cfg TCPConfig
	if err := strictUnmarshal(m.Config, &cfg); err != nil {
		return Result{Error: fmt.Sprintf("invalid tcp config: %v", err)}
	}

	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{}

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
