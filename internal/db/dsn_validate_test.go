package db_test

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
)

// TestValidatePostgresDSNAcceptsBothForms — pgxpool.ParseConfig (парсер, что
// реально потребляет db.NewPostgres) принимает и URL-форму DSN, и
// keyword/value-форму — ValidatePostgresDSN не должен сужать это до одной из
// двух: часть операторов пишет DSN как host=... user=... dbname=..., это
// такой же законный DSN для pgx, как postgres://...
func TestValidatePostgresDSNAcceptsBothForms(t *testing.T) {
	for _, dsn := range []string{
		"postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable",
		"host=localhost port=5432 user=gotcha password=gotcha dbname=gotcha sslmode=disable",
	} {
		if err := db.ValidatePostgresDSN(dsn); err != nil {
			t.Errorf("ValidatePostgresDSN(%q): want no error, got %v", dsn, err)
		}
	}
}

// TestValidatePostgresDSNRejectsUnparseable — то, что pgxpool.ParseConfig не
// разберёт ни в одной из форм, обязано быть отказом на старте, а не первым
// db.NewPostgres в рантайме.
func TestValidatePostgresDSNRejectsUnparseable(t *testing.T) {
	if err := db.ValidatePostgresDSN("::::"); err == nil {
		t.Error("ValidatePostgresDSN(\"::::\"): want error, got nil")
	}
}

// TestValidatePostgresDSNErrorDoesNotLeakPassword — pgx редактирует пароль в
// тексте ошибки ParseConfig ("xxxxxx" вместо значения), поэтому оборачивание
// через %w безопасно; тест ловит регресс, если это когда-нибудь перестанет
// быть так (например, апгрейд pgx сменит формат ошибки).
func TestValidatePostgresDSNErrorDoesNotLeakPassword(t *testing.T) {
	err := db.ValidatePostgresDSN("postgres://user:secretpass@host:notaport/db")
	if err == nil {
		t.Fatal("want error for a malformed port, got nil")
	}
	if strings.Contains(err.Error(), "secretpass") {
		t.Errorf("error leaks the password: %v", err)
	}
}

// TestValidateClickHouseDSNAcceptsValid — тот же парсер (clickhouse.ParseDSN),
// что db.NewClickHouse вызывает первым шагом.
func TestValidateClickHouseDSNAcceptsValid(t *testing.T) {
	if err := db.ValidateClickHouseDSN("clickhouse://gotcha:gotcha@localhost:9000/gotcha"); err != nil {
		t.Errorf("ValidateClickHouseDSN: want no error, got %v", err)
	}
}

// TestValidateClickHouseDSNRejectsUnparseable — как у Postgres: отказ на
// старте, а не таймаут/ошибка на первом db.NewClickHouse.
func TestValidateClickHouseDSNRejectsUnparseable(t *testing.T) {
	if err := db.ValidateClickHouseDSN("::::"); err == nil {
		t.Error("ValidateClickHouseDSN(\"::::\"): want error, got nil")
	}
}

// TestValidateClickHouseDSNErrorDoesNotLeakPassword — в отличие от pgx,
// clickhouse-go's ParseDSN эхом отдаёт весь DSN (с паролем) в тексте ошибки
// на некоторых кривых значениях (проверено — невалидный порт); поэтому
// ValidateClickHouseDSN обязан отдавать обобщённую формулировку, а не
// оборачивать сырую ошибку клиента, — та же защита, что db.NewClickHouse уже
// применяет.
func TestValidateClickHouseDSNErrorDoesNotLeakPassword(t *testing.T) {
	err := db.ValidateClickHouseDSN("clickhouse://user:secretpass@host:notaport/db")
	if err == nil {
		t.Fatal("want error for a malformed port, got nil")
	}
	if strings.Contains(err.Error(), "secretpass") {
		t.Errorf("error leaks the password: %v", err)
	}
}
