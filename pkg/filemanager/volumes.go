package filemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// VolumeRef names one volume inside its namespace.
type VolumeRef struct {
	// Namespace is the volume's namespace.
	Namespace string
	// Name is the volume's name.
	Name string
}

// String renders the reference as the setting stores it.
func (r VolumeRef) String() string { return r.Namespace + "/" + r.Name }

// ParseExposedVolumes reads the exposed-volume setting value. An empty value
// exposes nothing; any other value has to be a JSON list of namespace/name
// references, because a mistyped setting must not fall back to exposing every
// volume.
func ParseExposedVolumes(raw string) ([]VolumeRef, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var entries []string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("暴露的卷设置不是 JSON 字符串列表：%w", err)
	}
	refs := make([]VolumeRef, 0, len(entries))
	for index, entry := range entries {
		namespace, name, found := strings.Cut(entry, "/")
		if !found || namespace == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("暴露的卷条目 %d 不是 namespace/name：%q", index, entry)
		}
		refs = append(refs, VolumeRef{Namespace: namespace, Name: name})
	}
	return refs, nil
}

// contains reports whether the list names one volume.
func contains(refs []VolumeRef, ref VolumeRef) bool {
	for _, candidate := range refs {
		if candidate == ref {
			return true
		}
	}
	return false
}

// Volumes resolves one volume object (C-38).
type Volumes interface {
	Volume(ctx context.Context, namespace, name string) (*v1beta1.Volume, error)
}

// ClusterVolumes reads volumes through the write path, so the data plane sees
// exactly the objects the control plane serves.
type ClusterVolumes struct {
	// Writer reads the volume objects.
	Writer *write.Writer
}

// Volume returns one volume.
func (c ClusterVolumes) Volume(ctx context.Context, namespace, name string) (*v1beta1.Volume, error) {
	if c.Writer == nil {
		return nil, errors.New("no volume reader is configured")
	}
	object, err := c.Writer.Get(ctx, write.Ref{
		Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: namespace, Name: name,
	})
	if err != nil {
		return nil, err
	}
	var volume v1beta1.Volume
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &volume); err != nil {
		return nil, fmt.Errorf("read volume %s/%s: %w", namespace, name, err)
	}
	return &volume, nil
}

// Roots maps one volume to the directory the data plane serves for it (NG10).
type Roots interface {
	// Root returns the directory; false means the volume is not mounted here.
	Root(namespace, name string) (string, bool)
}

// DirectoryRoots serves one directory per volume below a base directory.
type DirectoryRoots struct {
	// Base is the directory the volume mounts live under.
	Base string
}

// Root returns the volume's directory below Base. A name that could step out
// of the base directory is not a volume this data plane serves.
func (d DirectoryRoots) Root(namespace, name string) (string, bool) {
	if d.Base == "" || !isPathSegment(namespace) || !isPathSegment(name) {
		return "", false
	}
	return filepath.Join(d.Base, namespace, name), true
}

// isPathSegment reports one directory component a volume may map to.
func isPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, `/\`)
}

// ownerAllows reports whether the session may open a volume's files (C-59,
// C-60). A volume without an owner belongs to the global Owner alone.
func ownerAllows(session identity.Session, volume *v1beta1.Volume) bool {
	if session.Role == write.RoleOwner {
		return true
	}
	return volume.Spec.Owner != "" && volume.Spec.Owner == session.Username
}

// isNotFound reports a volume the cluster does not hold.
func isNotFound(err error) bool { return apierrors.IsNotFound(err) }
