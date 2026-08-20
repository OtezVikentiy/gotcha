package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestWebMaintenanceUpdate — правка окна через форму: тот же набор полей, что
// и у создания, плюс window_id. Проверяем и содержимое страницы после правки:
// ссылка «Изменить» и предзаполненная модалка — это и есть весь смысл правки.
func TestWebMaintenanceUpdate(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintupd")

	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: proj.ID, Name: "DB upgrade", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	form := url.Values{
		"window_id": {strconv.FormatInt(w.ID, 10)},
		"name":      {"DB upgrade (продлено)"},
		"kind":      {"oneoff"},
		"starts_at": {"2026-08-01T02:00"},
		"ends_at":   {"2026-08-01T06:00"},
		"timezone":  {"UTC"},
	}
	resp := postForm(t, s.srv, path+"/update", form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST update status = %d, want 303: %s", resp.StatusCode, body)
	}

	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil || len(windows) != 1 {
		t.Fatalf("Windows = %+v err=%v, want one", windows, err)
	}
	got := windows[0]
	if got.Name != "DB upgrade (продлено)" {
		t.Fatalf("name = %q, want renamed", got.Name)
	}
	if got.EndsAt == nil || !got.EndsAt.Equal(time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("EndsAt = %v, want 06:00 UTC", got.EndsAt)
	}

	// Страница отдаёт модалку правки с уже подставленными значениями окна.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	anchor := "edit-window-" + strconv.FormatInt(w.ID, 10)
	if !strings.Contains(string(page), anchor) {
		t.Fatalf("page has no edit modal anchor %q", anchor)
	}
	if !strings.Contains(string(page), `value="2026-08-01T06:00"`) {
		t.Fatalf("edit modal not prefilled with the window's end time:\n%s", page)
	}
}

// TestWebMaintenanceEditIndefiniteWindowChecksBox — окно, созданное без
// ends_at (бессрочное), открывает форму правки с уже отмеченным чекбоксом
// «indefinite» — иначе сохранение формы без изменений тут же упёрлось бы в
// end_required (windowFieldDefaults должен предзаполнить чекбокс).
func TestWebMaintenanceEditIndefiniteWindowChecksBox(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintedgeindef")

	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	w, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: proj.ID, Name: "Ongoing freeze", StartsAt: &start, EndsAt: nil, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	page := string(body)
	anchor := "edit-window-" + strconv.FormatInt(w.ID, 10)
	if !strings.Contains(page, anchor) {
		t.Fatalf("page has no edit modal anchor %q:\n%s", anchor, page)
	}
	if !strings.Contains(page, `name="indefinite" checked`) {
		t.Fatalf("edit modal for indefinite window %d does not pre-check the indefinite box:\n%s", w.ID, page)
	}
}

// TestWebMaintenanceUpdateInvalidReopensModal — на 422 страница возвращается с
// открытой модалкой ИМЕННО этого окна и с введёнными значениями. Раньше
// признаком «открыть модалку» служил сам факт непустого состояния формы, и с
// появлением модалки на каждую строку ошибка правки открыла бы форму создания.
func TestWebMaintenanceUpdateInvalidReopensModal(t *testing.T) {
	s := newMaintenanceStack(t)
	proj, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintupdinv")

	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: proj.ID, Name: "DB upgrade", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/maintenance/update"
	form := url.Values{
		"window_id": {strconv.FormatInt(w.ID, 10)},
		"name":      {"Задом наперёд"},
		"kind":      {"oneoff"},
		"starts_at": {"2026-08-01T06:00"},
		"ends_at":   {"2026-08-01T02:00"}, // конец раньше начала
		"timezone":  {"UTC"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	page := string(body)

	anchor := "edit-window-" + strconv.FormatInt(w.ID, 10)
	if !strings.Contains(page, `id="`+anchor+`" class="modal modal--open"`) {
		t.Fatalf("edit modal for window %d not reopened:\n%s", w.ID, page)
	}
	if strings.Contains(page, `id="new-maintenance-window" class="modal modal--open"`) {
		t.Fatalf("create modal opened instead of the edit one:\n%s", page)
	}
	if !strings.Contains(page, `value="Задом наперёд"`) {
		t.Fatalf("entered name not returned to the form:\n%s", page)
	}
	// Само окно осталось прежним.
	windows, err := s.uptime.Windows(context.Background(), proj.ID)
	if err != nil || len(windows) != 1 || windows[0].Name != "DB upgrade" {
		t.Fatalf("window after failed update = %+v err=%v, want untouched", windows, err)
	}
}

// TestWebMaintenanceUpdateForeignWindow — окно чужого проекта не правится по
// подобранному id: 404, как и у удаления.
func TestWebMaintenanceUpdateForeignWindow(t *testing.T) {
	s := newMaintenanceStack(t)
	mine, ownerCookie, _ := maintenanceOwnerAndMember(t, s, "maintupdmine")
	theirs, _, _ := maintenanceOwnerAndMember(t, s, "maintupdtheirs")

	start := time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	w, err := s.uptime.CreateWindow(context.Background(), uptime.Window{
		ProjectID: theirs.ID, Name: "Theirs", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(mine.ID, 10) + "/maintenance/update"
	form := url.Values{
		"window_id": {strconv.FormatInt(w.ID, 10)},
		"name":      {"Hijacked"},
		"kind":      {"oneoff"},
		"starts_at": {"2026-08-01T02:00"},
		"ends_at":   {"2026-08-01T04:00"},
		"timezone":  {"UTC"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST foreign window status = %d, want 404", resp.StatusCode)
	}
	windows, err := s.uptime.Windows(context.Background(), theirs.ID)
	if err != nil || len(windows) != 1 || windows[0].Name != "Theirs" {
		t.Fatalf("victim window = %+v err=%v, want untouched", windows, err)
	}
}
