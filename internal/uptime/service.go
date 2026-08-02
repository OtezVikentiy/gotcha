package uptime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound       = errors.New("uptime: not found")
	ErrInvalidMonitor = errors.New("uptime: invalid monitor")
)

// Service — CRUD над мониторами доступности поверх PostgreSQL.
type Service struct {
	pool *pgxpool.Pool

	// LocalRegion — как на самом деле НАЗЫВАЕТСЯ регион встроенной пробы в
	// этой инсталляции: тот, которым Runner помечает свои проверки и который
	// он лизит (cmd/gotcha: cfg.LocalRegion, GOTCHA_LOCAL_REGION). Пустое
	// значение — DefaultRegion ("local"). Хардкодить "local" нельзя: Regions
	// предлагает этот список в форме монитора, и монитор, назначенный в
	// регион, который никто не лизит, не будет проверяться НИКОГДА (см.
	// localRegion).
	LocalRegion string
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// localRegion — имя встроенного региона: LocalRegion, а если не задано —
// DefaultRegion.
func (s *Service) localRegion() string {
	if s.LocalRegion == "" {
		return DefaultRegion
	}
	return s.LocalRegion
}

// generateHeartbeatToken — 32 случайных байта в hex (64 символа).
func generateHeartbeatToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// heartbeatTokenHash — sha256 сырого heartbeat-токена. В БД хранится только он
// (monitors.heartbeat_token_hash), а не сам токен — так же, как probe-токены
// (probeTokenHash) и session-токены. Сырой токен вызывающий видит один раз при
// Create; на приёме пинга входящий токен снова хешируется и ищется по хешу.
func heartbeatTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// checkChannelsBelongToProject проверяет, что все channelIDs — каналы
// проекта projectID; иначе ErrInvalidMonitor.
func checkChannelsBelongToProject(ctx context.Context, tx pgx.Tx, projectID int64, channelIDs []int64) error {
	if len(channelIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		"SELECT id FROM alert_channels WHERE project_id = $1 AND id = ANY($2)",
		projectID, channelIDs)
	if err != nil {
		return fmt.Errorf("uptime: check channels: %w", err)
	}
	defer rows.Close()
	found := make(map[int64]bool, len(channelIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("uptime: check channels: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("uptime: check channels: %w", err)
	}
	for _, id := range channelIDs {
		if !found[id] {
			return invalid("channels", "channel_foreign")
		}
	}
	return nil
}

// checkRegionsAvailable проверяет, что все выбранные регионы доступны
// организации проекта: встроенный регион этой инсталляции плюс регионы её
// неотозванных проб.
//
// Форма предлагает только свои регионы, но POST принимал любую строку. Монитор
// с несуществующим регионом попадал в очередь, и его не забирал никто — тихий
// отказ мониторинга, который выглядит как «проверок нет, значит всё хорошо».
// Кросс-тенантной утечки тут не было (лизы скоупятся по организации), но именно
// поэтому дефект и был незаметен.
func (s *Service) checkRegionsAvailable(ctx context.Context, tx pgx.Tx, projectID int64, regions []string) error {
	if len(regions) == 0 {
		return nil
	}
	var orgID int64
	if err := tx.QueryRow(ctx, "SELECT org_id FROM projects WHERE id = $1", projectID).Scan(&orgID); err != nil {
		return fmt.Errorf("uptime: check regions: %w", err)
	}
	available, err := s.Regions(ctx, orgID)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(available))
	for _, r := range available {
		allowed[r] = true
	}
	for _, r := range regions {
		if !allowed[r] {
			return invalid("regions", "region_unavailable", "region", r)
		}
	}
	return nil
}

func insertRegions(ctx context.Context, tx pgx.Tx, monitorID int64, regions []string) error {
	for _, r := range regions {
		if _, err := tx.Exec(ctx,
			"INSERT INTO monitor_regions (monitor_id, region) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			monitorID, r); err != nil {
			return fmt.Errorf("uptime: insert region: %w", err)
		}
	}
	return nil
}

func insertChannels(ctx context.Context, tx pgx.Tx, monitorID int64, channelIDs []int64) error {
	for _, id := range channelIDs {
		if _, err := tx.Exec(ctx,
			"INSERT INTO monitor_channels (monitor_id, channel_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			monitorID, id); err != nil {
			return fmt.Errorf("uptime: insert channel: %w", err)
		}
	}
	return nil
}

// Create создаёт монитор вместе с регионами и каналами в одной транзакции.
// Пустые regions превращаются в ["local"]. Для kind=heartbeat генерирует
// уникальный heartbeat_token.
func (s *Service) Create(ctx context.Context, m Monitor, regions []string, channelIDs []int64) (Monitor, error) {
	if err := validateMonitor(m, regions); err != nil {
		return Monitor{}, err
	}
	if len(regions) == 0 {
		regions = []string{"local"}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkChannelsBelongToProject(ctx, tx, m.ProjectID, channelIDs); err != nil {
		return Monitor{}, err
	}
	if err := s.checkRegionsAvailable(ctx, tx, m.ProjectID, regions); err != nil {
		return Monitor{}, err
	}

	if m.Kind == KindHeartbeat {
		token, err := generateHeartbeatToken()
		if err != nil {
			return Monitor{}, fmt.Errorf("uptime: create: %w", err)
		}
		m.HeartbeatToken = token
	} else {
		m.HeartbeatToken = ""
	}
	// В БД сохраняем только sha256 токена. Сырой m.HeartbeatToken остаётся в
	// возвращаемом мониторе и показывается вызывающему один раз (как probe).
	var heartbeatTokenHashVal []byte
	if m.HeartbeatToken != "" {
		heartbeatTokenHashVal = heartbeatTokenHash(m.HeartbeatToken)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO monitors (project_id, name, kind, enabled, interval_seconds, timeout_seconds,
			config, fail_threshold, recovery_threshold, consensus, remind_every_minutes,
			ssl_alert_days, heartbeat_token_hash, retries)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id, created_at`,
		m.ProjectID, m.Name, string(m.Kind), m.Enabled, m.IntervalSeconds, m.TimeoutSeconds,
		m.Config, m.FailThreshold, m.RecoveryThreshold, string(m.Consensus), m.RemindEveryMinutes,
		m.SSLAlertDays, heartbeatTokenHashVal, m.Retries,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}

	if err := insertRegions(ctx, tx, m.ID, regions); err != nil {
		return Monitor{}, err
	}
	if err := insertChannels(ctx, tx, m.ID, channelIDs); err != nil {
		return Monitor{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}
	m.Regions = regions
	m.RegionCount = len(regions)
	m.ChannelIDs = channelIDs
	return m, nil
}

// Update обновляет монитор и заменяет его regions/channels. kind и
// heartbeat_token монитора не меняются, даже если m содержит другие
// значения — они читаются из БД перед валидацией и записью.
func (s *Service) Update(ctx context.Context, m Monitor, regions []string, channelIDs []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind Kind
	var projectID int64
	err = tx.QueryRow(ctx, "SELECT kind, project_id FROM monitors WHERE id = $1", m.ID).Scan(&kind, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	m.Kind = kind
	m.ProjectID = projectID

	if err := validateMonitor(m, regions); err != nil {
		return err
	}
	if len(regions) == 0 {
		regions = []string{"local"}
	}

	if err := checkChannelsBelongToProject(ctx, tx, m.ProjectID, channelIDs); err != nil {
		return err
	}
	if err := s.checkRegionsAvailable(ctx, tx, m.ProjectID, regions); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE monitors SET name=$2, enabled=$3, interval_seconds=$4, timeout_seconds=$5,
			config=$6, fail_threshold=$7, recovery_threshold=$8, consensus=$9,
			remind_every_minutes=$10, ssl_alert_days=$11, retries=$12
		WHERE id = $1`,
		m.ID, m.Name, m.Enabled, m.IntervalSeconds, m.TimeoutSeconds, m.Config,
		m.FailThreshold, m.RecoveryThreshold, string(m.Consensus), m.RemindEveryMinutes, m.SSLAlertDays, m.Retries)
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx, "DELETE FROM monitor_regions WHERE monitor_id = $1", m.ID); err != nil {
		return fmt.Errorf("uptime: update regions: %w", err)
	}
	if err := insertRegions(ctx, tx, m.ID, regions); err != nil {
		return err
	}
	// Состояние и невыполненные задания снятых регионов — вслед за самими
	// регионами. Иначе строка состояния остаётся навсегда (её больше некому
	// перезаписать: задание для снятого региона не ставится), а задание в
	// очереди будет один раз взято в лизу и выполнено уже после того, как
	// регион у монитора убрали. Чтение дополнительно защищено JOIN'ом в
	// States/StatesBatch — это для тех строк, что уже накопились.
	if _, err := tx.Exec(ctx,
		"DELETE FROM monitor_state WHERE monitor_id = $1 AND region <> ALL($2)", m.ID, regions); err != nil {
		return fmt.Errorf("uptime: update: drop stale state: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM check_queue WHERE monitor_id = $1 AND region <> ALL($2)", m.ID, regions); err != nil {
		return fmt.Errorf("uptime: update: drop stale queue: %w", err)
	}

	if _, err := tx.Exec(ctx, "DELETE FROM monitor_channels WHERE monitor_id = $1", m.ID); err != nil {
		return fmt.Errorf("uptime: update channels: %w", err)
	}
	if err := insertChannels(ctx, tx, m.ID, channelIDs); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	return nil
}

// Delete удаляет монитор. Каскадом (FK ON DELETE CASCADE) удаляются его
// regions, channels, state, инциденты и т.д.
func (s *Service) Delete(ctx context.Context, monitorID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM monitors WHERE id = $1", monitorID)
	if err != nil {
		return fmt.Errorf("uptime: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func regionsOf(ctx context.Context, pool *pgxpool.Pool, monitorID int64) ([]string, error) {
	rows, err := pool.Query(ctx,
		"SELECT region FROM monitor_regions WHERE monitor_id = $1 ORDER BY region", monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: regions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("uptime: regions: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// regionsOfBatch — то же, что regionsOf, но для набора monitorIDs одним
// запросом (ORDER BY monitor_id, region — порядок регионов внутри каждого
// монитора тот же, что даёт regionsOf). Мониторы без регионов отсутствуют в
// карте; GetBatch читает через неё с обычным zero-value для nil-слайса.
func regionsOfBatch(ctx context.Context, pool *pgxpool.Pool, monitorIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		"SELECT monitor_id, region FROM monitor_regions WHERE monitor_id = ANY($1) ORDER BY monitor_id, region",
		monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("uptime: regions batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r string
		if err := rows.Scan(&id, &r); err != nil {
			return nil, fmt.Errorf("uptime: regions batch: scan: %w", err)
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

func channelIDsOf(ctx context.Context, pool *pgxpool.Pool, monitorID int64) ([]int64, error) {
	rows, err := pool.Query(ctx,
		"SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id", monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: channels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("uptime: channels: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// scanMonitor заполняет монитор из строки monitorColumns. HeartbeatToken при
// чтении не восстанавливается: в БД хранится только sha256 токена
// (heartbeat_token_hash), а сырой токен вызывающий видит лишь один раз при
// Create.
func scanMonitor(row pgx.Row, m *Monitor) error {
	return row.Scan(&m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
		&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
		&m.SSLAlertDays, &m.SSLExpiresAt, &m.LastBeatAt, &m.CreatedAt, &m.Retries)
}

const monitorColumns = `project_id, name, kind, enabled, interval_seconds, timeout_seconds, config,
	fail_threshold, recovery_threshold, consensus, remind_every_minutes, ssl_alert_days,
	ssl_expires_at, last_beat_at, created_at, retries`

// Get возвращает монитор вместе с его regions и channels.
func (s *Service) Get(ctx context.Context, monitorID int64) (Monitor, error) {
	m := Monitor{ID: monitorID}
	row := s.pool.QueryRow(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE id = $1", monitorID)
	if err := scanMonitor(row, &m); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Monitor{}, ErrNotFound
		}
		return Monitor{}, fmt.Errorf("uptime: get: %w", err)
	}

	regions, err := regionsOf(ctx, s.pool, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	m.Regions = regions
	m.RegionCount = len(regions)

	channelIDs, err := channelIDsOf(ctx, s.pool, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	m.ChannelIDs = channelIDs

	return m, nil
}

// GetBatch возвращает мониторы набора monitorIDs одним обходом таблицы
// monitors плюс один пакетный запрос regions (два запроса вместо Get()'овских
// трёх на монитор, умноженных на N) — публичная статус-страница иначе звала
// Get() в цикле и на сорока мониторах упиралась в таймаут сборки.
//
// ChannelIDs не заполняются, как и у List() (см. её комментарий): странице
// они не нужны, а тянуть их батчем ради неиспользуемого поля — лишняя работа.
// Если появится потребитель, которому ChannelIDs нужны в пакетном виде —
// заводить тем же приёмом, что ниже для Regions, а не звать channelIDsOf в
// цикле.
//
// Карта заполняется для ВСЕХ monitorIDs, а не только найденных (тот же
// приём, что StatesBatch, см. её комментарий): монитор, которого уже нет в
// БД (удалён между чтением списка страницы и сборкой самой страницы),
// получает нулевой Monitor{ID: id} — у настоящего монитора ProjectID
// никогда не бывает 0 (bigint GENERATED ALWAYS AS IDENTITY стартует с 1),
// поэтому проверка "m.ProjectID != sp.ProjectID" на стороне вызывающего
// одинаково отсекает и «монитора больше нет», и «монитор чужого проекта» —
// то же самое единственное решение (не показывать монитор на странице), что
// принимал раньше отдельный errors.Is(err, ErrNotFound) до объединения.
func (s *Service) GetBatch(ctx context.Context, monitorIDs []int64) (map[int64]Monitor, error) {
	out := make(map[int64]Monitor, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = Monitor{ID: id}
	}

	rows, err := s.pool.Query(ctx,
		"SELECT id, "+monitorColumns+" FROM monitors WHERE id = ANY($1)", monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("uptime: get batch: %w", err)
	}
	for rows.Next() {
		var m Monitor
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt,
			&m.LastBeatAt, &m.CreatedAt, &m.Retries); err != nil {
			rows.Close()
			return nil, fmt.Errorf("uptime: get batch: scan: %w", err)
		}
		out[m.ID] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: get batch: %w", err)
	}

	regionsByMon, err := regionsOfBatch(ctx, s.pool, monitorIDs)
	if err != nil {
		return nil, err
	}
	for id, m := range out {
		regions := regionsByMon[id]
		m.Regions = regions
		m.RegionCount = len(regions)
		out[id] = m
	}
	return out, nil
}

// List возвращает мониторы проекта, отсортированные по name, вместе с их
// regions (ChannelIDs не заполняются — см. Get).
func (s *Service) List(ctx context.Context, projectID int64) ([]Monitor, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, "+monitorColumns+" FROM monitors WHERE project_id = $1 ORDER BY name", projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: list: %w", err)
	}
	var out []Monitor
	for rows.Next() {
		var m Monitor
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt,
			&m.LastBeatAt, &m.CreatedAt, &m.Retries); err != nil {
			rows.Close()
			return nil, fmt.Errorf("uptime: list: %w", err)
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: list: %w", err)
	}

	for i := range out {
		regions, err := regionsOf(ctx, s.pool, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Regions = regions
		out[i].RegionCount = len(regions)
	}
	return out, nil
}

// SetEnabled включает/выключает монитор.
func (s *Service) SetEnabled(ctx context.Context, monitorID int64, enabled bool) error {
	tag, err := s.pool.Exec(ctx, "UPDATE monitors SET enabled = $2 WHERE id = $1", monitorID, enabled)
	if err != nil {
		return fmt.Errorf("uptime: set enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ByHeartbeatToken ищет монитор kind=heartbeat по его токену — используется
// эндпоинтом приёма heartbeat-пингов.
func (s *Service) ByHeartbeatToken(ctx context.Context, token string) (Monitor, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		"SELECT id FROM monitors WHERE heartbeat_token_hash = $1", heartbeatTokenHash(token)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Monitor{}, ErrNotFound
	}
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: by heartbeat token: %w", err)
	}
	return s.Get(ctx, id)
}

// SetSSLExpiry records the certificate expiry observed by an https check.
// A no-op (besides the write itself) when expires equals the value already
// stored. When it differs and is LATER than the stored one, ssl_alerted_days
// is cleared — a later expiry means a new certificate was issued, so any
// "N days left" alerts already sent for the old one no longer apply. The
// comparison and the clear happen in a single UPDATE so a concurrent caller
// can't observe (or race) a half-applied state.
func (s *Service) SetSSLExpiry(ctx context.Context, monitorID int64, expires time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE monitors SET
			ssl_expires_at = $2,
			ssl_alerted_days = CASE
				WHEN ssl_expires_at IS NOT NULL AND $2 > ssl_expires_at THEN '{}'::int[]
				ELSE ssl_alerted_days
			END
		WHERE id = $1`,
		monitorID, expires)
	if err != nil {
		return fmt.Errorf("uptime: set ssl expiry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MonitorChannelIDs returns the ids of monitorID's own delivery channels —
// the ones linked via monitor_channels — ordered by id, ВКЛЮЧАЯ выключенные.
// Пустой (nil) результат не ошибка: он означает, что у монитора нет
// собственных каналов, и вызывающий (см. OutboxNotifier.Notify) откатывается
// на каналы проекта. Выключенные каналы намеренно НЕ отфильтрованы здесь:
// иначе монитор, у которого единственный канал выключили, выглядел бы как
// «без собственных каналов» и его уведомления уходили бы ВО ВСЕ каналы
// проекта — ровно в те, которые оператор явно исключил. Пропуск выключенных
// делает сам Notify.
//
// Возвращаются именно идентификаторы, а не строки каналов: секреты лежат в
// alert_channels зашифрованными (secretbox, префикс "enc:"), а мастер-ключ
// есть только у alert.Service. Читая тут c.secret напрямую, uptime отдавал бы
// в доставку шифротекст в качестве токена бота — и молча, потому что попытки
// расшифровать не было, а значит не было и ошибки. Тело канала (включая
// расшифрованный секрет) добирается через alert.Service.Channels.
func (s *Service) MonitorChannelIDs(ctx context.Context, monitorID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id FROM monitor_channels
		WHERE monitor_id = $1
		ORDER BY channel_id`, monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: monitor channels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("uptime: monitor channels: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TouchHeartbeat records that a heartbeat monitor just received a ping,
// setting last_beat_at = now(). Used by the public heartbeat endpoint
// (internal/web/heartbeat.go); the missed-ping watchdog (plan 3) reads
// last_beat_at to detect a monitor that stopped pinging
// (last_beat_at + grace < now()).
func (s *Service) TouchHeartbeat(ctx context.Context, monitorID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE monitors SET last_beat_at = now() WHERE id = $1", monitorID)
	if err != nil {
		return fmt.Errorf("uptime: touch heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateHeartbeatToken выдаёт монитору новый heartbeat-токен, сохраняет только
// его sha256 и возвращает сырой токен — он показывается пользователю ОДИН РАЗ
// (в БД плейнтекста нет, «посмотреть» старый URL нельзя, можно лишь перевыпустить).
// Старый токен сразу перестаёт работать. Только для kind=heartbeat.
func (s *Service) RotateHeartbeatToken(ctx context.Context, monitorID int64) (string, error) {
	token, err := generateHeartbeatToken()
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE monitors SET heartbeat_token_hash = $2 WHERE id = $1 AND kind = 'heartbeat'",
		monitorID, heartbeatTokenHash(token))
	if err != nil {
		return "", fmt.Errorf("uptime: rotate heartbeat token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrInvalidMonitor
	}
	return token, nil
}
