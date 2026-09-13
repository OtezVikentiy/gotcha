package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Принимает соединение и молчит — SMTP-клиент виснет на ожидании приветствия, пока тест
// не закроет листенер. Синхронный обработчик получил бы эту задержку прямо в ответ.
func fakeHangingSMTP(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-make(chan struct{}) // до Close() листенера тестом
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	portNum, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return h, portNum
}

// Роль и создание приглашения идут через обычный пул (h.Org, sessionAuth). h.Auth сидит на
// пуле с одним свободным соединением из двух — второе держит тест; SMTP зависает намертво.
// Если бы recipientLocale/inviter-lookup/отправка остались на пути ответа, обработчик
// не ответил бы за отведённое время: либо упёршись в пул, либо повиснув на SMTP.
func TestOrgSettingsInviteRespondsWhileBackgroundEmailIsBlocked(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fullPool, err := db.NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("full pool: %v", err)
	}
	t.Cleanup(fullPool.Close)

	authCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	authCfg.MaxConns = 2
	authPool, err := pgxpool.NewWithConfig(context.Background(), authCfg)
	if err != nil {
		t.Fatalf("auth pool: %v", err)
	}
	t.Cleanup(authPool.Close)

	sessionAuth := auth.NewService(fullPool)
	orgSvc := org.NewService(fullPool, 1_000_000)

	ownerID, err := sessionAuth.Register(context.Background(), "invite-block-owner@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	token, err := sessionAuth.CreateSession(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	o, err := orgSvc.CreateOrg(context.Background(), "invite-block-co", "Invite Block Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	host, port := fakeHangingSMTP(t)
	h := &Handler{
		BaseURL: "http://gotcha.example",
		Auth:    auth.NewService(authPool),
		Org:     orgSvc,
		Email:   notify.NewEmailSender(notify.EmailConfig{Host: host, Port: port, From: "noreply@gotcha.test"}),
	}

	// Одно из двух соединений держим занятым — оставшееся обслуживает и ответ (currentEmail),
	// и, будь фон синхронным, конкурировало бы с ним же за recipientLocale/inviter-lookup.
	held, err := authPool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()

	body := "email=" + url.QueryEscape("invite-block-recipient@example.com") + "&role=member"
	r := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings/invite", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	r.SetPathValue("id", strconv.FormatInt(o.ID, 10))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		sessionAuth.RequireUser(http.HandlerFunc(h.orgSettingsInvite)).ServeHTTP(rec, r)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("обработчик не ответил вовремя — письмо (лукап получателя или сама SMTP-сессия) осталось на пути ответа")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

type responsePathMarkerKey struct{}

// Пишет только запросы с ctx, унаследованным от r.Context() (см. responsePathMarkerKey) —
// запросы sendInviteEmail идут на отдельном context.Background(), в трассу не попадают
// в принципе, гонка с фоновой горутиной структурно исключена, а не подавлена таймингом.
type recordingTracer struct {
	mu  sync.Mutex
	seq []string
}

func (rt *recordingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if marked, _ := ctx.Value(responsePathMarkerKey{}).(bool); marked {
		rt.mu.Lock()
		rt.seq = append(rt.seq, strings.Join(strings.Fields(data.SQL), " "))
		rt.mu.Unlock()
	}
	return ctx
}

func (rt *recordingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (rt *recordingTracer) sequence() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, len(rt.seq))
	copy(out, rt.seq)
	return out
}

// tag различает два прогона одного теста (owner/invitee не должны конфликтовать по email).
func inviteResponsePathQuerySequence(t *testing.T, tag string, registerInvitee bool) []string {
	t.Helper()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fullPool, err := db.NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("full pool: %v", err)
	}
	t.Cleanup(fullPool.Close)

	tracer := &recordingTracer{}
	authCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	authCfg.ConnConfig.Tracer = tracer
	authPool, err := pgxpool.NewWithConfig(context.Background(), authCfg)
	if err != nil {
		t.Fatalf("auth pool: %v", err)
	}
	t.Cleanup(authPool.Close)

	sessionAuth := auth.NewService(fullPool)
	orgSvc := org.NewService(fullPool, 1_000_000)

	ownerID, err := sessionAuth.Register(context.Background(), "invite-trace-owner-"+tag+"@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	token, err := sessionAuth.CreateSession(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	o, err := orgSvc.CreateOrg(context.Background(), "invite-trace-co-"+tag, "Invite Trace Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	inviteeEmail := "invite-trace-invitee-" + tag + "@example.com"
	if registerInvitee {
		if _, err := sessionAuth.Register(context.Background(), inviteeEmail, "correct-horse-battery"); err != nil {
			t.Fatalf("register invitee: %v", err)
		}
	}

	h := &Handler{
		BaseURL: "http://gotcha.example",
		Auth:    auth.NewService(authPool),
		Org:     orgSvc,
		// Порт 1 отказывает немедленно (connection refused) — фон уходит в лог warn, ответа
		// это не касается: он уже отдан до того, как sendInviteEmail дозвонится до SMTP.
		Email: notify.NewEmailSender(notify.EmailConfig{Host: "127.0.0.1", Port: 1, From: "noreply@gotcha.test"}),
	}

	body := "email=" + url.QueryEscape(inviteeEmail) + "&role=member"
	r := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings/invite", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	r.SetPathValue("id", strconv.FormatInt(o.ID, 10))
	r = r.WithContext(context.WithValue(r.Context(), responsePathMarkerKey{}, true))
	rec := httptest.NewRecorder()

	sessionAuth.RequireUser(http.HandlerFunc(h.orgSettingsInvite)).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return tracer.sequence()
}

// Инвариант, а не тайминг: набор и порядок SQL-запросов на пути ответа не должны зависеть
// от того, найден ли адресат — иначе сам факт лишнего запроса (не его длительность) выдаёт
// существование адреса. Аргументы намеренно не сравниваются (email заявки — законное отличие
// в аргументах INSERT, который проходит одинаково в обоих прогонах через h.Org, не h.Auth).
func TestOrgSettingsInviteResponsePathQueriesIdenticalRegardlessOfRecipientRegistration(t *testing.T) {
	registered := inviteResponsePathQuerySequence(t, "reg", true)
	unregistered := inviteResponsePathQuerySequence(t, "unreg", false)
	if !reflect.DeepEqual(registered, unregistered) {
		t.Fatalf("последовательность запросов на пути ответа зависит от регистрации адресата:\nзарегистрирован:    %v\nне зарегистрирован: %v", registered, unregistered)
	}
}
