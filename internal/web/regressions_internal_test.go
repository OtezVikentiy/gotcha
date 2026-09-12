package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
)

// Условие отсечения окна — «> окна», а не «>=»: деплой ровно на краю ещё привязывается.
func TestNearestPrecedingDeployWindowBoundary(t *testing.T) {
	started := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	onEdge := deploy.Deployment{Version: "v-edge", DeployedAt: started.Add(-regressionDeployWindow)}
	tooOld := deploy.Deployment{Version: "v-old", DeployedAt: started.Add(-regressionDeployWindow - time.Second)}

	got, ok := nearestPrecedingDeploy([]deploy.Deployment{onEdge}, started)
	if !ok || got.Version != "v-edge" {
		t.Fatalf("деплой ровно на границе 7д должен привязаться: got=%+v ok=%v", got, ok)
	}

	if _, ok := nearestPrecedingDeploy([]deploy.Deployment{tooOld}, started); ok {
		t.Fatalf("деплой старше окна на секунду не должен привязываться")
	}

	future := deploy.Deployment{Version: "v-future", DeployedAt: started.Add(time.Minute)}
	if _, ok := nearestPrecedingDeploy([]deploy.Deployment{future}, started); ok {
		t.Fatalf("деплой после начала регрессии не предшествует ей")
	}
}
