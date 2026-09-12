package web_test

import (
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestTopbarKeepsAllProjectsDoorWithSingleProject(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "single@example.com")
	p := createProject(t, s, uid, "solo-org", "solo-proj")

	resp := getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(p.ID, 10)+"/issues", cookie)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/orgs/"+strconv.FormatInt(p.OrgID, 10)+"/projects") {
		t.Fatal("при единственном проекте пропала ссылка «Все проекты» — создать второй проект стало неоткуда")
	}
}
