package monitor

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type raceRegressionMockClient struct {
	calls atomic.Int32
}

func (m *raceRegressionMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *raceRegressionMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	c := m.calls.Add(1)
	body := `
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_next" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_RACE_` + string(rune('0'+(c%10))) + `</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

// TestMonitor_ConfigurationCannotRaceWithRunningScheduler ensures all With... mutators
// (WithEventNotifier, WithPollNotifier, WithSessionLoader, WithScheduler) and their readers
// can safely execute concurrently while tasks run on an active scheduler.
func TestMonitor_ConfigurationCannotRaceWithRunningScheduler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "race_regression.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &raceRegressionMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Task producers enqueueing work into the running scheduler
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					if workerID%2 == 0 {
						_ = sched.Enqueue(newTestCatchUpTask(mon, conn.ID, conn.Generation))
					} else {
						_ = sched.Enqueue(NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation))
					}
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(i)
	}

	// Concurrent mutators calling With... methods
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				mon.WithEventNotifier(func(events []storage.EventNotification) {})
				mon.WithEventNotifier(nil)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				mon.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {})
				mon.WithPollNotifier(nil)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		dummyLoader := NewSessionLoader(store, nil, nil)
		for {
			select {
			case <-stopCh:
				return
			default:
				mon.WithSessionLoader(dummyLoader)
				mon.WithSessionLoader(nil)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				s := mon.Scheduler()
				if s != nil {
					mon.WithScheduler(s)
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				_ = mon.SessionLoader()
				_ = mon.Scheduler()
				_ = mon.PersistSession(ctx)
				_ = mon.RequestSync(ctx)
			}
		}
	}()

	time.Sleep(250 * time.Millisecond)
	close(stopCh)
	wg.Wait()
}

// TestHistoryJobRunner_ConfigurationCannotRaceWithActiveRunner ensures HistoryJobRunner's
// With... mutators can be called safely and concurrently while the runner is active.
func TestHistoryJobRunner_ConfigurationCannotRaceWithActiveRunner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "hjr_race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	client := &raceRegressionMockClient{}
	sched := scheduler.New(nil)
	sched.Start(ctx)
	defer sched.Stop()

	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, sched, nil).WithMonitor(mon)

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				runner.WithMonitor(mon)
				runner.WithMonitor(nil)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				runner.WithStaleThreshold(30 * time.Second)
				runner.WithStaleThreshold(60 * time.Second)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				runner.WithPollInterval(2 * time.Second)
				runner.WithPollInterval(5 * time.Second)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				_ = runner.Monitor()
				_ = runner.StaleThreshold()
				_ = runner.PollInterval()
				_, _ = runner.RecoverStaleJobs(ctx)
			}
		}
	}()

	time.Sleep(250 * time.Millisecond)
	close(stopCh)
	wg.Wait()
}
