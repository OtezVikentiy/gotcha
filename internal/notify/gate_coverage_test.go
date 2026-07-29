package notify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryNotifierPassesThroughTheDetailGate — ловушка на восьмой нотифаер.
//
// Гейт трансграничной передачи должен стоять перед КАЖДОЙ постановкой
// уведомления в очередь. Нотифаеров семь, они лежат в шести пакетах, и это уже
// один раз выстрелило: правка, которая по описанию касалась «двух нотифаеров»,
// на деле касалась шести, а несовпадение поймала не проверка, а случайность.
// Обычный юнит-тест такое не ловит — он проверяет тот нотифаер, про который
// автор помнил.
//
// Тест грубый намеренно: он смотрит на исходники, а не на поведение, потому
// что предмет проверки — свойство ВСЕГО дерева («нигде не забыли»), а не
// поведение отдельной функции. Ложное срабатывание здесь дешёвое: автор нового
// нотифаера прочтёт сообщение и либо добавит гейт, либо объяснит исключение.
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
		// Постановка в очередь доставки — это Outbox.Enqueue(ctx, channelID,
		// payload). Одноимённый метод конвейера приёма (pipeline.Enqueue) к
		// доставке отношения не имеет и в очередь уведомлений не пишет.
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

// TestGateCoverageTestItselfWorks — контроль самой ловушки: если бы она не
// находила ни одного файла с постановкой в очередь, она была бы зелёной всегда
// и не проверяла ничего (ровно та же болезнь, что у «поспал и убедился, что
// ничего не произошло»).
func TestGateCoverageTestItselfWorks(t *testing.T) {
	root := filepath.Join("..")
	found := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(src), ".Outbox.Enqueue(") {
			found++
		}
		return nil
	})
	// Семь нотифаеров: alert.Evaluator, alert.Digester, metric, trace (алерт и
	// регрессия), profile, uptime.
	if found < 7 {
		t.Fatalf("найдено %d файлов с постановкой в очередь, ожидалось не меньше 7 — "+
			"поиск сломан, и соседний тест ничего не проверяет", found)
	}
}
