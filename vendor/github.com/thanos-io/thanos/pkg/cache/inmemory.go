// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package cache

import (
	"context"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	lru "github.com/hashicorp/golang-lru/v2/simplelru"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"gopkg.in/yaml.v2"

	"github.com/thanos-io/thanos/pkg/model"
)

var (
	DefaultInMemoryCacheConfig = InMemoryCacheConfig{
		MaxSize:     250 * 1024 * 1024,
		MaxItemSize: 125 * 1024 * 1024,
	}
)

const (
	maxInt = int(^uint(0) >> 1)
)

// InMemoryCacheConfig holds the in-memory cache config.
type InMemoryCacheConfig struct {
	// MaxSize represents overall maximum number of bytes cache can contain.
	MaxSize model.Bytes `yaml:"max_size"`
	// MaxItemSize represents maximum size of single item.
	MaxItemSize model.Bytes `yaml:"max_item_size"`
}

type InMemoryCache struct {
	logger           log.Logger
	maxSizeBytes     uint64
	maxItemSizeBytes uint64
	name             string

	mtx         sync.Mutex
	curSize     uint64
	lru         *lru.LRU[string, cacheDataWithTTLWrapper]
	evicted     prometheus.Counter
	requests    prometheus.Counter
	hits        prometheus.Counter
	hitsExpired prometheus.Counter
	// The input cache value would be copied to an inmemory array
	// instead of simply using the one sent by the caller.
	added            prometheus.Counter
	current          prometheus.Gauge
	currentSize      prometheus.Gauge
	totalCurrentSize prometheus.Gauge
	overflow         prometheus.Counter
}

type cacheDataWithTTLWrapper struct {
	data []byte
	// Items exceeding their Time-To-Live (TTL) are not immediately removed from the cache.
	// Instead, when an access attempt is made for an item past its TTL, the item is evicted from the cache, and a null value is returned.
	// Efforts are underway to incorporate TTL directly into the Hashicorp golang cache.
	// Although this pull request (https://github.com/hashicorp/golang-lru/pull/41) has been completed, it's challenging to apply here due to the following reasons:
	// The Hashicorp LRU API requires setting the TTL during the constructor phase, whereas in Thanos, we set the TTL for each Set()/Store() operation.
	// Refer to this link for more details: https://github.com/thanos-io/thanos/blob/23d205286436291fa0c55c25c392ee08f42d5fbf/pkg/store/cache/caching_bucket.go#L167-L175
	expiryTime time.Time
}

// parseInMemoryCacheConfig unmarshals a buffer into a InMemoryCacheConfig with default values.
func parseInMemoryCacheConfig(conf []byte) (InMemoryCacheConfig, error) {
	config := DefaultInMemoryCacheConfig
	if err := yaml.Unmarshal(conf, &config); err != nil {
		return InMemoryCacheConfig{}, err
	}

	return config, nil
}

// NewInMemoryCache creates a new thread-safe LRU cache and ensures the total cache
// size approximately does not exceed maxBytes.
func NewInMemoryCache(name string, logger log.Logger, reg prometheus.Registerer, conf []byte) (*InMemoryCache, error) {
	config, err := parseInMemoryCacheConfig(conf)
	if err != nil {
		return nil, err
	}

	return NewInMemoryCacheWithConfig(name, logger, reg, config)
}

// NewInMemoryCacheWithConfig creates a new thread-safe LRU cache and ensures the total cache
// size approximately does not exceed maxBytes.
func NewInMemoryCacheWithConfig(name string, logger log.Logger, reg prometheus.Registerer, config InMemoryCacheConfig) (*InMemoryCache, error) {
	if config.MaxItemSize > config.MaxSize {
		return nil, errors.Errorf("max item size (%v) cannot be bigger than overall cache size (%v)", config.MaxItemSize, config.MaxSize)
	}

	c := &InMemoryCache{
		logger:           logger,
		maxSizeBytes:     uint64(config.MaxSize),
		maxItemSizeBytes: uint64(config.MaxItemSize),
		name:             name,
	}

	c.evicted = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_items_evicted_total",
		Help:        "Total number of items that were evicted from the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.added = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_items_added_total",
		Help:        "Total number of items that were added to the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.requests = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_requests_total",
		Help:        "Total number of requests to the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.hitsExpired = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_hits_on_expired_data_total",
		Help:        "Total number of requests to the inmemory cache that were a hit but needed to be evicted due to TTL.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.overflow = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_items_overflowed_total",
		Help:        "Total number of items that could not be added to the inmemory cache due to being too big.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.hits = promauto.With(reg).NewCounter(prometheus.CounterOpts{
		Name:        "thanos_cache_inmemory_hits_total",
		Help:        "Total number of requests to the inmemory cache that were a hit.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.current = promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name:        "thanos_cache_inmemory_items",
		Help:        "Current number of items in the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.currentSize = promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name:        "thanos_cache_inmemory_items_size_bytes",
		Help:        "Current byte size of items in the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	c.totalCurrentSize = promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name:        "thanos_cache_inmemory_total_size_bytes",
		Help:        "Current byte size of items (both value and key) in the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	})

	_ = promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "thanos_cache_inmemory_max_size_bytes",
		Help:        "Maximum number of bytes to be held in the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	}, func() float64 {
		return float64(c.maxSizeBytes)
	})
	_ = promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "thanos_cache_inmemory_max_item_size_bytes",
		Help:        "Maximum number of bytes for single entry to be held in the inmemory cache.",
		ConstLabels: prometheus.Labels{"name": name},
	}, func() float64 {
		return float64(c.maxItemSizeBytes)
	})

	// Initialize LRU cache with a high size limit since we will manage evictions ourselves
	// based on stored size using `RemoveOldest` method.
	l, err := lru.NewLRU[string, cacheDataWithTTLWrapper](maxInt, c.onEvict)
	if err != nil {
		return nil, err
	}
	c.lru = l

	level.Info(logger).Log(
		"msg", "created in-memory inmemory cache",
		"maxItemSizeBytes", c.maxItemSizeBytes,
		"maxSizeBytes", c.maxSizeBytes,
		"maxItems", "maxInt",
	)
	return c, nil
}

func (c *InMemoryCache) onEvict(key string, val cacheDataWithTTLWrapper) {
	keySize := uint64(len(key))
	entrySize := uint64(len(val.data))

	c.evicted.Inc()
	c.current.Dec()
	c.currentSize.Sub(float64(entrySize))
	c.totalCurrentSize.Sub(float64(keySize + entrySize))

	c.curSize -= entrySize
}

func (c *InMemoryCache) get(key string) ([]byte, bool) {
	c.requests.Inc()
	c.mtx.Lock()
	defer c.mtx.Unlock()

	v, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}
	// If the present time is greater than the TTL for the object from cache, the object will be
	// removed from the cache and a nil will be returned
	if time.Now().After(v.expiryTime) {
		c.hitsExpired.Inc()
		c.lru.Remove(key)
		return nil, false
	}
	c.hits.Inc()
	return v.data, true
}

func (c *InMemoryCache) set(key string, val []byte, ttl time.Duration) {
	var size = uint64(len(val))
	keySize := uint64(len(key))

	c.mtx.Lock()
	defer c.mtx.Unlock()

	if _, ok := c.lru.Get(key); ok {
		return
	}

	if !c.ensureFits(size) {
		c.overflow.Inc()
		return
	}

	// The caller may be passing in a sub-slice of a huge array. Copy the data
	// to ensure we don't waste huge amounts of space for something small.
	v := make([]byte, len(val))
	copy(v, val)
	c.lru.Add(key, cacheDataWithTTLWrapper{data: v, expiryTime: time.Now().Add(ttl)})

	c.added.Inc()
	c.currentSize.Add(float64(size))
	c.totalCurrentSize.Add(float64(keySize + size))
	c.current.Inc()
	c.curSize += size
}

// ensureFits tries to make sure that the passed slice will fit into the LRU cache.
// Returns true if it will fit.
func (c *InMemoryCache) ensureFits(size uint64) bool {
	if size > c.maxItemSizeBytes {
		level.Debug(c.logger).Log(
			"msg", "item bigger than maxItemSizeBytes. Ignoring..",
			"maxItemSizeBytes", c.maxItemSizeBytes,
			"maxSizeBytes", c.maxSizeBytes,
			"curSize", c.curSize,
			"itemSize", size,
		)
		return false
	}

	for c.curSize+size > c.maxSizeBytes {
		if _, _, ok := c.lru.RemoveOldest(); !ok {
			level.Error(c.logger).Log(
				"msg", "LRU has nothing more to evict, but we still cannot allocate the item. Resetting cache.",
				"maxItemSizeBytes", c.maxItemSizeBytes,
				"maxSizeBytes", c.maxSizeBytes,
				"curSize", c.curSize,
				"itemSize", size,
			)
			c.reset()
		}
	}
	return true
}

func (c *InMemoryCache) reset() {
	c.lru.Purge()
	c.current.Set(0)
	c.currentSize.Set(0)
	c.totalCurrentSize.Set(0)
	c.curSize = 0
}

func (c *InMemoryCache) Store(data map[string][]byte, ttl time.Duration) {
	for key, val := range data {
		c.set(key, val, ttl)
	}
}

// Fetch fetches multiple keys and returns a map containing cache hits
// In case of error, it logs and return an empty cache hits map.
func (c *InMemoryCache) Fetch(ctx context.Context, keys []string) map[string][]byte {
	results := make(map[string][]byte)
	for _, key := range keys {
		if b, ok := c.get(key); ok {
			results[key] = b
		}
	}
	return results
}

func (c *InMemoryCache) Name() string {
	return c.name
}
