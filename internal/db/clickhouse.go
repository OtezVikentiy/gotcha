package db

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func NewClickHouse(ctx context.Context, dsn string) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		// Сырую ошибку ParseDSN намеренно не оборачиваем и не логируем — она может содержать DSN с паролем.
		return nil, fmt.Errorf("clickhouse: invalid DSN")
	}
	// Потолок runaway-запроса — без него тяжёлый скан может занять сервер целиком; 60с выше любого
	// легитимного запроса. DSN (?max_execution_time=...) переопределяет — своё значение не трогаем.
	if opts.Settings == nil {
		opts.Settings = clickhouse.Settings{}
	}
	if _, ok := opts.Settings["max_execution_time"]; !ok {
		opts.Settings["max_execution_time"] = 60
	}
	// Инвариант «агрегат без GROUP BY даёт одну строку с нулями» используют ~11 read-путей — фиксируем 0
	// жёстко, не даём DSN переопределить (иначе пустое окно даёт ErrNoRows вместо нулей).
	opts.Settings["empty_result_for_aggregation_by_empty_set"] = 0
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

// Тот же парсер, что и NewClickHouse — принимает URL- и keyword-форму DSN одинаково.
func ValidateClickHouseDSN(dsn string) error {
	if _, err := clickhouse.ParseDSN(dsn); err != nil {
		// Как и выше — сырая ошибка может содержать DSN с паролем, поэтому не логируем её.
		return fmt.Errorf("clickhouse: invalid DSN")
	}
	return nil
}
