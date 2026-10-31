package extension

import (
	"strings"
	"testing"
)

// validPage is a page declaration every invalid shape starts from.
func validPage() Page {
	return Page{
		ID: "widgets", Title: "Widgets", Location: "panel",
		Items: []PageItem{{
			ID: "entries", Label: "Entries", Source: PageSourceLocal, Store: "widgets", Action: "install",
		}},
		Actions: []Action{{ID: "install", Method: "POST", Path: "/api/v1/action-requests"}},
	}
}

// validReadOnlyPage is a read-only page that states why and offers no action.
func validReadOnlyPage() Page {
	return Page{
		ID: "native", Title: "Native", Location: "native", ReadOnly: true,
		ReadOnlyReason: "只读展示",
		Items:          []PageItem{{ID: "fields", Label: "Fields", Source: PageSourceLocal, Store: "native"}},
	}
}

// TestLoadPagesRejectsEachInvalidShape checks every rule loadPages adds around
// C-22 and C-53 with a case that fails when that rule is removed.
func TestLoadPagesRejectsEachInvalidShape(t *testing.T) {
	cases := []struct {
		name  string
		build func() Page
		want  string
	}{
		{name: "missing location", want: "location is required", build: func() Page {
			page := validPage()
			page.Location = ""
			return page
		}},
		{name: "zero items", want: "at least one item is required", build: func() Page {
			page := validPage()
			page.Items = nil
			return page
		}},
		{name: "read-only without reason", want: "a read-only page must state why", build: func() Page {
			page := validReadOnlyPage()
			page.ReadOnlyReason = ""
			return page
		}},
		{name: "read-only with action", want: "a read-only page declares no action", build: func() Page {
			page := validReadOnlyPage()
			page.Actions = validPage().Actions
			return page
		}},
		{name: "read-only item with action", want: "a read-only page item has no modification entry", build: func() Page {
			page := validReadOnlyPage()
			page.Items[0].Action = "install"
			return page
		}},
		{name: "writable item without entry or reason", want: "a writable page item needs a modification entry or a reason", build: func() Page {
			page := validPage()
			page.Items[0].Action = ""
			return page
		}},
		{name: "item with entry and reason", want: "an item with a modification entry must not declare a reason", build: func() Page {
			page := validPage()
			page.Items[0].Reason = "只读展示"
			return page
		}},
		{name: "item reason with surrounding whitespace", want: "reason must not carry surrounding whitespace", build: func() Page {
			page := validPage()
			page.Items[0].Action = ""
			page.Items[0].Reason = " 只读展示 "
			return page
		}},
		{name: "unknown action reference", want: "is not a declared action", build: func() Page {
			page := validPage()
			page.Items[0].Action = "absent"
			return page
		}},
		{name: "local item missing store", want: "a local item needs the browser storage key", build: func() Page {
			page := validPage()
			page.Items[0].Store = ""
			return page
		}},
		{name: "local item with reference", want: "a local item has no control-plane reference", build: func() Page {
			page := validPage()
			page.Items[0].Reference = "cluster"
			return page
		}},
		{name: "server item missing reference", want: "a server item needs a reference", build: func() Page {
			page := validPage()
			page.Items[0].Source = PageSourceObject
			page.Items[0].Store = ""
			return page
		}},
		{name: "server item with store", want: "only a local item names browser storage", build: func() Page {
			page := validPage()
			page.Items[0].Source = PageSourceObject
			page.Items[0].Reference = "nodes"
			return page
		}},
		{name: "unknown source type", want: "is not a page source type", build: func() Page {
			page := validPage()
			page.Items[0].Source = "telepathy"
			return page
		}},
		{name: "GET action", want: "is not a control-plane write method", build: func() Page {
			page := validPage()
			page.Actions[0].Method = "GET"
			return page
		}},
		{name: "external URL action", want: "is not a control-plane API path", build: func() Page {
			page := validPage()
			page.Actions[0].Path = "https://evil.example/x"
			return page
		}},
		{name: "wrong API version action", want: "is not a control-plane API path", build: func() Page {
			page := validPage()
			page.Actions[0].Path = "/api/v2/pages"
			return page
		}},
		{name: "path traversal action", want: "is not a control-plane API path", build: func() Page {
			page := validPage()
			page.Actions[0].Path = "/api/v1/../x"
			return page
		}},
		{name: "empty action id", want: "id is required", build: func() Page {
			page := validPage()
			page.Actions[0].ID = ""
			return page
		}},
		{name: "duplicate action", want: "action install is declared twice", build: func() Page {
			page := validPage()
			page.Actions = append(page.Actions, page.Actions[0])
			return page
		}},
		{name: "duplicate item", want: "item entries is declared twice", build: func() Page {
			page := validPage()
			page.Items = append(page.Items, page.Items[0])
			return page
		}},
		{name: "read-only reason on a writable page", want: "readOnlyReason requires readOnly", build: func() Page {
			page := validPage()
			page.ReadOnlyReason = "只读展示"
			return page
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			kept, problems := loadPages([]Page{testCase.build()})
			if len(kept) != 0 {
				t.Fatalf("invalid page was kept: %+v", kept[0])
			}
			if !containsProblem(problems, testCase.want) {
				t.Fatalf("problems = %v, want %q", problems, testCase.want)
			}
		})
	}
}

// TestLoadPagesKeepsValidShapes checks that the new rules do not reject a page
// that names a modification entry or an explicit reason for every item.
func TestLoadPagesKeepsValidShapes(t *testing.T) {
	cases := []struct {
		name  string
		build func() Page
	}{
		{name: "writable item with action", build: validPage},
		{name: "writable item with reason", build: func() Page {
			page := validPage()
			page.Items[0].Action = ""
			page.Items[0].Reason = "只读展示"
			page.Actions = nil
			return page
		}},
		{name: "read-only page", build: validReadOnlyPage},
		{name: "all seven sources", build: func() Page {
			return Page{
				ID: "all", Title: "All", Location: "panel",
				Items: []PageItem{
					{ID: "object", Label: "Object", Source: PageSourceObject, Reference: "nodes", Reason: "只读展示"},
					{ID: "computed", Label: "Computed", Source: PageSourceComputed, Reference: "count", Reason: "只读展示"},
					{ID: "metric", Label: "Metric", Source: PageSourceMetric, Reference: "cpu", Reason: "只读展示"},
					{ID: "probe", Label: "Probe", Source: PageSourceProbe, Reference: "dns", Reason: "只读展示"},
					{ID: "read", Label: "Read", Source: PageSourceRead, Reference: "logs", Reason: "只读展示"},
					{ID: "local", Label: "Local", Source: PageSourceLocal, Store: "view", Reason: "只读展示"},
					{ID: "database", Label: "Database", Source: PageSourceDatabase, Reference: "members", Reason: "只读展示"},
				},
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			kept, problems := loadPages([]Page{testCase.build()})
			if len(problems) != 0 {
				t.Fatalf("valid page reported problems: %v", problems)
			}
			if len(kept) != 1 {
				t.Fatalf("kept %d pages, want 1", len(kept))
			}
		})
	}
}

// TestIsPageSourceNamesTheSevenTypes checks C-53's source vocabulary.
func TestIsPageSourceNamesTheSevenTypes(t *testing.T) {
	for _, source := range []string{
		PageSourceObject, PageSourceComputed, PageSourceMetric, PageSourceProbe,
		PageSourceRead, PageSourceLocal, PageSourceDatabase,
	} {
		if !IsPageSource(source) {
			t.Errorf("source %q is rejected", source)
		}
	}
	for _, source := range []string{"", "cluster", "browser"} {
		if IsPageSource(source) {
			t.Errorf("source %q is accepted", source)
		}
	}
}

// containsProblem reports whether one problem line carries the expected text.
func containsProblem(problems []string, want string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return true
		}
	}
	return false
}
