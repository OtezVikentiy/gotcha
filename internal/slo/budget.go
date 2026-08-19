package slo

// budget.go — чистая математика error budget и двухоконного burn rate над
// рядом корзин good/total ([]Bucket). Ядро корректности всей фичи: без БД/CH,
// только stdlib. Все функции сумматоры/делители с гвардами на пустое окно
// (Total==0) и вырожденный бюджет ((1-target)==0).
//
// Термины:
//   - error budget    = 1 - target      (допустимая доля плохих событий);
//   - attainment      = sum(Good)/sum(Total);
//   - badRate         = 1 - attainment;
//   - consumed        = badRate / (1-target)  (0=цел, 1=исчерпан, >1=перерасход);
//   - burn rate       = badRate_субокна / (1-target)  (та же формула на под-окне).

// sums суммирует Good и Total по корзинам.
func sums(bs []Bucket) (good, total uint64) {
	for _, b := range bs {
		good += b.Good
		total += b.Total
	}
	return good, total
}

// budgetSize — размер error budget (1-target). Гвард на вырожденный бюджет:
// при target ∉ (0,1) возвращает ok=false, чтобы деление не дало Inf/NaN.
// В норме CHECK БД гарантирует target ∈ (0,1), но защищаемся и здесь.
func budgetSize(target float64) (size float64, ok bool) {
	size = 1 - target
	if size <= 0 {
		return 0, false
	}
	return size, true
}

// Attainment — доля хороших событий sum(Good)/sum(Total) на окне.
// ok=false при пустом окне (Total==0): достижения нет, ratio=0.
func Attainment(bs []Bucket) (ratio float64, ok bool) {
	good, total := sums(bs)
	if total == 0 {
		return 0, false
	}
	return float64(good) / float64(total), true
}

// BudgetConsumedFraction — доля потреблённого error budget:
// (1-attainment)/(1-target). 0=бюджет цел, 1=исчерпан ровно, >1=перерасход.
// ok=false при пустом окне или вырожденном бюджете.
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

// BudgetRemainingFraction — доля оставшегося бюджета: 1-consumed.
// >0=есть запас, 0=исчерпан, <0=перерасход. ok зеркалит consumed.
func BudgetRemainingFraction(bs []Bucket, target float64) (frac float64, ok bool) {
	c, cok := BudgetConsumedFraction(bs, target)
	if !cok {
		return 0, false
	}
	return 1 - c, true
}

// BurnRate — скорость сжигания бюджета на переданном под-окне:
// (1-attainment)/(1-target). Формула совпадает с consumed, но применяется к
// узкому окну (fast/slow) для решения об алерте. ok=false при пустом под-окне
// (rate=0) или вырожденном бюджете.
func BurnRate(bs []Bucket, target float64) (rate float64, ok bool) {
	return BudgetConsumedFraction(bs, target)
}

// BurnDecision — итог двухоконного анализа burn rate.
type BurnDecision struct {
	OpenSignal  bool    // оба окна горят ≥ threshold → сигнал открыть инцидент
	CloseSignal bool    // короткое окно остыло < threshold → сигнал закрыть
	BurnLong    float64 // burn rate длинного (slow) окна
	BurnShort   float64 // burn rate короткого (fast) окна
}

// DecideBurn — двухоконное решение по методу multi-window burn rate.
// OpenSignal = burnLong≥threshold И burnShort≥threshold (оба окна подтверждают
// быстрый прожог — отсекает случайные всплески). CloseSignal = burnShort<threshold
// (короткое окно остыло). Пустое под-окно (нет данных) даёт burn 0 → не open,
// close. Гистерезис флапа (N тиков подряд перед фактическим закрытием) живёт в
// оценщике (evaluator), не здесь: DecideBurn — момент времени.
func DecideBurn(long, short []Bucket, target, threshold float64) BurnDecision {
	burnLong, _ := BurnRate(long, target)
	burnShort, _ := BurnRate(short, target)
	return BurnDecision{
		OpenSignal:  burnLong >= threshold && burnShort >= threshold,
		CloseSignal: burnShort < threshold,
		BurnLong:    burnLong,
		BurnShort:   burnShort,
	}
}
