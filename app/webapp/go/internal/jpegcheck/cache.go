package jpegcheck

import (
	"bytes"
	"crypto/sha256"
	"expvar"
	"sync"
	"sync/atomic"
)

// Store dimensions, never image bytes. At most two generations of 2048
// successful validations are retained. The key covers the entire upload,
// including entropy-coded data: a header or image ID cannot prove validity.
const validationCacheGenerationSize = 2048

type dimensions struct{ width, height int }

type validationCache struct {
	mu                sync.RWMutex
	current, previous map[[sha256.Size]byte]dimensions
	generation        uint64
	limit             int
	hits, misses      atomic.Uint64
}

func newValidationCache(limit int) *validationCache {
	return &validationCache{current: make(map[[sha256.Size]byte]dimensions), limit: limit}
}

var uploadedJPEGs = newValidationCache(validationCacheGenerationSize)

func init() {
	expvar.Publish("jpeg_validation_cache", expvar.Func(func() any {
		return map[string]uint64{"hits": uploadedJPEGs.hits.Load(), "misses": uploadedJPEGs.misses.Load()}
	}))
}

func (c *validationCache) lookup(key [sha256.Size]byte) (dimensions, bool, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.current[key]
	if !ok {
		d, ok = c.previous[key]
	}
	return d, ok, c.generation
}

func (c *validationCache) remember(key [sha256.Size]byte, d dimensions, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A validation that started before initialize must not repopulate the cache.
	if generation != c.generation {
		return
	}
	if len(c.current) >= c.limit {
		c.previous = c.current
		c.current = make(map[[sha256.Size]byte]dimensions, c.limit)
	}
	c.current[key] = d
}

func (c *validationCache) validate(data []byte) (int, int, error) {
	key := sha256.Sum256(data)
	d, ok, generation := c.lookup(key)
	if ok {
		c.hits.Add(1)
		return d.width, d.height, nil
	}
	c.misses.Add(1)
	w, h, err := Validate(bytes.NewReader(data))
	if err == nil {
		c.remember(key, dimensions{w, h}, generation)
	}
	return w, h, err
}

// ValidateBytes applies Validate's acceptance rules and reuses only successful
// full validations of the same content. Callers still apply their own size
// limits and abuse flags. No user or database state participates in this cache.
func ValidateBytes(data []byte) (width, height int, err error) {
	return uploadedJPEGs.validate(data)
}

func (c *validationCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.current = make(map[[sha256.Size]byte]dimensions)
	c.previous = nil
	c.hits.Store(0)
	c.misses.Store(0)
}

// ResetValidationCache is called by the local and peer initialize paths.
func ResetValidationCache() { uploadedJPEGs.reset() }
