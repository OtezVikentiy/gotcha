// Package db держит инфраструктуру хранилищ: соединения с PostgreSQL и
// ClickHouse и миграции. Репозитории живут в доменных пакетах.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgPoolMaxConns — потолок размера пула ОДНОГО процесса (W3-D, запись 7).
//
// БЕЗ этой настройки pgxpool.New берёт свой встроенный дефолт —
// max(4, runtime.NumCPU()) — то есть 4-8 соединений на большинстве прод-хостов.
// Один этот пул обслуживает ВСЁ: ~150 HTTP-маршрутов (internal/web), приём
// (internal/ingest, свой rate-limit только по частоте запроса, не по времени
// удержания соединения — см. Handler.publicLimiter в internal/web/web.go,
// который прямым текстом признаёт риск «аноним без единого ключа выбирает
// пул и роняет UI, алерты и квоты»), и 23 фоновых цикла: пять оценщиков
// (metric/trace(perf)/profile/host/slo), три цикла аптайма (Scheduler/
// Runner/Watchdog), шесть джаниторов (export/notify(outbox)/auth/
// escalation/telemetry(entity)/incidentgroup), escalation.Scheduler,
// notify.Worker+Digester, export.Worker/purge/spike-воркеры, три пул-метрики
// диска (storagePollers/pgUsedBytes/exportUsedBytes). Число — не голый
// grep `go .*\.Run(ctx)` по cmd/gotcha/main.go: наивный grep даёт 21 и
// пропускает export.Worker/export.Janitor — они запущены через
// `go func(){ X.Run(ctx) }()` (нужен WaitGroup для дренажа при остановке,
// см. exportWorkersWG) и по строке "X.Run(ctx)" не матчатся вовсе. Пересчитано
// поимённо по вызывающему коду startEvaluators/run() в cmd/gotcha/main.go.
// Всплеск publicLimiter-трафика (600/мин/IP = 10/с — уже разрешённая
// продуктом частота heartbeat/probe-запросов) сам по себе способен занять
// больше четырёх соединений одновременно, оставив без пула И веб-часть,
// И оценщики.
//
// 20 — компромисс, а не «побольше»: default max_connections сервера
// PostgreSQL — 100 (с запасом под superuser_reserved_connections и внешние
// psql-сессии дежурного). 20 на процесс даёт запас до пяти одновременно
// живых процессов с этим пулом (--mode=web/ingest/uptime/all в любых
// сочетаниях, плюс probe-реплики со своим меньшим пулом), не упираясь в
// потолок PostgreSQL — а внутри одного процесса кратно перекрывает и
// встроенный дефолт pgxpool, и типичный всплеск publicLimiter.
const pgPoolMaxConns = 20

// pgStatementTimeout — потолок ОДНОГО запроса (W3-D, запись 7), значение
// GUC statement_timeout в миллисекундах.
//
// Без него зависший или забытый неиндексированный запрос держит соединение
// пула бесконечно — при потолке в pgPoolMaxConns это способ исчерпать пул
// целиком единственным плохим запросом, а не всплеском трафика. 30 секунд
// щедрее любого легитимного пути продукта: у фоновых циклов бюджет тика
// начинается от 10с (см. host.Evaluator.minTickBudget) при интервалах от
// минуты и выше — 30с запроса внутри такого тика редкость, а не норма; для
// HTTP-запроса зависший на 30с PG для клиента неотличим от «PG недоступен»
// независимо от таймаута — быстрый явный отказ вместо тихого зависания
// возвращает соединение остальным 20 конкурентам пула раньше, чем накопится
// очередь.
const pgStatementTimeout = "30000"

// NewPostgres открывает пул соединений и проверяет его пингом.
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

// ValidatePostgresDSN проверяет, что dsn разбираем pgx — без установки
// соединения (та же проверка на старте, что NewPostgres/ForcePG/MigratePG
// делают неявно первым шагом). Разбор — тем же pgxpool.ParseConfig, что
// реально потребляет DSN, поэтому принимается ровно то, что примет клиент:
// и URL-форма (postgres://user:pass@host/db), и keyword/value-форма
// (host=... user=... dbname=...) — обе легальны для pgx, поэтому DSN нельзя
// гонять через ту же проверку «схема и хост обязательны», что у обычных
// базовых адресов (см. internal/baseurl) — она отвергла бы законный DSN.
//
// Текст ошибки pgxpool.ParseConfig безопасен для возврата как есть: pgx сам
// редактирует пароль в сообщении (заменяет на "xxxxxx"), поэтому оборачивание
// через %w не утекает секрет — в отличие от ClickHouse ниже.
func ValidatePostgresDSN(dsn string) error {
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	return nil
}
