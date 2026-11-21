package monitor

import (
	"context"
	"testing"
	"time"

	"sub2api.local/cache-manager/internal/config"
)

func TestHTTPQualityConfirmsPersistedStreakAfterDwellWithoutNewWindow(t *testing.T) {
	for _, tc := range []struct {
		name, intervening string
		wantFault         bool
	}{
		{name: "unchanged", wantFault: true},
		{name: "healthy completed window", intervening: "healthy"},
		{name: "unknown completed window", intervening: "unknown-ttft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, store, now, add, failNext := setupHTTPQuality(t, .8, 1000)
			first := now.Add(180 * time.Second)
			add(20, first, "bad", "m", "sync")
			e.Cycle(context.Background(), first)
			second := first.Add(time.Second)
			add(20, second, "bad", "m", "stream")
			e.Cycle(context.Background(), second)
			if r := ledgerOf(t, store).Records["7"]; r.BadWindows != 2 || r.FailureCount != 0 {
				t.Fatalf("expected two pending bad windows before dwell: %+v", r)
			}
			confirm := second.Add(180 * time.Second)
			if tc.intervening != "" {
				add(20, confirm.Add(-time.Second), tc.intervening, "m", "sync")
			}
			e = restartMonitor(t, e, c, store, confirm)
			failNext()
			e.Cycle(context.Background(), confirm)
			if r := ledgerOf(t, store).Records["7"]; r.FailureCount != 0 {
				t.Fatalf("failed usage scan confirmed a fault: %+v", r)
			}
			e.Cycle(context.Background(), confirm.Add(time.Second))
			r := ledgerOf(t, store).Records["7"]
			if tc.wantFault {
				if r.FailureCount != 1 || r.FailureKind != "quality" || snapshotAccount(t, e, 7).Schedulable {
					t.Fatalf("successful scan without a new window failed to confirm the persisted streak: %+v", r)
				}
			} else if r.BadWindows != 0 || r.FailureCount != 0 || !snapshotAccount(t, e, 7).Schedulable {
				t.Fatalf("intervening completed window failed to break the bad streak: %+v", r)
			}
		})
	}
}

func TestHTTPQualityRecoversOnFifthTTFTPeriod(t *testing.T) {
	for _, tc := range []struct {
		name, penultimate, final string
		wantRecovered            bool
	}{
		{name: "100 healthy requests", penultimate: "healthy", final: "healthy", wantRecovered: true},
		{name: "one slow window with in-range period average", penultimate: "healthy", final: "boundary-top-slow", wantRecovered: true},
		{name: "period average exceeds cap", penultimate: "boundary-top-slow", final: "boundary-top-slow"},
		{name: "missing TTFT in a completed window", penultimate: "unknown-ttft", final: "healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, store, now, add, _ := setupHTTPQuality(t, .8, 5000)
			rule := c.GroupRules["general"]
			rule.FirstTokenPeriods = config.Defaults().GroupRules["general"].FirstTokenPeriods
			c.GroupRules["general"] = rule
			if err := e.Apply(c); err != nil {
				t.Fatal(err)
			}
			// Commit the changed period policy before starting the two bad windows.
			e.Cycle(context.Background(), now)
			first := now.Add(180 * time.Second)
			add(20, first, "bad", "m", "sync")
			e.Cycle(context.Background(), first)
			second := first.Add(180 * time.Second)
			add(20, second, "bad", "m", "stream")
			e.Cycle(context.Background(), second)
			r := ledgerOf(t, store).Records["7"]
			if r.HoldUntil == nil || r.FailureCount != 1 {
				t.Fatalf("cache-only fault did not open a hold during TTFT warmup: %+v", r)
			}
			trial := *r.HoldUntil
			e.Cycle(context.Background(), trial)
			observation := trial.Add(300 * time.Second)
			add(60, observation, "healthy", "m", "sync")
			add(20, observation, tc.penultimate, "m", "stream")
			add(19, observation, tc.final, "m", "stream")
			e.Cycle(context.Background(), observation)
			row := snapshotAccount(t, e, 7)
			if !row.RecoveryTrial || row.FirstTokenPeriodAverage != nil || row.QualityGoodWindows != 0 ||
				row.QualitySampling == nil || row.QualitySampling.Requests != 19 {
				t.Fatalf("99 trial requests were treated as five completed TTFT periods: %+v", row)
			}
			wantProvisional := 4
			if tc.name == "missing TTFT in a completed window" {
				wantProvisional = 0
			}
			if r := ledgerOf(t, store).Records["7"]; r.ProvisionalGoodWindows != wantProvisional {
				t.Fatalf("unexpected provisional healthy-window count: want %d, got %+v", wantProvisional, r)
			}
			at := observation.Add(time.Second)
			e = restartMonitor(t, e, c, store, at)
			add(1, at, tc.final, "m", "stream")
			e.Cycle(context.Background(), at)
			row = snapshotAccount(t, e, 7)
			if tc.wantRecovered {
				if row.RecoveryTrial || row.State != "ready" || row.FirstTokenPeriodAverage == nil || *row.FirstTokenPeriodAverage > rule.MaxAverageFirstTokenMs ||
					ledgerOf(t, store).Records["7"].ProvisionalGoodWindows < recoveryWindows(rule) || row.FailureCount != 1 {
					t.Fatalf("five complete in-range TTFT periods did not finish recovery: %+v", row)
				}
			} else if !row.RecoveryTrial || row.QualityGoodWindows != 0 || row.FailureCount != 1 {
				t.Fatalf("incomplete or out-of-range TTFT periods finished recovery or raised a new fault: %+v", row)
			}
		})
	}
}
