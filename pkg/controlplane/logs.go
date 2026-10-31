package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// Log read bounds (C-64): every read has a line and a time-window upper bound.
// A request above either is clamped to the bound, so a caller never widens the
// read past what the panel shows.
const (
	defaultLogLines  = 200
	maxLogLines      = 1000
	defaultLogWindow = 15 * time.Minute
	maxLogWindow     = time.Hour
)

// LogSource reads the log lines of one pod container (C-64). NewClusterLogs is
// the production implementation; a test substitutes its own, because the log
// endpoint belongs to a kubelet the control plane does not run.
type LogSource interface {
	// Lines returns at most limit lines of one container written at or after
	// since, oldest first.
	Lines(ctx context.Context, namespace, pod, container string, since time.Time, limit int) ([]LogLine, error)
}

// LogLine is one line of one container's log.
type LogLine struct {
	// Pod is the pod the line came from.
	Pod string `json:"pod"`
	// Container is the container the line came from.
	Container string `json:"container"`
	// Timestamp is when the pod wrote the line.
	Timestamp time.Time `json:"timestamp"`
	// Message is the line without its timestamp prefix.
	Message string `json:"message"`
}

// LogRead is one application's log read (C-64): read when the request runs, so
// it is stored nowhere and changes no object.
type LogRead struct {
	// Application is the application the read is locked to.
	Application ObjectRef `json:"application"`
	// ObservedAt is when the read ran (C-13).
	ObservedAt time.Time `json:"observedAt"`
	// Window is the time window the read used.
	Window string `json:"window"`
	// Limit is the line bound that applied to the read.
	Limit int `json:"limit"`
	// Lines are the returned lines, oldest first.
	Lines []LogLine `json:"lines"`
	// Truncated marks a read whose content exceeded the line bound.
	Truncated bool `json:"truncated"`
	// TruncatedAt is the timestamp of the oldest returned line when the bound
	// cut content: the position the read starts at.
	TruncatedAt *time.Time `json:"truncatedAt,omitempty"`
}

// ClusterLogs reads pod logs through the cluster's log endpoint.
type ClusterLogs struct {
	// Kube is the typed client the log endpoint is reached through; required.
	Kube kubernetes.Interface
}

// NewClusterLogs reads pod logs through the cluster.
func NewClusterLogs(kube kubernetes.Interface) LogSource { return ClusterLogs{Kube: kube} }

// Lines implements LogSource.
func (l ClusterLogs) Lines(ctx context.Context, namespace, pod, container string, since time.Time, limit int) ([]LogLine, error) {
	tail := int64(limit)
	options := &corev1.PodLogOptions{Container: container, Timestamps: true, TailLines: &tail}
	if !since.IsZero() {
		options.SinceTime = &metav1.Time{Time: since}
	}
	stream, err := l.Kube.CoreV1().Pods(namespace).GetLogs(pod, options).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	lines := make([]LogLine, 0, limit)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		stamp, message := splitLogLine(scanner.Text())
		lines = append(lines, LogLine{Pod: pod, Container: container, Timestamp: stamp, Message: message})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// splitLogLine separates the timestamp a kubelet prefixes from the message. A
// line without one keeps its text and reports no time.
func splitLogLine(line string) (time.Time, string) {
	head, rest, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}, line
	}
	stamp, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		return time.Time{}, line
	}
	return stamp, rest
}

// logTarget is one container a log read collects.
type logTarget struct {
	Namespace string
	Pod       string
	Container string
}

// workloadClass maps a rendered workload kind to the class the cluster serves it
// in. Only kinds a package may render as a workload take part (C-64).
var workloadClass = map[string]ObjectType{
	"Deployment":            {Group: "apps", Version: "v1", Kind: "Deployment", Resource: "deployments", Namespaced: true},
	"StatefulSet":           {Group: "apps", Version: "v1", Kind: "StatefulSet", Resource: "statefulsets", Namespaced: true},
	"ReplicaSet":            {Group: "apps", Version: "v1", Kind: "ReplicaSet", Resource: "replicasets", Namespaced: true},
	"DaemonSet":             {Group: "apps", Version: "v1", Kind: "DaemonSet", Resource: "daemonsets", Namespaced: true},
	"ReplicationController": {Version: "v1", Kind: "ReplicationController", Resource: "replicationcontrollers", Namespaced: true},
	"Job":                   {Group: "batch", Version: "v1", Kind: "Job", Resource: "jobs", Namespaced: true},
	"CronJob":               {Group: "batch", Version: "v1", Kind: "CronJob", Resource: "cronjobs", Namespaced: true},
	"Pod":                   {Version: "v1", Kind: "Pod", Resource: "pods", Namespaced: true},
}

// handleApplicationLogs reads one application's logs (C-64): the read is locked
// to the application's rendered workloads, bounded in lines and time, and
// judged by C-59's visibility.
func (s *Server) handleApplicationLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, notFound("没有这个接口"))
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	objectType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		writeError(w, failure)
		return
	}
	ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name}
	observed, err := s.source.Get(r.Context(), objectType, namespace, name)
	if err != nil {
		writeError(w, s.clusterReadFailure(err, ref))
		return
	}
	// A session that may not see the application reads no logs of it, and is
	// answered as if the application were absent (C-59).
	if _, failure := s.authorize(r.Context(), VerbRead, ref, observed.Object, nil); failure != nil {
		writeError(w, failure)
		return
	}
	limit, window, failure := logBoundsFromQuery(r.URL.Query().Get("limit"), r.URL.Query().Get("window"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	targets, err := s.applicationLogTargets(r.Context(), observed.Object)
	if err != nil {
		writeError(w, s.clusterReadFailure(err, ref))
		return
	}
	now := s.now()
	cutoff := now.Add(-window)
	lines := make([]LogLine, 0)
	for _, target := range targets {
		read, err := s.logs.Lines(r.Context(), target.Namespace, target.Pod, target.Container, cutoff, limit+1)
		if apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) {
			// A container that has not started or already rotated its log file
			// has nothing to read; that is not the cluster being unreadable.
			s.log.Info("log unavailable for one container", "pod", target.Pod, "container", target.Container, "reason", err)
			continue
		}
		if err != nil {
			writeError(w, s.clusterReadFailure(err, ref))
			return
		}
		for _, line := range read {
			// A line the source did not timestamp cannot be placed inside the
			// window the read promises, so it is not returned: the window bound
			// is what the read is judged by (C-64).
			if line.Timestamp.IsZero() || line.Timestamp.Before(cutoff) {
				continue
			}
			if line.Pod == "" {
				line.Pod = target.Pod
			}
			if line.Container == "" {
				line.Container = target.Container
			}
			lines = append(lines, line)
		}
	}
	sortLogLines(lines)
	result := LogRead{Application: ref, ObservedAt: now, Window: window.String(), Limit: limit}
	// The bound keeps the newest lines; the oldest one that stayed names where
	// the read was cut (C-64).
	if len(lines) > limit {
		result.Truncated = true
		lines = lines[len(lines)-limit:]
		if !lines[0].Timestamp.IsZero() {
			stamp := lines[0].Timestamp
			result.TruncatedAt = &stamp
		}
	}
	result.Lines = lines
	if result.Lines == nil {
		result.Lines = []LogLine{}
	}
	writeJSON(w, http.StatusOK, result)
}

// applicationLogTargets locks a read to the application's own workloads (C-64):
// it walks status.rendered, reads each workload's pod selector and lists the
// pods that selector matches in the workload's namespace. A pod of another
// application in the same namespace carries other labels and is not returned.
func (s *Server) applicationLogTargets(ctx context.Context, application map[string]any) ([]logTarget, error) {
	namespace := nestedText(application, "spec", "namespace")
	if namespace == "" {
		namespace = nestedText(application, "metadata", "namespace")
	}
	rendered := renderedRefs(application)
	targets := make([]logTarget, 0, len(rendered))
	seen := make(map[string]bool)
	pods := make(map[string][]ObservedObject)
	for _, ref := range app.WorkloadRefs(rendered) {
		objectType, known := workloadClass[ref.Kind]
		if !known {
			continue
		}
		if ref.Version != "" {
			objectType.Version = ref.Version
		}
		workloadNamespace := ref.Namespace
		if workloadNamespace == "" {
			workloadNamespace = namespace
		}
		observed, err := s.source.Get(ctx, objectType, workloadNamespace, ref.Name)
		if apierrors.IsNotFound(err) {
			// A workload that is gone has no pods to read.
			continue
		}
		if err != nil {
			return nil, err
		}
		selected, err := s.workloadPods(ctx, objectType.Kind, observed.Object, workloadNamespace, pods)
		if err != nil {
			return nil, err
		}
		for _, pod := range selected {
			for _, container := range podContainers(pod.Object) {
				key := workloadNamespace + "/" + pod.Name + "/" + container
				if seen[key] {
					continue
				}
				seen[key] = true
				targets = append(targets, logTarget{Namespace: workloadNamespace, Pod: pod.Name, Container: container})
			}
		}
	}
	sort.Slice(targets, func(left, right int) bool {
		if targets[left].Pod != targets[right].Pod {
			return targets[left].Pod < targets[right].Pod
		}
		if targets[left].Container != targets[right].Container {
			return targets[left].Container < targets[right].Container
		}
		return targets[left].Namespace < targets[right].Namespace
	})
	return targets, nil
}

// selectedPod is one pod a workload's selector matched.
type selectedPod struct {
	Name   string
	Object map[string]any
}

// workloadPods lists the pods one workload owns, through the selector the
// workload declares. A pod-kind workload is its own pod. The namespace lists
// are shared across workloads of one read, so one namespace is read once.
func (s *Server) workloadPods(ctx context.Context, kind string, workload map[string]any, namespace string, cache map[string][]ObservedObject) ([]selectedPod, error) {
	if kind == "Pod" {
		name := nestedText(workload, "metadata", "name")
		if name == "" {
			return nil, nil
		}
		return []selectedPod{{Name: name, Object: workload}}, nil
	}
	selector, err := workloadSelector(kind, workload)
	if err != nil {
		return nil, err
	}
	if selector == nil || selector.Empty() {
		return nil, nil
	}
	listed, cached := cache[namespace]
	if !cached {
		listed, err = s.source.List(ctx, workloadClass["Pod"], namespace)
		if err != nil {
			return nil, err
		}
		cache[namespace] = listed
	}
	matched := make([]selectedPod, 0, len(listed))
	for _, pod := range listed {
		metadata, _ := pod.Object["metadata"].(map[string]any)
		if !selector.Matches(labels.Set(stringMap(metadata["labels"]))) {
			continue
		}
		matched = append(matched, selectedPod{Name: text(metadata["name"]), Object: pod.Object})
	}
	return matched, nil
}

// workloadSelector reads the pod selector one workload kind declares. A
// CronJob carries it on the job template, a ReplicationController as a plain
// map; every other kind carries the standard label selector.
func workloadSelector(kind string, workload map[string]any) (labels.Selector, error) {
	switch kind {
	case "ReplicationController":
		value, found, err := unstructured.NestedFieldNoCopy(workload, "spec", "selector")
		if err != nil || !found {
			return nil, err
		}
		return labels.SelectorFromSet(stringMap(value)), nil
	case "CronJob":
		value, found, err := unstructured.NestedFieldNoCopy(workload, "spec", "jobTemplate", "spec", "template", "metadata", "labels")
		if err != nil || !found {
			return nil, err
		}
		return labels.SelectorFromSet(stringMap(value)), nil
	}
	value, found, err := unstructured.NestedFieldNoCopy(workload, "spec", "selector")
	if err != nil || !found {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var declared metav1.LabelSelector
	if err := json.Unmarshal(encoded, &declared); err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(&declared)
}

// podContainers lists the containers a pod declares, in spec order.
func podContainers(pod map[string]any) []string {
	containers, _, _ := unstructured.NestedSlice(pod, "spec", "containers")
	names := make([]string, 0, len(containers))
	for _, entry := range containers {
		container, _ := entry.(map[string]any)
		if name := text(container["name"]); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// renderedRefs reads the objects the last applied render wrote (C-33).
func renderedRefs(application map[string]any) []v1beta1.ObjectRef {
	items, _, _ := unstructured.NestedSlice(application, "status", "rendered")
	refs := make([]v1beta1.ObjectRef, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		refs = append(refs, v1beta1.ObjectRef{
			Group:     text(entry["group"]),
			Version:   text(entry["version"]),
			Kind:      text(entry["kind"]),
			Namespace: text(entry["namespace"]),
			Name:      text(entry["name"]),
		})
	}
	return refs
}

// sortLogLines orders one read oldest first, so the newest lines are the ones a
// bound keeps. Every line here carries a timestamp, because the window filter
// dropped the untimed ones; the identity fields break ties so one read is
// deterministic.
func sortLogLines(lines []LogLine) {
	sort.SliceStable(lines, func(left, right int) bool {
		if !lines[left].Timestamp.Equal(lines[right].Timestamp) {
			return lines[left].Timestamp.Before(lines[right].Timestamp)
		}
		if lines[left].Pod != lines[right].Pod {
			return lines[left].Pod < lines[right].Pod
		}
		if lines[left].Container != lines[right].Container {
			return lines[left].Container < lines[right].Container
		}
		return lines[left].Message < lines[right].Message
	})
}

// logBoundsFromQuery reads the line and window bounds of one request (C-64).
// A request above a bound is clamped to it; a value the model rejects is a
// field failure.
func logBoundsFromQuery(limitRaw, windowRaw string) (int, time.Duration, *Error) {
	limit := defaultLogLines
	if limitRaw != "" {
		parsed, err := strconv.Atoi(limitRaw)
		if err != nil || parsed < 1 {
			return 0, 0, invalidField("limit 必须是正整数")
		}
		limit = parsed
	}
	if limit > maxLogLines {
		limit = maxLogLines
	}
	window := defaultLogWindow
	if windowRaw != "" {
		parsed, err := time.ParseDuration(windowRaw)
		if err != nil || parsed <= 0 {
			return 0, 0, invalidField("window 必须是正的时间段")
		}
		window = parsed
	}
	if window > maxLogWindow {
		window = maxLogWindow
	}
	return limit, window, nil
}
