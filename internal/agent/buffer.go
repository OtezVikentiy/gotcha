package agent

// Buffer — кольцевой буфер недоставленных батчей на случай, если инстанс
// временно недоступен: slice-очередь FIFO с суммарным счётчиком байт. При
// переполнении по числу батчей ИЛИ по суммарному объёму (границы «120
// батчей И 8 МиБ» — спека §1.3) вытесняется самый старый: свежие данные
// ценнее «дырки» в середине истории, а не наоборот.
//
// Не потокобезопасен; используется только из горутины Run (run.go).
type Buffer struct {
	maxBatches int
	maxBytes   int
	batches    [][]byte
	totalBytes int
}

// NewBuffer создаёт буфер с заданными границами. maxBytes — суммарный лимит
// по всем батчам сразу, а не лимит одного батча: батч, который сам по себе
// крупнее maxBytes, никогда не поместится и отбрасывается в Push целиком, не
// трогая остальное содержимое.
func NewBuffer(maxBatches, maxBytes int) *Buffer {
	return &Buffer{maxBatches: maxBatches, maxBytes: maxBytes}
}

// Push добавляет батч в конец очереди и вытесняет старейшие элементы
// oldest-first, пока не выполнятся обе границы.
func (b *Buffer) Push(body []byte) {
	if len(body) > b.maxBytes {
		return
	}
	b.batches = append(b.batches, body)
	b.totalBytes += len(body)
	for len(b.batches) > b.maxBatches || b.totalBytes > b.maxBytes {
		b.DropOldest()
	}
}

// Oldest возвращает самый старый батч, не удаляя его из буфера — вызывающий
// (T7) сначала пробует отправить, и только успех освобождает место через
// DropOldest.
func (b *Buffer) Oldest() ([]byte, bool) {
	if len(b.batches) == 0 {
		return nil, false
	}
	return b.batches[0], true
}

// DropOldest удаляет самый старый батч. Нет-оп на пустом буфере.
func (b *Buffer) DropOldest() {
	if len(b.batches) == 0 {
		return
	}
	b.totalBytes -= len(b.batches[0])
	b.batches[0] = nil // не удерживать память под GC дольше нужного
	b.batches = b.batches[1:]
}

// Len — текущее число буферизованных батчей.
func (b *Buffer) Len() int {
	return len(b.batches)
}
