package git

import "sync"

// Locks serialises mirror fetch and worktree creation per repository while
// letting different repositories proceed in parallel.
type Locks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// NewLocks returns an empty lock registry.
func NewLocks() *Locks {
	return &Locks{locks: make(map[int64]*sync.Mutex)}
}

// Lock blocks until the repository lock is held and returns its unlock func.
func (l *Locks) Lock(repositoryID int64) func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	mutex, ok := l.locks[repositoryID]
	if !ok {
		mutex = &sync.Mutex{}
		l.locks[repositoryID] = mutex
	}
	l.mu.Unlock()

	mutex.Lock()
	return mutex.Unlock
}
