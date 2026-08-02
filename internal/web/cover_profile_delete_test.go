package web_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebProfileDelete — самоудаление аккаунта (M5): единственный владелец орга
// блокируется (409), обычный участник проходит двухшаговое подтверждение и
// удаляется; после удаления сессия недействительна.
func TestWebProfileDelete(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// owner — единственный владелец организации → удаление запрещено (409).
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "pdel-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "pdel-org", "PDel", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	resp := postForm(t, s.srv, "/profile/delete", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("owner delete: status = %d, want 409 (sole owner)", resp.StatusCode)
	}

	// member — не владелец, может удалиться.
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "pdel-member@example.com")
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Без confirmed — страница подтверждения (200), удаления ещё нет.
	resp = postForm(t, s.srv, "/profile/delete", url.Values{}, s.srv.URL, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member delete (confirm step): status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "/profile/delete") {
		t.Fatalf("confirm page missing delete action form: %s", body)
	}

	// confirmed=yes — удаление, редирект на /login.
	resp = postForm(t, s.srv, "/profile/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("member delete (confirmed): status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}

	// Аккаунт удалён: старая сессия больше не пускает на /profile.
	resp = getWithCookie(t, s.srv, "/profile", memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("deleted user's session still valid on /profile (status 200)")
	}
}

// TestProfileDeletePurgesPendingInvites: адрес читался ПОСЛЕ удаления строки
// пользователя, поэтому возвращался пустым и удаление ожидающих приглашений не
// выполнялось ни разу. Приглашение переживало удаление аккаунта до истечения
// своего срока — при том что комментарий рядом объясняет, зачем ветка нужна для
// минимизации персональных данных.
func TestProfileDeletePurgesPendingInvites(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, _ := orgSettingsRegister(t, authSvc, "inv-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "inv-org", "Inv", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	const victim = "inv-member@example.com"
	memberID, memberCookie := orgSettingsRegister(t, authSvc, victim)
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Приглашение на ТОТ ЖЕ адрес: именно оно должно исчезнуть вместе с аккаунтом.
	if _, err := orgSvc.Invite(ctx, o.ID, victim, org.RoleMember); err != nil {
		t.Fatalf("invite: %v", err)
	}
	// Приглашение на чужой адрес — контроль: его трогать нельзя.
	const bystander = "inv-bystander@example.com"
	if _, err := orgSvc.Invite(ctx, o.ID, bystander, org.RoleMember); err != nil {
		t.Fatalf("invite bystander: %v", err)
	}

	resp := postForm(t, s.srv, "/profile/delete",
		url.Values{"confirmed": {"yes"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var left int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM org_invites WHERE email = $1", victim).Scan(&left); err != nil {
		t.Fatalf("count invites: %v", err)
	}
	if left != 0 {
		t.Fatalf("после удаления аккаунта осталось %d приглашение(й) на его адрес: "+
			"ветка очистки не выполнилась", left)
	}

	var others int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM org_invites WHERE email = $1", bystander).Scan(&others); err != nil {
		t.Fatalf("count bystander invites: %v", err)
	}
	if others != 1 {
		t.Fatalf("приглашение на чужой адрес = %d, want 1: очистка задела лишнее", others)
	}
}

// TestProfileDeleteLogsWhenEmailReadFails (раунд правок 1): пустой email в
// этой точке — НЕ «пользователя нет» (личность уже проверена auth.UserID
// выше, строка в users на момент чтения была на месте), а сбой самого
// чтения. currentEmail превращает в "" любую ошибку UserEmail — не только
// «юзера нет», но и обрыв БД, битую схему и т.п. (см. её докблок в web.go).
// Без явного лога такой сбой неотличим от «email не нашли, значит нечего
// чистить»: аккаунт удалён, приглашение осталось, в логе ни строчки — ровно
// тот класс тишины, ради которого существует весь подпроект.
//
// Ломаем именно чтение email, не удаляя пользователя: переименовываем
// колонку users.email прямо в тестовой БД. testenv.MigratedPG(t) выдаёт
// каждому тесту свою изолированную базу (видно по именам t_<hash> в логах
// падений при перегрузке инфраструктуры), поэтому ALTER TABLE здесь не
// аукается ни в соседних тестах, ни при параллельном запуске пакета.
// SoleOwnedOrgNames, DestroySession и DeleteUser колонку email не трогают
// (проверено по их SQL) — обломанная колонка бьёт ровно по currentEmail.
func TestProfileDeleteLogsWhenEmailReadFails(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	ctx := context.Background()

	uid, cookie := orgSettingsRegister(t, authSvc, "read-fail@example.com")

	if _, err := s.pool.Exec(ctx,
		"ALTER TABLE users RENAME COLUMN email TO email_broken_for_test"); err != nil {
		t.Fatalf("break email column: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	resp := postForm(t, s.srv, "/profile/delete",
		url.Values{"confirmed": {"yes"}}, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if buf.Len() == 0 {
		t.Fatal("сбой чтения email при удалении аккаунта прошёл молча: " +
			"ни строчки в логе, хотя pending-инвайты (если были) не вычищены")
	}
	if !strings.Contains(buf.String(), fmt.Sprint(uid)) {
		t.Fatalf("в записи лога нет user_id пользователя, чей email не прочитался: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "email") {
		t.Fatalf("запись в логе не объясняет, что именно не удалось: %s", buf.String())
	}
}
