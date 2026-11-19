package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type Account struct {
	ID                     int64      `json:"id"`
	Name                   string     `json:"name"`
	Platform               string     `json:"platform"`
	Type                   string     `json:"type"`
	Status                 string     `json:"status"`
	Schedulable            bool       `json:"schedulable"`
	Priority               int        `json:"priority"`
	RateMultiplier         *float64   `json:"rate_multiplier"`
	ExpiresAt              *int64     `json:"expires_at"`
	RateLimitResetAt       *time.Time `json:"rate_limit_reset_at"`
	OverloadUntil          *time.Time `json:"overload_until"`
	TempUnschedulableUntil *time.Time `json:"temp_unschedulable_until"`
}

func (a Account) Active(now time.Time) bool {
	if a.Status != "active" || (a.ExpiresAt != nil && *a.ExpiresAt <= now.Unix()) {
		return false
	}
	for _, until := range []*time.Time{a.RateLimitResetAt, a.OverloadUntil, a.TempUnschedulableUntil} {
		if until != nil && now.Before(*until) {
			return false
		}
	}
	return true
}

// AvailabilityReason distinguishes why the host account is not eligible for
// scheduling. It is diagnostic only; it never changes the host account.
func (a Account) AvailabilityReason(now time.Time) string {
	if a.Status != "active" {
		return fmt.Sprintf("宿主账号状态为 %s", a.Status)
	}
	if a.ExpiresAt != nil && *a.ExpiresAt <= now.Unix() {
		return fmt.Sprintf("账号已于 %s 过期", time.Unix(*a.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	for _, item := range []struct {
		name  string
		until *time.Time
	}{
		{"宿主限流", a.RateLimitResetAt},
		{"账号过载", a.OverloadUntil},
		{"临时不可调度", a.TempUnschedulableUntil},
	} {
		if item.until != nil && now.Before(*item.until) {
			return fmt.Sprintf("%s至 %s", item.name, item.until.UTC().Format(time.RFC3339))
		}
	}
	if !a.Schedulable {
		return "宿主已将账号设为不可调度"
	}
	return ""
}

type Window struct {
	Requests               int
	InputTokens            int64
	CacheCreationTokens    int64
	CacheReadTokens        int64
	AccountRate            *float64
	Complete               bool
	AverageFirstTokenMs    *float64
	FirstTokenSamples      int
	FirstTokenComplete     bool
	FirstTokenLimitReached bool
	SampleKey              string
	FirstTokenSampleKey    string
}

func (w Window) Ratio() *float64 {
	total := w.InputTokens + w.CacheCreationTokens + w.CacheReadTokens
	if !w.Complete || w.Requests == 0 || total <= 0 {
		return nil
	}
	value := float64(w.CacheReadTokens) / float64(total)
	return &value
}

type Usage struct {
	ID                  int64     `json:"id"`
	AccountID           int64     `json:"account_id"`
	CreatedAt           time.Time `json:"created_at"`
	InputTokens         *int64    `json:"input_tokens"`
	CacheCreationTokens *int64    `json:"cache_creation_tokens"`
	CacheReadTokens     *int64    `json:"cache_read_tokens"`
	AccountRate         *float64  `json:"account_rate_multiplier"`
	FirstTokenMs        *int64    `json:"first_token_ms"`
}

// RecentUsage reads the latest 20 calls for cache metrics, and independently
// the latest 20 valid TTFT observations. A fixed upper timestamp excludes calls
// arriving during pagination. Missing/negative TTFT never becomes zero.
func (c *Client) RecentUsage(ctx context.Context, id int64, until time.Time) (Window, error) {
	return c.RecentUsageSince(ctx, id, time.Time{}, until)
}

// RecentUsageSince keeps samples from before an explicit resume out of the new
// evaluation window.
func (c *Client) RecentUsageSince(ctx context.Context, id int64, since, until time.Time) (Window, error) {
	w := Window{Complete: true, FirstTokenComplete: true}
	seen := map[int64]bool{}
	var ttftSum float64
	var previous time.Time
	query := url.Values{"account_id": {strconv.FormatInt(id, 10)},
		"page_size": {"100"}, "sort_by": {"created_at"}, "sort_order": {"desc"},
		"end_date": {until.UTC().Format("2006-01-02")}, "timezone": {"UTC"}}
	if !since.IsZero() {
		query.Set("start_date", since.UTC().Format("2006-01-02"))
	}
	for page := 1; page <= 50; page++ {
		var data struct {
			Items *[]Usage `json:"items"`
		}
		query.Set("page", strconv.Itoa(page))
		if err := c.request(ctx, http.MethodGet, "/admin/usage", query, nil, &data); err != nil {
			w.Complete, w.FirstTokenComplete, w.AverageFirstTokenMs = false, false, nil
			return w, err
		}
		if data.Items == nil {
			w.Complete, w.FirstTokenComplete = false, false
			return w, errors.New("用量响应缺少 items 列表")
		}
		items := *data.Items
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID > items[j].ID
			}
			return items[i].CreatedAt.After(items[j].CreatedAt)
		})
		for _, u := range items {
			if u.ID <= 0 || u.AccountID != id || u.CreatedAt.IsZero() {
				w.Complete, w.FirstTokenComplete, w.AverageFirstTokenMs = false, false, nil
				return w, errors.New("用量数据缺失或账号不匹配")
			}
			// Results are sorted newest first, so the next pages cannot add
			// any samples from this session once we cross its start time.
			if !since.IsZero() && u.CreatedAt.Before(since) {
				break
			}
			if u.CreatedAt.After(until) || seen[u.ID] {
				continue
			}
			if !previous.IsZero() && u.CreatedAt.After(previous) {
				w.Complete, w.FirstTokenComplete = false, false
				return w, errors.New("用量记录顺序不兼容，未执行自动判定")
			}
			previous = u.CreatedAt
			seen[u.ID] = true
			key := u.CreatedAt.UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(u.ID, 10)
			if w.Requests < 20 {
				if w.SampleKey == "" {
					w.SampleKey = key
				}
				w.Requests++
				if u.InputTokens == nil || u.CacheCreationTokens == nil || u.CacheReadTokens == nil ||
					*u.InputTokens < 0 || *u.CacheCreationTokens < 0 || *u.CacheReadTokens < 0 {
					w.Complete = false
				} else {
					w.InputTokens += *u.InputTokens
					w.CacheCreationTokens += *u.CacheCreationTokens
					w.CacheReadTokens += *u.CacheReadTokens
				}
				if w.AccountRate == nil && u.AccountRate != nil && validNumber(*u.AccountRate) {
					w.AccountRate = u.AccountRate
				}
			}
			if w.FirstTokenSamples < 20 && u.FirstTokenMs != nil && *u.FirstTokenMs >= 0 {
				if w.FirstTokenSampleKey == "" {
					w.FirstTokenSampleKey = key
				}
				w.FirstTokenSamples++
				ttftSum += float64(*u.FirstTokenMs)
			}
			if w.Requests == 20 && w.FirstTokenSamples == 20 {
				value := ttftSum / 20
				w.AverageFirstTokenMs = &value
				return w, nil
			}
		}
		if len(items) < 100 || (!since.IsZero() && items[len(items)-1].CreatedAt.Before(since)) {
			return w, nil
		}
	}
	w.FirstTokenComplete = false
	w.FirstTokenLimitReached = true
	if w.Requests < 20 {
		w.Complete = false
	}
	return w, nil
}

type RateSnapshot struct {
	Status     string         `json:"status"`
	FreshUntil *time.Time     `json:"fresh_until"`
	ReceivedAt *time.Time     `json:"received_at"`
	Data       map[string]any `json:"data"`
}

type RateItem struct {
	AccountID int64         `json:"account_id"`
	Snapshot  *RateSnapshot `json:"snapshot"`
}

type Client struct {
	base string
	key  string
	http *http.Client
}

func New(base, key string, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{base: base, key: key, http: &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body, output any) error {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return errors.New("管理请求参数无法编码")
		}
		input = bytes.NewReader(raw)
	}
	endpoint := c.base + path
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return errors.New("管理地址无效")
	}
	request.Header.Set("x-api-key", c.key)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return errors.New("管理 API 连接失败或超时")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return errors.New("管理 API 未授权：请检查管理密钥")
		case http.StatusForbidden:
			return errors.New("管理 API 权限不足：当前密钥未开放此操作")
		case http.StatusNotFound:
			return errors.New("管理 API 或账号不存在，或账号超出授权范围")
		default:
			return fmt.Errorf("管理 API 返回 HTTP %d", response.StatusCode)
		}
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(raw) > 8*1024*1024 || json.Unmarshal(raw, &envelope) != nil ||
		envelope.Code == nil || *envelope.Code != 0 || len(envelope.Data) == 0 {
		return errors.New("管理 API 响应无效或过大")
	}
	if output != nil && json.Unmarshal(envelope.Data, output) != nil {
		return errors.New("管理 API 数据格式不兼容")
	}
	return nil
}

func (c *Client) Account(ctx context.Context, id int64) (Account, error) {
	var account Account
	err := c.request(ctx, http.MethodGet, fmt.Sprintf("/admin/accounts/%d", id), nil, nil, &account)
	if err == nil && account.ID != id {
		err = errors.New("管理 API 返回了不匹配的账号")
	}
	if account.RateMultiplier != nil && !validNumber(*account.RateMultiplier) {
		account.RateMultiplier = nil
	}
	return account, err
}

func (c *Client) SetSchedulable(ctx context.Context, id int64, value bool) error {
	return c.request(ctx, http.MethodPost, fmt.Sprintf("/admin/accounts/%d/schedulable", id),
		nil, map[string]bool{"schedulable": value}, nil)
}

func (c *Client) SetPriority(ctx context.Context, id int64, value int) error {
	return c.request(ctx, http.MethodPut, fmt.Sprintf("/admin/accounts/%d", id),
		nil, map[string]int{"priority": value}, nil)
}

// UsageWindow uses the host's date-only filters, then enforces the exact UTC
// window locally. A bounded or malformed result is explicitly incomplete.
func (c *Client) UsageWindow(ctx context.Context, id int64, from, until time.Time) (Window, error) {
	window := Window{Complete: true}
	seen := map[int64]bool{}
	const pageSize, maxPages = 100, 50
	for page := 1; page <= maxPages; page++ {
		query := url.Values{
			"account_id": {strconv.FormatInt(id, 10)}, "page": {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)}, "sort_by": {"created_at"}, "sort_order": {"desc"},
			"start_date": {from.UTC().Format("2006-01-02")}, "end_date": {until.UTC().Format("2006-01-02")},
			"timezone": {"UTC"},
		}
		var data struct {
			Items *[]Usage `json:"items"`
		}
		if err := c.request(ctx, http.MethodGet, "/admin/usage", query, nil, &data); err != nil {
			window.Complete = false
			return window, err
		}
		if data.Items == nil {
			window.Complete = false
			return window, errors.New("缓存用量响应缺少 items 列表")
		}
		reachedStart := false
		for _, usage := range *data.Items {
			if usage.ID <= 0 || usage.AccountID != id || usage.CreatedAt.IsZero() ||
				usage.InputTokens == nil || usage.CacheCreationTokens == nil || usage.CacheReadTokens == nil ||
				*usage.InputTokens < 0 || *usage.CacheCreationTokens < 0 || *usage.CacheReadTokens < 0 {
				window.Complete = false
				return window, errors.New("缓存用量数据缺失或不匹配")
			}
			if usage.CreatedAt.Before(from) {
				reachedStart = true
				continue
			}
			if usage.CreatedAt.After(until) || seen[usage.ID] {
				continue
			}
			seen[usage.ID] = true
			window.Requests++
			window.InputTokens += *usage.InputTokens
			window.CacheCreationTokens += *usage.CacheCreationTokens
			window.CacheReadTokens += *usage.CacheReadTokens
			if window.AccountRate == nil && usage.AccountRate != nil && validNumber(*usage.AccountRate) {
				window.AccountRate = usage.AccountRate
			}
		}
		if len(*data.Items) < pageSize || reachedStart {
			return window, nil
		}
	}
	window.Complete = false
	return window, errors.New("缓存窗口超过 5000 条记录，请缩短统计窗口")
}

func (c *Client) Rates(ctx context.Context) (map[int64]*RateSnapshot, error) {
	result := map[int64]*RateSnapshot{}
	for page := 1; page <= 100; page++ {
		var data struct {
			Items []RateItem `json:"items"`
		}
		err := c.request(ctx, http.MethodGet, "/admin/accounts/upstream-billing-rates",
			url.Values{"page": {strconv.Itoa(page)}, "page_size": {"100"}}, nil, &data)
		if err != nil {
			return nil, err
		}
		for _, item := range data.Items {
			result[item.AccountID] = item.Snapshot
		}
		if len(data.Items) < 100 {
			return result, nil
		}
	}
	return nil, errors.New("倍率账号列表超过采集上限")
}

// CurrentRate re-evaluates peak time at collection time instead of reusing a
// snapshot's effective value across a peak boundary.
func (s *RateSnapshot) CurrentRate(now time.Time) *float64 {
	if s == nil || s.Status != "ok" || s.FreshUntil == nil || !now.Before(*s.FreshUntil) ||
		s.ReceivedAt == nil || s.ReceivedAt.After(now.Add(30*time.Second)) || s.Data["billing_scope"] != "token" {
		return nil
	}
	base, ok := number(s.Data["resolved_rate_multiplier"])
	if !ok {
		return nil
	}
	peakEnabled, ok := s.Data["peak_rate_enabled"].(bool)
	if !ok {
		return nil
	}
	if peakEnabled {
		start, a := clockMinute(s.Data["peak_start"])
		end, b := clockMinute(s.Data["peak_end"])
		zone, c := s.Data["timezone"].(string)
		peak, d := number(s.Data["peak_rate_multiplier"])
		location, err := time.LoadLocation(zone)
		if !a || !b || !c || !d || start >= end || err != nil {
			return nil
		}
		local := now.In(location)
		minute := local.Hour()*60 + local.Minute()
		if minute >= start && minute < end {
			base *= peak
		}
	}
	if !validNumber(base) {
		return nil
	}
	return &base
}

func validNumber(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func number(value any) (float64, bool) {
	n, ok := value.(float64)
	return n, ok && validNumber(n)
}
func clockMinute(value any) (int, bool) {
	s, ok := value.(string)
	if !ok {
		return 0, false
	}
	t, err := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute(), err == nil
}
