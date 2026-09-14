package profile

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"runtime"
	stdpprof "runtime/pprof"
	"strings"
	"testing"
	"time"

	pp "github.com/google/pprof/profile"
)

// lenDelimField кодирует один минимальный элемент repeated-поля (тег + длина 0):
// для sample/location/function/string_table значения полей нашему счётчику не нужны.
func lenDelimField(fieldNum int) []byte {
	return []byte{byte(fieldNum<<3 | 2), 0}
}

func TestCheckPprofLimitsBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		field int
		cap   int
	}{
		{"sample", 2, maxStacks},
		{"mapping", 3, maxProfileMappings},
		{"location", 4, maxProfileLocations},
		{"function", 5, maxProfileFunctions},
		{"string_table", 6, maxProfileStrings},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atCap := bytes.Repeat(lenDelimField(c.field), c.cap)
			if err := checkPprofLimits(atCap); err != nil {
				t.Fatalf("ровно потолок (%d) отвергнут: %v", c.cap, err)
			}
			overCap := bytes.Repeat(lenDelimField(c.field), c.cap+1)
			err := checkPprofLimits(overCap)
			if !errors.Is(err, ErrProfileTooLarge) {
				t.Fatalf("потолок+1 (%d) не отвергнут: %v", c.cap+1, err)
			}
		})
	}
}

func TestCheckPprofLimitsGarbageSafe(t *testing.T) {
	cases := map[string][]byte{
		"nil":                  nil,
		"empty":                {},
		"truncated varint tag": {0x80},
		"varint over 10 bytes": {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// field 4 (location), длина заявлена огромной, буфер за тегом пуст — длина врёт.
		"length lies past buffer": {0x22, 0xff, 0xff, 0xff, 0x7f},
		// поле не одно из капнутых (field 1, varint) — не должно ничего насчитать.
		"unrelated varint field": {0x08, 0x01},
		// group wire type (3) — наш формат не использует, декодер должен отступить, не упасть.
		"group wire type":    {0x03},
		"not a pprof at all": []byte("not-a-pprof"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			if err := checkPprofLimits(data); errors.Is(err, ErrProfileTooLarge) {
				t.Fatalf("мусорный/обрезанный вход отвергнут precheck'ом как слишком большой: %v", err)
			}
		})
	}
}

// pprofManySamples строит несжатый протобуф с n минимальными Sample, все на одном
// общем Location — форма находки: крохотный провод, счётный взрыв в памяти без предела.
func pprofManySamples(t *testing.T, n int) []byte {
	t.Helper()
	fn := &pp.Function{ID: 1, Name: "f"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
	}
	prof.Sample = make([]*pp.Sample, n)
	for i := 0; i < n; i++ {
		prof.Sample[i] = &pp.Sample{Location: []*pp.Location{loc}, Value: []int64{1}}
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	return buf.Bytes()
}

func heapAllocDelta(f func()) int64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

func TestParsePprofRejectsOversizedSampleCountBeforeDecoding(t *testing.T) {
	raw := pprofManySamples(t, maxStacks+1)

	var got Profile
	var err error
	d := heapAllocDelta(func() {
		got, err = ParsePprof(raw, "", time.Now())
	})
	if !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("err = %v, want ErrProfileTooLarge", err)
	}
	if len(got.Samples) != 0 {
		t.Fatalf("samples = %d, want 0 — отказ обязан быть до декодирования", len(got.Samples))
	}
	// Находка: 1.5 млн сэмплов давали 242 МБ. Отказ до декодирования держит кучу
	// в единицах МБ независимо от заявленного числа сэмплов на входе.
	const heapCeiling = 8 << 20
	if d > heapCeiling {
		t.Fatalf("heap выросла на %d байт при отказе (потолок %d) — декодирование не пропущено", d, heapCeiling)
	}
}

func TestParsePprofAcceptsExactlySampleCap(t *testing.T) {
	raw := pprofManySamples(t, maxStacks)

	p, err := ParsePprof(raw, "", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	if len(p.Samples) != maxStacks {
		t.Fatalf("samples = %d, want %d", len(p.Samples), maxStacks)
	}
	if p.Truncated {
		t.Fatal("ровно потолок — Truncated не должен взводиться")
	}
}

// Доказывает через -benchmem, что отказ на злом входе не материализует профиль.
func BenchmarkParsePprofRejectsOversized(b *testing.B) {
	loc := &pp.Location{ID: 1}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Location:   []*pp.Location{loc},
	}
	const n = 1500000
	prof.Sample = make([]*pp.Sample, n)
	for i := 0; i < n; i++ {
		prof.Sample[i] = &pp.Sample{Location: []*pp.Location{loc}, Value: []int64{1}}
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		b.Fatalf("WriteUncompressed: %v", err)
	}
	raw := buf.Bytes()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParsePprof(raw, "", time.Now()); !errors.Is(err, ErrProfileTooLarge) {
			b.Fatal(err)
		}
	}
}

// ASCII-легаси профиль, в который pp.ParseData проваливается после неудачного
// протобуф-разбора; своего лимита у parseHeap нет.
func legacyHeapProfileText(nLines int) []byte {
	var sb strings.Builder
	sb.WriteString("heap profile: 1: 8 [1: 8] @ heap_v2/1\n")
	for i := 0; i < nLines; i++ {
		sb.WriteString("1: 8 [1: 8] @ 0x1000\n")
	}
	return []byte(sb.String())
}

func TestCheckPprofLimitsLegacyByteBoundary(t *testing.T) {
	// 0x03 (group wire type) — precheck отступает на первом теге, граница детерминирована.
	atCap := bytes.Repeat([]byte{0x03}, maxLegacyPprofBytes)
	if err := checkPprofLimits(atCap); err != nil {
		t.Fatalf("ровно потолок (%d байт) отвергнут: %v", maxLegacyPprofBytes, err)
	}
	overCap := bytes.Repeat([]byte{0x03}, maxLegacyPprofBytes+1)
	if err := checkPprofLimits(overCap); !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("потолок+1 (%d байт) не отвергнут: %v", maxLegacyPprofBytes+1, err)
	}
}

func TestParsePprofRejectsOversizedLegacyText(t *testing.T) {
	// форма находки ревьюера: ASCII heap-профиль, ~29-33× усиления на байт.
	raw := legacyHeapProfileText(1500000)
	if len(raw) <= maxLegacyPprofBytes {
		t.Fatalf("фикстура (%d байт) не превышает потолок (%d) — тест ничего не проверяет", len(raw), maxLegacyPprofBytes)
	}

	var got Profile
	var err error
	d := heapAllocDelta(func() {
		got, err = ParsePprof(raw, "", time.Now())
	})
	if !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("err = %v, want ErrProfileTooLarge", err)
	}
	if len(got.Samples) != 0 {
		t.Fatalf("samples = %d, want 0 — legacy-парсер не должен запускаться вовсе", len(got.Samples))
	}
	const heapCeiling = 24 << 20
	if d > heapCeiling {
		t.Fatalf("heap выросла на %d байт при отказе (потолок %d) — legacy-парсер не был остановлен", d, heapCeiling)
	}
}

// Живой профиль от runtime/pprof: настоящий протобуф идёт по элементным капам,
// не по байтовому потолку легаси-ветки.
func TestParsePprofAcceptsRealRuntimeProfile(t *testing.T) {
	var gz bytes.Buffer
	if err := stdpprof.WriteHeapProfile(&gz); err != nil {
		t.Fatalf("WriteHeapProfile: %v", err)
	}
	zr, err := gzip.NewReader(&gz)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	p, err := ParsePprof(raw, "", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	if p.Truncated {
		t.Fatal("настоящий небольшой профиль не должен усекаться")
	}
}

// Закрывает fixed64/fixed32 (успех/обрыв) и обрыв варинта значения/длины —
// ветки, не задетые ни счётными, ни garbage-тестами.
func TestCheckPprofLimitsWireTypeBranches(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"fixed64 ok", append([]byte{byte(1<<3 | 1)}, make([]byte, 8)...)},
		{"fixed32 ok", append([]byte{byte(1<<3 | 5)}, make([]byte, 4)...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkPprofLimits(c.data); err != nil {
				t.Fatalf("валидный вход отвергнут: %v", err)
			}
		})
	}

	truncated := []struct {
		name string
		data []byte
	}{
		{"fixed64 truncated", append([]byte{byte(1<<3 | 1)}, make([]byte, 4)...)},
		{"fixed32 truncated", append([]byte{byte(1<<3 | 5)}, make([]byte, 2)...)},
		{"value varint truncated", []byte{byte(1 << 3), 0x80}},
		{"length varint truncated", []byte{byte(4<<3 | 2), 0x80}},
	}
	for _, c := range truncated {
		t.Run(c.name, func(t *testing.T) {
			if err := checkPprofLimits(c.data); err != nil {
				t.Fatalf("обрезанный крохотный вход (%d байт) не должен отвергаться: %v", len(c.data), err)
			}
		})
	}
}

// Честный кусок протобуфа больше maxLegacyPprofBytes из string_table-полей —
// дёшево накопить размер без риска задеть maxProfileStrings.
func bigCleanStringTableTail() []byte {
	entry := append([]byte{byte(6<<3 | 2), 10}, bytes.Repeat([]byte("x"), 10)...)
	return bytes.Repeat(entry, 45000) // 45000×12 = 540000 байт > 512 КиБ
}

// Различает «явный скип и счёт дальше» от «бэйл в потолок»: без wireType=1
// вход не досчитывается чисто и отвергается по размеру, несмотря на честный хвост.
func TestCheckPprofLimitsFixed64SkipDoesNotForceLegacyCap(t *testing.T) {
	tail := bigCleanStringTableTail()
	if len(tail) <= maxLegacyPprofBytes {
		t.Fatalf("фикстура (%d байт) не превышает потолок (%d) — тест ничего не проверяет", len(tail), maxLegacyPprofBytes)
	}
	fixed64Field := append([]byte{byte(7<<3 | 1)}, make([]byte, 8)...) // неизвестное поле 7 (drop_frames), wireType 1 — тип не совпадает, декодер всё равно скипает по wire-механике
	data := append(fixed64Field, tail...)

	if err := checkPprofLimits(data); err != nil {
		t.Fatalf("честный fixed64-пропуск в буфере > потолка отвергнут: %v (буфер %d байт)", err, len(data))
	}
}

func TestCheckPprofLimitsFixed32SkipDoesNotForceLegacyCap(t *testing.T) {
	tail := bigCleanStringTableTail()
	if len(tail) <= maxLegacyPprofBytes {
		t.Fatalf("фикстура (%d байт) не превышает потолок (%d) — тест ничего не проверяет", len(tail), maxLegacyPprofBytes)
	}
	fixed32Field := append([]byte{byte(7<<3 | 5)}, make([]byte, 4)...) // неизвестное поле 7 (drop_frames), wireType 5 — тип не совпадает, декодер всё равно скипает по wire-механике
	data := append(fixed32Field, tail...)

	if err := checkPprofLimits(data); err != nil {
		t.Fatalf("честный fixed32-пропуск в буфере > потолка отвергнут: %v (буфер %d байт)", err, len(data))
	}
}

// Реальный протобуф (через библиотеку): один Sample с packed location_id (>2
// элементов упаковываются) — форма находки ревьюера.
func packedLocationIDBytes(t *testing.T, n int) []byte {
	t.Helper()
	fn := &pp.Function{ID: 1, Name: "f"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	locs := make([]*pp.Location, n)
	for i := range locs {
		locs[i] = loc
	}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
		Sample:     []*pp.Sample{{Location: locs, Value: []int64{1}}},
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	return buf.Bytes()
}

func TestParsePprofRejectsPackedLocationIDBomb(t *testing.T) {
	// форма находки ревьюера (8 млн упакованных location_id дали там +128.1 МБ).
	raw := packedLocationIDBytes(t, maxProfileElements*4)
	if len(raw) >= 10<<20 {
		t.Fatalf("фикстура (%d байт) не влезает под лимит распаковки 10 МиБ — тест не соответствует эндпойнту", len(raw))
	}

	var got Profile
	var err error
	d := heapAllocDelta(func() {
		got, err = ParsePprof(raw, "", time.Now())
	})
	if !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("err = %v, want ErrProfileTooLarge", err)
	}
	if len(got.Samples) != 0 {
		t.Fatalf("samples = %d, want 0 — отказ обязан быть до декодирования", len(got.Samples))
	}
	const heapCeiling = 24 << 20
	if d > heapCeiling {
		t.Fatalf("heap выросла на %d байт при отказе (потолок %d) — packed location_id не был остановлен", d, heapCeiling)
	}
}

func TestCheckPprofLimitsElementBudgetBoundary(t *testing.T) {
	atCap := packedVarintOnlySampleBytes(maxProfileElements)
	if err := checkPprofLimits(atCap); err != nil {
		t.Fatalf("ровно бюджет (%d элементов) отвергнут: %v", maxProfileElements, err)
	}
	overCap := packedVarintOnlySampleBytes(maxProfileElements + 1)
	if err := checkPprofLimits(overCap); !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("бюджет+1 не отвергнут: %v", err)
	}
}

// Минимальный протобуф с одним Sample и n элементами в packed Sample.value.
func packedVarintOnlySampleBytes(n int) []byte {
	value := bytes.Repeat([]byte{1}, n)
	var sample bytes.Buffer
	sample.WriteByte(2<<3 | 2) // Sample.value, packed (length-delimited)
	writeVarintN(&sample, uint64(n))
	sample.Write(value)

	var prof bytes.Buffer
	prof.WriteByte(6<<3 | 2) // string_table[0] = ""
	prof.WriteByte(0)
	prof.WriteByte(2<<3 | 2) // Profile.sample
	writeVarintN(&prof, uint64(sample.Len()))
	prof.Write(sample.Bytes())
	return prof.Bytes()
}

func writeVarintN(b *bytes.Buffer, v uint64) {
	for v >= 0x80 {
		b.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	b.WriteByte(byte(v))
}

// Честный профиль с реальной вложенностью (Location.line) в пределах бюджета —
// новый механизм не задевает форму, защищённую K2.
func TestParsePprofAcceptsNestedContentWithinBudget(t *testing.T) {
	const nLines = 1000
	raw := buildPprofOneLocationManyLines(nLines, 1, 8)

	p, err := ParsePprof(raw, "samples", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	if len(p.Samples) != 1 || len(p.Samples[0].Stack) != nLines {
		t.Fatalf("samples/stack = %d/%d, want 1/%d", len(p.Samples), len(p.Samples[0].Stack), nLines)
	}
	if p.Truncated {
		t.Fatal("в пределах всех бюджетов — усечения не должно быть")
	}
}

// commentFieldBytes — Profile.comment (field 13), packed repeated int64: тот же
// класс смуглинга, что Sample.location_id/value, только на верхнем уровне.
func commentFieldBytes(n int) []byte {
	value := bytes.Repeat([]byte{1}, n)
	var b bytes.Buffer
	b.WriteByte(13<<3 | 2)
	writeVarintN(&b, uint64(n))
	b.Write(value)
	return b.Bytes()
}

func TestCheckPprofLimitsCommentPackedVarintBoundary(t *testing.T) {
	atCap := commentFieldBytes(maxProfileElements)
	if err := checkPprofLimits(atCap); err != nil {
		t.Fatalf("ровно бюджет через comment (%d) отвергнут: %v", maxProfileElements, err)
	}
	overCap := commentFieldBytes(maxProfileElements + 1)
	if err := checkPprofLimits(overCap); !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("бюджет+1 через comment не отвергнут: %v", err)
	}
}

func TestParsePprofAcceptsSmallComment(t *testing.T) {
	fn := &pp.Function{ID: 1, Name: "f"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 1}}}
	prof := &pp.Profile{
		SampleType: []*pp.ValueType{{Type: "samples", Unit: "count"}},
		Function:   []*pp.Function{fn},
		Location:   []*pp.Location{loc},
		Sample:     []*pp.Sample{{Location: []*pp.Location{loc}, Value: []int64{1}}},
		Comments:   []string{"built by test", "another comment"},
	}
	var buf bytes.Buffer
	if err := prof.WriteUncompressed(&buf); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	p, err := ParsePprof(buf.Bytes(), "samples", time.Now())
	if err != nil {
		t.Fatalf("ParsePprof: %v", err)
	}
	if len(p.Samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(p.Samples))
	}
}

// Оборачивает nested как содержимое одного top-level Sample-поля — минимальный
// вход для checkPprofLimits, CheckValid-валидность не нужна.
func wrapAsTopLevelSample(nested []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(2<<3 | 2)
	writeVarintN(&b, uint64(len(nested)))
	b.Write(nested)
	return b.Bytes()
}

// walkPprofNested — отдельный код со своим набором проверок, не покрытым
// аналогами на верхнем уровне (walkPprofTop).
func TestCheckPprofLimitsNestedGarbageSafe(t *testing.T) {
	cases := map[string][]byte{
		"nested tag truncated":     {0x80},
		"nested value truncated":   {byte(1 << 3), 0x80},               // field1 (location_id), wireType0
		"nested length truncated":  {byte(3<<3 | 2), 0x80},             // field3 (label), wireType2
		"nested fixed64 truncated": {byte(1<<3 | 1), 0x11, 0x22, 0x33}, // <8 байт
		"nested fixed32 truncated": {byte(1<<3 | 5), 0x11, 0x22},       // <4 байта
		"nested group wiretype":    {0x03},
	}
	for name, nested := range cases {
		t.Run(name, func(t *testing.T) {
			data := wrapAsTopLevelSample(nested)
			if err := checkPprofLimits(data); err != nil {
				t.Fatalf("крохотный вложенный мусор (%d байт) отвергнут: %v", len(data), err)
			}
		})
	}
}

// Пропагация ошибки — для всех четырёх контейнеров, включая Function/Mapping
// (schemaFlat), не только Sample/Location.
func TestCheckPprofLimitsPropagatesNestedErrorForAllContainers(t *testing.T) {
	tail := bigCleanStringTableTail()
	containers := map[string]int{"sample": 2, "mapping": 3, "location": 4, "function": 5}
	for name, field := range containers {
		t.Run(name, func(t *testing.T) {
			bad := []byte{byte(field<<3 | 2), 1, 0x80} // длина=1, внутри — обрыв варинта
			data := append(append([]byte{}, bad...), tail...)
			if len(data) <= maxLegacyPprofBytes {
				t.Fatalf("фикстура (%d байт) не превышает потолок — тест ничего не проверяет", len(data))
			}
			if err := checkPprofLimits(data); !errors.Is(err, ErrProfileTooLarge) {
				t.Fatalf("несогласованность внутри контейнера %q не долетела наверх — большой вход (%d байт) принят: %v",
					name, len(data), err)
			}
		})
	}
}

// Истощение бюджета через вложенное подсообщение (Sample.label), не packed
// varint — другая ветка кода (pprofMessage, не walkPackedVarint).
func nestedLabelBombBytes(n int) []byte {
	label := []byte{3<<3 | 2, 0} // Sample.label, пустой Label
	var sample bytes.Buffer
	for i := 0; i < n; i++ {
		sample.Write(label)
	}
	return wrapAsTopLevelSample(sample.Bytes())
}

func TestCheckPprofLimitsNestedMessageBudgetBoundary(t *testing.T) {
	atCap := nestedLabelBombBytes(maxProfileElements)
	if err := checkPprofLimits(atCap); err != nil {
		t.Fatalf("ровно бюджет через вложенные Label (%d) отвергнут: %v", maxProfileElements, err)
	}
	overCap := nestedLabelBombBytes(maxProfileElements + 1)
	if err := checkPprofLimits(overCap); !errors.Is(err, ErrProfileTooLarge) {
		t.Fatalf("бюджет+1 через вложенные Label не отвергнут: %v", err)
	}
}

// Обрыв варинта внутри packed-элемента, не тега/длины самого packed-поля.
func TestCheckPprofLimitsPackedVarintElementTruncated(t *testing.T) {
	payload := append(bytes.Repeat([]byte{1}, 10), 0x80) // 10 валидных + 1 обрубленный
	var sample bytes.Buffer
	sample.WriteByte(2<<3 | 2) // Sample.value
	writeVarintN(&sample, uint64(len(payload)))
	sample.Write(payload)

	data := wrapAsTopLevelSample(sample.Bytes())
	if err := checkPprofLimits(data); err != nil {
		t.Fatalf("крохотный вход с обрубленным варинтом внутри packed-поля отвергнут: %v", err)
	}
}

// При текущих схемах глубина не превышает 2 — потолок недостижим легитимным
// входом, это предохранитель от будущей ошибки в схеме, проверенный искусственно.
func TestWalkPprofNestedDepthCap(t *testing.T) {
	self := pprofSchema{}
	self[1] = pprofFieldSpec{kind: pprofMessage, child: self}

	build := func(levels int) []byte {
		var payload []byte
		for i := 0; i < levels; i++ {
			var b bytes.Buffer
			b.WriteByte(1<<3 | 2)
			writeVarintN(&b, uint64(len(payload)))
			b.Write(payload)
			payload = b.Bytes()
		}
		return payload
	}

	t.Run("глубже потолка — отступает", func(t *testing.T) {
		budget := maxProfileElements
		deep := build(maxPprofWalkDepth + 5)
		if err := walkPprofNested(deep, self, 1, &budget); !errors.Is(err, errPprofNotClean) {
			t.Fatalf("вложенность %d при потолке %d должна отступать: %v", maxPprofWalkDepth+5, maxPprofWalkDepth, err)
		}
	})

	t.Run("в пределах потолка — не отступает по глубине", func(t *testing.T) {
		budget := maxProfileElements
		shallow := build(maxPprofWalkDepth - 2)
		if err := walkPprofNested(shallow, self, 1, &budget); errors.Is(err, errPprofNotClean) {
			t.Fatalf("вложенность %d при потолке %d не должна отступать по глубине: %v", maxPprofWalkDepth-2, maxPprofWalkDepth, err)
		}
	})
}
