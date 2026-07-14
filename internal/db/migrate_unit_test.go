package db

import "testing"

func TestPgx5URL(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "postgres scheme",
			dsn:  "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
			want: "pgx5://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
		},
		{
			name: "postgresql scheme",
			dsn:  "postgresql://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
			want: "pgx5://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
		},
		{
			name: "non-postgres string passed through unchanged",
			dsn:  "clickhouse://localhost:9000/gotcha",
			want: "clickhouse://localhost:9000/gotcha",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgx5URL(tc.dsn); got != tc.want {
				t.Errorf("pgx5URL(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestNeedsRetention(t *testing.T) {
	ddl := "CREATE TABLE events (...) TTL toDateTime(timestamp) + toIntervalDay(90)"
	if needsRetention(ddl, 90) {
		t.Error("same TTL: want no change")
	}
	if !needsRetention(ddl, 180) {
		t.Error("different TTL: want change")
	}
}
