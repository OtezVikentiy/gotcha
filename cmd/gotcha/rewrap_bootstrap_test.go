package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// capturingLogHandler — slog.Handler, копящий Record'ы в срез вместо вывода.
// Тот же приём, что internal/alert.capturingLogHandler (rewrap_secrets_test.go):
// не годится под t.Parallel(), потому что slog.SetDefault меняет глобальный
// логгер процесса.
type capturingLogHandler struct {
	records *[]slog.Record
}

func (h capturingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h capturingLogHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h capturingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingLogHandler) WithGroup(string) slog.Handler      { return h }

// TestRewrapAllSecretsCallSiteOrder — структурная проверка контракта
// «бэкфилл до слушателя», который не поднять юнит-тестом целиком (run()
// блокируется на сигнале и требует полного окружения — см. методику в
// internal/guards/handlerassembly_test.go: go/ast разбирает исходник, а не
// компилирует, и не привязан к номерам строк, которые смещаются от правки к
// правке). Мутация «перенести вызов rewrapAllSecrets после ListenAndServe»
// обязана уронить именно этот тест.
func TestRewrapAllSecretsCallSiteOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var rewrapCalls, listenCalls int
	var rewrapPos, listenPos token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "rewrapAllSecrets" {
				rewrapCalls++
				rewrapPos = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "ListenAndServe" {
				listenCalls++
				if listenPos == token.NoPos || call.Pos() < listenPos {
					listenPos = call.Pos()
				}
			}
		}
		return true
	})

	if rewrapCalls != 1 {
		t.Fatalf("вызовов rewrapAllSecrets в main.go = %d, want 1 (единственная точка входа бэкфилла)", rewrapCalls)
	}
	if listenCalls == 0 {
		t.Fatalf("вызовов ListenAndServe в main.go = 0 — тест не нашёл ориентир, проверь имя метода")
	}
	if rewrapPos >= listenPos {
		t.Fatalf("rewrapAllSecrets вызывается на позиции %v, ListenAndServe — на %v: "+
			"бэкфилл обязан идти ДО подъёма слушателя, иначе оператор не увидит итог "+
			"ротации в том же рестарте", fset.Position(rewrapPos), fset.Position(listenPos))
	}

	// Сигнатура без возврата ошибки — часть контракта «отказ прохода не
	// роняет старт»: run() физически не может получить err от rewrapAllSecrets
	// и вернуть его наверх. Мутация «добавить error и уронить старт» обязана
	// уронить эту проверку тоже, не только TestRewrapAllSecretsErrorDoesNotStopStart.
	var decl *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == "rewrapAllSecrets" {
			decl = fn
		}
		return true
	})
	if decl == nil {
		t.Fatalf("объявление func rewrapAllSecrets не найдено в main.go")
	}
	if decl.Type.Results != nil {
		t.Fatalf("rewrapAllSecrets возвращает значения (%v) — контракт «ошибка прохода не "+
			"роняет старт» требует, чтобы функции физически было нечего вернуть наверх",
			decl.Type.Results.List)
	}
}

// newBootstrapOrgAndProject заводит организацию и проект напрямую SQL — тем
// же приёмом, что newEvalProject (internal/alert) и newOrgWithSSO
// (internal/org). Секреты (org_sso/alert_channels/monitors) заводятся ниже
// через сами сервисы, а не так же напрямую: там важен боевой Seal, здесь —
// только окружение (org_id/project_id), к которому секрет привязан.

func newBootstrapOrgAndProject(t *testing.T, pool *pgxpool.Pool, slug string) (orgID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id",
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return orgID, projectID
}

// rawSecretColumn читает секретный столбец по значению столбца-фильтра
// whereCol — org_sso ключуется по org_id, а не по id (см. миграцию 0016),
// alert_channels и monitors — обычным id.
func rawSecretColumn(t *testing.T, pool *pgxpool.Pool, table, column, whereCol string, id int64) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		"SELECT "+column+" FROM "+table+" WHERE "+whereCol+" = $1", id).Scan(&v); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	return v
}

// TestRewrapAllSecretsRotationRoundTrip — сквозной сценарий ротации на уровне
// врезки в bootstrap: секреты трёх хранилищ записаны ключом A → инстанс
// "перезапускается" с кольцом (current=B, prev=A) → rewrapAllSecrets →
// читаемо кольцом ТОЛЬКО из B (старый ключ A инстансу больше не нужен).
// Обратимость — отдельным подтестом (design §7): тот же проход с
// переставленными местами ключами откатывает инстанс назад.
func TestRewrapAllSecretsRotationRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	const (
		keyA = "bootstrap-rotation-master-key-a"
		keyB = "bootstrap-rotation-master-key-b"
	)
	ringA, err := secretbox.NewKeyring(keyA, "")
	if err != nil {
		t.Fatalf("NewKeyring(A): %v", err)
	}

	orgSvc := org.NewService(pool, 1_000_000)
	orgSvc.SetKeyring(ringA)
	alertSvc := alert.NewService(pool)
	alertSvc.SetKeyring(ringA)
	uptimeSvc := uptime.NewService(pool)
	uptimeSvc.SetKeyring(ringA)

	// Организация и проект — сырым SQL (как newEvalProject/newOrgWithSSO в
	// internal/alert и internal/org): сервисам для этого теста нужен только
	// боевой путь записи СЕКРЕТА, не полный флоу регистрации организации.
	orgID, pid := newBootstrapOrgAndProject(t, pool, "bootrot")

	// SSO организации и канал алертов — через сервис (боевой Seal), а не
	// напрямую SQL: именно на этом пути секрет реально шифруется кольцом A.
	if err := orgSvc.UpsertSSO(ctx, org.SSOConfig{
		OrgID: orgID, Issuer: "https://idp.example", ClientID: "client-id",
		ClientSecret: "sso-client-secret-plaintext", Domain: "bootrot.example.com",
	}); err != nil {
		t.Fatalf("UpsertSSO: %v", err)
	}

	chID, err := alertSvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "12345", Secret: "alert-channel-secret-plaintext",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	mon, err := uptimeSvc.Create(ctx, uptime.Monitor{
		ProjectID:         pid,
		Name:              "bootrot",
		Kind:              uptime.KindHTTP,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config: json.RawMessage(`{"method":"GET","url":"https://example.com/health",` +
			`"headers":{"Authorization":"Bearer monitor-header-secret-plaintext"}}`),
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("uptime Create: %v", err)
	}

	// "Рестарт" с кольцом ротации: current=B, prev=A — тем же приёмом, что
	// bootstrap собирает secretRing из GOTCHA_SECRET_KEY/_PREV.
	ringRestart, err := secretbox.NewKeyring(keyB, keyA)
	if err != nil {
		t.Fatalf("NewKeyring(restart): %v", err)
	}
	orgSvc.SetKeyring(ringRestart)
	alertSvc.SetKeyring(ringRestart)
	uptimeSvc.SetKeyring(ringRestart)

	rewrapAllSecrets(ctx, orgSvc, alertSvc, uptimeSvc)

	ringBOnly, err := secretbox.NewKeyring(keyB, "")
	if err != nil {
		t.Fatalf("NewKeyring(B only): %v", err)
	}

	ssoStored := rawSecretColumn(t, pool, "org_sso", "client_secret", "org_id", orgID)
	if !strings.HasPrefix(ssoStored, "enc:v2:"+ringBOnly.CurrentID()+":") {
		t.Fatalf("org_sso.client_secret после бэкфилла = %q, не запечатан текущим (B) ключом", ssoStored)
	}
	ssoOpen, err := ringBOnly.Open(ssoStored)
	if err != nil {
		t.Fatalf("Open(org_sso) кольцом только из B: %v — старый ключ A всё ещё нужен", err)
	}
	if ssoOpen != "sso-client-secret-plaintext" {
		t.Fatalf("org_sso после бэкфилла = %q, want исходный plaintext", ssoOpen)
	}

	chStored := rawSecretColumn(t, pool, "alert_channels", "secret", "id", chID)
	if !strings.HasPrefix(chStored, "enc:v2:"+ringBOnly.CurrentID()+":") {
		t.Fatalf("alert_channels.secret после бэкфилла = %q, не запечатан текущим (B) ключом", chStored)
	}
	chOpen, err := ringBOnly.Open(chStored)
	if err != nil {
		t.Fatalf("Open(alert_channels) кольцом только из B: %v", err)
	}
	if chOpen != "alert-channel-secret-plaintext" {
		t.Fatalf("alert_channels после бэкфилла = %q, want исходный plaintext", chOpen)
	}

	// Мониторы читаются через сервис (заголовки лежат внутри jsonb конфига),
	// но проверяем и сырой текст на префикс ключа — тем же приёмом, что
	// проверочный SELECT из privacy.md §7.
	rawCfg := rawSecretColumn(t, pool, "monitors", "config::text", "id", mon.ID)
	if !strings.Contains(rawCfg, "enc:v2:"+ringBOnly.CurrentID()+":") {
		t.Fatalf("monitors.config после бэкфилла не содержит enc:v2:<B-id>: %s", rawCfg)
	}
	if strings.Contains(rawCfg, "enc:v2:"+ringA.CurrentID()+":") {
		t.Fatalf("monitors.config после бэкфилла всё ещё содержит конверт старого (A) ключа: %s", rawCfg)
	}

	uptimeReadSvc := uptime.NewService(pool)
	uptimeReadSvc.SetKeyring(ringBOnly)
	gotMon, err := uptimeReadSvc.Get(ctx, mon.ID)
	if err != nil {
		t.Fatalf("Get(monitor) кольцом только из B: %v", err)
	}
	var gotCfg struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(gotMon.Config, &gotCfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if gotCfg.Headers["Authorization"] != "Bearer monitor-header-secret-plaintext" {
		t.Fatalf("заголовок монитора после бэкфилла = %q, want исходный plaintext", gotCfg.Headers["Authorization"])
	}

	// Старый ключ A инстансу больше не нужен: кольцом БЕЗ B secretbox.Open
	// не откроет то, что теперь запечатано B.
	ringAOnly, err := secretbox.NewKeyring(keyA, "")
	if err != nil {
		t.Fatalf("NewKeyring(A only): %v", err)
	}
	if _, err := ringAOnly.Open(ssoStored); err == nil {
		t.Fatalf("Open(org_sso) кольцом только из A неожиданно удался — ротация не завершена")
	}

	t.Run("обратимость", func(t *testing.T) {
		// Тот же проход с переставленными местами ключами: current=A, prev=B —
		// откатывает инстанс назад (design §7, «страх необратимости»).
		ringReverse, err := secretbox.NewKeyring(keyA, keyB)
		if err != nil {
			t.Fatalf("NewKeyring(reverse): %v", err)
		}
		orgSvc.SetKeyring(ringReverse)
		alertSvc.SetKeyring(ringReverse)
		uptimeSvc.SetKeyring(ringReverse)

		rewrapAllSecrets(ctx, orgSvc, alertSvc, uptimeSvc)

		ssoBack := rawSecretColumn(t, pool, "org_sso", "client_secret", "org_id", orgID)
		if !strings.HasPrefix(ssoBack, "enc:v2:"+ringA.CurrentID()+":") {
			t.Fatalf("org_sso.client_secret после обратного прохода = %q, want конверт ключа A", ssoBack)
		}
		back, err := ringAOnly.Open(ssoBack)
		if err != nil {
			t.Fatalf("Open(org_sso) кольцом только из A после отката: %v", err)
		}
		if back != "sso-client-secret-plaintext" {
			t.Fatalf("org_sso после отката = %q, want исходный plaintext", back)
		}

		chBack := rawSecretColumn(t, pool, "alert_channels", "secret", "id", chID)
		if !strings.HasPrefix(chBack, "enc:v2:"+ringA.CurrentID()+":") {
			t.Fatalf("alert_channels.secret после обратного прохода = %q, want конверт ключа A", chBack)
		}
	})
}

// TestRewrapAllSecretsErrorDoesNotStopStart — ошибка любого из трёх проходов
// (отказал сам SQL, а не построчная нечитаемость — та обрабатывается внутри
// сервисов и не долетает досюда как err) не паникует и не имеет способа
// прервать bootstrap: rewrapAllSecrets ничего не возвращает (см. также
// структурную проверку сигнатуры в TestRewrapAllSecretsCallSiteOrder), а сама
// функция обязана залогировать по Warn на каждый из трёх отказов и вернуться
// штатно. Мутация «уронить старт при ошибке прохода» ловится либо здесь
// (если бы rewrapAllSecrets начала паниковать/os.Exit), либо структурным
// тестом выше (если бы у неё появился возврат ошибки).
func TestRewrapAllSecretsErrorDoesNotStopStart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ring, err := secretbox.NewKeyring("rewrap-error-does-not-stop-master", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	orgSvc := org.NewService(pool, 1_000_000)
	orgSvc.SetKeyring(ring)
	alertSvc := alert.NewService(pool)
	alertSvc.SetKeyring(ring)
	uptimeSvc := uptime.NewService(pool)
	uptimeSvc.SetKeyring(ring)

	// Закрытый пул — тот же приём, что internal/org.TestSSORewrapSecretsClosedPool
	// и его зеркала в internal/alert и internal/uptime: RewrapSecrets возвращает
	// (0, err) детерминированно, без сетевой гонки.
	pool.Close()

	var records []slog.Record
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prevDefault)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rewrapAllSecrets(context.Background(), orgSvc, alertSvc, uptimeSvc)
	}()
	<-done // паника внутри горутины уронила бы тест целиком, а не молча — этого достаточно

	var warnCount int
	for _, r := range records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "rewrap") {
			warnCount++
		}
	}
	if warnCount != 3 {
		t.Fatalf("Warn-записей о неудавшемся бэкфилле = %d, want 3 (org+alert+uptime); "+
			"записи: %+v", warnCount, records)
	}
}
