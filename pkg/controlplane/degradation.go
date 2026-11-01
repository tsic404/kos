package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/degrade"
)

// RequiredVersion is the kernel object version this control plane reads and
// writes; definitions that stop serving it make the process refuse to start
// (C-16, C-56).
const RequiredVersion = v1beta1.Version

// definitionCheckTimeout bounds the startup version check, so an unresponsive
// cluster fails the start instead of hanging it.
const definitionCheckTimeout = 30 * time.Second

// ReasonProtectionUnwired is the reason the health endpoint reports when no
// degradation protection is attached: an unmeasured process is never reported
// healthy (C-57).
const ReasonProtectionUnwired = "降级保护未接入，无法判定控制面是否降级"

// Protection is the degradation protection the health endpoint reports (C-57).
type Protection interface {
	Status() degrade.ProtectionStatus
}

// Definitions reports which kernel kinds a group/version no longer serves. A
// definition that stopped serving the version the control plane needs leaves
// the process with no usable object model, so it refuses to start (C-16).
type Definitions interface {
	// MissingDefinitions returns the kinds of the model that do not serve the
	// version; an empty list means every definition serves it.
	MissingDefinitions(ctx context.Context, group, version string, kinds []string) ([]string, error)
}

// StorageCapacity reports whether the backing disk can take a new volume
// (C-56).
type StorageCapacity interface {
	IsFull(ctx context.Context) (bool, string, error)
}

// healthResponse is what GET /api/v1/system/health answers (C-56, C-57). It is
// a probe surface: it carries the verdict and its reason, never the internal
// measurement values (the percentile, the threshold or the sample time), which
// stay in the diagnostic bundle. A probe has to answer before any session
// exists, so this one route is not behind the authorizer.
type healthResponse struct {
	// Status is ok, degraded or unavailable.
	Status string `json:"status"`
	// State is the process-wide degradation state the shell banners.
	State degrade.State `json:"state"`
	// Protection is the protection verdict without its internal numbers.
	Protection protectionHealth `json:"protection"`
	// Reason explains an unavailable status.
	Reason string `json:"reason,omitempty"`
}

// protectionHealth is the probe-safe part of the protection status.
type protectionHealth struct {
	// IsMeasurable reports a fresh measurement.
	IsMeasurable bool `json:"measurable"`
	// IsDegraded reports that real-time listening is off.
	IsDegraded bool `json:"degraded"`
	// Reason explains an active degradation.
	Reason string `json:"reason,omitempty"`
	// UnmeasurableReason explains why no judgement could be made.
	UnmeasurableReason string `json:"unmeasurableReason,omitempty"`
}

// verdictOf keeps the probe-safe fields.
func verdictOf(status degrade.ProtectionStatus) protectionHealth {
	return protectionHealth{
		IsMeasurable:       status.IsMeasurable,
		IsDegraded:         status.IsDegraded,
		Reason:             status.Reason,
		UnmeasurableReason: status.UnmeasurableReason,
	}
}

// handleHealth answers the degradation health endpoint. It reports unavailable
// when the protection cannot measure, and never reports ok from an unmeasured
// process (C-56, C-57).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	response := healthResponse{Status: "ok", State: s.degradationState()}
	if s.protection == nil {
		response.Status = "unavailable"
		response.Reason = ReasonProtectionUnwired
		response.Protection = protectionHealth{IsMeasurable: false, UnmeasurableReason: ReasonProtectionUnwired}
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	status := s.protection.Status()
	response.Protection = verdictOf(status)
	switch {
	case !status.IsMeasurable:
		response.Status = "unavailable"
		response.Reason = status.UnmeasurableReason
		if response.Reason == "" {
			response.Reason = degrade.ReasonNoReading
		}
		writeJSON(w, http.StatusServiceUnavailable, response)
	case status.IsDegraded:
		response.Status = "degraded"
		writeJSON(w, http.StatusOK, response)
	default:
		writeJSON(w, http.StatusOK, response)
	}
}

// degradationState reads the process-wide state; a server without one has
// nothing degraded.
func (s *Server) degradationState() degrade.State {
	if s.degradation == nil {
		return degrade.State{}
	}
	return s.degradation.State()
}

// refuseWrite answers a write the cluster cannot accept because it lost its
// write quorum (C-56). It reports whether the request was refused.
func (s *Server) refuseWrite(w http.ResponseWriter) bool {
	state := s.degradationState()
	if !state.IsReadOnly {
		return false
	}
	reason := ""
	if state.Notice != nil {
		reason = state.Notice.Reason
	}
	writeError(w, readOnlyFailure(reason))
	return true
}

// refuseFullStorage answers a new volume when the backing disk is full; a full
// disk blocks new volumes instead of letting them pend forever (C-56).
func (s *Server) refuseFullStorage(w http.ResponseWriter, ctx context.Context, object desiredObject) bool {
	if s.storageCapacity == nil || object.Type.Kind != "Volume" {
		return false
	}
	isFull, reason, err := s.storageCapacity.IsFull(ctx)
	if err != nil {
		writeError(w, internal(StageValidate, "无法读取磁盘容量，暂不接受新建卷").about(object.Ref))
		return true
	}
	if !isFull {
		return false
	}
	writeError(w, storageFullFailure(reason).about(object.Ref))
	return true
}

// refuseFullStorageSet applies the same full-disk rule to a desired set: an
// apply that would create a volume is refused before any object is written
// (C-56).
func (s *Server) refuseFullStorageSet(ctx context.Context, desired []desiredObject) *Error {
	if s.storageCapacity == nil {
		return nil
	}
	hasVolume := false
	for _, object := range desired {
		if object.Type.Kind == "Volume" {
			hasVolume = true
			break
		}
	}
	if !hasVolume {
		return nil
	}
	isFull, reason, err := s.storageCapacity.IsFull(ctx)
	if err != nil {
		return internal(StageValidate, "无法读取磁盘容量，暂不接受新建卷")
	}
	if !isFull {
		return nil
	}
	return storageFullFailure(reason)
}

// requireServedDefinitions refuses to build the API when the object definitions
// no longer serve the version it needs (C-16, C-56). The process either has a
// usable object model or does not start at all.
func requireServedDefinitions(definitions Definitions) error {
	if definitions == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), definitionCheckTimeout)
	defer cancel()
	missing, err := definitions.MissingDefinitions(ctx, api.Group, RequiredVersion, api.Kinds())
	if err != nil {
		return fmt.Errorf("controlplane: read object definition versions: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("controlplane: object definitions no longer serve %s/%s for %v; the control plane requires %s and refuses to start",
		api.Group, RequiredVersion, missing, RequiredVersion)
}
