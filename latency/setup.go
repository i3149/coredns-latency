package latency

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/redis/go-redis/v9"
)

// init registers the plugin with CoreDNS's Caddy loader.
func init() {
	plugin.Register("latency", setup)
}

// setup is called by Caddy when the "latency" directive is encountered in
// a Corefile block.
//
// Corefile syntax
// ---------------
//
//		latency {
//		    redis_addr       localhost:6379   # default: localhost:6379
//		    redis_password   ""               # default: no password
//		    redis_db         0                # default: 0
//		    redis_timeout    500ms            # dial/read/write timeout (default: 500ms)
//		    key_prefix       latency:         # Redis key prefix (default: "latency:")
//	            max_ips          1                # Return at most this many possible ips.
//	            max_latency_diff 100              # All ips within this ms of the best score returned.
//		    ttl              5                # DNS record TTL in seconds (default: 5)
//		    fallback                          # pass to next plugin when no data found
//		    zones            example.com.     # limit to these zones (default: all)
//		}
func setup(c *caddy.Controller) error {
	lp, err := parse(c)
	if err != nil {
		return plugin.Error("latency", err)
	}

	// Register plugin in the server chain.
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		lp.Next = next
		return lp
	})

	// Open Redis connection when the server starts; close it when it stops.
	c.OnStartup(func() error {
		if err := lp.rdb.Ping(context.Background()).Err(); err != nil {
			return fmt.Errorf("latency plugin: cannot reach Redis at %s: %w",
				lp.rdb.Options().Addr, err)
		}
		log.Infof("connected to Redis at %s", lp.rdb.Options().Addr)
		return nil
	})

	c.OnShutdown(func() error {
		return lp.rdb.Close()
	})

	return nil
}

// parse reads the Corefile block and returns a configured LatencyPlugin.
func parse(c *caddy.Controller) (*LatencyPlugin, error) {
	lp := &LatencyPlugin{
		keyPrefix: "latency:",
		ttl:       5,
		fallback:  false,
		maxIPS:    0,
	}

	// Redis client options with sensible defaults.
	opts := &redis.Options{
		Addr:         "localhost:6379",
		Password:     "",
		DB:           0,
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	}

	for c.Next() {
		// Zones can be listed on the same line as the directive.
		lp.zones = c.RemainingArgs()

		for c.NextBlock() {
			switch c.Val() {
			case "redis_addr":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				opts.Addr = c.Val()

			case "redis_password":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				opts.Password = c.Val()

			case "redis_db":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				db, err := strconv.Atoi(c.Val())
				if err != nil {
					return nil, fmt.Errorf("redis_db: %w", err)
				}
				opts.DB = db

			case "redis_timeout":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, fmt.Errorf("redis_timeout: %w", err)
				}
				opts.DialTimeout = d
				opts.ReadTimeout = d
				opts.WriteTimeout = d

			case "key_prefix":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				lp.keyPrefix = c.Val()

			case "max_ips":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				maxIPS, err := strconv.ParseInt(c.Val(), 0, 64)
				if err != nil {
					return nil, fmt.Errorf("max_ips: %w", err)
				}
				lp.maxIPS = maxIPS - 1 // Because zrange is inclusive, we want 1 less than max.
			case "max_latency_diff":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				maxLatencyDiff, err := strconv.ParseFloat(c.Val(), 64)
				if err != nil {
					return nil, fmt.Errorf("max_latency_diff: %w", err)
				}
				lp.maxLatencyDiff = maxLatencyDiff
			case "ttl":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				ttl, err := strconv.ParseUint(c.Val(), 10, 32)
				if err != nil {
					return nil, fmt.Errorf("ttl: %w", err)
				}
				lp.ttl = uint32(ttl)

			case "fallback":
				lp.fallback = true

			default:
				return nil, fmt.Errorf("unknown option %q", c.Val())
			}
		}
	}

	lp.rdb = redis.NewClient(opts)
	return lp, nil
}
