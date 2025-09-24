package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnectionLimiter manages connection limits for protocol servers
type ConnectionLimiter struct {
	maxConnections   int
	maxPerIP         int
	currentTotal     atomic.Int64
	perIPConnections map[string]*atomic.Int64
	mu               sync.RWMutex
	cleanupInterval  time.Duration
	protocol         string
	trustedNets      []*net.IPNet // Trusted networks that bypass per-IP limits
}

// NewConnectionLimiter creates a new connection limiter
func NewConnectionLimiter(protocol string, maxConnections, maxPerIP int) *ConnectionLimiter {
	return &ConnectionLimiter{
		maxConnections:   maxConnections,
		maxPerIP:         maxPerIP,
		perIPConnections: make(map[string]*atomic.Int64),
		cleanupInterval:  5 * time.Minute, // Clean up stale IP entries
		protocol:         protocol,
		trustedNets:      []*net.IPNet{},
	}
}

// NewConnectionLimiterWithTrustedNets creates a new connection limiter with trusted networks
func NewConnectionLimiterWithTrustedNets(protocol string, maxConnections, maxPerIP int, trustedProxies []string) *ConnectionLimiter {
	trustedNets, err := ParseTrustedNetworks(trustedProxies)
	if err != nil {
		log.Printf("[%s-LIMITER] WARNING: failed to parse trusted networks (%v), proceeding without trusted network exemptions", protocol, err)
		trustedNets = []*net.IPNet{}
	}

	return &ConnectionLimiter{
		maxConnections:   maxConnections,
		maxPerIP:         maxPerIP,
		perIPConnections: make(map[string]*atomic.Int64),
		cleanupInterval:  5 * time.Minute, // Clean up stale IP entries
		protocol:         protocol,
		trustedNets:      trustedNets,
	}
}

// isTrustedConnection checks if connection is from trusted network
func (cl *ConnectionLimiter) isTrustedConnection(remoteAddr net.Addr) bool {
	if len(cl.trustedNets) == 0 {
		return false
	}

	var ip net.IP
	switch addr := remoteAddr.(type) {
	case *net.TCPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	default:
		// Try to parse as string
		host, _, err := net.SplitHostPort(remoteAddr.String())
		if err != nil {
			return false
		}
		ip = net.ParseIP(host)
		if ip == nil {
			return false
		}
	}

	for _, network := range cl.trustedNets {
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

// CanAccept checks if a new connection can be accepted from the given remote address
func (cl *ConnectionLimiter) CanAccept(remoteAddr net.Addr) error {
	if cl.maxConnections <= 0 && cl.maxPerIP <= 0 {
		return nil // No limits configured
	}

	// Check total connection limit
	if cl.maxConnections > 0 {
		current := cl.currentTotal.Load()
		if current >= int64(cl.maxConnections) {
			return fmt.Errorf("maximum connections reached (%d/%d)", current, cl.maxConnections)
		}
	}

	// Check per-IP connection limit (skip if maxPerIP is 0, allowing unlimited per-IP for proxy scenarios)
	// Also skip per-IP limits for trusted networks (proxies)
	if cl.maxPerIP > 0 && !cl.isTrustedConnection(remoteAddr) {
		// Extract IP from remote address
		ip, _, err := net.SplitHostPort(remoteAddr.String())
		if err != nil {
			// Fallback to using the entire address if parsing fails
			ip = remoteAddr.String()
		}

		cl.mu.RLock()
		ipCounter, exists := cl.perIPConnections[ip]
		cl.mu.RUnlock()

		if exists {
			current := ipCounter.Load()
			if current >= int64(cl.maxPerIP) {
				return fmt.Errorf("maximum connections per IP reached for %s (%d/%d)", ip, current, cl.maxPerIP)
			}
		}
	}

	return nil
}

// Accept registers a new connection and returns a function to release it
func (cl *ConnectionLimiter) Accept(remoteAddr net.Addr) (func(), error) {
	err := cl.CanAccept(remoteAddr)
	if err != nil {
		return nil, err
	}

	// Extract IP from remote address
	ip, _, err := net.SplitHostPort(remoteAddr.String())
	if err != nil {
		ip = remoteAddr.String()
	}

	// Increment total counter
	total := cl.currentTotal.Add(1)

	var perIP int64 = 0
	var ipCounter *atomic.Int64
	isTrusted := cl.isTrustedConnection(remoteAddr)

	// Only track per-IP if limits are enabled (maxPerIP > 0) and not from trusted network
	if cl.maxPerIP > 0 && !isTrusted {
		// Increment per-IP counter
		cl.mu.Lock()
		var exists bool
		ipCounter, exists = cl.perIPConnections[ip]
		if !exists {
			ipCounter = &atomic.Int64{}
			cl.perIPConnections[ip] = ipCounter
		}
		cl.mu.Unlock()

		perIP = ipCounter.Add(1)

		log.Printf("[%s-LIMITER] Connection accepted from %s - Active connections: %d/%d total, %d/%d from this IP",
			cl.protocol, ip, total, cl.maxConnections, perIP, cl.maxPerIP)
	} else if isTrusted {
		log.Printf("[%s-LIMITER] Connection accepted from %s (trusted network) - Active connections: %d/%d total, unlimited from trusted IP",
			cl.protocol, ip, total, cl.maxConnections)
	} else {
		log.Printf("[%s-LIMITER] Connection accepted from %s - Active connections: %d/%d total, unlimited from this IP",
			cl.protocol, ip, total, cl.maxConnections)
	}

	// Return cleanup function
	return func() {
		cl.currentTotal.Add(-1)

		if cl.maxPerIP > 0 && !isTrusted && ipCounter != nil {
			remaining := ipCounter.Add(-1)

			// Clean up IP entry if no connections remain
			if remaining <= 0 {
				cl.mu.Lock()
				if ipCounter.Load() <= 0 {
					delete(cl.perIPConnections, ip)
				}
				cl.mu.Unlock()
			}

			log.Printf("[%s-LIMITER] Connection released from %s - Active connections remaining: %d total, %d from this IP",
				cl.protocol, ip, cl.currentTotal.Load(), remaining)
		} else {
			log.Printf("[%s-LIMITER] Connection released from %s - Active connections remaining: %d total, unlimited from this IP",
				cl.protocol, ip, cl.currentTotal.Load())
		}
	}, nil
}

// GetStats returns current connection statistics
func (cl *ConnectionLimiter) GetStats() ConnectionStats {
	cl.mu.RLock()
	defer cl.mu.RUnlock()

	stats := ConnectionStats{
		Protocol:         cl.protocol,
		TotalConnections: cl.currentTotal.Load(),
		MaxConnections:   int64(cl.maxConnections),
		MaxPerIP:         int64(cl.maxPerIP),
		IPConnections:    make(map[string]int64),
	}

	for ip, counter := range cl.perIPConnections {
		stats.IPConnections[ip] = counter.Load()
	}

	return stats
}

// StartCleanup starts a background goroutine to clean up stale IP entries
func (cl *ConnectionLimiter) StartCleanup(ctx context.Context) {
	if cl.cleanupInterval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(cl.cleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cl.cleanup()
			}
		}
	}()
}

// cleanup removes IP entries with zero connections
func (cl *ConnectionLimiter) cleanup() {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	cleaned := 0
	for ip, counter := range cl.perIPConnections {
		if counter.Load() <= 0 {
			delete(cl.perIPConnections, ip)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Printf("[%s-LIMITER] Cleaned up %d stale IP entries", cl.protocol, cleaned)
	}
}

// ConnectionStats represents connection statistics
type ConnectionStats struct {
	Protocol         string
	TotalConnections int64
	MaxConnections   int64
	MaxPerIP         int64
	IPConnections    map[string]int64
}
