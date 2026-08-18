package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
)

// TestNearestPrecedingDeployWindowBoundary — граница окна привязки регрессии к
// деплою ровно на regressionDeployWindow (7д): деплой РОВНО на краю окна
// привязывается (started - deployed_at == окно, условие «> окна» его не
// отбрасывает), а на секунду старше — уже нет. Чистая функция, без БД.
func TestNearestPrecedingDeployWindowBoundary(t *testing.T) {
	started := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	onEdge := deploy.Deployment{Version: "v-edge", DeployedAt: started.Add(-regressionDeployWindow)}
	tooOld := deploy.Deployment{Version: "v-old", DeployedAt: started.Add(-regressionDeployWindow - time.Second)}

	// Деплой ровно на границе окна — привязывается.
	got, ok := nearestPrecedingDeploy([]deploy.Deployment{onEdge}, started)
	if !ok || got.Version != "v-edge" {
		t.Fatalf("деплой ровно на границе 7д должен привязаться: got=%+v ok=%v", got, ok)
	}

	// Деплой на секунду старше границы — не привязывается.
	if _, ok := nearestPrecedingDeploy([]deploy.Deployment{tooOld}, started); ok {
		t.Fatalf("деплой старше окна на секунду не должен привязываться")
	}

	// Деплой ПОЗЖЕ started (в будущем от регрессии) не предшествует ей.
	future := deploy.Deployment{Version: "v-future", DeployedAt: started.Add(time.Minute)}
	if _, ok := nearestPrecedingDeploy([]deploy.Deployment{future}, started); ok {
		t.Fatalf("деплой после начала регрессии не предшествует ей")
	}
}
