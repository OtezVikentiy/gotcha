package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// TestReaperIsOff: автоматический сборщик контейнеров обязан быть отключён.
//
// Он владеет контейнерами посессионно и сносит их через десять секунд после
// того, как от сессии отключился последний процесс. Наши общие PostgreSQL и
// ClickHouse переживают и паузу между пакетами, и сам прогон, поэтому владение
// сессии для них неверно: со сборщиком контейнер исчезал посреди работы —
// пакет падал на «container is marked for removal» или на отказе в соединении.
//
// Проверяем не факт вызова Setenv, а то, что testcontainers ВИДИТ настройку:
// имя переменной у него своё, и опечатка в нём тихо вернула бы сборщик.
func TestReaperIsOff(t *testing.T) {
	if !testcontainers.ReadConfig().Config.RyukDisabled {
		t.Fatal("сборщик контейнеров включён — он снесёт общие контейнеры посреди прогона")
	}
}

// TestCleanupTargetFindsContainers: раз уборка теперь только явная, префикс
// имён — контракт между этим пакетом и `make test-env-down`. Проверяем обе его
// стороны: что имена контейнеров начинаются с префикса и что цель Makefile
// фильтрует ровно по нему. Разъедется одна из сторон — уборка молча перестанет
// находить контейнеры, а узнается это по забитой памяти через неделю.
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
