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
// Unauthorized / no_subscription (401/403) clear windows and are not stale:
// the previous plan is no longer a valid display.
//
// failed is the failed round's snapshot: it already carries the provider, the
// account names AND their aligned per-account fingerprints (the poller stamps
// all three with AssignAccountViews before fetching). They are carried over
// verbatim on purpose — a merged ClinePass row's identity and colour are built
// from them, so rebuilding the snapshot without the fingerprints would make a
// failed round degrade to the bare ·, an uncoloured account name and a
// position-keyed row while a successful round of the same group does not.
// errText is the English error code (see ErrorCode).
func (c *Cache) StoreFailed(fp string, failed Snapshot, errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.items[fp]
	keepWindows := ok && len(prev.Windows) > 0 && !clearsQuotaWindows(errText)
	out := failed
	out.FetchedAt = time.Now().UTC()
	out.Err = errText
	out.Stale = keepWindows
	// A failed fetch has no windows of its own: keep the last good ones
	// (stale) or clear them (401/403). AccountFPs travel with Accounts.
	out.Windows = nil
	if keepWindows {
		out.Windows = prev.Windows
	}
	c.items[fp] = out
}

func clearsQuotaWindows(errText string) bool {
	switch errText {
	case "unauthorized", "no_subscription":
		return true
	default:
		return false
	}
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
