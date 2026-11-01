package budget

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// passingReport is a report at the declared limits.
func passingReport() Report {
	return Report{
		Conditions:         DeclaredConditions(),
		Steady:             Usage{CPUMilli: SteadyCPUMilli, MemoryBytes: SteadyMemoryBytes},
		InstallPeak:        Usage{CPUMilli: InstallPeakCPUMilli, MemoryBytes: InstallPeakMemoryBytes},
		Collector:          Usage{CPUMilli: CollectorCPUMilli, MemoryBytes: CollectorMemoryBytes},
		DatabaseDiskBytes:  DatabaseDiskBytes,
		ReconcileP99:       ReconcileP99Target,
		ReconcileCoverage:  LatencyWindow,
		ProjectionP99:      ProjectionP99Target,
		ProjectionCoverage: LatencyWindow,
	}
}

func TestEvaluateDeclaredConditions(t *testing.T) {
	verdict := passingReport().Evaluate()
	if !verdict.IsWithin {
		t.Fatalf("a report at every declared limit failed: %+v", verdict.Findings)
	}
	if len(verdict.Findings) != 9 {
		t.Fatalf("findings = %d, want the nine budget items", len(verdict.Findings))
	}
	if conditions := DeclaredConditions(); conditions.Nodes != 3 || conditions.NodeCPUMilli != 2000 || conditions.NodeMemoryBytes != 4<<30 || conditions.FixtureApplications != 20 || conditions.LatencyWindow != 24*time.Hour {
		t.Fatalf("declared conditions = %+v, want C-57's three-node/twenty-application condition", conditions)
	}
}

func TestEvaluateRejectsOneOverBudget(t *testing.T) {
	report := passingReport()
	report.Steady.MemoryBytes = SteadyMemoryBytes + 1
	if verdict := report.Evaluate(); verdict.IsWithin {
		t.Fatal("a steady memory use above the declared budget passed")
	}
	report = passingReport()
	report.ReconcileP99 = ReconcileP99Target + time.Millisecond
	if verdict := report.Evaluate(); verdict.IsWithin {
		t.Fatal("a reconcile P99 above the target passed")
	}
}

// TestEvaluateUndecidableItems covers C-57's measurement honesty: an item the
// run did not measure, and a latency whose recorder lost part of its window, are
// undecidable and block the verdict instead of passing by construction.
func TestEvaluateUndecidableItems(t *testing.T) {
	unmeasured := passingReport()
	unmeasured.Unmeasured = []string{ItemCollectorCPU}
	verdict := unmeasured.Evaluate()
	if verdict.IsWithin {
		t.Fatal("a report with an unmeasured collector passed")
	}
	finding, found := findingOf(verdict, ItemCollectorCPU)
	if !found || finding.Decidable || finding.IsWithin || finding.Measured != unmeasuredValue {
		t.Fatalf("collector finding = %+v (found %v), want an undecidable item", finding, found)
	}

	truncated := passingReport()
	truncated.Truncated = []string{ItemProjectionP99}
	verdict = truncated.Evaluate()
	if verdict.IsWithin {
		t.Fatal("a truncated latency window passed")
	}
	finding, found = findingOf(verdict, ItemProjectionP99)
	if !found || finding.Decidable || finding.IsWithin {
		t.Fatalf("projection finding = %+v (found %v), want an undecidable latency", finding, found)
	}

	measured := passingReport()
	if finding, found := findingOf(measured.Evaluate(), ItemReconcileP99); !found || !finding.Decidable || !finding.IsWithin {
		t.Fatalf("a measured latency = %+v (found %v), want a decidable finding", finding, found)
	}
}

// TestEvaluateUndecidableLatency covers C-57's window claim: the report's own
// data decides, so a latency nobody sampled and one whose samples do not span
// the declared window are neither decidable nor passing.
func TestEvaluateUndecidableLatency(t *testing.T) {
	empty := passingReport()
	empty.ReconcileP99 = 0
	empty.ReconcileCoverage = 0
	finding, found := findingOf(empty.Evaluate(), ItemReconcileP99)
	if !found || finding.Decidable || finding.IsWithin {
		t.Fatalf("an unsampled latency = %+v (found %v), want an undecidable item", finding, found)
	}
	if empty.Evaluate().IsWithin {
		t.Fatal("a report with an unsampled latency passed")
	}

	short := passingReport()
	short.ProjectionP99 = time.Millisecond
	short.ProjectionCoverage = time.Minute
	finding, found = findingOf(short.Evaluate(), ItemProjectionP99)
	if !found || finding.Decidable || finding.IsWithin {
		t.Fatalf("a short-window latency = %+v (found %v), want an undecidable item", finding, found)
	}
	if short.Evaluate().IsWithin {
		t.Fatal("a report whose latency window is not covered passed")
	}
}

// findingOf reads one finding.
func findingOf(verdict Verdict, item string) (Finding, bool) {
	for _, finding := range verdict.Findings {
		if finding.Item == item {
			return finding, true
		}
	}
	return Finding{}, false
}

func TestEvaluateRealRetestFactor(t *testing.T) {
	// A real three-node retest may use one and a half times the declared
	// budget.
	retest := passingReport()
	retest.IsRealCluster = true
	retest.Steady.MemoryBytes = SteadyMemoryBytes * 3 / 2
	retest.ReconcileP99 = ReconcileP99Target * 3 / 2
	if verdict := retest.Evaluate(); !verdict.IsWithin {
		t.Fatalf("a retest at one and a half times the budget failed: %+v", verdict.Findings)
	}
	// One byte past the factor blocks the release.
	retest.Steady.MemoryBytes = SteadyMemoryBytes*3/2 + 1
	if verdict := retest.Evaluate(); verdict.IsWithin {
		t.Fatal("a retest past one and a half times the budget passed")
	}
}

func TestLatencyQuantile(t *testing.T) {
	latency := NewLatency(time.Hour, 0)
	for index := 1; index <= 100; index++ {
		latency.Observe(time.Duration(index) * time.Millisecond)
	}
	if got := latency.Quantile(0.99); got != 99*time.Millisecond {
		t.Fatalf("p99 = %s, want 99ms", got)
	}
	if got := latency.Quantile(0.5); got != 50*time.Millisecond {
		t.Fatalf("p50 = %s, want 50ms", got)
	}
	if latency.Count() != 100 {
		t.Fatalf("count = %d, want 100", latency.Count())
	}
}

// TestLatencyReportsTruncation covers C-57's window claim: a recorder that had
// to discard measurements inside its window reports the shortened window instead
// of passing the percentile off as the declared one.
func TestLatencyReportsTruncation(t *testing.T) {
	latency := NewLatency(time.Hour, 10)
	if latency.Truncated() {
		t.Fatal("an empty recorder reported truncation")
	}
	for index := range 100 {
		latency.Observe(time.Duration(index) * time.Millisecond)
	}
	if got := latency.Count(); got > 10 {
		t.Fatalf("count = %d, want the recorder bounded by its cap", got)
	}
	if !latency.Truncated() {
		t.Fatal("a recorder that discarded in-window samples reported a whole window")
	}
	if coverage := latency.Coverage(); coverage >= time.Hour {
		t.Fatalf("coverage = %s, want less than the declared window", coverage)
	}

	whole := NewLatency(time.Hour, 0)
	whole.Observe(time.Millisecond)
	if whole.Truncated() {
		t.Fatal("a recorder under its cap reported truncation")
	}
	if coverage := whole.Coverage(); coverage <= 0 || coverage >= time.Hour {
		t.Fatalf("coverage = %s, want the span the recorder holds", coverage)
	}
}

func TestLatencyDropsSamplesOutsideTheWindow(t *testing.T) {
	latency := NewLatency(10*time.Minute, 0)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	latency.now = func() time.Time { return now }
	latency.Observe(5 * time.Second)
	now = now.Add(11 * time.Minute)
	latency.Observe(50 * time.Millisecond)
	if got := latency.Quantile(0.99); got != 50*time.Millisecond {
		t.Fatalf("p99 = %s, want only the fresh sample", got)
	}
}

func TestProcessMeasurements(t *testing.T) {
	memory, err := ProcessMemory(t.Context())
	if err != nil {
		t.Fatalf("process memory: %v", err)
	}
	if memory <= 0 {
		t.Fatalf("process memory = %d, want a positive count", memory)
	}
	cpu, err := ProcessCPU()
	if err != nil {
		t.Fatalf("process CPU: %v", err)
	}
	if cpu < 0 {
		t.Fatalf("process CPU = %s, want a non-negative time", cpu)
	}
	if rate := CPUMilli(50*time.Millisecond, 500*time.Millisecond); rate != 100 {
		t.Fatalf("CPU rate = %dm, want 100m", rate)
	}
}

func TestDatabaseDiskSize(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "kos.db")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatalf("write database: %v", err)
	}
	if err := os.WriteFile(path+"-wal", make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	size, err := DatabaseDiskSize(path)
	if err != nil {
		t.Fatalf("database disk size: %v", err)
	}
	if size != 3072 {
		t.Fatalf("database disk = %d, want 3072", size)
	}
	// A database without a write-ahead log still measures.
	missing := filepath.Join(directory, "other.db")
	if err := os.WriteFile(missing, make([]byte, 10), 0o600); err != nil {
		t.Fatalf("write second database: %v", err)
	}
	if size, err := DatabaseDiskSize(missing); err != nil || size != 10 {
		t.Fatalf("database disk = %d (%v), want 10", size, err)
	}
}
