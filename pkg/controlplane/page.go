package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/labels"
)

const (
	// defaultPageSize is the page a list returns when the caller names no limit.
	defaultPageSize = 50
	// maxPageSize bounds one page so a single read cannot pull the whole cluster.
	maxPageSize = 500
)

// pageToken is the opaque cursor a list returns and the next request echoes. It
// carries the whole query it belongs to: a token replayed against another kind,
// namespace or filter is refused rather than silently skipping objects (C-12).
type pageToken struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Selector  string `json:"selector"`
	TargetApp string `json:"targetApp,omitempty"`
	Last      string `json:"last"`
}

// pageSize reads the limit parameter.
func pageSize(raw string) (int, *Error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 || size > maxPageSize {
		return 0, invalidField("limit 必须是 1 到 " + strconv.Itoa(maxPageSize) + " 之间的整数")
	}
	return size, nil
}

// parseSelector reads the labelSelector parameter; an empty value selects
// everything.
func parseSelector(raw string) (labels.Selector, *Error) {
	selector, err := labels.Parse(raw)
	if err != nil {
		return nil, invalidField("labelSelector 不是合法的选择器：" + err.Error())
	}
	return selector, nil
}

// decodePageToken reads the continue parameter and checks it belongs to the
// current query, object class and target filter included.
func decodePageToken(raw string, object ObjectType, namespace, selector, targetApp string) (string, *Error) {
	if raw == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", invalidField("continue 不是合法的分页游标")
	}
	var token pageToken
	if err := json.Unmarshal(decoded, &token); err != nil {
		return "", invalidField("continue 不是合法的分页游标")
	}
	if token.Group != object.Group || token.Version != object.Version || token.Kind != object.Kind ||
		token.Namespace != namespace || token.Selector != selector || token.TargetApp != targetApp {
		return "", invalidField("continue 属于另一次查询（对象类型、命名空间或过滤条件不同），请从头读取")
	}
	return token.Last, nil
}

// encodePageToken builds the cursor pointing behind last for this query.
func encodePageToken(object ObjectType, namespace, selector, targetApp, last string) string {
	encoded, err := json.Marshal(pageToken{
		Group: object.Group, Version: object.Version, Kind: object.Kind,
		Namespace: namespace, Selector: selector, TargetApp: targetApp, Last: last,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// sortProjections orders a page deterministically by reference.
func sortProjections(projections []Projection) {
	sort.Slice(projections, func(left, right int) bool {
		return projections[left].Ref.String() < projections[right].Ref.String()
	})
}

// pagePosition is where a page starts: the first item whose reference sorts
// after the cursor. A cursor whose object disappeared still resumes the walk. ok
// is false when the cursor sorts before every item, which means it cannot have
// come from this listing; resuming there would replay objects silently.
func pagePosition(projections []Projection, last string) (int, bool) {
	if last == "" {
		return 0, true
	}
	position := sort.Search(len(projections), func(index int) bool {
		return projections[index].Ref.String() > last
	})
	if position == 0 && len(projections) > 0 {
		return 0, false
	}
	return position, true
}
