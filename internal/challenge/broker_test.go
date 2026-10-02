package challenge

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type fixtureBrowser struct{ observation authbrowser.AuthObservation }

func (f *fixtureBrowser) Observe(context.Context, string) (authbrowser.AuthObservation, error) {
	return f.observation, nil
}

type fixtureSender struct{ reminders atomic.Int32 }

func (*fixtureSender) SendChallenge(context.Context, storage.AuthChallenge, []byte) (int64, error) {
	return 456, nil
}
func (*fixtureSender) DeleteMessage(context.Context, int64, int64) error { return nil }
func (f *fixtureSender) SendText(context.Context, int64, string, any) (int64, error) {
	f.reminders.Add(1)
	return 457, nil
}

type fixtureSubmitter struct {
	submits atomic.Int32
	mu      sync.Mutex
	value   string
	fail    bool
}

func (f *fixtureSubmitter) SubmitChallenge(_ context.Context, _ storage.AuthChallenge, value string) (authbrowser.AuthObservation, error) {
	f.submits.Add(1)
	f.mu.Lock()
	f.value = value
	f.mu.Unlock()
	if f.fail {
		return authbrowser.AuthObservation{}, errors.New("synthetic uncertain network")
	}
	return authbrowser.AuthObservation{State: "AUTHENTICATED", Revision: "new"}, nil
}
func TestChallengeConsumeAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.TelegramAuthState(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, e.Generation, "STARTING", "WAITING_OTP", ""); err != nil {
		t.Fatal(err)
	}
	browser := &fixtureBrowser{observation: authbrowser.AuthObservation{State: "OTP_REQUIRED", Revision: "otp-one", ExpiresAt: time.Now().Add(time.Minute)}}
	sender := &fixtureSender{}
	submitter := &fixtureSubmitter{}
	b := &Broker{Store: s, Browser: browser, Sender: sender, Submitter: submitter, Config: Config{ChatID: 123, OTPTTL: 30 * time.Second}}
	challenge, err := b.Prompt(ctx, e, browser.observation, "OTP", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.HandleReply(ctx, 123, challenge.PromptMessageID, 999, "12xx"); !errors.Is(err, ErrInvalidResponse) {
			t.Fatal(err)
		}
	}
	if sender.reminders.Load() != 1 || submitter.submits.Load() != 0 {
		t.Fatal("invalid response submitted or repeated reminder")
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.HandleReply(ctx, 123, challenge.PromptMessageID, 999, " 001234 ") }()
	}
	wg.Wait()
	if submitter.submits.Load() != 1 || submitter.value != "001234" {
		t.Fatalf("submits=%d leading zero not preserved", submitter.submits.Load())
	}
	reopened, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	b.Store = reopened
	if err := b.HandleReply(ctx, 123, challenge.PromptMessageID, 999, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatalf("replay=%v", err)
	}
	if submitter.submits.Load() != 1 {
		t.Fatal("restart replayed OTP")
	}
	if err := reopened.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "WAIT_OPERATOR", "OTP_UNKNOWN", time.Time{}); err != nil {
		t.Fatal(err)
	}
}
func TestChallengeFormatPreservesCaseAndLeadingZeros(t *testing.T) {
	for _, tc := range []struct {
		kind, value string
		valid       bool
	}{{"OTP", "001234", true}, {"OTP", "１２３４", false}, {"OTP", "123", false}, {"OTP", "12345678901", false}, {"CAPTCHA_TEXT", "Ab12CD", true}, {"CAPTCHA_TEXT", "A B", false}, {"CAPTCHA_TEXT", "<script>", false}, {"CAPTCHA_TEXT", "", false}} {
		if got := ValidResponse(tc.kind, tc.value); got != tc.valid {
			t.Errorf("kind=%s validity=%t", tc.kind, got)
		}
	}
}
