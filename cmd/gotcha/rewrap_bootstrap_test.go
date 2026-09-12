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

// Не годится под t.Parallel(): slog.SetDefault меняет глобальный логгер процесса.
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

func TestWireSecretRingBuildsKeyringFromCurrentAndPrevious(t *testing.T) {
	const (
		current  = "wire-secret-ring-master-current"
		previous = "wire-secret-ring-master-previous"
	)

	ring, err := wireSecretRing(current, previous, nil, nil, nil)
	if err != nil {
		t.Fatalf("wireSecretRing: %v", err)
	}

	wantCurrent, err := secretbox.NewKeyring(current, "")
	if err != nil {
		t.Fatalf("NewKeyring(current): %v", err)
	}
	wantPrevious, err := secretbox.NewKeyring(previous, "")
	if err != nil {
		t.Fatalf("NewKeyring(previous): %v", err)
	}

	if ring.CurrentID() != wantCurrent.CurrentID() {
		t.Fatalf("CurrentID() = %q, want %q (id ключа, выведенного из current) — "+
			"wireSecretRing собрал кольцо не из cfg.SecretKey", ring.CurrentID(), wantCurrent.CurrentID())
	}
	if ring.PreviousID() != wantPrevious.CurrentID() {
		t.Fatalf("PreviousID() = %q, want %q (id ключа, выведенного из previous): "+
			"если внутри wireSecretRing NewKeyring зовётся с пустой строкой вместо "+
			"previous-параметра, PreviousID() вернётся пустым и этот ассерт провалится — "+
			"именно так выжила находка ревью «кольцо теряет предыдущий ключ, "+
			"ротация молча перестаёт работать»", ring.PreviousID(), wantPrevious.CurrentID())
	}
}

func TestWireSecretRingDistributesSameRingToAllThree(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if f, ok := n.(*ast.FuncDecl); ok && f.Name.Name == "wireSecretRing" {
			fn = f
		}
		return true
	})
	if fn == nil {
		t.Fatalf("объявление func wireSecretRing не найдено в main.go")
	}

	var ringIdent string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewKeyring" {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			ringIdent = id.Name
		}
		return true
	})
	if ringIdent == "" {
		t.Fatalf("не нашёл присваивание результата secretbox.NewKeyring внутри wireSecretRing — " +
			"тест не нашёл ориентир, проверь, что сборка кольца всё ещё выглядит как " +
			"`ring, err := secretbox.NewKeyring(...)`")
	}

	var receivers, args []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetKeyring" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			t.Fatalf("SetKeyring вызван на получателе типа %T, не на простом идентификаторе — "+
				"тест не умеет это разобрать, проверь вручную", sel.X)
		}
		receivers = append(receivers, recv.Name)
		if len(call.Args) != 1 {
			t.Fatalf("SetKeyring вызван у %s с %d аргументами, want 1", recv.Name, len(call.Args))
		}
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok {
			t.Fatalf("аргумент SetKeyring у %s — не идентификатор (%T)", recv.Name, call.Args[0])
		}
		args = append(args, arg.Name)
		return true
	})

	if len(receivers) != 3 {
		t.Fatalf("вызовов SetKeyring внутри wireSecretRing = %d (получатели: %v), want 3 "+
			"(orgSvc+alertSvc+uptimeSvc) — пропавший узел раздачи молча оставляет сервис "+
			"без кольца на время ротации", len(receivers), receivers)
	}
	seen := map[string]bool{}
	for _, r := range receivers {
		if seen[r] {
			t.Fatalf("получатель %q встречается среди SetKeyring дважды (%v) — ожидались три "+
				"РАЗНЫХ сервиса, а не двойная раздача одному и пропуск другого", r, receivers)
		}
		seen[r] = true
	}
	for i, arg := range args {
		if arg != ringIdent {
			t.Fatalf("SetKeyring у %s вызван с %q, want %q (кольцо, собранное secretbox.NewKeyring "+
				"выше по функции): сервис получил бы ДРУГОЕ кольцо, чем остальные два, и молчаливо "+
				"потерял бы доступ к части секретов на время ротации", receivers[i], arg, ringIdent)
		}
	}
}

// Секреты ниже заводятся через сами сервисы, не так же напрямую SQL: там
// важен боевой Seal, здесь — только окружение (org_id/project_id).

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

// org_sso ключуется по org_id, не id (миграция 0016); alert_channels и
// monitors — обычным id.
func rawSecretColumn(t *testing.T, pool *pgxpool.Pool, table, column, whereCol string, id int64) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		"SELECT "+column+" FROM "+table+" WHERE "+whereCol+" = $1", id).Scan(&v); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	return v
}

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

	orgID, pid := newBootstrapOrgAndProject(t, pool, "bootrot")

	// SSO и канал алертов — через сервис (боевой Seal): именно на этом пути
	// секрет реально шифруется кольцом A.
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

	// Раздача — через сам wireSecretRing, не вручную тремя SetKeyring: тест
	// исполняет реальный код раздачи, а не только его структуру.
	if _, err := wireSecretRing(keyB, keyA, orgSvc, alertSvc, uptimeSvc); err != nil {
		t.Fatalf("wireSecretRing(restart): %v", err)
	}

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
	// но проверяем и сырой текст на префикс ключа.
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
		// откатывает инстанс назад.
		if _, err := wireSecretRing(keyA, keyB, orgSvc, alertSvc, uptimeSvc); err != nil {
			t.Fatalf("wireSecretRing(reverse): %v", err)
		}

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

	// Закрытый пул: RewrapSecrets возвращает (0, err) детерминированно, без
	// сетевой гонки.
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
