package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrBadProfile = errors.New("profile: malformed profile")

const (
	maxFrames = 1024
	maxStacks = 100000
	// maxMetaField — кап недоверенных строковых полей профиля (environment/
	// transaction/platform/trace_id) до записи, как capRunes в пакете ingest
	// (otlp.go/transaction.go — те же 200 рун). Свой хелпер здесь, а не импорт
	// из ingest: пакет profile не должен зависеть от приёмного слоя.
	maxMetaField = 200
	// maxFrameField — кап имени функции и файла КАДРА. Обязателен, а не косметика:
	// формат Sentry индексный (Stacks ссылаются на Frames по индексу), поэтому одно
	// огромное имя, упомянутое maxFrames раз, раздувается в Writer.Add при
	// strings.Join ключа стека — 393 КБ тела давали ~409 МБ аллокаций, а тело почти
	// однородно и жмётся до килобайта. Кап ставится на РАЗБОРЕ, до амплификации.
	maxFrameField = 512
	// maxStackBytes — бюджет на профиль по СУММЕ БАЙТ развёрнутых кадров.
	//
	// Счётных капов (maxFrames на стек, maxStacks на профиль) не хватает: они
	// перемножаются, разрешая 1024×100000 развёрнутых кадров, и каждый несёт до
	// 2×maxFrameField байт. Измерено на этом коде: 6.1 КиБ gzip-тела (индексы
	// «0,0,0,…» жмутся в 330 раз) разворачивались в 8.6 ГиБ аллокаций и 12.6 с
	// CPU на горутине запроса — с публичного DSN-ключа, то есть с любого сайта,
	// открытого в браузере. Байтовый бюджет ограничивает работу независимо от
	// того, как разложены стеки: хоть 64 глубоких, хоть 100000 мелких.
	//
	// Размер выбран по верхней границе ЧЕСТНОГО профиля, а не наугад: item
	// конверта ограничен ~1 МиБ, и 654 КиБ индексного JSON разворачиваются в
	// ~80 000 кадров — при длинных символах (60+60 байт) это 9.6 МБ текста.
	// 16 МиБ покрывает такой профиль с запасом. Худший случай по памяти на один
	// item — примерно втрое больше бюджета (сами кадры + ключи + склейка в
	// Writer.Add), то есть десятки МиБ вместо прежних гигабайт.
	maxStackBytes = 16 << 20
	// frameOverheadBytes — постоянная цена ОДНОГО развёрнутого кадра, помимо
	// длины имён.
	//
	// Без неё бюджет обходился дословно: кадр с пустыми function и filename
	// списывал НОЛЬ байт, и защита откатывалась к прежним счётным капам
	// (maxStacks × maxFrames = 102 млн кадров). Измерено на таком входе: 47 КБ
	// gzip → 7.8 МиБ распакованного → ~4 млн кадров и ~240 МиБ аллокаций на
	// горутине одного запроса. Пустое имя стоит памяти не меньше непустого: два
	// заголовка строки по 16 байт, элемент среза Frame, элемент keys[] и
	// разделитель в склейке ключа стека.
	frameOverheadBytes = 64
)

// capRunes обрезает недоверенную строку профиля до n рун (дубль ingest.capRunes:
// поля профиля из SDK не должны раздувать колонки/индексы БД без ограничений).
func capRunes(s string, n int) string {
	// Быстрый путь без единой аллокации. В UTF-8 байт всегда не меньше, чем рун,
	// поэтому len(s) <= n гарантирует, что рун тоже не больше n. Проверка стоит
	// ДО []rune(s) намеренно: подавляющее большинство строк приёма короткие, и
	// раньше каждая из них платила копией всей строки в срез рун — на индексных
	// профилях это давало миллионы копий и гигабайты аллокаций.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type sentryEnvelopeItem struct {
	Platform    string `json:"platform"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
	Transaction struct {
		Name    string `json:"name"`
		TraceID string `json:"trace_id"`
	} `json:"transaction"`
	Transactions []struct {
		Name    string `json:"name"`
		TraceID string `json:"trace_id"`
	} `json:"transactions"`
	Profile struct {
		Frames []struct {
			Function string `json:"function"`
			Filename string `json:"filename"`
			Lineno   int32  `json:"lineno"`
		} `json:"frames"`
		Stacks  [][]int `json:"stacks"`
		Samples []struct {
			StackID int `json:"stack_id"`
		} `json:"samples"`
	} `json:"profile"`
}

// ParseSentry разбирает Sentry profile sample-format v1 в общую модель. Сэмплы
// группируются по stack_id (value=count), стеки переворачиваются из лист→корень
// в корень→лист. Service у Sentry-профиля нет — оставляем пустым.
func ParseSentry(raw []byte, now time.Time) (Profile, error) {
	var it sentryEnvelopeItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrBadProfile, err)
	}
	transaction := it.Transaction.Name
	traceID := it.Transaction.TraceID
	if len(it.Transactions) > 0 {
		if transaction == "" {
			transaction = it.Transactions[0].Name
		}
		if traceID == "" {
			traceID = it.Transactions[0].TraceID
		}
	}

	// value по stack_id.
	counts := make(map[int]uint64)
	for _, s := range it.Profile.Samples {
		counts[s.StackID]++
	}

	// Таблицу кадров каппим ОДИН раз, до разворачивания стеков. Формат
	// индексный: один кадр упоминается в стеках сколько угодно раз, и
	// capRunes на каждом упоминании платил []rune-копией за УПОМИНАНИЕ, а не
	// за кадр — на замере это 1.02 млн копий вместо одной и 6.4 ГиБ из 8.6.
	// Здесь работа линейна по размеру тела, усиления нет.
	frames := make([]Frame, len(it.Profile.Frames))
	for i, fr := range it.Profile.Frames {
		frames[i] = Frame{
			Function: capRunes(fr.Function, maxFrameField),
			File:     capRunes(fr.Filename, maxFrameField),
			Line:     fr.Lineno,
		}
	}

	// Порядок обхода стеков ДЕТЕРМИНИРОВАН и отсортирован по убыванию веса.
	//
	// Обход map случаен, а с введением байтового бюджета усечение стало штатным
	// путём, а не редкостью: два одинаковых байт-в-байт POST'а сохраняли бы
	// разные подмножества стеков, и флеймграф одного профиля менялся бы от
	// загрузки к загрузке. Сортировка по value заодно тратит бюджет на значимые
	// стеки, а не на случайные; stackID — тай-брейк ради устойчивости.
	ordered := make([]int, 0, len(counts))
	for stackID := range counts {
		ordered = append(ordered, stackID)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a < b
	})

	budget := maxStackBytes
	var samples []Sample
	for _, stackID := range ordered {
		value := counts[stackID]
		if stackID < 0 || stackID >= len(it.Profile.Stacks) {
			continue // неизвестный стек — пропуск
		}
		if len(samples) >= maxStacks || budget <= 0 {
			break
		}
		idxs := it.Profile.Stacks[stackID]
		// Переворот лист→корень → корень→лист + кап кадров.
		// Ёмкость по min(len, maxFrames): len(idxs) — недоверенная длина от клиента,
		// и преаллокация по ней выделяла память ДО того, как сработает кап числа кадров.
		stack := make([]Frame, 0, min(len(idxs), maxFrames))
		for i := len(idxs) - 1; i >= 0; i-- {
			if len(stack) >= maxFrames || budget <= 0 {
				break
			}
			fi := idxs[i]
			if fi < 0 || fi >= len(frames) {
				continue
			}
			fr := frames[fi]
			// Кадры уже каппированы в таблице выше — здесь только ссылка на
			// готовые строки, копий не возникает. Списываем бюджет по фактическим
			// байтам имён: именно они дальше превращаются в ключи и склейку.
			budget -= len(fr.Function) + len(fr.File) + frameOverheadBytes
			stack = append(stack, fr)
		}
		if len(stack) == 0 {
			continue
		}
		samples = append(samples, Sample{Stack: stack, Value: value})
	}

	return Profile{
		// Недоверенные строковые поля из payload'а каппим до записи (maxMetaField),
		// как и остальные строки приёма.
		Environment: capRunes(it.Environment, maxMetaField),
		Transaction: capRunes(transaction, maxMetaField),
		Platform:    capRunes(it.Platform, maxMetaField),
		Type:        "cpu",
		// В формате Sentry значение выборки — число сэмплов с этим стеком
		// (см. counts выше), а не время: единица «count», а не наносекунды.
		Unit:      "count",
		TraceID:   capRunes(traceID, maxMetaField),
		Timestamp: now,
		Samples:   samples,
	}, nil
}
