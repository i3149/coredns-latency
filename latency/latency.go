package latency

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"github.com/redis/go-redis/v9"
)

var log = clog.NewWithPlugin("latency")

// RedisKeyFormat defines how latency keys are stored in Redis.
//
// Two layouts are supported (configured via `key_format`):
//
//  1. sorted_set (default)
//     Key  : "latency:<fqdn>"          e.g. "latency:api.example.com."
//     Type : Redis Sorted Set
//     Score: latency in milliseconds (float64)
//     Member: IP address string        e.g. "10.0.0.1"
//
//     ZADD latency:api.example.com. 12.5 10.0.0.1
//     ZADD latency:api.example.com.  8.3 10.0.0.2
//
//  2. hash
//     Key  : "latency:<fqdn>"
//     Type : Redis Hash
//     Field: IP address string
//     Value: latency in milliseconds (string-encoded float) e.g. "12.5"
//
//     HSET latency:api.example.com. 10.0.0.1 12.5
//     HSET latency:api.example.com. 10.0.0.2  8.3

type keyFormat int

const (
	sortedSet keyFormat = iota
	hashMap
)

// LatencyPlugin is the main plugin struct.
type LatencyPlugin struct {
	Next plugin.Handler

	rdb       *redis.Client
	keyPrefix string    // default: "latency:"
	format    keyFormat // sortedSet | hashMap
	ttl       uint32    // TTL for synthesised A/AAAA records (seconds)
	fallback  bool      // pass through to next plugin when no Redis data found
	zones     []string  // zones this plugin is authoritative for; empty = all
}

// Name implements plugin.Handler.
func (lp *LatencyPlugin) Name() string { return "latency" }

// ServeDNS implements plugin.Handler.
func (lp *LatencyPlugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.Name() // always fully-qualified with trailing dot
	qtype := state.QType()

	// Only handle A and AAAA queries.
	if qtype != dns.TypeA && qtype != dns.TypeAAAA {
		return plugin.NextOrFailure(lp.Name(), lp.Next, ctx, w, r)
	}

	// Zone check – skip if configured zones don't match.
	if len(lp.zones) > 0 {
		zName := plugin.Zones(lp.zones).Matches(qname)
		if zName == "" {
			return plugin.NextOrFailure(lp.Name(), lp.Next, ctx, w, r)
		}
	}

	start := time.Now()

	ip, err := lp.lowestLatencyIP(ctx, qname)
	if err != nil || ip == nil {
		if lp.fallback {
			log.Debugf("no latency data for %s, falling through", qname)
			return plugin.NextOrFailure(lp.Name(), lp.Next, ctx, w, r)
		}
		log.Warningf("no latency data for %s and fallback disabled: %v", qname, err)
		return dns.RcodeServerFailure, err
	}

	elapsed := time.Since(start)
	log.Debugf("resolved %s → %s (redis lookup: %s)", qname, ip, elapsed)

	// Record metrics.
	requestCount.WithLabelValues(metrics.WithServer(ctx)).Inc()
	redisLookupDuration.WithLabelValues(metrics.WithServer(ctx)).Observe(elapsed.Seconds())

	msg := buildResponse(r, qname, ip, lp.ttl, qtype)
	if err := w.WriteMsg(msg); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

// lowestLatencyIP queries Redis for the IP with the smallest latency score.
func (lp *LatencyPlugin) lowestLatencyIP(ctx context.Context, fqdn string) (net.IP, error) {
	key := lp.keyPrefix + fqdn

	switch lp.format {
	case sortedSet:
		return lp.fromSortedSet(ctx, key)
	case hashMap:
		return lp.fromHash(ctx, key)
	default:
		return nil, fmt.Errorf("unknown key format %d", lp.format)
	}
}

// fromSortedSet uses ZRANGE key 0 0 (lowest score first).
func (lp *LatencyPlugin) fromSortedSet(ctx context.Context, key string) (net.IP, error) {
	results, err := lp.rdb.ZRangeWithScores(ctx, key, 0, 0).Result()
	if err != nil {
		return nil, fmt.Errorf("ZRANGE %s: %w", key, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("sorted set %q is empty or does not exist", key)
	}

	ipStr, ok := results[0].Member.(string)
	if !ok {
		return nil, fmt.Errorf("unexpected member type in sorted set %q", key)
	}

	log.Debugf("sorted_set: key=%s best_ip=%s latency=%.2fms", key, ipStr, results[0].Score)
	return parseIP(ipStr)
}

// fromHash scans all fields and finds the minimum value.
func (lp *LatencyPlugin) fromHash(ctx context.Context, key string) (net.IP, error) {
	fields, err := lp.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", key, err)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("hash %q is empty or does not exist", key)
	}

	var (
		bestIP      string
		bestLatency = -1.0
	)
	for ipStr, latStr := range fields {
		var lat float64
		if _, err := fmt.Sscanf(latStr, "%f", &lat); err != nil {
			log.Warningf("hash %s: skipping field %s, bad value %q: %v", key, ipStr, latStr, err)
			continue
		}
		if bestLatency < 0 || lat < bestLatency {
			bestLatency = lat
			bestIP = ipStr
		}
	}

	if bestIP == "" {
		return nil, fmt.Errorf("hash %q had no parseable latency values", key)
	}

	log.Debugf("hash: key=%s best_ip=%s latency=%.2fms", key, bestIP, bestLatency)
	return parseIP(bestIP)
}

// buildResponse constructs a DNS reply containing the resolved IP.
func buildResponse(r *dns.Msg, qname string, ip net.IP, ttl uint32, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = false

	hdr := dns.RR_Header{Name: qname, Ttl: ttl, Class: dns.ClassINET}

	v4 := ip.To4()

	switch {
	case qtype == dns.TypeA && v4 != nil:
		hdr.Rrtype = dns.TypeA
		m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: v4})

	case qtype == dns.TypeAAAA && v4 == nil:
		hdr.Rrtype = dns.TypeAAAA
		m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: ip})

	default:
		// qtype/IP family mismatch → return NOERROR with empty answer.
		m.Answer = nil
	}

	return m
}

// parseIP validates and returns a net.IP, preferring the 16-byte form.
func parseIP(s string) (net.IP, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %q", s)
	}
	return ip, nil
}
