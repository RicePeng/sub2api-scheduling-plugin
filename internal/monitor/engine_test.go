package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
)

type memoryStore struct {
	raw               []byte
	fail              bool
	extra             map[string][]byte
	maxValueSize      int
	historyWriteError bool
}

type failFallbackIntentStore struct {
	*memoryStore
	failed bool
}

func (s *failFallbackIntentStore) Set(ctx context.Context, key string, value []byte) error {
	var ledger Ledger
	if key == "actions" && !s.failed && json.Unmarshal(value, &ledger) == nil {
		for _, record := range ledger.Records {
			if record != nil && record.FallbackPending {
				s.failed = true
				return errors.New("one-time fallback intent failure")
			}
		}
	}
	return s.memoryStore.Set(ctx, key, value)
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	if key == "actions" {
		return append([]byte{}, s.raw...), nil
	}
	return append([]byte{}, s.extra[key]...), nil
}
func (s *memoryStore) Set(_ context.Context, key string, value []byte) error {
	if s.fail {
		return errors.New("storage failed")
	}
	if s.historyWriteError && strings.HasPrefix(key, "quality-history.") {
		return errors.New("history storage failed")
	}
	if s.maxValueSize > 0 && len(value) > s.maxValueSize {
		return errors.New("storage value exceeds host cap")
	}
	if key == "actions" {
		s.raw = append([]byte{}, value...)
	} else {
		if s.extra == nil {
			s.extra = map[string][]byte{}
		}
		s.extra[key] = append([]byte{}, value...)
	}
	return nil
}

type fakeAPI struct {
	accounts           map[int64]adminapi.Account
	accountErrors      map[int64]error
	usage              map[int64]adminapi.Window
	rates              map[int64]*adminapi.RateSnapshot
	rateError          error
	rateReads          int
	usageReads, writes int
	clearCalls         int
	writeError         bool
	priorityWriteError map[int64]error
	ignoreWrite        bool
	clearError         bool
	beforeWrite        func()
	beforeAccount      func(int64)
	lastUsageSince     time.Time
	usageThresholds    map[int64]int64
}

func (f *fakeAPI) Account(_ context.Context, id int64) (adminapi.Account, error) {
	if f.beforeAccount != nil {
		f.beforeAccount(id)
	}
	if err := f.accountErrors[id]; err != nil {
		return adminapi.Account{}, err
	}
	a, ok := f.accounts[id]
	if !ok {
		return adminapi.Account{}, errors.New("not found")
	}
	return a, nil
}
func (f *fakeAPI) RecentUsageSince(_ context.Context, id int64, since, _ time.Time, minInputTokens int64) (adminapi.Window, error) {
	f.usageReads++
	f.lastUsageSince = since
	if f.usageThresholds == nil {
		f.usageThresholds = map[int64]int64{}
	}
	f.usageThresholds[id] = minInputTokens
	w := f.usage[id]
	w.MinInputTokens = minInputTokens
	if w.QualityCurrent != nil {
		current := *w.QualityCurrent
		current.MinInputTokens = minInputTokens
		w.QualityCurrent = &current
	}
	for i := range w.QualityWindows {
		w.QualityWindows[i].MinInputTokens = minInputTokens
	}
	return w, nil
}
func (f *fakeAPI) Rates(context.Context) (map[int64]*adminapi.RateSnapshot, error) {
	f.rateReads++
	return f.rates, f.rateError
}
func (f *fakeAPI) Close() {}
func (f *fakeAPI) ClearError(_ context.Context, id int64) error {
	f.clearCalls++
	if f.clearError {
		return errors.New("clear failed")
	}
	a := f.accounts[id]
	a.RateLimitResetAt, a.OverloadUntil, a.TempUnschedulableUntil = nil, nil, nil
	f.accounts[id] = a
	return nil
}
func (f *fakeAPI) SetSchedulable(_ context.Context, id int64, value bool) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	f.writes++
	a := f.accounts[id]
	a.Schedulable = value
	if !f.ignoreWrite {
		f.accounts[id] = a
	}
	if f.writeError {
		return errors.New("write outcome unknown")
	}
	return nil
}
func (f *fakeAPI) SetPriority(_ context.Context, id int64, value int) error {
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	f.writes++
	if err := f.priorityWriteError[id]; err != nil {
		return err
	}
	a := f.accounts[id]
	a.Priority = value
	if !f.ignoreWrite {
		f.accounts[id] = a
	}
	if f.writeError {
		return errors.New("write outcome unknown")
	}
	return nil
}

func account(id int64, platform, kind string, priority int, schedulable bool) adminapi.Account {
	rate := .1
	return adminapi.Account{ID: id, Name: "test", Platform: platform, Type: kind, Status: "active", Schedulable: schedulable, Priority: priority, RateMultiplier: &rate}
}
func lowCache() adminapi.Window {
	return adminapi.Window{Complete: true, Requests: 20, InputTokens: 100, CacheReadTokens: 300, CacheRatio: ptr(.75)}
}
func setup(t *testing.T, accounts ...adminapi.Account) (*Engine, config.Config, *fakeAPI, *memoryStore, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{accounts: map[int64]adminapi.Account{}, usage: map[int64]adminapi.Window{}}
	c := config.Defaults()
	// Existing scheduler fixtures exercise single-window cache and TTFT rules.
	// Production defaults intentionally use five independent TTFT periods.
	c.Rule.FirstTokenPeriods = 1
	for group, rule := range c.GroupRules {
		rule.FirstTokenPeriods = 1
		c.GroupRules[group] = rule
	}
	c.AdminURL, c.AdminAPIKey, c.ExecutorHostname = "http://test/api/v1", "test-key", "node-a"
	c.AutomationEnabled = true
	for _, a := range accounts {
		c.AccountIDs = append(c.AccountIDs, a.ID)
		api.accounts[a.ID] = a
		api.usage[a.ID] = lowCache()
		c.AccountSettings = append(c.AccountSettings, config.AccountSetting{AccountID: a.ID, Platform: a.Platform, Group: "general", RateMode: "fixed", FixedRate: a.RateMultiplier})
	}
	e := New("test", "node-a")
	e.clock = func() time.Time { return now }
	e.newAPI = func(config.Config) API { return api }
	store := &memoryStore{}
	e.SetStore(store)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	return e, c, api, store, now
}
func snapshotAccount(t *testing.T, e *Engine, id int64) AccountStatus {
	t.Helper()
	for _, a := range e.Snapshot().Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %d missing: %+v", id, e.Snapshot())
	return AccountStatus{}
}
func ledgerOf(t *testing.T, s *memoryStore) Ledger {
	t.Helper()
	var l Ledger
	if err := json.Unmarshal(s.raw, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestEnabledSyncsGroupPriorityAndStopRestoresOriginal(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	api.beforeWrite = func() {
		r := ledgerOf(t, store).Records["7"]
		if r == nil || r.PendingField == "" {
			t.Fatal("schedule write lacked durable intent")
		}
	}
	e.Cycle(context.Background(), now)
	if a := api.accounts[7]; a.Priority != 8 || !a.Schedulable || api.writes != 1 {
		t.Fatalf("group priority not synchronized: %+v writes=%d", a, api.writes)
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if a := api.accounts[7]; a.Priority != 5 || !a.Schedulable || len(ledgerOf(t, store).Records) != 0 {
		t.Fatalf("owned setting not restored: %+v ledger=%+v", a, ledgerOf(t, store))
	}
}

func TestInitialSchedulerSwitchIsManagedAndRestored(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "initially_closed", true: "initially_open"}[initial], func(t *testing.T) {
			a := account(7, "openai", "apikey", 5, initial)
			a.RateMultiplier = ptr(.1)
			e, c, api, store, now := setup(t, a)
			api.usage[7] = adminapi.Window{}
			c.AutomationEnabled = false
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			e.Cycle(context.Background(), now)
			if row := snapshotAccount(t, e, 7); row.State != "observing" || row.Availability != "" || api.writes != 0 {
				t.Fatalf("initial scheduler switch was treated as a runtime fault: %+v writes=%d", row, api.writes)
			}
			c.AutomationEnabled = true
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				e.Cycle(context.Background(), now.Add(time.Duration(i)*time.Minute))
				if row := snapshotAccount(t, e, 7); row.State != "sampling" || !row.Schedulable || row.Priority != 8 || row.Availability != "" {
					t.Fatalf("current group did not open for new samples: %+v", row)
				}
			}
			c.AutomationEnabled = false
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			for i := 3; i <= 4; i++ {
				e.Cycle(context.Background(), now.Add(time.Duration(i)*time.Minute))
				if row := snapshotAccount(t, e, 7); row.State != "observing" || row.Schedulable != initial || row.Priority != 5 || row.Availability != "" || row.RecoveryAt != nil {
					t.Fatalf("stopped account retained a runtime fault or plugin settings: %+v", row)
				}
			}
			if a := api.accounts[7]; a.Schedulable != initial || a.Priority != 5 || len(ledgerOf(t, store).Records) != 0 {
				t.Fatalf("original host settings were not restored: %+v", a)
			}
		})
	}
}

func TestRuntimeSchedulerChangeIsDetectedAfterOwnershipAndHiddenWhenStopped(t *testing.T) {
	a := account(7, "openai", "apikey", 5, false)
	a.RateMultiplier = ptr(.1)
	e, c, api, store, now := setup(t, a)
	e.Cycle(context.Background(), now)
	a = api.accounts[7]
	a.Schedulable = false
	api.accounts[7] = a
	writes := api.writes
	e.Cycle(context.Background(), now.Add(time.Minute))
	if row := snapshotAccount(t, e, 7); row.State != "manual_change" || !strings.Contains(row.Reason, "运行监控后") || !strings.Contains(row.Reason, "不可调度") || api.writes != writes {
		t.Fatalf("external scheduler change was not identified accurately: %+v writes=%d", row, api.writes)
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if row := snapshotAccount(t, e, 7); row.State != "observing" || row.Reason != "" || row.Availability != "" {
		t.Fatalf("stopping retained the previous runtime exception: %+v", row)
	}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	if row := snapshotAccount(t, e, 7); row.State != "observing" || row.Reason != "" || row.Availability != "" || api.writes != writes || api.accounts[7].Schedulable {
		t.Fatalf("stopped monitoring retained a runtime fault or overwrote the external setting: %+v", row)
	}
	if ledgerOf(t, store).Records["7"].Phase != "manual_change" {
		t.Fatal("manual-change protection was discarded")
	}
}

func TestGroupClosureIsNotHostUnavailableAndRestoresOnStop(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 3, true), account(8, "openai", "apikey", 4, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		e.Cycle(context.Background(), now.Add(time.Duration(i)*time.Minute))
		if row := snapshotAccount(t, e, 8); row.State != "group_wait" || row.Schedulable || row.Availability != "" {
			t.Fatalf("plugin closure was mistaken for host unavailability: %+v", row)
		}
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	for _, id := range []int64{7, 8} {
		if row := snapshotAccount(t, e, id); row.State != "observing" || !row.Schedulable || row.Availability != "" || row.Priority != int(id-4) {
			t.Fatalf("group did not leave monitoring cleanly: %+v", row)
		}
		if !api.accounts[id].Schedulable {
			t.Fatal("plugin-owned group closure was not restored")
		}
	}
	if len(ledgerOf(t, store).Records) != 0 {
		t.Fatal("restored ownership records remained")
	}
}

func TestReenablingAutomationStartsANewSamplingWindow(t *testing.T) {
	a := account(7, "openai", "apikey", 5, true)
	a.RateMultiplier = ptr(.1)
	e, c, api, store, now := setup(t, a)
	current := now
	e.clock = func() time.Time { return current }
	e.Cycle(context.Background(), now)
	if !e.Snapshot().SamplingActive || snapshotAccount(t, e, 7).CacheRatio == nil {
		t.Fatal("expected active sampling before stopping")
	}
	reads := api.usageReads

	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().SamplingActive || snapshotAccount(t, e, 7).Requests != 0 {
		t.Fatal("disabled status retained old usage before the next poll")
	}
	current = now.Add(time.Minute)
	e.Cycle(context.Background(), current)
	stopped := snapshotAccount(t, e, 7)
	if api.usageReads != reads || stopped.Requests != 0 || stopped.CacheRatio != nil || stopped.AverageFirstTokenMs != nil || stopped.FirstTokenSamples != 0 {
		t.Fatalf("disabled cycle read or exposed usage: %+v", stopped)
	}
	if stopped.Priority != 5 || stopped.Name != "test" || stopped.EffectiveRate == nil {
		t.Fatalf("disabled cycle lost restored priority, identity or multiplier: %+v", stopped)
	}

	current = now.Add(2 * time.Minute)
	c.AutomationEnabled = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if got := ledgerOf(t, store).SamplingSince; !got.Equal(current) {
		t.Fatalf("activation boundary=%s, want %s", got, current)
	}
	if e.Snapshot().SamplingActive || snapshotAccount(t, e, 7).Requests != 0 {
		t.Fatal("re-enabled status exposed usage before sampling")
	}
	api.usage[7] = adminapi.Window{}
	api.lastUsageSince = time.Time{}
	e.Cycle(context.Background(), current)
	if !api.lastUsageSince.Equal(current) {
		t.Fatalf("usage query reused calls before activation: since=%s, want %s", api.lastUsageSince, current)
	}
	if state := snapshotAccount(t, e, 7); !e.Snapshot().SamplingActive || state.Requests != 0 || state.State != "sampling" || !state.Schedulable {
		t.Fatalf("current group did not open in waiting-for-samples state: %+v", state)
	}
}

func TestUnavailableStatusKeepsSpecificHostReason(t *testing.T) {
	a := account(7, "openai", "apikey", 5, true)
	until := time.Date(2026, 11, 18, 12, 5, 0, 0, time.UTC)
	a.OverloadUntil = &until
	e, _, _, _, now := setup(t, a)
	e.Cycle(context.Background(), now)
	state := snapshotAccount(t, e, 7)
	if state.State != "temporary_retry" || state.FailureKind != "temporary" || state.Availability == "" ||
		state.Reason != state.Availability || !strings.Contains(state.Reason, "过载") {
		t.Fatalf("specific host unavailability reason was lost: %+v", state)
	}
}

func TestTemporaryGroupIsClosedAtPriority99AndRestoredWhenStopped(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 4, true))
	c.AccountSettings[0].Group = "temporary"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if a := api.accounts[7]; a.Priority != 99 || a.Schedulable {
		t.Fatalf("temporary account remained callable: %+v", a)
	}
	if got := snapshotAccount(t, e, 7).State; got != "temporary" {
		t.Fatalf("state=%s", got)
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if a := api.accounts[7]; a.Priority != 4 || !a.Schedulable || len(ledgerOf(t, store).Records) != 0 {
		t.Fatalf("temporary state not restored: %+v", a)
	}
}

func TestOnlyAPIKeysAreReadForUsageOrManaged(t *testing.T) {
	e, _, api, _, now := setup(t, account(7, "openai", "oauth", 5, true))
	e.Cycle(context.Background(), now)
	if api.usageReads != 0 || api.writes != 0 {
		t.Fatalf("unsupported account was touched: reads=%d writes=%d", api.usageReads, api.writes)
	}
	if got := snapshotAccount(t, e, 7).State; got != "unsupported" {
		t.Fatalf("state=%s", got)
	}
}

func TestAccountReadFailureBlocksOnlyItsPlatform(t *testing.T) {
	e, c, api, _, now := setup(t,
		account(7, "openai", "apikey", 3, true),
		account(8, "openai", "apikey", 4, false),
		account(9, "anthropic", "apikey", 3, true),
		account(10, "anthropic", "apikey", 4, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	c.AccountSettings[2].Group, c.AccountSettings[3].Group = "excellent", "good"
	api.accountErrors = map[int64]error{7: errors.New("read unavailable")}
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.accounts[8].Schedulable || !api.accounts[9].Schedulable || api.accounts[10].Schedulable ||
		snapshotAccount(t, e, 7).State != "unknown" || snapshotAccount(t, e, 8).State != "group_wait" {
		t.Fatalf("read uncertainty escaped its platform or opened an unsafe same-platform group: %+v", e.Snapshot().Accounts)
	}
}

func TestMonitoringSummaryRequiresConfirmedAccountState(t *testing.T) {
	c := config.Defaults()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	item := &managedAccount{selected: true, readOK: true,
		account: adminapi.Account{ID: 7, Type: "apikey", Status: "active", Schedulable: true},
		row:     AccountStatus{ID: 7, State: "ready", Schedulable: true}}
	if state, _ := monitoringSummary(&Ledger{}, c, []*managedAccount{item}, 0, now); state != "stable" {
		t.Fatalf("confirmed account was not reported stable: %s", state)
	}
	item.row.State = "account_error"
	if state, _ := monitoringSummary(&Ledger{}, c, []*managedAccount{item}, 0, now); state != "unknown" {
		t.Fatalf("host account error was reported as %s", state)
	}
	item.row.State = "ready"
	item.record = &ActionRecord{PendingField: "schedulable"}
	if state, _ := monitoringSummary(&Ledger{}, c, []*managedAccount{item}, 0, now); state != "unknown" {
		t.Fatalf("unconfirmed write was reported as %s", state)
	}
}

func TestFallbackRecoveryTriggersOnceFromEarliestValidGroup(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 6, false),
		account(8, "openai", "apikey", 7, false), account(9, "openai", "apikey", 9, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group, c.AccountSettings[2].Group = "excellent", "good", "reserve"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.Cycle(ctx, now)
	hold := now.Add(time.Hour)
	ledger := ledgerOf(t, store)
	for _, id := range []string{"7", "8"} {
		r := ledger.Records[id]
		r.FailureKind, r.HoldUntil, r.TargetSchedulable = "quality", &hold, false
	}
	if err := save(ctx, store, &ledger); err != nil {
		t.Fatal(err)
	}
	host := api.accounts[7]
	host.Schedulable = false
	api.accounts[7] = host
	e.Cycle(ctx, now.Add(time.Minute))
	if !api.accounts[9].Schedulable {
		t.Fatal("healthy reserve did not open")
	}
	host = api.accounts[9]
	host.Status = "error" // The host rejects routing even if the switch remains on.
	api.accounts[9] = host
	api.accountErrors = map[int64]error{8: errors.New("read unavailable")}
	e.Cycle(ctx, now.Add(2*time.Minute))
	if e.Snapshot().OverallState != "unknown" || ledgerOf(t, store).FallbackTriggered || api.accounts[7].Schedulable {
		t.Fatal("uncertain host state consumed the emergency retry")
	}
	delete(api.accountErrors, 8)
	at := now.Add(3 * time.Minute)
	api.ignoreWrite = true
	e.Cycle(ctx, at)
	ledger = ledgerOf(t, store)
	if ledger.FallbackTriggered || !ledger.Records["7"].FallbackPending || ledger.Records["7"].PendingField != "schedulable" || api.accounts[7].Schedulable {
		t.Fatalf("unconfirmed fallback open was consumed or lost: ledger=%+v host=%+v", ledger, api.accounts[7])
	}
	api.ignoreWrite = false
	at = at.Add(time.Minute)
	e.Cycle(ctx, at)
	ledger, status := ledgerOf(t, store), e.Snapshot()
	if !status.FallbackTriggered || status.OverallState != "anomaly_detected" || !api.accounts[7].Schedulable ||
		api.accounts[7].Priority != c.GroupPriorities["excellent"] || api.accounts[8].Schedulable || api.clearCalls != 0 ||
		ledger.Records["7"].RecoveryStartedAt == nil || !ledger.Records["7"].RecoveryStartedAt.Equal(at) {
		t.Fatalf("fallback did not start the earliest valid group at its original priority: %+v", status)
	}
	host = api.accounts[7]
	host.Status = "error"
	api.accounts[7] = host
	restarted := restartMonitor(t, e, c, store, at.Add(time.Minute))
	restarted.Cycle(ctx, at.Add(time.Minute))
	status = restarted.Snapshot()
	if status.OverallState != "anomaly_detected" || status.FallbackAt == nil || !status.FallbackAt.Equal(at) || api.accounts[8].Schedulable {
		t.Fatal("restarting the process re-armed the one-shot fallback or lost the warning")
	}
}

func TestFallbackDoesNotStartSecondRecoveryWhileTrialIsActive(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 6, true),
		account(8, "openai", "apikey", 7, false), account(9, "openai", "apikey", 9, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group, c.AccountSettings[2].Group = "excellent", "good", "reserve"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)

	ledger := ledgerOf(t, store)
	trialStarted, trialDeadline, hold := now, now.Add(15*time.Minute), now.Add(time.Hour)
	trial := ledger.Records["7"]
	trial.FailureKind, trial.RecoveryTrial = "quality", true
	trial.RecoveryStartedAt, trial.RecoveryDeadline = &trialStarted, &trialDeadline
	waiting := ledger.Records["8"]
	waiting.FailureKind, waiting.HoldUntil, waiting.TargetSchedulable = "quality", &hold, false
	if err := save(context.Background(), store, &ledger); err != nil {
		t.Fatal(err)
	}
	tail := api.accounts[9]
	tail.Status = "error"
	api.accounts[9] = tail

	e.Cycle(context.Background(), now.Add(time.Minute))
	ledger, status := ledgerOf(t, store), e.Snapshot()
	if ledger.FallbackTriggered || !api.accounts[7].Schedulable || api.accounts[8].Schedulable ||
		ledger.Records["7"].RecoveryStartedAt == nil || status.FallbackTriggered {
		t.Fatalf("terminal failure started a second recovery or displaced the active trial: status=%+v ledger=%+v accounts=%+v",
			status, ledger, api.accounts)
	}
}

func TestEarlierGroupAvailabilityAndPerGroupCacheRule(t *testing.T) {
	excellent := account(7, "openai", "apikey", 3, true)
	good := account(8, "openai", "apikey", 4, true)
	e, c, api, _, now := setup(t, excellent, good)
	c.AccountSettings[0].Group = "excellent"
	c.AccountSettings[1].Group = "good"
	rule := c.GroupRules["excellent"]
	rule.MaxRateMultiplier = 0
	rule.MinCacheRatio = .8
	rule.MinRequests = 20
	rule.TriggerSamples = 2
	rule.CooldownSeconds = 30
	c.GroupRules["excellent"] = rule
	goodRule := c.GroupRules["good"]
	goodRule.MaxRateMultiplier = 0
	goodRule.MinCacheRatio = 0
	c.GroupRules["good"] = goodRule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	at := now.Add(180 * time.Second)
	qualityCycle(e, api, at, 7, qualityWindow(at, 100, "model-a", "sync", .75))
	if !api.accounts[7].Schedulable || api.accounts[8].Schedulable {
		t.Fatal("one unconfirmed quality window switched the group")
	}
	at = now.Add(360 * time.Second)
	qualityCycle(e, api, at, 7, qualityWindow(at, 120, "model-a", "sync", .75))
	if api.accounts[7].Schedulable || !api.accounts[8].Schedulable {
		t.Fatalf("fallback did not activate the next healthy group: excellent=%+v good=%+v", api.accounts[7], api.accounts[8])
	}
	if !strings.Contains(snapshotAccount(t, e, 7).Reason, "缓存命中比例") {
		t.Fatalf("group rule did not trigger: %+v", snapshotAccount(t, e, 7))
	}
}

func TestAverageTTFTThresholdPausesOnlyAfterTwentyValidSamples(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	rule := c.GroupRules["general"]
	rule.MaxRateMultiplier = 0
	rule.MaxAverageFirstTokenMs = 1000
	rule.MinCacheRatio = 0
	rule.TriggerSamples = 2
	rule.CooldownSeconds = 30
	c.GroupRules["general"] = rule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	at := now.Add(180 * time.Second)
	b := qualityWindow(at, 100, "model-a", "stream", .9)
	b.FirstTokenSamples, b.AverageFirstTokenMs = 19, nil
	qualityCycle(e, api, at, 7, b)
	if !api.accounts[7].Schedulable {
		t.Fatal("19 TTFT samples triggered a pause")
	}
	at = now.Add(360 * time.Second)
	b = qualityWindow(at, 120, "model-a", "stream", .9)
	b.FirstTokenSamples, b.AverageFirstTokenMs = 20, ptr(1500)
	qualityCycle(e, api, at, 7, b)
	if !api.accounts[7].Schedulable {
		t.Fatal("incomplete TTFT sample counted toward confirmation")
	}
	at = now.Add(540 * time.Second)
	b = qualityWindow(at, 140, "model-a", "stream", .9)
	b.FirstTokenSamples, b.AverageFirstTokenMs = 20, ptr(1500)
	qualityCycle(e, api, at, 7, b)
	if api.accounts[7].Schedulable {
		t.Fatal("two complete slow TTFT windows did not pause the account")
	}
}

func TestPerPlatformPrioritiesStayIndependentAndTemporaryIsFixed(t *testing.T) {
	if config.DefaultGroupPriorities()["temporary"] != 99 {
		t.Fatal("temporary default priority must be 99")
	}
	c := config.Defaults()
	c.PlatformGroupPriorities = map[string]map[string]int{"openai": {"good": 7}, "anthropic": {"good": 70}}
	decoded, err := config.Decode(c.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GroupPriority("openai", "good") != 7 || decoded.GroupPriority("anthropic", "good") != 70 || decoded.GroupPriority("anthropic", "temporary") != 99 {
		t.Fatalf("platform priority leakage: %+v", decoded.PlatformGroupPriorities)
	}
}

func TestManualPriorityChangeIsNotOverwritten(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	e.Cycle(context.Background(), now)
	a := api.accounts[7]
	a.Priority = 123
	api.accounts[7] = a
	e.Cycle(context.Background(), now.Add(time.Minute))
	if api.accounts[7].Priority != 123 || snapshotAccount(t, e, 7).State != "manual_change" || ledgerOf(t, store).Records["7"].Phase != "manual_change" {
		t.Fatal("manual edit was overwritten")
	}
	reads, writes := api.usageReads, api.writes
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	if api.usageReads != reads || api.writes != writes || snapshotAccount(t, e, 7).State != "observing" || ledgerOf(t, store).Records["7"].Phase != "manual_change" {
		t.Fatal("stopping must clear runtime display while preserving manual settings and protection")
	}
	c.AutomationEnabled = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(3*time.Minute))
	if api.accounts[7].Priority != 8 || snapshotAccount(t, e, 7).State == "manual_change" || ledgerOf(t, store).Records["7"].OriginalPriority != 123 {
		t.Fatal("an explicit new session did not reset the manual state using current host settings as its baseline")
	}
}

func TestTemporaryManualOpeningRemainsManualUntilExplicitNewSession(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 4, true))
	c.AccountSettings[0].Group = "temporary"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.accounts[7].Schedulable || api.accounts[7].Priority != 99 {
		t.Fatal("temporary account was not closed directly")
	}
	a := api.accounts[7]
	a.Schedulable = true
	api.accounts[7] = a
	writes := api.writes
	for i := 1; i <= 2; i++ {
		e.Cycle(context.Background(), now.Add(time.Duration(i)*time.Minute))
		row := snapshotAccount(t, e, 7)
		if row.State != "manual_change" || !row.Schedulable || !strings.Contains(row.Reason, "人工开启") || api.writes != writes {
			t.Fatalf("manual opening was overwritten or hidden: %+v", row)
		}
	}
	// A process restart is not the user's explicit off/on reset.
	restarted := New("test", "node-a")
	restarted.clock = e.clock
	restarted.newAPI = e.newAPI
	restarted.SetStore(store)
	if err := restarted.Apply(c); err != nil {
		t.Fatal(err)
	}
	restarted.Cycle(context.Background(), now.Add(3*time.Minute))
	if row := snapshotAccount(t, restarted, 7); row.State != "manual_change" || !row.Schedulable || api.writes != writes {
		t.Fatalf("process restart took over a manual account: %+v", row)
	}
	c.AutomationEnabled = false
	if err := restarted.Apply(c); err != nil {
		t.Fatal(err)
	}
	restarted.Cycle(context.Background(), now.Add(4*time.Minute))
	if !api.accounts[7].Schedulable || api.writes != writes {
		t.Fatal("stop overwrote the manual scheduling switch")
	}
	c.AutomationEnabled = true
	if err := restarted.Apply(c); err != nil {
		t.Fatal(err)
	}
	restarted.Cycle(context.Background(), now.Add(5*time.Minute))
	if row := snapshotAccount(t, restarted, 7); row.State != "temporary" || row.Schedulable {
		t.Fatalf("explicit new session retained the old manual state: %+v", row)
	}
}

func TestNewSessionClearsHoldWithoutDiscardingUnrestoredOwnership(t *testing.T) {
	a := account(7, "openai", "apikey", 4, true)
	a.RateMultiplier = ptr(.1)
	e, c, api, store, now := setup(t, a)
	configureQuality(&c, "general", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, seconds := range []int{180, 360} {
		at := now.Add(time.Duration(seconds) * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+i*20, "model-a", "sync", .75))
	}
	if ledgerOf(t, store).Records["7"].HoldUntil == nil {
		t.Fatal("test did not create an exception hold")
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	// Re-enable before the asynchronous stop cycle restored the owned values.
	c.AutomationEnabled = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if record := ledgerOf(t, store).Records["7"]; record.HoldUntil != nil || record.Reason != "" || record.OriginalPriority != 4 || !record.OriginalSchedulable {
		t.Fatalf("reset lost ownership or retained a hold: %+v", record)
	}
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), now.Add(7*time.Minute))
	if row := snapshotAccount(t, e, 7); row.State != "sampling" || !row.Schedulable || row.Requests != 0 {
		t.Fatalf("fresh session did not resume sampling: %+v", row)
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(8*time.Minute))
	if api.accounts[7].Priority != 4 || !api.accounts[7].Schedulable {
		t.Fatal("reset discarded the original recoverable settings")
	}
}

func TestManualPollQueuesOneCycleWithoutResettingSession(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 4, true))
	e.Cycle(context.Background(), now)
	for len(e.wake) > 0 {
		<-e.wake
	}
	reads, session := api.usageReads, ledgerOf(t, store).SamplingSince
	for i := 0; i < 2; i++ {
		if err := e.RequestPoll(); err != nil {
			t.Fatal(err)
		}
	}
	if !e.Snapshot().Polling || e.Snapshot().NextPoll != nil || len(e.wake) != 1 || api.usageReads != reads {
		t.Fatal("manual request ran synchronously or queued duplicate cycles")
	}
	<-e.wake
	finishedAt := now.Add(2 * time.Minute)
	e.clock = func() time.Time { return finishedAt }
	e.Cycle(context.Background(), now.Add(time.Minute))
	state := e.Snapshot()
	if state.Polling || state.NextPoll == nil || !state.NextPoll.Equal(finishedAt.Add(time.Duration(c.PollSeconds)*time.Second)) || api.usageReads != reads+1 || !ledgerOf(t, store).SamplingSince.Equal(session) {
		t.Fatal("manual request failed to collect, reset its next countdown or retained the wrong sampling session")
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	if err := e.RequestPoll(); err == nil {
		t.Fatal("manual sampling was allowed while disabled")
	}
	c.AutomationEnabled, c.ExecutorHostname = true, "other-node"
	// Use a fresh engine: the existing engine intentionally protects owned records.
	standby := New("test", "node-a")
	if err := standby.Apply(c); err != nil {
		t.Fatal(err)
	}
	if err := standby.RequestPoll(); err == nil {
		t.Fatal("standby node accepted a manual collection request")
	}
}

func TestExclusiveExitUsesIndependentCacheAndTTFTThresholds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ratio       float64
		requests    int
		complete    bool
		ttft        float64
		ttftSamples int
		exits       bool
	}{
		{"cache_below", .75, 20, true, 500, 20, true},
		{"ttft_above", .9, 20, true, 1500, 20, true},
		{"ttft_independent_of_cache", .9, 0, false, 1500, 20, true},
		{"cache_boundary", .8, 20, true, 1000, 20, false},
		{"cache_sample_shortage", .75, 19, true, 500, 20, false},
		{"cache_incomplete", .75, 20, false, 500, 20, false},
		{"ttft_sample_shortage", .9, 20, true, 1500, 19, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := account(7, "openai", "apikey", 4, true), account(8, "openai", "apikey", 5, true)
			a.RateMultiplier, b.RateMultiplier = ptr(.01), ptr(.1)
			e, c, api, store, now := setup(t, a, b)
			for i := range c.AccountSettings {
				c.AccountSettings[i].Group = "excellent"
			}
			rule := c.GroupRules["excellent"]
			rule.MinCacheRatio, rule.MaxAverageFirstTokenMs = .5, 2000
			rule.ExclusiveMinCacheRatio, rule.ExclusiveMaxAverageFirstTokenMs = .8, 1000
			c.GroupRules["excellent"] = rule
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			healthy := adminapi.Window{Complete: true, Requests: 20, CacheRatio: ptr(.9),
				QualityCurrent: &adminapi.QualityWindow{WindowKnown: true}}
			withTTFT := func(w adminapi.Window, at time.Time, ms float64, samples int) adminapi.Window {
				b := qualityWindow(at, 1, "m", "stream", .9)
				b.AverageFirstTokenMs, b.FirstTokenSamples = ptr(ms), samples
				w.QualityWindows = []adminapi.QualityWindow{b}
				return w
			}
			api.usage[7], api.usage[8] = withTTFT(healthy, now, 500, 20), healthy
			e.Cycle(context.Background(), now)
			if !api.accounts[7].Schedulable || api.accounts[8].Schedulable {
				t.Fatal("test did not start with a single exclusive account")
			}
			w := healthy
			w.Requests, w.Complete = tc.requests, tc.complete
			w.CacheRatio = ptr(tc.ratio)
			api.usage[7] = withTTFT(w, now.Add(time.Minute), tc.ttft, tc.ttftSamples)
			e.Cycle(context.Background(), now.Add(time.Minute))
			if !api.accounts[7].Schedulable || api.accounts[8].Schedulable != tc.exits || ledgerOf(t, store).Records["7"].HoldUntil != nil {
				t.Fatalf("exclusive exit paused an otherwise eligible account or failed to reopen the group: first=%+v second=%+v", api.accounts[7], api.accounts[8])
			}
			if tc.exits && !strings.Contains(snapshotAccount(t, e, 7).Reason, "退出独占") {
				t.Fatal("exclusive exit reason was not exposed")
			}
			api.usage[7] = withTTFT(healthy, now.Add(2*time.Minute), 500, 20)
			e.Cycle(context.Background(), now.Add(2*time.Minute))
			if api.accounts[8].Schedulable {
				t.Fatal("recovered exclusive metrics did not restore exclusivity")
			}
		})
	}
}

func TestServingManualAccountStillBlocksLaterGroup(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true), account(8, "openai", "apikey", 5, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	a := api.accounts[7]
	a.Priority = 123
	api.accounts[7] = a
	e.Cycle(context.Background(), now.Add(time.Minute))
	if api.accounts[7].Priority != 123 || !api.accounts[7].Schedulable || api.accounts[8].Schedulable {
		t.Fatal("manual priority change bypassed fixed group order")
	}
}

func TestStorageFailurePreventsScheduleWrites(t *testing.T) {
	e, _, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	store.fail = true
	e.Cycle(context.Background(), now)
	if api.writes != 0 {
		t.Fatalf("wrote without durable ownership: %d", api.writes)
	}
}

func TestMultiplierUpperLimitPausesAtConfiguredThreshold(t *testing.T) {
	rule := config.Defaults().Rule
	bad, known, _, _ := evaluate(rule, AccountStatus{EffectiveRate: ptr(.21)})
	if !bad || !known {
		t.Fatalf("multiplier above bounded limit was accepted: %t %t", bad, known)
	}
	if bad, known, _, _ := evaluate(rule, AccountStatus{EffectiveRate: ptr(.2)}); bad {
		t.Fatalf("multiplier at limit was rejected: %t %t", bad, known)
	}
}
func ptr(v float64) *float64 { return &v }

func TestFixedGroupOrderIgnoresPriorityNumbersAndIsIndependentPerPlatform(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true), account(8, "openai", "apikey", 5, true), account(9, "anthropic", "apikey", 5, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group, c.AccountSettings[2].Group = "excellent", "good", "reserve"
	c.PlatformGroupPriorities = map[string]map[string]int{"openai": {"excellent": 80, "good": 1}, "anthropic": {"reserve": 40}}
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if !api.accounts[7].Schedulable || api.accounts[8].Schedulable || !api.accounts[9].Schedulable || api.accounts[7].Priority != 80 || api.accounts[8].Priority != 1 || api.accounts[9].Priority != 40 {
		t.Fatalf("fixed group order or platform isolation failed: %+v", api.accounts)
	}
}

func TestTemporaryMoveAndRemovalRestoreOwnedSettings(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 4, true))
	c.AccountSettings[0].Group = "temporary"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	c.AccountSettings[0].Group = "general"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if a := api.accounts[7]; a.Priority != 8 || !a.Schedulable {
		t.Fatalf("moving out of temporary failed: %+v", a)
	}
	c.AccountIDs, c.AccountSettings = nil, nil
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	if a := api.accounts[7]; a.Priority != 4 || !a.Schedulable || len(ledgerOf(t, store).Records) != 0 {
		t.Fatalf("removal did not restore original settings: %+v", a)
	}
}

func TestCooldownRetriesWithoutImmediatelyReusingTriggeringSamples(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	configureQuality(&c, "general", .9, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, seconds := range []int{180, 360} {
		at := now.Add(time.Duration(seconds) * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+i*20, "m", "sync", .2))
	}
	record := ledgerOf(t, store).Records["7"]
	if api.accounts[7].Schedulable || record.HoldUntil == nil {
		t.Fatal("cache exception did not pause")
	}
	trialAt := *record.HoldUntil
	e.Cycle(context.Background(), trialAt)
	if state := snapshotAccount(t, e, 7); !api.accounts[7].Schedulable || state.State != "recovering" || state.Requests != 0 {
		t.Fatalf("cooldown did not reopen account in a fresh sampling window: %+v", state)
	}
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), trialAt.Add(time.Second))
	if !api.lastUsageSince.Equal(trialAt) {
		t.Fatal("recovery did not start a new usage sampling window")
	}
}

func TestUnconfirmedFallbackOpenKeepsCurrentGroupUntilRestartReconciles(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true), account(8, "openai", "apikey", 5, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	a := api.accounts[7]
	until := now.Add(time.Hour)
	a.RateLimitResetAt = &until
	api.accounts[7], api.ignoreWrite = a, true
	e.Cycle(context.Background(), now.Add(time.Minute))
	if !api.accounts[7].Schedulable || api.accounts[8].Schedulable ||
		ledgerOf(t, store).Records["8"].PendingField != "schedulable" {
		t.Fatal("unconfirmed fallback open did not preserve the serving account")
	}
	api.ignoreWrite = false
	restarted := New("test", "node-a")
	restarted.clock = func() time.Time { return now.Add(2 * time.Minute) }
	restarted.newAPI = func(config.Config) API { return api }
	restarted.SetStore(store)
	if err := restarted.Apply(c); err != nil {
		t.Fatal(err)
	}
	restarted.Cycle(context.Background(), now.Add(2*time.Minute))
	if api.accounts[7].Schedulable || !api.accounts[8].Schedulable {
		t.Fatal("restart failed to reconcile pending closure and fail over")
	}
}

func TestDelayedOpenRemainsOwnedUntilStopCanCloseIt(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 8, false))
	api.ignoreWrite = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.accounts[7].Schedulable || ledgerOf(t, store).Records["7"].PendingSchedulable != true {
		t.Fatal("test did not establish an unconfirmed host open")
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if ledgerOf(t, store).Records["7"] == nil {
		t.Fatal("stop discarded ownership while an open write was unresolved")
	}
	late := api.accounts[7]
	late.Schedulable = true
	api.accounts[7], api.ignoreWrite = late, false
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	if api.accounts[7].Schedulable || ledgerOf(t, store).Records["7"] != nil {
		t.Fatalf("late open was not closed during restoration: host=%+v ledger=%+v", api.accounts[7], ledgerOf(t, store))
	}
}

func TestFallbackIntentStorageFailureDoesNotConsumeOneShot(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 6, true),
		account(8, "openai", "apikey", 9, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "general"
	configureQuality(&c, "excellent", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, seconds := range []int{180, 360} {
		at := now.Add(time.Duration(seconds) * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+i*20, "m", "sync", .2))
	}
	if !api.accounts[8].Schedulable {
		t.Fatal("terminal general account did not become active")
	}
	tail := api.accounts[8]
	tail.Status = "error"
	api.accounts[8] = tail
	writes := api.writes
	intentStore := &failFallbackIntentStore{memoryStore: store}
	e.SetStore(intentStore)
	e.Cycle(context.Background(), now.Add(7*time.Minute))
	ledger := ledgerOf(t, store)
	if !intentStore.failed || ledger.FallbackTriggered || ledger.Records["7"].FallbackPending ||
		api.accounts[7].Schedulable != false || api.writes != writes {
		t.Fatalf("failed fallback intent was consumed or sent to host: ledger=%+v host=%+v writes=%d/%d",
			ledger, api.accounts[7], api.writes, writes)
	}
	e.Cycle(context.Background(), now.Add(8*time.Minute))
	ledger = ledgerOf(t, store)
	if !ledger.FallbackTriggered || !api.accounts[7].Schedulable {
		t.Fatalf("fallback did not retry after storage recovered: ledger=%+v host=%+v", ledger, api.accounts[7])
	}
}

func TestMissingUpstreamDoesNotUseImplicitAccountMultiplier(t *testing.T) {
	e, c, _, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	c.AccountSettings[0].RateMode, c.AccountSettings[0].FixedRate = "upstream", nil
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	state := snapshotAccount(t, e, 7)
	if state.EffectiveRate == nil || *state.EffectiveRate != 1 || state.RateMode != "default" {
		t.Fatalf("missing upstream must use default 1 rather than account metadata: %+v", state)
	}
}

func TestEnabledUsesDefaultRateAndSyncsPriorityChanges(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	c.UpstreamRatesEnabled = true
	c.AccountSettings[0].RateMode, c.AccountSettings[0].FixedRate = "upstream", nil
	c.PlatformGroupPriorities = map[string]map[string]int{"openai": {"general": 37}}
	rule := c.GroupRules["general"]
	rule.MinCacheRatio, rule.MaxRateMultiplier = 0, 0
	c.GroupRules["general"] = rule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if a := api.accounts[7]; a.Priority != 37 || !a.Schedulable {
		t.Fatalf("account did not receive group priority or default rate scheduling: %+v", a)
	}
	if state := snapshotAccount(t, e, 7); state.Priority != 37 || state.RateMode != "default" || state.EffectiveRate == nil || *state.EffectiveRate != 1 {
		t.Fatalf("snapshot lost synced priority: %+v", state)
	}

	c.PlatformGroupPriorities["openai"]["general"] = 41
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if a := api.accounts[7]; a.Priority != 41 || !a.Schedulable {
		t.Fatalf("updated group priority was not synchronized: %+v", a)
	}
}

func TestUserScheduleSwitchWritesOnceAndRemainsUserOwned(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	falseValue := false
	c.AccountSettings[0].Group = "general"
	c.AccountSettings[0].DesiredSchedulable = &falseValue
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.accounts[7].Schedulable || api.accounts[7].Priority != 8 {
		t.Fatalf("automatic management did not apply preconfigured group stop: %+v", api.accounts[7])
	}
	if state := snapshotAccount(t, e, 7); state.State != "manually_disabled" || state.Schedulable {
		t.Fatalf("group-level stop did not remove account from callable pool: %+v", state)
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	if !api.accounts[7].Schedulable || api.accounts[7].Priority != 5 {
		t.Fatalf("disabling automatic management did not restore original state: %+v", api.accounts[7])
	}

	trueValue := true
	c.AccountSettings[0].DesiredSchedulable = &trueValue
	c.AutomationEnabled = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	if state := snapshotAccount(t, e, 7); state.State == "manually_disabled" {
		t.Fatalf("group-level resume did not return account to group scheduler: %+v", state)
	}
}

func TestDefaultRateRuleClosesItsAccountAndKeepsOtherGroupAvailable(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true), account(8, "openai", "apikey", 5, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	c.AccountSettings[0].RateMode, c.AccountSettings[0].FixedRate = "upstream", nil
	c.AccountSettings[1].RateMode, c.AccountSettings[1].FixedRate = "fixed", ptr(.1)
	rule := c.GroupRules["excellent"]
	rule.MinCacheRatio, rule.MaxRateMultiplier, rule.TriggerSamples = 0, .2, 1
	c.GroupRules["excellent"] = rule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.accounts[7].Schedulable || !api.accounts[8].Schedulable {
		t.Fatalf("default rate rule failed to close the first group and open the next: excellent=%+v good=%+v", api.accounts[7], api.accounts[8])
	}
	if state := snapshotAccount(t, e, 7); state.State != "policy_wait" || state.EffectiveRate == nil ||
		*state.EffectiveRate != 1 || state.RateMode != "default" || state.FailureCount != 0 ||
		state.DegradationLevel != 0 || state.NextRetryAt != nil {
		t.Fatalf("default fallback did not follow the group exception rule: %+v", state)
	}
}

func TestDefaultUpstreamSnapshotAndFallbackKeepFixedAccountsIndependent(t *testing.T) {
	for _, name := range []string{"valid", "missing", "expired", "unavailable"} {
		t.Run(name, func(t *testing.T) {
			e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true), account(8, "openai", "apikey", 5, true))
			c.AccountSettings[0].RateMode, c.AccountSettings[0].FixedRate = "upstream", ptr(9)
			c.AccountSettings[1].FixedRate = ptr(.07)
			fresh, received := now.Add(time.Hour), now
			snapshot := &adminapi.RateSnapshot{Status: "ok", FreshUntil: &fresh, ReceivedAt: &received,
				Data: map[string]any{"billing_scope": "token", "resolved_rate_multiplier": .039, "peak_rate_enabled": false}}
			api.rates = map[int64]*adminapi.RateSnapshot{7: snapshot, 8: snapshot}
			expected, mode := 1.0, "default"
			switch name {
			case "valid":
				expected, mode = .039, "upstream"
			case "missing":
				api.rates = nil
			case "expired":
				fresh = now.Add(-time.Second)
			case "unavailable":
				api.rates, api.rateError = nil, errors.New("snapshot unavailable")
			}
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			e.Cycle(context.Background(), now)
			upstream, fixed := snapshotAccount(t, e, 7), snapshotAccount(t, e, 8)
			if api.rateReads != 1 || upstream.RateMode != mode || upstream.EffectiveRate == nil || *upstream.EffectiveRate != expected ||
				fixed.RateMode != "fixed" || fixed.EffectiveRate == nil || *fixed.EffectiveRate != .07 {
				t.Fatalf("rate choice changed without an account setting change: upstream=%+v fixed=%+v", upstream, fixed)
			}
		})
	}
}

func TestEnabledUpstreamFallbackDisplaysDefaultAndAllowsRuleEvaluation(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	c.UpstreamRatesEnabled = true
	c.AccountSettings[0].RateMode, c.AccountSettings[0].FixedRate = "upstream", nil
	rule := c.GroupRules["general"]
	rule.MinCacheRatio, rule.MaxAverageFirstTokenMs, rule.MaxRateMultiplier = 0, 0, 0
	c.GroupRules["general"] = rule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	state := snapshotAccount(t, e, 7)
	if !api.accounts[7].Schedulable || state.State == "needs_rate" || state.RateMode != "default" ||
		state.EffectiveRate == nil || *state.EffectiveRate != 1 {
		t.Fatalf("default fallback must remain schedulable until a rule triggers: %+v", state)
	}
}
