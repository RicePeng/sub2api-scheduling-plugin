package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestRecentUsageSeparatesLastTwentyCallsFromValidTTFT(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	makeUsage := func(id int64, ttft *int64) Usage {
		return Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Duration(id) * time.Second),
			InputTokens: tokenValue(100), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(300), FirstTokenMs: ttft}
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
				var ttft *int64
				if id%10 == 0 {
					ttft = tokenValue(id * 10)
				}
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
	// TTFT: 100..900 (9 observations), then 1000..1100 (11 observations).
	wantAverage := float64(4500+11550) / 20
	if err != nil || calls != 2 || !w.Complete || !w.FirstTokenComplete || w.Requests != 20 || w.FirstTokenSamples != 20 ||
		w.InputTokens != 2000 || w.CacheReadTokens != 6000 || w.AverageFirstTokenMs == nil || *w.AverageFirstTokenMs != wantAverage {
		t.Fatalf("recent window=%+v calls=%d error=%v", w, calls, err)
	}
	if w.SampleKey == w.FirstTokenSampleKey || w.Ratio() == nil || *w.Ratio() != .75 {
		t.Fatal("independent cache and TTFT samples were lost")
	}
}

func TestRecentUsageInsufficientTTFTAndMissingTokensStayUnknown(t *testing.T) {
	now := time.Now().UTC()
	items := []Usage{}
	for id := int64(1); id <= 20; id++ {
		u := Usage{ID: id, AccountID: 7, CreatedAt: now.Add(-time.Duration(id) * time.Second),
			InputTokens: tokenValue(100), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(300)}
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
	if err != nil || w.Complete || w.Ratio() != nil || w.FirstTokenSamples != 19 || w.AverageFirstTokenMs != nil {
		t.Fatalf("unknown observations treated as known: %+v %v", w, err)
	}
	items[19].FirstTokenMs = tokenValue(0)
	w, err = client.RecentUsage(context.Background(), 7, now)
	if err != nil || w.AverageFirstTokenMs == nil || *w.AverageFirstTokenMs != 0 {
		t.Fatal("valid zero TTFT was discarded")
	}
}

func TestRecentUsageStopsAtSessionBoundaryAndKeepsCacheAtScanLimit(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		window   time.Duration
		pages    int
		requests int
		limited  bool
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
						InputTokens: tokenValue(100), CacheCreationTokens: tokenValue(0), CacheReadTokens: tokenValue(300)}
				}
				reply(w, map[string]any{"items": items})
			}))
			defer server.Close()
			client := New(server.URL, "key", time.Second)
			defer client.Close()
			window, err := client.RecentUsageSince(context.Background(), 7, since, now)
			if err != nil || calls != tc.pages || window.Requests != tc.requests || window.FirstTokenLimitReached != tc.limited ||
				window.FirstTokenSamples != 0 || window.AverageFirstTokenMs != nil || window.Ratio() == nil || *window.Ratio() != .75 {
				t.Fatalf("unexpected sampling result: %+v, pages=%d, error=%v", window, calls, err)
			}
		})
	}
}

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
	if window.Ratio() == nil || *window.Ratio() < .59 {
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
			if err == nil || window.Complete || window.Ratio() != nil || calls > 50 {
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
				if err != nil || !window.Complete || window.Ratio() != nil {
					t.Fatalf(`valid zero tokens rejected: %+v %v`, window, err)
				}
				return
			}
			if err == nil || window.Complete || window.Ratio() != nil {
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
