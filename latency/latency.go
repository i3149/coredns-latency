package latency

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"github.com/redis/go-redis/v9"
)

var log = clog.NewWithPlugin("latency")

//
//  2. hash
//     Key  : "latency:<fqdn>"
//     Type : Redis Hash
//     Field: IP address string
//     Value: latency in milliseconds (string-encoded float) e.g. "12.5"
//
//     HSET latency:api.example.com.:A 10.0.0.1 12.5
//     HSET latency:api.example.com.:A 10.0.0.2  8.3

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

	return lp.fromHash(ctx, key)
}

// Holder here for a way to sort hash values.
type ipset struct {
	ip net.IP
	s  float64
}
type byScore []ipset

func (a byScore) Len() int           { return len(a) }
func (a byScore) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byScore) Less(i, j int) bool { return a[i].s < a[j].s }

// fromHash scans all fields and finds the minimum value.
func (lp *LatencyPlugin) fromHash(ctx context.Context, key string) ([]net.IP, error) {
	fields, err := lp.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("HGETALL %s: %w", key, err)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("hash %q is empty or does not exist", key)
	}

	ips := make([]ipset, 0, len(fields))
	for ipStr, latStr := range fields {
		if lat, err := strconv.ParseFloat(latStr, 64); err != nil {
			log.Warningf("hash %s: skipping field %s, bad value %q: %v", key, ipStr, latStr, err)
			continue
		} else {
			ip, err := parseIP(ipStr)
			if err != nil || lat <= 0 {
				continue
			}
			ips = append(ips, ipset{ip: ip, s: lat})
		}
	}

	// Sort here to make picking out top x easier.
	sort.Sort(byScore(ips))

	if len(ips) == 0 {
		return nil, fmt.Errorf("hash %q had no parseable latency values", key)
	}

	// Now, itterate one more time getting all of the ones within lp.maxLatencyDiff of the best.
	results := make([]net.IP, 0, lp.maxIPS)
	bestLat := ips[0].s
	for _, ip := range ips {
		if ip.s-bestLat < lp.maxLatencyDiff {
			results = append(results, ip.ip)
		}
	}

	log.Debugf("hash: key=%s best_ips=%v", key, results)
	return results, nil
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
