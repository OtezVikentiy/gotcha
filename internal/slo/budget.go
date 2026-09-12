package slo

func sums(bs []Bucket) (good, total uint64) {
	for _, b := range bs {
		good += b.Good
		total += b.Total
	}
	return good, total
}

// target вне (0,1) даёт ok=false, чтобы деление не дало Inf/NaN — CHECK БД это
// и так гарантирует, но здесь защищаемся тоже.
func budgetSize(target float64) (size float64, ok bool) {
	size = 1 - target
	if size <= 0 {
		return 0, false
	}
	return size, true
}

// ok=false при пустом окне (Total==0).
func Attainment(bs []Bucket) (ratio float64, ok bool) {
	good, total := sums(bs)
	if total == 0 {
		return 0, false
	}
	return float64(good) / float64(total), true
}

// (1-attainment)/(1-target): 0=бюджет цел, 1=исчерпан ровно, >1=перерасход.
func BudgetConsumedFraction(bs []Bucket, target float64) (frac float64, ok bool) {
	a, aok := Attainment(bs)
	if !aok {
		return 0, false
	}
	size, sok := budgetSize(target)
	if !sok {
		return 0, false
	}
	return (1 - a) / size, true
}

// 1-consumed: >0=есть запас, 0=исчерпан, <0=перерасход.
func BudgetRemainingFraction(bs []Bucket, target float64) (frac float64, ok bool) {
	c, cok := BudgetConsumedFraction(bs, target)
	if !cok {
		return 0, false
	}
	return 1 - c, true
}

// та же формула, что BudgetConsumedFraction, но на узком под-окне (fast/slow).
func BurnRate(bs []Bucket, target float64) (rate float64, ok bool) {
	return BudgetConsumedFraction(bs, target)
}

type BurnDecision struct {
	OpenSignal  bool    // оба окна горят ≥ threshold → сигнал открыть инцидент
	CloseSignal bool    // короткое окно остыло < threshold → сигнал закрыть
	BurnLong    float64 // burn rate длинного (slow) окна
	BurnShort   float64 // burn rate короткого (fast) окна
}

// оба окна должны гореть — отсекает случайные всплески. Гистерезис флапа
// (N тиков перед закрытием) живёт в evaluator, не здесь: это момент времени.
//
// нет данных хотя бы в одном окне (ok=false) — решение нулевое: не открываем
// и не закрываем, единообразно с internal/metric.Evaluator.evalRule.
func DecideBurn(long, short []Bucket, target, threshold float64) BurnDecision {
	burnLong, longOK := BurnRate(long, target)
	burnShort, shortOK := BurnRate(short, target)
	if !longOK || !shortOK {
		return BurnDecision{}
	}
	return BurnDecision{
		OpenSignal:  burnLong >= threshold && burnShort >= threshold,
		CloseSignal: burnShort < threshold,
		BurnLong:    burnLong,
		BurnShort:   burnShort,
	}
}
