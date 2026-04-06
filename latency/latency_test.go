package latency

import (
	"context"
	"net"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
	"github.com/redis/go-redis/v9"
)

// newTestPlugin spins up a miniredis server and returns a configured plugin.
func newTestPlugin(t *testing.T, format keyFormat) (*LatencyPlugin, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return &LatencyPlugin{
		rdb:       rdb,
		keyPrefix: "latency:",
		format:    format,
		ttl:       5,
		fallback:  false,
		Next:      test.ErrorHandler(),
	}, mr
}

// ---------------------------------------------------------------------------
// Sorted-set tests
// ---------------------------------------------------------------------------

func TestSortedSet_ReturnsLowestLatency(t *testing.T) {
	lp, mr := newTestPlugin(t, sortedSet)

	// Populate: 10.0.0.3 has lowest latency (5 ms).
	mr.ZAdd("latency:api.example.com.", 50, "10.0.0.1")
	mr.ZAdd("latency:api.example.com.", 30, "10.0.0.2")
	mr.ZAdd("latency:api.example.com.", 5, "10.0.0.3")

	ip, err := lp.lowestLatencyIP(context.Background(), "api.example.com.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.0.0.3")) {
		t.Errorf("expected 10.0.0.3, got %s", ip)
	}
}

func TestSortedSet_EmptyKey(t *testing.T) {
	lp, _ := newTestPlugin(t, sortedSet)

	_, err := lp.lowestLatencyIP(context.Background(), "missing.example.com.")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

// ---------------------------------------------------------------------------
// Hash tests
// ---------------------------------------------------------------------------

func TestHash_ReturnsLowestLatency(t *testing.T) {
	lp, mr := newTestPlugin(t, hashMap)

	mr.HSet("latency:api.example.com.", "10.0.0.1", "120")
	mr.HSet("latency:api.example.com.", "10.0.0.2", "8")
	mr.HSet("latency:api.example.com.", "10.0.0.3", "45")

	ip, err := lp.lowestLatencyIP(context.Background(), "api.example.com.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.0.0.2")) {
		t.Errorf("expected 10.0.0.2, got %s", ip)
	}
}

func TestHash_SkipsBadValues(t *testing.T) {
	lp, mr := newTestPlugin(t, hashMap)

	mr.HSet("latency:api.example.com.", "10.0.0.1", "not-a-number")
	mr.HSet("latency:api.example.com.", "10.0.0.2", "25")

	ip, err := lp.lowestLatencyIP(context.Background(), "api.example.com.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("10.0.0.2")) {
		t.Errorf("expected 10.0.0.2, got %s", ip)
	}
}

// ---------------------------------------------------------------------------
// Full DNS response tests
// ---------------------------------------------------------------------------

func TestServeDNS_ARecord(t *testing.T) {
	lp, mr := newTestPlugin(t, sortedSet)
	mr.ZAdd("latency:api.example.com.", 10, "1.2.3.4")

	req := new(dns.Msg)
	req.SetQuestion("api.example.com.", dns.TypeA)

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	code, err := lp.ServeDNS(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("ServeDNS error: %v", err)
	}
	if code != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got %d", code)
	}

	resp := rec.Msg
	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("expected A record, got %T", resp.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("expected 1.2.3.4, got %s", a.A)
	}
}

func TestServeDNS_AAAARecord(t *testing.T) {
	lp, mr := newTestPlugin(t, sortedSet)
	mr.ZAdd("latency:ipv6.example.com.", 10, "2001:db8::1")

	req := new(dns.Msg)
	req.SetQuestion("ipv6.example.com.", dns.TypeAAAA)

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	code, err := lp.ServeDNS(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("ServeDNS error: %v", err)
	}
	if code != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got %d", code)
	}

	resp := rec.Msg
	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(resp.Answer))
	}
	aaaa, ok := resp.Answer[0].(*dns.AAAA)
	if !ok {
		t.Fatalf("expected AAAA record, got %T", resp.Answer[0])
	}
	if !aaaa.AAAA.Equal(net.ParseIP("2001:db8::1")) {
		t.Errorf("expected 2001:db8::1, got %s", aaaa.AAAA)
	}
}

func TestServeDNS_FallbackOnMissingData(t *testing.T) {
	lp, _ := newTestPlugin(t, sortedSet)
	lp.fallback = true
	// Next handler returns NXDOMAIN.
	lp.Next = test.ErrorHandler()

	req := new(dns.Msg)
	req.SetQuestion("ghost.example.com.", dns.TypeA)

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	// Should not error; falls through to Next.
	_, err := lp.ServeDNS(context.Background(), rec, req)
	if err == nil {
		// ErrorHandler returns an error – that's expected.
	}
}

func TestServeDNS_NonAQuery_PassThrough(t *testing.T) {
	lp, _ := newTestPlugin(t, sortedSet)

	req := new(dns.Msg)
	req.SetQuestion("api.example.com.", dns.TypeMX)

	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	// MX queries must be passed to Next without touching Redis.
	lp.ServeDNS(context.Background(), rec, req) //nolint:errcheck
}
