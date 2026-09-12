package uptime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

var (
	ErrNotFound       = errors.New("uptime: not found")
	ErrInvalidMonitor = errors.New("uptime: invalid monitor")
)

type Service struct {
	pool *pgxpool.Pool

	// реальное имя региона встроенной пробы (cfg.LocalRegion), которым лизит Runner —
	// хардкодить "local" нельзя, иначе монитор в другом регионе не проверится никогда.
	LocalRegion string

	// secretKeySet=false (dev, ключ не задан) — заголовки хранятся plaintext,
	// читатель распознаёт по отсутствию префикса "enc:".
	ring         secretbox.Keyring
	secretKeySet bool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) SetKeyring(ring secretbox.Keyring) {
	s.ring = ring
	s.secretKeySet = true
}

// в БД заголовки не должны лежать plaintext — их видит роль operator.
func (s *Service) encryptMonitorConfig(kind Kind, raw json.RawMessage) (json.RawMessage, error) {
	if !s.secretKeySet || kind != KindHTTP {
		return raw, nil
	}
	return sealHTTPHeaders(s.ring, raw)
}

// ошибка = нечитаемый ciphertext — вызывающий сам решает, ронять операцию или
// деградировать поштучно; без ключа enc:-заголовки обнуляются, а не текут наружу.
func (s *Service) decryptMonitorConfig(m *Monitor) error {
	if m.Kind != KindHTTP {
		return nil
	}
	if !s.secretKeySet {
		scrubbedCfg, scrubbed, err := scrubEncryptedHeaders(m.Config)
		if err != nil {
			return err
		}
		if scrubbed {
			slog.Error("uptime: monitor has encrypted header values but no master key is set; header values dropped",
				"monitor_id", m.ID)
		}
		m.Config = scrubbedCfg
		return nil
	}
	opened, err := openHTTPHeaders(s.ring, m.Config)
	if err != nil {
		return err
	}
	m.Config = opened
	return nil
}

// сколько нечитаемых заголовков бэкфилл логирует подробно за один проход.
const rewrapLogCap = 5

// курсор закрывается до UPDATE — держать его открытым при записи по тому же
// пулу нельзя; апгрейд идёт по значению, нечитаемый сосед в строке не мешает.
func (s *Service) RewrapSecrets(ctx context.Context) (int, error) {
	if !s.secretKeySet {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT id, config FROM monitors WHERE kind = $1", string(KindHTTP))
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id  int64
		cfg json.RawMessage
	}
	var todo []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.cfg); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var updated, unreadable, logged int
	for _, c := range todo {
		next, changed, failures := rewrapHTTPHeaders(s.ring, c.cfg)
		unreadable += len(failures)
		for _, f := range failures {
			if logged < rewrapLogCap {
				slog.Error("uptime: rewrap secrets: header value unreadable, left as is",
					"monitor_id", c.id, "err", f)
				logged++
			}
		}
		if !changed {
			continue
		}
		ok, err := s.casUpdateMonitorConfig(ctx, c.id, next, c.cfg)
		if err != nil {
			slog.Warn("uptime: rewrap secrets: update monitor failed", "monitor_id", c.id, "err", err)
			continue
		}
		if !ok {
			continue // config изменили между чтением и записью — не наша забота
		}
		updated++
	}
	slog.Info("uptime: rewrap secrets done", "updated", updated, "unreadable_skipped", unreadable)
	return updated, nil
}

// отдельный метод — CAS-предикат проверяется детерминированным тестом,
// без гонки по времени с остальным циклом бэкфилла.
func (s *Service) casUpdateMonitorConfig(ctx context.Context, id int64, newCfg, oldCfg json.RawMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"UPDATE monitors SET config = $2 WHERE id = $1 AND config = $3::jsonb", id, newCfg, oldCfg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Service) localRegion() string {
	if s.LocalRegion == "" {
		return DefaultRegion
	}
	return s.LocalRegion
}

func generateHeartbeatToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// в БД хранится только хеш, не сам токен; вызывающий видит сырой токен
// один раз при Create, дальше пинг хешируется и ищется по хешу.
func heartbeatTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

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

// не кросс-тенантная защита — та обеспечена скоупом лиз по организации;
// здесь только против монитора в регионе, который никто не лизит.
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
	// в БД — только sha256 токена; сырой остаётся в m и показывается один раз.
	var heartbeatTokenHashVal []byte
	if m.HeartbeatToken != "" {
		heartbeatTokenHashVal = heartbeatTokenHash(m.HeartbeatToken)
	}

	// Значения заголовков шифруем at-rest; в возвращаемом мониторе m.Config
	// остаётся plaintext (сервис отдаёт расшифрованное — форме и живой проверке).
	storedConfig, err := s.encryptMonitorConfig(m.Kind, m.Config)
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO monitors (project_id, name, kind, enabled, interval_seconds, timeout_seconds,
			config, fail_threshold, recovery_threshold, consensus, remind_every_minutes,
			ssl_alert_days, heartbeat_token_hash, retries)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id, created_at`,
		m.ProjectID, m.Name, string(m.Kind), m.Enabled, m.IntervalSeconds, m.TimeoutSeconds,
		storedConfig, m.FailThreshold, m.RecoveryThreshold, string(m.Consensus), m.RemindEveryMinutes,
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

// kind и heartbeat_token не меняются через Update, даже если m содержит
// другие значения — читаются из БД до валидации и записи.
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

	// точка ленивой миграции: форма грузит монитор через Get (расшифровка),
	// пользователь сохраняет — Update кладёт обратно enc:.
	storedConfig, err := s.encryptMonitorConfig(m.Kind, m.Config)
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE monitors SET name=$2, enabled=$3, interval_seconds=$4, timeout_seconds=$5,
			config=$6, fail_threshold=$7, recovery_threshold=$8, consensus=$9,
			remind_every_minutes=$10, ssl_alert_days=$11, retries=$12
		WHERE id = $1`,
		m.ID, m.Name, m.Enabled, m.IntervalSeconds, m.TimeoutSeconds, storedConfig,
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
	// иначе строка состояния снятого региона зависает навсегда (её больше
	// некому перезаписать), а его задание в очереди ещё раз выполнится.
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

// каскадом (FK ON DELETE CASCADE) удаляются regions/channels/state/инциденты.
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

// мониторы без регионов отсутствуют в карте — GetBatch читает нулевым
// значением для nil-слайса, порядок внутри монитора тот же, что у regionsOf.
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

// HeartbeatToken не восстанавливается: в БД только его sha256; сырой
// вызывающий видит один раз при Create.
func scanMonitor(row pgx.Row, m *Monitor) error {
	return row.Scan(&m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
		&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
		&m.SSLAlertDays, &m.SSLExpiresAt, &m.LastBeatAt, &m.CreatedAt, &m.Retries)
}

const monitorColumns = `project_id, name, kind, enabled, interval_seconds, timeout_seconds, config,
	fail_threshold, recovery_threshold, consensus, remind_every_minutes, ssl_alert_days,
	ssl_expires_at, last_beat_at, created_at, retries`

func (s *Service) Get(ctx context.Context, monitorID int64) (Monitor, error) {
	m := Monitor{ID: monitorID}
	row := s.pool.QueryRow(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE id = $1", monitorID)
	if err := scanMonitor(row, &m); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Monitor{}, ErrNotFound
		}
		return Monitor{}, fmt.Errorf("uptime: get: %w", err)
	}
	// Get отдаёт расшифрованные заголовки — его читают форма редактирования
	// и разовая живая проверка.
	if err := s.decryptMonitorConfig(&m); err != nil {
		return Monitor{}, fmt.Errorf("uptime: get: decrypt headers: %w", err)
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

// удалённый монитор получает нулевой Monitor{ID: id} — ProjectID=0 и есть сигнал
// «нет»; Config тут в виде enc: (не расшифрован) — не скармливать обратно в Update.
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

// Config здесь НЕ расшифрован (в отличие от Get) — значения как в БД (enc:);
// не скармливать обратно в Update, за реальными заголовками — в Get.
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

// новая expires позже прежней — новый сертификат, обнуляем ssl_alerted_days
// (старые «осталось N дней» уже неактуальны); сравнение и обнуление — в одном UPDATE.
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

// nil — своих каналов нет, откат на проектные; выключенные не фильтруются здесь,
// иначе Notify ушёл бы во все каналы проекта; тела каналов — через alert.Service.
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

// сырой токен виден один раз; посмотреть прежний нельзя, только перевыпустить —
// старый сразу перестаёт работать. Только для kind=heartbeat.
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
