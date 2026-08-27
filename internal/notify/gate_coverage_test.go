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

// TestEveryNotifierChecksDeliverable — второй сторож той же формы: канал,
// у которого не читается секрет, не должен получать уведомлений.
//
// Правило «слать можно только во включённый канал с рабочим секретом» живёт в
// Channel.Deliverable(). Разложенное по семи нотифаерам условие разъедется:
// проверка на выключенность уже была в каждом из них по отдельности, и добавить
// к ней вторую половину в шести местах из семи — вопрос одного невнимательного
// патча. Отправка с пустым секретом означает вебхук без подписи и запрос в
// Telegram без токена.
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
		// Голая проверка включённости вместо Deliverable() — ровно та ошибка,
		// от которой этот сторож защищает.
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

// commonDispatchFile — общий контур постановки в очередь (W3-E): семь
// источников инцидентов (host, metric, slo, profile, trace-regression,
// uptime, alert.Evaluator) раньше несли по копии dispatch() каждый, и копии
// уже успели разойтись (ContainsID был не во всех). Теперь весь код,
// который реально зовёт Outbox.Enqueue для эскалации, лежит здесь одной
// точкой — и TestEveryNotifierPassesThroughTheDetailGate/
// TestEveryNotifierChecksDeliverable выше видят её как единственный
// найденный файл.
const commonDispatchFile = "escalation/notifydispatch.go"

// directEnqueueExceptions — файлы вне общего контура, которым РАЗРЕШЕНО
// ставить в очередь напрямую: форма постановки у них другая, а не забытая
// миграция в escalation.Dispatch.
//   - alert/digest.go — Digester шлёт сводку по батчу подавленных алертов
//     одним уведомлением, а не по одной ступени эскалации на канал; контур
//     Dispatch рассчитан на ChannelIDs/ContainsID одной ступени, сюда не
//     ложится.
//   - trace/notify.go — OutboxNotifier уведомляет о самом факте
//     перф-issue (NotifyNew/NotifyRegression), это не ступенчатая
//     эскалация. За эскалацию регрессии отвечает
//     trace/regression_notify.go, который уже идёт через Dispatch.
//
// Новый файл в этом списке — решение, которое принимает автор нотифаера
// сознательно (и объясняет здесь, почему его форма не ложится на контур), а
// не тихая правка счётчика.
var directEnqueueExceptions = map[string]bool{
	"alert/digest.go": true,
	"trace/notify.go": true,
}

// TestGateCoverageTestItselfWorks — контроль самой ловушки. До W3-E он
// требовал не меньше семи ФАЙЛОВ с буквальной ".Outbox.Enqueue(" — это было
// верно, пока дублировали dispatch() по источникам. Консолидация в общий
// контур (escalation.Dispatch) законно свела прямые вызовы к трём файлам, и
// счётчик «не меньше семи» упал не на регрессии, а на здоровой уборке долга.
//
// Опора теперь — не число файлов с сырой строкой, а два свойства, которые
// остаются истинными по конструкции общего контура:
//
//  1. У commonDispatchFile не пропала точка постановки — иначе
//     TestEveryNotifier* выше находят пустое множество файлов и становятся
//     вечнозелёными без предмета проверки (та же болезнь, что раньше ловил
//     этот тест).
//  2. Не меньше семи РАЗНЫХ файлов зовут escalation.Dispatch( — источник
//     инцидента определяется не литеральным списком имён в этом тесте, а
//     тем, что он реально вызывает общий контур. Список из семи имён (host,
//     metric, slo, profile, trace-regression, uptime, alert.Evaluator) —
//     сегодняшний факт кодовой базы, а не то, что проверяет сборка.
//  3. Ни один файл вне контура и вне явного directEnqueueExceptions не
//     содержит ".Outbox.Enqueue(" — то есть источник не завёл собственный
//     обход контура заново.
//
// Чего тест НЕ ловит: правильность значений, которые вызывающий кладёт в
// DispatchChannel (Deliverable/AllowsDetails), — их проверяют предметные
// тесты host/metric/slo/profile/trace/uptime/alert и notifydispatch_test.go
// в пакете escalation.
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
				// разрешённое исключение, см. комментарий выше
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
