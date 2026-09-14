package integration_test

import (
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
)

// TestSingletonWorkerLockExclusivity proves Gate 7:
// The production ACB worker uses strict non-blocking file locking on gateway.lock.
// Two active workers can never hold the lock concurrently.
// During upgrade, Worker 1 yields/releases before Worker 2 can acquire.
func TestSingletonWorkerLockExclusivity(t *testing.T) {
	tempDir := t.TempDir()
	lockPath := filepath.Join(tempDir, "gateway.lock")

	// 1. Worker 1 acquires exclusive lock
	flock1, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatalf("worker 1 failed to acquire lock: %v", err)
	}
	defer flock1.Close()

	// 2. Candidate Worker 2 attempts to acquire lock while Worker 1 is running -> MUST FAIL
	flock2, err := lock.Acquire(lockPath)
	if err == nil {
		flock2.Close()
		t.Fatal("worker 2 unexpectedly acquired lock while worker 1 held it: split-brain invariant violated!")
	}

	// 3. Worker 1 receives quiesce signal and releases lock
	if err := flock1.Close(); err != nil {
		t.Fatalf("worker 1 failed to release lock cleanly: %v", err)
	}

	// 4. Candidate Worker 2 can now acquire exclusive lock
	flock2, err = lock.Acquire(lockPath)
	if err != nil {
		t.Fatalf("worker 2 failed to acquire lock after worker 1 quiesced: %v", err)
	}
	defer flock2.Close()

	// 5. Candidate Worker 2 now holds exclusive ownership; a 3rd worker cannot acquire
	flock3, err := lock.Acquire(lockPath)
	if err == nil {
		flock3.Close()
		t.Fatal("worker 3 unexpectedly acquired lock while worker 2 held it")
	}
}
