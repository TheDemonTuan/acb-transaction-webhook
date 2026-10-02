package authbrowser

import (
	"context"
	"errors"
	"sync"
	"time"
)

const ACBLoginURL = "https://online.acb.com.vn/acbib/Request"

type BrowserSession struct {
	AttemptID string `json:"attemptId"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expiresAt"`
}

type StartFunc func(context.Context, string) error
type StopFunc func(string) error

type Controller struct {
	mu      sync.Mutex
	active  string
	expires time.Time
	start   StartFunc
	stop    StopFunc
	ttl     time.Duration
	now     func() time.Time
}

func New(start StartFunc, stop StopFunc, ttl time.Duration) *Controller {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Controller{start: start, stop: stop, ttl: ttl, now: time.Now}
}

func (c *Controller) Start(ctx context.Context, attemptID string) (BrowserSession, error) {
	if attemptID == "" {
		return BrowserSession{}, errors.New("attempt ID is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != "" && c.now().Before(c.expires) {
		return BrowserSession{}, errors.New("another ACB browser session is active")
	}
	if c.start == nil {
		return BrowserSession{}, errors.New("browser runtime is unavailable")
	}
	if err := c.start(ctx, attemptID); err != nil {
		return BrowserSession{}, err
	}
	c.active = attemptID
	c.expires = c.now().Add(c.ttl)
	return BrowserSession{AttemptID: attemptID, Status: "AWAITING_USER_LOGIN", ExpiresAt: c.expires.UTC().Format(time.RFC3339)}, nil
}

func (c *Controller) Cancel(attemptID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != attemptID {
		return errors.New("ACB browser session not found")
	}
	if c.stop != nil {
		if err := c.stop(attemptID); err != nil {
			return err
		}
	}
	c.active = ""
	c.expires = time.Time{}
	return nil
}
