package metric

import "math"

// Полоса гистерезиса: инцидент закрывается только когда значение отошло от порога на 5% в безопасную
// сторону — без неё значение на границе хлопало бы open/close на каждом тике.
const recoveryBand = 0.05

// Поля взаимоисключающие; всё false → без изменений.
type Decision struct {
	Open  bool
	Close bool
	Bump  bool
}

// gt: нарушение current>threshold, восстановление current<=threshold-band; lt — зеркально.
// Полоса — от |threshold|: умножение на отрицательных порогах дало бы одновременные Open/Close.
func Decide(current float64, comparator string, threshold float64, open bool) Decision {
	band := recoveryBand * math.Abs(threshold)
	var breached, recovered bool
	switch comparator {
	case "gt":
		breached = current > threshold
		recovered = current <= threshold-band
	case "lt":
		breached = current < threshold
		recovered = current >= threshold+band
	}
	switch {
	case !open && breached:
		return Decision{Open: true}
	case open && recovered:
		return Decision{Close: true}
	case open:
		// Всё ещё нарушено или в мёртвой зоне — держим открытым, обновляем peak.
		return Decision{Bump: true}
	default:
		return Decision{}
	}
}
