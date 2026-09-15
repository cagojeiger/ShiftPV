package capacity

import "sync"

// Locker is a keyed mutex: it serializes work for one key, such as a Pool
// capacity decision or a Volume lifecycle, while allowing independent keys to
// proceed concurrently.
type Locker struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func (l *Locker) Lock(pool string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*lockEntry)
	}
	entry := l.entries[pool]
	if entry == nil {
		entry = &lockEntry{}
		l.entries[pool] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, pool)
		}
		l.mu.Unlock()
	}
}
