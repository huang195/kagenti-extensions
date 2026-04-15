package auth

import (
	"context"
	"sync"
	"time"
)

// CachedActorTokenSource wraps an ActorTokenSource with a single-entry cache.
// The cached token is refreshed 30 seconds before expiry, matching the
// old go-processor's actorTokenCache behavior.
type CachedActorTokenSource struct {
	source    ActorTokenSource
	mu        sync.RWMutex
	token     string
	expiresAt time.Time
	ttlBuffer time.Duration
}

// NewCachedActorTokenSource wraps the given source with caching.
func NewCachedActorTokenSource(source ActorTokenSource) *CachedActorTokenSource {
	return &CachedActorTokenSource{
		source:    source,
		ttlBuffer: 30 * time.Second,
	}
}

// FetchToken returns a cached actor token, refreshing if expired or near expiry.
func (c *CachedActorTokenSource) FetchToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	if c.token != "" && time.Now().Add(c.ttlBuffer).Before(c.expiresAt) {
		token := c.token
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if c.token != "" && time.Now().Add(c.ttlBuffer).Before(c.expiresAt) {
		return c.token, nil
	}

	token, err := c.source(ctx)
	if err != nil {
		return "", err
	}
	c.token = token
	// Always reset expiry on refresh. Default TTL of 5 minutes;
	// SetTTL can override with the actual expires_in from the grant.
	c.expiresAt = time.Now().Add(5 * time.Minute)
	return token, nil
}

// SetTTL updates the cache expiry based on the token's expires_in value.
// TODO: Wire from client-credentials response in HandleOutbound when actor token
// caching is enabled. Currently unused — the 5-minute default applies.
func (c *CachedActorTokenSource) SetTTL(expiresIn time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if expiresIn < 60*time.Second {
		expiresIn = 60 * time.Second
	}
	c.expiresAt = time.Now().Add(expiresIn)
}
