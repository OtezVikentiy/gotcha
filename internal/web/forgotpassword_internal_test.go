package web

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func postForgotPassword(h *Handler, email, remoteAddr string) *httptest.ResponseRecorder {
	body := "email=" + url.QueryEscape(email)
	r := httptest.NewRequest(http.MethodPost, "/forgot-password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.forgotPasswordSubmit(rec, r)
	return rec
}

// Требование безопасности: перебор адресов не должен раскрывать, какие из них
// зарегистрированы — ответ обязан быть побайтово одинаковым.
func TestForgotPasswordSubmitIdenticalResponseForExistingAndUnknownEmail(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = true // h.Email остаётся nil — доставка пропускается, ответ от неё не зависит

	if _, err := h.Auth.Register(context.Background(), "exists@example.com", "password123"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	existing := postForgotPassword(h, "exists@example.com", "203.0.113.60:1")
	unknown := postForgotPassword(h, "nosuchuser@example.com", "203.0.113.61:1")

	if existing.Code != unknown.Code {
		t.Fatalf("status differs: existing=%d unknown=%d", existing.Code, unknown.Code)
	}
	if existing.Body.String() != unknown.Body.String() {
		t.Fatalf("body differs between an existing and an unknown email:\nexisting=%q\nunknown=%q",
			existing.Body.String(), unknown.Body.String())
	}
}

// passwordResetIPLimiter проверяется раньше per-address passwordResetEmailLimiter — тот же
// порядок и обоснование, что у loginSubmit (TestLoginSubmitIPLimiterBoundsLoginLimiterGrowth).
func TestForgotPasswordSubmitIPLimiterBoundsEmailLimiterGrowth(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = true

	const attempts = 50
	const ipLimit = 20 // см. authTestHandler: passwordResetIPLimiter limit
	var limited int
	for i := 0; i < attempts; i++ {
		rec := postForgotPassword(h, fmt.Sprintf("attacker%d@example.com", i), "203.0.113.70:1")
		if rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}

	if want := attempts - ipLimit; limited != want {
		t.Errorf("отказов 429 = %d, want %d — passwordResetIPLimiter обязан пропускать ровно %d запросов с одного IP в минуту, остальные отсекать ДО passwordResetEmailLimiter", limited, want, ipLimit)
	}
	if got := h.passwordResetEmailLimiter.size(); got != ipLimit {
		t.Errorf("passwordResetEmailLimiter.size() = %d, want %d — один IP не должен заводить в лимитер больше ключей, чем разрешает passwordResetIPLimiter", got, ipLimit)
	}
}

// КРИТИЧНО: бюджет восстановления обязан быть СВОИМ — поток запросов на адрес жертвы с чужих
// IP не должен повлиять на способность жертвы войти паролем со своего собственного IP.
func TestForgotPasswordFloodDoesNotLockOutVictimLogin(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = true

	const victimEmail = "victim@example.com"
	const victimPassword = "correct-horse-1"
	if _, err := h.Auth.Register(context.Background(), victimEmail, victimPassword); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 50 запросов восстановления на адрес жертвы, каждый со своего IP — passwordResetIPLimiter
	// (по IP атакующего) не мешает ни одному из них, только passwordResetEmailLimiter мог бы.
	for i := 0; i < 50; i++ {
		postForgotPassword(h, victimEmail, fmt.Sprintf("198.51.100.%d:1", i%250))
	}

	// Жертва входит паролем со своего IP — не должна получить 429 из-за чужого потока восстановлений.
	body := "email=" + url.QueryEscape(victimEmail) + "&password=" + url.QueryEscape(victimPassword)
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.RemoteAddr = "203.0.113.200:1"
	rec := httptest.NewRecorder()
	h.loginSubmit(rec, r)

	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("жертва получила 429 на собственном входе из-за потока /forgot-password на её адрес: %s", rec.Body.String())
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303 (успешный вход верным паролем): %s", rec.Code, rec.Body.String())
	}
}

// Требование: инстанс без настроенной почты честно отказывает, а не показывает форму,
// которая молча ничего не отправит.
func TestForgotPasswordPageUnavailableWithoutEmailHidesForm(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = false

	r := httptest.NewRequest(http.MethodGet, "/forgot-password", nil)
	rec := httptest.NewRecorder()
	h.forgotPasswordPage(rec, r)

	body := rec.Body.String()
	if strings.Contains(body, `name="email"`) {
		t.Errorf("форма с email показана при выключенной почте: %s", body)
	}
	want := i18n.T(context.Background(), "auth.forgot.unavailable")
	if !strings.Contains(body, want) {
		t.Errorf("страница не сообщает о недоступности восстановления: %s", body)
	}
}

// Прямой POST мимо скрытой на GET формы обязан отказать так же, как GET — иначе UI-скрытие
// формы ничего не защищает.
func TestForgotPasswordSubmitUnavailableWithoutEmailIgnoresPost(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = false

	if _, err := h.Auth.Register(context.Background(), "exists2@example.com", "password123"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := postForgotPassword(h, "exists2@example.com", "203.0.113.80:1")

	if strings.Contains(rec.Body.String(), i18n.T(context.Background(), "auth.forgot.sent")) {
		t.Errorf("POST при выключенной почте всё равно отрендерил «письмо отправлено»: %s", rec.Body.String())
	}
	if got := h.passwordResetEmailLimiter.size(); got != 0 {
		t.Errorf("passwordResetEmailLimiter.size() = %d, want 0 — запрос при выключенной почте не должен доходить до лимитера/БД", got)
	}
}

// Отвечает с задержкой, но не бесконечно, и закрывает done по завершении обмена — тест
// дожидается done, иначе горутина переживает тест и гоняется за slog с соседними тестами.
func fakeDelayedSMTP(t *testing.T, delay time.Duration) (host string, port int, done <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(delay)
		tp := textproto.NewConn(conn)
		_ = tp.PrintfLine("220 fake.smtp ready")
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_ = tp.PrintfLine("250 fake.smtp")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				_ = tp.PrintfLine("250 OK")
			case line == "DATA":
				_ = tp.PrintfLine("354 go ahead")
				for {
					body, err := tp.ReadLine()
					if err != nil {
						return
					}
					if body == "." {
						break
					}
				}
				_ = tp.PrintfLine("250 OK")
			case strings.HasPrefix(line, "QUIT"):
				_ = tp.PrintfLine("221 bye")
				return
			default:
				_ = tp.PrintfLine("250 OK")
			}
		}
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var portNum int
	if _, err := fmt.Sscanf(p, "%d", &portNum); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return h, portNum, finished
}

// Требование безопасности: SMTP-задержка не должна быть таймингом, по которому различимо
// существование адреса — доставка обязана уйти в фон, а не блокировать ответ клиенту.
func TestForgotPasswordSubmitDoesNotBlockOnSlowSMTP(t *testing.T) {
	h := authTestHandler(t)
	h.EmailEnabled = true
	const smtpDelay = 1500 * time.Millisecond
	host, port, done := fakeDelayedSMTP(t, smtpDelay)
	h.Email = notify.NewEmailSender(notify.EmailConfig{Host: host, Port: port, From: "noreply@gotcha.test"})

	if _, err := h.Auth.Register(context.Background(), "slow@example.com", "password123"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	start := time.Now()
	rec := postForgotPassword(h, "slow@example.com", "203.0.113.90:1")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed >= smtpDelay {
		t.Errorf("forgotPasswordSubmit заняло %v (>= задержки SMTP %v) — ответ не должен ждать доставку письма", elapsed, smtpDelay)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("фоновая доставка не завершилась вовремя")
	}
}

// Отвечает 250 на EHLO, затем рвёт с ошибкой на MAIL FROM — эмулирует реальный сбой SMTP,
// не блэкхол: ошибка возвращается синхронно и её можно проверить на содержимое.
func fakeRejectingMailFrom(t *testing.T) (host string, port int) {
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
		tp := textproto.NewConn(conn)
		_ = tp.PrintfLine("220 fake.smtp ready")
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_ = tp.PrintfLine("250 fake.smtp")
			case strings.HasPrefix(line, "MAIL FROM"):
				_ = tp.PrintfLine("450 mailbox unavailable")
			case strings.HasPrefix(line, "QUIT"):
				_ = tp.PrintfLine("221 bye")
				return
			default:
				_ = tp.PrintfLine("250 OK")
			}
		}
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var portNum int
	if _, err := fmt.Sscanf(p, "%d", &portNum); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return h, portNum
}

func getResetPasswordPage(h *Handler, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/reset-password/"+token, nil)
	r.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	h.resetPasswordPage(rec, r)
	return rec
}

// Ветка с годным токеном: форма нового пароля показана, ошибки нет.
func TestResetPasswordPageValidToken(t *testing.T) {
	h := authTestHandler(t)
	if _, err := h.Auth.Register(context.Background(), "resetpage@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, found, err := h.Auth.RequestPasswordReset(context.Background(), "resetpage@example.com")
	if err != nil || !found {
		t.Fatalf("RequestPasswordReset = (%q,%v,%v)", token, found, err)
	}

	rec := getResetPasswordPage(h, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="new"`) {
		t.Errorf("форма нового пароля не показана для годного токена: %s", rec.Body.String())
	}
}

// Ветка с негодным токеном: форма скрыта, показана ошибка.
func TestResetPasswordPageInvalidToken(t *testing.T) {
	h := authTestHandler(t)

	rec := getResetPasswordPage(h, "no-such-token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="new"`) {
		t.Errorf("форма нового пароля показана для негодного токена: %s", body)
	}
	want := i18n.T(context.Background(), "err.auth.reset_invalid")
	if !strings.Contains(body, want) {
		t.Errorf("страница не сообщает о недействительности токена: %s", body)
	}
}

// Соседняя ручка (/forgot-password) лимитирована, эта — нет вовсе: токен непереборен, но
// лимит по IP нужен ради единообразия и чтобы шум атаки не доходил до БД.
func TestResetPasswordSubmitIPRateLimited(t *testing.T) {
	h := authTestHandler(t)

	post := func() int {
		body := "new=aaaaaaaa1&new2=aaaaaaaa1"
		r := httptest.NewRequest(http.MethodPost, "/reset-password/bogus-token", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", h.BaseURL)
		r.SetPathValue("token", "bogus-token")
		r.RemoteAddr = "203.0.113.150:1"
		rec := httptest.NewRecorder()
		h.resetPasswordSubmit(rec, r)
		return rec.Code
	}

	const attempts = 30
	const ipLimit = 20 // см. authTestHandler: passwordResetIPLimiter limit
	var limited int
	for i := 0; i < attempts; i++ {
		if code := post(); code == http.StatusTooManyRequests {
			limited++
		}
	}
	if want := attempts - ipLimit; limited != want {
		t.Errorf("отказов 429 = %d, want %d — passwordResetIPLimiter обязан ограничивать /reset-password по IP", limited, want)
	}
}

// Требование безопасности: токен не должен попадать в журнал ни в каком виде — даже когда
// сама отправка письма падает и путь доставки логирует предупреждение.
func TestDeliverPasswordResetEmailDoesNotLogToken(t *testing.T) {
	h := &Handler{}
	host, port := fakeRejectingMailFrom(t)
	h.Email = notify.NewEmailSender(notify.EmailConfig{Host: host, Port: port, From: "noreply@gotcha.test"})

	const secretToken = "sUpEr-sEcReT-reset-token-12345"
	link := "http://gotcha.example/reset-password/" + secretToken

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h.deliverPasswordResetEmail("victim@example.com", "subject", "body with a link: "+link)

	log := buf.String()
	if !strings.Contains(log, "failed to send reset email") {
		t.Fatalf("ожидалось предупреждение о неудачной отправке, лог: %s", log)
	}
	if strings.Contains(log, secretToken) {
		t.Errorf("токен попал в лог: %s", log)
	}
}

// Личное письмо получателю обязано читать его users.locale, а не локаль инстанса
// по умолчанию — иначе англоязычный сотрудник на GOTCHA_LOCALE=ru получит письмо по-русски.
func TestRecipientLocaleUsesStoredLocaleOverInstanceDefault(t *testing.T) {
	h := authTestHandler(t)
	h.NotifyLocale = i18n.Locale{Code: "ru"}

	uid, err := h.Auth.Register(context.Background(), "loc-explicit@example.com", "password123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := h.Auth.SetLocale(context.Background(), uid, "en"); err != nil {
		t.Fatalf("SetLocale: %v", err)
	}

	got := h.recipientLocale(context.Background(), "loc-explicit@example.com", h.NotifyLocale)
	if got.Code != "en" {
		t.Errorf("recipientLocale = %q, want %q (личная locale получателя)", got.Code, "en")
	}
}

func TestRecipientLocaleFallsBackToInstanceWhenUnset(t *testing.T) {
	h := authTestHandler(t)
	h.NotifyLocale = i18n.Locale{Code: "en"}

	if _, err := h.Auth.Register(context.Background(), "loc-unset@example.com", "password123"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got := h.recipientLocale(context.Background(), "loc-unset@example.com", h.NotifyLocale)
	if got.Code != "en" {
		t.Errorf("recipientLocale = %q, want фолбэк %q — пользователь locale не выбирал", got.Code, "en")
	}
}

func TestRecipientLocaleFallsBackToInstanceForUnknownEmail(t *testing.T) {
	h := authTestHandler(t)
	h.NotifyLocale = i18n.Locale{Code: "ru"}

	got := h.recipientLocale(context.Background(), "loc-nobody@example.com", h.NotifyLocale)
	if got.Code != "ru" {
		t.Errorf("recipientLocale = %q, want фолбэк %q для несуществующего адреса", got.Code, "ru")
	}
}

// Приглашение и итог выгрузки — единственные письма со сроком действия, показанным
// пользователю в интерфейсе (org.invite.col_expires, exports.table.expires): не назвать
// его в письме значит доверить человеку помнить дедлайн, которого он не видел.
func TestInviteEmailPayloadMentionsExpiry(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	payload := inviteEmailPayload(ctx, "Acme", "alice@example.com", "http://gotcha.example/invite/tok")

	body := fmt.Sprint(payload["body"])
	if !strings.Contains(body, "7 дней") {
		t.Errorf("в письме-приглашении нет срока действия ссылки: %q", body)
	}
}

// Свойство, не тайминг: пул на один коннект держим занятым — синхронный обработчик
// повис бы, ожидая освобождения, асинхронный ответит, пока фон ещё ждёт соединение.
func TestForgotPasswordSubmitRespondsWhileBackgroundWorkIsBlockedOnDB(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)

	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()

	h := &Handler{
		BaseURL:                   "http://gotcha.example",
		Auth:                      auth.NewService(pool),
		EmailEnabled:              true,
		passwordResetIPLimiter:    newRateLimiter(time.Now, 20, time.Minute, passwordResetMaxKeys, "passwordResetIPLimiter"),
		passwordResetEmailLimiter: newRateLimiter(time.Now, 5, 15*time.Minute, passwordResetMaxKeys, "passwordResetEmailLimiter"),
	}

	body := "email=" + url.QueryEscape("blocked@example.com")
	r := httptest.NewRequest(http.MethodPost, "/forgot-password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.RemoteAddr = "203.0.113.66:1"
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.forgotPasswordSubmit(rec, r)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("обработчик не ответил, пока единственное соединение пула занято — путь ответа синхронно ждёт БД")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// tag разводит email/DSN двух прогонов; registerUser сеет адрес заранее через отдельный,
// нетрассируемый пул (регистрация — не часть проверяемого пути ответа).
func forgotPasswordResponsePathQuerySequence(t *testing.T, tag string, registerUser bool) []string {
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

	email := "forgot-trace-" + tag + "@example.com"
	if registerUser {
		if _, err := auth.NewService(fullPool).Register(context.Background(), email, "correct-horse-battery"); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	h := &Handler{
		BaseURL:                   "http://gotcha.example",
		Auth:                      auth.NewService(authPool),
		EmailEnabled:              true,
		passwordResetIPLimiter:    newRateLimiter(time.Now, 20, time.Minute, passwordResetMaxKeys, "passwordResetIPLimiter"),
		passwordResetEmailLimiter: newRateLimiter(time.Now, 5, 15*time.Minute, passwordResetMaxKeys, "passwordResetEmailLimiter"),
	}

	body := "email=" + url.QueryEscape(email)
	r := httptest.NewRequest(http.MethodPost, "/forgot-password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", h.BaseURL)
	r.RemoteAddr = "203.0.113.90:1"
	r = r.WithContext(context.WithValue(r.Context(), responsePathMarkerKey{}, true))
	rec := httptest.NewRecorder()

	h.forgotPasswordSubmit(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return tracer.sequence()
}

// Инвариант сильнее, чем у инвайта: путь ответа анонимен, аутентификации там нет вовсе —
// значит обе последовательности обязаны быть ПУСТЫ, не просто одинаковы.
func TestForgotPasswordSubmitResponsePathTouchesNoAuthQueriesRegardlessOfRecipientRegistration(t *testing.T) {
	if existing := forgotPasswordResponsePathQuerySequence(t, "reg", true); len(existing) != 0 {
		t.Errorf("путь ответа для существующего адреса обратился к БД аутентификации: %v", existing)
	}
	if unknown := forgotPasswordResponsePathQuerySequence(t, "unreg", false); len(unknown) != 0 {
		t.Errorf("путь ответа для несуществующего адреса обратился к БД аутентификации: %v", unknown)
	}
}
