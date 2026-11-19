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
}

func groupRank(group string) int {
	for i, known := range callGroups {
		if group == known {
			return i
		}
	}
	return len(callGroups)
}

// schedule closes non-selected pools before opening the winning pool. Priority
// alone cannot enforce strict failover, so later pools are explicitly paused.
func (e *Engine) schedule(ctx context.Context, api API, store Store, ledger *Ledger, c config.Config,
	items []*managedAccount, unknownSelected bool, now time.Time) error {
	winners := map[string]int{}
	for _, item := range items {
		a, row := item.account, &item.row
		key := strconv.FormatInt(row.ID, 10)
		r := ledger.Records[key]
		item.record = r
		if !item.readOK {
			continue
		}
		if a.Type != "apikey" {
			if r != nil {
				row.Error = "此非 API Key 账号存在旧管理记录；请在宿主人工核对并恢复，插件不会写入此账号"
			}
			continue
		}
		if r != nil {
			if r.Action != "schedule" {
				item.restoring = true
				// Migrate the old single-action ledger to the same guarded writer.
				if r.Phase == "pending_restore" && matchesOriginal(a, r) {
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
					row.State, row.Reason = "manual_change", "检测到人工操作；已进入手动操作组，插件保留当前调度与优先级，不再自动修改"
					if !a.Schedulable && r.TargetSchedulable {
						row.Reason = "运行监控后检测到宿主将账号设为不可调度；已进入手动操作组，保持调度关闭"
					} else if a.Schedulable && !r.TargetSchedulable {
						row.Reason = "运行监控后检测到人工开启调度；已进入手动操作组，保持调度开启"
					}
				}
				// A manually changed account can still be serving traffic. It
				// must continue to block later groups without being overwritten.
				if c.AutomationEnabled && item.selected && a.Schedulable && a.Active(now) && item.setting.Group != "temporary" {
					rank := groupRank(item.setting.Group)
					if winner, ok := winners[a.Platform]; !ok || rank < winner {
						winners[a.Platform] = rank
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
		// Capture the initial scheduling settings before applying group policy.
		// Host health remains a gate; the initial scheduling switch is restored
		// when management stops, rather than excluding the account forever.
		if r == nil {
			r = &ActionRecord{AccountID: a.ID, AdminURL: c.AdminURL, Executor: c.ExecutorHostname, Action: "schedule", Phase: "active",
				OriginalPriority: a.Priority, TargetPriority: a.Priority, OriginalSchedulable: a.Schedulable, TargetSchedulable: a.Schedulable,
				OriginalStatus: a.Status, Platform: a.Platform, Group: item.setting.Group, AppliedAt: now}
			ledger.Records[key], item.record = r, r
		}
		hostAvailable := a.Active(now)
		rule := c.GroupRule(a.Platform, item.setting.Group)
		if r.Group != item.setting.Group || r.Platform != a.Platform || r.Policy != rule {
			r.HoldUntil, r.LastBadSample, r.Reason = nil, "", ""
			e.counters[a.ID] = counter{}
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
		if !hostAvailable {
			reason := row.Availability
			if reason == "" {
				reason = "宿主账号当前不可调用"
			}
			row.State, row.Reason = "unavailable", reason
			item.managed = true
			continue
		}
		if row.EffectiveRate == nil {
			row.State, row.Reason = "needs_rate", "没有有效倍率，请设置固定倍率"
			if row.RateMode == "waiting" {
				row.Reason = "等待获取上游倍率；也可设置固定倍率后参与调用"
			}
			// Keep an account with unknown rate closed. Previously it could
			// retain an open scheduler state while later groups waited for it,
			// leaving no callable pool.
			item.schedulable = false
			item.managed = true
			continue
		}
		if r.HoldUntil != nil && now.Before(*r.HoldUntil) {
			row.State, row.Reason, row.RecoveryAt = "paused", r.Reason, r.HoldUntil
			item.managed = true
			continue
		}
		bad, known, reason, sample := false, false, "等待本次启用后的调用样本", ""
		if r.HoldUntil != nil {
			r.HoldUntil = nil
			r.SampleSince = &now
			r.LastBadSample = ""
			e.counters[a.ID] = counter{}
			row.clearUsage()
			reason = "异常等待期已到，正在重新采样"
			logEvent(ledger, a.ID, a.Name, "restored", reason, now)
		} else {
			bad, known, reason, sample = evaluate(rule, *row)
			if !bad && row.Requests == 0 {
				known = false
				reason = "等待本次启用后的调用样本"
			}
		}
		count := e.counters[a.ID]
		if bad && known && sample != r.LastBadSample {
			count.bad++
		} else {
			count.bad = 0
		}
		e.counters[a.ID] = count
		if bad && known && count.bad >= rule.TriggerSamples {
			until := now.Add(time.Duration(rule.CooldownSeconds) * time.Second)
			r.HoldUntil, r.LastBadSample, r.Reason = &until, sample, reason
			row.State, row.Reason, row.RecoveryAt = "paused", reason, &until
			logEvent(ledger, a.ID, a.Name, "pause", reason, now)
			continue
		}
		item.eligible = true
		row.State, row.Reason = "ready", "组内账号可参与调用"
		if bad && known && sample != r.LastBadSample {
			row.State, row.Reason = "pending", reason
		} else if !known {
			row.State, row.Reason = "sampling", reason
		}
		rank := groupRank(item.setting.Group)
		if winner, ok := winners[a.Platform]; !ok || rank < winner {
			winners[a.Platform] = rank
		}
	}
	// Excellent group's optional low-rate rule selects one account per platform.
	exclusive := map[string]*managedAccount{}
	for _, item := range items {
		if !item.eligible || item.setting.Group != "excellent" {
			continue
		}
		rule := c.GroupRule(item.account.Platform, "excellent")
		threshold := rule.ExclusiveRateBelow
		if threshold <= 0 || item.row.EffectiveRate == nil || *item.row.EffectiveRate >= threshold {
			continue
		}
		if reason := exclusiveExitReason(rule, item.row); reason != "" {
			// Leaving exclusivity does not pause the account. It remains in the
			// ordinary group pool unless that group's own rules stop it.
			item.row.Reason = joinError(item.row.Reason, reason)
			continue
		}
		previous := exclusive[item.account.Platform]
		if previous == nil || *item.row.EffectiveRate < *previous.row.EffectiveRate || (*item.row.EffectiveRate == *previous.row.EffectiveRate && item.row.ID < previous.row.ID) {
			exclusive[item.account.Platform] = item
		}
	}
	for _, item := range items {
		if !item.eligible {
			continue
		}
		if unknownSelected {
			item.row.State, item.row.Reason = "group_wait", "有账号信息读取失败，暂不开放新的调用组"
			continue
		}
		if groupRank(item.setting.Group) != winners[item.account.Platform] {
			item.row.State, item.row.Reason = "group_wait", "等待前序调用组全部不可用"
			continue
		}
		if chosen := exclusive[item.account.Platform]; item.setting.Group == "excellent" && chosen != nil && chosen != item {
			item.row.State, item.row.Reason = "exclusive_wait", "优秀组低倍率账号独占调用"
			continue
		}
		item.schedulable = true
	}
	// Persist ownership, holds and decisions before the first mutation.
	if err := save(ctx, store, ledger); err != nil {
		return err
	}
	ordered := append([]*managedAccount{}, items...)
	sort.SliceStable(ordered, func(i, j int) bool { return !ordered[i].schedulable && ordered[j].schedulable })
	for _, item := range ordered {
		r := item.record
		if !item.readOK || item.account.Type != "apikey" || r == nil || r.Phase == "manual_change" {
			continue
		}
		if err := e.syncSchedule(ctx, api, store, ledger, item); err != nil {
			item.row.Error = joinError(item.row.Error, err.Error())
			// A failed write for one account must not prevent safe closure or
			// priority updates for the rest of the platform.
			continue
		}
		if item.restoring {
			delete(ledger.Records, strconv.FormatInt(item.row.ID, 10))
			logEvent(ledger, item.row.ID, item.row.Name, "restored", "已恢复插件接管前的优先级和调度状态", now)
			item.row.State, item.row.Reason = "observing", "原设置已恢复"
		} else if r.LastState != item.row.State {
			logEvent(ledger, item.row.ID, item.row.Name, item.row.State, item.row.Reason, now)
			r.LastState = item.row.State
		}
		if err := save(ctx, store, ledger); err != nil {
			return err
		}
	}
	return nil
}

func exclusiveExitReason(rule config.Rule, row AccountStatus) string {
	if rule.ExclusiveMinCacheRatio > 0 && row.Complete && row.Requests >= rule.MinRequests &&
		row.CacheRatio != nil && *row.CacheRatio < rule.ExclusiveMinCacheRatio {
		return fmt.Sprintf("缓存命中比例 %.1f%% 低于独占下限 %.1f%%，退出独占并按本组规则调用", *row.CacheRatio*100, rule.ExclusiveMinCacheRatio*100)
	}
	if rule.ExclusiveMaxAverageFirstTokenMs > 0 && row.AverageFirstTokenMs != nil && row.FirstTokenSamples >= 20 &&
		*row.AverageFirstTokenMs > rule.ExclusiveMaxAverageFirstTokenMs {
		return fmt.Sprintf("最近 20 次平均首字时间 %.0f ms 超过独占上限 %.0f ms，退出独占并按本组规则调用", *row.AverageFirstTokenMs, rule.ExclusiveMaxAverageFirstTokenMs)
	}
	return ""
}

func matchesOriginal(a adminapi.Account, r *ActionRecord) bool {
	return a.Status == r.OriginalStatus && a.Priority == r.OriginalPriority && a.Schedulable == r.OriginalSchedulable
}

func reconcileOwned(a adminapi.Account, r *ActionRecord) bool {
	if a.ID != r.AccountID || a.Type != "apikey" || a.Platform != r.Platform || a.Status != r.OriginalStatus {
		return false
	}
	if a.Priority == r.TargetPriority && a.Schedulable == r.TargetSchedulable {
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
	return false
}

func (e *Engine) syncSchedule(ctx context.Context, api API, store Store, ledger *Ledger, item *managedAccount) error {
	r := item.record
	fields := []string{"priority", "schedulable"}
	if !item.schedulable {
		fields = []string{"schedulable", "priority"}
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
		if field == "schedulable" && item.schedulable && !item.restoring {
			checked := item.row
			applyRateChoice(&checked, item.setting, item.snapshot, checked.RateMode != "waiting", e.clock().UTC())
			if !fresh.Active(e.clock().UTC()) || checked.EffectiveRate == nil {
				return errors.New("提交前账号或倍率已不可用，取消开放调度")
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
		if !reconcileOwned(confirmed, r) {
			r.Phase, item.row.State = "manual_change", "manual_change"
			item.row.Reason = "管理写入未确认，请人工核对账号设置"
			item.row.Priority, item.row.Schedulable = confirmed.Priority, confirmed.Schedulable
			if err := save(ctx, store, ledger); err != nil {
				return err
			}
			return errors.New("管理写入未确认，保留记录等待人工核对")
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
		if !row.Complete || row.CacheRatio == nil || row.Requests < rule.MinRequests {
			known = false
		} else if *row.CacheRatio < rule.MinCacheRatio {
			return true, true, fmt.Sprintf("缓存命中比例 %.1f%% 低于本组下限 %.1f%%", *row.CacheRatio*100, rule.MinCacheRatio*100), "cache:" + row.SampleKey
		}
	}
	if rule.MaxAverageFirstTokenMs > 0 {
		if row.AverageFirstTokenMs == nil || row.FirstTokenSamples < 20 {
			known = false
		} else if *row.AverageFirstTokenMs > rule.MaxAverageFirstTokenMs {
			return true, true, fmt.Sprintf("最近 20 次平均首字时间 %.0f ms 超过本组上限 %.0f ms", *row.AverageFirstTokenMs, rule.MaxAverageFirstTokenMs), "ttft:" + row.FirstTokenSampleKey
		}
	}
	if !known {
		if row.EffectiveRate == nil {
			return false, false, "有效倍率未知，等待上游倍率或设置固定倍率", ""
		}
		return false, false, "有效样本不足或采集不完整，等待监控数据", ""
	}
	return false, true, "本组异常条件未触发", ""
}
