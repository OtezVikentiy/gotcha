package ingest

import "gitflic.ru/otezvikentiy/gotcha/internal/org"

// Матрица ЗАКРЫТА: тип, которого здесь нет (включая пустой), не получает
// ничего — fail-closed, а не оптимистичный дефолт.
var keyScopeMatrix = map[org.KeyKind]map[IngestSignal]bool{
	org.KindBrowser: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true, SignalLog: true,
	},
	org.KindServer: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true,
		SignalLog: true, SignalProfile: true, SignalDeploy: true,
	},
	org.KindAgent: {
		SignalMetric: true,
	},
	org.KindLegacy: {
		SignalEvent: true, SignalTransaction: true, SignalMetric: true,
		SignalLog: true, SignalProfile: true, SignalDeploy: true,
	},
}

// Отдельно от keyScopeMatrix: регистрация хоста — не сигнал, а ветка внутри otlpMetrics.
var keyScopeHosts = map[org.KeyKind]bool{
	org.KindAgent:  true,
	org.KindLegacy: true,
}

// Порядок фиксирован ради стабильного порядка регистрации self-метрик.
var allIngestSignals = []IngestSignal{
	SignalEvent, SignalTransaction, SignalMetric,
	SignalProfile, SignalLog, SignalDeploy,
}

var envelopeAlsoSignals = []IngestSignal{SignalTransaction, SignalProfile}

func scopeAllows(kind org.KeyKind, signal IngestSignal) bool {
	return keyScopeMatrix[kind][signal]
}

func scopeAllowsHosts(kind org.KeyKind) bool {
	return keyScopeHosts[kind]
}

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

// Пустой тип ключа участвует в переборе наравне с зарегистрированными: без
// него пара (key_scope, metric) для незаданного типа не считалась бы нигде.
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
