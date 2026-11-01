package budget

import (
	"fmt"
	"time"
)

// Budgets of C-57. Every number is declared, not read from a running process:
// the declared conditions are what a measurement run reproduces.
const (
	// SteadyCPUMilli is the control plane's steady CPU budget.
	SteadyCPUMilli int64 = 100
	// SteadyMemoryBytes is the control plane's steady process memory budget.
	SteadyMemoryBytes int64 = 256 << 20
	// InstallPeakCPUMilli is the control plane's install-peak CPU budget.
	InstallPeakCPUMilli int64 = 500
	// InstallPeakMemoryBytes is the control plane's install-peak memory budget.
	InstallPeakMemoryBytes int64 = 512 << 20
	// CollectorCPUMilli is the per-node collector's CPU budget.
	CollectorCPUMilli int64 = 10
	// CollectorMemoryBytes is the per-node collector's memory budget.
	CollectorMemoryBytes int64 = 48 << 20
	// DatabaseDiskBytes is the database disk cap: the data file plus the
	// write-ahead log.
	DatabaseDiskBytes int64 = 32 << 20

	// ReconcileP99Target is the single-round reconcile latency target. One
	// round's hard limit (30 seconds) is recorded separately.
	ReconcileP99Target = 2 * time.Second
	// ReconcileHardLimit is the separate hard limit of one round.
	ReconcileHardLimit = 30 * time.Second
	// ProjectionP99Target is the read-projection latency target.
	ProjectionP99Target = 200 * time.Millisecond
	// LatencyWindow is the window both latency targets are measured over.
	LatencyWindow = 24 * time.Hour

	// RealRetestFactor is the factor of the declared budget a real three-node
	// retest may not exceed.
	RealRetestFactor = 1.5
)

// Budget items of C-57, as a report and a verdict name them.
const (
	ItemSteadyCPU         = "control-plane-steady-cpu"
	ItemSteadyMemory      = "control-plane-steady-memory"
	ItemInstallPeakCPU    = "control-plane-install-peak-cpu"
	ItemInstallPeakMemory = "control-plane-install-peak-memory"
	ItemCollectorCPU      = "node-collector-cpu"
	ItemCollectorMemory   = "node-collector-memory"
	ItemDatabaseDisk      = "database-disk"
	ItemReconcileP99      = "reconcile-p99"
	ItemProjectionP99     = "projection-p99"
)

// Values a verdict shows for an item it could not judge (C-57).
const (
	// unmeasuredValue is the measured column of an item the run did not
	// measure: its component is not part of the run.
	unmeasuredValue = "not measured"
	// windowNotCoveredValue is the measured column of a latency whose samples
	// do not span the declared window, whether the recorder discarded some or
	// the run was simply shorter.
	windowNotCoveredValue = "window not covered"
)

// Conditions is the measurement condition of C-57: a three-node kind cluster
// with two CPUs and four GiB per node, fixed garbage-collection parameters and
// a fixture of twenty applications.
type Conditions struct {
	// Nodes is the node count.
	Nodes int `json:"nodes"`
	// NodeCPUMilli is the CPU limit of one node.
	NodeCPUMilli int64 `json:"nodeCpuMilli"`
	// NodeMemoryBytes is the memory limit of one node.
	NodeMemoryBytes int64 `json:"nodeMemoryBytes"`
	// FixtureApplications is the number of applications the fixture installs.
	FixtureApplications int `json:"fixtureApplications"`
	// LatencyWindow is the window the latency targets are measured over.
	LatencyWindow time.Duration `json:"latencyWindow"`
}

// DeclaredConditions is the condition C-57 fixes for a measurement run.
func DeclaredConditions() Conditions {
	return Conditions{
		Nodes:               3,
		NodeCPUMilli:        2000,
		NodeMemoryBytes:     4 << 30,
		FixtureApplications: 20,
		LatencyWindow:       LatencyWindow,
	}
}

// Usage is one process's resource use: CPU in millicores and memory in bytes.
type Usage struct {
	// CPUMilli is the CPU use in millicores (one thousandth of a core).
	CPUMilli int64 `json:"cpuMilli"`
	// MemoryBytes is the process memory in bytes.
	MemoryBytes int64 `json:"memoryBytes"`
}

// Report is one measurement run. IsRealCluster distinguishes the declared kind
// condition from the real three-node retest, which C-57 allows one and a half
// times the declared budget.
type Report struct {
	// Conditions is the measurement condition.
	Conditions Conditions `json:"conditions"`
	// IsRealCluster marks the real three-node retest.
	IsRealCluster bool `json:"realCluster"`
	// Steady is the control plane at steady state.
	Steady Usage `json:"steady"`
	// InstallPeak is the control plane at the installation peak.
	InstallPeak Usage `json:"installPeak"`
	// Collector is one node collector.
	Collector Usage `json:"collector"`
	// DatabaseDiskBytes is the database data file plus its write-ahead log.
	DatabaseDiskBytes int64 `json:"databaseDiskBytes"`
	// ReconcileP99 is the measured single-round reconcile latency.
	ReconcileP99 time.Duration `json:"reconcileP99"`
	// ProjectionP99 is the measured read-projection latency.
	ProjectionP99 time.Duration `json:"projectionP99"`
	// ReconcileCoverage and ProjectionCoverage are the parts of the latency
	// window the two recorders actually held. A percentile is only a
	// twenty-four hour figure while its coverage equals that window (C-57).
	ReconcileCoverage  time.Duration `json:"reconcileCoverage"`
	ProjectionCoverage time.Duration `json:"projectionCoverage"`
	// Unmeasured names the items this run did not measure, because their
	// component does not run here. Evaluate judges them undecidable: an
	// unmeasured budget is never a passing one (C-57).
	Unmeasured []string `json:"unmeasured,omitempty"`
	// Truncated names the latency items whose recorder discarded measurements
	// still inside its window, so the percentile covers less than the window.
	Truncated []string `json:"truncated,omitempty"`
}

// Finding is one budget item's judgement.
type Finding struct {
	// Item names the budget item.
	Item string `json:"item"`
	// Measured is the measured value as a display string.
	Measured string `json:"measured"`
	// Limit is the limit in force as a display string.
	Limit string `json:"limit"`
	// IsWithin reports that the measured value is inside the limit.
	IsWithin bool `json:"within"`
	// Decidable reports that the item was measured at all. An item the run did
	// not measure, or a latency whose recorder lost part of its window, is
	// undecidable: it blocks the release without being a violation (C-57).
	Decidable bool `json:"decidable"`
}

// Verdict is the judgement of one report.
type Verdict struct {
	// IsWithin is true when every finding is inside its limit.
	IsWithin bool `json:"within"`
	// Findings lists every budget item in contract order.
	Findings []Finding `json:"findings"`
}

// Evaluate judges a report against C-57. A real three-node retest is judged
// against one and a half times the declared limits; the kind-condition run is
// judged against the declared limits themselves. An item the run did not
// measure, and a latency whose recorder lost part of its window, are reported
// as undecidable and block the verdict instead of passing by construction.
func (r Report) Evaluate() Verdict {
	factor := 1.0
	if r.IsRealCluster {
		factor = RealRetestFactor
	}
	verdict := Verdict{IsWithin: true}
	judge := func(finding Finding) {
		verdict.Findings = append(verdict.Findings, finding)
		if !finding.IsWithin {
			verdict.IsWithin = false
		}
	}
	recordScaled := func(item string, value, limit int64, unit func(int64) string) {
		limitValue := scaledLimit(limit, factor)
		finding := Finding{Item: item, Measured: unit(value), Limit: unit(limitValue), IsWithin: value <= limitValue, Decidable: true}
		if contains(r.Unmeasured, item) {
			finding.Measured = unmeasuredValue
			finding.IsWithin = false
			finding.Decidable = false
		}
		judge(finding)
	}
	recordLatency := func(item string, measured, coverage, target time.Duration) {
		limitValue := time.Duration(scaledLimit(int64(target), factor))
		window := r.Conditions.LatencyWindow
		if window <= 0 {
			window = LatencyWindow
		}
		over := fmt.Sprintf("%s over %s", formatNanos(int64(measured)), coverage.Round(time.Second))
		finding := Finding{
			Item:      item,
			Measured:  over,
			Limit:     formatNanos(int64(limitValue)),
			IsWithin:  measured <= limitValue,
			Decidable: true,
		}
		switch {
		case contains(r.Unmeasured, item) || measured <= 0 || coverage <= 0:
			// No sample is not a passing measurement: a percentile nobody
			// took cannot be inside a target (C-57).
			finding.Measured = unmeasuredValue
			finding.IsWithin = false
			finding.Decidable = false
		case contains(r.Truncated, item) || coverage < window:
			// The samples do not span the declared window, so the percentile
			// is not a window figure and the release cannot be judged on it.
			finding.Measured = fmt.Sprintf("%s (%s)", over, windowNotCoveredValue)
			finding.IsWithin = false
			finding.Decidable = false
		}
		judge(finding)
	}
	recordScaled(ItemSteadyCPU, r.Steady.CPUMilli, SteadyCPUMilli, formatMilli)
	recordScaled(ItemSteadyMemory, r.Steady.MemoryBytes, SteadyMemoryBytes, formatMiB)
	recordScaled(ItemInstallPeakCPU, r.InstallPeak.CPUMilli, InstallPeakCPUMilli, formatMilli)
	recordScaled(ItemInstallPeakMemory, r.InstallPeak.MemoryBytes, InstallPeakMemoryBytes, formatMiB)
	recordScaled(ItemCollectorCPU, r.Collector.CPUMilli, CollectorCPUMilli, formatMilli)
	recordScaled(ItemCollectorMemory, r.Collector.MemoryBytes, CollectorMemoryBytes, formatMiB)
	recordScaled(ItemDatabaseDisk, r.DatabaseDiskBytes, DatabaseDiskBytes, formatMiB)
	recordLatency(ItemReconcileP99, r.ReconcileP99, r.ReconcileCoverage, ReconcileP99Target)
	recordLatency(ItemProjectionP99, r.ProjectionP99, r.ProjectionCoverage, ProjectionP99Target)
	return verdict
}

// contains reports that one item is listed.
func contains(items []string, item string) bool {
	for _, name := range items {
		if name == item {
			return true
		}
	}
	return false
}

// scaledLimit is the limit in force; the real retest gets the factor.
func scaledLimit(limit int64, factor float64) int64 {
	if factor == 1.0 {
		return limit
	}
	return int64(float64(limit) * factor)
}

// formatMilli renders millicores.
func formatMilli(value int64) string { return fmt.Sprintf("%dm", value) }

// formatMiB renders bytes as MiB.
func formatMiB(bytes int64) string { return fmt.Sprintf("%d MiB", bytes/(1<<20)) }

// formatNanos renders nanoseconds as milliseconds or seconds.
func formatNanos(nanos int64) string {
	duration := time.Duration(nanos)
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", duration.Seconds())
}
