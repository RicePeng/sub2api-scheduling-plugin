package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
)

type Rule struct {
	MaxRateMultiplier               float64 `json:"max_rate_multiplier"`
	MinCacheRatio                   float64 `json:"min_cache_ratio"`
	MaxAverageFirstTokenMs          float64 `json:"max_average_first_token_ms"`
	FirstTokenPeriods               int     `json:"first_token_periods"`
	ExclusiveRateBelow              float64 `json:"exclusive_rate_below"`
	ExclusiveMinCacheRatio          float64 `json:"exclusive_min_cache_ratio"`
	ExclusiveMaxAverageFirstTokenMs float64 `json:"exclusive_max_average_first_token_ms"`
	MinRequests                     int     `json:"min_requests"`
	MinInputTokens                  int64   `json:"min_input_tokens"`
	TriggerSamples                  int     `json:"trigger_samples"`
	RecoverySamples                 int     `json:"recovery_samples"`
	ConfirmationSeconds             int     `json:"confirmation_seconds"`
	RecoverySeconds                 int     `json:"recovery_seconds"`
	CooldownSeconds                 int     `json:"cooldown_seconds"`
	Action                          string  `json:"action"`
	PriorityPenalty                 int     `json:"priority_penalty"`
	RateSource                      string  `json:"rate_source,omitempty"`
}

// QualityWindowRequests is the fixed sample size for recent usage and quality rules.
const QualityWindowRequests = 20
const DefaultFirstTokenPeriods = 5

const DefaultMinInputTokens int64 = 40000
const MaxMinInputTokens int64 = 1000000000

// AccountSetting contains plugin-owned scheduling metadata for one monitored
// account. The host account itself is never mutated by configuration writes.
type AccountSetting struct {
	AccountID          int64    `json:"account_id"`
	Group              string   `json:"group"`
	RateMode           string   `json:"rate_mode"`
	FixedRate          *float64 `json:"fixed_rate,omitempty"`
	Platform           string   `json:"platform,omitempty"`
	DesiredSchedulable *bool    `json:"desired_schedulable,omitempty"`
}

type Config struct {
	AdminURL                string                     `json:"admin_url"`
	AdminAPIKey             string                     `json:"admin_api_key"`
	AccountIDs              []int64                    `json:"account_ids"`
	PollSeconds             int                        `json:"poll_seconds"`
	WindowMinutes           int                        `json:"window_minutes"`
	TimeoutSeconds          int                        `json:"timeout_seconds"`
	UpstreamRatesEnabled    bool                       `json:"upstream_rates_enabled"`
	AutomationEnabled       bool                       `json:"automation_enabled"`
	ExecutorHostname        string                     `json:"executor_hostname"`
	AccountSettings         []AccountSetting           `json:"account_settings"`
	GroupPriorities         map[string]int             `json:"group_priorities"`
	GroupRules              map[string]Rule            `json:"group_rules"`
	PlatformGroupPriorities map[string]map[string]int  `json:"platform_group_priorities"`
	PlatformGroupRules      map[string]map[string]Rule `json:"platform_group_rules"`
	Rule                    Rule                       `json:"rule"`
}

var groupIDs = []string{"always_on", "excellent", "good", "general", "reserve", "temporary"}

func DefaultGroupPriorities() map[string]int {
	return map[string]int{"always_on": 5, "excellent": 6, "good": 7, "general": 8, "reserve": 9, "temporary": 99}
}

func defaultRule() Rule {
	return Rule{MaxRateMultiplier: .2, MinCacheRatio: .8, MaxAverageFirstTokenMs: 10000, FirstTokenPeriods: DefaultFirstTokenPeriods, ExclusiveRateBelow: .05,
		ExclusiveMinCacheRatio: .8, ExclusiveMaxAverageFirstTokenMs: 10000,
		MinRequests: QualityWindowRequests, MinInputTokens: DefaultMinInputTokens,
		TriggerSamples: 2, RecoverySamples: 2, ConfirmationSeconds: 180, RecoverySeconds: 300,
		CooldownSeconds: 300, Action: "pause", PriorityPenalty: 10, RateSource: "account"}
}

func Defaults() Config {
	rule := defaultRule()
	groupRules := make(map[string]Rule, len(groupIDs))
	for _, id := range groupIDs {
		groupRules[id] = rule
	}
	return Config{
		AccountIDs: []int64{}, AccountSettings: []AccountSetting{}, GroupPriorities: DefaultGroupPriorities(),
		GroupRules:  groupRules,
		PollSeconds: 60, WindowMinutes: 15, TimeoutSeconds: 15, UpstreamRatesEnabled: true,
		Rule: rule,
	}
}

func Decode(raw []byte) (Config, error) {
	c := Defaults()
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 65536 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return c, errors.New("配置必须是小于 64 KiB 的 JSON 对象")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, errors.New("配置 JSON 无效或包含不支持的字段")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, errors.New("配置只能包含一个 JSON 对象")
	}
	// The global switch was removed. Retain the field to read existing configs
	// while each account's rate mode continues to select upstream or fixed.
	c.UpstreamRatesEnabled = true
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if _, present := fields["group_rules"]; !present {
		c.GroupRules = nil // Migrate the previously global rule into each group.
	}
	c.AdminURL, c.AdminAPIKey = strings.TrimSpace(c.AdminURL), strings.TrimSpace(c.AdminAPIKey)
	c.ExecutorHostname = strings.TrimSpace(c.ExecutorHostname)
	if c.AdminURL != "" {
		u, err := url.Parse(c.AdminURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("管理地址必须是无凭证、查询参数和片段的 HTTP(S) 地址")
		}
		c.AdminURL = strings.TrimRight(c.AdminURL, "/")
		if !strings.HasSuffix(c.AdminURL, "/api/v1") {
			c.AdminURL += "/api/v1"
		}
	}
	if (c.AdminURL == "") != (c.AdminAPIKey == "") {
		return c, errors.New("管理地址与管理密钥需要同时填写")
	}
	if strings.ContainsAny(c.AdminAPIKey, "\r\n") {
		return c, errors.New("管理密钥不能包含换行")
	}
	if c.PollSeconds < 15 || c.PollSeconds > 3600 || c.WindowMinutes < 1 || c.WindowMinutes > 1440 ||
		c.TimeoutSeconds < 3 || c.TimeoutSeconds > 120 {
		return c, errors.New("采集间隔须为 15–3600 秒，窗口为 1–1440 分钟，超时为 3–120 秒")
	}
	if len(c.AccountIDs) > 100 {
		return c, errors.New("单个插件最多监控 100 个账号")
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(c.AccountIDs))
	for _, id := range c.AccountIDs {
		if id <= 0 || seen[id] {
			return c, errors.New("账号 ID 必须为不重复的正整数")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	c.AccountIDs = ids
	c.Rule = NormalizeRule(c.Rule)
	if err := validateRule(c.Rule); err != nil {
		return c, err
	}
	if c.GroupRules == nil {
		c.GroupRules = make(map[string]Rule, len(groupIDs))
		for _, id := range groupIDs {
			c.GroupRules[id] = c.Rule
		}
	}
	for id, rule := range c.GroupRules {
		if !containsGroup(id) {
			return c, errors.New("异常条件包含无效分组")
		}
		rule = NormalizeRule(rule)
		if err := validateRule(rule); err != nil {
			return c, err
		}
		c.GroupRules[id] = rule
	}
	for _, id := range groupIDs {
		if _, ok := c.GroupRules[id]; !ok {
			c.GroupRules[id] = c.Rule
		}
	}
	if c.AccountSettings == nil {
		c.AccountSettings = []AccountSetting{}
	}
	selected := map[int64]bool{}
	for _, id := range c.AccountIDs {
		selected[id] = true
	}
	seenSettings := map[int64]bool{}
	for i := range c.AccountSettings {
		setting := &c.AccountSettings[i]
		if setting.AccountID <= 0 || !selected[setting.AccountID] || seenSettings[setting.AccountID] {
			return c, errors.New("账号设置必须对应不重复的监控账号 ID")
		}
		seenSettings[setting.AccountID] = true
		if setting.Group == "" {
			setting.Group = "temporary"
		}
		if !containsGroup(setting.Group) {
			return c, errors.New("账号分组无效")
		}
		if setting.Platform != "" && !validPlatform(setting.Platform) {
			return c, errors.New("账号平台标识无效")
		}
		if setting.RateMode == "" || setting.RateMode == "account" {
			setting.RateMode = "upstream"
		}
		if setting.RateMode != "account" && setting.RateMode != "upstream" && setting.RateMode != "fixed" {
			return c, errors.New("账号倍率方案必须为 account、upstream 或 fixed")
		}
		if setting.FixedRate != nil && (!validNumber(*setting.FixedRate) || *setting.FixedRate > 1000000) {
			return c, errors.New("账号固定倍率必须为非负有限数")
		}
		if setting.Group != "temporary" && setting.RateMode == "fixed" && setting.FixedRate == nil {
			return c, errors.New("固定倍率方案必须填写倍率")
		}
	}
	if c.GroupPriorities == nil {
		c.GroupPriorities = DefaultGroupPriorities()
	}
	for _, id := range groupIDs {
		priority, ok := c.GroupPriorities[id]
		if !ok {
			c.GroupPriorities[id] = DefaultGroupPriorities()[id]
			continue
		}
		if priority < 0 || priority > 10000 {
			return c, errors.New("调用组优先级必须在 0 与 10000 之间")
		}
	}
	for id := range c.GroupPriorities {
		if !containsGroup(id) {
			return c, errors.New("调用组优先级包含无效分组")
		}
	}
	c.GroupPriorities["temporary"] = 99
	for platform, priorities := range c.PlatformGroupPriorities {
		if !validPlatform(platform) {
			return c, errors.New("平台标识无效")
		}
		if priorities == nil {
			priorities = map[string]int{}
		}
		for group, priority := range priorities {
			if !containsGroup(group) || priority < 0 || priority > 10000 {
				return c, errors.New("平台调用组优先级无效")
			}
		}
		for _, group := range groupIDs {
			if _, ok := priorities[group]; !ok {
				priorities[group] = c.GroupPriorities[group]
			}
		}
		priorities["temporary"] = 99
		c.PlatformGroupPriorities[platform] = priorities
	}
	for platform, rules := range c.PlatformGroupRules {
		if !validPlatform(platform) {
			return c, errors.New("平台标识无效")
		}
		if rules == nil {
			rules = map[string]Rule{}
		}
		for group, rule := range rules {
			if !containsGroup(group) {
				return c, errors.New("平台异常条件包含无效分组")
			}
			rule = NormalizeRule(rule)
			if err := validateRule(rule); err != nil {
				return c, err
			}
			rules[group] = rule
		}
		for _, group := range groupIDs {
			if _, ok := rules[group]; !ok {
				rules[group] = c.GroupRules[group]
			}
		}
		c.PlatformGroupRules[platform] = rules
	}
	if c.AutomationEnabled {
		if c.AdminURL == "" || len(c.AccountIDs) == 0 || c.ExecutorHostname == "" {
			return c, errors.New("自动管理需要管理连接、账号列表和唯一执行节点主机名")
		}
	}
	return c, nil
}

// NormalizeRule applies the current defaults to configuration and stored policy.
func NormalizeRule(r Rule) Rule {
	d := defaultRule()
	if r.ExclusiveMinCacheRatio == 0 {
		r.ExclusiveMinCacheRatio = d.ExclusiveMinCacheRatio
	}
	if r.ExclusiveMaxAverageFirstTokenMs == 0 {
		r.ExclusiveMaxAverageFirstTokenMs = d.ExclusiveMaxAverageFirstTokenMs
	}
	// Accept previously valid settings without changing the fixed window size.
	if r.MinRequests >= 0 && r.MinRequests <= 20 {
		r.MinRequests = QualityWindowRequests
	}
	if r.MinInputTokens == 0 {
		r.MinInputTokens = d.MinInputTokens
	}
	if r.FirstTokenPeriods == 0 {
		r.FirstTokenPeriods = d.FirstTokenPeriods
	}
	if r.TriggerSamples == 0 {
		r.TriggerSamples = d.TriggerSamples
	}
	if r.RecoverySamples == 0 {
		r.RecoverySamples = d.RecoverySamples
	}
	// Old one-poll rules must not bypass independent-window confirmation.
	if r.TriggerSamples == 1 {
		r.TriggerSamples = 2
	}
	if r.RecoverySamples == 1 {
		r.RecoverySamples = 2
	}
	if r.ConfirmationSeconds == 0 {
		r.ConfirmationSeconds = d.ConfirmationSeconds
	}
	if r.RecoverySeconds == 0 {
		r.RecoverySeconds = d.RecoverySeconds
	}
	if r.CooldownSeconds == 0 {
		r.CooldownSeconds = d.CooldownSeconds
	}
	if r.PriorityPenalty == 0 {
		r.PriorityPenalty = d.PriorityPenalty
	}
	if r.Action == "" {
		r.Action = d.Action
	}
	return r
}

func validateRule(r Rule) error {
	if r.ConfirmationSeconds < 60 || r.ConfirmationSeconds > 86400 || r.RecoverySeconds < 60 || r.RecoverySeconds > 86400 {
		return errors.New("质量确认与恢复观察时间须为 60–86400 秒")
	}
	if !validNumber(r.MaxRateMultiplier) || r.MaxRateMultiplier > 1000000 ||
		!validNumber(r.MinCacheRatio) || r.MinCacheRatio > 1 || !validNumber(r.MaxAverageFirstTokenMs) || r.MaxAverageFirstTokenMs > 3600000 ||
		!validNumber(r.ExclusiveRateBelow) || r.ExclusiveRateBelow > 1000000 {
		return errors.New("倍率上限须为非负有限数，缓存下限须为 0–1，首字时间上限须为 0–3600000 毫秒，倍率下限须为非负有限数")
	}
	if !validNumber(r.ExclusiveMinCacheRatio) || r.ExclusiveMinCacheRatio > 1 ||
		!validNumber(r.ExclusiveMaxAverageFirstTokenMs) || r.ExclusiveMaxAverageFirstTokenMs > 3600000 {
		return errors.New("独占缓存下限须为 0–1，独占首字时间上限须为 0–3600000 毫秒；0 使用默认值")
	}
	if r.MinRequests != QualityWindowRequests {
		return fmt.Errorf("调用窗口固定为 %d 次；旧版调用样本数配置须为 1–20", QualityWindowRequests)
	}
	if r.FirstTokenPeriods < 1 || r.FirstTokenPeriods > 20 {
		return errors.New("平均首字时间周期须为 1–20 个窗口")
	}
	if r.MinInputTokens < 1 || r.MinInputTokens > MaxMinInputTokens {
		return fmt.Errorf("最小输入上下文须为 1–%d token", MaxMinInputTokens)
	}
	if r.TriggerSamples < 1 || r.TriggerSamples > 100 || r.RecoverySamples < 1 || r.RecoverySamples > 100 ||
		r.CooldownSeconds < 30 || r.CooldownSeconds > 86400 {
		return errors.New("连续次数须为 1–100，恢复等待须为 30–86400 秒")
	}
	if r.Action != "pause" && r.Action != "deprioritize" {
		return errors.New("动作必须为 pause 或 deprioritize")
	}
	return nil
}

var platformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validPlatform(s string) bool { return platformPattern.MatchString(s) }

func (c Config) GroupRule(platform, group string) Rule {
	if r, ok := c.PlatformGroupRules[platform][group]; ok {
		return r
	}
	if r, ok := c.GroupRules[group]; ok {
		return r
	}
	return c.Rule
}
func (c Config) GroupPriority(platform, group string) int {
	if group == "temporary" {
		return 99
	}
	if p, ok := c.PlatformGroupPriorities[platform][group]; ok {
		return p
	}
	if p, ok := c.GroupPriorities[group]; ok {
		return p
	}
	return DefaultGroupPriorities()[group]
}

func containsGroup(id string) bool {
	for _, known := range groupIDs {
		if id == known {
			return true
		}
	}
	return false
}

func validNumber(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (c Config) JSON() []byte {
	raw, _ := json.Marshal(c)
	return raw
}
