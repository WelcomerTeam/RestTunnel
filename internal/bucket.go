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
var ErrBucketCircularAlias = xerrors.New("Bucket '%s' references itself in bucket '%s'")

// Bucket represents a ratelimit bucket
type Bucket struct {
	mu sync.RWMutex

	name  string
	alias string

	limit     *int32
	duration  *int64
	ResetsAt  *int64
	Available *int32
}

// CreateBucket creates a new bucket
func CreateBucket(name string, limit int32, duration time.Duration) (b *Bucket) {
	nanos := duration.Nanoseconds()
	return &Bucket{
		name:     name,
		limit:    &limit,
		duration: &nanos,

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
		atomic.StoreInt64(b.ResetsAt, now+atomic.LoadInt64(b.duration))
		atomic.StoreInt32(b.Available, atomic.LoadInt32(b.limit))
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
