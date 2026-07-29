package metric

import "math"

// recoveryBand — полоса гистерезиса: инцидент закрывается только когда значение
// отошло от порога на 5% в безопасную сторону. Без неё значение, колеблющееся
// на границе, хлопало бы инцидент open/close на каждом тике.
const recoveryBand = 0.05

// Decision — что сделать с инцидентом правила на текущем тике. Поля
// взаимоисключающие; всё false → без изменений.
type Decision struct {
	Open  bool
	Close bool
	Bump  bool
}

// Decide решает по текущему значению агрегата, порогу, компаратору и тому, есть
// ли уже открытый инцидент. gt: нарушение при current>threshold, восстановление
// при current <= threshold-band. lt — зеркально.
//
// Полоса берётся от МОДУЛЯ порога, а не умножением на (1±band). Умножение
// разворачивало полосу на отрицательных порогах: при пороге -100 «безопасной
// стороной» для gt получалось -95, то есть ВЫШЕ порога, и значение -98
// оказывалось одновременно нарушением (-98 > -100) и восстановлением
// (-98 <= -95). Взаимоисключающие по смыслу состояния становились истинными
// разом, и Decide хлопал инцидент open/close на каждом тике — ровно та
// лавина, ради предотвращения которой полоса и заводилась. Отрицательные
// пороги валидация допускает и допускать должна: температура, дельта, лаг
// репликации со знаком.
//
// При нулевом пороге полоса тоже нулевая, и мёртвой зоны нет. Это осознанно:
// «порог 0» означает «любое ненулевое значение — нарушение», и относительной
// полосы от нуля не существует, а абсолютная зависела бы от единиц измерения
// метрики, которых Decide не знает. Взаимоисключаемость при этом сохраняется.
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
