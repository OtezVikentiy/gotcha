package notify_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

func TestWebhookSenderSignsAndPosts(t *testing.T) {
	var gotBody []byte
	var gotSig, gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get("X-Gotcha-Signature")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &notify.WebhookSender{Client: srv.Client()}
	target := notify.Target{Kind: "webhook", Target: srv.URL, Secret: "s3cr3t"}
	payload := map[string]any{"title": "boom", "issue_id": float64(42)}

	if err := sender.Send(context.Background(), target, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}

	mac := hmac.New(sha256.New, []byte(target.Secret))
	mac.Write(gotBody)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != wantSig {
		t.Errorf("signature = %q, want %q", gotSig, wantSig)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if decoded["title"] != "boom" {
		t.Errorf("body title = %v, want boom", decoded["title"])
	}
}

// TestWebhookSenderDoesNotLeakTransportFields guards against the outbound
// body echoing channel_kind/target/secret: alert.Evaluator stuffs those
// transport fields into the outbox payload so the worker can rebuild a
// notify.Target (see worker.go's process), but WebhookSender must never
// forward them in the POST body — "secret" in particular is the very key
// used to sign the request, so leaking it lets anyone reading the
// receiver's logs forge signed alerts.
func TestWebhookSenderDoesNotLeakTransportFields(t *testing.T) {
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Gotcha-Signature")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const secret = "SIGNING-KEY"
	sender := &notify.WebhookSender{Client: srv.Client()}
	target := notify.Target{Kind: "webhook", Target: srv.URL, Secret: secret}
	payload := map[string]any{
		"channel_kind": "webhook",
		"target":       srv.URL,
		"secret":       secret,
		"title":        "boom",
		"issue_id":     float64(42),
	}

	if err := sender.Send(context.Background(), target, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	for _, leaked := range []string{"secret", "target", "channel_kind"} {
		if _, ok := decoded[leaked]; ok {
			t.Errorf("outbound body contains transport field %q: %s", leaked, gotBody)
		}
	}
	if decoded["title"] != "boom" {
		t.Errorf("body title = %v, want boom", decoded["title"])
	}
	if decoded["issue_id"] != float64(42) {
		t.Errorf("body issue_id = %v, want 42", decoded["issue_id"])
	}

	// The signature must verify against the bytes actually received (i.e.
	// signing happens on the filtered body, not the original payload).
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != wantSig {
		t.Errorf("signature = %q, want %q (computed over received bytes)", gotSig, wantSig)
	}
}

func TestWebhookSenderNoSecretNoSignature(t *testing.T) {
	var gotSig []string
	var sawSig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig, sawSig = r.Header["X-Gotcha-Signature"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &notify.WebhookSender{Client: srv.Client()}
	target := notify.Target{Kind: "webhook", Target: srv.URL}
	if err := sender.Send(context.Background(), target, map[string]any{"a": "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawSig {
		t.Errorf("X-Gotcha-Signature present with no secret: %q", gotSig)
	}
}

func TestWebhookSenderNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sender := &notify.WebhookSender{Client: srv.Client()}
	target := notify.Target{Kind: "webhook", Target: srv.URL}
	if err := sender.Send(context.Background(), target, map[string]any{}); err == nil {
		t.Fatal("Send: want error on 500, got nil")
	}
}

func TestTelegramSenderPostsMessage(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &notify.TelegramSender{Client: srv.Client(), BaseURL: srv.URL}
	target := notify.Target{Kind: "telegram", Target: "12345", Secret: "bot-token"}
	payload := map[string]any{"body": "hello there"}

	if err := sender.Send(context.Background(), target, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.HasPrefix(gotPath, "/bot") || !strings.HasSuffix(gotPath, "/sendMessage") {
		t.Errorf("path = %q, want /bot<secret>/sendMessage", gotPath)
	}
	if !strings.Contains(gotPath, target.Secret) {
		t.Errorf("path = %q, want to contain secret %q", gotPath, target.Secret)
	}
	if gotBody["chat_id"] != "12345" {
		t.Errorf("chat_id = %v, want 12345", gotBody["chat_id"])
	}
	if gotBody["text"] != "hello there" {
		t.Errorf("text = %v, want %q", gotBody["text"], "hello there")
	}
	if gotBody["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview = %v, want true", gotBody["disable_web_page_preview"])
	}
}

func TestTelegramSenderNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	sender := &notify.TelegramSender{Client: srv.Client(), BaseURL: srv.URL}
	target := notify.Target{Kind: "telegram", Target: "1", Secret: "tok"}
	if err := sender.Send(context.Background(), target, map[string]any{"body": "x"}); err == nil {
		t.Fatal("Send: want error on 400, got nil")
	}
}

// TestEmailSenderHonoursContextDeadline guards against EmailSender.Send
// hanging forever against a blackholed SMTP server: net/smtp.SendMail (the
// pre-fix implementation) sets no deadlines of its own, so a peer that
// accepts the TCP connection and then never speaks would stall the
// sequential Worker.tick loop indefinitely — no alerts would be delivered
// on ANY channel, not just the broken one. With a ctx carrying a short
// deadline, Send must return (with an error) close to that deadline
// instead of hanging.
func TestEmailSenderHonoursContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Accept the connection and then say nothing, ever - simulates a
		// blackholed SMTP server (no banner, no response to anything).
		<-context.Background().Done()
		_ = conn
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	s := notify.NewEmailSender(notify.EmailConfig{Host: host, Port: port, From: "alerts@gotcha.dev"})
	target := notify.Target{Kind: "email", Target: "ops@example.com"}
	payload := map[string]any{"subject": "boom", "body": "boom happened"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.Send(ctx, target, payload) }()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted connection")
	}

	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("Send: want error against blackholed server, got nil")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Send took %v, want close to the 1s ctx deadline", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not respect ctx deadline: still hanging after 5s")
	}
}

func TestEmailSenderConfigured(t *testing.T) {
	s := notify.NewEmailSender(notify.EmailConfig{})
	if s.Configured() {
		t.Error("Configured() = true for empty host, want false")
	}
	s2 := notify.NewEmailSender(notify.EmailConfig{Host: "smtp.example.com"})
	if !s2.Configured() {
		t.Error("Configured() = false for non-empty host, want true")
	}
}

func TestBuildEmailContainsHeadersAndBody(t *testing.T) {
	msg := notify.BuildEmail("alerts@gotcha.dev", "ops@example.com", "New issue: boom", "Issue boom occurred.\nSee details.")
	s := string(msg)

	if !strings.Contains(s, "From: alerts@gotcha.dev") {
		t.Errorf("missing From header: %s", s)
	}
	if !strings.Contains(s, "To: ops@example.com") {
		t.Errorf("missing To header: %s", s)
	}
	if !strings.Contains(s, "Subject: New issue: boom") {
		t.Errorf("missing Subject header: %s", s)
	}
	if !strings.Contains(s, "Content-Type: text/plain") {
		t.Errorf("missing Content-Type header: %s", s)
	}
	if !strings.Contains(s, "Issue boom occurred.") {
		t.Errorf("missing body: %s", s)
	}
	// headers must be separated from the body by a blank line (CRLF CRLF).
	if !strings.Contains(s, "\r\n\r\n") {
		t.Errorf("missing header/body separator: %q", s)
	}
}

// TestTelegramSenderTransportErrorDoesNotLeakToken guards against the bot
// token leaking through *url.Error, which embeds the full request URL
// (including {secret} from the /bot{secret}/sendMessage path) in its
// Error() string. A transport-level failure (here: connection refused,
// standing in for ctx-cancel-during-shutdown / DNS / TLS / timeout) must
// not surface the token to callers who log or persist Send's error.
func TestTelegramSenderTransportErrorDoesNotLeakToken(t *testing.T) {
	// Bind and immediately close a listener: the port is guaranteed free of
	// any listener, so a connection attempt is refused at the transport
	// level (never reaches HTTP), reliably reproducing *url.Error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const secret = "SECRET-TOKEN-123"
	sender := &notify.TelegramSender{BaseURL: "http://" + addr}
	target := notify.Target{Kind: "telegram", Target: "12345", Secret: secret}

	err = sender.Send(context.Background(), target, map[string]any{"body": "hi"})
	if err == nil {
		t.Fatal("Send: want error against closed port, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Send error leaks bot token: %q", err.Error())
	}
}

// TestBuildEmailRejectsHeaderInjection guards against CRLF injection via a
// user-controlled subject (derived from issue titles). Without
// sanitization, "\r\n" in the subject terminates the Subject header early
// and lets an attacker inject arbitrary headers (e.g. Bcc) or spoof the
// body.
func TestBuildEmailRejectsHeaderInjection(t *testing.T) {
	msg := notify.BuildEmail("alerts@gotcha.dev", "ops@example.com", "Hi\r\nBcc: evil@example.com", "body")
	s := string(msg)

	// "Bcc:" must not start its own header line (i.e. be reachable by
	// injecting a line break) — the literal text is allowed to survive as
	// inert subject content, it just must not function as a header.
	if strings.Contains(s, "\r\nBcc:") || strings.HasPrefix(s, "Bcc:") {
		t.Errorf("BuildEmail allowed header injection: %q", s)
	}
	// The Subject header itself must be confined to a single line: find it
	// and check there's no embedded CR/LF before the following \r\n.
	idx := strings.Index(s, "Subject: ")
	if idx == -1 {
		t.Fatalf("missing Subject header: %q", s)
	}
	rest := s[idx+len("Subject: "):]
	end := strings.Index(rest, "\r\n")
	if end == -1 {
		t.Fatalf("Subject header not terminated: %q", s)
	}
	line := rest[:end]
	if strings.ContainsAny(line, "\r\n") {
		t.Errorf("Subject header line contains embedded CR/LF: %q", line)
	}
}
