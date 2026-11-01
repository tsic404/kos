package budget

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcessMemory reads the resident memory of the running process in bytes. The
// budget and the degradation protection read the same measurement (C-57). The
// counter comes from the kernel, not from the Go heap: the budget is about the
// process.
func ProcessMemory(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	content, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, fmt.Errorf("read process memory: %w", err)
	}
	fields := strings.Fields(string(content))
	if len(fields) < 2 {
		return 0, fmt.Errorf("process memory counter is malformed: %q", strings.TrimSpace(string(content)))
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process memory: %w", err)
	}
	return pages * int64(os.Getpagesize()), nil
}

// ProcessCPU reads the process CPU time consumed so far. A measurement run
// converts the difference between two reads over a wall-clock window into
// millicores with CPUMilli. The counter is the kernel's user and system time of
// this process: the runtime's own CPU metric is not filled on every platform,
// and a budget that reads zero would pass by construction (C-57).
func ProcessCPU() (time.Duration, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, fmt.Errorf("read process CPU: %w", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), nil
}

// CPUMilli converts a CPU-time delta over a wall-clock window into millicores.
func CPUMilli(cpu, window time.Duration) int64 {
	if window <= 0 {
		return 0
	}
	return int64(float64(cpu) / float64(window) * 1000)
}

// DatabaseDiskSize sums the database data file and its write-ahead log, which
// is the cap C-57 declares. A missing log file contributes nothing.
func DatabaseDiskSize(databasePath string) (int64, error) {
	total := int64(0)
	for _, path := range []string{databasePath, databasePath + "-wal"} {
		info, err := os.Stat(path)
		switch {
		case err == nil:
			total += info.Size()
		case os.IsNotExist(err):
			continue
		default:
			return 0, fmt.Errorf("measure database disk %s: %w", filepath.Base(path), err)
		}
	}
	return total, nil
}
