package notify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryNotifierPassesThroughTheDetailGate(t *testing.T) {
	root := filepath.Join("..") // internal/
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(src)
		// Доставка — это Outbox.Enqueue(channelID, payload); pipeline.Enqueue (тот же метод
		// на конвейере приёма) к доставке отношения не имеет и не пишет в очередь уведомлений.
		if !strings.Contains(text, ".Outbox.Enqueue(") {
			return nil
		}
		if !strings.Contains(text, "AllowsDetails(") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("эти файлы ставят уведомление в очередь, минуя гейт "+
			"alert.DetailPolicy.AllowsDetails — детали события уедут получателю "+
			"вне контура оператора (152-ФЗ ст. 12): %v", offenders)
	}
}

func TestEveryNotifierChecksDeliverable(t *testing.T) {
	root := filepath.Join("..")
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(src)
		if !strings.Contains(text, ".Outbox.Enqueue(") {
			return nil
		}
		// Голая проверка "!ch.Enabled" вместо Deliverable() — то, от чего сторож защищает.
		if strings.Contains(text, "!ch.Enabled") || !strings.Contains(text, "Deliverable()") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("эти нотифаеры фильтруют каналы не через Channel.Deliverable() — "+
			"канал с нечитаемым секретом получит уведомление с пустым секретом: %v", offenders)
	}
}

const commonDispatchFile = "escalation/notifydispatch.go"

// alert/digest.go — сводка по батчу, не ступень эскалации; trace/notify.go —
// уведомление о самом issue, не эскалация (эскалацию делает regression_notify.go).
var directEnqueueExceptions = map[string]bool{
	"alert/digest.go": true,
	"trace/notify.go": true,
}

// Проверяет по конструкции общего контура, не по числу сырых Enqueue: файл
// диспетчера жив, dispatch зовут ≥7 файлов, обходов вне exceptions нет.
func TestGateCoverageTestItselfWorks(t *testing.T) {
	root := filepath.Join("..")
	dispatchCallers := map[string]bool{}
	var bypassers []string
	commonFileFound := false

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		text := string(src)

		if strings.Contains(text, "escalation.Dispatch(") {
			dispatchCallers[rel] = true
		}
		if strings.Contains(text, ".Outbox.Enqueue(") {
			switch {
			case rel == commonDispatchFile:
				commonFileFound = true
			case directEnqueueExceptions[rel]:
			default:
				bypassers = append(bypassers, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход дерева: %v", err)
	}

	if !commonFileFound {
		t.Fatalf("%s больше не ставит уведомление в очередь через .Outbox.Enqueue( — "+
			"TestEveryNotifier* выше проверяют пустое множество файлов", commonDispatchFile)
	}
	if len(dispatchCallers) < 7 {
		t.Fatalf("escalation.Dispatch( зовут только %d файлов, ожидалось не меньше 7 — "+
			"источник инцидента перестал идти через общий контур: %v", len(dispatchCallers), dispatchCallers)
	}
	if len(bypassers) > 0 {
		t.Fatalf("эти файлы ставят уведомление в очередь напрямую, в обход "+
			"escalation.Dispatch, и не входят в directEnqueueExceptions: %v", bypassers)
	}
}
