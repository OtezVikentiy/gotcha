package agent

import (
	"errors"
	"math"
	"runtime"
	"testing"
	"time"
)

// fakeProbes — детерминированные пробы для тестов Collector; DefaultProbes
// (gopsutil) тестами не вызывается — сборка недетерминирована на CI-машинах.
func fakeProbes() Probes {
	return Probes{
		CPUTimes: func() (CPUTimes, error) { return CPUTimes{User: 10, System: 5, Idle: 85}, nil },
		CPUCount: func() (int, error) { return 4, nil },
		Memory:   func() (map[string]float64, error) { return map[string]float64{"used": 0.4, "free": 0.6}, nil },
		FS: func() ([]FSSample, error) {
			return []FSSample{
				{Device: "/dev/sda1", Mountpoint: "/", FSType: "ext4", Utilization: 0.5},
				{Device: "loop0", Mountpoint: "/snap/core/1", FSType: "squashfs", Utilization: 1.0}, // режется префиксом
				{Device: "tmpfs", Mountpoint: "/tmp2", FSType: "tmpfs", Utilization: 0.1},           // режется типом
			}, nil
		},
		DiskIO:  func() (map[string]IOBytes, error) { return map[string]IOBytes{"sda": {Read: 100, Write: 200}}, nil },
		NetIO:   func() (map[string]NetBytes, error) { return map[string]NetBytes{"eth0": {Recv: 1, Sent: 2}}, nil },
		LoadAvg: func() (float64, float64, float64, error) { return 0.5, 0.4, 0.3, nil },
		Procs:   func() (map[string]int, error) { return map[string]int{"running": 2, "sleeping": 90}, nil },
		Uptime:  func() (float64, error) { return 3600, nil },
		BootTime: func() (time.Time, error) {
			return time.Unix(1000, 0), nil
		},
	}
}

func TestCollectFirstTickSkipsCPU(t *testing.T) {
	c := NewCollector(fakeProbes())
	now := time.Unix(2000, 0)
	s1, err := c.Collect(now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if s1.CPU != nil {
		t.Errorf("s1.CPU = %v, want nil (нет дельты на первом тике)", s1.CPU)
	}
	if s1.CPUCount != 4 {
		t.Errorf("s1.CPUCount = %d, want 4", s1.CPUCount)
	}
	if s1.Time != now {
		t.Errorf("s1.Time = %v, want %v", s1.Time, now)
	}
	if s1.Memory["used"] != 0.4 || s1.Memory["free"] != 0.6 {
		t.Errorf("s1.Memory = %v", s1.Memory)
	}
	if len(s1.Filesystems) != 1 || s1.Filesystems[0].Mountpoint != "/" {
		t.Errorf("s1.Filesystems = %v, want ровно один /", s1.Filesystems)
	}
	if s1.DiskIO["sda"] != (IOBytes{Read: 100, Write: 200}) {
		t.Errorf("s1.DiskIO = %v", s1.DiskIO)
	}
	if s1.NetIO["eth0"] != (NetBytes{Recv: 1, Sent: 2}) {
		t.Errorf("s1.NetIO = %v", s1.NetIO)
	}
	if s1.Load1 != 0.5 || s1.Load5 != 0.4 || s1.Load15 != 0.3 {
		t.Errorf("load = %v/%v/%v", s1.Load1, s1.Load5, s1.Load15)
	}
	if s1.Procs["running"] != 2 || s1.Procs["sleeping"] != 90 {
		t.Errorf("s1.Procs = %v", s1.Procs)
	}
	if s1.UptimeSec != 3600 {
		t.Errorf("s1.UptimeSec = %v, want 3600", s1.UptimeSec)
	}
	if !s1.BootTime.Equal(time.Unix(1000, 0)) {
		t.Errorf("s1.BootTime = %v", s1.BootTime)
	}
}

func TestCollectCPUDelta(t *testing.T) {
	probes := fakeProbes()
	tick := 0
	probes.CPUTimes = func() (CPUTimes, error) {
		tick++
		if tick == 1 {
			return CPUTimes{User: 10, System: 5, Idle: 85}, nil
		}
		return CPUTimes{User: 12, System: 5, Idle: 93}, nil // +2 user, +8 idle, всего +10с
	}
	c := NewCollector(probes)
	if _, err := c.Collect(time.Unix(2000, 0)); err != nil {
		t.Fatalf("первый Collect: %v", err)
	}
	s2, err := c.Collect(time.Unix(2010, 0))
	if err != nil {
		t.Fatalf("второй Collect: %v", err)
	}
	if s2.CPU == nil {
		t.Fatal("s2.CPU == nil, want заполненную карту")
	}
	if got := s2.CPU["user"]; math.Abs(got-0.2) > 1e-9 {
		t.Errorf("s2.CPU[user] = %v, want ≈0.2", got)
	}
	if got := s2.CPU["idle"]; math.Abs(got-0.8) > 1e-9 {
		t.Errorf("s2.CPU[idle] = %v, want ≈0.8", got)
	}
	if got := s2.CPU["system"]; math.Abs(got) > 1e-9 {
		t.Errorf("s2.CPU[system] = %v, want ≈0 (не менялось)", got)
	}
}

func TestCollectFiltersFS(t *testing.T) {
	c := NewCollector(fakeProbes())
	s, err := c.Collect(time.Unix(2000, 0))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(s.Filesystems) != 1 {
		t.Fatalf("len(Filesystems) = %d, want 1: %v", len(s.Filesystems), s.Filesystems)
	}
	if s.Filesystems[0].Mountpoint != "/" || s.Filesystems[0].FSType != "ext4" {
		t.Errorf("Filesystems[0] = %+v, want /  ext4", s.Filesystems[0])
	}
}

func TestCollectProbeErrorDoesNotKillSample(t *testing.T) {
	probes := fakeProbes()
	probes.DiskIO = func() (map[string]IOBytes, error) {
		return nil, errors.New("boom")
	}
	c := NewCollector(probes)
	s, err := c.Collect(time.Unix(2000, 0))
	if err != nil {
		t.Fatalf("Collect вернул err при частичном отказе: %v", err)
	}
	if s.DiskIO != nil {
		t.Errorf("s.DiskIO = %v, want nil", s.DiskIO)
	}
	if s.Memory == nil || s.NetIO == nil || s.Procs == nil || len(s.Filesystems) == 0 {
		t.Errorf("остальные секции должны остаться заполненными: %+v", s)
	}
}

func TestCollectAllProbesFailReturnsError(t *testing.T) {
	failCPUTimes := func() (CPUTimes, error) { return CPUTimes{}, errors.New("boom") }
	failCPUCount := func() (int, error) { return 0, errors.New("boom") }
	failMemory := func() (map[string]float64, error) { return nil, errors.New("boom") }
	failFS := func() ([]FSSample, error) { return nil, errors.New("boom") }
	failDiskIO := func() (map[string]IOBytes, error) { return nil, errors.New("boom") }
	failNetIO := func() (map[string]NetBytes, error) { return nil, errors.New("boom") }
	failLoad := func() (float64, float64, float64, error) { return 0, 0, 0, errors.New("boom") }
	failProcs := func() (map[string]int, error) { return nil, errors.New("boom") }
	failUptime := func() (float64, error) { return 0, errors.New("boom") }
	failBootTime := func() (time.Time, error) { return time.Time{}, errors.New("boom") }

	c := NewCollector(Probes{
		CPUTimes: failCPUTimes, CPUCount: failCPUCount, Memory: failMemory, FS: failFS,
		DiskIO: failDiskIO, NetIO: failNetIO, LoadAvg: failLoad, Procs: failProcs,
		Uptime: failUptime, BootTime: failBootTime,
	})
	_, err := c.Collect(time.Unix(2000, 0))
	if err == nil {
		t.Fatal("Collect с полностью упавшими пробами должен вернуть err")
	}
}

// TestDefaultProbesSmoke — единственный тест, трогающий реальный gopsutil:
// каждая проба должна отработать без ошибки на текущей машине. Значения не
// проверяем (недетерминированы), только факт успеха.
func TestDefaultProbesSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("DefaultProbes рассчитан на linux (прод-хосты агента)")
	}
	p := DefaultProbes()

	if _, err := p.CPUTimes(); err != nil {
		t.Errorf("CPUTimes: %v", err)
	}
	if _, err := p.CPUCount(); err != nil {
		t.Errorf("CPUCount: %v", err)
	}
	if _, err := p.Memory(); err != nil {
		t.Errorf("Memory: %v", err)
	}
	if _, err := p.FS(); err != nil {
		t.Errorf("FS: %v", err)
	}
	if _, err := p.DiskIO(); err != nil {
		t.Errorf("DiskIO: %v", err)
	}
	if _, err := p.NetIO(); err != nil {
		t.Errorf("NetIO: %v", err)
	}
	if _, _, _, err := p.LoadAvg(); err != nil {
		t.Errorf("LoadAvg: %v", err)
	}
	if _, err := p.Procs(); err != nil {
		t.Errorf("Procs: %v", err)
	}
	if _, err := p.Uptime(); err != nil {
		t.Errorf("Uptime: %v", err)
	}
	if _, err := p.BootTime(); err != nil {
		t.Errorf("BootTime: %v", err)
	}
}
