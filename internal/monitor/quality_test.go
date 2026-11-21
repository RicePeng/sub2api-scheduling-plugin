package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
)

func qualityWindow(at time.Time, oldestID int, model, requestType string, ratio float64) adminapi.QualityWindow {
	key := at.UTC().Format(time.RFC3339Nano) + ":"
	return adminapi.QualityWindow{
		Requests: config.QualityWindowRequests, WindowKnown: true,
		CacheRatio: &ratio,
		LastSample: key + strconv.Itoa(oldestID+config.QualityWindowRequests-1), FirstSample: key + strconv.Itoa(oldestID),
		Models: []adminapi.QualityModelUsage{{Model: model, RequestType: requestType, Requests: config.QualityWindowRequests, CacheRatio: &ratio}},
	}
}

func qualityCycle(e *Engine, api *fakeAPI, at time.Time, id int64, windows ...adminapi.QualityWindow) {
	api.usage[id] = adminapi.Window{QualityCurrent: &adminapi.QualityWindow{WindowKnown: true}, QualityWindows: windows}
	e.Cycle(context.Background(), at)
}

func configureQuality(c *config.Config, group string, minRatio, maxTTFT float64) {
	rule := c.GroupRules[group]
	rule.MinCacheRatio, rule.MaxAverageFirstTokenMs, rule.MaxRateMultiplier = minRatio, maxTTFT, 0
	c.GroupRules[group] = rule
}

func TestQualityHistoryEvaluatesTTFTPerWindow(t *testing.T) {
	rule := config.Defaults().GroupRules["general"]
	rule.MinCacheRatio, rule.MaxAverageFirstTokenMs, rule.FirstTokenPeriods = .8, 1000, 5
	ttft := 100.0
	window := qualityWindow(time.Date(2026, 11, 18, 12, 1, 0, 0, time.UTC), 1, "m", "stream", .9)
	window.AverageFirstTokenMs, window.FirstTokenSamples = &ttft, config.QualityWindowRequests
	store := &memoryStore{}
	item := &managedAccount{
		row:            AccountStatus{ID: 7},
		history:        &accountQualityHistory{AdminURL: "admin", Executor: "node", SamplingSince: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC), Scope: accountQualityScope},
		qualityWindows: []adminapi.QualityWindow{window},
	}
	writeQualityHistory(context.Background(), store, item, rule, windowTime(window))
	if item.history == nil || len(item.history.History) != 1 || item.history.History[0].Result != "healthy" {
		t.Fatalf("single history window was incorrectly gated by the five-period TTFT setting: %+v", item.history)
	}
}

func TestPolicyComparisonMigratesMissingTTFTPeriodsButDetectsChanges(t *testing.T) {
	current := config.Defaults().GroupRules["general"]
	legacy := current
	legacy.FirstTokenPeriods = 0
	if policyDiffersForHealth(legacy, current) {
		t.Fatal("missing legacy TTFT periods were treated as an edited quality policy")
	}
	previous := current
	current.FirstTokenPeriods--
	if !policyDiffersForHealth(previous, current) {
		t.Fatal("an explicit TTFT period change was ignored")
	}
}

func windowTime(window adminapi.QualityWindow) time.Time {
	at, _, _ := adminapi.ParseSampleKey(window.LastSample)
	return at
}

func restartMonitor(t *testing.T, e *Engine, c config.Config, store *memoryStore, at time.Time) *Engine {
	t.Helper()
	restarted := New("test", "node-a")
	restarted.clock = func() time.Time { return at }
	restarted.newAPI = e.newAPI
	restarted.SetStore(store)
	if err := restarted.Apply(c); err != nil {
		t.Fatal(err)
	}
	return restarted
}

func TestQualityWindowsPreserveWarmupAndAccountWideConfirmation(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	configureQuality(&c, "general", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	at := func(seconds int) time.Time { return now.Add(time.Duration(seconds) * time.Second) }
	bad := func(seconds, id int, model, typ string) adminapi.QualityWindow {
		return qualityWindow(at(seconds), id, model, typ, .2)
	}
	warmup := bad(179, 200, "a", "sync")
	store.historyWriteError = true
	qualityCycle(e, api, at(179), 7, warmup)
	row := snapshotAccount(t, e, 7)
	if row.QualitySampling == nil || len(row.QualitySampling.History) != 0 ||
		row.QualitySampling.BadWindows != 0 {
		t.Fatalf("startup grace counted progress or published unsaved history: %+v", row)
	}
	store.historyWriteError = false
	e = restartMonitor(t, e, c, store, at(180))
	qualityCycle(e, api, at(180), 7, warmup)
	if s := snapshotAccount(t, e, 7).QualitySampling; s == nil || len(s.History) != 1 || s.BadWindows != 0 {
		t.Fatalf("history retry after restart counted warmup traffic as a fault: %+v", s)
	}
	api.usage[7] = adminapi.Window{QualityCurrent: &adminapi.QualityWindow{Requests: 19, WindowKnown: true}}
	e.Cycle(context.Background(), at(180))
	if s := snapshotAccount(t, e, 7).QualitySampling; s == nil || s.Requests != 19 || s.BadWindows != 0 {
		t.Fatalf("19 calls contributed to confirmation instead of trailing: %+v", s)
	}
	qualityCycle(e, api, at(180), 7, bad(180, 400, "a", "sync"))
	qualityCycle(e, api, at(359), 7, bad(180, 414, "b", "stream"))
	if s := snapshotAccount(t, e, 7).QualitySampling; s.BadWindows != 1 || len(s.History) != 2 {
		t.Fatalf("overlapping timestamps/IDs counted in another model: %+v", s)
	}
	qualityCycle(e, api, at(359), 7, bad(359, 500, "b", "stream"))
	if s := snapshotAccount(t, e, 7).QualitySampling; s.BadWindows != 2 || !api.accounts[7].Schedulable {
		t.Fatalf("account bad streak did not combine both models, or took action too early: %+v", s)
	}
	qualityCycle(e, api, at(360), 7)
	if api.accounts[7].Schedulable {
		t.Fatal("a successful known scan after the dwell did not confirm the pending fault")
	}
	record := ledgerOf(t, store).Records["7"]
	if api.accounts[7].Schedulable || record.FailureKind != "quality" || record.FailureCount != 1 ||
		record.HoldUntil == nil || !record.HoldUntil.Equal(at(660)) {
		t.Fatalf("confirmed account windows did not pause at the default cooldown: %+v", record)
	}
}

func TestQualityHistoryIsPersistedForTemporaryAccounts(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	c.AccountSettings[0].Group = "temporary"
	configureQuality(&c, "temporary", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	qualityCycle(e, api, now.Add(time.Minute), 7, qualityWindow(now.Add(time.Minute), 100, "m", "sync", .9))
	row := snapshotAccount(t, e, 7)
	if row.QualitySampling == nil || len(row.QualitySampling.History) != 1 {
		t.Fatalf("temporary account completed window was not published: %+v", row.QualitySampling)
	}
	if raw, err := store.Get(context.Background(), historyKey(7)); err != nil || len(raw) == 0 {
		t.Fatalf("temporary account completed window was not persisted: err=%v raw=%q", err, raw)
	}
}

func TestRetryBackoffDoublesAndCapsAtOneDay(t *testing.T) {
	if got := retryBackoff(300, 1); got != 300*time.Second {
		t.Fatalf("initial cooldown=%s, want 5m", got)
	}
	if got := retryBackoff(300, 2); got != 600*time.Second {
		t.Fatalf("second cooldown=%s, want 10m", got)
	}
	if got := retryBackoff(300, 20); got != 24*time.Hour {
		t.Fatalf("maximum cooldown=%s, want 24h", got)
	}
}

func TestCooldownBackoffAndRecoveryTrialPreserveOriginalGroup(t *testing.T) {
	excellent, good := account(7, "openai", "apikey", 3, true), account(8, "openai", "apikey", 4, true)
	e, c, api, store, now := setup(t, excellent, good)
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	configureQuality(&c, "excellent", .8, 0)
	configureQuality(&c, "good", 0, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, offset := range []time.Duration{180, 360} {
		at := now.Add(offset * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+20*i, "m", "stream", .2))
	}
	record := ledgerOf(t, store).Records["7"]
	if api.accounts[7].Schedulable || !api.accounts[8].Schedulable || record.FailureCount != 1 ||
		record.DegradationLevel != 1 || record.HoldUntil == nil || !record.HoldUntil.Equal(now.Add(660*time.Second)) {
		t.Fatalf("first confirmed fault did not fail over with base cooldown: row=%+v record=%+v", api.accounts, record)
	}
	// A second confirmed failure advances the degradation level and doubles cooldown.
	failureAt := *record.HoldUntil
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), failureAt)
	for i, offset := range []time.Duration{180, 360} {
		at := failureAt.Add(offset * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 200+20*i, "m", "stream", .2))
	}
	record = ledgerOf(t, store).Records["7"]
	if record.FailureCount != 2 || record.DegradationLevel != 2 || record.HoldUntil == nil ||
		!record.HoldUntil.Equal(failureAt.Add(960*time.Second)) {
		t.Fatalf("repeated failure did not double the cooldown: %+v", record)
	}

	// The fallback remains open while the recovered account trials at priority 6.
	trialAt := *record.HoldUntil
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), trialAt)
	if !api.accounts[7].Schedulable || !api.accounts[8].Schedulable ||
		snapshotAccount(t, e, 7).Priority != 6 || snapshotAccount(t, e, 7).State != "recovering" {
		t.Fatalf("recovery candidate changed group or closed fallback: first=%+v second=%+v", api.accounts[7], api.accounts[8])
	}
	for i, offset := range []time.Duration{100, 200} {
		at := trialAt.Add(offset * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 300+20*i, "other-model", "stream", .95))
		if !api.accounts[8].Schedulable {
			t.Fatal("healthy account windows closed fallback before the recovery dwell time")
		}
	}
	qualityCycle(e, api, trialAt.Add(300*time.Second), 7)
	if !api.accounts[7].Schedulable || api.accounts[8].Schedulable ||
		snapshotAccount(t, e, 7).Priority != 6 {
		t.Fatalf("healthy candidate did not reclaim original group after fresh windows: first=%+v second=%+v", api.accounts[7], api.accounts[8])
	}
}

func TestNoTrafficAtRecoveryDeadlineDefersWithoutNewFailure(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	configureQuality(&c, "general", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, offset := range []time.Duration{180, 360} {
		at := now.Add(offset * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+20*i, "m", "sync", .2))
	}
	record := ledgerOf(t, store).Records["7"]
	trialAt := *record.HoldUntil
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), trialAt)
	deadline := ledgerOf(t, store).Records["7"].RecoveryDeadline
	if deadline == nil {
		t.Fatal("recovery deadline was not persisted")
	}
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), *deadline)
	record = ledgerOf(t, store).Records["7"]
	if record.FailureCount != 1 || record.DegradationLevel != 1 || record.RecoveryFailures != 0 ||
		record.HoldUntil == nil || !record.HoldUntil.Equal(deadline.Add(300*time.Second)) {
		t.Fatalf("no traffic was treated as a new fault or did not defer retry: %+v", record)
	}
}

func TestRestartPreservesQualityProgressAndCooldown(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	configureQuality(&c, "general", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	at := now.Add(180 * time.Second)
	qualityCycle(e, api, at, 7, qualityWindow(at, 100, "m", "sync", .2))
	restarted := restartMonitor(t, e, c, store, now.Add(200*time.Second))
	at = now.Add(360 * time.Second)
	qualityCycle(restarted, api, at, 7, qualityWindow(at, 120, "m", "sync", .2))
	record := ledgerOf(t, store).Records["7"]
	if record.FailureCount != 1 || record.DegradationLevel != 1 || record.HoldUntil == nil ||
		!record.HoldUntil.Equal(now.Add(660*time.Second)) {
		t.Fatalf("restart discarded prior account progress: %+v", record)
	}
	restarted = restartMonitor(t, restarted, c, store, now.Add(400*time.Second))
	api.usage[7] = adminapi.Window{}
	restarted.Cycle(context.Background(), now.Add(400*time.Second))
	record = ledgerOf(t, store).Records["7"]
	if record.FailureCount != 1 || record.DegradationLevel != 1 || api.accounts[7].Schedulable ||
		record.HoldUntil == nil || !record.HoldUntil.Equal(now.Add(660*time.Second)) {
		t.Fatalf("restart bypassed persisted cooldown: %+v", record)
	}
}

func TestAccountScopeUpgradeKeepsRecoveryButNotOldProgress(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 5, true))
	configureQuality(&c, "general", .8, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i, seconds := range []int{180, 360} {
		at := now.Add(time.Duration(seconds) * time.Second)
		qualityCycle(e, api, at, 7, qualityWindow(at, 100+20*i, "m", "sync", .2))
	}
	hold := ledgerOf(t, store).Records["7"].HoldUntil
	if hold == nil {
		t.Fatal("test setup did not pause account")
	}
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), *hold)
	at := hold.Add(60 * time.Second)
	consumed := at.UTC().Format(time.RFC3339Nano) + ":159"
	ledger := ledgerOf(t, store)
	r := ledger.Records["7"]
	r.QualityScope, r.Policy.MinRequests, r.GoodWindows = "", 20, 1
	r.QualityTracks = map[string]legacyQualityTrack{
		"old-a": {LastSample: at.UTC().Format(time.RFC3339Nano) + ":99"},
		"old-b": {LastSample: consumed},
	}
	if err := save(context.Background(), store, &ledger); err != nil {
		t.Fatal(err)
	}
	restarted := restartMonitor(t, e, c, store, hold.Add(300*time.Second))
	// The synthetic window is made exclusively from already consumed calls.
	overlap := qualityWindow(at, 140, "m", "sync", .95)
	qualityCycle(restarted, api, hold.Add(300*time.Second), 7, overlap)
	r = ledgerOf(t, store).Records["7"]
	if r.QualityScope != accountQualityScope || !r.RecoveryTrial || r.RecoveryStartedAt == nil ||
		!r.RecoveryStartedAt.Equal(*hold) || r.QualityTracks != nil || r.QualityLegacyAnchor != consumed || r.GoodWindows != 0 {
		t.Fatalf("upgrade reused old progress or lost recovery state: %+v", r)
	}
	for i, seconds := range []int{360, 420} {
		at = hold.Add(time.Duration(seconds) * time.Second)
		qualityCycle(restarted, api, at, 7, qualityWindow(at, 160+20*i, "m", "sync", .95))
		if r = ledgerOf(t, store).Records["7"]; r.RecoveryTrial != (i == 0) {
			t.Fatalf("trial recovery did not require two fresh windows after upgrade: %+v", r)
		}
	}
}

func TestLegacyPolicyFieldsDoNotReleaseActiveHoldOnUpgrade(t *testing.T) {
	e, c, api, store, now := setup(t,
		account(7, "openai", "apikey", 3, true), account(8, "openai", "apikey", 4, true))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	host := api.accounts[7]
	host.Schedulable = false
	api.accounts[7] = host
	hold := now.Add(5 * time.Minute)
	policy := json.RawMessage(`{"min_cache_ratio":0.8,"max_rate_multiplier":0.2,"max_average_first_token_ms":10000,
		"exclusive_rate_below":0.05,"exclusive_min_cache_ratio":0.8,"exclusive_max_average_first_token_ms":10000,
		"min_requests":20,"trigger_samples":1,"recovery_samples":1,"cooldown_seconds":300,
		"action":"pause","priority_penalty":10,"rate_source":"account"}`)
	raw, err := json.Marshal(map[string]any{"sampling_since": now, "records": map[string]any{"7": map[string]any{
		"account_id": 7, "platform": "openai", "group": "excellent", "admin_url": c.AdminURL, "executor": c.ExecutorHostname,
		"action": "schedule", "phase": "active", "original_priority": 3, "target_priority": 6,
		"original_schedulable": true, "target_schedulable": false, "original_status": "active",
		"policy": policy, "hold_until": hold, "last_bad_sample": "cache:2026-11-18T12:00:00Z:45",
		"reason": "缓存命中比例异常",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	store.raw = raw
	restarted := restartMonitor(t, e, c, store, now.Add(time.Minute))
	restarted.Cycle(context.Background(), now.Add(time.Minute))
	r, row := ledgerOf(t, store).Records["7"], snapshotAccount(t, restarted, 7)
	if r == nil || r.FailureKind != "quality" || r.HoldUntil == nil || !r.HoldUntil.Equal(hold) ||
		r.QualityScope != accountQualityScope || !r.OriginalSchedulable || row.State != "paused" ||
		api.accounts[7].Schedulable || !api.accounts[8].Schedulable {
		t.Fatalf("0.1.0 upgrade canceled a persisted pause or failed to open its fallback: %+v %+v", r, api.accounts)
	}
}

func TestQualityUpgradePreservesManualProtection(t *testing.T) {
	for _, phase := range []string{"manual_change", "active"} {
		t.Run(phase, func(t *testing.T) {
			e, c, api, store, now := setup(t, account(7, "openai", "apikey", 8, true))
			e.Cycle(context.Background(), now)
			ledger := ledgerOf(t, store)
			r := ledger.Records["7"]
			anchor := now.Format(time.RFC3339Nano) + ":15"
			hold := now.Add(5 * time.Minute)
			r.Phase, r.QualityScope, r.Policy.MinRequests = phase, "account-v1", 15
			r.BadWindows, r.QualityReason, r.QualityEvaluatedAt = 1, "旧质量异常", &now
			r.QualityHistoryRetry = &qualityHistoryRetry{Since: now, Through: anchor}
			r.HoldUntil, r.FailureKind, r.DegradationLevel = &hold, "quality", 2
			if err := save(context.Background(), store, &ledger); err != nil {
				t.Fatal(err)
			}
			a := api.accounts[7]
			a.Priority, a.Schedulable = 17, false
			api.accounts[7] = a
			api.usage[7] = adminapi.Window{QualityCurrent: &adminapi.QualityWindow{WindowKnown: true}}
			at := now.Add(time.Minute)
			restarted := restartMonitor(t, e, c, store, at)
			restarted.Cycle(context.Background(), at)
			row := snapshotAccount(t, restarted, 7)
			r = ledgerOf(t, store).Records["7"]
			if r.Phase != "manual_change" || row.State != "manual_change" || row.Schedulable || row.Priority != 17 ||
				r.HoldUntil == nil || !r.HoldUntil.Equal(hold) || r.FailureKind != "quality" || r.DegradationLevel != 2 ||
				!r.OriginalSchedulable || r.OriginalPriority != 8 {
				t.Fatalf("upgrade changed manual protection or durable restoration state: row=%+v record=%+v", row, r)
			}
			if r.QualityScope != accountQualityScope || r.QualityLegacyAnchor != anchor || r.QualityHistoryRetry != nil ||
				r.BadWindows != 0 || r.QualityReason != "" || r.QualityEvaluatedAt != nil || row.QualitySampling == nil ||
				row.QualitySampling.BadWindows != 0 || strings.Contains(row.QualitySampling.Reason, "旧质量异常") {
				t.Fatalf("manual account still exposes incompatible quality progress: row=%+v record=%+v", row, r)
			}
		})
	}
}

func TestQualityHistoryIsBoundedIndependentAndPersistent(t *testing.T) {
	e, c, api, store, now := setup(t,
		account(7, "openai", "apikey", 5, true), account(8, "anthropic", "apikey", 4, true))
	c.AccountSettings[0].Group = "always_on"
	c.AccountSettings[1].Group = "good"
	configureQuality(&c, "always_on", .8, 1000)
	rule := c.GroupRules["always_on"]
	rule.MinInputTokens = 49000
	c.PlatformGroupRules = map[string]map[string]config.Rule{"openai": {"always_on": rule}}
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if api.usageThresholds[7] != 49000 || api.usageThresholds[8] != 40000 {
		t.Fatalf("wrong thresholds by platform/group: %v", api.usageThresholds)
	}
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i+1) * time.Minute)
		b := qualityWindow(at, 100+20*i, "m", "sync", .9)
		b.FirstTokenSamples, b.AverageFirstTokenMs = 20, ptr(100)
		if i == 2 {
			b.FirstTokenSamples, b.AverageFirstTokenMs = 19, nil
		}
		if i == 3 {
			b.CacheRatio = ptr(.7)
			b.ExcludedSmallRequests, b.ExcludedUnknownRequests = 2, 1
		}
		partial := &adminapi.QualityWindow{Requests: 19, WindowKnown: true,
			Models: []adminapi.QualityModelUsage{{Model: "m", RequestType: "stream", Requests: 19}}}
		api.usage[7] = adminapi.Window{QualityCurrent: partial, QualityWindows: []adminapi.QualityWindow{b}}
		e.Cycle(context.Background(), at)
	}
	restarted := restartMonitor(t, e, c, store, now.Add(5*time.Minute))
	restarted.Cycle(context.Background(), now.Add(5*time.Minute))
	row := snapshotAccount(t, restarted, 7)
	if row.MinInputTokens != 49000 || row.State != "ready" || !row.Schedulable || row.QualitySampling == nil ||
		row.QualitySampling.Requests != 19 || len(row.QualitySampling.Models) != 1 {
		t.Fatalf("always-on or current partial window wrong after restart: %+v", row)
	}
	sampling := row.QualitySampling
	h := sampling.History
	if sampling.BadWindows != 0 || sampling.ExcludedSmallRequests != 0 || sampling.ExcludedUnknownRequests != 0 || len(h) != 3 ||
		h[0].Result != "abnormal" || h[1].Result != "unknown" || h[2].Result != "healthy" ||
		h[0].MinInputTokens != 49000 || h[0].ExcludedSmallRequests != 2 || h[0].ExcludedUnknownRequests != 1 ||
		h[0].Requests != 20 || !h[0].WindowEnd.Equal(now.Add(4*time.Minute)) ||
		!h[2].WindowEnd.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("history is not durable, bounded, and truthfully ordered: %+v", sampling)
	}
}

func TestQualityHistoryDoesNotLeakIntoNewSession(t *testing.T) {
	e, c, api, _, now := setup(t, account(7, "openai", "apikey", 5, true))
	current := now
	e.clock = func() time.Time { return current }
	e.Cycle(context.Background(), current)
	current = now.Add(time.Minute)
	qualityCycle(e, api, current, 7, qualityWindow(current, 100, "m", "sync", .9))
	row := snapshotAccount(t, e, 7)
	if row.QualitySampling == nil || len(row.QualitySampling.History) != 1 {
		t.Fatal("first session did not persist its history")
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	current = now.Add(2 * time.Minute)
	e.Cycle(context.Background(), current)
	c.AutomationEnabled = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	qualityCycle(e, api, current, 7)
	if row = snapshotAccount(t, e, 7); row.QualitySampling == nil || row.QualitySampling.Requests != 0 || len(row.QualitySampling.History) != 0 {
		t.Fatalf("historical samples from another session leaked into a no-traffic session: %+v", row)
	}
}

func setupHTTPQuality(t *testing.T, ratio, ttft float64) (*Engine, config.Config, *memoryStore, time.Time,
	func(int, time.Time, string, string, string), func()) {
	t.Helper()
	a := account(7, "openai", "apikey", 8, true)
	_, c, _, _, now := setup(t, a)
	store := &memoryStore{}
	c.UpstreamRatesEnabled = false
	configureQuality(&c, "general", ratio, ttft)
	var mu sync.Mutex
	var rows []adminapi.Usage
	failNextUsage := false
	respond := func(w http.ResponseWriter, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/admin/accounts/7":
			if r.Method == http.MethodGet {
				respond(w, a)
				return
			}
		case "/admin/accounts/7/schedulable":
			var body struct {
				Schedulable bool `json:"schedulable"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				a.Schedulable = body.Schedulable
				respond(w, a)
				return
			}
		case "/admin/usage":
			if failNextUsage {
				failNextUsage = false
				http.Error(w, "test usage failure", http.StatusBadGateway)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			// As with the host, later pages are older, and time ties use numeric IDs.
			start := max(0, len(rows)-page*100)
			end := max(0, len(rows)-(page-1)*100)
			respond(w, map[string]any{"items": rows[start:end]})
			return
		}
		http.Error(w, "unexpected test endpoint", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	c.AdminURL = server.URL
	e := New("test", "node-a")
	e.clock = func() time.Time { return now }
	e.SetStore(store)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	add := func(count int, at time.Time, state, model, requestType string) {
		mu.Lock()
		defer mu.Unlock()
		for range count {
			input, creation, read, ttft := int64(5000), int64(0), int64(35000), int64(100)
			switch state {
			case "bad":
				input, read, ttft = 20000, 20000, 300
			case "high-cache":
				input, read, ttft = 10000, 90000, 120
			case "small":
				input, ttft = 4999, 9000
			case "boundary-low":
				input, read = 20000, 20000
			case "boundary-mid":
				input, read = 11200, 28800
			case "boundary-high":
				input, read = 4000, 36000
			case "boundary-top":
				input, read = 0, 40000
			case "boundary-top-slow":
				input, read, ttft = 0, 40000, 20000
			}
			row := adminapi.Usage{ID: int64(len(rows) + 1), AccountID: 7, CreatedAt: at,
				Model: model, RequestType: requestType, InputTokens: &input, CacheCreationTokens: &creation, CacheReadTokens: &read, FirstTokenMs: &ttft}
			if state == "unknown" {
				row.CacheCreationTokens = nil
			}
			if state == "unknown-ttft" {
				row.FirstTokenMs = nil
			}
			rows = append(rows, row)
		}
	}
	return e, c, store, now, add, func() {
		mu.Lock()
		defer mu.Unlock()
		failNextUsage = true
	}
}

func TestHTTPQualityAtExactCacheBoundaryKeepsExcellentExclusivity(t *testing.T) {
	e, c, _, now, add, _ := setupHTTPQuality(t, .8, 1000)
	c.AccountSettings[0].Group, c.AccountSettings[0].FixedRate = "excellent", ptr(.01)
	c.GroupPriorities["excellent"] = c.GroupPriorities["general"]
	configureQuality(&c, "excellent", .8, 1000)
	rule := c.GroupRules["excellent"]
	rule.ExclusiveMaxAverageFirstTokenMs = 1000
	c.GroupRules["excellent"] = rule
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	for i := 1; i <= 2; i++ {
		at := now.Add(time.Duration(i) * 180 * time.Second)
		add(2, at, "boundary-low", "a", "sync")
		add(10, at, "boundary-mid", "a", "sync")
		add(2, at, "boundary-high", "b", "stream")
		add(4, at, "boundary-top", "b", "stream")
		add(2, at, "boundary-top-slow", "b", "stream")
		e.Cycle(context.Background(), at)
		row := snapshotAccount(t, e, 7)
		if !row.Schedulable || row.State != "ready" || strings.Contains(row.Reason, "退出独占") || row.CacheRatio == nil || *row.CacheRatio != .8 ||
			row.AverageFirstTokenMs == nil || *row.AverageFirstTokenMs != 100 || row.QualitySampling == nil ||
			row.QualitySampling.GoodWindows != i || row.QualitySampling.BadWindows != 0 ||
			len(row.QualitySampling.History) != i || row.QualitySampling.History[0].Result != "healthy" {
			t.Fatalf("an exact cache boundary or discarded TTFT spike changed scheduling: row=%+v sampling=%+v", row, row.QualitySampling)
		}
	}
}

func TestCachePolicyMigrationPreservesObservationCursorAndHold(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	first := qualityWindow(now, 1, "a", "sync", .7)
	observed := qualityWindow(now, 21, "a", "sync", .7)
	hold := now.Add(time.Hour)
	record := &ActionRecord{
		QualityScope: "account-trimmed-v2", LastQualityWindow: first.LastSample,
		FirstTokenThrough: observed.LastSample, BadWindows: 1,
		FailureKind: "quality", FailureCount: 2, HoldUntil: &hold,
	}
	migrateQualityWindow(record)
	if record.QualityScope != accountQualityScope || record.QualityLegacyAnchor != observed.LastSample ||
		freshQualityWindow(observed, record.QualityLegacyAnchor) || record.BadWindows != 0 || record.HoldUntil != &hold || record.FailureCount != 2 {
		t.Fatalf("cache policy migration lost a hold or reused old quality evidence: %+v", record)
	}
}

func TestHTTPQualityUpgradesAccountV1DuringRecovery(t *testing.T) {
	e, c, store, now, add, _ := setupHTTPQuality(t, .8, 1000)
	oldFirst, oldPrior := now.Add(time.Second), now.Add(2*time.Second)
	trial := now.Add(3 * time.Minute)
	lastOld := trial.Add(time.Minute)
	key := func(at time.Time, id int) string { return at.Format(time.RFC3339Nano) + ":" + strconv.Itoa(id) }
	add(15, oldFirst, "bad", "old", "sync")
	add(15, oldPrior, "bad", "old", "sync")
	add(15, lastOld, "healthy", "old-trial", "sync")
	ctx := context.Background()
	history, err := json.Marshal(accountQualityHistory{AdminURL: c.AdminURL, Executor: c.ExecutorHostname,
		SamplingSince: now, Scope: "account-v1", LastSample: key(oldFirst, 15),
		History: []QualityWindowHistory{{Requests: 15, Result: "abnormal"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, historyKey(7), history); err != nil {
		t.Fatal(err)
	}
	row := AccountStatus{ID: 7}
	if h := readQualityHistory(ctx, store, c, &row, now); h == nil || h.LastSample != key(oldFirst, 15) || len(h.History) != 0 {
		t.Fatalf("upgrade did not preserve the history boundary without old results: %+v", h)
	}
	ledger := ledgerOf(t, store)
	r := ledger.Records["7"]
	deadline := trial.Add(15 * time.Minute)
	r.QualityScope, r.Policy.MinRequests, r.GoodWindows = "account-v1", 15, 1
	r.LastQualityWindow, r.QualityReason = key(lastOld, 45), "旧窗口健康"
	r.QualityHistoryRetry = &qualityHistoryRetry{Since: now, Through: key(oldPrior, 30)}
	r.SampleSince, r.RecoveryStartedAt, r.RecoveryDeadline = &trial, &trial, &deadline
	r.RecoveryTrial, r.FailureKind, r.FailureCount, r.DegradationLevel = true, "quality", 2, 2
	if err := save(ctx, store, &ledger); err != nil {
		t.Fatal(err)
	}
	at := trial.Add(5 * time.Minute)
	add(19, at, "healthy", "fresh", "sync")
	e = restartMonitor(t, e, c, store, at)
	e.Cycle(ctx, at)
	row, r = snapshotAccount(t, e, 7), ledgerOf(t, store).Records["7"]
	b := row.QualitySampling
	if row.Error != "" || row.State != "recovering" || b == nil || b.Requests != 19 || len(b.History) != 0 || b.GoodWindows != 0 ||
		r.QualityScope != accountQualityScope || r.QualityLegacyAnchor != key(lastOld, 45) || r.QualityHistoryRetry != nil ||
		!r.RecoveryTrial || r.RecoveryStartedAt == nil || !r.RecoveryStartedAt.Equal(trial) ||
		r.RecoveryDeadline == nil || !r.RecoveryDeadline.Equal(deadline) || r.FailureCount != 2 || r.DegradationLevel != 2 {
		t.Fatalf("upgrade reused old rows or lost trial state: row=%+v sampling=%+v record=%+v", row, b, r)
	}
	add(1, at.Add(time.Second), "healthy", "fresh", "sync")
	e.Cycle(ctx, at.Add(time.Second))
	row = snapshotAccount(t, e, 7)
	b = row.QualitySampling
	if row.State != "recovering" || b.GoodWindows != 1 || b.Requests != 0 || len(b.History) != 1 ||
		b.History[0].Requests != 20 || !b.History[0].WindowStart.Equal(at) {
		t.Fatalf("the first fresh window did not seal or inherited old recovery progress: row=%+v sampling=%+v", row, b)
	}
	add(20, at.Add(2*time.Second), "healthy", "fresh", "stream")
	e.Cycle(ctx, at.Add(2*time.Second))
	row = snapshotAccount(t, e, 7)
	if row.State != "ready" || row.RecoveryTrial || row.QualityGoodWindows != 2 {
		t.Fatalf("two fresh windows did not complete recovery: %+v", row)
	}
}

func TestHTTPQualitySealsConsecutiveWindowsAndShowsTrailingCalls(t *testing.T) {
	e, c, store, now, add, _ := setupHTTPQuality(t, .8, 0)
	// An old per-model history cursor consumed 15 calls before the upgrade.
	// The account history must start after that cursor and discard its old result.
	add(15, now.Add(time.Second), "healthy", "legacy", "sync")
	legacy := map[string]any{"admin_url": c.AdminURL, "executor": c.ExecutorHostname,
		"sampling_since": now, "buckets": map[string]any{"legacy": map[string]any{
			"last_sample": now.Add(time.Second).Format(time.RFC3339Nano) + ":15",
			"history":     []any{map[string]any{"requests": 15, "result": "healthy"}},
		}}}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), historyKey(7), legacyRaw); err != nil {
		t.Fatal(err)
	}

	add(1, now.Add(2*time.Second), "small", "a", "sync")
	add(10, now.Add(15*time.Second), "bad", "a", "sync")
	at := now.Add(180 * time.Second)
	e.Cycle(context.Background(), at)
	b := snapshotAccount(t, e, 7).QualitySampling
	if b == nil || b.Requests != 10 || len(b.History) != 0 || len(b.Models) != 1 || b.Models[0].Requests != 10 {
		t.Fatalf("legacy history leaked or 10 requests did not remain in the account window: %+v", b)
	}
	add(10, at.Add(time.Second), "high-cache", "b", "stream")
	at = at.Add(time.Second)
	e.Cycle(context.Background(), at)
	row := snapshotAccount(t, e, 7)
	b = row.QualitySampling
	if b == nil || b.Requests != 0 || len(b.History) != 1 {
		t.Fatalf("10+10 requests did not make a single account window: %+v", b)
	}
	h := b.History[0]
	if h.Requests != 20 || h.FirstTokenSamples != 20 || len(h.Models) != 2 ||
		h.Models[0].Model != "a" || h.Models[0].RequestType != "sync" || h.Models[0].Requests != 10 ||
		h.Models[1].Model != "b" || h.Models[1].RequestType != "stream" || h.Models[1].Requests != 10 ||
		h.CacheRatio == nil || *h.CacheRatio != .7 ||
		h.AverageFirstTokenMs == nil || *h.AverageFirstTokenMs != 210 ||
		h.Result != "abnormal" || h.ExcludedSmallRequests != 1 || b.BadWindows != 1 {
		t.Fatalf("incorrect window metrics and model details: %+v", h)
	}

	add(1, at.Add(time.Second), "unknown", "a", "sync")
	add(20, at.Add(2*time.Second), "healthy", "a", "sync")
	add(20, at.Add(3*time.Second), "bad", "b", "stream")
	add(1, at.Add(4*time.Second), "small", "a", "sync")
	add(1, at.Add(4*time.Second), "unknown", "b", "stream")
	partialAt := at.Add(5 * time.Second)
	add(2, partialAt, "bad", "a", "sync")
	add(2, partialAt, "bad", "b", "stream")
	at = partialAt
	beforeWrite, _ := store.Get(context.Background(), historyKey(7))
	store.historyWriteError = true
	e.Cycle(context.Background(), at)
	failedHistory := snapshotAccount(t, e, 7)
	afterFailedWrite, _ := store.Get(context.Background(), historyKey(7))
	if failedHistory.QualitySampling.BadWindows != 1 || len(failedHistory.QualitySampling.History) != 1 ||
		string(beforeWrite) != string(afterFailedWrite) ||
		!strings.Contains(failedHistory.Error, "无法保存质量窗口历史") {
		t.Fatalf("history I/O failure affected the sealed decision or published unsaved history: %+v", failedHistory)
	}
	store.historyWriteError = false
	e.Cycle(context.Background(), at)
	row = snapshotAccount(t, e, 7)
	if row.QualitySampling == nil {
		t.Fatalf("real HTTP collection did not produce account quality sampling: %+v", row)
	}
	b = row.QualitySampling
	if !row.Schedulable || !b.WindowKnown || b.Requests != 4 || b.ExcludedSmallRequests != 1 ||
		b.ExcludedUnknownRequests != 1 || b.BadWindows != 1 || len(b.History) != 3 ||
		b.History[0].Result != "abnormal" || b.History[1].Result != "healthy" ||
		b.History[2].Result != "abnormal" || b.History[1].ExcludedUnknownRequests != 1 ||
		b.History[2].ExcludedSmallRequests != 1 {
		t.Fatalf("first batch must seal 3 ordered windows, apply the healthy reset and retain true partial: %+v", b)
	}
	serialized, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Sampling *QualityStatus  `json:"quality_sampling"`
		Buckets  json.RawMessage `json:"quality_buckets"`
	}
	if err := json.Unmarshal(serialized, &output); err != nil {
		t.Fatal(err)
	}
	if output.Sampling == nil || output.Buckets != nil || len(output.Sampling.Models) != 2 {
		t.Fatalf("unexpected account status JSON contract: %s", serialized)
	}
	checkTotal := func(requests int, models []adminapi.QualityModelUsage) {
		t.Helper()
		total := 0
		for _, model := range models {
			total += model.Requests
		}
		if total != requests {
			t.Fatalf("model counts total %d with %d account requests: %+v", total, requests, models)
		}
	}
	checkTotal(output.Sampling.Requests, output.Sampling.Models)
	for _, entry := range output.Sampling.History {
		checkTotal(entry.Requests, entry.Models)
	}
	sealedAt := at
	// Time alone cannot reconfirm an already-consumed window.
	at = sealedAt.Add(180 * time.Second)
	e = restartMonitor(t, e, c, store, at)
	e.Cycle(context.Background(), at)
	b = snapshotAccount(t, e, 7).QualitySampling
	if b.BadWindows != 1 || b.Requests != 4 || len(b.History) != 3 || b.ExcludedSmallRequests != 1 ||
		!b.History[0].RecordedAt.Equal(sealedAt) {
		t.Fatalf("unchanged poll re-used a sealed or partial window: %+v", b)
	}
	add(16, at.Add(time.Second), "bad", "c", "ws_v2")
	e.Cycle(context.Background(), at.Add(time.Second))
	row = snapshotAccount(t, e, 7)
	b = row.QualitySampling
	if row.State != "paused" || row.Schedulable || b.Requests != 0 || len(b.History) != 3 ||
		b.History[0].ExcludedSmallRequests != 1 || b.History[0].ExcludedUnknownRequests != 1 ||
		!b.History[0].WindowStart.Equal(partialAt) ||
		!b.History[0].WindowEnd.Equal(at.Add(time.Second)) || ledgerOf(t, store).Records["7"].FailureCount != 1 {
		t.Fatalf("16 new eligible calls did not close the partial into the next window and confirm a fault: row=%+v", row)
	}
}

func TestHTTPQualityKeepsDecisionBoundaryWhenHistoryFallsBeyondScanCap(t *testing.T) {
	e, c, store, now, add, _ := setupHTTPQuality(t, .8, 0)
	at := now.Add(180 * time.Second)
	add(20, at, "healthy", "a", "sync")
	e.Cycle(context.Background(), at)
	store.historyWriteError = true
	at = at.Add(time.Minute)
	add(4800, at, "healthy", "a", "sync")
	e.Cycle(context.Background(), at)
	b := snapshotAccount(t, e, 7).QualitySampling
	if b == nil || b.GoodWindows != 241 || len(b.History) != 1 {
		t.Fatalf("decision progress was not retained across diagnostic write failure: %+v", b)
	}
	at = at.Add(time.Minute)
	add(301, at, "small", "b", "stream")
	store.historyWriteError = false
	e = restartMonitor(t, e, c, store, at)
	e.Cycle(context.Background(), at)
	row := snapshotAccount(t, e, 7)
	b = row.QualitySampling
	if !row.QualityScanLimitReached || b == nil || !b.WindowKnown ||
		b.GoodWindows != 241 || b.Requests != 0 || b.ExcludedSmallRequests != 301 || len(b.History) != 3 ||
		!b.History[0].WindowEnd.Equal(at.Add(-time.Minute)) || b.History[0].Models[0].Model != "a" {
		t.Fatalf("capped scan did not align and restore the three visible predecessors: row=%+v sampling=%+v", row, b)
	}
	add(21, at, "healthy", "b", "stream")
	e.Cycle(context.Background(), at)
	b = snapshotAccount(t, e, 7).QualitySampling
	if !b.WindowKnown || b.GoodWindows != 242 || b.Requests != 1 || len(b.History) != 3 ||
		b.History[0].ExcludedSmallRequests != 301 || b.History[0].Models[0].Model != "b" ||
		b.History[1].Models[0].Model != "a" {
		t.Fatalf("repeated poll re-used quality windows restored after the cap: %+v", b)
	}
}

func TestHTTPQualityRetriesDiagnosticHistoryAcrossTrialBoundary(t *testing.T) {
	e, c, store, now, add, _ := setupHTTPQuality(t, .8, 1000)
	firstBad := now.Add(180 * time.Second)
	add(20, firstBad, "bad", "a", "sync")
	e.Cycle(context.Background(), firstBad)
	faultAt := firstBad.Add(180 * time.Second)
	add(20, faultAt, "bad", "b", "stream")
	store.historyWriteError = true
	e.Cycle(context.Background(), faultAt)
	record := ledgerOf(t, store).Records["7"]
	if record.HoldUntil == nil || record.FailureCount != 1 {
		t.Fatalf("two bad account windows did not begin the hold: %+v", record)
	}
	trial := *record.HoldUntil
	// A trailing pre-trial partial cannot be combined with trial traffic.
	add(19, trial.Add(-time.Second), "healthy", "pretrial", "sync")
	e = restartMonitor(t, e, c, store, trial)
	e.Cycle(context.Background(), trial)
	record = ledgerOf(t, store).Records["7"]
	if record.RecoveryStartedAt == nil || !record.RecoveryStartedAt.Equal(trial) || record.QualityHistoryRetry == nil {
		t.Fatalf("trial did not persist the unfinished diagnostic boundary: %+v", record)
	}
	store.historyWriteError = false
	next := trial.Add(300 * time.Second)
	e = restartMonitor(t, e, c, store, next)
	add(1, next, "healthy", "trial", "stream")
	e.Cycle(context.Background(), next)
	row := snapshotAccount(t, e, 7)
	b := row.QualitySampling
	if row.State != "recovering" || b == nil || b.Requests != 1 || b.GoodWindows != 0 || len(b.Models) != 1 ||
		b.Models[0].Model != "trial" || len(b.History) != 2 || !b.History[0].WindowEnd.Equal(faultAt) ||
		b.History[0].Result != "abnormal" {
		t.Fatalf("diagnostic retry lost old history or mixed partial calls across the trial boundary: row=%+v sampling=%+v", row, b)
	}
	add(18, next.Add(time.Second), "healthy", "trial", "stream")
	add(1, next.Add(time.Second), "unknown-ttft", "trial", "stream")
	add(20, next.Add(2*time.Second), "healthy", "another", "ws_v2")
	e.Cycle(context.Background(), next.Add(2*time.Second))
	b = snapshotAccount(t, e, 7).QualitySampling
	if record = ledgerOf(t, store).Records["7"]; !record.RecoveryTrial || record.QualityHistoryRetry != nil ||
		b.GoodWindows != 1 || len(b.History) != 3 || b.History[1].Result != "unknown" ||
		b.History[1].FirstTokenSamples != 19 || b.History[1].AverageFirstTokenMs != nil ||
		b.History[1].Models[0].AverageFirstTokenMs == nil || b.History[1].Models[0].FirstTokenSamples != 19 {
		t.Fatalf("unknown TTFT counted as recovery or prior retry remained active: record=%+v sampling=%+v", record, b)
	}
	add(20, next.Add(3*time.Second), "healthy", "replacement", "sync")
	e.Cycle(context.Background(), next.Add(3*time.Second))
	row = snapshotAccount(t, e, 7)
	if row.State != "ready" || row.RecoveryTrial || row.QualityGoodWindows != 2 || row.FailureCount != 1 {
		t.Fatalf("two account-wide healthy windows did not complete recovery: %+v", row)
	}
}

func TestHTTPQualityRetriesPriorHistoryAfterTransientReadFailure(t *testing.T) {
	e, _, store, now, add, failNextUsage := setupHTTPQuality(t, .8, 1000)
	firstBad := now.Add(180 * time.Second)
	add(20, firstBad, "bad", "a", "sync")
	e.Cycle(context.Background(), firstBad)
	faultAt := firstBad.Add(180 * time.Second)
	add(20, faultAt, "bad", "b", "stream")
	store.historyWriteError = true
	e.Cycle(context.Background(), faultAt)
	record := ledgerOf(t, store).Records["7"]
	if record.HoldUntil == nil {
		t.Fatal("fault did not begin the hold")
	}
	trial := *record.HoldUntil
	e.Cycle(context.Background(), trial)
	store.historyWriteError = false
	next := trial.Add(time.Minute)
	add(20, next, "healthy", "trial", "stream")
	failNextUsage()
	e.Cycle(context.Background(), next)
	row := snapshotAccount(t, e, 7)
	b := row.QualitySampling
	if row.State != "recovering" || b == nil || b.GoodWindows != 1 || len(b.History) != 1 ||
		!strings.Contains(row.Error, "无法补齐") {
		t.Fatalf("a transient diagnostic read changed live decisions or displaced retryable old history: row=%+v sampling=%+v", row, b)
	}
	e.Cycle(context.Background(), next)
	b = snapshotAccount(t, e, 7).QualitySampling
	if b == nil || b.GoodWindows != 1 || len(b.History) != 3 || !b.History[0].WindowEnd.Equal(next) ||
		!b.History[1].WindowEnd.Equal(faultAt) {
		t.Fatalf("transient failure prevented both old and live windows being saved in order: %+v", b)
	}
}

func TestHTTPQualityRecoversCappedPriorHistoryDuringTrial(t *testing.T) {
	e, c, store, now, add, _ := setupHTTPQuality(t, .8, 0)
	first := now.Add(180 * time.Second)
	add(20, first, "bad", "a", "sync")
	e.Cycle(context.Background(), first)
	store.historyWriteError = true
	second := first.Add(time.Minute)
	add(4800, second, "bad", "b", "sync")
	e.Cycle(context.Background(), second)
	fault := first.Add(180 * time.Second)
	add(301, fault, "small", "c", "stream")
	add(20, fault, "bad", "c", "stream")
	e.Cycle(context.Background(), fault)
	record := ledgerOf(t, store).Records["7"]
	if record.HoldUntil == nil {
		t.Fatalf("capped scan did not confirm the new bad window: %+v", record)
	}
	trial := *record.HoldUntil
	e.Cycle(context.Background(), trial)
	store.historyWriteError = false
	e = restartMonitor(t, e, c, store, trial.Add(time.Minute))
	e.Cycle(context.Background(), trial.Add(time.Minute))
	row := snapshotAccount(t, e, 7)
	b := row.QualitySampling
	if row.State != "recovering" || b == nil || b.Requests != 0 || b.GoodWindows != 0 ||
		len(b.History) != 3 || !b.History[0].WindowEnd.Equal(fault) ||
		b.History[0].ExcludedSmallRequests != 301 || b.History[1].Models[0].Model != "b" {
		t.Fatalf("prior diagnostic replay did not recover the latest windows in the capped scan: row=%+v sampling=%+v", row, b)
	}
}

func TestSmallOnlyRequestsDoNotPopulateSchedulingProgress(t *testing.T) {
	e, c, api, store, now := setup(t,
		account(7, "openai", "apikey", 3, true), account(8, "openai", "apikey", 4, false))
	store.maxValueSize = 256 * 1024
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	until := now.Add(time.Hour)
	a := api.accounts[7]
	a.OverloadUntil = &until
	api.accounts[7] = a
	api.usage[8] = adminapi.Window{QualityCurrent: &adminapi.QualityWindow{WindowKnown: true, ExcludedSmallRequests: 5000}}
	e.Cycle(context.Background(), now.Add(3*time.Minute))
	sampling := snapshotAccount(t, e, 8).QualitySampling
	if ledger := ledgerOf(t, store); api.accounts[7].Schedulable || !api.accounts[8].Schedulable ||
		ledger.Records["8"].LastQualityWindow != "" || sampling == nil || sampling.ExcludedSmallRequests != 5000 || len(sampling.Models) != 0 {
		t.Fatal("small requests blocked failover or advanced quality/model usage")
	}
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(4*time.Minute))
	if host := api.accounts[8]; host.Schedulable || host.Priority != 4 || len(ledgerOf(t, store).Records) != 0 {
		t.Fatal("small requests prevented stop from restoring prior ownership")
	}
}

func TestTemporaryUnavailableAndAccountErrorHaveSeparateFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		temporary bool
		kind      string
	}{
		{"temporary timer", "active", true, "temporary"},
		{"account error", "error", false, "account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := account(7, "openai", "apikey", 5, true)
			a.Status = tc.status
			if tc.status == "error" {
				a.Schedulable = false
			}
			if tc.temporary {
				until := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
				a.OverloadUntil = &until
			}
			e, c, api, store, now := setup(t, a)
			c.AccountSettings[0].Group = "always_on"
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			e.Cycle(context.Background(), now)
			record, row := ledgerOf(t, store).Records["7"], snapshotAccount(t, e, 7)
			if record == nil || record.FailureKind != tc.kind || row.FailureKind != tc.kind ||
				record.FailureCount != 1 || record.DegradationLevel != 1 {
				t.Fatalf("failure kind/count conflated: record=%+v row=%+v", record, row)
			}
			if tc.temporary && (api.clearCalls != 1 || row.State != "recovering" || !api.accounts[7].Schedulable) {
				t.Fatalf("temporary timer was not cleared and confirmed separately: row=%+v clearCalls=%d", row, api.clearCalls)
			}
			if tc.temporary && (row.Status != "active" || row.Availability != "" ||
				api.accounts[7].OverloadUntil != nil || row.State == "temporary_retry") {
				t.Fatalf("second scheduler pass reused stale temporary status: row=%+v host=%+v", row, api.accounts[7])
			}
			if !tc.temporary && (api.clearCalls != 0 || row.State != "account_error") {
				t.Fatalf("account error was automatically cleared: row=%+v calls=%d", row, api.clearCalls)
			}
		})
	}
}

func TestOwnedAccountStatusErrorIsNotManualChangeAndCanRecover(t *testing.T) {
	excellent := account(7, "openai", "apikey", 3, true)
	good := account(8, "openai", "apikey", 4, true)
	e, c, api, store, now := setup(t, excellent, good)
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "excellent", "good"
	configureQuality(&c, "excellent", 0, 0)
	configureQuality(&c, "good", 0, 0)
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	if !api.accounts[7].Schedulable || api.accounts[8].Schedulable {
		t.Fatal("test did not establish an owned active account and closed fallback")
	}

	hostAccount := api.accounts[7]
	hostAccount.Status, hostAccount.Schedulable = "error", false
	api.accounts[7] = hostAccount
	e.Cycle(context.Background(), now.Add(time.Minute))
	row, record := snapshotAccount(t, e, 7), ledgerOf(t, store).Records["7"]
	if row.State != "account_error" || row.FailureKind != "account" || record == nil ||
		!record.AccountFault || record.FailureKind != "account" || api.accounts[7].Schedulable ||
		!api.accounts[8].Schedulable || api.clearCalls != 0 {
		t.Fatalf("host account error was treated as manual or not failed over: row=%+v record=%+v accounts=%+v clears=%d",
			row, record, api.accounts, api.clearCalls)
	}

	hostAccount = api.accounts[7]
	hostAccount.Status = "active"
	api.accounts[7] = hostAccount
	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	row, record = snapshotAccount(t, e, 7), ledgerOf(t, store).Records["7"]
	if row.State != "recovering" || !record.RecoveryTrial || !api.accounts[7].Schedulable ||
		!api.accounts[8].Schedulable || api.clearCalls != 0 {
		t.Fatalf("host repair did not start an owned recovery trial with fallback open: row=%+v record=%+v accounts=%+v clears=%d",
			row, record, api.accounts, api.clearCalls)
	}
}

func TestRestoreNeverReopensHostOwnedErrorClosure(t *testing.T) {
	for _, action := range []string{"stop", "remove"} {
		t.Run(action, func(t *testing.T) {
			e, c, api, store, now := setup(t, account(7, "openai", "apikey", 3, true))
			e.Cycle(context.Background(), now)
			host := api.accounts[7]
			if !host.Schedulable || host.Priority != 8 {
				t.Fatalf("test did not start with an owned open account: %+v", host)
			}
			host.Status, host.Schedulable = "error", false
			api.accounts[7] = host
			if action == "stop" {
				c.AutomationEnabled = false
			} else {
				c.AccountIDs, c.AccountSettings = nil, nil
			}
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			e.Cycle(context.Background(), now.Add(time.Minute))
			if got := api.accounts[7]; got.Status != "error" || got.Schedulable || got.Priority != 3 ||
				api.clearCalls != 0 || len(ledgerOf(t, store).Records) != 0 {
				t.Fatalf("restoration must restore only owned priority, not undo host closure: %+v, clears=%d",
					got, api.clearCalls)
			}
		})
	}
}

func TestHostErrorKeepsUnconfirmedPriorityRestorePending(t *testing.T) {
	e, c, api, store, now := setup(t, account(7, "openai", "apikey", 3, true))
	e.Cycle(context.Background(), now)
	host := api.accounts[7]
	if host.Priority != 8 || !host.Schedulable {
		t.Fatalf("test did not establish owned priority 8 on an open account: %+v", host)
	}
	host.Status, host.Schedulable = "error", false
	api.accounts[7], api.ignoreWrite = host, true
	c.AutomationEnabled = false
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now.Add(time.Minute))
	row, record := snapshotAccount(t, e, 7), ledgerOf(t, store).Records["7"]
	if record == nil || record.PendingField != "priority" || record.PendingPriority != 3 ||
		row.Priority != 8 || row.Schedulable || api.accounts[7].Priority != 8 || api.accounts[7].Schedulable ||
		api.clearCalls != 0 {
		t.Fatalf("successful API response without readback incorrectly finalized ownership: row=%+v record=%+v host=%+v",
			row, record, api.accounts[7])
	}

	api.ignoreWrite = false
	e.Cycle(context.Background(), now.Add(2*time.Minute))
	row = snapshotAccount(t, e, 7)
	if row.Priority != 3 || row.Schedulable || api.accounts[7].Priority != 3 || api.accounts[7].Schedulable ||
		api.clearCalls != 0 || len(ledgerOf(t, store).Records) != 0 {
		t.Fatalf("confirmed priority restoration did not finish safely: row=%+v host=%+v", row, api.accounts[7])
	}
}

func TestClosedTemporaryPriorityFailureDoesNotBlockOpeningExcellent(t *testing.T) {
	e, c, api, store, now := setup(t,
		account(7, "openai", "apikey", 3, false), account(8, "openai", "apikey", 6, false))
	c.AccountSettings[0].Group, c.AccountSettings[1].Group = "temporary", "excellent"
	api.priorityWriteError = map[int64]error{7: errors.New("priority write failed")}
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}
	e.Cycle(context.Background(), now)
	record := ledgerOf(t, store).Records["7"]
	if api.accounts[7].Schedulable || api.accounts[7].Priority != 3 ||
		record == nil || record.PendingField != "priority" || !api.accounts[8].Schedulable {
		t.Fatalf("priority-only error on an already closed account blocked an unrelated opening: accounts=%+v, record=%+v",
			api.accounts, record)
	}
}

func TestHostErrorAtPreflightAbortsStaleOpenOrClear(t *testing.T) {
	for _, action := range []string{"open", "clear"} {
		t.Run(action, func(t *testing.T) {
			a := account(7, "openai", "apikey", 6, false)
			group := "excellent"
			if action == "clear" {
				a.Priority, a.Schedulable, group = 5, true, "always_on"
				until := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
				a.OverloadUntil = &until
			}
			e, c, api, store, now := setup(t, a)
			c.AccountSettings[0].Group = group
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			reads := 0
			api.beforeAccount = func(id int64) {
				if id == 7 {
					reads++
					if reads == 2 {
						host := api.accounts[id]
						host.Status, host.Schedulable = "error", false
						api.accounts[id] = host
					}
				}
			}
			e.Cycle(context.Background(), now)
			record, row := ledgerOf(t, store).Records["7"], snapshotAccount(t, e, 7)
			if reads < 2 || row.State != "account_error" || row.Status != "error" || row.Schedulable ||
				record == nil || !record.AccountFault || record.Phase == "manual_change" ||
				api.accounts[7].Schedulable || api.writes != 0 || api.clearCalls != 0 {
				t.Fatalf("stale %s proceeded or host fault was not recorded: reads=%d writes=%d clears=%d row=%+v record=%+v",
					action, reads, api.writes, api.clearCalls, row, record)
			}
		})
	}
}

func TestChangedTemporaryTimerBypassesBackoffForNewClearAttempt(t *testing.T) {
	a := account(7, "openai", "apikey", 5, true)
	firstTimer := time.Date(2026, 11, 18, 12, 10, 0, 0, time.UTC)
	a.OverloadUntil = &firstTimer
	e, c, api, store, now := setup(t, a)
	c.AccountSettings[0].Group = "always_on"
	api.clearError = true
	if err := e.Apply(c); err != nil {
		t.Fatal(err)
	}

	e.Cycle(context.Background(), now)
	record := ledgerOf(t, store).Records["7"]
	if api.clearCalls != 1 || record.FailureKind != "temporary" || record.DegradationLevel != 1 ||
		record.HoldUntil == nil || snapshotAccount(t, e, 7).State != "temporary_retry" {
		t.Fatalf("initial temporary clear failure did not enter backoff: record=%+v row=%+v calls=%d",
			record, snapshotAccount(t, e, 7), api.clearCalls)
	}

	api.usage[7] = adminapi.Window{}
	e.Cycle(context.Background(), now.Add(4*time.Minute))
	if api.clearCalls != 1 {
		t.Fatalf("unchanged temporary state retried before its backoff: %d", api.clearCalls)
	}
	e.Cycle(context.Background(), now.Add(5*time.Minute))
	record = ledgerOf(t, store).Records["7"]
	if api.clearCalls != 2 || record.DegradationLevel != 2 || record.HoldUntil == nil ||
		!record.HoldUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("unchanged state did not back off exponentially: record=%+v calls=%d", record, api.clearCalls)
	}

	// A new host timer is a new failure state and must be checked immediately,
	// even though the retry hold for the prior timer has not expired.
	secondTimer := firstTimer.Add(5 * time.Minute)
	hostAccount := api.accounts[7]
	hostAccount.OverloadUntil = &secondTimer
	api.accounts[7] = hostAccount
	e.Cycle(context.Background(), now.Add(6*time.Minute))
	record = ledgerOf(t, store).Records["7"]
	if api.clearCalls != 3 || record.DegradationLevel != 3 || record.FailureCount != 2 ||
		record.HoldUntil == nil || !record.HoldUntil.Equal(now.Add(26*time.Minute)) {
		t.Fatalf("changed temporary state waited on the old backoff: record=%+v calls=%d", record, api.clearCalls)
	}
}
