package ingest

import "gitflic.ru/otezvikentiy/gotcha/internal/org"

// keyScopeMatrix — ЕДИНСТВЕННАЯ истина о том, какому типу DSN-ключа какой
// сигнал приёма разрешён (§3.1 спеки, ADR 0012). Матрица живёт здесь, а не в
// internal/org: org знает допустимые ЗНАЧЕНИЯ типа, знание «что кому можно»
// принадлежит приёму.
//
// Матрица ЗАКРЫТА: тип, которого здесь нет (в том числе пустой — забытое
// значение), не получает ничего. Это не оптимистичный дефолт, а fail-closed:
// org.Key{} конструируется в десятке мест, и трактовка пустого типа как
// «полный допуск» означала бы, что любая забытая инициализация выдаёт права.
//
// Ось сигналов — существующий IngestSignal: соответствие маршрут → сигнал
// полное и однозначное, отдельный тип скоупа не заводится.
var keyScopeMatrix = map[org.KeyKind]map[IngestSignal]bool{
	// browser публикуется в JS по замыслу: профили и деплой-маркеры ему
	// закрыты, регистрация хостов — тем более.
	org.KindBrowser: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true, SignalLog: true,
	},
	// server — серверный SDK, CI и коллектор ПРИКЛАДНЫХ метрик. Хосты не
	// регистрирует (для этого есть agent).
	org.KindServer: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true,
		SignalLog: true, SignalProfile: true, SignalDeploy: true,
	},
	// agent — источник ХОСТОВЫХ метрик: только метрики и только он регистрирует
	// хост.
	org.KindAgent: {
		SignalMetric: true,
	},
	// legacy — ключ, выпущенный до появления типов. Полный допуск бессрочно:
	// переход пассивный, чужой приём в дату не ломаем.
	org.KindLegacy: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true,
		SignalLog: true, SignalProfile: true, SignalDeploy: true,
	},
}

// keyScopeHosts — типы, которым разрешена РЕГИСТРАЦИЯ ХОСТА. Это не сигнал, а
// ветка внутри приёма метрик (otlpMetrics), поэтому отдельный предикат: путь
// /v1/metrics открыт троим, а заводить хосты может только agent.
var keyScopeHosts = map[org.KeyKind]bool{
	org.KindAgent:  true,
	org.KindLegacy: true,
}

// allIngestSignals — все шесть входов приёма. Порядок фиксирован ради
// стабильного порядка регистрации self-метрик.
var allIngestSignals = []IngestSignal{
	SignalEvent, SignalTransaction, SignalMetric,
	SignalProfile, SignalLog, SignalDeploy,
}

// envelopeAlsoSignals — сигналы СВЕРХ основного (event), которые может нести
// один envelope. Гейт аутентификации на envelope проверяет допуск к любому из
// них: проверять только event нельзя — пока ни один тип не разрешает
// transaction без event, разницы нет, но связь неявная и сломается молча при
// первой правке матрицы.
var envelopeAlsoSignals = []IngestSignal{SignalTransaction, SignalProfile}

// scopeAllows — допущен ли ключ этого типа к сигналу.
func scopeAllows(kind org.KeyKind, signal IngestSignal) bool {
	return keyScopeMatrix[kind][signal]
}

// scopeAllowsHosts — может ли ключ этого типа зарегистрировать хост.
func scopeAllowsHosts(kind org.KeyKind) bool {
	return keyScopeHosts[kind]
}

// scopeAllowsRoute — открыт ли маршрут ключу этого типа: по основному сигналу
// или, для мультисигнальных входов, по любому из дополнительных. Точный отбор
// внутри мультисигнального запроса — дело вторичного гейта (envelope.go).
func scopeAllowsRoute(kind org.KeyKind, signal IngestSignal, also []IngestSignal) bool {
	if scopeAllows(kind, signal) {
		return true
	}
	for _, s := range also {
		if scopeAllows(kind, s) {
			return true
		}
	}
	return false
}

// keyScopeRejectionPairs — пары (key_scope, signal), которые приёмник способен
// произвести. ВЫЧИСЛЯЮТСЯ из матрицы, а не выписываются руками: countRejected
// молча игнорирует пару, которой нет в наборе, поэтому недостающая пара — это
// fail-silent (отказ есть, счётчика нет), а лишняя — всего лишь ноль на
// дашборде. Цена ошибки несимметрична, значит рассинхрон должен быть
// невозможен по построению.
//
// Пустой тип ключа участвует в переборе НАРАВНЕ с зарегистрированными: он
// запрещает всё, включая metric, который разрешён всем четырём известным
// типам. Без него пара (key_scope, metric) выпала бы, и отказ ключу с
// незаданным типом на /v1/metrics не считался бы нигде.
func keyScopeRejectionPairs() []IngestRejectionKey {
	kinds := []org.KeyKind{org.KindBrowser, org.KindServer, org.KindAgent, org.KindLegacy, ""}
	out := make([]IngestRejectionKey, 0, len(allIngestSignals))
	for _, s := range allIngestSignals {
		for _, k := range kinds {
			if !scopeAllows(k, s) {
				out = append(out, IngestRejectionKey{RejectKeyScope, s})
				break
			}
		}
	}
	return out
}
