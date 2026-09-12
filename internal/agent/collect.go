package agent

import (
	"errors"
	"slices"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
)

// Агрегат по всем ядрам, секунды (cpu.Times(false) у gopsutil).
type CPUTimes struct {
	User, System, Idle, Nice, Iowait, Irq, Softirq, Steal float64
}

// До фильтрации хостовых исключений (см. filterFS).
type FSSample struct {
	Device, Mountpoint, FSType, Mode string // Mode "ro"|"rw" из реальных опций монтирования
	Utilization                      float64
}

// Счётчики since-boot, не дельта — дельту считает потребитель.
type IOBytes struct{ Read, Write uint64 }

// Счётчики since-boot (как IOBytes).
type NetBytes struct{ Recv, Sent uint64 }

type Sample struct {
	Time                 time.Time
	CPU                  map[string]float64 // state → доля 0..1; nil на первом тике (нет дельты) и при откате счётчика
	CPUCount             int
	Memory               map[string]float64 // state → доля 0..1
	Filesystems          []FSSample         // уже отфильтрованы hostmetric.Excluded*
	DiskIO               map[string]IOBytes
	NetIO                map[string]NetBytes
	Load1, Load5, Load15 float64
	Procs                map[string]int // status → count, раскладка hostmetrics
	UptimeSec            float64
	BootTime             time.Time
}

// Каждая проба независима: падение одной не требует успеха остальных.
type Probes struct {
	CPUTimes func() (CPUTimes, error)
	CPUCount func() (int, error)
	Memory   func() (map[string]float64, error) // уже в долях
	FS       func() ([]FSSample, error)         // без фильтрации — фильтрует Collect
	DiskIO   func() (map[string]IOBytes, error)
	NetIO    func() (map[string]NetBytes, error)
	LoadAvg  func() (l1, l5, l15 float64, err error)
	Procs    func() (map[string]int, error)
	Uptime   func() (float64, error)
	BootTime func() (time.Time, error)
}

// Хранит CPUTimes прошлого тика для дельты долей.
type Collector struct {
	probes  Probes
	prevCPU *CPUTimes
}

func NewCollector(p Probes) *Collector {
	return &Collector{probes: p}
}

// Ошибка одной пробы не роняет тик — секция остаётся нулевой; err только,
// если упали все пробы.
func (c *Collector) Collect(now time.Time) (Sample, error) {
	s := Sample{Time: now}
	var errs []error
	ok := 0

	if ct, err := c.probes.CPUTimes(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.CPU = c.cpuUtilization(ct)
	}

	if n, err := c.probes.CPUCount(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.CPUCount = n
	}

	if m, err := c.probes.Memory(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.Memory = m
	}

	if fs, err := c.probes.FS(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.Filesystems = filterFS(fs)
	}

	if io, err := c.probes.DiskIO(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.DiskIO = io
	}

	if nio, err := c.probes.NetIO(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.NetIO = nio
	}

	if l1, l5, l15, err := c.probes.LoadAvg(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.Load1, s.Load5, s.Load15 = l1, l5, l15
	}

	if procs, err := c.probes.Procs(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.Procs = procs
	}

	if up, err := c.probes.Uptime(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.UptimeSec = up
	}

	if bt, err := c.probes.BootTime(); err != nil {
		errs = append(errs, err)
	} else {
		ok++
		s.BootTime = bt
	}

	if ok == 0 {
		return Sample{}, errors.Join(errs...)
	}
	return s, nil
}

// Имена states — канон hostmetrics cpu-scraper, не поля gopsutil:
// Iowait→wait, Irq→interrupt, остальные без изменений.
func (c *Collector) cpuUtilization(cur CPUTimes) map[string]float64 {
	prev := c.prevCPU
	c.prevCPU = &cur
	if prev == nil {
		return nil // первый тик — дельты ещё нет
	}
	delta := map[string]float64{
		"user":      cur.User - prev.User,
		"system":    cur.System - prev.System,
		"idle":      cur.Idle - prev.Idle,
		"nice":      cur.Nice - prev.Nice,
		"wait":      cur.Iowait - prev.Iowait,
		"interrupt": cur.Irq - prev.Irq,
		"softirq":   cur.Softirq - prev.Softirq,
		"steal":     cur.Steal - prev.Steal,
	}
	total := 0.0
	for _, v := range delta {
		total += v
	}
	if total <= 0 {
		return nil // счётчик прыгнул назад (ребут/гонка) — доли не посчитать
	}
	util := make(map[string]float64, len(delta))
	for k, v := range delta {
		util[k] = v / total
	}
	return util
}

// Список исключений общий с web/hosts.go и evaluator (internal/hostmetric).
func filterFS(all []FSSample) []FSSample {
	var out []FSSample
	for _, fs := range all {
		if slices.Contains(hostmetric.ExcludedFSTypes, fs.FSType) {
			continue
		}
		if hasExcludedPrefix(fs.Mountpoint) {
			continue
		}
		out = append(out, fs)
	}
	return out
}

func hasExcludedPrefix(mountpoint string) bool {
	for _, prefix := range hostmetric.ExcludedMountPrefixes {
		if strings.HasPrefix(mountpoint, prefix) {
			return true
		}
	}
	return false
}
