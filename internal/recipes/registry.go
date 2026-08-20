package recipes

import "fmt"

// Конфиг-сниппеты собираются fmt.Sprintf-строкой, а не YAML-маршалингом
// структуры, — по тем же соображениям, что collectorConfigTmpl (hosts.go):
// это конфиг ЧУЖОЙ программы (otelcol-contrib), маршалинг завёл бы Go-типы
// под формат, которым мы не управляем, ради двух подстановок. endpoint
// экспортёра — БАЗОВЫЙ URL без /v1/metrics: путь дописывает сам
// otlphttp-экспортёр. resourcedetection в сниппеты НЕ ставим (спека §5
// MAJOR-2): host у точек остаётся пустым, страница рецепта работает без
// host-скоупа. Чужие секреты (пароли сервисов) — CHANGE_ME-плейсхолдеры,
// наших секретов в сниппете нет (только ключ проекта, как /hosts).
//
// Transform-процессор в сниппетах postgres/docker обязателен (BLOCKER-1,
// см. package doc): продвигает resource-атрибуты в datapoint-атрибуты.
// Синтаксис — «advanced» форма transformprocessor (`context: datapoint` +
// `set(attributes[...], resource.attributes[...])`): поддерживается и
// старыми, и текущими версиями коллектора, в отличие от flat-путей
// (`datapoint.attributes[...]`), требующих свежего контекст-инференса.

// postgresConfigTmpl: postgresql-ресивер. postgresql.deadlocks по умолчанию
// ВЫКЛЮЧЕНА в metadata.yaml ресивера (сверка T1 Step 1) — включаем явно,
// иначе рекомендованный critical-порог по дедлокам был бы мёртвым.
// Строка databases закомментирована нарочно (финревью P1-1): без неё ресивер
// собирает ВСЕ базы, и порог по дедлокам следит за суммой/максимумом — для
// точного порога по одной базе пользователь раскомментирует и сужает список.
// Комментарий в YAML — по-английски: сниппет один на обе локали, а кириллица
// в проде живёт только в i18n-каталогах.
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

// nginxConfigTmpl: nginx-ресивер поверх stub_status (модуль надо включить в
// nginx: location /status { stub_status; }). Transform не нужен: state —
// родной datapoint-атрибут nginx.connections_current.
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

// redisConfigTmpl: redis-ресивер. Все нужные метрики включены по умолчанию;
// группировок по resource-атрибутам нет — transform не нужен.
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

// dockerConfigTmpl: docker_stats-ресивер (нужен доступ к докер-сокету).
// Каждый контейнер у ресивера — отдельный Resource; container.name —
// resource-атрибут, продвигаем обязательно, иначе пер-контейнерные графики
// слипнутся в одну группу "". container.image.name НЕ продвигаем (аудит P2):
// им не пользуется ни один график/порог, а лишний datapoint-атрибут — лишняя
// кардинальность в metric_points.
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

// registry — рецепты в порядке показа. Имена метрик, типы и атрибуты сверены
// с metadata.yaml ресиверов otel-collector-contrib (T1 Step 1); ключевые
// расхождения со спекой §3 зафиксированы в отчёте T1:
//   - сигнатуры postgres/nginx/redis — не gauge, а НЕ-monotonic cumulative
//     sum; для детекции это эквивалентно (наш тракт агрегирует их скалярно
//     с первой же точки, rate-путь не включается);
//   - postgresql.rows — НЕ-monotonic sum (текущие live/dead строки), поэтому
//     график rows скалярный (Rate=false), группировка по родному
//     datapoint-атрибуту state, а не «rate» из чернового списка спеки.
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
			// blocks_read и deadlocks — пер-базовая группировка (финревью
			// P1-1): без GroupKey счётчики РАЗНЫХ баз складывались бы в одну
			// линию, а rate на сумме кумулятивов от нескольких баз даёт
			// артефакты при рестарте любой из них.
			{Key: "blocks_read", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "postgresql.blocks_read", Rate: true}},
				GroupKey: "postgresql.database.name"},
			{Key: "deadlocks", Unit: "1/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "postgresql.deadlocks", Rate: true}},
				GroupKey: "postgresql.database.name"},
			// rows — не-monotonic sum (текущее число строк), скалярный график
			// с группировкой по родному datapoint-атрибуту state (live/dead).
			{Key: "rows", Series: []ChartSeries{{Metric: "postgresql.rows"}},
				GroupKey: "state", Agg: "avg"},
		},
		Rules: []RuleSpec{
			// sum по monotonic cumulative = прирост за окно (Aggregate делает
			// rate): «были новые дедлоки за 5 минут», НЕ «всего дедлоков».
			{Metric: "postgresql.deadlocks", Agg: "sum", Comparator: "gt", Threshold: 0,
				WindowSeconds: 300, Severity: "critical", NoteKey: "deadlocks"},
			// NoteKey поясняет: подстройте под ваш max_connections; значение
			// усреднено по базам и скрейпам (ревью MINOR-5).
			{Metric: "postgresql.backends", Agg: "avg", Comparator: "gt", Threshold: 80,
				WindowSeconds: 300, NoteKey: "backends"},
		},
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(postgresConfigTmpl, baseURL, apiKey)
		},
	},
	{
		ID:        "nginx",
		Signature: "nginx.connections_current", // не-monotonic sum (НЕ requests: тот monotonic — ревью MINOR-3)
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
			// Один мягкий порог (развилка F7): «дропы» accepted−handled
			// одно-метричным правилом не выразимы. NoteKey: подстройте под
			// мощность вашего nginx.
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
			// НЕ путать с monotonic redis.commands.processed (ревью MINOR-4).
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
			// Сеть — ДВА графика, по направлению на каждый (аудит QA P1-2):
			// парный rx+tx без GroupKey складывал счётчики всех контейнеров в
			// две безымянные линии; пер-контейнерная группировка (как у cpu и
			// memory) требует ровно одной Series на график — направление
			// поэтому разнесено по графикам, а не по рядам.
			{Key: "network_rx", Unit: "By/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "container.network.io.usage.rx_bytes", Rate: true}},
				GroupKey: "container.name"},
			{Key: "network_tx", Unit: "By/s", Agg: "avg",
				Series:   []ChartSeries{{Metric: "container.network.io.usage.tx_bytes", Rate: true}},
				GroupKey: "container.name"},
		},
		// Rules пусты намеренно (развилка F1): метрики пер-контейнерные,
		// разумный дефолт «на все контейнеры разом» невозможен; страница
		// честно объясняет отсутствие порогов.
		Config: func(baseURL, apiKey string) string {
			return fmt.Sprintf(dockerConfigTmpl, baseURL, apiKey)
		},
	},
}
