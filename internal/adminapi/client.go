package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"sub2api.local/cache-manager/internal/config"
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

func (a Account) TemporaryUnavailableReason(now time.Time) string {
	reasons := []string{}
	for _, item := range []struct {
		name  string
		until *time.Time
	}{
		{"宿主限流", a.RateLimitResetAt},
		{"账号过载", a.OverloadUntil},
		{"临时不可调度", a.TempUnschedulableUntil},
	} {
		if item.until != nil && now.Before(*item.until) {
			reasons = append(reasons, fmt.Sprintf("%s至 %s", item.name, item.until.UTC().Format(time.RFC3339)))
		}
	}
	return strings.Join(reasons, "；")
}

type Window struct {
	Requests                int
	InputTokens             int64
	CacheCreationTokens     int64
	CacheReadTokens         int64
	CacheRatio              *float64
	AccountRate             *float64
	Complete                bool
	AverageFirstTokenMs     *float64
	FirstTokenSamples       int
	FirstTokenComplete      bool
	FirstTokenLimitReached  bool
	SampleKey               string
	SampleEndKey            string
	FirstTokenSampleKey     string
	QualityCurrent          *QualityWindow
	QualityWindows          []QualityWindow
	qualityRead             bool
	qualityRows             []Usage
	qualityOldest           Usage
	QualityScanLimitReached bool
	MinInputTokens          int64
	ExcludedSmallRequests   int
	ExcludedUnknownRequests int
}

// A QualityWindow contains consecutive eligible calls across the account.
// Models explain its usage composition but never define window boundaries.
type QualityWindow struct {
	Requests                int
	WindowKnown             bool
	CacheRatio              *float64
	AverageFirstTokenMs     *float64
	FirstTokenSamples       int
	FirstSample             string
	LastSample              string
	MinInputTokens          int64
	ExcludedSmallRequests   int
	ExcludedUnknownRequests int
	Models                  []QualityModelUsage
}

type QualityModelUsage struct {
	Model               string   `json:"model"`
	RequestType         string   `json:"request_type"`
	Requests            int      `json:"requests"`
	CacheRatio          *float64 `json:"cache_ratio"`
	AverageFirstTokenMs *float64 `json:"average_first_token_ms"`
	FirstTokenSamples   int      `json:"first_token_samples"`
}

type qualityTotals struct {
	requests, firstTokenSamples             int
	inputTokens, creationTokens, readTokens int64
	ttftSum                                 float64
}

type qualitySamples struct {
	cache                   [config.QualityWindowRequests]*big.Rat
	ttft                    [config.QualityWindowRequests]int64
	requests, ttftAvailable int
}

func (s *qualitySamples) add(u Usage, total int64) {
	s.cache[s.requests] = new(big.Rat).SetFrac(big.NewInt(*u.CacheReadTokens), big.NewInt(total))
	s.requests++
	if u.FirstTokenMs != nil && *u.FirstTokenMs >= 0 {
		s.ttft[s.ttftAvailable] = *u.FirstTokenMs
		s.ttftAvailable++
	}
}

func (s *qualitySamples) averages() (cache, ttft *float64) {
	cache = qualityCacheAverage(s.cache[:s.requests])
	if s.ttftAvailable == s.requests {
		ttft = qualityAverage(s.ttft[:s.ttftAvailable])
	}
	return cache, ttft
}

func qualityCacheAverage(values []*big.Rat) *float64 {
	if len(values) == 0 {
		return nil
	}
	sum := new(big.Rat)
	for _, value := range values {
		sum.Add(sum, value)
	}
	mean, _ := new(big.Rat).Quo(sum, big.NewRat(int64(len(values)), 1)).Float64()
	return &mean
}

func qualityAverage(values []int64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	if len(values) == config.QualityWindowRequests {
		values = values[2 : len(values)-2]
	}
	sum := new(big.Int)
	for _, value := range values {
		sum.Add(sum, big.NewInt(value))
	}
	mean, _ := new(big.Rat).SetFrac(sum, big.NewInt(int64(len(values)))).Float64()
	return &mean
}

func (s *qualityTotals) add(u Usage) {
	s.requests++
	s.inputTokens += *u.InputTokens
	s.creationTokens += *u.CacheCreationTokens
	s.readTokens += *u.CacheReadTokens
	if u.FirstTokenMs != nil && *u.FirstTokenMs >= 0 {
		s.firstTokenSamples++
		s.ttftSum += float64(*u.FirstTokenMs)
	}
}

func (s qualityTotals) averageTTFT() *float64 {
	if s.firstTokenSamples == 0 {
		return nil
	}
	v := s.ttftSum / float64(s.firstTokenSamples)
	return &v
}

func cacheRatio(input, creation, read int64) *float64 {
	total := input + creation + read
	if total <= 0 {
		return nil
	}
	value := float64(read) / float64(total)
	return &value
}

type Usage struct {
	ID                  int64     `json:"id"`
	AccountID           int64     `json:"account_id"`
	Model               string    `json:"model"`
	RequestType         string    `json:"request_type"`
	CreatedAt           time.Time `json:"created_at"`
	InputTokens         *int64    `json:"input_tokens"`
	CacheCreationTokens *int64    `json:"cache_creation_tokens"`
	CacheReadTokens     *int64    `json:"cache_read_tokens"`
	AccountRate         *float64  `json:"account_rate_multiplier"`
	FirstTokenMs        *int64    `json:"first_token_ms"`
}

func (u Usage) inputContext() (int64, bool) {
	// Reject values that cannot be accumulated safely across a full window.
	const maxComponent = math.MaxInt64 / (3 * config.QualityWindowRequests)
	var total int64
	for _, n := range []*int64{u.InputTokens, u.CacheCreationTokens, u.CacheReadTokens} {
		if n == nil || *n < 0 || *n > maxComponent {
			return 0, false
		}
		total += *n
	}
	return total, true
}

func newerUsage(a, b Usage) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID > b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

// RecentUsage calculates rolling cache and TTFT metrics over the same latest
// eligible calls. TTFT needs an observation for every call in the window.
// A fixed upper timestamp excludes calls arriving during pagination.
func (c *Client) RecentUsage(ctx context.Context, id int64, until time.Time) (Window, error) {
	return c.RecentUsageSince(ctx, id, time.Time{}, until, config.DefaultMinInputTokens)
}

// RecentUsageSince keeps samples from before an explicit resume out of the new
// evaluation window. Account quality windows use the same capacity-eligible calls for
// both metrics and never treat management errors as usage.
func (c *Client) RecentUsageSince(ctx context.Context, id int64, since, until time.Time, minInputTokens int64) (Window, error) {
	const sampleSize, pageSize, maxPages = config.QualityWindowRequests, 100, 50
	w := Window{Complete: true, MinInputTokens: minInputTokens}
	seen := map[int64]struct{}{}
	var rolling qualitySamples
	var previous Usage
	finish := func() {
		w.FirstTokenComplete = w.Complete && rolling.ttftAvailable == sampleSize
		if w.Complete {
			cache, ttft := rolling.averages()
			w.CacheRatio = cache
			if w.FirstTokenComplete {
				w.AverageFirstTokenMs = ttft
			}
		}
	}
	fail := func(err error, invalidateAggregate bool) (Window, error) {
		w.qualityRows = nil // Never seal a window from failed pagination.
		if invalidateAggregate || w.Requests < sampleSize {
			w.Complete = false
		}
		finish()
		return w, err
	}
	query := url.Values{"account_id": {strconv.FormatInt(id, 10)},
		"page_size": {strconv.Itoa(pageSize)}, "sort_by": {"created_at"}, "sort_order": {"desc"},
		"end_date": {until.UTC().Format("2006-01-02")}, "timezone": {"UTC"}}
	if !since.IsZero() {
		query.Set("start_date", since.UTC().Format("2006-01-02"))
	}
	for page := 1; page <= maxPages; page++ {
		var data struct {
			Items    *[]Usage `json:"items"`
			Page     *int     `json:"page"`
			PageSize *int     `json:"page_size"`
			Pages    *int     `json:"pages"`
		}
		query.Set("page", strconv.Itoa(page))
		if err := c.request(ctx, http.MethodGet, "/admin/usage", query, nil, &data); err != nil {
			return fail(err, false)
		}
		if data.Items == nil {
			return fail(errors.New("用量响应缺少 items 列表"), true)
		}
		items := *data.Items
		if len(items) > pageSize || (data.Page != nil && *data.Page != page) ||
			(data.PageSize != nil && *data.PageSize != pageSize) ||
			(data.Pages != nil && (*data.Pages < 1 || (*data.Pages > page && len(items) < pageSize))) {
			return fail(errors.New("用量响应分页不兼容，未执行自动判定"), true)
		}
		for _, u := range items {
			if u.ID <= 0 || u.AccountID != id || u.CreatedAt.IsZero() {
				return fail(errors.New("用量数据缺失或账号不匹配"), true)
			}
		}
		// Host order is (created_at DESC, id DESC), including across pages.
		sort.SliceStable(items, func(i, j int) bool { return newerUsage(items[i], items[j]) })
		reachedStart, advanced := false, false
		for _, u := range items {
			if _, ok := seen[u.ID]; ok {
				continue
			}
			if previous.ID != 0 && newerUsage(u, previous) {
				return fail(errors.New("用量记录顺序不兼容，未执行自动判定"), true)
			}
			previous = u
			advanced = true
			seen[u.ID] = struct{}{}
			if !since.IsZero() && u.CreatedAt.Before(since) {
				reachedStart = true
				break
			}
			if u.CreatedAt.After(until) {
				continue
			}
			if w.AccountRate == nil && u.AccountRate != nil && validNumber(*u.AccountRate) {
				w.AccountRate = u.AccountRate
			}
			w.qualityRows = append(w.qualityRows, u)
			if w.Requests < sampleSize && qualityRequestIdentity(u) {
				total, capacityKnown := u.inputContext()
				if !capacityKnown || total < minInputTokens {
					countExcluded(capacityKnown, &w.ExcludedSmallRequests, &w.ExcludedUnknownRequests)
					continue
				}
				key := sampleKey(u)
				if w.SampleKey == "" {
					w.SampleKey = key
				}
				if w.FirstTokenSampleKey == "" && u.FirstTokenMs != nil && *u.FirstTokenMs >= 0 {
					w.FirstTokenSampleKey = key
				}
				w.Requests++
				w.SampleEndKey = key
				w.InputTokens += *u.InputTokens
				w.CacheCreationTokens += *u.CacheCreationTokens
				w.CacheReadTokens += *u.CacheReadTokens
				rolling.add(u, total)
				w.FirstTokenSamples = rolling.ttftAvailable
			}
		}
		if len(items) == pageSize && !advanced {
			return fail(errors.New("用量分页未前进，未执行自动判定"), true)
		}
		if reachedStart || len(items) < pageSize || (data.Pages != nil && page >= *data.Pages) {
			break
		}
		if page == maxPages {
			w.QualityScanLimitReached = true
			w.FirstTokenLimitReached = w.Requests < sampleSize
			if w.Requests < sampleSize {
				w.Complete = false
			}
		}
	}
	w.qualityOldest, w.qualityRead = previous, true
	finish()
	return w, nil
}

func countExcluded(known bool, small, unknown *int) {
	if known {
		(*small)++
	} else {
		(*unknown)++
	}
}

func ParseSampleKey(key string) (time.Time, int64, bool) {
	i := strings.LastIndex(key, ":")
	if i < 0 {
		return time.Time{}, 0, false
	}
	at, err := time.Parse(time.RFC3339Nano, key[:i])
	id, idErr := strconv.ParseInt(key[i+1:], 10, 64)
	return at, id, err == nil && idErr == nil && id > 0
}

func qualityRequestIdentity(u Usage) bool {
	return strings.TrimSpace(u.Model) != "" && strings.TrimSpace(u.RequestType) != "" &&
		u.RequestType != "unknown" && u.RequestType != "cyber"
}

func sampleKey(u Usage) string {
	return u.CreatedAt.UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(u.ID, 10)
}

// QualityPredecessors recovers the most recent sealed windows from a capped scan
// whose decision boundary is still visible. Only windows with their preceding
// boundary in the scan have trustworthy request and exclusion counts.
func (w Window) QualityPredecessors(since time.Time, through string, limit int) []QualityWindow {
	at, id, ok := ParseSampleKey(through)
	if !ok || limit <= 0 {
		return nil
	}
	end := Usage{CreatedAt: at, ID: id}
	first := -1
	var boundary Usage
	requests := 0
	for i, u := range w.qualityRows {
		if first == -1 {
			if newerUsage(u, end) {
				continue
			}
			if u.ID != id || !u.CreatedAt.Equal(at) {
				return nil
			}
			first = i
		}
		total, known := u.inputContext()
		if !qualityRequestIdentity(u) || !known || total < w.MinInputTokens {
			continue
		}
		if requests > 0 && requests%config.QualityWindowRequests == 0 {
			boundary = u
			if requests/config.QualityWindowRequests == limit {
				break
			}
		}
		requests++
	}
	if first == -1 || boundary.ID == 0 {
		return nil
	}
	w.qualityRows = w.qualityRows[first:]
	w.SealQuality(since, sampleKey(boundary))
	return w.QualityWindows
}

// SealQuality replays the validated usage batch oldest-first after the account's
// durable cursor. The latest verified boundary is required to align a capped scan.
func (w *Window) SealQuality(since time.Time, after string) {
	if !w.qualityRead {
		return
	}
	var anchor Usage
	if at, id, ok := ParseSampleKey(after); ok && !at.Before(since) {
		anchor = Usage{CreatedAt: at, ID: id}
	}
	w.QualityWindows = nil
	known := !w.QualityScanLimitReached || (anchor.ID != 0 && !newerUsage(w.qualityOldest, anchor))
	current := QualityWindow{MinInputTokens: w.MinInputTokens, WindowKnown: known}
	w.QualityCurrent = &current
	if !known {
		return
	}
	type modelKey struct{ model, requestType string }
	samples := qualitySamples{}
	models := map[modelKey]*qualityTotals{}
	finish := func() {
		// A missing TTFT never makes an otherwise complete account window
		// look healthy. Per-model averages are diagnostic and show their sample counts.
		current.CacheRatio, current.AverageFirstTokenMs = samples.averages()
		current.FirstTokenSamples = samples.ttftAvailable
		current.Models = make([]QualityModelUsage, 0, len(models))
		for key, model := range models {
			current.Models = append(current.Models, QualityModelUsage{Model: key.model, RequestType: key.requestType,
				Requests: model.requests, CacheRatio: cacheRatio(model.inputTokens, model.creationTokens, model.readTokens),
				AverageFirstTokenMs: model.averageTTFT(), FirstTokenSamples: model.firstTokenSamples})
		}
		sort.Slice(current.Models, func(i, j int) bool {
			a, b := current.Models[i], current.Models[j]
			if a.Model == b.Model {
				return a.RequestType < b.RequestType
			}
			return a.Model < b.Model
		})
	}
	for i := len(w.qualityRows) - 1; i >= 0; i-- {
		u := w.qualityRows[i]
		if (anchor.ID != 0 && !newerUsage(u, anchor)) || u.CreatedAt.Before(since) {
			continue
		}
		if !qualityRequestIdentity(u) {
			continue
		}
		total, capacityKnown := u.inputContext()
		if !capacityKnown || total < w.MinInputTokens {
			countExcluded(capacityKnown, &current.ExcludedSmallRequests, &current.ExcludedUnknownRequests)
			continue
		}
		sample := sampleKey(u)
		if current.Requests == 0 {
			current.FirstSample = sample
		}
		current.LastSample = sample
		current.Requests++
		samples.add(u, total)
		key := modelKey{u.Model, u.RequestType}
		if models[key] == nil {
			models[key] = &qualityTotals{}
		}
		models[key].add(u)
		if current.Requests == config.QualityWindowRequests {
			finish()
			w.QualityWindows = append(w.QualityWindows, current)
			current = QualityWindow{MinInputTokens: w.MinInputTokens, WindowKnown: true}
			samples, models = qualitySamples{}, map[modelKey]*qualityTotals{}
		}
	}
	finish()
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

func (c *Client) ClearError(ctx context.Context, id int64) error {
	return c.request(ctx, http.MethodPost, fmt.Sprintf("/admin/accounts/%d/clear-error", id), nil, nil, nil)
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
			window.CacheRatio = cacheRatio(window.InputTokens, window.CacheCreationTokens, window.CacheReadTokens)
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
