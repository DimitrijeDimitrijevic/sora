package metrics

import (
	"context"
	"log"
	"time"
)

// MetricsStats holds aggregate statistics returned by the database
type MetricsStats struct {
	TotalAccounts  int64
	TotalMailboxes int64
	TotalMessages  int64
}

// StatsProvider is an interface for retrieving metrics statistics
type StatsProvider interface {
	GetMetricsStatsWithRetry(ctx context.Context) (*MetricsStats, error)
}

// CacheStatsProvider is an interface for cache statistics
type CacheStatsProvider interface {
	GetStats() (objectCount int64, totalSize int64, err error)
}

// Collector periodically collects and updates database-backed metrics
type Collector struct {
	provider      StatsProvider
	cacheProvider CacheStatsProvider
	interval      time.Duration
	stopCh        chan struct{}
}

// NewCollector creates a new metrics collector
func NewCollector(provider StatsProvider, interval time.Duration) *Collector {
	if interval == 0 {
		interval = 60 * time.Second // Default to 60 seconds
	}

	return &Collector{
		provider: provider,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// NewCollectorWithCache creates a new metrics collector with cache support
func NewCollectorWithCache(provider StatsProvider, cacheProvider CacheStatsProvider, interval time.Duration) *Collector {
	if interval == 0 {
		interval = 60 * time.Second // Default to 60 seconds
	}

	return &Collector{
		provider:      provider,
		cacheProvider: cacheProvider,
		interval:      interval,
		stopCh:        make(chan struct{}),
	}
}

// Start begins the metrics collection loop
func (c *Collector) Start(ctx context.Context) {
	// Collect immediately on start
	c.collect(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	log.Printf("[METRICS-COLLECTOR] Started with interval %v", c.interval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[METRICS-COLLECTOR] Stopping due to context cancellation")
			return
		case <-c.stopCh:
			log.Printf("[METRICS-COLLECTOR] Stopping due to stop signal")
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

// Stop signals the collector to stop
func (c *Collector) Stop() {
	close(c.stopCh)
}

// collect retrieves and updates all metrics
func (c *Collector) collect(ctx context.Context) {
	stats, err := c.provider.GetMetricsStatsWithRetry(ctx)
	if err != nil {
		log.Printf("[METRICS-COLLECTOR] Error collecting metrics: %v", err)
		return
	}

	// Update Prometheus gauges
	AccountsTotal.Set(float64(stats.TotalAccounts))
	MailboxesTotal.Set(float64(stats.TotalMailboxes))
	// Note: MessagesTotal is per-mailbox, so we only update the total here
	// Individual mailbox metrics would require more complex queries
	// For now, we'll skip the per-mailbox metric

	log.Printf("[METRICS-COLLECTOR] Updated DB metrics: accounts=%d, mailboxes=%d, messages=%d",
		stats.TotalAccounts, stats.TotalMailboxes, stats.TotalMessages)

	// Update cache metrics if cache provider is available
	if c.cacheProvider != nil {
		objectCount, totalSize, err := c.cacheProvider.GetStats()
		if err != nil {
			log.Printf("[METRICS-COLLECTOR] Error collecting cache metrics: %v", err)
		} else {
			CacheObjectsTotal.Set(float64(objectCount))
			CacheSizeBytes.Set(float64(totalSize))
			log.Printf("[METRICS-COLLECTOR] Updated cache metrics: objects=%d, size_bytes=%d",
				objectCount, totalSize)
		}
	}
}
