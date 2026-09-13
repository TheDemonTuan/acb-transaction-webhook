package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduleResolverBoundaries(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	settings := &MonitorSettings{
		Revision: 1,
		Enabled:  true,
		Timezone: "Asia/Ho_Chi_Minh",
		DefaultProfile: Profile{
			Mode:       ModeKeepaliveOnly,
			MinSeconds: 180,
			MaxSeconds: 300,
		},
		Windows: []Window{
			{
				Name:       "Giờ hành chính & ban ngày",
				DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
				StartTime:  "07:00",
				EndTime:    "23:00",
				Profile: Profile{
					Mode:       ModeRealtime,
					MinSeconds: 5,
					MaxSeconds: 15,
				},
			},
		},
	}

	tests := []struct {
		name     string
		hour     int
		minute   int
		wantMode PollMode
	}{
		{"06:59 is outside window -> KEEPALIVE_ONLY", 6, 59, ModeKeepaliveOnly},
		{"07:00 is exactly start -> REALTIME", 7, 0, ModeRealtime},
		{"12:30 is middle -> REALTIME", 12, 30, ModeRealtime},
		{"22:59 is inside -> REALTIME", 22, 59, ModeRealtime},
		{"23:00 is exactly end -> KEEPALIVE_ONLY", 23, 0, ModeKeepaliveOnly},
		{"02:00 is midnight -> KEEPALIVE_ONLY", 2, 0, ModeKeepaliveOnly},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkTime := time.Date(2026, 9, 12, tc.hour, tc.minute, 10, 0, loc)
			resolved := ResolveSchedule(checkTime, settings)
			if resolved.Mode != tc.wantMode {
				t.Errorf("at %02d:%02d: got mode %s, want %s", tc.hour, tc.minute, resolved.Mode, tc.wantMode)
			}
		})
	}
}

func TestScheduleOvernightWindow(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	settings := &MonitorSettings{
		Revision: 1,
		Enabled:  true,
		Timezone: "Asia/Ho_Chi_Minh",
		DefaultProfile: Profile{
			Mode:       ModePaused,
			MinSeconds: 0,
			MaxSeconds: 0,
		},
		Windows: []Window{
			{
				Name:       "Khung giờ đêm",
				DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
				StartTime:  "23:00",
				EndTime:    "06:00",
				Profile: Profile{
					Mode:       ModeKeepaliveOnly,
					MinSeconds: 180,
					MaxSeconds: 300,
				},
			},
		},
	}

	t1 := time.Date(2026, 9, 12, 23, 30, 0, 0, loc)
	res1 := ResolveSchedule(t1, settings)
	if res1.Mode != ModeKeepaliveOnly {
		t.Errorf("expected ModeKeepaliveOnly at 23:30, got %s", res1.Mode)
	}

	t2 := time.Date(2026, 9, 12, 4, 0, 0, 0, loc)
	res2 := ResolveSchedule(t2, settings)
	if res2.Mode != ModeKeepaliveOnly {
		t.Errorf("expected ModeKeepaliveOnly at 04:00, got %s", res2.Mode)
	}

	t3 := time.Date(2026, 9, 12, 10, 0, 0, 0, loc)
	res3 := ResolveSchedule(t3, settings)
	if res3.Mode != ModePaused {
		t.Errorf("expected ModePaused at 10:00, got %s", res3.Mode)
	}
}

func TestScheduleOvernightWindowSingleWeekday(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	// Window configured for Monday (1) only: 23:00 to 02:00
	settings := &MonitorSettings{
		Revision: 1,
		Enabled:  true,
		Timezone: "Asia/Ho_Chi_Minh",
		DefaultProfile: Profile{
			Mode:       ModePaused,
			MinSeconds: 0,
			MaxSeconds: 0,
		},
		Windows: []Window{
			{
				Name:       "Monday night shift",
				DaysOfWeek: []int{1}, // Monday only
				StartTime:  "23:00",
				EndTime:    "02:00",
				Profile: Profile{
					Mode:       ModeRealtime,
					MinSeconds: 5,
					MaxSeconds: 15,
				},
			},
		},
	}

	// 2026-09-14 is a Monday
	// Monday 23:30 should match Monday night shift -> ModeRealtime
	monEvening := time.Date(2026, 9, 14, 23, 30, 0, 0, loc)
	resMon := ResolveSchedule(monEvening, settings)
	if resMon.Mode != ModeRealtime {
		t.Errorf("Monday 23:30 should match ModeRealtime, got %s", resMon.Mode)
	}

	// Tuesday 01:30 (next morning of Monday night shift) should match -> ModeRealtime
	tueMorning := time.Date(2026, 9, 15, 1, 30, 0, 0, loc)
	resTue := ResolveSchedule(tueMorning, settings)
	if resTue.Mode != ModeRealtime {
		t.Errorf("Tuesday 01:30 (spanning from Monday) should match ModeRealtime, got %s", resTue.Mode)
	}

	// Tuesday 03:00 (after 02:00) should NOT match -> ModePaused
	tueAfter := time.Date(2026, 9, 15, 3, 0, 0, 0, loc)
	resTueAfter := ResolveSchedule(tueAfter, settings)
	if resTueAfter.Mode != ModePaused {
		t.Errorf("Tuesday 03:00 should be ModePaused, got %s", resTueAfter.Mode)
	}

	// Monday 01:30 (morning before Monday evening) should NOT match (Sunday was not in DaysOfWeek) -> ModePaused
	monEarlyMorning := time.Date(2026, 9, 14, 1, 30, 0, 0, loc)
	resMonEarly := ResolveSchedule(monEarlyMorning, settings)
	if resMonEarly.Mode != ModePaused {
		t.Errorf("Monday 01:30 should be ModePaused, got %s", resMonEarly.Mode)
	}
}

func TestDefaultMonitorSettingsIntervals(t *testing.T) {
	if got := DefaultMonitorSettings.DefaultProfile; got.Mode != ModeKeepaliveOnly || got.MinSeconds != 120 || got.MaxSeconds != 180 {
		t.Fatalf("unexpected default keepalive profile: %+v", got)
	}
	if len(DefaultMonitorSettings.Windows) != 1 {
		t.Fatalf("expected one default window, got %d", len(DefaultMonitorSettings.Windows))
	}
	if got := DefaultMonitorSettings.Windows[0].Profile; got.Mode != ModeRealtime || got.MinSeconds != 3 || got.MaxSeconds != 10 {
		t.Fatalf("unexpected default realtime profile: %+v", got)
	}
}

func TestScheduleValidationBounds(t *testing.T) {
	if err := validateProfile(Profile{Mode: ModeRealtime, MinSeconds: 3, MaxSeconds: 10}); err != nil {
		t.Errorf("expected 3-10 second REALTIME profile to be valid: %v", err)
	}

	// REALTIME minSeconds < 3 must fail
	pRealtimeTooLow := Profile{Mode: ModeRealtime, MinSeconds: 2, MaxSeconds: 10}
	if err := validateProfile(pRealtimeTooLow); err == nil {
		t.Errorf("expected error for REALTIME minSeconds < 3")
	}

	// REALTIME maxSeconds > 300 must fail
	pRealtimeTooHigh := Profile{Mode: ModeRealtime, MinSeconds: 5, MaxSeconds: 301}
	if err := validateProfile(pRealtimeTooHigh); err == nil {
		t.Errorf("expected error for REALTIME maxSeconds > 300")
	}

	// KEEPALIVE minSeconds < 60 must fail
	pKeepaliveTooLow := Profile{Mode: ModeKeepaliveOnly, MinSeconds: 59, MaxSeconds: 100}
	if err := validateProfile(pKeepaliveTooLow); err == nil {
		t.Errorf("expected error for KEEPALIVE minSeconds < 60")
	}

	// KEEPALIVE maxSeconds > 1800 must fail
	pKeepaliveTooHigh := Profile{Mode: ModeKeepaliveOnly, MinSeconds: 60, MaxSeconds: 1801}
	if err := validateProfile(pKeepaliveTooHigh); err == nil {
		t.Errorf("expected error for KEEPALIVE maxSeconds > 1800")
	}
}

func TestSaveMonitorSettingsOptimisticLocking(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_mon_settings.db")
	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	settings, err := store.GetMonitorSettings(ctx)
	if err != nil {
		t.Fatalf("GetMonitorSettings default: %v", err)
	}
	if settings.Revision != 1 {
		t.Errorf("expected default revision 1, got %d", settings.Revision)
	}

	// First save increments from default revision 1 to 2
	settings.Enabled = true
	saved, err := store.SaveMonitorSettings(ctx, settings)
	if err != nil {
		t.Fatalf("SaveMonitorSettings: %v", err)
	}
	if saved.Revision != 2 {
		t.Errorf("expected initial saved revision 2, got %d", saved.Revision)
	}

	// Second save with matching revision 2 succeeds and increments to 3
	saved.DefaultProfile.MinSeconds = 130
	saved2, err := store.SaveMonitorSettings(ctx, saved)
	if err != nil {
		t.Fatalf("SaveMonitorSettings second save: %v", err)
	}
	if saved2.Revision != 3 {
		t.Errorf("expected revision 3, got %d", saved2.Revision)
	}

	// Stale save with old revision 2 fails with ErrSettingsConflict (since DB is now 3)
	_, err = store.SaveMonitorSettings(ctx, saved)
	if err != ErrSettingsConflict {
		t.Errorf("expected ErrSettingsConflict for stale revision, got %v", err)
	}
}
