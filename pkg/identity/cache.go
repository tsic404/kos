package identity

import (
	"sync"
	"time"
)

// sessionCacheLimit bounds the in-memory session copy. It is not a second
// source of truth: the copy exists so an already signed-in read survives a
// database that stopped answering (C-61, C-63).
const sessionCacheLimit = 4096

// sessionCache keeps the last known state of issued sessions.
type sessionCache struct {
	mu      sync.Mutex
	entries map[string]Session
}

// newSessionCache builds an empty cache.
func newSessionCache() *sessionCache {
	return &sessionCache{entries: make(map[string]Session)}
}

// put records a session under its token hash.
func (c *sessionCache) put(hash string, session Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= sessionCacheLimit {
		c.prune(time.Now())
		if len(c.entries) >= sessionCacheLimit {
			clear(c.entries)
		}
	}
	c.entries[hash] = session
}

// get returns an unexpired copy.
func (c *sessionCache) get(hash string, now time.Time) (Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.entries[hash]
	if !ok {
		return Session{}, false
	}
	if !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
		delete(c.entries, hash)
		return Session{}, false
	}
	return session, true
}

// remove forgets one session.
func (c *sessionCache) remove(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
}

// removeUser forgets every session of one account.
func (c *sessionCache) removeUser(username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for hash, session := range c.entries {
		if session.Username == username {
			delete(c.entries, hash)
		}
	}
}

// prune drops expired copies. The caller holds the lock.
func (c *sessionCache) prune(now time.Time) {
	for hash, session := range c.entries {
		if !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
			delete(c.entries, hash)
		}
	}
}
