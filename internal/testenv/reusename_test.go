package testenv

import "testing"

// Имя переиспользуемого контейнера обязано меняться вместе с версией образа — иначе
// бамп версии молча переиспользует старый контейнер.
func TestReuseNameCarriesImageTag(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"postgres:17-alpine", "gotcha-test-postgres-17-alpine"},
		{"postgres:18-alpine", "gotcha-test-postgres-18-alpine"},
		{"clickhouse/clickhouse-server:25.3-alpine", "gotcha-test-postgres-25-3-alpine"},
		{"postgres", "gotcha-test-postgres-postgres"},
	}
	for _, tc := range cases {
		if got := reuseName("postgres", tc.image); got != tc.want {
			t.Errorf("reuseName(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}

	if reuseName("postgres", "postgres:17-alpine") == reuseName("postgres", "postgres:18-alpine") {
		t.Error("имена совпали для разных версий образа — бамп версии переиспользует старый контейнер")
	}
	if postgresReuseName == clickhouseReuseName {
		t.Error("PostgreSQL и ClickHouse делят имя контейнера")
	}
}
