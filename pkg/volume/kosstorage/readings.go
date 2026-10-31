package kosstorage

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
	volumeapi "github.com/tsic404/kos/pkg/volume"
)

// storageClasses is the native class list a storage policy is read from.
var storageClasses = schema.GroupVersionResource{
	Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses",
}

// UsageSource reports the sampled fill of the volumes a page shows (C-49). A
// distribution with no volume metric reader leaves it nil and the item states
// that instead of presenting allocated capacity as usage.
type UsageSource interface {
	// Usage returns one sample per volume.
	Usage(ctx context.Context) ([]VolumeUsage, error)
}

// VolumeUsage is one volume's sampled fill.
type VolumeUsage struct {
	// Namespace is the volume's namespace.
	Namespace string `json:"namespace"`
	// Name is the volume name.
	Name string `json:"name"`
	// Used is the sampled used size.
	Used string `json:"used"`
	// Capacity is the size the claim reports.
	Capacity string `json:"capacity"`
}

// Readings resolves the items the storage page declares (C-53).
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// Usage reports volume fill samples; nil leaves the item stating no source
	// is configured.
	Usage UsageSource
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemDevicesAndPolicies:
		return r.devicesAndPolicies(ctx)
	case ItemVolumes:
		return r.volumes(ctx)
	case ItemUsage:
		return r.usage(ctx)
	case ItemBackups:
		return r.backups(ctx)
	case ItemMigration:
		return r.migrationTargets(ctx)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// deviceView is one device as the page shows it.
type deviceView struct {
	Node               string `json:"node"`
	Name               string `json:"name"`
	Serial             string `json:"serial,omitempty"`
	Model              string `json:"model,omitempty"`
	Kind               string `json:"kind,omitempty"`
	Present            bool   `json:"present"`
	Recognized         bool   `json:"recognized"`
	DriverReady        bool   `json:"driverReady"`
	Published          bool   `json:"published"`
	Occupied           bool   `json:"occupied"`
	OccupancySource    string `json:"occupancySource,omitempty"`
	OccupancyReference string `json:"occupancyReference,omitempty"`
	Stale              bool   `json:"stale"`
	Invalid            bool   `json:"invalid"`
	Allocatable        bool   `json:"allocatable"`
}

// storageClassView is one native storage class as the page shows it.
type storageClassView struct {
	Name                 string `json:"name"`
	Provisioner          string `json:"provisioner,omitempty"`
	ReclaimPolicy        string `json:"reclaimPolicy,omitempty"`
	AllowVolumeExpansion bool   `json:"allowVolumeExpansion"`
	VolumeBindingMode    string `json:"volumeBindingMode,omitempty"`
}

// devicesAndPolicies reads the device facts and the storage classes a volume
// resolves against (C-49).
func (r *Readings) devicesAndPolicies(ctx context.Context) (any, *controlplane.Error) {
	facts, failure := r.reader().List(ctx, "NodeDevice", "")
	if failure != nil {
		return nil, failure
	}
	objects := make([]*unstructured.Unstructured, 0, len(facts))
	for _, fact := range facts {
		objects = append(objects, &unstructured.Unstructured{Object: fact})
	}
	view := device.BuildView(objects, r.reader().Clock())
	devices := make([]deviceView, 0, len(view.Devices))
	for _, entry := range view.Devices {
		devices = append(devices, deviceView{
			Node: entry.Node, Name: entry.Name, Serial: entry.Serial, Model: entry.Model, Kind: entry.Kind,
			Present: entry.IsPresent, Recognized: entry.IsRecognized, DriverReady: entry.IsDriverReady,
			Published: entry.IsPublished, Occupied: entry.IsOccupied, OccupancySource: entry.OccupancySource,
			OccupancyReference: entry.OccupancyReference, Stale: entry.IsStale, Invalid: entry.IsInvalid,
			Allocatable: entry.IsAllocatable,
		})
	}
	classes, failure := r.classes(ctx)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{"devices": devices, "warnings": view.Warnings, "storageClasses": classes}, nil
}

// classes reads the native storage classes.
func (r *Readings) classes(ctx context.Context) ([]storageClassView, *controlplane.Error) {
	list, err := r.Dynamic.Resource(storageClasses).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, controlplaneFailure(err, "storageclasses")
	}
	classes := make([]storageClassView, 0, len(list.Items))
	for index := range list.Items {
		item := list.Items[index]
		classes = append(classes, storageClassView{
			Name:                 item.GetName(),
			Provisioner:          cluster.Text(item.Object, "provisioner"),
			ReclaimPolicy:        cluster.Text(item.Object, "reclaimPolicy"),
			AllowVolumeExpansion: boolField(item.Object, "allowVolumeExpansion"),
			VolumeBindingMode:    cluster.Text(item.Object, "volumeBindingMode"),
		})
	}
	return classes, nil
}

// boolField reads one boolean field of a stored object.
func boolField(object map[string]any, path ...string) bool {
	value, _, _ := unstructured.NestedBool(object, path...)
	return value
}

// volumeView is one volume as the page shows it.
type volumeView struct {
	Namespace           string `json:"namespace"`
	Name                string `json:"name"`
	Phase               string `json:"phase"`
	Health              string `json:"health"`
	Mobility            string `json:"mobility,omitempty"`
	Capacity            string `json:"capacity,omitempty"`
	BoundClaim          string `json:"boundClaim,omitempty"`
	StorageClass        string `json:"storageClass,omitempty"`
	Provider            string `json:"provider,omitempty"`
	ProviderVersion     string `json:"providerVersion,omitempty"`
	Node                string `json:"node,omitempty"`
	ReclaimPolicy       string `json:"reclaimPolicy,omitempty"`
	SnapshotConsistency string `json:"snapshotConsistency,omitempty"`
}

// volumes reads the volume objects and their phases (C-38, C-49).
func (r *Readings) volumes(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Volume", "")
	if failure != nil {
		return nil, failure
	}
	views := make([]volumeView, 0, len(objects))
	for _, object := range objects {
		var volume v1beta1.Volume
		if failure := cluster.Decode(object, &volume); failure != nil {
			return nil, failure
		}
		views = append(views, volumeView{
			Namespace: volume.Namespace, Name: volume.Name,
			Phase: volume.Status.Phase, Health: string(volume.Status.Health.State),
			Mobility: string(volume.Spec.Mobility), Capacity: volume.Spec.Capacity,
			BoundClaim: volume.Status.BoundClaim, StorageClass: volume.Status.StorageClass,
			Provider: volume.Status.Provider.Name, ProviderVersion: volume.Status.Provider.Version,
			Node: volume.Status.Node, ReclaimPolicy: volume.Status.ReclaimPolicy,
			SnapshotConsistency: volume.Status.SnapshotConsistency,
		})
	}
	return map[string]any{"volumes": views}, nil
}

// usage reports the sampled fill of the volumes (C-49). Without a metric reader
// the item says so: allocated capacity is not usage.
func (r *Readings) usage(ctx context.Context) (any, *controlplane.Error) {
	if r.Usage == nil {
		return map[string]any{
			"available": false,
			"reason":    "发行版没有接入卷使用量的指标来源，KOS 不读取卷内数据",
		}, nil
	}
	samples, err := r.Usage.Usage(ctx)
	if err != nil {
		return map[string]any{"available": false, "reason": "卷使用量读取失败：" + err.Error()}, nil
	}
	return map[string]any{"available": true, "volumes": samples}, nil
}

// backupView is one backup policy with its newest run and drill (C-41, C-42).
type backupView struct {
	Namespace    string         `json:"namespace"`
	Name         string         `json:"name"`
	Phase        string         `json:"phase"`
	Health       string         `json:"health"`
	Schedule     string         `json:"schedule,omitempty"`
	Consistency  string         `json:"consistency,omitempty"`
	TargetPath   string         `json:"targetPath,omitempty"`
	NextRunTime  string         `json:"nextRunTime,omitempty"`
	LastBackup   *backupRunView `json:"lastBackup,omitempty"`
	LastDrill    *drillView     `json:"lastDrill,omitempty"`
	DrillEnabled bool           `json:"drillEnabled"`
}

// backupRunView is the newest backup run.
type backupRunView struct {
	CompletedAt string `json:"completedAt,omitempty"`
	Consistency string `json:"consistency,omitempty"`
	Succeeded   bool   `json:"succeeded"`
	Reason      string `json:"reason,omitempty"`
}

// drillView is the newest recovery drill.
type drillView struct {
	Phase       string `json:"phase,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	Succeeded   bool   `json:"succeeded"`
	Reason      string `json:"reason,omitempty"`
}

// backups reads the backup policies with the backup and drill state C-49 shows.
func (r *Readings) backups(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "BackupPolicy", "")
	if failure != nil {
		return nil, failure
	}
	views := make([]backupView, 0, len(objects))
	for _, object := range objects {
		var policy v1beta1.BackupPolicy
		if failure := cluster.Decode(object, &policy); failure != nil {
			return nil, failure
		}
		view := backupView{
			Namespace: policy.Namespace, Name: policy.Name,
			Phase: policy.Status.Phase, Health: string(policy.Status.Health.State),
			Schedule: policy.Spec.Schedule, Consistency: string(policy.Spec.Consistency),
			TargetPath: policy.Spec.Target.Path, DrillEnabled: policy.Spec.Drill.Enabled,
		}
		if policy.Status.NextRunTime != nil {
			view.NextRunTime = policy.Status.NextRunTime.UTC().Format(time.RFC3339)
		}
		if record := policy.Status.LastBackup; record != nil {
			view.LastBackup = &backupRunView{
				CompletedAt: timeText(record.CompletedAt), Consistency: string(record.Consistency),
				Succeeded: record.Succeeded, Reason: record.Reason,
			}
		}
		if drill := policy.Status.LastDrill; drill != nil {
			view.LastDrill = &drillView{
				Phase: string(drill.Phase), CompletedAt: timeText(drill.CompletedAt),
				Succeeded: drill.Succeeded, Reason: drill.Reason,
			}
		}
		views = append(views, view)
	}
	return map[string]any{"policies": views}, nil
}

// timeText renders an optional timestamp.
func timeText(moment *metav1.Time) string {
	if moment == nil {
		return ""
	}
	return moment.UTC().Format(time.RFC3339)
}

// migrationTargets names the volumes a migration can move: a bound volume with a
// resolved policy (C-40).
func (r *Readings) migrationTargets(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Volume", "")
	if failure != nil {
		return nil, failure
	}
	targets := make([]map[string]string, 0, len(objects))
	for _, object := range objects {
		var volume v1beta1.Volume
		if failure := cluster.Decode(object, &volume); failure != nil {
			return nil, failure
		}
		if volume.Status.Phase != volumeapi.PhaseBound {
			continue
		}
		storageClass := volume.Status.StorageClass
		if storageClass == "" {
			storageClass = volume.Spec.StorageClass
		}
		targets = append(targets, map[string]string{
			"namespace": volume.Namespace, "name": volume.Name, "storageClass": storageClass,
		})
	}
	return map[string]any{"volumes": targets}, nil
}

// controlplaneFailure maps a native read failure onto the API envelope.
func controlplaneFailure(err error, resource string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeClusterUnavailable,
		Message:     "读取 " + resource + " 失败：" + err.Error(),
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-storage 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
