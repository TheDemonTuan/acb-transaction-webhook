package monitor

import (
	"time"
)

type PollBoostPhase string

const (
	PollBoostIdle   PollBoostPhase = "IDLE"
	PollBoostGrace  PollBoostPhase = "GRACE"
	PollBoostHot    PollBoostPhase = "HOT"
	PollBoostWarm   PollBoostPhase = "WARM"
	PollBoostCool   PollBoostPhase = "COOL"
	PollBoostLocked PollBoostPhase = "LOCKED"
)

var (
	paymentGraceDuration = 5 * time.Second

	paymentHotDuration  = 60 * time.Second
	paymentWarmDuration = 120 * time.Second
	paymentCoolDuration = 180 * time.Second

	paymentHotMin = 2 * time.Second
	paymentHotMax = 4 * time.Second

	paymentWarmMin = 3 * time.Second
	paymentWarmMax = 6 * time.Second

	paymentCoolMin = 6 * time.Second
	paymentCoolMax = 10 * time.Second

	// Anti-abuse budgets: bounded continuous boost window per cycle
	// and a mandatory lockout cooldown to prevent infinite boost extortion.
	paymentHotBudget    = 5 * time.Minute
	paymentWarmBudget   = 15 * time.Minute
	paymentCoolBudget   = 30 * time.Minute
	paymentBoostLockout = 10 * time.Minute
)

type PollBoostProfile struct {
	Phase       PollBoostPhase
	Active      bool
	MinInterval time.Duration
	MaxInterval time.Duration
	NextPhaseAt time.Time
}

type PollBoostState struct {
	FirstPollNotBefore time.Time
	HotUntil           time.Time
	WarmUntil          time.Time
	CoolUntil          time.Time
	BoostStartedAt     time.Time
	LockoutUntil       time.Time
}

func (b *PollBoostState) Active(now time.Time) bool {
	return now.Before(b.CoolUntil)
}

func (b *PollBoostState) InLockout(now time.Time) bool {
	return !b.LockoutUntil.IsZero() && now.Before(b.LockoutUntil)
}

func (b *PollBoostState) Activate(now time.Time) {
	// If currently locked out due to budget exhaustion, refuse new activation
	if b.InLockout(now) {
		return
	}

	if !b.Active(now) {
		b.BoostStartedAt = now
		b.FirstPollNotBefore = now.Add(paymentGraceDuration)
	}

	hotUntil := now.Add(paymentHotDuration)
	maxHot := b.BoostStartedAt.Add(paymentHotBudget)
	if hotUntil.After(maxHot) {
		hotUntil = maxHot
	}
	if hotUntil.After(b.HotUntil) {
		b.HotUntil = hotUntil
	}

	warmUntil := now.Add(paymentWarmDuration)
	maxWarm := b.BoostStartedAt.Add(paymentWarmBudget)
	if warmUntil.After(maxWarm) {
		warmUntil = maxWarm
	}
	if warmUntil.After(b.WarmUntil) {
		b.WarmUntil = warmUntil
	}

	coolUntil := now.Add(paymentCoolDuration)
	maxCool := b.BoostStartedAt.Add(paymentCoolBudget)
	if coolUntil.After(maxCool) || coolUntil.Equal(maxCool) {
		coolUntil = maxCool
		// Cool reaches max continuous budget -> trigger mandatory lockout window
		b.LockoutUntil = maxCool.Add(paymentBoostLockout)
	}
	if coolUntil.After(b.CoolUntil) {
		b.CoolUntil = coolUntil
	}
}

func (b PollBoostState) Resolve(now time.Time) PollBoostProfile {
	if b.InLockout(now) {
		return PollBoostProfile{
			Phase:       PollBoostLocked,
			Active:      false,
			NextPhaseAt: b.LockoutUntil,
		}
	}

	if !b.Active(now) {
		return PollBoostProfile{
			Phase:  PollBoostIdle,
			Active: false,
		}
	}

	if now.Before(b.FirstPollNotBefore) {
		return PollBoostProfile{
			Phase:       PollBoostGrace,
			Active:      true,
			NextPhaseAt: b.FirstPollNotBefore,
		}
	}

	if now.Before(b.HotUntil) {
		return PollBoostProfile{
			Phase:       PollBoostHot,
			Active:      true,
			MinInterval: paymentHotMin,
			MaxInterval: paymentHotMax,
			NextPhaseAt: b.HotUntil,
		}
	}

	if now.Before(b.WarmUntil) {
		return PollBoostProfile{
			Phase:       PollBoostWarm,
			Active:      true,
			MinInterval: paymentWarmMin,
			MaxInterval: paymentWarmMax,
			NextPhaseAt: b.WarmUntil,
		}
	}

	return PollBoostProfile{
		Phase:       PollBoostCool,
		Active:      true,
		MinInterval: paymentCoolMin,
		MaxInterval: paymentCoolMax,
		NextPhaseAt: b.CoolUntil,
	}
}
