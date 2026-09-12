package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Пул общий на все HTTP-роуты, приём и фоновые циклы процесса — встроенный
// дефолт pgxpool (max(4, NumCPU)) на такую нагрузку не рассчитан.
const pgPoolMaxConns = 20

// Без лимита зависший запрос держит соединение пула бесконечно и способен
// исчерпать весь пул в одиночку — значение GUC statement_timeout, мс.
const pgStatementTimeout = "30000"

func NewPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	cfg.MaxConns = pgPoolMaxConns
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string, 1)
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = pgStatementTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return pool, nil
}

// Ошибку pgxpool.ParseConfig можно возвращать как есть: pgx сам редактирует
// пароль в тексте ("xxxxxx") — %w не утекает секрет, в отличие от ClickHouse ниже.
func ValidatePostgresDSN(dsn string) error {
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	return nil
}
