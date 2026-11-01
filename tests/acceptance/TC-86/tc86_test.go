// Package tc86 verifies the read-only and stale behaviour of AC-43 and C-56:
// when the cluster loses write quorum the global banner appears and writes are
// refused, and when the cluster API stops answering reads come from the cache
// with the observation time they keep, which marks them stale.
package tc86

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/degrade"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const application = "cached"

func TestTC86ReadOnlyBannerAndStaleReads(t *testing.T) {
	cluster := harness.Start(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := harness.NewClock(now)

	source := &switchingSource{
		live: controlplane.NewClusterSource(cluster.Dynamic),
		now:  clock.Now,
	}
	state := &degrade.Static{}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Source = source
		config.Degradation = state
		config.Now = clock.Now
	})

	cluster.Create(t, harness.Application(application, nil))

	// A read fills the cache and is fresh.
	status, body := gateway.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d: %v", status, body)
	}
	observedAt := firstObservedAt(t, body)
	if firstStale(t, body) {
		t.Fatal("a live read was marked stale")
	}

	// The cluster API stops answering and time passes beyond the freshness
	// bound: the same read is served from the cache with its own observation
	// time, marked stale (C-56).
	source.isFailing.Store(true)
	clock.Advance(controlplane.StaleThreshold + time.Minute)
	status, body = gateway.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("cached list status = %d, want 200 from the cache: %v", status, body)
	}
	if !firstStale(t, body) {
		t.Fatalf("the cached projection carries no stale marker: %v", body)
	}
	if got := firstObservedAt(t, body); !got.Equal(observedAt) {
		t.Fatalf("cached observation time = %s, want the original %s", got, observedAt)
	}

	// The cluster loses write quorum: the banner appears and every write is
	// refused with the stable read-only code.
	state.EnterReadOnly("etcd 失去多数派", clock.Now())
	manifest := harness.ApplicationManifest("refused", harness.Namespace, "shop", harness.Namespace, nil, nil)
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("write status = %d, want 503: %v", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeReadOnly {
		t.Fatalf("write code = %q, want %q", code, controlplane.CodeReadOnly)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cluster.Direct(t, "kos.tsic.top", "v1beta1", "Application", harness.Namespace).Get(ctx, "refused", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a refused write reached the cluster: %v", err)
	}

	// The panel shows the banner above the page.
	notice := bannerOf(t, state)
	if notice == nil || notice.Kind != degrade.NoticeReadOnly {
		t.Fatalf("banner = %+v, want a read-only notice", notice)
	}
	if notice.Reason != "etcd 失去多数派" || notice.Since.IsZero() {
		t.Fatalf("banner = %+v, want the reason and the start of the state", notice)
	}
	assertPageBanner(t, state)
}

// switchingSource stamps reads with the case clock and can stop answering.
type switchingSource struct {
	live      controlplane.ObjectSource
	now       func() time.Time
	isFailing atomic.Bool
}

func (s *switchingSource) List(ctx context.Context, objectType controlplane.ObjectType, namespace string) ([]controlplane.ObservedObject, error) {
	if s.isFailing.Load() {
		return nil, errors.New("cluster API is unreachable")
	}
	items, err := s.live.List(ctx, objectType, namespace)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index].ObservedAt = s.now()
	}
	return items, nil
}

func (s *switchingSource) Get(ctx context.Context, objectType controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	if s.isFailing.Load() {
		return controlplane.ObservedObject{}, errors.New("cluster API is unreachable")
	}
	item, err := s.live.Get(ctx, objectType, namespace, name)
	if err != nil {
		return controlplane.ObservedObject{}, err
	}
	item.ObservedAt = s.now()
	return item, nil
}

// firstObservedAt reads the observation time of the first projection.
func firstObservedAt(t *testing.T, body map[string]any) time.Time {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("response carries no projections: %v", body)
	}
	item, _ := items[0].(map[string]any)
	text := harness.String(item, "observedAt")
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("observedAt %q is not a time: %v", text, err)
	}
	return parsed
}

// firstStale reads the stale marker of the first projection.
func firstStale(t *testing.T, body map[string]any) bool {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("response carries no projections: %v", body)
	}
	item, _ := items[0].(map[string]any)
	stale, _ := item["stale"].(bool)
	return stale
}

// bannerOf reads the notice the shell renders.
func bannerOf(t *testing.T, state *degrade.Static) *degrade.Notice {
	t.Helper()
	return state.State().Notice
}

// assertPageBanner renders one page over the shared state and checks the
// read-only banner (C-56).
func assertPageBanner(t *testing.T, state *degrade.Static) {
	t.Helper()
	service := pages.New(staticPages{}, pages.Config{Banners: bannerSource{state: state}})
	page, failure := service.Read(context.Background(), identity.Session{Username: "owner", Role: write.RoleOwner}, "readonly")
	if failure != nil {
		t.Fatalf("read the page: %v", failure)
	}
	if page.Banner == nil || page.Banner.Kind != degrade.NoticeReadOnly {
		t.Fatalf("page banner = %+v, want the read-only notice", page.Banner)
	}
	summaries := service.List(identity.Session{Username: "owner", Role: write.RoleOwner})
	if len(summaries) != 1 || summaries[0].Banner == nil {
		t.Fatalf("page list carries no banner: %+v", summaries)
	}
}

// bannerSource adapts the degradation state to the page shell.
type bannerSource struct {
	state *degrade.Static
}

func (b bannerSource) Banner() *degrade.Notice {
	return b.state.State().Notice
}

// staticPages declares one read-only page.
type staticPages struct{}

func (staticPages) Pages() []extension.Page {
	return []extension.Page{{
		ID: "readonly", Title: "Read only", Location: "panel",
		ReadOnly: true, ReadOnlyReason: "acceptance",
		Items: []extension.PageItem{
			{ID: "note", Label: "Note", Source: extension.PageSourceLocal, Store: "note", Reason: "只读展示"},
		},
	}}
}

func (s staticPages) PageFor(id string, _ write.Role) (extension.Page, bool) {
	for _, page := range s.Pages() {
		if page.ID == id {
			return page, true
		}
	}
	return extension.Page{}, false
}
