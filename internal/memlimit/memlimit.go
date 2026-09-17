package memlimit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

// Остаток — запас на стеки горутин, аллокации вне кучи и рантайм. 0.8 замерено на боевом VPS 2 ядра/2 ГБ
// при подготовке стеснённого профиля docker-compose.small.yml.
const defaultRatio = 0.8

// Переменные, а не константы: тесты подставляют временный каталог вместо корня cgroup.
var (
	cgroupRoot         = "/sys/fs/cgroup"
	procSelfCgroupPath = "/proc/self/cgroup"
	cgroupV1Path       = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// Лимит не задан: процесс не в контейнере либо контейнер без ограничения памяти. Не ошибка сама по себе.
var ErrNoLimit = errors.New("memlimit: no container memory limit")

// Не вмешивается, когда GOMEMLIMIT задан явно — оператор, написавший значение руками, имел в виду именно
// его. Возвращает ErrNoLimit, когда лимита нет — решение логировать за вызывающим.
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

// Чистая функция — решение проверяется без рантайма и без cgroup. Явно заданный GOMEMLIMIT не
// переопределяется: оператор, написавший значение руками, имел в виду именно его.
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

func heapTarget(limit int64) int64 {
	return int64(float64(limit) * defaultRatio)
}

// Сперва собственный cgroup процесса (systemd кладёт юнит не в корень дерева, а в свой слайс),
// затем корень v2 (докер монтирует лимит контейнера прямо туда), затем v1.
func containerLimit() (int64, error) {
	for _, path := range candidateLimitPaths() {
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

func candidateLimitPaths() []string {
	paths := make([]string, 0, 3)
	if raw, err := os.ReadFile(procSelfCgroupPath); err == nil {
		if rel, ok := selfCgroupPath(string(raw)); ok && rel != "/" {
			paths = append(paths, filepath.Join(cgroupRoot, rel, "memory.max"))
		}
	}
	return append(paths, filepath.Join(cgroupRoot, "memory.max"), cgroupV1Path)
}

// Разбирает /proc/self/cgroup. Распознаёт только unified-иерархию v2 (строка "0::<путь>");
// v1-строки вида "11:memory:/docker/abc" здесь не нужны — лимит v1 читается по фиксированному пути.
func selfCgroupPath(raw string) (string, bool) {
	for _, line := range strings.Split(raw, "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok || rel == "" {
			continue
		}
		return rel, true
	}
	return "", false
}

// "max" (v2) и заведомо огромное число (v1 пишет ~2^63-1, округлённое до страницы) означают «ограничения нет».
func readLimitFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseLimit(string(data))
}

// cgroup v1 пишет «без ограничения» как максимальное число, помещающееся в счётчик страниц — на разных
// ядрах разные значения около 2^63. Порог 1 ПиБ отделяет их от любого реального лимита.
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
