package hostmetric

// Публичный контракт OTel semconv — фиксируются набело, менять нельзя.
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

const (
	AttrState      = "state"
	AttrDevice     = "device"
	AttrDirection  = "direction"
	AttrMountpoint = "mountpoint"
	AttrFSType     = "type"
	AttrFSMode     = "mode"
	AttrStatus     = "status"
)

// В ClickHouse не попадают никогда — MapOTLP пропускает только три ключа, остальное отбрасывается.
const (
	AgentVersionAttr = "gotcha.agent.version"
	AgentAttrPrefix  = "gotcha.agent."
)

// squashfs всегда 100% по замыслу, tmpfs — это ОЗУ, overlay — слои поверх уже посчитанного корня.
var ExcludedFSTypes = []string{
	"autofs", "binfmt_misc", "bpf", "cgroup", "cgroup2", "configfs",
	"debugfs", "devpts", "devtmpfs", "efivarfs", "fusectl", "hugetlbfs",
	"iso9660", "mqueue", "nsfs", "overlay", "proc", "pstore", "ramfs",
	"securityfs", "squashfs", "sysfs", "tmpfs", "tracefs",
}

// Агент матчит strings.HasPrefix; YAML коллектора рендерит как "^<префикс>.*".
var ExcludedMountPrefixes = []string{
	"/snap/", "/var/lib/docker/", "/var/lib/kubelet/",
	"/run/", "/dev/", "/proc/", "/sys/",
}

// Порядок стабилен — важно для тестов.
func AllMetrics() []string {
	return []string{
		CPUUtilization, CPULogicalCount, MemoryUtilization,
		FilesystemUtilization, DiskIO, NetworkIO,
		LoadAvg1m, LoadAvg5m, LoadAvg15m, ProcessesCount, Uptime,
	}
}
