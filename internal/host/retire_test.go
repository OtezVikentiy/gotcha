package host_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeRetireNotifier — host.RetireNotifier для тестов: копит вызовы и умеет
// падать (модель недоступного канала/outbox).
type fakeRetireNotifier struct {
	mu      sync.Mutex
	retired []retireCall
	err     error
}

type retireCall struct {
	host host.Host
	open []host.Incident
}

func (f *fakeRetireNotifier) HostRetired(_ context.Context, h host.Host, open []host.Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.retired = append(f.retired, retireCall{host: h, open: open})
	return nil
}

func (f *fakeRetireNotifier) calls() []retireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]retireCall(nil), f.retired...)
}

// incidentRow читает статус и флаг уведомления инцидента напрямую.
func incidentRow(t *testing.T, pool *pgxpool.Pool, id int64) (status string, resolved bool, notifiedClose bool) {
	t.Helper()
	var resolvedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT status, resolved_at, notified_close FROM host_incidents WHERE id = $1", id).
		Scan(&status, &resolvedAt, &notifiedClose); err != nil {
		t.Fatalf("read incident %d: %v", id, err)
	}
	return status, resolvedAt != nil, notifiedClose
}

func hostExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM hosts WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count hosts: %v", err)
	}
	return n > 0
}

// openIncident открывает инцидент вида kind у хоста.
func openIncident(t *testing.T, svc *host.IncidentService, projectID int64, h host.Host, kind string) host.Incident {
	t.Helper()
	in, created, err := svc.Open(context.Background(), projectID, h.ID, kind, 0.99, "")
	if err != nil || !created {
		t.Fatalf("open incident %s: created=%v err=%v", kind, created, err)
	}
	return in
}

// TestRetirerClosesAndAnnouncesOpenIncidents — решение владельца на приёмке
// A1: истёкший хост не должен исчезать молча. Открытые инциденты закрываются,
// о снятии с наблюдения уходит уведомление — и только после этого чистильщик
// удаляет строку.
func TestRetirerClosesAndAnnouncesOpenIncidents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	incidents := host.NewIncidentService(pool)
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "retire-me")

	// Мёртвый сервер держит открытыми сразу несколько порогов: оценщик считает
	// только живые хосты, поэтому disk/load остаются висеть с последнего
	// живого тика, а сверху добавляется «Тишина».
	silent := openIncident(t, incidents, projectID, h, "silent")
	disk := openIncident(t, incidents, projectID, h, "disk")

	notifier := &fakeRetireNotifier{}
	r := &host.Retirer{Hosts: host.NewStore(pool), Incidents: incidents, Notifier: notifier}

	if err := r.Retire(ctx, []int64{h.ID}); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("уведомлений = %d, want 1 на хост (а не на инцидент)", len(calls))
	}
	if calls[0].host.Name != "retire-me" {
		t.Errorf("уведомление про хост %q", calls[0].host.Name)
	}
	if len(calls[0].open) != 2 {
		t.Errorf("в уведомление попало %d открытых инцидентов, want 2", len(calls[0].open))
	}
	for _, id := range []int64{silent.ID, disk.ID} {
		status, resolved, notified := incidentRow(t, pool, id)
		if status != "resolved" || !resolved {
			t.Errorf("инцидент %d: status=%q resolved_at?=%v — должен быть закрыт", id, status, resolved)
		}
		if !notified {
			t.Errorf("инцидент %d: notified_close=false, хотя уведомление ушло", id)
		}
	}
}

// TestRetirerSilentForHostWithoutIncidents — хост, о котором рассказывать
// нечего, снимается молча: уведомление о снятии имеет смысл только как
// закрытие открытого инцидента.
func TestRetirerSilentForHostWithoutIncidents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "no-incidents")
	// Закрытый инцидент уведомления тоже не порождает — о нём уже сообщали,
	// когда он закрывался.
	closed := openIncident(t, host.NewIncidentService(pool), projectID, h, "disk")
	if _, err := host.NewIncidentService(pool).Resolve(context.Background(), closed.ID, 0.5); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	notifier := &fakeRetireNotifier{}
	r := &host.Retirer{Hosts: host.NewStore(pool), Incidents: host.NewIncidentService(pool), Notifier: notifier}

	if err := r.Retire(context.Background(), []int64{h.ID}); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if got := len(notifier.calls()); got != 0 {
		t.Errorf("уведомлений = %d, want 0 — открытых инцидентов у хоста нет", got)
	}
}

// TestRetirerKeepsIncidentOpenWhenNotifyFails — порядок «сначала сообщить,
// потом закрыть». Провал уведомления обязан оставить инцидент ОТКРЫТЫМ: иначе
// следующий проход не нашёл бы, о чём рассказывать, и удалил бы хост молча —
// ровно то, что этот механизм и предотвращает.
func TestRetirerKeepsIncidentOpenWhenNotifyFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	incidents := host.NewIncidentService(pool)
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "notify-fails")
	in := openIncident(t, incidents, projectID, h, "silent")

	notifier := &fakeRetireNotifier{err: errors.New("outbox down")}
	r := &host.Retirer{Hosts: host.NewStore(pool), Incidents: incidents, Notifier: notifier}

	if err := r.Retire(context.Background(), []int64{h.ID}); err == nil {
		t.Fatal("Retire вернул nil при провале уведомления — чистильщик удалил бы хост")
	}
	status, resolved, _ := incidentRow(t, pool, in.ID)
	if status != "open" || resolved {
		t.Errorf("инцидент закрыт (status=%q resolved_at?=%v), хотя сообщить не удалось", status, resolved)
	}
}

// TestEntityJanitorRetiresHostsBeforeDelete — сцепка целиком, как в main.go:
// правило hosts чистильщика с хуком host.Retirer. Проверяется весь порядок —
// уведомление, закрытие, удаление, — потому что по отдельности каждая часть
// зелёная и в неправильном порядке.
func TestEntityJanitorRetiresHostsBeforeDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	incidents := host.NewIncidentService(pool)
	projectID := seedEvalProject(t, pool)

	stale := seedEvalHost(t, pool, projectID, "stale-host")
	setHostLastSeen(t, pool, stale.ID, time.Now().UTC().Add(-40*24*time.Hour))
	openIncident(t, incidents, projectID, stale, "silent")

	quiet := seedEvalHost(t, pool, projectID, "stale-no-incidents")
	setHostLastSeen(t, pool, quiet.ID, time.Now().UTC().Add(-40*24*time.Hour))

	live := seedEvalHost(t, pool, projectID, "live-host")
	openIncident(t, incidents, projectID, live, "disk")

	notifier := &fakeRetireNotifier{}
	retirer := &host.Retirer{Hosts: host.NewStore(pool), Incidents: incidents, Notifier: notifier}
	j := &telemetry.EntityJanitor{
		Pool:      pool,
		Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour},
		PreDelete: map[string]telemetry.PreDeleteHook{"hosts": retirer.Retire},
	}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if hostExists(t, pool, stale.ID) {
		t.Errorf("истёкший хост не удалён — снятие с наблюдения не должно его сохранять")
	}
	if hostExists(t, pool, quiet.ID) {
		t.Errorf("истёкший хост без инцидентов не удалён")
	}
	if !hostExists(t, pool, live.ID) {
		t.Errorf("живой хост удалён")
	}

	calls := notifier.calls()
	if len(calls) != 1 {
		t.Fatalf("уведомлений = %d, want 1 (только истёкший хост с открытым инцидентом)", len(calls))
	}
	if calls[0].host.Name != "stale-host" {
		t.Errorf("уведомление про %q, want stale-host", calls[0].host.Name)
	}
}

// TestEntityJanitorKeepsHostWhenRetireFails — провал снятия отменяет удаление
// батча: хост доживает до следующего прохода вместе со своим инцидентом.
// Соседнее правило (host_incidents) при этом отрабатывает — отказ одного
// правила не отменяет остальные.
func TestEntityJanitorKeepsHostWhenRetireFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	incidents := host.NewIncidentService(pool)
	projectID := seedEvalProject(t, pool)

	stale := seedEvalHost(t, pool, projectID, "retire-broken")
	setHostLastSeen(t, pool, stale.ID, time.Now().UTC().Add(-40*24*time.Hour))
	in := openIncident(t, incidents, projectID, stale, "silent")

	retirer := &host.Retirer{
		Hosts:     host.NewStore(pool),
		Incidents: incidents,
		Notifier:  &fakeRetireNotifier{err: errors.New("outbox down")},
	}
	j := &telemetry.EntityJanitor{
		Pool:      pool,
		Retention: telemetry.Retentions{Metrics: 30 * 24 * time.Hour},
		PreDelete: map[string]telemetry.PreDeleteHook{"hosts": retirer.Retire},
	}
	if _, err := j.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if !hostExists(t, pool, stale.ID) {
		t.Errorf("хост удалён, хотя сообщить о снятии не удалось")
	}
	if status, _, _ := incidentRow(t, pool, in.ID); status != "open" {
		t.Errorf("инцидент %d закрыт при неудачном снятии: status=%q", in.ID, status)
	}
}
