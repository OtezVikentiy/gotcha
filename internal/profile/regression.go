package profile

// RegressionConfig — параметры детектора регрессий self-CPU функций (этап 9).
type RegressionConfig struct {
	ThresholdPct  float64 // открытие: recent > base×(1+ThresholdPct)
	RecoveryPct   float64 // закрытие: recent ≤ base×(1+RecoveryPct); RecoveryPct < ThresholdPct (гистерезис)
	WindowMinutes int     // размер свежего окна
	BaselineDays  int     // окно скользящей базы (медиана дневных долей)
	MinSamples    int     // минимум сэмплов (total value) в окне, иначе решения нет
	ShareFloor    float64 // абсолютный пол доли: функции ниже не открываем (шум)
	TopK          int     // сколько верхних функций проверять на сервис
}

// DefaultProfileRegressionConfig — дефолты (профили реже, база дневная).
func DefaultProfileRegressionConfig() RegressionConfig {
	return RegressionConfig{
		ThresholdPct:  0.5,
		RecoveryPct:   0.2,
		WindowMinutes: 60,
		BaselineDays:  7,
		MinSamples:    100,
		ShareFloor:    0.05,
		TopK:          20,
	}
}

// DecisionKind — что сделать с инцидентом функции на текущем тике.
type DecisionKind int

const (
	DecisionNone DecisionKind = iota
	DecisionOpen
	DecisionResolve
	DecisionBump
)

// Decision — результат Decide.
type Decision struct {
	Kind DecisionKind
}

// Decide решает по свежей self-доле функции, скользящей базе и наличию
// открытого инцидента. Мало сэмплов → None. Открытие: доля выросла над базой на
// ThresholdPct, доля ≥ пола, база > 0. Закрытие: доля вернулась в пределы
// RecoveryPct (гистерезис); при усохшей до нуля базе — при возврате доли к
// шумовому полу ShareFloor. Иначе (открыт и всё ещё нарушено/мёртвая зона) — Bump.
func Decide(baseShare, recentShare float64, recentSamples uint64, cfg RegressionConfig, open bool) Decision {
	if recentSamples < uint64(cfg.MinSamples) {
		return Decision{Kind: DecisionNone}
	}
	if open {
		// Гистерезис закрытия. Пока есть база — закрываем при возврате доли под
		// recovery-порог base×(1+RecoveryPct). Если же база усохла до нуля
		// (функция перестала попадать в базовое окно, напр. переставала
		// исполняться N дней), сравнивать не с чем: закрываем, когда доля
		// вернулась к абсолютному шумовому полу ShareFloor. Без этой ветки
		// инцидент с base==0 навсегда застревал в Bump и держал on-call на пейдже.
		if baseShare > 0 {
			if recentShare <= baseShare*(1+cfg.RecoveryPct) {
				return Decision{Kind: DecisionResolve}
			}
			return Decision{Kind: DecisionBump}
		}
		if recentShare <= cfg.ShareFloor {
			return Decision{Kind: DecisionResolve}
		}
		return Decision{Kind: DecisionBump}
	}
	if recentShare >= cfg.ShareFloor && baseShare > 0 && recentShare > baseShare*(1+cfg.ThresholdPct) {
		return Decision{Kind: DecisionOpen}
	}
	return Decision{Kind: DecisionNone}
}
