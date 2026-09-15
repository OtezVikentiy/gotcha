package db_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Полный список непроверочных файлов под internal/ и cmd/, где есть вызов PrepareBatch — не членство,
// а равенство: новый писатель, не попавший в список, обязан провалить тест, а не проехать мимо.
var batchContextExpectedFiles = []string{
	"internal/event/batcher.go",
	"internal/log/writer.go",
	"internal/metric/writer.go",
	"internal/profile/writer.go",
	"internal/trace/writer.go",
	"internal/uptime/results.go",
}

// Селектор перед именем отличает вызов (w.conn.PrepareBatch() ) от объявления метода интерфейса
// (PrepareBatch(ctx ...) без точки) — CHConn-интерфейсы в тех же файлах не должны давать ложных срабатываний.
var prepareBatchCallRe = regexp.MustCompile(`\.PrepareBatch\(`)

var forbiddenBatchContextCtors = []string{
	"context.WithTimeout(",
	"context.WithCancel(",
	"context.WithDeadline(",
	"context.WithCancelCause(",
	"context.WithTimeoutCause(",
	"context.WithDeadlineCause(",
}

func TestBatchContextGuard(t *testing.T) {
	root, err := findRepoRoot(t)
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	var actual []string
	for _, dir := range []string{"internal", "cmd"} {
		walkErr := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if prepareBatchCallRe.Match(body) {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				actual = append(actual, filepath.ToSlash(rel))
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s/: %v", dir, walkErr)
		}
	}
	sort.Strings(actual)

	expected := append([]string(nil), batchContextExpectedFiles...)
	sort.Strings(expected)

	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("файлы с вызовом PrepareBatch разошлись со списком ожидаемых:\nожидание: %v\nфакт:     %v", expected, actual)
	}

	for _, rel := range actual {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		if !strings.Contains(text, "db.BatchContext(") {
			t.Errorf("%s: вызывает PrepareBatch, но не использует db.BatchContext", rel)
		}
		for _, forbidden := range forbiddenBatchContextCtors {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s: содержит %s — контекст батча снова может стать отменяемым", rel, forbidden)
			}
		}
	}
}

func findRepoRoot(t *testing.T) (string, error) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
