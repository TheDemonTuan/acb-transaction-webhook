package storage

import (
	"errors"
	"fmt"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

type PollMode string

const (
	ModeRealtime      PollMode = "REALTIME"
	ModeKeepaliveOnly PollMode = "KEEPALIVE_ONLY"
	ModePaused        PollMode = "PAUSED"
)

type Profile struct {
	Mode       PollMode `json:"mode"`
	MinSeconds int      `json:"minSeconds"`
	MaxSeconds int      `json:"maxSeconds"`
}

type Window struct {
	Name       string  `json:"name"`
	DaysOfWeek []int   `json:"daysOfWeek"` // 0=Sunday, 1=Monday, ..., 6=Saturday
	StartTime  string  `json:"startTime"`  // HH:MM (e.g. "07:00")
	EndTime    string  `json:"endTime"`    // HH:MM (e.g. "23:00")
	Profile    Profile `json:"profile"`
}

type MonitorSettings struct {
	Revision       int64    `json:"revision"`
	Enabled        bool     `json:"enabled"`
	Timezone       string   `json:"timezone"`
	DefaultProfile Profile  `json:"defaultProfile"`
	Windows        []Window `json:"windows"`
	UpdatedAt      string   `json:"updatedAt"`
}

type ResolvedSchedule struct {
	Mode           PollMode      `json:"mode"`
	MinInterval    time.Duration `json:"minInterval"`
	MaxInterval    time.Duration `json:"maxInterval"`
	ActiveWindow   string        `json:"activeWindow,omitempty"`
	NextTransition time.Time     `json:"nextTransition"`
	NextMode       PollMode      `json:"nextMode"`
}

var DefaultMonitorSettings = MonitorSettings{
	Revision: 1,
	Enabled:  true,
	Timezone: "Asia/Ho_Chi_Minh",
	DefaultProfile: Profile{
		Mode:       ModeKeepaliveOnly,
		MinSeconds: 60,
		MaxSeconds: 120,
	},
	Windows: []Window{
		{
			Name:       "Giờ hoạt động thường ngày",
			DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
			StartTime:  "07:00",
			EndTime:    "23:00",
			Profile: Profile{
				Mode:       ModeRealtime,
				MinSeconds: 3,
				MaxSeconds: 10,
			},
		},
	},
}

// Validate validates the structure and logical consistency of monitor settings.
func (s *MonitorSettings) Validate() error {
	if s.Timezone == "" {
		s.Timezone = "Asia/Ho_Chi_Minh"
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", s.Timezone, err)
	}

	if err := validateProfile(s.DefaultProfile); err != nil {
		return fmt.Errorf("invalid default profile: %w", err)
	}

	for i, w := range s.Windows {
		if w.StartTime == "" || w.EndTime == "" {
			return fmt.Errorf("window %d (%s): start and end times required", i, w.Name)
		}
		if _, err := time.Parse("15:04", w.StartTime); err != nil {
			return fmt.Errorf("window %d (%s): invalid startTime format (HH:MM): %w", i, w.Name, err)
		}
		if _, err := time.Parse("15:04", w.EndTime); err != nil {
			return fmt.Errorf("window %d (%s): invalid endTime format (HH:MM): %w", i, w.Name, err)
		}
		if w.StartTime == w.EndTime {
			return fmt.Errorf("window %d (%s): startTime must not equal endTime", i, w.Name)
		}
		if len(w.DaysOfWeek) == 0 {
			return fmt.Errorf("window %d (%s): at least one day of week is required", i, w.Name)
		}
		for _, day := range w.DaysOfWeek {
			if day < 0 || day > 6 {
				return fmt.Errorf("window %d (%s): invalid day of week %d (0-6)", i, w.Name, day)
			}
		}
		if err := validateProfile(w.Profile); err != nil {
			return fmt.Errorf("window %d (%s): invalid profile: %w", i, w.Name, err)
		}
	}

	return nil
}

func validateProfile(p Profile) error {
	switch p.Mode {
	case ModeRealtime:
		if p.MinSeconds < 3 {
			return errors.New("REALTIME minSeconds must be at least 3")
		}
		if p.MaxSeconds > 300 {
			return errors.New("REALTIME maxSeconds must not exceed 300")
		}
		if p.MaxSeconds < p.MinSeconds {
			return errors.New("maxSeconds must be >= minSeconds")
		}
	case ModeKeepaliveOnly:
		if p.MinSeconds < 60 {
			return errors.New("KEEPALIVE minSeconds must be at least 60")
		}
		if p.MaxSeconds > 1800 {
			return errors.New("KEEPALIVE maxSeconds must not exceed 1800")
		}
		if p.MaxSeconds < p.MinSeconds {
			return errors.New("maxSeconds must be >= minSeconds")
		}
	case ModePaused:
	default:
		return fmt.Errorf("unknown mode %q", p.Mode)
	}
	return nil
}

// ResolveSchedule determines the current active mode, jitter bounds, and next transition boundary.
func ResolveSchedule(now time.Time, s *MonitorSettings) ResolvedSchedule {
	if s == nil {
		return ResolvedSchedule{
			Mode:        ModeRealtime,
			MinInterval: 3 * time.Second,
			MaxInterval: 12 * time.Second,
		}
	}

	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = acb.DefaultLocation
	}
	localNow := now.In(loc)

	if !s.Enabled {
		return ResolvedSchedule{
			Mode:           ModeRealtime,
			MinInterval:    5 * time.Second,
			MaxInterval:    15 * time.Second,
			ActiveWindow:   "Cấu hình mặc định (Realtime liên tục)",
			NextTransition: localNow.Add(24 * time.Hour),
			NextMode:       ModeRealtime,
		}
	}

	nowMinutes := localNow.Hour()*60 + localNow.Minute()
	nowWeekday := int(localNow.Weekday())

	var matchedWindow *Window
	for _, w := range s.Windows {
		startT, _ := time.Parse("15:04", w.StartTime)
		endT, _ := time.Parse("15:04", w.EndTime)
		startMin := startT.Hour()*60 + startT.Minute()
		endMin := endT.Hour()*60 + endT.Minute()

		isMatch := false
		if startMin < endMin {
			// Normal intraday window: must match today's weekday
			dayMatch := len(w.DaysOfWeek) == 0
			for _, d := range w.DaysOfWeek {
				if d == nowWeekday {
					dayMatch = true
					break
				}
			}
			if dayMatch && nowMinutes >= startMin && nowMinutes < endMin {
				isMatch = true
			}
		} else {
			// Overnight window across midnight (e.g. 23:00 -> 02:00)
			// Matches either:
			// 1. evening part (nowMinutes >= startMin) on window's starting day (nowWeekday)
			// 2. morning part (nowMinutes < endMin) on window's ending day (which started on yesterday: (nowWeekday+6)%7)
			yesterdayWeekday := (nowWeekday + 6) % 7
			dayMatchEvening := len(w.DaysOfWeek) == 0
			dayMatchMorning := len(w.DaysOfWeek) == 0
			for _, d := range w.DaysOfWeek {
				if d == nowWeekday {
					dayMatchEvening = true
				}
				if d == yesterdayWeekday {
					dayMatchMorning = true
				}
			}
			if (dayMatchEvening && nowMinutes >= startMin) || (dayMatchMorning && nowMinutes < endMin) {
				isMatch = true
			}
		}

		if isMatch {
			wCopy := w
			matchedWindow = &wCopy
			break
		}
	}

	activeProfile := s.DefaultProfile
	activeWindowName := "Mặc định (Ngoài khung giờ)"
	if matchedWindow != nil {
		activeProfile = matchedWindow.Profile
		activeWindowName = matchedWindow.Name
	}

	nextTransition := localNow.Add(24 * time.Hour)
	nextMode := s.DefaultProfile.Mode

	if matchedWindow != nil {
		endT, _ := time.Parse("15:04", matchedWindow.EndTime)
		endToday := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), endT.Hour(), endT.Minute(), 0, 0, loc)
		if endToday.After(localNow) {
			nextTransition = endToday
		} else {
			nextTransition = endToday.AddDate(0, 0, 1)
		}
		nextMode = s.DefaultProfile.Mode
	} else {
		var soonest time.Time
		for _, w := range s.Windows {
			startT, _ := time.Parse("15:04", w.StartTime)
			startToday := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), startT.Hour(), startT.Minute(), 0, 0, loc)
			candidate := startToday
			if !candidate.After(localNow) {
				candidate = candidate.AddDate(0, 0, 1)
			}
			if soonest.IsZero() || candidate.Before(soonest) {
				soonest = candidate
				nextMode = w.Profile.Mode
			}
		}
		if !soonest.IsZero() {
			nextTransition = soonest
		}
	}

	minDur := time.Duration(activeProfile.MinSeconds) * time.Second
	maxDur := time.Duration(activeProfile.MaxSeconds) * time.Second
	if minDur <= 0 {
		minDur = 5 * time.Second
	}
	if maxDur < minDur {
		maxDur = minDur
	}

	return ResolvedSchedule{
		Mode:           activeProfile.Mode,
		MinInterval:    minDur,
		MaxInterval:    maxDur,
		ActiveWindow:   activeWindowName,
		NextTransition: nextTransition,
		NextMode:       nextMode,
	}
}
