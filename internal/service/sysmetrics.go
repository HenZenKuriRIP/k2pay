package service

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/HenZenKuriRIP/k2pay/internal/model"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

var (
	processStartTime = time.Now()
	metricsOnce      sync.Once
	selfProcess      *process.Process
)

func initSelfProcess() {
	metricsOnce.Do(func() {
		p, err := process.NewProcess(int32(os.Getpid()))
		if err == nil {
			selfProcess = p
		}
	})
}

// GetSystemMetrics 采集本机与进程运行指标（仪表盘监控）
func GetSystemMetrics() map[string]interface{} {
	initSelfProcess()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	out := map[string]interface{}{
		"time":         time.Now().Format(time.RFC3339),
		"go_version":   runtime.Version(),
		"go_os":        runtime.GOOS,
		"go_arch":      runtime.GOARCH,
		"num_cpu":      runtime.NumCPU(),
		"goroutines":   runtime.NumGoroutine(),
		"uptime_sec":   int64(time.Since(processStartTime).Seconds()),
		"uptime_human": formatDuration(time.Since(processStartTime)),
		"pid":          os.Getpid(),
	}

	// 进程内存（Go runtime）
	out["process"] = map[string]interface{}{
		"alloc_mb":       bytesToMB(ms.Alloc),
		"total_alloc_mb": bytesToMB(ms.TotalAlloc),
		"sys_mb":         bytesToMB(ms.Sys),
		"heap_inuse_mb":  bytesToMB(ms.HeapInuse),
		"heap_sys_mb":    bytesToMB(ms.HeapSys),
		"stack_inuse_mb": bytesToMB(ms.StackInuse),
		"gc_num":         ms.NumGC,
		"gc_pause_ns":    ms.PauseNs[(ms.NumGC+255)%256],
		"next_gc_mb":     bytesToMB(ms.NextGC),
	}

	// 主机信息
	if hi, err := host.Info(); err == nil && hi != nil {
		out["host"] = map[string]interface{}{
			"hostname":         hi.Hostname,
			"os":               hi.OS,
			"platform":         hi.Platform,
			"platform_version": hi.PlatformVersion,
			"kernel_version":   hi.KernelVersion,
			"uptime_sec":       hi.Uptime,
			"uptime_human":     formatDuration(time.Duration(hi.Uptime) * time.Second),
		}
	}

	// CPU
	cpuPercent := 0.0
	if percents, err := cpu.Percent(0, false); err == nil && len(percents) > 0 {
		cpuPercent = percents[0]
	}
	cpuCount, _ := cpu.Counts(true)
	out["cpu"] = map[string]interface{}{
		"percent":   round1(cpuPercent),
		"cores":     cpuCount,
		"logical":   runtime.NumCPU(),
	}

	// 进程自身 CPU（若可取）
	if selfProcess != nil {
		if pcpu, err := selfProcess.CPUPercent(); err == nil {
			out["cpu"].(map[string]interface{})["process_percent"] = round1(pcpu)
		}
		if pmem, err := selfProcess.MemoryInfo(); err == nil && pmem != nil {
			out["process"].(map[string]interface{})["rss_mb"] = bytesToMB(pmem.RSS)
			out["process"].(map[string]interface{})["vms_mb"] = bytesToMB(pmem.VMS)
		}
	}

	// 系统内存
	if vm, err := mem.VirtualMemory(); err == nil && vm != nil {
		out["memory"] = map[string]interface{}{
			"total_mb":     bytesToMB(vm.Total),
			"used_mb":      bytesToMB(vm.Used),
			"available_mb": bytesToMB(vm.Available),
			"free_mb":      bytesToMB(vm.Free),
			"used_percent": round1(vm.UsedPercent),
		}
	}

	// 负载
	if avg, err := load.Avg(); err == nil && avg != nil {
		out["load"] = map[string]interface{}{
			"load1":  round2(avg.Load1),
			"load5":  round2(avg.Load5),
			"load15": round2(avg.Load15),
		}
	}

	// 磁盘（数据目录或根分区）
	path := "/"
	if runtime.GOOS == "windows" {
		path = "C:\\"
	}
	// 优先使用配置的数据目录所在分区
	// 这里简单检测根分区即可
	if usage, err := disk.Usage(path); err == nil && usage != nil {
		out["disk"] = map[string]interface{}{
			"path":         path,
			"total_gb":     bytesToGB(usage.Total),
			"used_gb":      bytesToGB(usage.Used),
			"free_gb":      bytesToGB(usage.Free),
			"used_percent": round1(usage.UsedPercent),
		}
	}

	// 数据库连接池
	if stats := model.GetDBStats(); stats != nil {
		out["database"] = stats
	}

	// 综合健康评分（0-100，仅作 UI 提示）
	out["health_score"] = calcHostHealthScore(out)
	return out
}

func calcHostHealthScore(m map[string]interface{}) int {
	score := 100
	if cpuM, ok := m["cpu"].(map[string]interface{}); ok {
		if p, ok := cpuM["percent"].(float64); ok {
			switch {
			case p > 95:
				score -= 35
			case p > 85:
				score -= 25
			case p > 70:
				score -= 15
			case p > 50:
				score -= 5
			}
		}
	}
	if memM, ok := m["memory"].(map[string]interface{}); ok {
		if p, ok := memM["used_percent"].(float64); ok {
			switch {
			case p > 95:
				score -= 30
			case p > 85:
				score -= 20
			case p > 75:
				score -= 10
			case p > 60:
				score -= 4
			}
		}
	}
	if diskM, ok := m["disk"].(map[string]interface{}); ok {
		if p, ok := diskM["used_percent"].(float64); ok {
			switch {
			case p > 95:
				score -= 25
			case p > 90:
				score -= 15
			case p > 80:
				score -= 8
			}
		}
	}
	if g, ok := m["goroutines"].(int); ok && g > 5000 {
		score -= 10
	}
	if score < 0 {
		score = 0
	}
	return score
}

func bytesToMB(b uint64) float64 {
	return round1(float64(b) / 1024 / 1024)
}

func bytesToGB(b uint64) float64 {
	return round2(float64(b) / 1024 / 1024 / 1024)
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int64(d.Seconds())
	days := sec / 86400
	sec %= 86400
	hours := sec / 3600
	sec %= 3600
	mins := sec / 60
	sec %= 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, mins, sec)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, sec)
	}
	return fmt.Sprintf("%ds", sec)
}
