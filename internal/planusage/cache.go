package planusage

import (
	"sync"
	"time"
)

// Cache holds the last snapshot per key fingerprint.
type Cache struct {
	mu    sync.RWMutex
	items map[string]Snapshot
}

func NewCache() *Cache {
	return &Cache{items: make(map[string]Snapshot)}
}

func (c *Cache) Store(fp string, snap Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[fp] = snap
}

// StoreFailed keeps the last good windows (if any) and marks the snapshot stale.
func (c *Cache) StoreFailed(fp string, provider string, accounts []string, errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.items[fp]
	snap := Snapshot{
		Provider:  provider,
		Accounts:  accounts,
		FetchedAt: time.Now().UTC(),
		Err:       errText,
		Stale:     ok && len(prev.Windows) > 0,
	}
	if snap.Stale {
		snap.Windows = prev.Windows
	}
	c.items[fp] = snap
}

func (c *Cache) ForgetMissing(live map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for fp := range c.items {
		if _, ok := live[fp]; !ok {
			delete(c.items, fp)
		}
	}
}

func (c *Cache) List() []Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Snapshot, 0, len(c.items))
	for _, s := range c.items {
		out = append(out, s)
	}
	return out
}
