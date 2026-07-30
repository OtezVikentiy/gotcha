// Package memlimit приводит потолок памяти Go-рантайма в соответствие с
// лимитом контейнера.
//
// Существует потому, что Go-рантайм лимит cgroup не читает: сборщик мусора
// ориентируется на память ХОСТА и спокойно перерастает лимит контейнера. Для
// gotcha это не абстрактная неаккуратность — при недоступном ClickHouse буферы
// растут по замыслу, дожидаясь возвращения хранилища, и без потолка первым
// срабатывает не аккуратный сброс избытка по maxBuf, а OOM-killer ядра: тогда
// теряются все пять буферов целиком (события, транзакции, спаны, метрики,
// профили).
//
// Раньше защита существовала только как GOMEMLIMIT в small-оверлее compose, то
// есть работала на худшем сервере и не работала на рекомендованном, в
// Kubernetes и в systemd-слайсе.
package memlimit

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// defaultRatio — доля лимита контейнера, отдаваемая куче.
//
// 0.8 взята из small-оверлея, где она объяснена и замерена: остаток — запас на
// стеки горутин, аллокации вне кучи и сам рантайм. Потолок кучи, равный лимиту
// контейнера, не защищает ни от чего: превысить его означает быть убитым.
const defaultRatio = 0.8

// cgroup v2 и v1 — файлы, в которых лежит лимит памяти.
const (
	cgroupV2Path = "/sys/fs/cgroup/memory.max"
	cgroupV1Path = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// ErrNoLimit — лимит не задан: процесс не в контейнере, либо контейнер запущен
// без ограничения памяти. Не ошибка сама по себе.
var ErrNoLimit = errors.New("memlimit: no container memory limit")

// Apply выставляет потолок кучи в доле от лимита контейнера и возвращает
// установленное значение.
//
// Не вмешивается, когда GOMEMLIMIT задан явно: оператор, написавший значение
// руками, имел в виду именно его. Возвращает ErrNoLimit, когда лимита нет —
// вызывающий решает сам, писать ли об этом в лог.
func Apply() (int64, error) {
	env, envSet := os.LookupEnv("GOMEMLIMIT")
	limit, limitErr := containerLimit()
	target, apply, err := decide(env, envSet, limit, limitErr)
	if err != nil {
		return 0, err
	}
	if !apply {
		// Значение из окружения рантайм применил ещё при старте — сообщаем его.
		return debug.SetMemoryLimit(-1), nil
	}
	debug.SetMemoryLimit(target)
	return target, nil
}

// decide — что делать с потолком кучи: чистая функция, чтобы решение
// проверялось без рантайма и без cgroup. Возвращает (потолок, ставить ли его,
// ошибку).
//
// Явно заданный GOMEMLIMIT не переопределяется: оператор, написавший значение
// руками, имел в виду именно его — так продолжает работать GOMEMLIMIT из
// small-оверлея compose.
func decide(env string, envSet bool, limit int64, limitErr error) (target int64, apply bool, err error) {
	if envSet && strings.TrimSpace(env) != "" {
		return 0, false, nil
	}
	if limitErr != nil {
		return 0, false, limitErr
	}
	target = heapTarget(limit)
	if target <= 0 {
		return 0, false, fmt.Errorf("memlimit: container limit %d too small to derive a heap ceiling", limit)
	}
	return target, true, nil
}

// heapTarget — потолок кучи для заданного лимита контейнера.
func heapTarget(limit int64) int64 {
	return int64(float64(limit) * defaultRatio)
}

// containerLimit читает лимит памяти cgroup — сперва v2, затем v1.
func containerLimit() (int64, error) {
	for _, path := range []string{cgroupV2Path, cgroupV1Path} {
		limit, err := readLimitFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return limit, nil
	}
	return 0, ErrNoLimit
}

// readLimitFile разбирает файл лимита. «max» (v2) и заведомо огромное число
// (v1 пишет туда ~2^63-1, округлённое до размера страницы) означают
// «ограничения нет».
func readLimitFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseLimit(string(data))
}

// unlimitedThreshold — выше этого значения лимит считается отсутствующим.
//
// cgroup v1 записывает «без ограничения» как максимальное число, помещающееся в
// счётчик страниц, — на разных ядрах это разные значения около 2^63. Порог в
// 1 ПиБ отделяет их от любого реального лимита, не привязываясь к конкретной
// константе ядра.
const unlimitedThreshold int64 = 1 << 50

func parseLimit(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "max" {
		return 0, ErrNoLimit
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memlimit: parse %q: %w", s, err)
	}
	if v <= 0 || v >= unlimitedThreshold {
		return 0, ErrNoLimit
	}
	return v, nil
}
