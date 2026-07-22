package db

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// NewClickHouse открывает нативное соединение и проверяет его пингом.
func NewClickHouse(ctx context.Context, dsn string) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		// Сырую ошибку ParseDSN намеренно НЕ оборачиваем через %w и не
		// логируем: она может содержать сам DSN с паролем и утечь в логи
		// оператора. Отдаём обобщённую формулировку без credentials.
		return nil, fmt.Errorf("clickhouse: invalid DSN")
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse ping: %w", err)
	}
	return conn, nil
}
