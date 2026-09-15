package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Options configure the scheduler behavior.
type Options struct {
	MaxCapacity    int
	QuantumTimeout time.Duration
	Metrics        MetricsCollector
}

type delayedItem struct {
	task      UpstreamTask
	requeueAt time.Time
}

// Scheduler coordinates all upstream ACB requests using a single-owner priority queue.
type Scheduler struct {
	opts   Options
	queue  *TaskQueue
	stopCh chan struct{}
	notify chan struct{}
	wg     sync.WaitGroup

	running     atomic.Bool
	stopped     atomic.Bool
	paused      atomic.Bool
	dispatching atomic.Bool
	admissionMu sync.Mutex

	delayedMu sync.Mutex
	delayed   []delayedItem

	// Observability & current task tracking
	currentMu       sync.RWMutex
	currentKind     string
	currentPriority UpstreamPriority
	currentTaskID   string
	currentStarted  time.Time
	currentCancel   context.CancelFunc

	// Concurrency detection: must NEVER exceed 1
	activeGoroutines atomic.Int32
}

// New creates an unstarted Scheduler instance.
func New(opts *Options) *Scheduler {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.MaxCapacity <= 0 {
		o.MaxCapacity = 1000
	}
	if o.QuantumTimeout <= 0 {
		o.QuantumTimeout = 30 * time.Second
	}

	return &Scheduler{
		opts:    o,
		queue:   NewTaskQueue(o.MaxCapacity),
		stopCh:  make(chan struct{}),
		notify:  make(chan struct{}, 1),
		delayed: make([]delayedItem, 0),
	}
}

// Start launches the single scheduler loop goroutine.
func (s *Scheduler) Start(ctx context.Context) {
	if s.running.Swap(true) {
		return
	}
	s.wg.Add(1)
	go s.run(ctx)
}

// IsRunning reports whether the scheduler loop is actively running.
func (s *Scheduler) IsRunning() bool {
	return s.running.Load() && !s.stopped.Load()
}

// Stop shuts down the scheduler, cancels any in-flight task step, and waits for loop exit.
func (s *Scheduler) Stop() error {
	if !s.running.Load() {
		return nil
	}
	if s.stopped.Swap(true) {
		return nil
	}

	close(s.stopCh)

	// Cancel current in-flight task step
	s.currentMu.RLock()
	if s.currentCancel != nil {
		s.currentCancel()
	}
	s.currentMu.RUnlock()

	s.wg.Wait()
	return nil
}

// Pause halts the processing of queued tasks and cancels any currently running quantum so it yields promptly.
func (s *Scheduler) Pause() {
	s.admissionMu.Lock()
	s.paused.Store(true)
	s.admissionMu.Unlock()
}

// PauseAndDrain stops accepting new tasks and waits for the active step to
// finish without canceling its stateful upstream request.
func (s *Scheduler) PauseAndDrain(ctx context.Context) error {
	s.Pause()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !s.dispatching.Load() && s.CurrentTaskKind() == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return ErrSchedulerStopped
		case <-ticker.C:
		}
	}
}

// Resume unpauses the scheduler loop and signals readiness.
func (s *Scheduler) Resume() {
	s.admissionMu.Lock()
	s.paused.Store(false)
	s.admissionMu.Unlock()
	s.signalReady()
}

// IsPaused reports whether the scheduler is paused.
func (s *Scheduler) IsPaused() bool {
	return s.paused.Load()
}

// Enqueue submits an upstream task. Returns ErrQueueFull on overload, ErrSchedulerPaused if paused, or ErrSchedulerStopped if stopped.
func (s *Scheduler) Enqueue(task UpstreamTask) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.stopped.Load() {
		return ErrSchedulerStopped
	}
	if s.paused.Load() {
		return ErrSchedulerPaused
	}

	if s.coalesceDelayed(task) {
		return nil
	}

	err := s.queue.Push(task, time.Now())
	if err != nil {
		if s.opts.Metrics != nil && err == ErrQueueFull {
			s.opts.Metrics.OnQueueOverloaded(task.Kind(), task.Priority())
		}
		return err
	}

	if s.opts.Metrics != nil {
		s.opts.Metrics.OnTaskEnqueued(task.Kind(), task.Priority())
	}

	s.signalReady()
	return nil
}

// CancelTask cancels a task by ID or coalescing key if queued, or cancels its in-flight context.
func (s *Scheduler) CancelTask(id string) bool {
	removed := s.queue.Remove(id)

	s.delayedMu.Lock()
	for i, it := range s.delayed {
		if it.task.ID() == id || taskKey(it.task) == id {
			s.delayed = append(s.delayed[:i], s.delayed[i+1:]...)
			removed = true
			break
		}
	}
	s.delayedMu.Unlock()

	s.currentMu.RLock()
	if s.currentTaskID == id && s.currentCancel != nil {
		s.currentCancel()
		removed = true
	}
	s.currentMu.RUnlock()

	return removed
}

func (s *Scheduler) signalReady() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Scheduler) run(ctx context.Context) {
	defer s.wg.Done()
	defer s.running.Store(false)

	for {
		if s.stopped.Load() || ctx.Err() != nil {
			return
		}

		if s.paused.Load() {
			select {
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			case <-s.notify:
				continue
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		// 1. Promote any delayed tasks whose RequeueAt has expired
		nextWakeup := s.promoteDelayedTasks()

		// 2. Pop highest priority ready task
		task := s.queue.Pop()
		if task != nil {
			s.dispatching.Store(true)
			if s.paused.Load() {
				s.dispatching.Store(false)
				_ = s.queue.Push(task, time.Now())
				continue
			}
			s.executeQuantum(ctx, task)
			s.dispatching.Store(false)
			continue
		}

		// 3. Queue is empty; wait for notify, timer, stop, or ctx cancel
		var timerCh <-chan time.Time
		var timer *time.Timer
		if !nextWakeup.IsZero() {
			d := time.Until(nextWakeup)
			if d <= 0 {
				continue
			}
			timer = time.NewTimer(d)
			timerCh = timer.C
		}

		select {
		case <-s.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.notify:
			if timer != nil {
				timer.Stop()
			}
		case <-timerCh:
		}
	}
}

func (s *Scheduler) promoteDelayedTasks() time.Time {
	s.delayedMu.Lock()
	defer s.delayedMu.Unlock()

	now := time.Now()
	var earliest time.Time
	remaining := s.delayed[:0]

	for _, item := range s.delayed {
		if !item.requeueAt.After(now) {
			_ = s.queue.Push(item.task, item.requeueAt)
		} else {
			remaining = append(remaining, item)
			if earliest.IsZero() || item.requeueAt.Before(earliest) {
				earliest = item.requeueAt
			}
		}
	}
	s.delayed = remaining
	return earliest
}

func (s *Scheduler) coalesceDelayed(task UpstreamTask) bool {
	key := taskKey(task)
	s.delayedMu.Lock()
	defer s.delayedMu.Unlock()
	for i := range s.delayed {
		if taskKey(s.delayed[i].task) != key {
			continue
		}
		if task.Generation() > s.delayed[i].task.Generation() {
			s.delayed[i].task = task
		}
		return true
	}
	return false
}

func (s *Scheduler) scheduleDelayed(task UpstreamTask, requeueAt time.Time) {
	s.delayedMu.Lock()
	key := taskKey(task)
	for i := range s.delayed {
		if taskKey(s.delayed[i].task) != key {
			continue
		}
		if task.Generation() > s.delayed[i].task.Generation() {
			s.delayed[i].task = task
		}
		if requeueAt.After(s.delayed[i].requeueAt) {
			s.delayed[i].requeueAt = requeueAt
		}
		s.delayedMu.Unlock()
		s.signalReady()
		return
	}
	s.delayed = append(s.delayed, delayedItem{task: task, requeueAt: requeueAt})
	s.delayedMu.Unlock()
	s.signalReady()
}

func (s *Scheduler) executeQuantum(ctx context.Context, task UpstreamTask) {
	// Assert single active execution invariant
	curr := s.activeGoroutines.Add(1)
	if curr > 1 {
		s.activeGoroutines.Add(-1)
		panic("scheduler: invariant violation - concurrent upstream steps detected")
	}
	defer s.activeGoroutines.Add(-1)

	stepCtx, cancel := context.WithTimeout(ctx, s.opts.QuantumTimeout)
	defer cancel()

	s.currentMu.Lock()
	s.currentKind = task.Kind()
	s.currentPriority = task.Priority()
	s.currentTaskID = task.ID()
	s.currentStarted = time.Now()
	s.currentCancel = cancel
	s.currentMu.Unlock()

	if s.opts.Metrics != nil {
		s.opts.Metrics.OnTaskStarted(task.Kind(), task.Priority())
	}

	start := time.Now()
	res, err := task.Step(stepCtx)
	duration := time.Since(start)

	s.currentMu.Lock()
	s.currentKind = ""
	s.currentPriority = 0
	s.currentTaskID = ""
	s.currentStarted = time.Time{}
	s.currentCancel = nil
	s.currentMu.Unlock()

	outcome := res.Outcome
	if outcome == "" {
		outcome = ClassifyOutcome(err, nil)
	}
	res.Outcome = outcome

	if s.opts.Metrics != nil {
		if res.Done {
			s.opts.Metrics.OnTaskCompleted(task.Kind(), task.Priority(), duration, outcome, err)
		} else {
			s.opts.Metrics.OnTaskYielded(task.Kind(), task.Priority(), duration)
		}
	}

	// Requeue if task is not done and scheduler is still running
	if !res.Done && !s.stopped.Load() && ctx.Err() == nil {
		if !res.RequeueAt.IsZero() && res.RequeueAt.After(time.Now()) {
			s.scheduleDelayed(task, res.RequeueAt)
		} else {
			_ = s.queue.Push(task, time.Now())
		}
	}
}

// QueueDepth returns the number of queued tasks with priority p.
func (s *Scheduler) QueueDepth(p UpstreamPriority) int {
	return s.queue.DepthByPriority(p)
}

// TotalQueueDepth returns total queued tasks.
func (s *Scheduler) TotalQueueDepth() int {
	return s.queue.Len()
}

// CurrentTaskKind returns the kind of the currently running task, or empty string.
func (s *Scheduler) CurrentTaskKind() string {
	s.currentMu.RLock()
	defer s.currentMu.RUnlock()
	return s.currentKind
}

// CurrentTaskPriority returns the priority of the currently running task.
func (s *Scheduler) CurrentTaskPriority() UpstreamPriority {
	s.currentMu.RLock()
	defer s.currentMu.RUnlock()
	return s.currentPriority
}

// CurrentTaskDuration returns elapsed duration of currently running task, or 0.
func (s *Scheduler) CurrentTaskDuration() time.Duration {
	s.currentMu.RLock()
	defer s.currentMu.RUnlock()
	if s.currentStarted.IsZero() {
		return 0
	}
	return time.Since(s.currentStarted)
}

// IsBusy reports true if a task is currently executing a quantum.
func (s *Scheduler) IsBusy() bool {
	return s.activeGoroutines.Load() > 0
}
