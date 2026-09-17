package monitor

import (
	"testing"
	"time"
)

func TestPollBoostFreshActivationHasFiveSecondGrace(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	p0 := state.Resolve(start)
	if p0.Phase != PollBoostGrace || !p0.Active {
		t.Fatalf("expected GRACE at T+0, got %+v", p0)
	}
	if !p0.NextPhaseAt.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("expected NextPhaseAt to be T+5s, got %v", p0.NextPhaseAt)
	}

	pGrace := state.Resolve(start.Add(4999 * time.Millisecond))
	if pGrace.Phase != PollBoostGrace || !pGrace.Active {
		t.Fatalf("expected GRACE at T+4.999s, got %+v", pGrace)
	}
}

func TestPollBoostHotProfile(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	pHotStart := state.Resolve(start.Add(5 * time.Second))
	if pHotStart.Phase != PollBoostHot || !pHotStart.Active {
		t.Fatalf("expected HOT at T+5s, got %+v", pHotStart)
	}
	if pHotStart.MinInterval != 2*time.Second || pHotStart.MaxInterval != 4*time.Second {
		t.Fatalf("expected 2-4s interval, got min=%v max=%v", pHotStart.MinInterval, pHotStart.MaxInterval)
	}
	if !pHotStart.NextPhaseAt.Equal(start.Add(60 * time.Second)) {
		t.Fatalf("expected NextPhaseAt to be T+60s, got %v", pHotStart.NextPhaseAt)
	}

	pHotEnd := state.Resolve(start.Add(59 * time.Second))
	if pHotEnd.Phase != PollBoostHot || !pHotEnd.Active {
		t.Fatalf("expected HOT at T+59s, got %+v", pHotEnd)
	}
}

func TestPollBoostWarmProfile(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	pWarmStart := state.Resolve(start.Add(60 * time.Second))
	if pWarmStart.Phase != PollBoostWarm || !pWarmStart.Active {
		t.Fatalf("expected WARM at T+60s, got %+v", pWarmStart)
	}
	if pWarmStart.MinInterval != 3*time.Second || pWarmStart.MaxInterval != 6*time.Second {
		t.Fatalf("expected 3-6s interval, got min=%v max=%v", pWarmStart.MinInterval, pWarmStart.MaxInterval)
	}
	if !pWarmStart.NextPhaseAt.Equal(start.Add(120 * time.Second)) {
		t.Fatalf("expected NextPhaseAt to be T+120s, got %v", pWarmStart.NextPhaseAt)
	}
}

func TestPollBoostCoolProfile(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	pCoolStart := state.Resolve(start.Add(120 * time.Second))
	if pCoolStart.Phase != PollBoostCool || !pCoolStart.Active {
		t.Fatalf("expected COOL at T+120s, got %+v", pCoolStart)
	}
	if pCoolStart.MinInterval != 6*time.Second || pCoolStart.MaxInterval != 10*time.Second {
		t.Fatalf("expected 6-10s interval, got min=%v max=%v", pCoolStart.MinInterval, pCoolStart.MaxInterval)
	}
	if !pCoolStart.NextPhaseAt.Equal(start.Add(180 * time.Second)) {
		t.Fatalf("expected NextPhaseAt to be T+180s, got %v", pCoolStart.NextPhaseAt)
	}
}

func TestPollBoostExpiresToIdle(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	pIdle := state.Resolve(start.Add(180 * time.Second))
	if pIdle.Phase != PollBoostIdle || pIdle.Active {
		t.Fatalf("expected IDLE at T+180s, got %+v", pIdle)
	}

	pLater := state.Resolve(start.Add(300 * time.Second))
	if pLater.Phase != PollBoostIdle || pLater.Active {
		t.Fatalf("expected IDLE at T+300s, got %+v", pLater)
	}
}

func TestPollBoostActivationDuringHotExtendsWithoutGrace(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	// Second activation at T+30s while in HOT
	t30 := start.Add(30 * time.Second)
	state.Activate(t30)

	// Still in HOT, no 5s grace re-introduced
	pAt30 := state.Resolve(t30)
	if pAt30.Phase != PollBoostHot {
		t.Fatalf("expected HOT at T+30s after re-activation, got %+v", pAt30)
	}

	// HOT extended to T+30 + 60 = T+90s
	pAt89 := state.Resolve(start.Add(89 * time.Second))
	if pAt89.Phase != PollBoostHot {
		t.Fatalf("expected HOT at T+89s (extended), got %+v", pAt89)
	}

	// WARM begins at T+90s
	pAt90 := state.Resolve(start.Add(90 * time.Second))
	if pAt90.Phase != PollBoostWarm {
		t.Fatalf("expected WARM at T+90s, got %+v", pAt90)
	}
}

func TestPollBoostActivationDuringWarmPromotesToHot(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	// At T+70s, it is in WARM
	t70 := start.Add(70 * time.Second)
	pBefore := state.Resolve(t70)
	if pBefore.Phase != PollBoostWarm {
		t.Fatalf("expected WARM at T+70s, got %+v", pBefore)
	}

	// Scan arrives at T+70s
	state.Activate(t70)

	// Immediately returns to HOT, no grace
	pAfter := state.Resolve(t70)
	if pAfter.Phase != PollBoostHot {
		t.Fatalf("expected HOT immediately at T+70s, got %+v", pAfter)
	}
	if !pAfter.NextPhaseAt.Equal(t70.Add(60 * time.Second)) {
		t.Fatalf("expected HOT until T+130s, got %v", pAfter.NextPhaseAt)
	}
}

func TestPollBoostAntiAbuseBudgetClampsHotWindow(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	state.Activate(start)

	// Simulate attacker sending activation every 30s for 10 minutes
	for step := 1; step <= 20; step++ {
		cur := start.Add(time.Duration(step*30) * time.Second)
		state.Activate(cur)
	}

	// HOT budget is capped at BoostStartedAt + 5m (T+300s)
	maxHotTime := start.Add(paymentHotBudget)
	if state.HotUntil.After(maxHotTime) {
		t.Fatalf("HotUntil exceeded budget: got %v, want <= %v", state.HotUntil, maxHotTime)
	}

	// At T+301s, phase MUST have degraded to WARM or COOL, never HOT
	pAfterHot := state.Resolve(start.Add(301 * time.Second))
	if pAfterHot.Phase == PollBoostHot {
		t.Fatalf("expected degradation past 5m budget, still HOT: %+v", pAfterHot)
	}
}

func TestPollBoostLockoutTriggersAfterExhaustion(t *testing.T) {
	var state PollBoostState
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	// Keep activating every 2m from T+0 to T+30m to stay active continuously
	for m := 0; m <= 30; m += 2 {
		state.Activate(start.Add(time.Duration(m) * time.Minute))
	}

	// At T+30m, lockout must be engaged
	at30m := start.Add(30 * time.Minute)
	if !state.InLockout(at30m) {
		t.Fatalf("expected lockout to be engaged after reaching max cool budget, lockoutUntil=%v", state.LockoutUntil)
	}

	pLocked := state.Resolve(start.Add(30 * time.Minute))
	if pLocked.Phase != PollBoostLocked || pLocked.Active {
		t.Fatalf("expected LOCKED phase, got %+v", pLocked)
	}

	// Further activations during lockout must be ignored
	state.Activate(start.Add(35 * time.Minute))
	pStillLocked := state.Resolve(start.Add(35 * time.Minute))
	if pStillLocked.Phase != PollBoostLocked {
		t.Fatalf("activation during lockout must not revive boost, got %+v", pStillLocked)
	}

	// After lockout window expires, fresh activation succeeds
	postLockout := start.Add(30*time.Minute + paymentBoostLockout + 1*time.Second)
	state.Activate(postLockout)
	pFresh := state.Resolve(postLockout)
	if pFresh.Phase != PollBoostGrace || !pFresh.Active {
		t.Fatalf("expected fresh GRACE post-lockout, got %+v", pFresh)
	}
}
