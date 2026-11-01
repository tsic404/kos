// Package tc97 verifies the resource budget of AC-49 and C-57: the declared
// condition and limits, a real measurement of the two latency targets, the
// control-plane memory and CPU and the database disk cap, and the one-and-a-half
// times gate of the real three-node retest.
package tc97

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/tsic404/kos/pkg/budget"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/install"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const fixtureApplications = 20

func TestTC97ResourceBudget(t *testing.T) {
	// The declared condition and every budget of C-57.
	conditions := budget.DeclaredConditions()
	if conditions.Nodes != 3 || conditions.NodeCPUMilli != 2000 || conditions.NodeMemoryBytes != 4<<30 ||
		conditions.FixtureApplications != fixtureApplications || conditions.LatencyWindow != 24*time.Hour {
		t.Fatalf("conditions = %+v, want C-57's three-node kind condition with twenty applications", conditions)
	}
	if budget.SteadyCPUMilli != 100 || budget.SteadyMemoryBytes != 256<<20 ||
		budget.InstallPeakCPUMilli != 500 || budget.InstallPeakMemoryBytes != 512<<20 ||
		budget.CollectorCPUMilli != 10 || budget.CollectorMemoryBytes != 48<<20 ||
		budget.DatabaseDiskBytes != 32<<20 ||
		budget.ReconcileP99Target != 2*time.Second || budget.ProjectionP99Target != 200*time.Millisecond ||
		budget.ReconcileHardLimit != 30*time.Second || budget.RealRetestFactor != 1.5 {
		t.Fatal("a declared budget differs from C-57")
	}

	cluster := harness.Start(t)
	// The install peak is measured while a real installation runs in its own
	// process, so both the memory peak and the CPU rate belong to the installer
	// rather than to this test process (C-57).
	installPeak := measureInstall(t, cluster)

	projectionLatency := budget.NewLatency(time.Hour, 0)
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.ProjectionLatency = projectionLatency
	})
	for index := range fixtureApplications {
		cluster.Create(t, harness.Application(fmt.Sprintf("app-%02d", index), nil))
	}

	// Read the projection route enough times to have a percentile.
	for range 40 {
		if status, body := gateway.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, nil); status != http.StatusOK {
			t.Fatalf("projection read: %d %v", status, body)
		}
	}
	projectionP99 := projectionLatency.Quantile(0.99)
	if projectionP99 == 0 {
		t.Fatal("the projection path recorded no measurement")
	}

	// Reconcile rounds are measured by the same recorder the production loop
	// fills (C-57).
	reconcileLatency := budget.NewLatency(time.Hour, 0)
	rounds := 0
	runner, err := reconcile.NewController(reconciler{rounds: &rounds}, reconcile.Options{Latency: reconcileLatency})
	if err != nil {
		t.Fatalf("build the reconcile loop: %v", err)
	}
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(runContext)
	}()
	for index := range 40 {
		runner.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: fmt.Sprintf("round-%02d", index)})
	}
	waitFor(t, 10*time.Second, "the reconcile rounds to run", func() bool { return reconcileLatency.Count() >= 40 })
	stop()
	<-done
	reconcileP99 := reconcileLatency.Quantile(0.99)
	if reconcileP99 == 0 {
		t.Fatal("the reconcile path recorded no measurement")
	}

	// Steady control-plane use comes from the process itself, and the database
	// cap from a real database file. The node collector is not part of this
	// binary, so its two items are reported as unmeasured: Evaluate judges them
	// undecidable instead of accepting a value nobody took (C-57).
	memoryBytes, err := budget.ProcessMemory(context.Background())
	if err != nil {
		t.Fatalf("measure process memory: %v", err)
	}
	cpuBefore, err := budget.ProcessCPU()
	if err != nil {
		t.Fatalf("measure process CPU: %v", err)
	}
	started := time.Now()
	time.Sleep(300 * time.Millisecond)
	cpuAfter, err := budget.ProcessCPU()
	if err != nil {
		t.Fatalf("measure process CPU: %v", err)
	}
	steadyCPU := budget.CPUMilli(cpuAfter-cpuBefore, time.Since(started))

	databasePath := openDatabase(t)
	databaseDisk, err := budget.DatabaseDiskSize(databasePath)
	if err != nil {
		t.Fatalf("measure the database disk: %v", err)
	}

	unmeasured := []string{budget.ItemCollectorCPU, budget.ItemCollectorMemory}
	report := budget.Report{
		Conditions:         conditions,
		Steady:             budget.Usage{CPUMilli: steadyCPU, MemoryBytes: memoryBytes},
		InstallPeak:        installPeak,
		Unmeasured:         unmeasured,
		DatabaseDiskBytes:  databaseDisk,
		ReconcileP99:       reconcileP99,
		ReconcileCoverage:  reconcileLatency.Coverage(),
		ProjectionP99:      projectionP99,
		ProjectionCoverage: projectionLatency.Coverage(),
	}
	if report.ReconcileCoverage <= 0 || report.ProjectionCoverage <= 0 {
		t.Fatalf("coverage = reconcile %s, projection %s, want the window each recorder held",
			report.ReconcileCoverage, report.ProjectionCoverage)
	}
	verdict := report.Evaluate()
	for _, finding := range verdict.Findings {
		switch finding.Item {
		case budget.ItemCollectorCPU, budget.ItemCollectorMemory:
			// The collector is not part of this binary: an unmeasured budget is
			// not a passing one (C-57).
			if finding.Decidable || finding.IsWithin {
				t.Errorf("%s = %+v, want an undecidable item", finding.Item, finding)
			}
		case budget.ItemReconcileP99, budget.ItemProjectionP99:
			// This case runs for seconds, so the recorders cover far less than
			// the declared day: the verdict must not call that a window figure.
			if finding.Decidable || finding.IsWithin {
				t.Errorf("%s = %+v, want an undecidable latency", finding.Item, finding)
			}
		default:
			if !finding.Decidable || !finding.IsWithin {
				t.Errorf("%s measured %s, limit %s", finding.Item, finding.Measured, finding.Limit)
			}
		}
	}
	// Neither an unmeasured budget nor a short latency window is a passing
	// report, although every item that was measured is inside its limit (C-57).
	if verdict.IsWithin {
		t.Fatalf("a report with an unmeasured collector and a short window passed: %+v", verdict.Findings)
	}

	// A report whose items are all measured and whose recorders cover the
	// declared window passes; both items this case does not measure still fail
	// when they are over budget, so neither is judged by construction (C-57).
	measured := report
	measured.Unmeasured = nil
	measured.Collector = budget.Usage{CPUMilli: budget.CollectorCPUMilli, MemoryBytes: budget.CollectorMemoryBytes}
	measured.ReconcileCoverage = conditions.LatencyWindow
	measured.ProjectionCoverage = conditions.LatencyWindow
	if verdict := measured.Evaluate(); !verdict.IsWithin {
		t.Fatalf("a fully measured report inside the budget failed: %+v", verdict.Findings)
	}
	over := measured
	over.InstallPeak.MemoryBytes = budget.InstallPeakMemoryBytes + 1
	over.Collector.MemoryBytes = budget.CollectorMemoryBytes + 1
	if verdict := over.Evaluate(); verdict.IsWithin {
		t.Fatalf("an over-budget install peak and collector passed: %+v", verdict.Findings)
	}
	for _, item := range []string{budget.ItemInstallPeakMemory, budget.ItemCollectorMemory} {
		if finding, found := findingOf(over.Evaluate(), item); !found || finding.IsWithin || !finding.Decidable {
			t.Fatalf("finding %q = %+v (found %v), want an over-budget finding", item, finding, found)
		}
	}

	// A recorder that had to discard measurements inside its window reports a
	// shortened window, and the verdict refuses to call its percentile a
	// twenty-four hour figure (C-57).
	narrow := budget.NewLatency(time.Hour, 4)
	for index := range 16 {
		narrow.Observe(time.Duration(index) * time.Millisecond)
	}
	if !narrow.Truncated() {
		t.Fatalf("a recorder capped at four samples kept %d without reporting truncation", narrow.Count())
	}
	truncated := measured
	truncated.Truncated = []string{budget.ItemReconcileP99}
	if verdict := truncated.Evaluate(); verdict.IsWithin {
		t.Fatalf("a truncated latency window passed: %+v", verdict.Findings)
	}
	if finding, found := findingOf(truncated.Evaluate(), budget.ItemReconcileP99); !found || finding.Decidable {
		t.Fatalf("truncated finding = %+v (found %v), want an undecidable latency", finding, found)
	}

	// A real three-node retest may reach one and a half times the declared
	// budget; one byte past it blocks the release.
	retest := measured
	retest.IsRealCluster = true
	retest.Steady.MemoryBytes = budget.SteadyMemoryBytes * 3 / 2
	if verdict := retest.Evaluate(); !verdict.IsWithin {
		t.Fatalf("a retest at one and a half times the budget failed: %+v", verdict.Findings)
	}
	retest.Steady.MemoryBytes++
	if verdict := retest.Evaluate(); verdict.IsWithin {
		t.Fatal("a retest past one and a half times the budget passed")
	}
}

// measureInstall runs one installation in its own process, so the memory peak
// and the CPU rate belong to the installer alone rather than to this test
// process, and returns the sampled peak (C-57).
func measureInstall(t *testing.T, cluster *harness.Cluster) budget.Usage {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(kubeconfigOf(t, cluster), kubeconfig); err != nil {
		t.Fatalf("write the helper kubeconfig: %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTC97InstallHelper$")
	command.Env = append(os.Environ(),
		"KOS_BUDGET_INSTALL_HELPER=1",
		"KOS_BUDGET_KUBECONFIG="+kubeconfig,
		"KOS_BUDGET_DB="+filepath.Join(t.TempDir(), "helper.db"),
		"KOS_BUDGET_REPO="+harness.RepoRoot(t),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run the install helper: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		payload, found := strings.CutPrefix(line, "BUDGET_INSTALL_RESULT ")
		if !found {
			continue
		}
		result := budget.Usage{}
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			t.Fatalf("decode the helper result %q: %v", payload, err)
		}
		if result.MemoryBytes <= 0 || result.CPUMilli <= 0 {
			t.Fatalf("the helper measured no peak: %s", payload)
		}
		return result
	}
	t.Fatalf("the helper printed no result:\n%s", output)
	return budget.Usage{}
}

// TestTC97InstallHelper is the child process that runs one installation and
// prints its own peak memory and CPU rate. It is skipped in a normal run.
func TestTC97InstallHelper(t *testing.T) {
	if os.Getenv("KOS_BUDGET_INSTALL_HELPER") != "1" {
		t.Skip("not the install helper")
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KOS_BUDGET_KUBECONFIG"))
	if err != nil {
		t.Fatalf("read the helper kubeconfig: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("build the helper dynamic client: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("build the helper kube client: %v", err)
	}
	extensionsClient, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("build the helper extension client: %v", err)
	}
	accounts, err := identity.Open(context.Background(), identity.OpenOptions{Path: os.Getenv("KOS_BUDGET_DB")})
	if err != nil {
		t.Fatalf("open the helper database: %v", err)
	}
	defer accounts.Close()
	runner, err := install.New(install.Config{
		Plan:          harness.InstallerPlan(nil),
		Dynamic:       dynamicClient,
		Kube:          kubeClient,
		APIExtensions: extensionsClient,
		Definitions:   install.CRDDirectory{FS: os.DirFS(os.Getenv("KOS_BUDGET_REPO")), Dir: "config/crd"},
		Accounts:      identity.New(accounts, identity.Config{}),
		QuietWindow:   200 * time.Millisecond,
		SettleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the helper installer: %v", err)
	}
	peak := budget.Usage{}
	var peakMemory, peakCPU atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		previousCPU, err := budget.ProcessCPU()
		if err != nil {
			return
		}
		windowStart := time.Now()
		for {
			select {
			case <-stop:
				return
			case tick := <-ticker.C:
				if memory, err := budget.ProcessMemory(context.Background()); err == nil && memory > peakMemory.Load() {
					peakMemory.Store(memory)
				}
				cpu, err := budget.ProcessCPU()
				if err != nil {
					continue
				}
				// The budget is a sustained rate, so the peak is the busiest
				// one-second window: a twenty-millisecond burst is scheduling
				// noise, not a rate the process held.
				if window := tick.Sub(windowStart); window >= time.Second {
					if milli := budget.CPUMilli(cpu-previousCPU, window); milli > peakCPU.Load() {
						peakCPU.Store(milli)
					}
					previousCPU, windowStart = cpu, tick
				}
			}
		}
	}()
	started := time.Now()
	cpuBefore, cpuErr := budget.ProcessCPU()
	_, installErr := runner.Install(context.Background())
	elapsed := time.Since(started)
	close(stop)
	<-sampled
	if installErr != nil {
		t.Fatalf("helper install: %v", installErr)
	}
	// A last partial window still counts, and the average over the whole
	// installation is the floor of the peak.
	if cpuAfter, err := budget.ProcessCPU(); cpuErr == nil && err == nil {
		if average := budget.CPUMilli(cpuAfter-cpuBefore, elapsed); average > peakCPU.Load() {
			peakCPU.Store(average)
		}
	}
	peak.CPUMilli = peakCPU.Load()
	peak.MemoryBytes = peakMemory.Load()
	payload, err := json.Marshal(peak)
	if err != nil {
		t.Fatalf("encode the helper result: %v", err)
	}
	fmt.Printf("BUDGET_INSTALL_RESULT %s\n", payload)
}

// kubeconfigOf turns the cluster's rest configuration into a kubeconfig file.
func kubeconfigOf(t *testing.T, cluster *harness.Cluster) clientcmdapi.Config {
	t.Helper()
	config := cluster.Config
	return clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"acceptance": {
				Server:                   config.Host,
				CertificateAuthorityData: config.CAData,
				InsecureSkipTLSVerify:    config.Insecure,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"acceptance": {
				ClientCertificateData: config.CertData,
				ClientKeyData:         config.KeyData,
				Token:                 config.BearerToken,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"acceptance": {Cluster: "acceptance", AuthInfo: "acceptance"},
		},
		CurrentContext: "acceptance",
	}
}

// findingOf reads one budget finding.
func findingOf(verdict budget.Verdict, item string) (budget.Finding, bool) {
	for _, finding := range verdict.Findings {
		if finding.Item == item {
			return finding, true
		}
	}
	return budget.Finding{}, false
}

// reconciler counts the rounds the loop gave it.
type reconciler struct {
	rounds *int
}

func (r reconciler) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	*r.rounds = *r.rounds + 1
	return reconcile.Result{}, nil
}

// openDatabase writes one account into a real database and returns its path.
func openDatabase(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/kos.db"
	store, err := identity.Open(context.Background(), identity.OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer store.Close()
	service := identity.New(store, identity.Config{})
	owner, err := service.BootstrapOwner(context.Background(), "owner")
	if err != nil {
		t.Fatalf("bootstrap the owner: %v", err)
	}
	if !owner.Created {
		t.Fatal("the owner already existed")
	}
	return path
}

// waitFor waits until check holds.
func waitFor(t *testing.T, timeout time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}
