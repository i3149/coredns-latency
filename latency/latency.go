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

/**
TODO
*/

// RedisKeyFormat defines how latency keys are stored in Redis.
//
//  1. sorted_set
//     Key  : "latency:<fqdn>:<dns_type>"          e.g. "latency:api.example.com.:A"
//     Type : Redis Sorted Set
//     Score: latency in milliseconds (float64)
//     Member: IP address string        e.g. "10.0.0.1"
//
//     ZADD latency:api.example.com.:A  12.5 10.0.0.1
//     ZADD latency:api.example.com.:A  8.3 10.0.0.2

// LatencyPlugin is the main plugin struct.
type LatencyPlugin struct {
	Next plugin.Handler

	rdb            *redis.Client
	keyPrefix      string   // default: "latency:"
	ttl            uint32   // TTL for synthesised A/AAAA records (seconds)
	fallback       bool     // pass through to next plugin when no Redis data found
	zones          []string // zones this plugin is authoritative for; empty = all
	maxIPS         int64    // Max number of IPs to return for a given service.
	maxLatencyDiff float64  // Max difference between lowest and highest latencies in an equivalence class.
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

	ips, err := lp.lowestLatencyIPS(ctx, qname, qtype)
	if err != nil || len(ips) == 0 {
		if lp.fallback {
			log.Debugf("no latency data for %s, falling through", qname)
			return plugin.NextOrFailure(lp.Name(), lp.Next, ctx, w, r)
		}
		log.Warningf("no latency data for %s and fallback disabled: %v", qname, err)
		return dns.RcodeServerFailure, err
	}

	elapsed := time.Since(start)
	log.Debugf("resolved %s → %v (redis lookup: %s)", qname, ips, elapsed)

	// Record metrics.
	requestCount.WithLabelValues(metrics.WithServer(ctx)).Inc()
	redisLookupDuration.WithLabelValues(metrics.WithServer(ctx)).Observe(elapsed.Seconds())

	msg := buildResponse(r, qname, ips, lp.ttl, qtype)
	if err := w.WriteMsg(msg); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

// lowestLatencyIP queries Redis for the IP set with the smallest latency score.
func (lp *LatencyPlugin) lowestLatencyIPS(ctx context.Context, fqdn string, qtype uint16) ([]net.IP, error) {
	key := lp.keyPrefix + fqdn
	switch qtype {
	case dns.TypeA:
		key = key + ":A"
	case dns.TypeAAAA:
		key = key + ":AAAA"
	}

	return lp.fromSortedSet(ctx, key)
}

// fromSortedSet uses ZRANGE key 0 0 (lowest score first).
func (lp *LatencyPlugin) fromSortedSet(ctx context.Context, key string) ([]net.IP, error) {
	results, err := lp.rdb.ZRangeWithScores(ctx, key, 0, lp.maxIPS).Result()
	if err != nil {
		return nil, fmt.Errorf("ZRANGE %s: %w", key, err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("sorted set %q is empty or does not exist", key)
	}

	bestScore := results[0].Score
	ips := make([]net.IP, 0, len(results))
	for _, result := range results {
		ipStr, ok := result.Member.(string)
		if !ok {
			log.Errorf("unexpected member type in sorted set %q", key)
			continue
		}

		if result.Score-bestScore > lp.maxLatencyDiff { // This score is too high off to be added to the latency set.
			log.Debugf("sorted_set dropping: key=%s best_ip=%s latency=%.2fms", key, ipStr, result.Score)
			continue
		}
		log.Debugf("sorted_set adding: key=%s best_ip=%s latency=%.2fms", key, ipStr, result.Score)
		ip, err := parseIP(ipStr)
		if err != nil {
			log.Errorf("unexpected ip in sorted set %s", ipStr)
			continue
		}
		ips = append(ips, ip)

	}
	return ips, nil
}

// buildResponse constructs a DNS reply containing the resolved IP.
func buildResponse(r *dns.Msg, qname string, ips []net.IP, ttl uint32, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = false

	hdr := dns.RR_Header{Name: qname, Ttl: ttl, Class: dns.ClassINET}

	for _, ip := range ips {
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
