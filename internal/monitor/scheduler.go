package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"sub2api.local/cache-manager/internal/adminapi"
	"sub2api.local/cache-manager/internal/config"
)

var callGroups = []string{"excellent", "good", "general", "reserve"}

type managedAccount struct {
	account                               adminapi.Account
	row                                   AccountStatus
	setting                               config.AccountSetting
	snapshot                              *adminapi.RateSnapshot
	selected, readOK, eligible, restoring bool
	managed                               bool
	record                                *ActionRecord
	priority                              int
	schedulable                           bool
	candidate                             bool
	history                               *accountQualityHistory
	qualityWindows                        []adminapi.QualityWindow
	priorQualityWindows                   []adminapi.QualityWindow
	priorQualityPending                   bool
}

func groupRank(group string) int {
	for i, known := range callGroups {
		if group == known {
			return i
		}
	}
	return len(callGroups)
}

// A recovery candidate keeps its configured priority. The stable group stays
// open until the candidate has proved healthy using fresh traffic.
func (e *Engine) schedule(ctx context.Context, api API, store Store, ledger *Ledger, c config.Config,
	items []*managedAccount, unknownPlatforms map[string]bool, now time.Time) error {
	winners := map[string]int{}
	candidates := map[string]*managedAccount{}
	for _, item := range items {
		a, row := item.account, &item.row
		key := strconv.FormatInt(a.ID, 10)
		r := ledger.Records[key]
		item.record = r
		if !item.readOK {
			continue
		}
		if a.Type != "apikey" {
			if r != nil {
				row.Error = "非 API Key 账号有旧记录，请人工核对，插件不会写入"
			}
			continue
		}
		if r != nil {
			migrateQualityWindow(r)
			if r.Action != "schedule" {
				item.restoring = true
				if r.Phase == "pending_restore" && r.PendingField == "" && matchesOriginal(a, r) {
					delete(ledger.Records, key)
					item.record = nil
					continue
				}
				if r.Action == "pause" {
					r.TargetPriority, r.TargetSchedulable = r.OriginalPriority, false
				} else {
					r.TargetSchedulable = r.OriginalSchedulable
				}
				r.Action, r.Platform = "schedule", a.Platform
			}
			if r.Phase != "manual_change" && !reconcileOwned(a, r) {
				r.Phase = "manual_change"
				logEvent(ledger, a.ID, a.Name, "manual_change", "检测到人工修改或未确认的写入，保留当前值并停止接管", now)
			}
			if r.Phase == "manual_change" {
				if c.AutomationEnabled {
					row.State, row.Reason = "manual_change", "检测到人工操作，保留当前调度与优先级，不再自动修改"
					if !a.Schedulable && r.TargetSchedulable {
						row.Reason = "运行监控后检测到宿主将账号设为不可调度；保持调度关闭"
					} else if a.Schedulable && !r.TargetSchedulable {
						row.Reason = "运行监控后检测到人工开启调度；保持调度开启"
					}
					if item.selected && a.Schedulable && a.Active(now) && groupRank(item.setting.Group) < len(callGroups) {
						selectWinner(winners, a.Platform, groupRank(item.setting.Group))
					}
				}
				if !item.selected {
					delete(ledger.Records, key)
					item.record = nil
				}
				continue
			}
		}
		if !c.AutomationEnabled || !item.selected || item.restoring {
			if r != nil {
				item.priority, item.schedulable = r.OriginalPriority, r.OriginalSchedulable
				item.restoring = true
				row.State, row.Reason = "recovering", "恢复插件接管前的设置"
			}
			continue
		}
		rule := config.NormalizeRule(c.GroupRule(a.Platform, item.setting.Group))
		if r == nil {
			r = &ActionRecord{AccountID: a.ID, AdminURL: c.AdminURL, Executor: c.ExecutorHostname, Action: "schedule", Phase: "active",
				OriginalPriority: a.Priority, TargetPriority: a.Priority, OriginalSchedulable: a.Schedulable, TargetSchedulable: a.Schedulable,
				OriginalStatus: a.Status, Platform: a.Platform, Group: item.setting.Group, Policy: rule, AppliedAt: now, StatusSince: now,
				QualityScope: accountQualityScope}
			ledger.Records[key], item.record = r, r
		}
		if retry := r.QualityHistoryRetry; retry != nil && item.history != nil &&
			!sampleAfter(retry.Through, item.history.LastSample) && item.history.LastSample != "" {
			r.QualityHistoryRetry = nil
		}
		if r.Group != item.setting.Group || r.Platform != a.Platform || policyDiffersForHealth(r.Policy, rule) {
			resetHealth(r)
			r.SampleSince = &now
		}
		r.Group, r.Platform, r.Policy = item.setting.Group, a.Platform, rule
		item.priority = c.GroupPriority(a.Platform, item.setting.Group)
		if item.setting.Group == "temporary" {
			row.State, row.Reason = "temporary", "临时组不参与调用，优先级固定为 99"
			item.managed = true
			continue
		}
		if item.setting.DesiredSchedulable != nil && !*item.setting.DesiredSchedulable {
			row.State, row.Reason = "manually_disabled", "已在当前调用组中设置停止调用"
			item.managed = true
			continue
		}
		if a.Status == "error" {
			if c.AutomationEnabled && r.FallbackPending && r.PendingField == "" {
				r.FallbackPending = false
				triggerFallback(ledger, item, now)
			}
			markAccountFault(item, a, now)
			continue
		}
		if a.Status != "active" || (a.ExpiresAt != nil && *a.ExpiresAt <= now.Unix()) {
			row.State, row.Reason = "unavailable", row.Availability
			item.managed = true
			continue
		}
		if r.AccountFault {
			r.AccountFault = false
			r.HoldUntil = &now
			row.clearUsage()
			logEvent(ledger, a.ID, a.Name, "account_repaired", "宿主账号已由人工修复，排队验证恢复", now)
		}
		if c.AutomationEnabled && r.FallbackPending && r.PendingField == "" && a.Schedulable && a.Active(now) {
			r.FallbackPending = false
			triggerFallback(ledger, item, now)
			startTrial(item, ledger.SamplingSince, now, rule)
			logEvent(ledger, a.ID, a.Name, "fallback", ledger.FallbackReason, now)
		}
		// Temporary host flags still gate requests at the host. A failed clear
		// must not turn off the always-on scheduling switch as well.
		if item.setting.Group == "always_on" {
			item.schedulable = true
		}
		temp := a.TemporaryUnavailableReason(now)
		newTemporaryState := temp != "" && temp != r.LastTemporaryState
		if temp != "" && (r.FailureKind != "temporary" || r.RecoveryStartedAt != nil || newTemporaryState) {
			recordFailure(r, "temporary", temp, now, rule)
			r.LastTemporaryState = temp
			if item.setting.Group == "always_on" {
				r.HoldUntil = &now
			}
		}
		if r.HoldUntil != nil && now.Before(*r.HoldUntil) {
			row.State, row.Reason, row.RecoveryAt = "paused", r.Reason, r.HoldUntil
			if r.FailureKind == "temporary" {
				row.State = "temporary_retry"
			}
			item.managed = true
			continue
		}
		if item.setting.Group == "always_on" {
			// This group is independent of the four ordinary failover groups.
			if temp != "" {
				if err := e.clearTemporary(ctx, api, store, ledger, item, now); err != nil {
					if errors.Is(err, errManagementPersistence) {
						return err
					}
					if !r.AccountFault {
						retryFailed(r, err.Error(), now, rule)
						row.State, row.Reason, row.RecoveryAt = "temporary_retry", r.Reason, r.HoldUntil
					}
					row.Error = joinError(row.Error, err.Error())
					item.managed = true
					continue
				}
				a = item.account
				r.HoldUntil, r.LastTemporaryState = nil, ""
			} else if r.HoldUntil != nil {
				r.HoldUntil = nil
			}
			if r.RecoveryTrial && r.RecoveryStartedAt != nil && now.Sub(*r.RecoveryStartedAt) >= time.Duration(recoverySeconds(rule))*time.Second && row.Complete && row.Requests == config.QualityWindowRequests {
				finishRecovery(r)
			}
			item.eligible, item.schedulable = true, true
			row.State, row.Reason = "ready", "常开组持续开启；与普通调用组独立调度"
			if r.RecoveryTrial {
				row.State, row.Reason = "recovering", "常开组已清理临时状态，保持开启并等待新的成功调用确认稳定"
			}
			continue
		}
		// Price is a scheduling policy, not evidence of a broken account.
		if row.EffectiveRate == nil || (rule.MaxRateMultiplier > 0 && *row.EffectiveRate > rule.MaxRateMultiplier) {
			row.State, row.Reason = "needs_rate", "没有有效倍率，请设置倍率"
			if row.EffectiveRate != nil {
				row.State, row.Reason = "policy_wait", fmt.Sprintf("采用倍率 %.4g× 超过本组上限 %.4g×，等待倍率恢复", *row.EffectiveRate, rule.MaxRateMultiplier)
			}
			item.managed = true
			continue
		}
		if r.HoldUntil != nil || r.RecoveryTrial || r.FailureKind != "" {
			item.candidate = true
			row.State, row.Reason = "recovery_wait", "已到恢复时间，等待前序组恢复尝试；使用原组优先级"
			// A running trial cannot reuse observations from before it opened.
			if r.RecoveryStartedAt != nil && !a.Schedulable {
				// A previous version started the clock before the open was confirmed.
				r.RecoveryStartedAt, r.RecoveryDeadline = nil, nil
			}
			if r.RecoveryStartedAt != nil {
				failed, healthy := evaluateQuality(r, item, rule, now, true)
				if failed {
					recordFailure(r, "quality", row.Reason, now, rule)
					row.State, row.Reason, row.RecoveryAt = "paused", r.Reason, r.HoldUntil
					item.candidate = false
					continue
				}
				if healthy && now.Sub(*r.RecoveryStartedAt) >= time.Duration(recoverySeconds(rule))*time.Second {
					finishRecovery(r)
					item.candidate = false
					row.State, row.Reason = "ready", "恢复观察通过，已回到原组正常调度"
					logEvent(ledger, a.ID, a.Name, "recovered", row.Reason, now)
				} else if r.RecoveryDeadline != nil && !now.Before(*r.RecoveryDeadline) {
					// No traffic or incomplete samples are not additional account faults.
					until := now.Add(retryBackoff(rule.CooldownSeconds, r.DegradationLevel))
					r.HoldUntil, r.RecoveryTrial, r.RecoveryStartedAt, r.RecoveryDeadline = &until, false, nil, nil
					r.Reason = "恢复观察期内有效健康样本不足，延后再次尝试；未增加故障次数"
					row.State, row.Reason, row.RecoveryAt = "paused", r.Reason, &until
					item.candidate = false
					continue
				}
			}
		} else {
			start := ledger.SamplingSince
			if r.SampleSince != nil {
				start = *r.SampleSince
			}
			if !now.Before(start.Add(time.Duration(confirmationSeconds(rule)) * time.Second)) {
				failed, _ := evaluateQuality(r, item, rule, now, false)
				if failed {
					recordFailure(r, "quality", row.Reason, now, rule)
					row.State, row.Reason, row.RecoveryAt = "paused", r.Reason, r.HoldUntil
					item.managed = true
					continue
				}
			} else {
				// Persist the boundary even if saving diagnostic history fails.
				for _, window := range item.qualityWindows {
					if freshQualityWindow(window, latestSample(r.QualityLegacyAnchor, r.LastQualityWindow)) {
						r.LastQualityWindow = window.LastSample
					}
				}
			}
			row.State, row.Reason = "sampling", fmt.Sprintf("等待账号 %d 次有效调用组成完整窗口", config.QualityWindowRequests)
			if r.BadWindows > 0 {
				row.State, row.Reason = "pending", fmt.Sprintf("质量异常待确认：%d / %d 个独立窗口；%s", r.BadWindows, triggerWindows(rule), r.QualityReason)
			} else if r.GoodWindows > 0 {
				row.State, row.Reason = "ready", "质量窗口健康，可参与本组调度"
			}
		}
		item.eligible = true
		if item.candidate {
			old := candidates[a.Platform]
			if old == nil || (r.RecoveryStartedAt != nil && old.record.RecoveryStartedAt == nil) || (r.RecoveryStartedAt == nil && old.record.RecoveryStartedAt == nil && (groupRank(item.setting.Group) < groupRank(old.setting.Group) || (item.setting.Group == old.setting.Group && a.ID < old.account.ID))) {
				candidates[a.Platform] = item
			}
		} else {
			selectWinner(winners, a.Platform, groupRank(item.setting.Group))
		}
	}
	var fallback *managedAccount
	if c.AutomationEnabled && !ledger.FallbackTriggered {
		fallback = fallbackCandidate(items, winners, candidates, unknownPlatforms, now)
	}
	if fallback != nil {
		fallback.candidate, fallback.eligible = true, true
		candidates[fallback.account.Platform] = fallback
	}
	exclusive := map[string]*managedAccount{}
	for _, item := range items {
		if !item.eligible || item.candidate || item.setting.Group != "excellent" {
			continue
		}
		rule := c.GroupRule(item.account.Platform, "excellent")
		if rule.ExclusiveRateBelow <= 0 || item.row.EffectiveRate == nil || *item.row.EffectiveRate >= rule.ExclusiveRateBelow {
			continue
		}
		if reason := exclusiveExitReason(rule, item.row); reason != "" {
			item.row.Reason = joinError(item.row.Reason, reason)
			continue
		}
		old := exclusive[item.account.Platform]
		if old == nil || *item.row.EffectiveRate < *old.row.EffectiveRate || (*item.row.EffectiveRate == *old.row.EffectiveRate && item.row.ID < old.row.ID) {
			exclusive[item.account.Platform] = item
		}
	}
	for _, item := range items {
		if !item.eligible || item.setting.Group == "always_on" {
			continue
		}
		if unknownPlatforms[item.account.Platform] || unknownPlatforms["*"] {
			item.schedulable = item.account.Schedulable
			item.row.State, item.row.Reason = "group_wait", "本平台有账号信息读取失败，保留现有调度，暂不开放新的调用组"
			continue
		}
		winner, hasWinner := winners[item.account.Platform]
		if item.candidate {
			if candidates[item.account.Platform] != item || (hasWinner && groupRank(item.setting.Group) > winner) {
				continue
			}
			// Configuration may deliberately override priorities. Do not silently make
			// a recovery candidate equal to (or lower than) a later fallback group.
			if hasWinner && groupRank(item.setting.Group) < winner && item.priority >= c.GroupPriority(item.account.Platform, callGroups[winner]) {
				item.row.Reason = "原组优先级未高于后备组，请停止监控后调整组优先级再恢复"
				continue
			}
			if item == fallback {
				item.schedulable = true
				continue
			}
			if item.record.RecoveryStartedAt == nil {
				if item.account.TemporaryUnavailableReason(now) != "" {
					if err := e.clearTemporary(ctx, api, store, ledger, item, now); err != nil {
						if errors.Is(err, errManagementPersistence) {
							return err
						}
						if !item.record.AccountFault {
							retryFailed(item.record, err.Error(), now, item.record.Policy)
							item.row.State, item.row.Reason, item.row.RecoveryAt = "temporary_retry", item.record.Reason, item.record.HoldUntil
						}
						item.row.Error = joinError(item.row.Error, err.Error())
						continue
					}
					item.record.HoldUntil, item.record.LastTemporaryState = nil, ""
				}
			}
			item.schedulable = true
			if item.record.RecoveryStartedAt != nil {
				setRecoveryStatus(item)
			}
			continue
		}
		if !hasWinner || groupRank(item.setting.Group) != winner {
			item.row.State, item.row.Reason = "group_wait", "等待前序调用组全部不可用"
			continue
		}
		if chosen := exclusive[item.account.Platform]; item.setting.Group == "excellent" && chosen != nil && chosen != item {
			item.row.State, item.row.Reason = "exclusive_wait", "优秀组低倍率账号独占调用"
			continue
		}
		item.schedulable = true
	}
	for _, item := range items {
		updateRecordStatus(ledger, item, now)
	}
	if err := save(ctx, store, ledger); err != nil {
		return err
	}
	ordered := append([]*managedAccount{}, items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].schedulable && !ordered[j].schedulable
	})
	blockedTransitions := map[string]bool{}
	for _, item := range ordered {
		r := item.record
		if !item.readOK || item.account.Type != "apikey" || r == nil || r.Phase == "manual_change" {
			continue
		}
		if !item.restoring && r.AccountFault && item.account.Status == "error" {
			continue // A confirmed host error already prevents routing.
		}
		if !item.restoring && !item.schedulable && item.account.Schedulable && blockedTransitions[item.account.Platform] {
			item.row.State, item.row.Reason = "group_wait", "后备账号开启未确认，暂时保留当前调度"
			updateRecordStatus(ledger, item, now)
			continue
		}
		if err := e.syncSchedule(ctx, api, store, ledger, item, item == fallback, now); err != nil {
			if errors.Is(err, errManagementPersistence) {
				return err
			}
			item.row.Error = joinError(item.row.Error, err.Error())
			if item.schedulable && !item.restoring && item.setting.Group != "always_on" {
				blockedTransitions[item.account.Platform] = true
			}
			updateRecordStatus(ledger, item, now)
			if item == fallback {
				return err
			}
			continue
		}
		if item.restoring {
			delete(ledger.Records, strconv.FormatInt(item.row.ID, 10))
			logEvent(ledger, item.row.ID, item.row.Name, "restored", "已恢复插件接管前的优先级和调度状态", now)
			item.row.State, item.row.Reason = "observing", "原设置已恢复"
			clearHealthDetails(&item.row)
		} else {
			updateRecordStatus(ledger, item, now)
		}
		if c.AutomationEnabled && !item.restoring && item.schedulable && item.account.Schedulable &&
			item.record.FailureKind != "" && item.record.RecoveryStartedAt == nil &&
			(item == fallback || item.record.HoldUntil == nil || !now.Before(*item.record.HoldUntil)) {
			rowReason := "开始按原组优先级恢复试运行，后备组保持开启"
			if item == fallback {
				triggerFallback(ledger, item, now)
				rowReason = ledger.FallbackReason
			} else {
				item.record.RecoveryAttempts++
				item.record.LastRecoveryAttempt = now
			}
			item.record.FallbackPending = false
			startTrial(item, ledger.SamplingSince, now, item.record.Policy)
			logEvent(ledger, item.row.ID, item.row.Name, "recovery_trial", rowReason, now)
			setRecoveryStatus(item)
			updateRecordStatus(ledger, item, now)
		}
		if err := save(ctx, store, ledger); err != nil {
			return err
		}
	}
	if c.AutomationEnabled {
		for _, item := range items {
			if r := item.record; r != nil && item.row.quality != nil && item.row.quality.WindowKnown {
				// Keep observational windows even during warmup and in the always-on group.
				for _, window := range item.qualityWindows {
					appendFirstTokenWindow(r, window)
				}
			}
		}
	}
	return save(ctx, store, ledger)
}

func markAccountFault(item *managedAccount, a adminapi.Account, now time.Time) {
	r := item.record
	if !r.AccountFault {
		recordFailure(r, "account", "宿主账号 error，需要人工处理凭证或账号配置", now, r.Policy)
	}
	r.AccountFault, r.HoldUntil = true, nil
	item.account, item.schedulable, item.managed = a, false, true
	item.row.Status, item.row.Priority, item.row.Schedulable = a.Status, a.Priority, a.Schedulable
	item.row.Availability = a.AvailabilityReason(now)
	item.row.State, item.row.Reason = "account_error", "宿主账号 error，需要人工处理凭证或账号配置；插件不会自动清除此状态"
}

func selectWinner(winners map[string]int, platform string, rank int) {
	if old, ok := winners[platform]; !ok || rank < old {
		winners[platform] = rank
	}
}

const frequentSwitchThreshold = 3

func reserveAccountFailed(item *managedAccount) bool {
	if item.setting.Group != "reserve" || !item.readOK || !item.selected || item.account.Type != "apikey" ||
		item.restoring || (item.setting.DesiredSchedulable != nil && !*item.setting.DesiredSchedulable) ||
		(item.record != nil && item.record.Phase == "manual_change") {
		return false
	}
	if item.record != nil && item.record.AccountFault {
		return true
	}
	switch item.row.State {
	case "account_error", "paused", "deprioritized", "temporary_retry", "unavailable":
		return true
	default:
		return item.record != nil && item.record.FailureKind != "" && item.record.HoldUntil != nil
	}
}

func fallbackCandidate(items []*managedAccount, winners map[string]int, recoveryCandidates map[string]*managedAccount,
	unknownPlatforms map[string]bool, now time.Time) *managedAccount {
	for _, item := range items {
		if item.record == nil || !item.record.FallbackPending {
			continue
		}
		platform := item.account.Platform
		if !selectedCallAccount(item) || unknownPlatforms[platform] || unknownPlatforms["*"] ||
			item.record.AccountFault || item.row.Error != "" || !item.account.Active(now) ||
			(item.record.Policy.MaxRateMultiplier > 0 && (item.row.EffectiveRate == nil || *item.row.EffectiveRate > item.record.Policy.MaxRateMultiplier)) {
			return nil
		}
		return item
	}
	tailRanks := map[string]int{}
	for _, item := range items {
		if !selectedCallAccount(item) {
			continue
		}
		if groupRank(item.setting.Group) > tailRanks[item.account.Platform] {
			tailRanks[item.account.Platform] = groupRank(item.setting.Group)
		}
	}
	reserveFailed := map[string]bool{}
	for _, item := range items {
		if selectedCallAccount(item) && groupRank(item.setting.Group) == tailRanks[item.account.Platform] && accountFailed(item) {
			reserveFailed[item.account.Platform] = true
		}
	}
	candidates := []*managedAccount{}
	for _, item := range items {
		platform := item.account.Platform
		if !reserveFailed[platform] || unknownPlatforms[platform] || unknownPlatforms["*"] {
			continue
		}
		if _, hasWinner := winners[platform]; hasWinner {
			continue
		}
		if trial := recoveryCandidates[platform]; trial != nil && trial.record != nil && trial.record.RecoveryStartedAt != nil {
			continue // The platform already has a host-confirmed recovery trial in flight.
		}
		if !item.readOK || !item.selected || item.account.Type != "apikey" || item.restoring || item.record == nil ||
			groupRank(item.setting.Group) >= tailRanks[platform] || item.record.AccountFault || item.record.Phase == "manual_change" ||
			(item.setting.DesiredSchedulable != nil && !*item.setting.DesiredSchedulable) || !item.account.Active(now) ||
			item.account.Schedulable || item.record.RecoveryStartedAt != nil || item.row.Error != "" || item.row.EffectiveRate == nil ||
			(item.record.Policy.MaxRateMultiplier > 0 && *item.row.EffectiveRate > item.record.Policy.MaxRateMultiplier) {
			continue
		}
		if item.record.HoldUntil == nil || !now.Before(*item.record.HoldUntil) {
			continue
		}
		candidates = append(candidates, item)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if groupRank(left.setting.Group) != groupRank(right.setting.Group) {
			return groupRank(left.setting.Group) < groupRank(right.setting.Group)
		}
		if left.priority != right.priority {
			return left.priority < right.priority
		}
		return left.account.ID < right.account.ID
	})
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

func selectedCallAccount(item *managedAccount) bool {
	return item.readOK && item.selected && item.account.Type == "apikey" && !item.restoring &&
		groupRank(item.setting.Group) < len(callGroups) &&
		(item.setting.DesiredSchedulable == nil || *item.setting.DesiredSchedulable) &&
		(item.record == nil || item.record.Phase != "manual_change")
}

func accountFailed(item *managedAccount) bool {
	if item.record != nil && item.record.AccountFault {
		return true
	}
	switch item.row.State {
	case "account_error", "paused", "deprioritized", "temporary_retry", "unavailable":
		return true
	default:
		return item.record != nil && item.record.FailureKind != "" && item.record.HoldUntil != nil
	}
}

func triggerFallback(ledger *Ledger, item *managedAccount, now time.Time) {
	ledger.FallbackTriggered = true
	ledger.FallbackAt = &now
	ledger.FallbackReason = "备用组账号异常，前序账号尚未恢复，已从最前有效组发起一次托底试运行"
	item.record.RecoveryAttempts++
	item.record.LastRecoveryAttempt = now
	logEvent(ledger, item.row.ID, item.row.Name, "fallback", ledger.FallbackReason, now)
}

// Switches measure confirmed changes in the preferred ordinary account, not actual requests.
// Keep this diagnostic out of the host's bounded durable ownership ledger.
type switchTracker struct {
	session time.Time
	winners map[string]int64
	events  []time.Time
}

func (s *switchTracker) observe(session time.Time, items []*managedAccount, unknownPlatforms map[string]bool, windowMinutes int, now time.Time) int {
	if s.winners == nil || !s.session.Equal(session) {
		*s = switchTracker{session: session, winners: map[string]int64{}}
	}
	current := map[string]*managedAccount{}
	for _, item := range items {
		if item.selected && item.readOK && item.account.Type == "apikey" && groupRank(item.setting.Group) < len(callGroups) && item.row.Error != "" {
			unknownPlatforms[item.account.Platform] = true
		}
	}
	for _, item := range items {
		if !item.readOK || !item.selected || item.account.Type != "apikey" || item.restoring ||
			groupRank(item.setting.Group) >= len(callGroups) || unknownPlatforms["*"] || unknownPlatforms[item.account.Platform] ||
			!item.row.Schedulable || !item.account.Active(now) || item.row.Error != "" {
			continue
		}
		old := current[item.account.Platform]
		if old == nil || item.row.Priority < old.row.Priority ||
			(item.row.Priority == old.row.Priority && item.account.ID < old.account.ID) {
			current[item.account.Platform] = item
		}
	}
	for platform, item := range current {
		previous := s.winners[platform]
		if previous != 0 && previous != item.account.ID {
			s.events = append(s.events, now)
		}
		s.winners[platform] = item.account.ID
	}
	cutoff := now.Add(-time.Duration(max(1, windowMinutes)) * time.Minute)
	for len(s.events) > 0 && s.events[0].Before(cutoff) {
		s.events = s.events[1:]
	}
	return len(s.events)
}

func monitoringSummary(ledger *Ledger, c config.Config, items []*managedAccount, switches int, now time.Time) (string, string) {
	if ledger.FallbackTriggered {
		return "anomaly_detected", ledger.FallbackReason
	}
	callable := false
	for _, item := range items {
		if item.selected && item.record != nil && item.record.FallbackPending {
			return "unknown", "托底调度写入结果待宿主确认"
		}
		pendingWrite := item.record != nil && (item.record.PendingField != "" || item.record.Phase == "pending_apply")
		if item.selected && (!item.readOK || item.row.Error != "" || item.row.State == "account_error" || item.row.State == "manual_change" || pendingWrite) {
			return "unknown", "部分账号采集或调度未确认"
		}
		if item.selected && item.account.Type == "apikey" && item.setting.Group != "temporary" &&
			item.row.Schedulable && item.account.Active(now) {
			callable = true
		}
	}
	if !callable {
		return "unavailable", "没有确认可调用的账号"
	}
	if switches >= frequentSwitchThreshold {
		return "frequent_switch", fmt.Sprintf("最近 %d 分钟主调度账号切换 %d 次", max(1, c.WindowMinutes), switches)
	}
	return "stable", fmt.Sprintf("最近 %d 分钟主调度账号切换 %d 次", max(1, c.WindowMinutes), switches)
}

func triggerWindows(r config.Rule) int  { return max(2, r.TriggerSamples) }
func recoveryWindows(r config.Rule) int { return max(2, r.RecoverySamples) }
func confirmationSeconds(r config.Rule) int {
	if r.ConfirmationSeconds < 60 {
		return 180
	}
	return r.ConfirmationSeconds
}
func recoverySeconds(r config.Rule) int {
	if r.RecoverySeconds < 60 {
		return 300
	}
	return r.RecoverySeconds
}
func retryBackoff(baseSeconds, level int) time.Duration {
	delay := time.Duration(max(30, baseSeconds)) * time.Second
	for i := 1; i < level && delay < 24*time.Hour; i++ {
		delay *= 2
	}
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}
func recordFailure(r *ActionRecord, kind, reason string, now time.Time, rule config.Rule) {
	if r.RecoveryStartedAt != nil {
		r.RecoveryFailures++
	}
	r.FailureCount++
	if kind == "temporary" {
		r.TemporaryFailures++
	}
	r.DegradationLevel = min(16, r.DegradationLevel+1)
	until := now.Add(retryBackoff(rule.CooldownSeconds, r.DegradationLevel))
	r.FailureKind, r.Reason, r.LastFailureAt, r.HoldUntil = kind, reason, &now, &until
	r.RecoveryTrial, r.RecoveryStartedAt, r.RecoveryDeadline = false, nil, nil
	r.GoodWindows = 0
}
func retryFailed(r *ActionRecord, reason string, now time.Time, rule config.Rule) {
	r.RecoveryFailures++
	if r.LastFailureAt == nil || !r.LastFailureAt.Equal(now) {
		r.DegradationLevel = min(16, r.DegradationLevel+1)
	}
	until := now.Add(retryBackoff(rule.CooldownSeconds, r.DegradationLevel))
	r.HoldUntil, r.RecoveryTrial, r.RecoveryStartedAt, r.RecoveryDeadline = &until, false, nil, nil
	r.Reason = "临时状态恢复未确认：" + reason
}
func resetHealth(r *ActionRecord) {
	r.HoldUntil, r.SampleSince, r.LastFailureAt, r.RecoveryStartedAt, r.RecoveryDeadline = nil, nil, nil, nil, nil
	r.DegradationLevel = 0
	r.RecoveryTrial, r.AccountFault = false, false
	resetQualityProgress(r)
	r.QualityTracks, r.QualityLegacyAnchor = nil, ""
	r.QualityHistoryRetry = nil
	r.FailureKind, r.Reason, r.LastBadSample, r.LastTemporaryState = "", "", "", ""
	r.FailureCount, r.TemporaryFailures, r.RecoveryAttempts, r.RecoveryFailures = 0, 0, 0, 0
	r.LastRecoveryAttempt = time.Time{}
	r.NextRetryAt = nil
}
func startTrial(item *managedAccount, since, now time.Time, rule config.Rule) {
	r := item.record
	rememberPriorQualityHistory(r, item, since)
	r.HoldUntil, r.SampleSince, r.RecoveryStartedAt = nil, &now, &now
	r.LastTemporaryState = ""
	deadline := now.Add(time.Duration(max(600, recoverySeconds(rule)*3)) * time.Second)
	r.RecoveryDeadline, r.RecoveryTrial = &deadline, true
	cursor := r.LastQualityWindow
	resetQualityProgress(r)
	r.LastQualityWindow = cursor
	item.row.clearUsage()
}
func setRecoveryStatus(item *managedAccount) {
	item.row.State = "recovering"
	item.row.Reason = fmt.Sprintf("原组优先级 %d 恢复试运行；健康窗口 %d / %d；后备组保持开启",
		item.priority, item.record.GoodWindows, recoveryWindows(item.record.Policy))
}
func finishRecovery(r *ActionRecord) {
	r.FailureKind, r.Reason = "", ""
	r.HoldUntil, r.RecoveryStartedAt, r.RecoveryDeadline = nil, nil, nil
	r.RecoveryTrial, r.DegradationLevel = false, 0
}
func updateRecordStatus(ledger *Ledger, item *managedAccount, now time.Time) {
	r := item.record
	if r == nil || item.row.State == "" {
		return
	}
	if r.LastState != item.row.State {
		r.LastState, r.StatusSince = item.row.State, now
		logEvent(ledger, item.row.ID, item.row.Name, item.row.State, item.row.Reason, now)
	}
	fillRecordDetails(&item.row, r)
}
func fillRecordDetails(row *AccountStatus, r *ActionRecord) {
	row.StatusSince = &r.StatusSince
	row.QualityBadWindows, row.QualityGoodWindows = r.BadWindows, r.GoodWindows
	row.FailureKind, row.FailureCount = r.FailureKind, r.FailureCount
	row.TemporaryFailures, row.RecoveryAttempts, row.RecoveryFailures = r.TemporaryFailures, r.RecoveryAttempts, r.RecoveryFailures
	row.LastFailureAt, row.NextRetryAt = r.LastFailureAt, r.HoldUntil
	row.RecoveryAt = r.HoldUntil
	if !r.LastRecoveryAttempt.IsZero() {
		at := r.LastRecoveryAttempt
		row.LastRecoveryAttemptAt = &at
	}
	row.DegradationLevel, row.RecoveryTrial = r.DegradationLevel, r.RecoveryTrial
	row.RecoveryStartedAt, row.RecoveryDeadline = r.RecoveryStartedAt, r.RecoveryDeadline
	row.ErrorMetricsAvailable, row.ErrorRequests = false, nil
}
func clearHealthDetails(row *AccountStatus) {
	row.StatusSince, row.LastFailureAt, row.LastRecoveryAttemptAt, row.NextRetryAt, row.RecoveryAt = nil, nil, nil, nil, nil
	row.FailureKind, row.DegradedTo = "", ""
	row.FailureCount, row.TemporaryFailures, row.RecoveryAttempts, row.RecoveryFailures, row.DegradationLevel = 0, 0, 0, 0, 0
	row.QualityBadWindows, row.QualityGoodWindows = 0, 0
	row.RecoveryTrial, row.RecoveryStartedAt, row.RecoveryDeadline = false, nil, nil
}

// Persist the attempt and its next eligible retry before clearing host runtime
// state. A crash or an uncertain HTTP result never creates a tight reset loop.
func (e *Engine) clearTemporary(ctx context.Context, api API, store Store, ledger *Ledger, item *managedAccount, now time.Time) error {
	r := item.record
	fresh, err := api.Account(ctx, item.row.ID)
	if err != nil {
		return err
	}
	if !reconcileOwned(fresh, r) {
		r.Phase = "manual_change"
		item.row.State, item.row.Reason = "manual_change", "恢复提交前检测到人工修改，已停止接管"
		return errors.New("账号设置已变化，取消恢复")
	}
	if fresh.Status == "error" {
		markAccountFault(item, fresh, now)
		return errors.New("账号已进入 error，取消临时状态清理")
	}
	if fresh.Status != "active" || (fresh.ExpiresAt != nil && *fresh.ExpiresAt <= now.Unix()) {
		return errors.New("账号状态需要人工处理，取消临时状态清理")
	}
	if fresh.TemporaryUnavailableReason(now) == "" {
		item.account = fresh
		return nil
	}
	r.RecoveryAttempts++
	r.LastRecoveryAttempt = now
	next := now.Add(retryBackoff(r.Policy.CooldownSeconds, r.DegradationLevel))
	r.HoldUntil = &next
	if err := save(ctx, store, ledger); err != nil {
		return err
	}
	if err := api.ClearError(ctx, item.row.ID); err != nil {
		return err
	}
	confirmed, err := api.Account(ctx, item.row.ID)
	if err != nil {
		return err
	}
	if !reconcileOwned(confirmed, r) {
		r.Phase = "manual_change"
		item.row.State, item.row.Reason = "manual_change", "清理后检测到人工修改，停止接管"
		return errors.New("账号设置变化，恢复未接管")
	}
	if confirmed.Status == "error" {
		markAccountFault(item, confirmed, now)
		return errors.New("账号已进入 error，停止自动恢复")
	}
	if !confirmed.Active(now) {
		return errors.New("宿主仍有限流、过载或账号异常，恢复尚未确认")
	}
	item.account = confirmed
	item.row.Status, item.row.Priority, item.row.Schedulable = confirmed.Status, confirmed.Priority, confirmed.Schedulable
	item.row.Availability = ""
	return nil
}

func exclusiveExitReason(rule config.Rule, row AccountStatus) string {
	if rule.ExclusiveMinCacheRatio > 0 && row.Complete && row.Requests >= config.QualityWindowRequests &&
		row.CacheRatio != nil && *row.CacheRatio < rule.ExclusiveMinCacheRatio {
		return fmt.Sprintf("平均缓存命中比例 %.1f%% 低于独占下限 %.1f%%，退出独占并按本组规则调用", *row.CacheRatio*100, rule.ExclusiveMinCacheRatio*100)
	}
	if rule.ExclusiveMaxAverageFirstTokenMs > 0 && row.FirstTokenPeriodAverage != nil && row.FirstTokenWindows >= firstTokenPeriods(rule) &&
		*row.FirstTokenPeriodAverage > rule.ExclusiveMaxAverageFirstTokenMs {
		return fmt.Sprintf("最近 %d 个首字时间周期均值 %.0f ms 超过独占上限 %.0f ms，退出独占并按本组规则调用", firstTokenPeriods(rule), *row.FirstTokenPeriodAverage, rule.ExclusiveMaxAverageFirstTokenMs)
	}
	return ""
}

func matchesOriginal(a adminapi.Account, r *ActionRecord) bool {
	return a.Priority == r.OriginalPriority && a.Schedulable == r.OriginalSchedulable
}

func reconcileOwned(a adminapi.Account, r *ActionRecord) bool {
	if a.ID != r.AccountID || a.Type != "apikey" || a.Platform != r.Platform ||
		(a.Status != r.OriginalStatus && a.Status != "active" && a.Status != "error") {
		return false
	}
	// Host SetError owns the accompanying closure. Accept it at every readback,
	// and never restore the old open switch over that host-owned state.
	if a.Status == "error" && !a.Schedulable && (a.Priority == r.TargetPriority ||
		(r.PendingField == "priority" && a.Priority == r.PendingPriority)) {
		r.TargetSchedulable, r.OriginalSchedulable = false, false
		if r.PendingField != "priority" || a.Priority == r.PendingPriority {
			r.TargetPriority, r.PendingField, r.Phase = a.Priority, "", "active"
		}
		return true
	}
	switch r.PendingField {
	case "priority":
		if a.Priority == r.PendingPriority && a.Schedulable == r.TargetSchedulable {
			r.TargetPriority, r.PendingField, r.Phase = r.PendingPriority, "", "active"
			return true
		}
	case "schedulable":
		if a.Priority == r.TargetPriority && a.Schedulable == r.PendingSchedulable {
			r.TargetSchedulable, r.PendingField, r.Phase = r.PendingSchedulable, "", "active"
			return true
		}
	}
	if a.Priority == r.TargetPriority && a.Schedulable == r.TargetSchedulable {
		return true
	}
	return false
}

func (e *Engine) syncSchedule(ctx context.Context, api API, store Store, ledger *Ledger, item *managedAccount, fallback bool, now time.Time) error {
	r := item.record
	fields := []string{"priority", "schedulable"}
	if !item.schedulable {
		fields = []string{"schedulable", "priority"}
	}
	if r.PendingField != "" {
		desiredPriority, desiredSchedulable := item.priority, item.schedulable
		if item.restoring {
			desiredPriority, desiredSchedulable = r.OriginalPriority, r.OriginalSchedulable
		}
		matchesPending := (r.PendingField == "priority" && r.PendingPriority == desiredPriority) ||
			(r.PendingField == "schedulable" && r.PendingSchedulable == desiredSchedulable)
		if !matchesPending {
			return errors.New("上次管理写入尚未确认，等待宿主状态同步后再操作")
		}
		fields = []string{r.PendingField}
	}
	for _, field := range fields {
		desiredPriority, desiredSchedulable := item.priority, item.schedulable
		if item.restoring {
			desiredPriority, desiredSchedulable = r.OriginalPriority, r.OriginalSchedulable
		}
		if field == "priority" && r.TargetPriority == desiredPriority || field == "schedulable" && r.TargetSchedulable == desiredSchedulable {
			continue
		}
		if ctx.Err() != nil {
			return errors.New("配置已变化，管理动作已取消")
		}
		fresh, err := api.Account(ctx, item.row.ID)
		if err != nil {
			return err
		}
		if !reconcileOwned(fresh, r) {
			r.Phase, item.row.State = "manual_change", "manual_change"
			item.row.Reason = "提交前账号设置发生变化，未覆盖人工修改"
			item.row.Priority, item.row.Schedulable = fresh.Priority, fresh.Schedulable
			logEvent(ledger, item.row.ID, item.row.Name, "manual_change", "提交前账号设置发生变化，未覆盖人工修改", e.clock().UTC())
			if err := save(ctx, store, ledger); err != nil {
				return err
			}
			return errors.New("账号已变化，已停止接管")
		}
		if fresh.Status == "error" && !item.restoring {
			markAccountFault(item, fresh, e.clock().UTC())
			if err := save(ctx, store, ledger); err != nil {
				return err
			}
			return errors.New("账号已进入 error，取消本次调度写入")
		}
		if item.restoring {
			desiredSchedulable = r.OriginalSchedulable
			if field == "schedulable" && r.TargetSchedulable == desiredSchedulable {
				continue
			}
		}
		if field == "schedulable" && item.schedulable && !item.restoring {
			checked := item.row
			applyRateChoice(&checked, item.setting, item.snapshot, checked.RateMode != "waiting", e.clock().UTC())
			available := fresh.Active(e.clock().UTC())
			if item.setting.Group == "always_on" {
				available = fresh.Status == "active" && (fresh.ExpiresAt == nil || *fresh.ExpiresAt > e.clock().UTC().Unix())
			}
			if !available || (item.setting.Group != "always_on" && checked.EffectiveRate == nil) {
				return errors.New("提交前账号或倍率已不可用，取消开放调度")
			}
			if item.setting.Group != "always_on" && r.Policy.MaxRateMultiplier > 0 && *checked.EffectiveRate > r.Policy.MaxRateMultiplier {
				return errors.New("提交前倍率已超过本组上限，取消开放调度")
			}
			if fallback {
				r.FallbackPending = true
			}
		}
		r.PendingField, r.PendingPriority, r.PendingSchedulable = field, desiredPriority, desiredSchedulable
		if err := save(ctx, store, ledger); err != nil {
			return err
		}
		if field == "priority" {
			err = api.SetPriority(ctx, item.row.ID, desiredPriority)
		} else {
			err = api.SetSchedulable(ctx, item.row.ID, desiredSchedulable)
		}
		if err != nil {
			return err
		} // Keep durable intent; never blindly replay an uncertain write.
		confirmed, err := api.Account(ctx, item.row.ID)
		if err != nil {
			return err
		}
		item.account = confirmed
		if !reconcileOwned(confirmed, r) {
			r.Phase, item.row.State = "manual_change", "manual_change"
			item.row.Reason = "管理写入未确认，请人工核对账号设置"
			item.row.Priority, item.row.Schedulable = confirmed.Priority, confirmed.Schedulable
			if err := save(ctx, store, ledger); err != nil {
				return err
			}
			return errors.New("管理写入未确认，保留记录等待人工核对")
		}
		if confirmed.Status == "error" && !item.restoring {
			markAccountFault(item, confirmed, e.clock().UTC())
			if err := save(ctx, store, ledger); err != nil {
				return err
			}
			return errors.New("账号已进入 error，停止后续调度写入")
		}
		if r.PendingField != "" {
			// A successful HTTP response can still precede an unapplied write.
			// Keep the durable intent and do not open the next pool yet.
			return errors.New("管理写入尚未反映到账号，保留记录等待下次核对")
		}
		item.row.Priority, item.row.Schedulable = r.TargetPriority, r.TargetSchedulable
		if field == "priority" {
			logEvent(ledger, item.row.ID, item.row.Name, "priority", fmt.Sprintf("优先级已同步为 %d", r.TargetPriority), e.clock().UTC())
		}
		if err := save(ctx, store, ledger); err != nil {
			return err
		}
	}
	item.row.Priority, item.row.Schedulable = r.TargetPriority, r.TargetSchedulable
	if item.managed {
		// Keep the synced group priority on accounts that are not yet eligible.
		if r != nil {
			item.row.Priority = r.TargetPriority
		}
		return nil
	}
	if item.restoring {
		item.row.Priority, item.row.Schedulable = r.OriginalPriority, r.OriginalSchedulable
	}
	return nil
}

// Missing metrics are unknown, not zero. Each condition can independently
// trigger a hold; an unavailable cache metric does not hide a known slow TTFT.
func evaluate(rule config.Rule, row AccountStatus) (bad, known bool, reason, sample string) {
	known = true
	if rule.MaxRateMultiplier > 0 {
		if row.EffectiveRate == nil {
			known = false
		} else if *row.EffectiveRate > rule.MaxRateMultiplier {
			return true, true, fmt.Sprintf("当前采用倍率 %.4g× 超过本组上限 %.4g×", *row.EffectiveRate, rule.MaxRateMultiplier), "rate:" + row.SampleKey
		}
	}
	if rule.MinCacheRatio > 0 {
		if !row.Complete || row.CacheRatio == nil || row.Requests < config.QualityWindowRequests {
			known = false
		} else if *row.CacheRatio < rule.MinCacheRatio {
			return true, true, fmt.Sprintf("平均缓存命中比例 %.1f%% 低于本组下限 %.1f%%", *row.CacheRatio*100, rule.MinCacheRatio*100), "cache:" + row.SampleKey
		}
	}
	if rule.MaxAverageFirstTokenMs > 0 {
		average := row.FirstTokenPeriodAverage
		if average == nil {
			average = row.AverageFirstTokenMs
		}
		if average == nil || row.FirstTokenWindows < firstTokenPeriods(rule) {
			known = false
		} else if *average > rule.MaxAverageFirstTokenMs {
			return true, true, fmt.Sprintf("最近 %d 个首字时间周期均值 %.0f ms 超过本组上限 %.0f ms", firstTokenPeriods(rule), *average, rule.MaxAverageFirstTokenMs), "ttft:" + row.FirstTokenSampleKey
		}
	}
	if !known {
		if rule.MaxRateMultiplier > 0 && row.EffectiveRate == nil {
			return false, false, "有效倍率未知，等待上游倍率或设置固定倍率", ""
		}
		return false, false, "有效样本不足或采集不完整，等待监控数据", ""
	}
	return false, true, "本组异常条件未触发", ""
}
