package db_test

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
)

// Часть операторов пишет DSN в keyword/value-форме, не только URL — сужать формат нельзя.
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

// Ошибка парсинга должна быть отказом на старте, а не первым db.NewPostgres в рантайме.
func TestValidatePostgresDSNRejectsUnparseable(t *testing.T) {
	if err := db.ValidatePostgresDSN("::::"); err == nil {
		t.Error("ValidatePostgresDSN(\"::::\"): want error, got nil")
	}
}

// pgx редактирует пароль в тексте ошибки ParseConfig — потому оборачивание через %w безопасно;
// тест ловит регресс, если апгрейд pgx сменит формат ошибки.
func TestValidatePostgresDSNErrorDoesNotLeakPassword(t *testing.T) {
	err := db.ValidatePostgresDSN("postgres://user:secretpass@host:notaport/db")
	if err == nil {
		t.Fatal("want error for a malformed port, got nil")
	}
	if strings.Contains(err.Error(), "secretpass") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestValidateClickHouseDSNAcceptsValid(t *testing.T) {
	if err := db.ValidateClickHouseDSN("clickhouse://gotcha:gotcha@localhost:9000/gotcha"); err != nil {
		t.Errorf("ValidateClickHouseDSN: want no error, got %v", err)
	}
}

func TestValidateClickHouseDSNRejectsUnparseable(t *testing.T) {
	if err := db.ValidateClickHouseDSN("::::"); err == nil {
		t.Error("ValidateClickHouseDSN(\"::::\"): want error, got nil")
	}
}

// В отличие от pgx, clickhouse-go эхом отдаёт весь DSN (с паролем) в тексте ошибки — поэтому здесь
// нужна обобщённая формулировка, а не обёрнутая сырая ошибка клиента.
func TestValidateClickHouseDSNErrorDoesNotLeakPassword(t *testing.T) {
	err := db.ValidateClickHouseDSN("clickhouse://user:secretpass@host:notaport/db")
	if err == nil {
		t.Fatal("want error for a malformed port, got nil")
	}
	if strings.Contains(err.Error(), "secretpass") {
		t.Errorf("error leaks the password: %v", err)
	}
}
