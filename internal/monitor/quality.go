package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
)

const accountQualityScope = "account-v1"
const qualityHistoryLimit = 3

type legacyQualityTrack struct {
	LastSample   string `json:"last_sample,omitempty"`
	LegacyAnchor string `json:"legacy_anchor,omitempty"`
}

// Compare timestamps and numeric IDs, not their variable-width encodings.
func sampleAfter(a, b string) bool {
	ta, ia, okA := adminapi.ParseSampleKey(a)
	tb, ib, okB := adminapi.ParseSampleKey(b)
	return okA && okB && (ta.After(tb) || (ta.Equal(tb) && ia > ib))
}

func latestSample(current, candidate string) string {
	if _, _, ok := adminapi.ParseSampleKey(candidate); ok && (current == "" || sampleAfter(candidate, current)) {
		return candidate
	}
	return current
}

func legacyQualityAnchor(r *ActionRecord) string {
	if r == nil {
		return ""
	}
	if r.QualityScope == accountQualityScope {
		return r.QualityLegacyAnchor
	}
	// A single cutoff after all consumed legacy windows avoids reusing calls
	// or trying to combine pieces of incompatible per-model boundaries.
	last := latestSample("", r.LastQualityWindow)
	for _, track := range r.QualityTracks {
		last = latestSample(last, track.LastSample)
		last = latestSample(last, track.LegacyAnchor)
	}
	if kind, key, ok := strings.Cut(r.LastBadSample, ":"); ok && (kind == "cache" || kind == "ttft") {
		last = latestSample(last, key)
	}
	return last
}

func resetQualityProgress(r *ActionRecord) {
	r.BadWindows, r.GoodWindows = 0, 0
	r.LastQualityWindow, r.QualityReason = "", ""
	r.QualityBadSince, r.QualityEvaluatedAt = nil, nil
}

func migrateQualityWindow(r *ActionRecord) {
	if r.QualityScope == accountQualityScope {
		return
	}
	r.QualityLegacyAnchor = legacyQualityAnchor(r)
	r.QualityScope, r.QualityTracks = accountQualityScope, nil
	// Legacy releases omitted some default fields from stored rules.
	r.Policy = config.NormalizeRule(r.Policy)
	if r.FailureKind == "" && r.HoldUntil != nil &&
		(strings.HasPrefix(r.LastBadSample, "cache:") || strings.HasPrefix(r.LastBadSample, "ttft:")) {
		r.FailureKind = "quality"
	}
	r.LastBadSample = ""
	resetQualityProgress(r)
}

type accountQualityHistory struct {
	AdminURL      string                        `json:"admin_url"`
	Executor      string                        `json:"executor"`
	SamplingSince time.Time                     `json:"sampling_since"`
	Scope         string                        `json:"scope"`
	LastSample    string                        `json:"last_sample"`
	History       []QualityWindowHistory        `json:"history"`
	LegacyBuckets map[string]legacyQualityTrack `json:"buckets,omitempty"`
}

type qualityHistoryRetry struct {
	Since   time.Time `json:"since"`
	Through string    `json:"through"`
}

// Keep just the boundary of the last segment when a trial advances SampleSince.
// Its diagnostic windows can be reconstructed without storing them in actions.
func rememberPriorQualityHistory(r *ActionRecord, item *managedAccount, since time.Time) {
	if r.SampleSince != nil && r.SampleSince.After(since) {
		since = *r.SampleSince
	}
	through := r.LastQualityWindow
	if n := len(item.qualityWindows); n != 0 {
		through = latestSample(through, item.qualityWindows[n-1].LastSample)
	}
	at, _, ok := adminapi.ParseSampleKey(through)
	if !ok || at.Before(since) {
		return
	}
	if item.history != nil && item.history.LastSample != "" && !sampleAfter(through, item.history.LastSample) {
		return
	}
	r.QualityHistoryRetry = &qualityHistoryRetry{Since: since, Through: through}
}

func retryPriorQualityHistory(ctx context.Context, api API, row *AccountStatus, record *ActionRecord, rule config.Rule, history *accountQualityHistory) ([]adminapi.QualityWindow, bool) {
	retry := record.QualityHistoryRetry
	if retry == nil || (history.LastSample != "" && !sampleAfter(retry.Through, history.LastSample)) {
		return nil, false
	}
	until, _, ok := adminapi.ParseSampleKey(retry.Through)
	if !ok {
		return nil, false
	}
	w, err := api.RecentUsageSince(ctx, row.ID, retry.Since, until, rule.MinInputTokens)
	w.SealQuality(retry.Since, latestSample(history.LastSample, record.QualityLegacyAnchor))
	windows := w.QualityWindows
	if w.QualityScanLimitReached && w.QualityCurrent != nil && !w.QualityCurrent.WindowKnown {
		windows = w.QualityPredecessors(retry.Since, retry.Through, qualityHistoryLimit)
	}
	windows = slices.DeleteFunc(windows, func(window adminapi.QualityWindow) bool {
		return sampleAfter(window.LastSample, retry.Through)
	})
	if err != nil || len(windows) == 0 || windows[len(windows)-1].LastSample != retry.Through {
		row.Error = joinError(row.Error, "无法补齐恢复试运行前的质量窗口历史")
		return nil, true
	}
	return windows, false
}

// Keep bounded diagnostic histories separate from authoritative scheduling
// decisions and restoration data.
func historyKey(id int64) string { return "quality-history." + strconv.FormatInt(id, 10) }

func readQualityHistory(ctx context.Context, store Store, c config.Config, row *AccountStatus, session time.Time) *accountQualityHistory {
	raw, err := store.Get(ctx, historyKey(row.ID))
	var saved accountQualityHistory
	if err != nil || (len(raw) != 0 && json.Unmarshal(raw, &saved) != nil) {
		row.Error = joinError(row.Error, "无法读取质量窗口历史，当前窗口未确认")
		return nil
	}
	if saved.AdminURL != c.AdminURL || saved.Executor != c.ExecutorHostname || !saved.SamplingSince.Equal(session) {
		saved = accountQualityHistory{AdminURL: c.AdminURL, Executor: c.ExecutorHostname, SamplingSince: session}
	}
	if saved.Scope != accountQualityScope {
		saved.Scope, saved.LastSample, saved.History = accountQualityScope, "", nil
		for _, track := range saved.LegacyBuckets {
			saved.LastSample = latestSample(saved.LastSample, track.LastSample)
		}
	}
	saved.LegacyBuckets = nil
	return &saved
}

func qualityMeasurement(rule config.Rule, window adminapi.QualityWindow) (bad, known bool, reason string) {
	rule.MaxRateMultiplier = 0 // Price policy is evaluated separately by the scheduler.
	bad, known, reason, _ = evaluate(rule, AccountStatus{Complete: true,
		Requests: window.Requests, CacheRatio: window.Ratio(),
		AverageFirstTokenMs: window.AverageFirstTokenMs, FirstTokenSamples: window.FirstTokenSamples})
	return bad, known, reason
}

func freshQualityWindow(window adminapi.QualityWindow, after string) bool {
	return window.WindowKnown && window.Requests == config.QualityWindowRequests &&
		sampleAfter(window.LastSample, window.FirstSample) &&
		(after == "" || sampleAfter(window.FirstSample, after))
}

func writeQualityHistory(ctx context.Context, store Store, item *managedAccount, rule config.Rule, now time.Time) {
	saved := item.history
	if saved == nil || item.setting.Group == "temporary" {
		return
	}
	// Preserve the older cursor until its missing history is recovered or is
	// displaced by a complete set of three new windows.
	if item.priorQualityPending && len(item.priorQualityWindows)+len(item.qualityWindows) < qualityHistoryLimit {
		return
	}
	next := *saved
	for _, window := range slices.Concat(item.priorQualityWindows, item.qualityWindows) {
		if !freshQualityWindow(window, next.LastSample) {
			continue
		}
		start, _, _ := adminapi.ParseSampleKey(window.FirstSample)
		end, _, _ := adminapi.ParseSampleKey(window.LastSample)
		bad, known, reason := qualityMeasurement(rule, window)
		if rule.MinCacheRatio == 0 && rule.MaxAverageFirstTokenMs == 0 {
			known, reason = false, "未配置缓存或首字时间质量条件"
		}
		result := "unknown"
		if known {
			if bad {
				result = "abnormal"
			} else {
				result = "healthy"
			}
		}
		entry := QualityWindowHistory{WindowStart: start.UTC(), WindowEnd: end.UTC(), RecordedAt: now.UTC(),
			Requests: window.Requests, MinInputTokens: window.MinInputTokens,
			ExcludedSmallRequests: window.ExcludedSmallRequests, ExcludedUnknownRequests: window.ExcludedUnknownRequests,
			CacheRatio: window.Ratio(), AverageFirstTokenMs: window.AverageFirstTokenMs, FirstTokenSamples: window.FirstTokenSamples,
			Models: modelDetails(window.Models), Result: result, Reason: reason}
		next.History = append([]QualityWindowHistory{entry}, next.History[:min(qualityHistoryLimit-1, len(next.History))]...)
		next.LastSample = window.LastSample
	}
	if next.LastSample == saved.LastSample {
		return
	}
	raw, err := json.Marshal(next)
	if err != nil || store.Set(ctx, historyKey(item.row.ID), raw) != nil {
		item.row.Error = joinError(item.row.Error, "无法保存质量窗口历史")
		return
	}
	*saved = next
}

func evaluateQuality(r *ActionRecord, item *managedAccount, rule config.Rule, now time.Time, recovering bool) (failed, healthy bool) {
	row := &item.row
	if row.quality == nil || !row.quality.WindowKnown {
		return false, false
	}
	freshBad := false
	after := latestSample(r.QualityLegacyAnchor, r.LastQualityWindow)
	for _, window := range item.qualityWindows {
		if !freshQualityWindow(window, after) {
			continue
		}
		after, r.LastQualityWindow, r.QualityEvaluatedAt = window.LastSample, window.LastSample, &now
		bad, known, reason := qualityMeasurement(rule, window)
		r.QualityReason, freshBad = reason, known && bad
		switch {
		case !known:
			// An unknown completed window cannot continue either streak.
			r.BadWindows, r.GoodWindows, r.QualityBadSince = 0, 0, nil
		case bad:
			if r.BadWindows == 0 {
				r.QualityBadSince = &now
			}
			r.BadWindows, r.GoodWindows = r.BadWindows+1, 0
		default:
			r.BadWindows, r.GoodWindows, r.QualityBadSince = 0, r.GoodWindows+1, nil
		}
	}
	if freshBad && r.BadWindows >= triggerWindows(rule) && r.QualityBadSince != nil &&
		now.Sub(*r.QualityBadSince) >= time.Duration(confirmationSeconds(rule))*time.Second {
		row.Reason = fmt.Sprintf("%s，账号连续 %d 个不重叠窗口异常", r.QualityReason, r.BadWindows)
		return true, false
	}
	return false, recovering && r.GoodWindows >= recoveryWindows(rule)
}

func modelDetails(models []adminapi.QualityModelUsage) []adminapi.QualityModelUsage {
	if models == nil {
		return []adminapi.QualityModelUsage{}
	}
	return models
}

func fillQualityStatus(row *AccountStatus, r *ActionRecord, saved *accountQualityHistory) {
	if row.quality == nil && (saved == nil || len(saved.History) == 0) {
		return
	}
	status := &QualityStatus{MinInputTokens: row.MinInputTokens,
		History: []QualityWindowHistory{}, Models: []adminapi.QualityModelUsage{},
		Reason: "未到达已知窗口边界，当前窗口不可确认"}
	if saved != nil && len(saved.History) != 0 {
		status.History = saved.History
	}
	if w := row.quality; w != nil {
		status.Requests, status.FirstTokenSamples, status.CacheRatio, status.AverageFirstTokenMs =
			w.Requests, w.FirstTokenSamples, w.Ratio(), w.AverageFirstTokenMs
		status.MinInputTokens, status.ExcludedSmallRequests, status.ExcludedUnknownRequests =
			w.MinInputTokens, w.ExcludedSmallRequests, w.ExcludedUnknownRequests
		status.WindowKnown, status.Models = w.WindowKnown, modelDetails(w.Models)
		if w.WindowKnown {
			status.Reason = "等待账号完整质量窗口或确认时间"
		}
	}
	if r != nil {
		status.BadWindows, status.GoodWindows, status.LastEvaluatedAt = r.BadWindows, r.GoodWindows, r.QualityEvaluatedAt
		if status.WindowKnown && r.QualityReason != "" {
			status.Reason = r.QualityReason
		}
	}
	row.QualitySampling = status
}
