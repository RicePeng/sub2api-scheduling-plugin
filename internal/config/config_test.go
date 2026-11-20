package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeDefaultsAndStrictInput(t *testing.T) {
	c, err := Decode([]byte("{}"))
	if err != nil || c.AutomationEnabled || !c.UpstreamRatesEnabled || c.PollSeconds != 60 || c.AccountIDs == nil ||
		c.Rule.MinCacheRatio != .8 || c.Rule.MaxAverageFirstTokenMs != 10000 ||
		c.Rule.MinRequests != 20 || c.Rule.TriggerSamples != 2 || c.Rule.RecoverySamples != 2 ||
		c.Rule.ConfirmationSeconds != 180 || c.Rule.RecoverySeconds != 300 ||
		c.Rule.MaxRateMultiplier != .2 || c.GroupRules["excellent"].ExclusiveRateBelow != .05 ||
		c.GroupRules["excellent"].ExclusiveMinCacheRatio != .8 || c.GroupRules["excellent"].ExclusiveMaxAverageFirstTokenMs != 10000 {
		t.Fatalf("unsafe defaults: %+v %v", c, err)
	}
	for _, raw := range []string{`null`, `{"unknown":1}`, `{} {}`, `{"admin_api_key":"SECRET"}`,
		`{"poll_seconds":1}`, `{"account_ids":[1,1]}`, `{"rule":{"action":"delete"}}`,
		`{"automation_enabled":true}`, `{"admin_url":"https://user:SECRET@example.test","admin_api_key":"x"}`} {
		_, err := Decode([]byte(raw))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("invalid or leaky result for %s: %v", raw, err)
		}
	}
}

func TestLegacyMinRequestsNormalizesToFixedQualityWindow(t *testing.T) {
	c, err := Decode([]byte(`{"rule":{"min_requests":15},"group_rules":{"general":{"min_requests":14}},
		"platform_group_rules":{"openai":{"general":{"min_requests":15}}}}`))
	if err != nil || c.Rule.MinRequests != 20 || c.GroupRules["general"].MinRequests != 20 ||
		c.GroupRule("openai", "general").MinRequests != 20 {
		t.Fatalf("legacy sample sizes must load with a fixed 20-request window: %+v %v", c, err)
	}
}

func TestInputThresholdDefaultsAndPlatformOverrides(t *testing.T) {
	c, err := Decode([]byte(`{"rule":{"min_input_tokens":0},"group_rules":{"general":{"min_input_tokens":45000}},
		"platform_group_rules":{"openai":{"general":{"min_input_tokens":60000}}}}`))
	if err != nil || c.Rule.MinInputTokens != 40000 || c.GroupRule("openai", "general").MinInputTokens != 60000 ||
		c.GroupRule("anthropic", "general").MinInputTokens != 45000 ||
		c.GroupRule("openai", "always_on").MinInputTokens != 40000 {
		t.Fatalf("threshold fallback or override not applied: %+v %v", c, err)
	}
	for _, raw := range []string{`{"rule":{"min_input_tokens":-1}}`, `{"rule":{"min_input_tokens":1000000001}}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("invalid capacity accepted: %s", raw)
		}
	}
}

func TestRemovedUpstreamSwitchPreservesAccountRateChoices(t *testing.T) {
	c, err := Decode([]byte(`{"upstream_rates_enabled":false,"account_ids":[7,8],"account_settings":[{"account_id":7,"rate_mode":"upstream"},{"account_id":8,"rate_mode":"fixed","fixed_rate":0.07}]}`))
	if err != nil || !c.UpstreamRatesEnabled || c.AccountSettings[0].RateMode != "upstream" ||
		c.AccountSettings[1].RateMode != "fixed" || c.AccountSettings[1].FixedRate == nil || *c.AccountSettings[1].FixedRate != .07 {
		t.Fatalf("retired switch changed account choices or blocked snapshots: %+v %v", c, err)
	}
	parsed, err := Decode(c.JSON())
	if err != nil || !parsed.UpstreamRatesEnabled {
		t.Fatalf("snapshot default did not survive a save round trip: %+v %v", parsed, err)
	}
}

func TestExclusiveThresholdsValidateAndRemainIndependent(t *testing.T) {
	c := Defaults()
	rule := c.GroupRules["excellent"]
	rule.MinCacheRatio, rule.MaxAverageFirstTokenMs = .4, 20000
	rule.ExclusiveMinCacheRatio, rule.ExclusiveMaxAverageFirstTokenMs = .9, 1500
	c.PlatformGroupRules = map[string]map[string]Rule{"openai": {"excellent": rule}}
	parsed, err := Decode(c.JSON())
	if err != nil {
		t.Fatal(err)
	}
	r := parsed.GroupRule("openai", "excellent")
	if r.MinCacheRatio != .4 || r.ExclusiveMinCacheRatio != .9 || r.MaxAverageFirstTokenMs != 20000 || r.ExclusiveMaxAverageFirstTokenMs != 1500 || parsed.GroupRule("anthropic", "excellent").ExclusiveMinCacheRatio != .8 {
		t.Fatalf("exclusive thresholds leaked into ordinary or other platform rules: %+v", r)
	}
	for _, raw := range []string{
		`{"rule":{"exclusive_min_cache_ratio":1.01}}`,
		`{"rule":{"exclusive_min_cache_ratio":-0.1}}`,
		`{"group_rules":{"excellent":{"exclusive_max_average_first_token_ms":3600001}}}`,
		`{"platform_group_rules":{"openai":{"excellent":{"exclusive_max_average_first_token_ms":-1}}}}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("invalid exclusive threshold was accepted: %s", raw)
		}
	}
	legacy, err := Decode([]byte(`{"group_rules":{"excellent":{"exclusive_rate_below":0.05}}}`))
	if err != nil || legacy.GroupRules["excellent"].ExclusiveMinCacheRatio != .8 || legacy.GroupRules["excellent"].ExclusiveMaxAverageFirstTokenMs != 10000 {
		t.Fatalf("old configuration lost new defaults: %+v %v", legacy.GroupRules["excellent"], err)
	}
}

func TestAutomationAndCacheActionValidation(t *testing.T) {
	c := Defaults()
	c.AdminURL, c.AdminAPIKey, c.AccountIDs, c.ExecutorHostname = "https://example.test", "key", []int64{1}, "node"
	c.AutomationEnabled, c.Rule.MaxRateMultiplier = true, .2
	parsed, err := Decode(c.JSON())
	if err != nil || parsed.AdminURL != "https://example.test/api/v1" || parsed.Rule.MaxRateMultiplier != .2 {
		t.Fatalf("%+v %v", parsed, err)
	}
	c.GroupRules["general"] = c.Rule
	rule := c.GroupRules["general"]
	rule.MinCacheRatio = .2
	c.GroupRules["general"] = rule
	if _, err := Decode(c.JSON()); err != nil {
		t.Fatalf("cache pause should be allowed: %v", err)
	}
	var normalized map[string]any
	if json.Unmarshal(parsed.JSON(), &normalized) != nil {
		t.Fatal("cannot encode normalized config")
	}
}

func TestGroupRulesAndPlatformSettingsNormalizeIndependently(t *testing.T) {
	c := Defaults()
	c.GroupRules = map[string]Rule{"good": {MinCacheRatio: .25, MinRequests: 20, TriggerSamples: 2, RecoverySamples: 3, CooldownSeconds: 300, Action: "pause", PriorityPenalty: 10}}
	c.PlatformGroupPriorities = map[string]map[string]int{"openai": {"good": 7}, "anthropic": {"good": 70}}
	c.PlatformGroupRules = map[string]map[string]Rule{"openai": {"good": {MinCacheRatio: .4}}, "anthropic": {"good": {MaxAverageFirstTokenMs: 800}}}
	parsed, err := Decode(c.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.GroupPriority("openai", "good") != 7 || parsed.GroupPriority("anthropic", "good") != 70 || parsed.GroupPriority("anthropic", "temporary") != 99 {
		t.Fatalf("priority leaked between platforms: %+v", parsed.PlatformGroupPriorities)
	}
	if parsed.GroupRule("openai", "good").MinCacheRatio != .4 || parsed.GroupRule("anthropic", "good").MaxAverageFirstTokenMs != 800 {
		t.Fatalf("rules leaked between platforms: %+v", parsed.PlatformGroupRules)
	}
	if parsed.GroupPriorities["temporary"] != 99 {
		t.Fatal("temporary priority must be fixed at 99")
	}
}

func TestAccountSettingsAndGroupPriorities(t *testing.T) {
	c := Defaults()
	c.AccountIDs = []int64{7}
	c.AccountSettings = []AccountSetting{{AccountID: 7, Group: "general", RateMode: "account"}}
	if _, err := Decode(c.JSON()); err != nil {
		t.Fatal(err)
	}
	c.AccountSettings[0].RateMode = "fixed"
	if _, err := Decode(c.JSON()); err == nil {
		t.Fatal("fixed mode without a value must be rejected")
	}
	rate := 1.25
	c.AccountSettings[0].FixedRate = &rate
	if _, err := Decode(c.JSON()); err != nil {
		t.Fatal(err)
	}
	c.GroupPriorities["unknown"] = 1
	if _, err := Decode(c.JSON()); err == nil {
		t.Fatal("unknown group priority must be rejected")
	}
}

func TestLegacyRulesMigrateAndAccountRateIsNotImplicitFixedRate(t *testing.T) {
	c, err := Decode([]byte(`{"account_ids":[7],"account_settings":[{"account_id":7,"rate_mode":"account"}],"rule":{"min_cache_ratio":0.4,"max_rate_multiplier":2}}`))
	if err != nil || c.GroupRule("openai", "general").MinCacheRatio != .4 ||
		c.AccountSettings[0].Group != "temporary" || c.AccountSettings[0].RateMode != "upstream" || c.AccountSettings[0].FixedRate != nil {
		t.Fatalf("incorrect migration: %+v %v", c, err)
	}
}
