package resttunnel

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/xerrors"
)

// ErrBucketDoesNotExist is raised when attempting to fetch a bucket that does not exist
var ErrBucketDoesNotExist = xerrors.New("Bucket '%s' does not exist")

// ErrBucketCircularAlias is raised when a bucket references itself down a stack
var ErrBucketCircularAlias = xerrors.New("Bucket '%s' references itself. Stack: %v")

// Bucket represents a ratelimit bucket
type Bucket struct {
	mu sync.RWMutex

	Name  string
	Alias string

	// When handling the bucket lock, make sure you check the global bucket if it has
	// been defined.
	Global string

	Limit     *int32
	Duration  *int64
	ResetsAt  *int64
	Available *int32
}

// CreateBucket creates a new bucket
func CreateBucket(name string, limit int32, duration time.Duration, alias string, global string) (b *Bucket) {
	nanos := duration.Nanoseconds()
	return &Bucket{
		Name:     name,
		Alias:    alias,
		Global:   global,
		Limit:    &limit,
		Duration: &nanos,

		ResetsAt:  new(int64),
		Available: new(int32),
	}
}

// Lock waits until a bucket is ready.
func (b *Bucket) Lock() (hit bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := time.Now().UnixNano()

	// If we have passed reset, reset the available hits
	if atomic.LoadInt64(b.ResetsAt) <= now {
		atomic.StoreInt64(b.ResetsAt, now+atomic.LoadInt64(b.Duration))
		atomic.StoreInt32(b.Available, atomic.LoadInt32(b.Limit))
	}

	if atomic.LoadInt32(b.Available) <= 0 {
		sleepDuration := time.Duration(atomic.LoadInt64(b.ResetsAt) - now)
		hit = true
		time.Sleep(sleepDuration)
		b.Lock()
		return
	}

	atomic.AddInt32(b.Available, -1)
	return
}

// Exhaust removes all available calls and modifies the ResetAfter
func (b *Bucket) Exhaust(reset int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	atomic.StoreInt32(b.Available, 0)
	atomic.StoreInt64(b.ResetsAt, reset)
}

// Modify modifies the Limit, Duration etc
func (b *Bucket) Modify(limit int32, duration int64, alias string, global string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Alias = alias
	b.Global = global
	atomic.StoreInt32(b.Limit, limit)
	atomic.StoreInt64(b.Duration, duration)
}
