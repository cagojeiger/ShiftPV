package controller

import "sync"

type volumeLifecycles struct {
	mu      sync.Mutex
	entries map[string]*volumeLifecycle
}

type volumeLifecycle struct {
	mu   sync.Mutex
	refs int
}

func (l *volumeLifecycles) lock(volumeID string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*volumeLifecycle)
	}
	entry := l.entries[volumeID]
	if entry == nil {
		entry = &volumeLifecycle{}
		l.entries[volumeID] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, volumeID)
		}
		l.mu.Unlock()
	}
}
