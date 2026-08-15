// probes.go — DefaultProbes: реализация Probes поверх gopsutil/v4. Единственное
// место в internal/agent, которое импортирует gopsutil — весь остальной пакет
// работает через шов Probes и тестируется без реального железа.
package agent

import (
	"errors"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// DefaultProbes — боевые пробы для NewCollector. Не вызывается тестами
// Collector (недетерминированные значения реального хоста); покрыта
// TestDefaultProbesSmoke.
func DefaultProbes() Probes {
	return Probes{
		CPUTimes: func() (CPUTimes, error) {
			ts, err := cpu.Times(false) // false = агрегат по всем ядрам одной строкой
			if err != nil {
				return CPUTimes{}, err
			}
			if len(ts) == 0 {
				return CPUTimes{}, errors.New("cpu.Times: empty response")
			}
			t := ts[0]
			return CPUTimes{
				User: t.User, System: t.System, Idle: t.Idle, Nice: t.Nice,
				Iowait: t.Iowait, Irq: t.Irq, Softirq: t.Softirq, Steal: t.Steal,
			}, nil
		},
		CPUCount: func() (int, error) { return cpu.Counts(true) },
		Memory: func() (map[string]float64, error) {
			v, err := mem.VirtualMemory()
			if err != nil {
				return nil, err
			}
			if v.Total == 0 {
				return nil, errors.New("mem.VirtualMemory: Total == 0")
			}
			total := float64(v.Total)
			return map[string]float64{
				"used":               float64(v.Used) / total,
				"free":               float64(v.Free) / total,
				"cached":             float64(v.Cached) / total,
				"buffered":           float64(v.Buffers) / total,
				"slab_reclaimable":   float64(v.Sreclaimable) / total,
				"slab_unreclaimable": float64(v.Sunreclaim) / total,
			}, nil
		},
		FS: func() ([]FSSample, error) {
			parts, err := disk.Partitions(false)
			if err != nil {
				return nil, err
			}
			out := make([]FSSample, 0, len(parts))
			for _, p := range parts {
				u, err := disk.Usage(p.Mountpoint)
				if err != nil {
					continue // один недоступный раздел не должен ронять всю пробу
				}
				mode := "rw"
				for _, opt := range p.Opts {
					// Точное совпадение, не strings.Contains: фолбэк gopsutil на
					// /proc/1/mounts (когда обычный источник недоступен) отдаёт
					// суперблочные опции без нормализации, и там встречается
					// "errors=remount-ro" — оно содержит подстроку "ro", но не
					// значит «раздел смонтирован read-only». Коллектор ниже
					// сравнивает Mode целиком с "ro", поэтому и здесь нужно
					// точное совпадение самой опции.
					if opt == "ro" {
						mode = "ro"
						break
					}
				}
				out = append(out, FSSample{
					Device:      p.Device,
					Mountpoint:  p.Mountpoint,
					FSType:      p.Fstype,
					Mode:        mode,
					Utilization: u.UsedPercent / 100,
				})
			}
			return out, nil
		},
		DiskIO: func() (map[string]IOBytes, error) {
			counters, err := disk.IOCounters()
			if err != nil {
				return nil, err
			}
			out := make(map[string]IOBytes, len(counters))
			for name, c := range counters {
				out[name] = IOBytes{Read: c.ReadBytes, Write: c.WriteBytes}
			}
			return out, nil
		},
		NetIO: func() (map[string]NetBytes, error) {
			// true = все интерфейсы, включая lo: network-scraper hostmetrics по
			// умолчанию не фильтрует, и A1-коллектор уже шлёт lo — фильтр здесь
			// разошёлся бы с графиком сети из A1.
			counters, err := net.IOCounters(true)
			if err != nil {
				return nil, err
			}
			out := make(map[string]NetBytes, len(counters))
			for _, c := range counters {
				out[c.Name] = NetBytes{Recv: c.BytesRecv, Sent: c.BytesSent}
			}
			return out, nil
		},
		LoadAvg: func() (float64, float64, float64, error) {
			a, err := load.Avg()
			if err != nil {
				return 0, 0, 0, err
			}
			return a.Load1, a.Load5, a.Load15, nil
		},
		Procs: func() (map[string]int, error) {
			procs, err := process.Processes()
			if err != nil {
				return nil, err
			}
			out := make(map[string]int)
			for _, p := range procs {
				statuses, err := p.Status()
				if err != nil || len(statuses) == 0 {
					continue // процесс мог завершиться между Processes() и Status()
				}
				out[mapProcStatus(statuses[0])]++
			}
			return out, nil
		},
		Uptime: func() (float64, error) {
			u, err := host.Uptime()
			if err != nil {
				return 0, err
			}
			return float64(u), nil
		},
		BootTime: func() (time.Time, error) {
			bt, err := host.BootTime()
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(int64(bt), 0), nil
		},
	}
}

// mapProcStatus переводит слова-константы gopsutil v4 (process.go:52-66) в
// статусы hostmetrics processes-scraper: running→running, sleep→sleeping,
// blocked→blocked, stop→stopped, zombie→zombies, wait→paging, lock→locked,
// idle→idle, неизвестное→unknown.
func mapProcStatus(s string) string {
	switch s {
	case process.Running:
		return "running"
	case process.Sleep:
		return "sleeping"
	case process.Blocked:
		return "blocked"
	case process.Stop:
		return "stopped"
	case process.Zombie:
		return "zombies"
	case process.Wait:
		return "paging"
	case process.Lock:
		return "locked"
	case process.Idle:
		return "idle"
	default:
		return "unknown"
	}
}
