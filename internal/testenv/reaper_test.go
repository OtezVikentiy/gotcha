package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// Сборщик Ryuk сносит контейнеры через 10с после отключения последней сессии — общие
// PostgreSQL/ClickHouse переживают паузы между пакетами, поэтому обязан быть отключён.
func TestReaperIsOff(t *testing.T) {
	if !testcontainers.ReadConfig().Config.RyukDisabled {
		t.Fatal("сборщик контейнеров включён — он снесёт общие контейнеры посреди прогона")
	}
}

// Префикс имён — контракт между этим пакетом и `make test-env-down`; проверяет обе стороны.
func TestCleanupTargetFindsContainers(t *testing.T) {
	for _, name := range []string{postgresReuseName, clickhouseReuseName} {
		if !strings.HasPrefix(name, reuseNamePrefix) {
			t.Errorf("имя %q не начинается с %q", name, reuseNamePrefix)
		}
	}

	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("прочитать Makefile: %v", err)
	}
	filter := "name=^" + reuseNamePrefix
	if !strings.Contains(string(makefile), filter) {
		t.Errorf("в Makefile нет фильтра %q — `make test-env-down` не найдёт тестовые контейнеры", filter)
	}
}
