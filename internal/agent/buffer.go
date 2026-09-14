package agent

// Не потокобезопасен; используется только из горутины Run (run.go).
type Buffer struct {
	maxBatches int
	maxBytes   int
	batches    [][]byte
	points     []int // параллельно batches — точек OTLP в соответствующем батче
	totalBytes int

	droppedOversizedPoints int // с последнего TakeDropped: батч не влез целиком
	droppedEvictedPoints   int // с последнего TakeDropped: вытеснен переполнением из Push
}

// maxBytes — суммарный лимит по всем батчам, не лимит одного: батч крупнее
// него отбрасывается в Push целиком, не трогая остальное.
func NewBuffer(maxBatches, maxBytes int) *Buffer {
	return &Buffer{maxBatches: maxBatches, maxBytes: maxBytes}
}

// points — число OTLP-точек в body; нужно, чтобы честно считать потери
// в точках, а не в батчах (батчи разных тиков несут разное число точек).
func (b *Buffer) Push(body []byte, points int) {
	if len(body) > b.maxBytes {
		b.droppedOversizedPoints += points
		return
	}
	b.batches = append(b.batches, body)
	b.points = append(b.points, points)
	b.totalBytes += len(body)
	for len(b.batches) > b.maxBatches || b.totalBytes > b.maxBytes {
		b.droppedEvictedPoints += b.points[0]
		b.DropOldest()
	}
}

// Считает только потери из Push (переполнение); успешный дренаж тоже зовёт
// DropOldest, но это не потеря — вызывающий сам её не считает.
func (b *Buffer) TakeDropped() (oversizedPoints, evictedPoints int) {
	oversizedPoints, evictedPoints = b.droppedOversizedPoints, b.droppedEvictedPoints
	b.droppedOversizedPoints, b.droppedEvictedPoints = 0, 0
	return
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
	b.points = b.points[1:]
}

func (b *Buffer) Len() int {
	return len(b.batches)
}
