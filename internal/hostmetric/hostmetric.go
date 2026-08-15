// Package hostmetric — единственный источник правды об именах, атрибутах и
// списках исключений хост-метрик. На него смотрят ТРИ стороны: агент
// (internal/agent — что эмитить), потребители (internal/web/hostcharts,
// internal/host/evaluator — что читать) и генератор YAML коллектора
// (internal/web/hosts.go). Лист-пакет без зависимостей: web не должен тянуть
// gopsutil, агент не должен тянуть web.
package hostmetric

// Имена метрик — OTel semconv, публичный контракт (фиксируются набело).
const (
	CPUUtilization        = "system.cpu.utilization"
	CPULogicalCount       = "system.cpu.logical.count"
	MemoryUtilization     = "system.memory.utilization"
	FilesystemUtilization = "system.filesystem.utilization"
	DiskIO                = "system.disk.io"
	NetworkIO             = "system.network.io"
	LoadAvg1m             = "system.cpu.load_average.1m"
	LoadAvg5m             = "system.cpu.load_average.5m"
	LoadAvg15m            = "system.cpu.load_average.15m"
	ProcessesCount        = "system.processes.count"
	Uptime                = "system.uptime"
)

// Атрибуты датапойнтов (semconv hostmetrics).
const (
	AttrState      = "state"
	AttrDevice     = "device"
	AttrDirection  = "direction"
	AttrMountpoint = "mountpoint"
	AttrFSType     = "type"
	AttrFSMode     = "mode"
	AttrStatus     = "status"
)

// Служебные resource-атрибуты агента: в ClickHouse не попадают никогда
// (сегодня — автоматически: metric.MapOTLP промотирует лишь три ключа и
// остальные ресурсные атрибуты отбрасывает; правило фиксируем на будущее).
const (
	AgentVersionAttr = "gotcha.agent.version"
	AgentAttrPrefix  = "gotcha.agent."
)

// ExcludedFSTypes — типы ФС, которые не считаются «диском» (A1, ревью I1):
// squashfs заполнен на 100% по замыслу, tmpfs — ОЗУ, overlay — слои поверх
// уже посчитанного корня, остальное — псевдо-ФС ядра.
var ExcludedFSTypes = []string{
	"autofs", "binfmt_misc", "bpf", "cgroup", "cgroup2", "configfs",
	"debugfs", "devpts", "devtmpfs", "efivarfs", "fusectl", "hugetlbfs",
	"iso9660", "mqueue", "nsfs", "overlay", "proc", "pstore", "ramfs",
	"securityfs", "squashfs", "sysfs", "tmpfs", "tracefs",
}

// ExcludedMountPrefixes — префиксы точек монтирования вне мониторинга.
// Агент матчит их strings.HasPrefix; YAML коллектора рендерит "^<префикс>.*".
var ExcludedMountPrefixes = []string{
	"/snap/", "/var/lib/docker/", "/var/lib/kubelet/",
	"/run/", "/dev/", "/proc/", "/sys/",
}

// AllMetrics — полный набор эмиссии агента (порядок стабилен для тестов).
func AllMetrics() []string {
	return []string{
		CPUUtilization, CPULogicalCount, MemoryUtilization,
		FilesystemUtilization, DiskIO, NetworkIO,
		LoadAvg1m, LoadAvg5m, LoadAvg15m, ProcessesCount, Uptime,
	}
}
