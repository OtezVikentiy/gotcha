package metric

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
// при current<=threshold*(1-band). lt — зеркально.
func Decide(current float64, comparator string, threshold float64, open bool) Decision {
	var breached, recovered bool
	switch comparator {
	case "gt":
		breached = current > threshold
		recovered = current <= threshold*(1-recoveryBand)
	case "lt":
		breached = current < threshold
		recovered = current >= threshold*(1+recoveryBand)
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
