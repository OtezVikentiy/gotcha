package templates

import "sync/atomic"

// atomic.Value: параллельные тест-серверы могут писать версию, пока другой
// рендерит — простое присваивание словило бы гонку (-race).
var assetVersion atomic.Value // string

func SetAssetVersion(v string) { assetVersion.Store(v) }

func assetURL(path string) string {
	v, _ := assetVersion.Load().(string) // до первого Store — nil → "" → без версии
	if v == "" {
		return path
	}
	return path + "?v=" + v
}
