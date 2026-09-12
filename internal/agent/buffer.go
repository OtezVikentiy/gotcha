package agent

// Не потокобезопасен; используется только из горутины Run (run.go).
type Buffer struct {
	maxBatches int
	maxBytes   int
	batches    [][]byte
	totalBytes int
}

// maxBytes — суммарный лимит по всем батчам, не лимит одного: батч крупнее
// него отбрасывается в Push целиком, не трогая остальное.
func NewBuffer(maxBatches, maxBytes int) *Buffer {
	return &Buffer{maxBatches: maxBatches, maxBytes: maxBytes}
}

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

// Не удаляет батч — вызывающий сначала пробует отправить, и только успех
// освобождает место через DropOldest.
func (b *Buffer) Oldest() ([]byte, bool) {
	if len(b.batches) == 0 {
		return nil, false
	}
	return b.batches[0], true
}

// Нет-оп на пустом буфере.
func (b *Buffer) DropOldest() {
	if len(b.batches) == 0 {
		return
	}
	b.totalBytes -= len(b.batches[0])
	b.batches[0] = nil // не удерживать память под GC дольше нужного
	b.batches = b.batches[1:]
}

func (b *Buffer) Len() int {
	return len(b.batches)
}
