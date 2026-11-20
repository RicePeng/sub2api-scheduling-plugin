package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func reply(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}

func tokenValue(value int64) *int64 { return &value }

func TestRecentUsageCannotFillMissingTTFTFromOlderCalls(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	makeUsage := func(id int64, ttft *int64) Usage {
		return Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Duration(id) * time.Second), Model: "a", RequestType: "stream",
			InputTokens: tokenValue(10000), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(30000), FirstTokenMs: ttft}
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("account_id") != "7" || q.Get("sort_order") != "desc" || q.Get("end_date") != "2026-11-18" || q.Get("timezone") != "UTC" {
			t.Errorf("incorrect recent-call filter: %v", q)
		}
		items := []Usage{}
		if q.Get("page") == "1" {
			// Newer in-flight calls are outside this fixed collection boundary.
			future := makeUsage(999, tokenValue(999999))
			future.CreatedAt = now.Add(time.Second)
			items = append(items, future)
			for id := int64(1); id <= 99; id++ {
				ttft := tokenValue(id * 10)
				if id == 1 {
					ttft = tokenValue(-1)
				}
				items = append(items, makeUsage(id, ttft))
			}
		} else {
			items = append(items, makeUsage(90, tokenValue(900))) // duplicate must not count twice
			for id := int64(100); id <= 110; id++ {
				items = append(items, makeUsage(id, tokenValue(id*10)))
			}
		}
		reply(w, map[string]any{"items": items})
	}))
	defer server.Close()
	client := New(server.URL, "key", time.Second)
	defer client.Close()
	w, err := client.RecentUsage(context.Background(), 7, now)
	if err != nil || calls != 2 || !w.Complete || w.FirstTokenComplete || w.Requests != 20 || w.FirstTokenSamples != 19 ||
		w.InputTokens != 200000 || w.CacheReadTokens != 600000 || w.AverageFirstTokenMs != nil {
		t.Fatalf("recent window=%+v calls=%d error=%v", w, calls, err)
	}
	if w.SampleKey == w.FirstTokenSampleKey || w.CacheRatio == nil || *w.CacheRatio != .75 {
		t.Fatal("known cache metrics or available TTFT cursor were lost")
	}
}

func TestRecentUsageInsufficientTTFTAndMissingTokensStayUnknown(t *testing.T) {
	now := time.Now().UTC()
	items := []Usage{}
	for id := int64(1); id <= 20; id++ {
		u := Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Duration(id) * time.Second), Model: "a", RequestType: "sync",
			InputTokens: tokenValue(10000), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(30000)}
		if id < 20 {
			u.FirstTokenMs = tokenValue(0)
		}
		items = append(items, u)
	}
	items[0].InputTokens = nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]any{"items": items}) }))
	defer server.Close()
	client := New(server.URL, "key", time.Second)
	defer client.Close()
	w, err := client.RecentUsage(context.Background(), 7, now)
	if err != nil || w.ExcludedUnknownRequests != 1 || w.Requests != 19 || w.FirstTokenSamples != 18 || w.AverageFirstTokenMs != nil {
		t.Fatalf("unknown observations treated as known: %+v %v", w, err)
	}
	items[19].FirstTokenMs = tokenValue(0)
	items[0].InputTokens = tokenValue(10000)
	w, err = client.RecentUsage(context.Background(), 7, now)
	if err != nil || w.AverageFirstTokenMs == nil || *w.AverageFirstTokenMs != 0 {
		t.Fatal("valid zero TTFT was discarded")
	}
}

func TestRecentUsageStopsAtSessionBoundaryAndKeepsCacheAtScanLimit(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		window          time.Duration
		pages, requests int
		limited         bool
	}{
		{"fresh session", 3 * time.Second, 1, 3, false},
		{"TTFT missing after scan limit", 2 * time.Hour, 50, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			since := now.Add(-tc.window)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got := r.URL.Query().Get("start_date"); got != since.Format("2006-01-02") {
					t.Errorf("incorrect activation date filter: %q", got)
				}
				items := make([]Usage, 100)
				for i := range items {
					id := int64((calls-1)*len(items) + i + 1)
					items[i] = Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Duration(id) * time.Second),
						Model: "common", RequestType: "sync",
						InputTokens: tokenValue(10000), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(30000)}
					if id == 1 {
						items[i].Model = "rare"
					}
				}
				reply(w, map[string]any{"items": items})
			}))
			defer server.Close()
			client := New(server.URL, "key", time.Second)
			defer client.Close()
			window, err := client.RecentUsageSince(context.Background(), 7, since, now, 40000)
			window.SealQuality(since, "")
			if err != nil || calls != tc.pages || window.Requests != tc.requests || window.FirstTokenLimitReached ||
				window.FirstTokenSamples != 0 || window.AverageFirstTokenMs != nil || window.CacheRatio == nil || *window.CacheRatio != .75 {
				t.Fatalf("unexpected aggregate sampling: requests=%d, pages=%d, error=%v", window.Requests, calls, err)
			}
			current := window.QualityCurrent
			if current == nil || window.QualityScanLimitReached != tc.limited || current.WindowKnown == tc.limited || len(window.QualityWindows) != 0 {
				t.Fatalf("unexpected quality boundary: current=%+v, windows=%+v", current, window.QualityWindows)
			}
			if !tc.limited {
				if current.Requests != 3 || len(current.Models) != 2 || current.Models[0].Requests != 2 || current.Models[1].Requests != 1 {
					t.Fatalf("exact session boundary lost or included rows: %+v", current)
				}
				return
			}
			window.SealQuality(since, now.Add(-5001*time.Second).Format(time.RFC3339Nano)+":5001")
			if window.QualityCurrent.WindowKnown || len(window.QualityWindows) != 0 {
				t.Fatal("a cursor outside the scanned range incorrectly proved alignment")
			}
			window.SealQuality(since, now.Add(-23*time.Second).Format(time.RFC3339Nano)+":23")
			if len(window.QualityWindows) != 1 || window.QualityWindows[0].Requests != 20 ||
				window.QualityWindows[0].AverageFirstTokenMs != nil || window.QualityCurrent.Requests != 2 {
				t.Fatalf("verified anchor at the scan limit did not seal: current=%+v, windows=%+v", window.QualityCurrent, window.QualityWindows)
			}
		})
	}
}

func TestRecentUsageIndependentlyTrimsAccountMetricsAcrossPages(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	shared, since := now.Add(-time.Second), now.Add(-2*time.Second)
	usage := func(id int64, at time.Time, model, requestType string, capacity, cache, ttft int64) Usage {
		return Usage{ID: id, AccountID: 7, CreatedAt: at, Model: model, RequestType: requestType,
			InputTokens: tokenValue(capacity - cache), CacheCreationTokens: tokenValue(0),
			CacheReadTokens: tokenValue(cache), FirstTokenMs: tokenValue(ttft)}
	}
	first := []Usage{usage(200, now.Add(time.Second), "future", "sync", 40000, 100, 10)}
	for id := int64(199); id >= 101; id-- {
		kind := []string{"cyber", "unknown", ""}[id%3]
		u := usage(id, shared, "ineligible", kind, 40000, 0, 10)
		if id >= 115 && id <= 124 {
			cache, ttft := int64(75000), int64(300)
			if id >= 123 {
				cache = 100000
			}
			if id <= 116 {
				ttft = 0
			}
			u = usage(id, shared, "b", "ws_v2", 100000, cache, ttft)
		}
		first = append(first, u)
	}
	second := []Usage{first[1]}
	for id := int64(100); id >= 91; id-- {
		typ := "sync"
		if id >= 97 {
			typ = "stream"
		}
		at := shared
		if id == 91 {
			at = since
		}
		cache, ttft := int64(10000), int64(100)
		if id <= 92 {
			cache = 0
		}
		if id == 97 || id == 98 {
			ttft = 10000
		}
		second = append(second, usage(id, at, "a", typ, 40000, cache, ttft))
	}
	second = append(second, second[1], usage(90, since.Add(-time.Nanosecond), "before", "sync", 40000, 100, 10))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		items := first
		if r.URL.Query().Get("page") == "2" {
			items = second
		}
		reply(w, map[string]any{"items": items, "page": calls, "page_size": 100, "pages": 2})
	}))
	defer server.Close()
	client := New(server.URL, "key", time.Second)
	defer client.Close()
	window, err := client.RecentUsageSince(context.Background(), 7, since, now, 40000)
	window.SealQuality(since, "")
	if err != nil || calls != 2 || window.QualityScanLimitReached || window.QualityCurrent == nil ||
		window.QualityCurrent.Requests != 0 || len(window.QualityWindows) != 1 {
		t.Fatalf("20 eligible calls did not close exactly one account window: current=%+v, windows=%+v, pages=%d, error=%v",
			window.QualityCurrent, window.QualityWindows, calls, err)
	}
	sealed := window.QualityWindows[0]
	if sealed.FirstSample != since.Format(time.RFC3339Nano)+":91" ||
		sealed.LastSample != shared.Format(time.RFC3339Nano)+":124" || sealed.Requests != 20 ||
		sealed.CacheRatio == nil || *sealed.CacheRatio != .5 || sealed.FirstTokenSamples != 20 ||
		sealed.AverageFirstTokenMs == nil || *sealed.AverageFirstTokenMs != 200 ||
		sealed.ExcludedSmallRequests != 0 || sealed.ExcludedUnknownRequests != 0 {
		t.Fatalf("incorrect timeline or independently trimmed cache and TTFT: %+v", sealed)
	}
	if window.Requests != 20 || window.FirstTokenSamples != 20 || window.CacheRatio == nil || *window.CacheRatio != *sealed.CacheRatio ||
		window.AverageFirstTokenMs == nil || *window.AverageFirstTokenMs != *sealed.AverageFirstTokenMs {
		t.Fatalf("rolling and sealed account metrics disagree: %+v, %+v", window, sealed)
	}
	want := []struct {
		model, typ  string
		count       int
		ratio, ttft float64
	}{{"a", "stream", 4, .25, 5050}, {"a", "sync", 6, 40000.0 / 240000, 100}, {"b", "ws_v2", 10, .8, 240}}
	if len(sealed.Models) != len(want) {
		t.Fatalf("expected exactly three diagnostic model/type groups: %+v", sealed.Models)
	}
	for i, model := range sealed.Models {
		w := want[i]
		if model.Model != w.model || model.RequestType != w.typ || model.Requests != w.count ||
			model.FirstTokenSamples != w.count || model.CacheRatio == nil || *model.CacheRatio != w.ratio ||
			model.AverageFirstTokenMs == nil || *model.AverageFirstTokenMs != w.ttft {
			t.Errorf("wrong model breakdown at %d: %+v", i, model)
		}
	}
}

func TestRecentUsageQualityRejectsUnreliablePagesAndDoesNotMeasureManagement502(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	makeUsage := func(id int64) Usage {
		return Usage{ID: id, AccountID: 7, CreatedAt: now,
			Model: "a", RequestType: "stream", InputTokens: tokenValue(10000), CacheCreationTokens: tokenValue(0),
			CacheReadTokens: tokenValue(30000), FirstTokenMs: tokenValue(10)}
	}
	first := []Usage{}
	for id := int64(200); id > 100; id-- {
		first = append(first, makeUsage(id))
	}
	for _, tc := range []struct {
		name         string
		next         Usage
		responsePage int
		status       int
		errorText    string
	}{
		{"order", makeUsage(201), 2, 200, "顺序"},
		{"page", makeUsage(100), 1, 200, "分页"},
		{"duplicate page", Usage{}, 2, 200, "未前进"},
		{"account", Usage{ID: 100, AccountID: 8, CreatedAt: now}, 2, 200, "账号不匹配"},
		{"management 502", Usage{}, 2, 502, "HTTP 502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					reply(w, map[string]any{"items": first, "page": 1, "page_size": 100, "pages": 2})
					return
				}
				if tc.status != http.StatusOK {
					http.Error(w, "upstream secret", tc.status)
					return
				}
				items := []Usage{tc.next}
				if tc.name == "duplicate page" {
					items = first
				}
				reply(w, map[string]any{"items": items, "page": tc.responsePage, "page_size": 100, "pages": 2})
			}))
			defer server.Close()
			client := New(server.URL, "key", time.Second)
			defer client.Close()
			window, err := client.RecentUsageSince(context.Background(), 7, now.Add(-time.Hour), now, 40000)
			window.SealQuality(now.Add(-time.Hour), "")
			if err == nil || !strings.Contains(err.Error(), tc.errorText) || strings.Contains(err.Error(), "secret") ||
				window.QualityCurrent != nil || len(window.QualityWindows) != 0 || window.QualityScanLimitReached {
				t.Fatalf("unreliable quality window was accepted: %+v, %v", window, err)
			}
			if tc.status == http.StatusBadGateway && (window.Requests != 20 || !window.Complete || window.AverageFirstTokenMs == nil || *window.AverageFirstTokenMs != 10) {
				t.Fatalf("later management 502 changed already complete rolling metrics: %+v", window)
			}
		})
	}
}

func TestRecentUsageQualityRequiresTwentyCalls(t *testing.T) {
	now := time.Now().UTC()
	rows := make([]Usage, 20)
	for i := range rows {
		rows[i] = Usage{ID: int64(i + 1), AccountID: 7, CreatedAt: now, Model: "a", RequestType: "sync",
			InputTokens: tokenValue(10000), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(30000),
			FirstTokenMs: tokenValue(42)}
	}
	for _, count := range []int{19, 20} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reply(w, map[string]any{"items": rows[:count]})
			}))
			defer server.Close()
			client := New(server.URL, "key", time.Second)
			defer client.Close()
			w, err := client.RecentUsage(context.Background(), 7, now)
			w.SealQuality(time.Time{}, "")
			if err != nil || w.Requests != count || w.FirstTokenSamples != count || w.QualityCurrent == nil {
				t.Fatalf("unexpected aggregate at %d requests: %+v, %v", count, w, err)
			}
			b := w.QualityCurrent
			if count == 19 && (len(w.QualityWindows) != 0 || b.Requests != 19 || len(b.Models) != 1 ||
				b.Models[0].Requests != 19 || w.AverageFirstTokenMs != nil) {
				t.Fatalf("19 calls did not stay in the trailing partial window: %+v", b)
			}
			if count == 20 && (b.Requests != 0 || len(w.QualityWindows) != 1 ||
				w.QualityWindows[0].CacheRatio == nil || *w.QualityWindows[0].CacheRatio != .75 ||
				w.QualityWindows[0].AverageFirstTokenMs == nil || *w.QualityWindows[0].AverageFirstTokenMs != 42 ||
				w.AverageFirstTokenMs == nil || *w.AverageFirstTokenMs != 42) {
				t.Fatalf("20 calls did not form a complete window with no remainder: %+v", w.QualityWindows)
			}
		})
	}
}

func TestRecentUsageCapacityFilterCountsWithinEachWindow(t *testing.T) {
	now := time.Now().UTC()
	makeRow := func(id int64, model, requestType string, capacity int64) Usage {
		return Usage{ID: id, AccountID: 7, CreatedAt: now.Add(time.Duration(id-100) * time.Second),
			Model: model, RequestType: requestType, InputTokens: tokenValue(capacity - 30000),
			CacheCreationTokens: tokenValue(5000), CacheReadTokens: tokenValue(25000),
			FirstTokenMs: tokenValue(42)}
	}
	firstSmall := makeRow(99, "a", "sync", 39999)
	firstSmall.FirstTokenMs = tokenValue(9999)
	firstSmall.AccountRate = floatPtrForUsageTest(.7)
	unknown := makeRow(97, "a", "sync", 40000)
	unknown.CacheCreationTokens, unknown.FirstTokenMs = nil, tokenValue(9999)
	rows := []Usage{firstSmall, makeRow(98, "a", "stream", 39999), unknown, firstSmall}
	for id := int64(96); id >= 77; id-- {
		rows = append(rows, makeRow(id, "a", "sync", 40000))
	}
	olderUnknown := makeRow(75, "a", "sync", 40000)
	olderUnknown.CacheReadTokens = tokenValue(-1)
	rows = append(rows, makeRow(76, "a", "sync", 39999), olderUnknown)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"items": rows})
	}))
	defer server.Close()
	client := New(server.URL, "key", time.Second)
	defer client.Close()
	for poll := 0; poll < 2; poll++ {
		w, err := client.RecentUsageSince(context.Background(), 7, now.Add(-time.Hour), now, 40000)
		w.SealQuality(now.Add(-time.Hour), "")
		if err != nil || w.MinInputTokens != 40000 || w.Requests != 20 || !w.Complete ||
			w.ExcludedSmallRequests != 2 || w.ExcludedUnknownRequests != 1 ||
			w.CacheRatio == nil || *w.CacheRatio != .625 || w.AverageFirstTokenMs == nil ||
			*w.AverageFirstTokenMs != 42 || w.AccountRate == nil || *w.AccountRate != .7 || w.QualityCurrent == nil {
			t.Fatalf("poll %d: wrong aggregate filter or exclusions: %+v, %v", poll, w, err)
		}
		current := w.QualityCurrent
		if current.MinInputTokens != 40000 || current.Requests != 0 || current.ExcludedSmallRequests != 2 ||
			current.ExcludedUnknownRequests != 1 || len(current.Models) != 0 || len(w.QualityWindows) != 1 {
			t.Fatalf("poll %d: wrong trailing exclusions or excluded calls appeared as model usage: %+v", poll, current)
		}
		sealed := w.QualityWindows[0]
		if sealed.Requests != 20 || sealed.ExcludedSmallRequests != 1 || sealed.ExcludedUnknownRequests != 1 ||
			sealed.CacheRatio == nil || *sealed.CacheRatio != .625 || sealed.AverageFirstTokenMs == nil || *sealed.AverageFirstTokenMs != 42 {
			t.Fatalf("poll %d: wrong sealed account window: %+v", poll, sealed)
		}
	}
}

func floatPtrForUsageTest(v float64) *float64 { return &v }

func TestUsageWindowDateFilterDedupAndExactBounds(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 5, 0, 0, time.UTC)
	from := now.Add(-15 * time.Minute)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.Header.Get("x-api-key") != "private-test-value" || q.Get("timezone") != "UTC" ||
			q.Get("start_date") != "2026-11-17" || q.Get("end_date") != "2026-11-18" ||
			q.Get("sort_order") != "desc" || q.Get("account_id") != "7" {
			t.Errorf("incorrect filter/header: %v", q)
		}
		items := []Usage{}
		if q.Get("page") == "1" {
			for id := int64(1); id <= 100; id++ {
				items = append(items, Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Minute),
					InputTokens: tokenValue(100), CacheReadTokens: tokenValue(300), CacheCreationTokens: tokenValue(100)})
			}
		} else {
			items = []Usage{
				{ID: 100, AccountID: 7, CreatedAt: now.Add(-time.Minute), InputTokens: tokenValue(1), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(999)},
				{ID: 101, AccountID: 7, CreatedAt: from, InputTokens: tokenValue(100), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(0)},
				{ID: 102, AccountID: 7, CreatedAt: from.Add(-time.Second), InputTokens: tokenValue(1), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(999)},
				{ID: 103, AccountID: 7, CreatedAt: now.Add(time.Second), InputTokens: tokenValue(1), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(999)},
			}
		}
		reply(w, map[string]any{"items": items})
	}))
	defer server.Close()
	client := New(server.URL, "private-test-value", time.Second)
	defer client.Close()
	window, err := client.UsageWindow(context.Background(), 7, from, now)
	if err != nil || !window.Complete || window.Requests != 101 || window.CacheReadTokens != 30000 ||
		window.InputTokens != 10100 || calls != 2 {
		t.Fatalf("window=%+v calls=%d err=%v", window, calls, err)
	}
	if window.CacheRatio == nil || *window.CacheRatio < .59 {
		t.Fatal("bad cache ratio")
	}
}

func TestUsageWindowRejectsTruncationAndMismatchedRows(t *testing.T) {
	now := time.Now().UTC()
	for _, mismatch := range []bool{false, true} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				account := int64(7)
				if mismatch {
					account = 8
				}
				items := []Usage{}
				for i := 1; i <= 100; i++ {
					items = append(items, Usage{ID: int64(i), AccountID: account, CreatedAt: now,
						InputTokens: tokenValue(10), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(0)})
				}
				reply(w, map[string]any{"items": items})
			}))
			defer server.Close()
			client := New(server.URL, "key", time.Second)
			defer client.Close()
			window, err := client.UsageWindow(context.Background(), 7, now.Add(-time.Hour), now)
			if err == nil || window.Complete || window.CacheRatio != nil || calls > 50 {
				t.Fatalf("unsafe incomplete window: %+v %v %d", window, err, calls)
			}
		})
	}
}

func TestUsageWindowMissingTokensAndItemsIsIncomplete(t *testing.T) {
	now := time.Now().UTC()
	zero := int64(0)
	cases := map[string]any{
		`missing-input`:        map[string]any{`id`: 1, `account_id`: 7, `created_at`: now, `cache_creation_tokens`: 0, `cache_read_tokens`: 0},
		`null-cache-read`:      map[string]any{`id`: 1, `account_id`: 7, `created_at`: now, `input_tokens`: 10, `cache_creation_tokens`: 0, `cache_read_tokens`: nil},
		`missing-cache-create`: map[string]any{`id`: 1, `account_id`: 7, `created_at`: now, `input_tokens`: 10, `cache_read_tokens`: 0},
		`complete-zero`:        map[string]any{`id`: 1, `account_id`: 7, `created_at`: now, `input_tokens`: zero, `cache_creation_tokens`: zero, `cache_read_tokens`: zero},
		`missing-items`:        nil,
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if name == `missing-items` {
					reply(w, map[string]any{`other`: []any{}})
				} else {
					reply(w, map[string]any{`items`: []any{item}})
				}
			}))
			defer server.Close()
			client := New(server.URL, `key`, time.Second)
			defer client.Close()
			window, err := client.UsageWindow(context.Background(), 7, now.Add(-time.Hour), now)
			if name == `complete-zero` {
				if err != nil || !window.Complete || window.CacheRatio != nil {
					t.Fatalf(`valid zero tokens rejected: %+v %v`, window, err)
				}
				return
			}
			if err == nil || window.Complete || window.CacheRatio != nil {
				t.Fatalf(`incomplete usage was accepted: %+v %v`, window, err)
			}
		})
	}
}

func TestAdminErrorsDoNotLeakAndRedirectsAreNotFollowed(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls++
	}))
	defer target.Close()
	for _, status := range []int{301, 401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("SECRET-upstream-body"))
			}))
			defer server.Close()
			client := New(server.URL, "SECRET-admin-key", time.Second)
			defer client.Close()
			_, err := client.Account(context.Background(), 7)
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal(err)
			}
		})
	}
	if targetCalls != 0 {
		t.Fatal("management credential could follow a redirect")
	}
}

func TestCurrentRateRecomputesPeakAndRequiresFreshness(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	until := now.Add(12 * time.Hour)
	s := &RateSnapshot{Status: "ok", FreshUntil: &until, ReceivedAt: &now, Data: map[string]any{
		"billing_scope": "token", "resolved_rate_multiplier": 2.0, "peak_rate_enabled": true,
		"peak_start": "09:00", "peak_end": "18:00", "peak_rate_multiplier": 1.5, "timezone": "UTC",
		"effective_rate_multiplier": 999.0,
	}}
	if value := s.CurrentRate(now); value == nil || *value != 3 {
		t.Fatal(value)
	}
	if value := s.CurrentRate(now.Add(9 * time.Hour)); value == nil || *value != 2 {
		t.Fatal(value)
	}
	if s.CurrentRate(until) != nil {
		t.Fatal("stale rate accepted")
	}
	s.Status = "failed"
	if s.CurrentRate(now) != nil {
		t.Fatal("failed rate accepted")
	}
	s.Status = "ok"
	s.Data["resolved_rate_multiplier"] = -1.0
	if s.CurrentRate(now) != nil {
		t.Fatal("negative rate accepted")
	}
	var empty *RateSnapshot
	if empty.CurrentRate(now) != nil {
		t.Fatal("nil rate accepted")
	}
}

func TestMissingRateRemainsUnknownAndWritesAreNarrow(t *testing.T) {
	var writes []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reply(w, map[string]any{"id": 7, "status": "active", "schedulable": true})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writes = append(writes, body)
		reply(w, map[string]any{"id": 7})
	}))
	defer server.Close()
	client := New(server.URL, "key", time.Second)
	defer client.Close()
	a, err := client.Account(context.Background(), 7)
	if err != nil || a.RateMultiplier != nil {
		t.Fatalf("%+v %v", a, err)
	}
	if err := client.SetSchedulable(context.Background(), 7, false); err != nil {
		t.Fatal(err)
	}
	if err := client.SetPriority(context.Background(), 7, 15); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 || len(writes[0]) != 1 || len(writes[1]) != 1 ||
		writes[0]["schedulable"] != false || writes[1]["priority"] != float64(15) {
		t.Fatal(writes)
	}
}

func TestClearErrorPostsToAccountWithoutPayload(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts/7/clear-error" ||
			r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "" || r.Header.Get("x-api-key") != "key" {
			t.Errorf("unexpected clear-error request: %s %s, body length=%d", r.Method, r.URL, len(body))
		}
		reply(w, map[string]any{"id": 7, "status": "active"})
	}))
	defer server.Close()
	client := New(server.URL+"/api/v1", "key", time.Second)
	defer client.Close()
	if err := client.ClearError(context.Background(), 7); err != nil || calls != 1 {
		t.Fatalf("clear-error calls=%d error=%v", calls, err)
	}
}

func TestTemporaryUnavailableReasonReportsOnlyActiveHostTimers(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	account := Account{Status: "error", RateLimitResetAt: &later, OverloadUntil: &later, TempUnschedulableUntil: &now}
	reason := account.TemporaryUnavailableReason(now)
	if !strings.Contains(reason, "宿主限流") || !strings.Contains(reason, "账号过载") ||
		strings.Contains(reason, "临时不可调度") || strings.Contains(reason, "error") {
		t.Fatalf("incorrect timer report: %q", reason)
	}
	if account.Active(later) || account.AvailabilityReason(later) == "" || account.TemporaryUnavailableReason(later) != "" {
		t.Fatal("expired timers or explicit host status incorrectly cleared")
	}
}
