package scheduler

import (
	"container/heap"
	"sync"
	"time"
)

type queueItem struct {
	task        UpstreamTask
	priority    UpstreamPriority
	coalesceKey string
	enqueuedAt  time.Time
	seq         uint64
	index       int
}

type priorityHeap []*queueItem

func (h priorityHeap) Len() int { return len(h) }

func (h priorityHeap) Less(i, j int) bool {
	// 1. Strict priority between classes: higher priority always comes first.
	if h[i].priority != h[j].priority {
		return h[i].priority > h[j].priority
	}
	// 2. Aging ONLY within class: earliest enqueuedAt comes first (FIFO).
	if !h[i].enqueuedAt.Equal(h[j].enqueuedAt) {
		return h[i].enqueuedAt.Before(h[j].enqueuedAt)
	}
	// 3. Deterministic tie-breaker within identical timestamp.
	return h[i].seq < h[j].seq
}

func (h priorityHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *priorityHeap) Push(x any) {
	n := len(*h)
	item := x.(*queueItem)
	item.index = n
	*h = append(*h, item)
}

func (h *priorityHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[0 : n-1]
	return item
}

// TaskQueue provides a thread-safe priority queue with class-isolated aging,
// deduplication/coalescing, and bounded capacity.
type TaskQueue struct {
	mu          sync.Mutex
	h           priorityHeap
	byKey       map[string]*queueItem
	maxCapacity int
	seqCounter  uint64
}

// NewTaskQueue creates an empty TaskQueue with bounded capacity.
func NewTaskQueue(maxCapacity int) *TaskQueue {
	if maxCapacity <= 0 {
		maxCapacity = 1000
	}
	q := &TaskQueue{
		h:           make(priorityHeap, 0),
		byKey:       make(map[string]*queueItem),
		maxCapacity: maxCapacity,
	}
	heap.Init(&q.h)
	return q
}

func taskKey(task UpstreamTask) string {
	if c, ok := task.(CoalescingTask); ok {
		if k := c.CoalesceKey(); k != "" {
			return k
		}
	}
	return task.ID()
}

// Push inserts a task into the queue. If a task with the same coalesce key is
// already queued, it coalesces by updating if the new task has higher generation,
// or ignoring if older/equal. Returns ErrQueueFull when capacity is exceeded.
func (q *TaskQueue) Push(task UpstreamTask, enqueuedAt time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	key := taskKey(task)

	// Check if already queued for coalescing.
	if existing, ok := q.byKey[key]; ok {
		if task.Generation() > existing.task.Generation() {
			existing.task = task
			existing.priority = task.Priority()
			heap.Fix(&q.h, existing.index)
		}
		return nil
	}

	if len(q.h) >= q.maxCapacity {
		return ErrQueueFull
	}

	q.seqCounter++
	item := &queueItem{
		task:        task,
		priority:    task.Priority(),
		coalesceKey: key,
		enqueuedAt:  enqueuedAt,
		seq:         q.seqCounter,
	}
	heap.Push(&q.h, item)
	q.byKey[key] = item
	return nil
}

// Pop extracts the highest priority task (with aging applied strictly within its class).
// Returns nil if queue is empty.
func (q *TaskQueue) Pop() UpstreamTask {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.h) == 0 {
		return nil
	}

	item := heap.Pop(&q.h).(*queueItem)
	delete(q.byKey, item.coalesceKey)
	return item.task
}

// Remove removes a queued task by its coalesce key or ID. Returns true if removed.
func (q *TaskQueue) Remove(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	item, ok := q.byKey[key]
	if !ok {
		return false
	}

	heap.Remove(&q.h, item.index)
	delete(q.byKey, key)
	return true
}

// Len returns total number of queued tasks.
func (q *TaskQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.h)
}

// DepthByPriority returns the number of queued tasks with the given priority.
func (q *TaskQueue) DepthByPriority(p UpstreamPriority) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	count := 0
	for _, item := range q.h {
		if item.priority == p {
			count++
		}
	}
	return count
}
