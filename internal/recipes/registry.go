package recipes

import (
	"fmt"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

// сниппеты — fmt.Sprintf-строка, не YAML-маршалинг чужого конфига otelcol-contrib.
// resourcedetection не ставим: host у точек пуст, страница рецепта работает без host-скоупа.

// postgresql.deadlocks выключена в metadata.yaml — включаем явно, иначе critical-порог мёртв.
// databases закомментирована нарочно: без неё ресивер берёт все базы, порог — по сумме/максимуму.
const postgresConfigTmpl = `receivers:
  postgresql:
    endpoint: localhost:5432
    transport: tcp
    username: CHANGE_ME
    password: CHANGE_ME
    # databases: [CHANGE_ME]  # limit to a single database for a precise deadlock threshold
    tls:
      insecure: true
    collection_interval: 30s
    metrics:
      postgresql.deadlocks: {enabled: true}
processors:
  transform/recipe:
    metric_statements:
      - context: datapoint
        statements:
          - set(attributes["postgresql.database.name"], resource.attributes["postgresql.database.name"])
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [postgresql]
      processors: [transform/recipe, batch]
      exporters: [otlphttp]
`

// ресивер mysql, но поддерживает и MariaDB — отсюда несовпадение с ID рецепта mariadb.
// query.slow.count по умолчанию выключена в metadata.yaml — включаем явно, иначе порог мёртв.
const mariadbConfigTmpl = `receivers:
  mysql:
    endpoint: localhost:3306
    username: CHANGE_ME
    password: CHANGE_ME
    collection_interval: 30s
    metrics:
      mysql.query.slow.count: {enabled: true}
processors:
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [mysql]
      processors: [batch]
      exporters: [otlphttp]
`

// нужен модуль stub_status в nginx (location /status { stub_status; }).
// Transform не нужен: state — родной атрибут nginx.connections_current.
const nginxConfigTmpl = `receivers:
  nginx:
    endpoint: http://localhost:80/status
    collection_interval: 30s
processors:
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [nginx]
      processors: [batch]
      exporters: [otlphttp]
`

// все нужные метрики включены по умолчанию; группировок по атрибутам нет — transform не нужен.
const redisConfigTmpl = `receivers:
  redis:
    endpoint: localhost:6379
    password: CHANGE_ME
    collection_interval: 30s
processors:
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [redis]
      processors: [batch]
      exporters: [otlphttp]
`

// container.name продвигаем обязательно — иначе пер-контейнерные графики схлопнутся в одну группу.
// container.image.name не продвигаем: график им не пользуется, а это лишняя кардинальность.
const dockerConfigTmpl = `receivers:
  docker_stats:
    endpoint: unix:///var/run/docker.sock
    collection_interval: 30s
processors:
  transform/recipe:
    metric_statements:
      - context: datapoint
        statements:
          - set(attributes["container.name"], resource.attributes["container.name"])
  batch: {}
exporters:
  otlphttp:
    endpoint: %s
    headers:
      Authorization: "Bearer %s"
service:
  pipelines:
    metrics:
      receivers: [docker_stats]
      processors: [transform/recipe, batch]
      exporters: [otlphttp]
`

var registry = []Recipe{
	{
		ID:        "postgres",
		Signature: "postgresql.backends", // не-monotonic sum: скалярный путь, детекция с первого скрейпа
		Metrics: []string{
			"postgresql.backends", "postgresql.commits", "postgresql.rollbacks",
			"postgresql.db_size", "postgresql.blocks_read", "postgresql.deadlocks",
			"postgresql.rows",
		},
		PromotedAttrs: []string{"postgresql.database.name"},
		Charts: []Chart{
			{Key: "backends", Series: []ChartSeries{{Metric: "postgresql.backends"}},
				GroupKey: "postgresql.database.name", Agg: "avg"},
			{Key: "commits_rollbacks", Unit: "1/s", Agg: "avg", Series: []ChartSeries{
				{Metric: "postgresql.commits", Rate: true, LabelSuffix: "commits"},
				{Metric: "postgresql.rollbacks", Rate: true, LabelSuffix: "rollbacks"},
			}},
			{Key: "db_size", Unit: "By", Series: []ChartSeries{{Metric: "postgresql.db_size"}},
				GroupKey: "postgresql.database.name", Agg: "avg"},
			// пер-базовая группировка: без GroupKey счётчики разных баз сложились бы
			// в одну линию, а rate на сумме кумулятивов даёт артефакты при рестарте любой из них.
			{Key: "blocks_read", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "postgresql.blocks_read", Rate: true}},
				GroupKey: "postgresql.database.name"},
			{Key: "deadlocks", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "postgresql.deadlocks", Rate: true}},
				GroupKey: "postgresql.database.name"},
			// не-monotonic sum (текущее число строк): скалярный график, группировка по родному state.
			{Key: "rows", Series: []ChartSeries{{Metric: "postgresql.rows"}},
				GroupKey: "state", Agg: "avg"},
		},
		Rules: []RuleSpec{
			// sum по monotonic cumulative = прирост за окно: «новые дедлоки за 5 минут», не «всего».
			{Metric: "postgresql.deadlocks", Agg: "sum", Comparator: "gt", Threshold: 0,
				WindowSeconds: 300, Severity: "critical", NoteKey: "deadlocks"},
			// значение усреднено по базам и скрейпам — NoteKey просит подстроить под max_connections.
			{Metric: "postgresql.backends", Agg: "avg", Comparator: "gt", Threshold: 80,
				WindowSeconds: 300, NoteKey: "backends"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(postgresConfigTmpl, baseURL, apiKey)
		},
	},
	{
		ID:        "mariadb",
		Signature: "mysql.threads", // не-monotonic sum: скалярный путь, детекция с первого скрейпа
		Metrics: []string{
			"mysql.threads", "mysql.operations", "mysql.buffer_pool.pages",
			"mysql.row_operations", "mysql.locks", "mysql.query.slow.count",
		},
		Charts: []Chart{
			// три ряда с матчерами, не GroupKey: kind=created на порядки больше остальных —
			// в общей группировке он задавал бы масштаб оси и прижимал остальные к нулю.
			{Key: "threads", Agg: "avg", Series: []ChartSeries{
				{Metric: "mysql.threads", LabelSuffix: "connected",
					Matchers: []metric.LabelMatcher{{Key: "kind", Value: "connected"}}},
				{Metric: "mysql.threads", LabelSuffix: "running",
					Matchers: []metric.LabelMatcher{{Key: "kind", Value: "running"}}},
				{Metric: "mysql.threads", LabelSuffix: "cached",
					Matchers: []metric.LabelMatcher{{Key: "kind", Value: "cached"}}},
			}},
			{Key: "operations", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "mysql.operations", Rate: true}},
				GroupKey: "operation"},
			// атрибут — kind (data/free/misc), не status: тот у mysql.buffer_pool.data_pages.
			{Key: "buffer_pool_pages", Series: []ChartSeries{{Metric: "mysql.buffer_pool.pages"}},
				GroupKey: "kind", Agg: "avg"},
			{Key: "row_operations", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "mysql.row_operations", Rate: true}},
				GroupKey: "operation"},
			// рост kind=waited — ранний сигнал контеншена.
			{Key: "locks", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "mysql.locks", Rate: true}},
				GroupKey: "kind"},
		},
		Rules: []RuleSpec{
			// 120 ≈ 80% дефолтного max_connections=151 — NoteKey просит подстроить под свой.
			{Metric: "mysql.threads", Agg: "avg", Comparator: "gt", Threshold: 120,
				WindowSeconds: 300, LabelKey: "kind", LabelValue: "connected", NoteKey: "threads_connected"},
			// метрика default=off — сниппет включает её явно; warning, не critical:
			// slow query — повод разобраться, не однозначная авария.
			{Metric: "mysql.query.slow.count", Agg: "sum", Comparator: "gt", Threshold: 0,
				WindowSeconds: 300, NoteKey: "slow_queries"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(mariadbConfigTmpl, baseURL, apiKey)
		},
	},
	{
		ID:        "nginx",
		Signature: "nginx.connections_current", // не-monotonic sum (не requests: тот monotonic)
		Metrics: []string{
			"nginx.requests", "nginx.connections_current",
			"nginx.connections_accepted", "nginx.connections_handled",
		},
		Charts: []Chart{
			{Key: "requests", Unit: "1/s", Agg: "avg",
				Series: []ChartSeries{{Metric: "nginx.requests", Rate: true}}},
			{Key: "connections", Series: []ChartSeries{{Metric: "nginx.connections_current"}},
				GroupKey: "state", Agg: "avg"},
			{Key: "accepted_handled", Unit: "1/s", Agg: "avg", Series: []ChartSeries{
				{Metric: "nginx.connections_accepted", Rate: true, LabelSuffix: "accepted"},
				{Metric: "nginx.connections_handled", Rate: true, LabelSuffix: "handled"},
			}},
		},
		Rules: []RuleSpec{
			// единственный порог: «дропы» accepted−handled одной метрикой не выразимы.
			{Metric: "nginx.connections_current", Agg: "avg", Comparator: "gt", Threshold: 1000,
				WindowSeconds: 300, LabelKey: "state", LabelValue: "active", NoteKey: "active_connections"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(nginxConfigTmpl, baseURL, apiKey)
		},
	},
	{
		ID:        "redis",
		Signature: "redis.clients.connected", // не-monotonic sum: скалярный путь
		Metrics: []string{
			"redis.clients.connected", "redis.clients.blocked", "redis.memory.used",
			"redis.memory.fragmentation_ratio", "redis.keyspace.hits", "redis.keyspace.misses",
			"redis.commands", "redis.connections.rejected",
		},
		Charts: []Chart{
			{Key: "memory", Unit: "By", Agg: "avg",
				Series: []ChartSeries{{Metric: "redis.memory.used"}}},
			{Key: "clients", Agg: "avg",
				Series: []ChartSeries{{Metric: "redis.clients.connected"}}},
			{Key: "keyspace", Unit: "1/s", Agg: "avg", Series: []ChartSeries{
				{Metric: "redis.keyspace.hits", Rate: true, LabelSuffix: "hits"},
				{Metric: "redis.keyspace.misses", Rate: true, LabelSuffix: "misses"},
			}},
			// redis.commands — gauge ops/sec от самого Redis (Rate=false);
			// не путать с monotonic redis.commands.processed.
			{Key: "commands", Unit: "1/s", Agg: "avg",
				Series: []ChartSeries{{Metric: "redis.commands"}}},
			{Key: "fragmentation", Agg: "avg",
				Series: []ChartSeries{{Metric: "redis.memory.fragmentation_ratio"}}},
		},
		Rules: []RuleSpec{
			// Прирост отказов за окно = упёрлись в maxclients.
			{Metric: "redis.connections.rejected", Agg: "sum", Comparator: "gt", Threshold: 0,
				WindowSeconds: 300, Severity: "critical", NoteKey: "rejected"},
			{Metric: "redis.memory.fragmentation_ratio", Agg: "avg", Comparator: "gt", Threshold: 1.5,
				WindowSeconds: 600, NoteKey: "fragmentation"},
			{Metric: "redis.clients.blocked", Agg: "avg", Comparator: "gt", Threshold: 5,
				WindowSeconds: 300, NoteKey: "blocked"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(redisConfigTmpl, baseURL, apiKey)
		},
	},
	{
		ID:        "docker",
		Signature: "container.cpu.utilization", // gauge
		Metrics: []string{
			"container.cpu.utilization", "container.memory.percent",
			"container.network.io.usage.rx_bytes", "container.network.io.usage.tx_bytes",
		},
		PromotedAttrs: []string{"container.name"},
		Charts: []Chart{
			{Key: "cpu", Series: []ChartSeries{{Metric: "container.cpu.utilization"}},
				GroupKey: "container.name", Agg: "avg"},
			{Key: "memory", Series: []ChartSeries{{Metric: "container.memory.percent"}},
				GroupKey: "container.name", Agg: "avg"},
			// два графика по направлению, не по рядам: пер-контейнерная группировка
			// (как у cpu/memory) требует ровно одной Series на график.
			{Key: "network_rx", Unit: "By/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "container.network.io.usage.rx_bytes", Rate: true}},
				GroupKey: "container.name"},
			{Key: "network_tx", Unit: "By/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "container.network.io.usage.tx_bytes", Rate: true}},
				GroupKey: "container.name"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(dockerConfigTmpl, baseURL, apiKey)
		},
	},
}
