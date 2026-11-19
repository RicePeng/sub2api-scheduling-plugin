package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
)

type Store interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte) error
}

type API interface {
	Account(context.Context, int64) (adminapi.Account, error)
	RecentUsage(context.Context, int64, time.Time) (adminapi.Window, error)
	RecentUsageSince(context.Context, int64, time.Time, time.Time) (adminapi.Window, error)
	Rates(context.Context) (map[int64]*adminapi.RateSnapshot, error)
	SetSchedulable(context.Context, int64, bool) error
	SetPriority(context.Context, int64, int) error
	Close()
}

type AccountStatus struct {
	ID                     int64      `json:"id"`
	Name                   string     `json:"name"`
	Platform               string     `json:"platform"`
	Type                   string     `json:"type"`
	Group                  string     `json:"group"`
	RateMode               string     `json:"rate_mode"`
	FixedRate              *float64   `json:"fixed_rate"`
	Status                 string     `json:"status"`
	Schedulable            bool       `json:"schedulable"`
	DesiredSchedulable     *bool      `json:"desired_schedulable"`
	Priority               int        `json:"priority"`
	AccountRate            *float64   `json:"account_rate"`
	UpstreamRate           *float64   `json:"upstream_rate"`
	EffectiveRate          *float64   `json:"effective_rate"`
	Requests               int        `json:"requests"`
	InputTokens            int64      `json:"input_tokens"`
	CacheCreationTokens    int64      `json:"cache_creation_tokens"`
	CacheReadTokens        int64      `json:"cache_read_tokens"`
	CacheRatio             *float64   `json:"cache_ratio"`
	Complete               bool       `json:"complete"`
	AverageFirstTokenMs    *float64   `json:"average_first_token_ms"`
	FirstTokenSamples      int        `json:"first_token_samples"`
	FirstTokenLimitReached bool       `json:"first_token_limit_reached,omitempty"`
	RecoveryAt             *time.Time `json:"recovery_at,omitempty"`
	SampleKey              string     `json:"-"`
	FirstTokenSampleKey    string     `json:"-"`
	State                  string     `json:"state"`
	ReadOK                 bool       `json:"read_ok"`
	Availability           string     `json:"availability_reason,omitempty"`
	Reason                 string     `json:"reason"`
	Error                  string     `json:"error"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

func (row *AccountStatus) clearUsage() {
	row.Requests, row.InputTokens, row.CacheCreationTokens, row.CacheReadTokens = 0, 0, 0, 0
	row.CacheRatio, row.AverageFirstTokenMs = nil, nil
	row.Complete, row.FirstTokenSamples, row.FirstTokenLimitReached = false, 0, false
	row.SampleKey, row.FirstTokenSampleKey = "", ""
}

type Event struct {
	Time        time.Time `json:"time"`
	AccountID   int64     `json:"account_id"`
	AccountName string    `json:"account_name,omitempty"`
	Action      string    `json:"action"`
	Message     string    `json:"message"`
}

type Status struct {
	Version        string          `json:"version"`
	Hostname       string          `json:"hostname"`
	Mode           string          `json:"mode"`
	StorageReady   bool            `json:"storage_ready"`
	SamplingActive bool            `json:"sampling_active"`
	SamplingSince  *time.Time      `json:"sampling_since,omitempty"`
	Polling        bool            `json:"polling"`
	LastPoll       *time.Time      `json:"last_poll"`
	NextPoll       *time.Time      `json:"next_poll"`
	Error          string          `json:"error"`
	Accounts       []AccountStatus `json:"accounts"`
	Events         []Event         `json:"events"`
}

type ActionRecord struct {
	AccountID           int64       `json:"account_id"`
	AdminURL            string      `json:"admin_url"`
	Executor            string      `json:"executor"`
	Action              string      `json:"action"`
	Phase               string      `json:"phase"`
	OriginalPriority    int         `json:"original_priority"`
	TargetPriority      int         `json:"target_priority"`
	OriginalSchedulable bool        `json:"original_schedulable"`
	OriginalStatus      string      `json:"original_status"`
	AppliedAt           time.Time   `json:"applied_at"`
	Policy              config.Rule `json:"policy"`
	WindowMinutes       int         `json:"window_minutes"`
	TargetSchedulable   bool        `json:"target_schedulable"`
	Platform            string      `json:"platform,omitempty"`
	Group               string      `json:"group,omitempty"`
	PendingField        string      `json:"pending_field,omitempty"`
	PendingPriority     int         `json:"pending_priority,omitempty"`
	PendingSchedulable  bool        `json:"pending_schedulable"`
	HoldUntil           *time.Time  `json:"hold_until,omitempty"`
	LastBadSample       string      `json:"last_bad_sample,omitempty"`
	SampleSince         *time.Time  `json:"sample_since,omitempty"`
	Reason              string      `json:"reason,omitempty"`
	LastState           string      `json:"last_state,omitempty"`
}

type Ledger struct {
	Records       map[string]*ActionRecord `json:"records"`
	Events        []Event                  `json:"events"`
	SamplingSince time.Time                `json:"sampling_since,omitempty"`
}

type counter struct{ bad, good int }

type Engine struct {
	mu           sync.RWMutex
	cycleMu      sync.Mutex
	cfg          config.Config
	seq          uint64
	store        Store
	cycleCancel  context.CancelFunc
	wake         chan struct{}
	status       Status
	counters     map[int64]counter
	pendingCount int
	newAPI       func(config.Config) API
	clock        func() time.Time
	clearEvents  bool
}

func New(version, hostname string) *Engine {
	return &Engine{
		cfg: config.Defaults(), wake: make(chan struct{}, 1), counters: map[int64]counter{},
		status: Status{Version: version, Hostname: hostname, Mode: "unconfigured",
			Accounts: []AccountStatus{}, Events: []Event{}},
		clock: time.Now,
		newAPI: func(c config.Config) API {
			return adminapi.New(c.AdminURL, c.AdminAPIKey, time.Duration(c.TimeoutSeconds)*time.Second)
		},
	}
}

func (e *Engine) Apply(c config.Config) error {
	e.mu.Lock()
	if bytes.Equal(c.JSON(), e.cfg.JSON()) {
		e.mu.Unlock()
		return nil
	}
	if e.cycleCancel != nil {
		e.cycleCancel()
	}
	e.mu.Unlock()
	// Stop the current cycle before consulting durable ownership. This also
	// handles first startup and writes whose result was not yet published.
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	wasAutomationEnabled := e.cfg.AutomationEnabled
	if c.AdminURL != e.cfg.AdminURL || c.ExecutorHostname != e.cfg.ExecutorHostname {
		if e.store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, err := e.store.Get(ctx, "actions")
			var ledger Ledger
			if err != nil || (len(raw) != 0 && json.Unmarshal(raw, &ledger) != nil) {
				return errors.New("无法核对持久化管理记录，暂不能更改连接或执行节点")
			}
			for _, record := range ledger.Records {
				if record == nil || record.AdminURL != c.AdminURL || record.Executor != c.ExecutorHostname {
					return errors.New("存在未恢复动作：请先保留原连接与执行节点，关闭自动管理并等待恢复")
				}
			}
		} else if e.pendingCount > 0 {
			return errors.New("管理存储不可用且存在未恢复动作，暂不能更改连接或执行节点")
		}
	}
	if !wasAutomationEnabled && c.AutomationEnabled && c.ExecutorHostname == e.status.Hostname {
		if e.store == nil {
			return errors.New("宿主 KV 未就绪，无法重置监控采样")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ledger := &Ledger{Records: map[string]*ActionRecord{}, Events: []Event{}}
		raw, err := e.store.Get(ctx, "actions")
		if err != nil || (len(raw) != 0 && json.Unmarshal(raw, ledger) != nil) || !validLedger(ledger, c.AdminURL, c.ExecutorHostname) {
			return errors.New("无法核对持久化管理记录，暂不能启用自动管理")
		}
		if ledger.Records == nil {
			ledger.Records = map[string]*ActionRecord{}
		}
		// An explicit off/on starts a new monitoring session. Keep unresolved
		// owned settings recoverable, but release last session's manual accounts
		// so their current host values become the next session's baseline.
		for key, record := range ledger.Records {
			if record.Phase == "manual_change" {
				if e.cfg.AdminURL == c.AdminURL && e.cfg.ExecutorHostname == c.ExecutorHostname {
					delete(ledger.Records, key)
				}
				continue
			}
			record.HoldUntil, record.SampleSince = nil, nil
			record.LastBadSample, record.Reason, record.LastState = "", "", ""
		}
		ledger.SamplingSince = e.clock().UTC()
		if err := save(ctx, e.store, ledger); err != nil {
			return fmt.Errorf("无法重置监控采样：%w", err)
		}
		e.status.SamplingSince = &ledger.SamplingSince
	}
	if wasAutomationEnabled != c.AutomationEnabled {
		e.status.SamplingActive = false
		e.status.Polling, e.status.NextPoll = false, nil
		for i := range e.status.Accounts {
			row := &e.status.Accounts[i]
			row.clearUsage()
			row.State, row.Reason, row.RecoveryAt, row.Availability, row.Error = "observing", "", nil, "", ""
		}
	}
	e.cfg, e.seq = c, e.seq+1
	e.counters = map[int64]counter{}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}

// RequestPoll queues a regular cycle without changing configuration, sampling
// boundaries or counters. Health remains a strictly read-only snapshot.
func (e *Engine) RequestPoll() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.AutomationEnabled {
		return errors.New("请先启用监控功能")
	}
	if e.cfg.ExecutorHostname != e.status.Hostname {
		return errors.New("当前节点不是执行节点，不能手动采集")
	}
	if e.status.Polling {
		return nil
	}
	e.status.Polling, e.status.NextPoll = true, nil
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}

func (e *Engine) SetStore(store Store) {
	e.mu.Lock()
	e.store = store
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) ClearEvents() {
	e.mu.Lock()
	e.clearEvents = true
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) Snapshot() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.status
	s.Accounts = append([]AccountStatus{}, e.status.Accounts...)
	s.Events = append([]Event{}, e.status.Events...)
	return s
}

func (e *Engine) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		case <-timer.C:
		}
		e.Cycle(ctx, time.Now().UTC())
		e.mu.RLock()
		interval := time.Duration(e.cfg.PollSeconds) * time.Second
		e.mu.RUnlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
	}
}

func (e *Engine) Cycle(parent context.Context, now time.Time) {
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	e.mu.Lock()
	c, seq, store := e.cfg, e.seq, e.store
	e.cycleCancel = cancel
	s := Status{Version: e.status.Version, Hostname: e.status.Hostname, Mode: "monitoring",
		StorageReady: store != nil, LastPoll: &now, Polling: true, Accounts: []AccountStatus{}, Events: []Event{}}
	e.status.Polling, e.status.NextPoll = true, nil
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if seq == e.seq {
			e.status.Polling = false
		}
	}()
	finish := func(ledger *Ledger) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if seq != e.seq {
			return
		}
		if ledger != nil {
			s.Events = append([]Event{}, ledger.Events...)
			e.pendingCount = len(ledger.Records)
		}
		next := e.clock().UTC().Add(time.Duration(c.PollSeconds) * time.Second)
		s.Polling, s.NextPoll = false, &next
		e.status = s
	}
	if c.AdminURL == "" {
		s.Mode = "unconfigured"
		finish(nil)
		return
	}
	api := e.newAPI(c)
	defer api.Close()
	ledger := &Ledger{Records: map[string]*ActionRecord{}, Events: []Event{}}
	ledgerOK := store != nil
	if store != nil {
		raw, err := store.Get(ctx, "actions")
		if err != nil || (len(raw) != 0 && json.Unmarshal(raw, ledger) != nil) ||
			!validLedger(ledger, c.AdminURL, c.ExecutorHostname) {
			s.Error, ledgerOK = "管理记录存储不可用：自动动作已停止", false
			ledger = &Ledger{Records: map[string]*ActionRecord{}, Events: []Event{}}
		}
		if ledger.Records == nil {
			ledger.Records = map[string]*ActionRecord{}
		}
	}
	executor := c.ExecutorHostname != "" && c.ExecutorHostname == s.Hostname
	if ledgerOK && executor && c.AutomationEnabled && ledger.SamplingSince.IsZero() {
		ledger.SamplingSince = now.UTC()
		if err := save(ctx, store, ledger); err != nil {
			s.Error, ledgerOK = joinError(s.Error, "无法初始化监控采样起点："+err.Error()), false
		}
	}
	if c.AutomationEnabled && !ledger.SamplingSince.IsZero() {
		s.SamplingSince = &ledger.SamplingSince
	}
	e.mu.Lock()
	if e.clearEvents && ledgerOK {
		ledger.Events = []Event{}
		if err := save(ctx, store, ledger); err != nil {
			s.Error = joinError(s.Error, err.Error())
		} else {
			e.clearEvents = false
		}
	}
	e.mu.Unlock()
	if c.AutomationEnabled {
		switch {
		case !executor:
			s.Mode = "standby"
		case !ledgerOK:
			s.Mode, s.Error = "error", "宿主 KV 未就绪，自动管理已暂停"
		default:
			s.Mode = "automatic"
			s.SamplingActive = true
		}
	}
	var rates map[int64]*adminapi.RateSnapshot
	var rateErr error
	if c.UpstreamRatesEnabled {
		rates, rateErr = api.Rates(ctx)
	}
	selected := map[int64]bool{}
	ids := append([]int64{}, c.AccountIDs...)
	for _, id := range ids {
		selected[id] = true
	}
	for _, record := range ledger.Records {
		if !selected[record.AccountID] {
			ids = append(ids, record.AccountID)
		}
	}
	items := []*managedAccount{}
	unknownSelected := false
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		a, err := api.Account(ctx, id)
		setting := accountSetting(c, id)
		item := &managedAccount{setting: setting, selected: selected[id], snapshot: rates[id], row: AccountStatus{ID: id, Group: setting.Group, RateMode: setting.RateMode, FixedRate: setting.FixedRate, DesiredSchedulable: setting.DesiredSchedulable,
			State: "unknown", UpdatedAt: now}}
		row := &item.row
		items = append(items, item)
		if err != nil {
			e.mu.Lock()
			delete(e.counters, id)
			e.mu.Unlock()
			row.Error = err.Error()
			unknownSelected = unknownSelected || selected[id]
			continue
		}
		item.account, item.readOK = a, true
		row.ReadOK = true
		row.Name, row.Status, row.Schedulable, row.Priority = a.Name, a.Status, a.Schedulable, a.Priority
		row.Platform, row.Type = a.Platform, a.Type
		availabilityAccount := a
		// Schedulable is an owned setting, not a host health condition. Capture
		// its initial value for restoration; later changes are reconciled against
		// the durable target in schedule, after monitoring has taken ownership.
		availabilityAccount.Schedulable = true
		row.Availability = availabilityAccount.AvailabilityReason(e.clock().UTC())
		if a.Type != "apikey" {
			row.State, row.Reason = "unsupported", "仅支持 API Key 账号，不采集用量或修改此账号"
			continue
		}
		row.AccountRate, row.State = a.RateMultiplier, "observing"
		if row.Availability != "" {
			row.State, row.Reason = "unavailable", row.Availability
		}
		if rateErr != nil {
			row.Error = rateErr.Error()
		}
		rateNow := e.clock().UTC()
		row.UpstreamRate = item.snapshot.CurrentRate(rateNow)
		applyRateChoice(row, setting, item.snapshot, c.UpstreamRatesEnabled, rateNow)
	}
	// Publish account identity and settings before reading usage. Usage scans can
	// span multiple pages; they must not delay a newly added account showing up.
	preview := s
	preview.Accounts = []AccountStatus{}
	for _, item := range items {
		preview.Accounts = append(preview.Accounts, item.row)
	}
	e.mu.Lock()
	if seq == e.seq {
		preview.Events = append([]Event{}, e.status.Events...)
		// Once a cycle has collected samples, keep the last complete status
		// visible while the next usage request is in flight.
		if !preview.SamplingActive || !e.status.SamplingActive {
			e.status = preview
		}
	}
	e.mu.Unlock()
	for _, item := range items {
		if !s.SamplingActive || !item.readOK || !item.selected || item.account.Type != "apikey" {
			continue
		}
		row := &item.row
		since := ledger.SamplingSince
		if record := ledger.Records[strconv.FormatInt(item.account.ID, 10)]; record != nil && record.SampleSince != nil {
			if since.IsZero() || record.SampleSince.After(since) {
				since = *record.SampleSince
			}
		}
		w, readErr := api.RecentUsageSince(ctx, item.account.ID, since, now)
		row.Requests, row.InputTokens, row.CacheCreationTokens, row.CacheReadTokens =
			w.Requests, w.InputTokens, w.CacheCreationTokens, w.CacheReadTokens
		if row.AccountRate == nil && w.AccountRate != nil {
			row.AccountRate = w.AccountRate
		}
		row.Complete, row.CacheRatio = w.Complete, w.Ratio()
		row.AverageFirstTokenMs, row.FirstTokenSamples = w.AverageFirstTokenMs, w.FirstTokenSamples
		row.FirstTokenLimitReached = w.FirstTokenLimitReached
		row.SampleKey, row.FirstTokenSampleKey = w.SampleKey, w.FirstTokenSampleKey
		if readErr != nil {
			row.Error = joinError(row.Error, readErr.Error())
		}
	}
	if executor && ledgerOK && ctx.Err() == nil {
		if err := e.schedule(ctx, api, store, ledger, c, items, unknownSelected, now); err != nil {
			s.Error = joinError(s.Error, err.Error())
		}
	}
	for _, item := range items {
		s.Accounts = append(s.Accounts, item.row)
	}
	if len(c.AccountIDs) == 0 && len(ledger.Records) == 0 {
		s.Error = joinError(s.Error, "请在配置中填写需要监控的账号 ID")
	}
	finish(ledger)
}

func validLedger(ledger *Ledger, adminURL, executor string) bool {
	if ledger == nil {
		return false
	}
	for key, record := range ledger.Records {
		if record == nil || key != strconv.FormatInt(record.AccountID, 10) || record.AccountID <= 0 ||
			record.AdminURL != adminURL || record.Executor != executor ||
			(record.Action != "pause" && record.Action != "deprioritize" && record.Action != "schedule") ||
			(record.Phase != "pending_apply" && record.Phase != "active" &&
				record.Phase != "pending_restore" && record.Phase != "manual_change") ||
			record.TargetPriority < 0 || record.OriginalPriority < 0 ||
			(record.PendingField != "" && record.PendingField != "priority" && record.PendingField != "schedulable") {
			return false
		}
	}
	return true
}

func joinError(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

func accountSetting(c config.Config, id int64) config.AccountSetting {
	for _, setting := range c.AccountSettings {
		if setting.AccountID == id {
			if setting.Group == "" {
				setting.Group = "temporary"
			}
			if setting.RateMode == "" || setting.RateMode == "account" {
				setting.RateMode = "upstream"
			}
			return setting
		}
	}
	return config.AccountSetting{AccountID: id, Group: "temporary", RateMode: "upstream"}
}

func applyRateChoice(row *AccountStatus, setting config.AccountSetting, snapshot *adminapi.RateSnapshot, upstreamRatesEnabled bool, now time.Time) {
	if setting.Group == "" {
		setting.Group = "temporary"
	}
	if setting.RateMode == "" {
		setting.RateMode = "upstream"
	}
	row.Group, row.RateMode, row.FixedRate = setting.Group, setting.RateMode, setting.FixedRate
	switch setting.RateMode {
	case "fixed":
		row.EffectiveRate = setting.FixedRate
	default:
		if !upstreamRatesEnabled {
			row.RateMode, row.EffectiveRate = "waiting", nil
			return
		}
		row.EffectiveRate = snapshot.CurrentRate(now)
		if row.EffectiveRate == nil {
			row.RateMode = "default"
			fallback := 1.0
			row.EffectiveRate = &fallback
		}
	}
}

func save(ctx context.Context, store Store, ledger *Ledger) error {
	raw, err := json.Marshal(ledger)
	if err != nil || store.Set(ctx, "actions", raw) != nil {
		return errors.New("无法保存管理记录，已停止后续写操作")
	}
	return nil
}

func logEvent(ledger *Ledger, id int64, name, action, message string, now time.Time) {
	ledger.Events = append(ledger.Events, Event{Time: now, AccountID: id, AccountName: name, Action: action, Message: message})
	if len(ledger.Events) > 50 {
		ledger.Events = ledger.Events[len(ledger.Events)-50:]
	}
}
