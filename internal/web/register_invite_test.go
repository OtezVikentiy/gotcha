package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// registerBootstrap заводит первого пользователя инстанса (bootstrap
// инстанс-админа проходит при любом режиме) и его организацию — дальше режим
// регистрации уже действует.
func registerBootstrap(t *testing.T, s *stack, prefix string) (*org.Service, org.Org) {
	t.Helper()
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, _ := orgSettingsRegister(t, authSvc, prefix+"-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), prefix+"-co", "Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return orgSvc, o
}

func postRegister(t *testing.T, s *stack, email string) *http.Response {
	t.Helper()
	return postForm(t, s.srv, "/register", url.Values{
		"email": {email}, "password": {"correct-horse-battery"}, "password2": {"correct-horse-battery"},
	}, s.srv.URL, nil)
}

// TestRegisterInviteModeAllowsInvitedEmail — режим invite (он же по умолчанию):
// приглашённый заводит аккаунт паролем.
//
// Существует потому, что проверка приглашения жила только в OAuth-ветке, а
// парольная регистрация при любом не-open режиме отдавала 403. На типовой
// self-hosted-инсталляции без OAuth-провайдера ссылка-приглашение не работала
// вовсе: человек шёл по ней и упирался в «регистрация закрыта».
func TestRegisterInviteModeAllowsInvitedEmail(t *testing.T) {
	s := newStack(t)
	s.h.RegistrationMode = "invite"
	orgSvc, o := registerBootstrap(t, s, "reginvite")

	const invited = "invited@example.com"
	if _, err := orgSvc.Invite(context.Background(), o.ID, invited, org.RoleMember); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	resp := postRegister(t, s, invited)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invited register status = %d, want 303: %s", resp.StatusCode, body)
	}
}

// TestRegisterInviteModeRejectsUninvited — тот же режим, адрес без приглашения:
// самостоятельная регистрация по-прежнему закрыта.
func TestRegisterInviteModeRejectsUninvited(t *testing.T) {
	s := newStack(t)
	s.h.RegistrationMode = "invite"
	registerBootstrap(t, s, "reguninvited")

	resp := postRegister(t, s, "stranger@example.com")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("uninvited register status = %d, want 403: %s", resp.StatusCode, body)
	}
}

// TestRegisterClosedModeRejectsEvenInvited — closed отличается от invite ровно
// этим: новых аккаунтов не появляется даже по действующему приглашению.
func TestRegisterClosedModeRejectsEvenInvited(t *testing.T) {
	s := newStack(t)
	s.h.RegistrationMode = "closed"
	orgSvc, o := registerBootstrap(t, s, "regclosed")

	const invited = "invited-closed@example.com"
	if _, err := orgSvc.Invite(context.Background(), o.ID, invited, org.RoleMember); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	resp := postRegister(t, s, invited)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("closed-mode register status = %d, want 403", resp.StatusCode)
	}
}

// TestRegisterFormVisibleInInviteMode — форму в режиме invite надо показывать:
// приглашённому больше некуда ввести свой адрес. В closed — заглушка.
func TestRegisterFormVisibleInInviteMode(t *testing.T) {
	s := newStack(t)
	registerBootstrap(t, s, "regform")

	s.h.RegistrationMode = "invite"
	resp, err := http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `name="password2"`) {
		t.Fatalf("invite mode must still render the form:\n%s", body)
	}

	s.h.RegistrationMode = "closed"
	resp, err = http.Get(s.srv.URL + "/register")
	if err != nil {
		t.Fatalf("GET /register: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `name="password2"`) {
		t.Fatalf("closed mode must not render the form:\n%s", body)
	}
}
