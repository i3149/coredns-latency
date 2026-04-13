# coredns-latency

A [CoreDNS](https://coredns.io) plugin that **returns the IP address(s) with the
lowest measured latency** from a candidate set stored in Redis.

```
DNS client  ──▶  CoreDNS (latency plugin)  ──▶  Redis
                        │
                  lowest-latency IP
                        │
               ◀── A / AAAA response
```

---

## How it works

1. A DNS query arrives for `api.example.com. A`.
2. The plugin looks up the Redis key `latency:api.example.com.`.
3. It picks the members with the lowest scores within a defined equivalency set.
4. It synthesises an `A` (or `AAAA`) record and returns it immediately.

---

## Redis data models

### `sorted_set` (default) — recommended

```
Key   : "latency:<fqdn>"          e.g.  latency:api.example.com.
Type  : Sorted Set
Score : latency in milliseconds   (float64)
Member: IP address string

# Write
ZADD latency:api.example.com.:A 12.5 "10.0.0.1"
ZADD latency:api.example.com.:A  8.3 "10.0.0.2"
ZADD latency:api.example.com.:A 35.0 "10.0.0.3"

# Read (done internally by the plugin)
ZRANGE latency:api.example.com.:A 0 0 WITHSCORES
# → "10.0.0.2"  8.3
```

**Why sorted sets?** O(log N) writes, O(1) minimum retrieval — perfect for
frequently updated latency scores.

---

## Corefile syntax

```corefile
latency [ZONES...] {
    redis_addr       <host:port>     # default: localhost:6379
    redis_password   <password>      # default: (none)
    redis_db         <int>           # default: 0
    redis_timeout    <duration>      # default: 500ms
    key_prefix       <string>        # default: "latency:"
    max_ips          1               # Return at most this many possible ips.
    max_latency_diff 100             # All ips within this ms of the best score returned.
    ttl              <seconds>       # default: 5
    fallback                         # pass to next plugin if no Redis data
}
```

| Option | Default | Description |
|---|---|---|
| `redis_addr` | `localhost:6379` | Redis server address |
| `redis_password` | _(empty)_ | Redis AUTH password |
| `redis_db` | `0` | Redis logical database |
| `redis_timeout` | `500ms` | Dial / read / write timeout |
| `key_prefix` | `latency:` | Prepended to the FQDN to form the Redis key |
| `max_ips` | `1` | Return at most this many ips |
| `max_latency_diff` | `100` | All ips returned must be at least this close to the best score |
| `ttl` | `5` | DNS record TTL in seconds |
| `fallback` | _(flag)_ | When set, pass through to `next` plugin if no data |

---

## Integration into CoreDNS

CoreDNS plugins must be compiled in. Add the plugin to your fork:

### 1. Clone CoreDNS

```bash
git clone https://github.com/coredns/coredns.git
cd coredns
```

### 2. Register the plugin

Add one line to `plugin.cfg` (order matters — place before `forward`):

```
latency:github.com/i3149/coredns-latency
```

### 3. Update `go.mod`

```bash
go get github.com/yourorg/coredns-latency@latest
go mod tidy
```

### 4. Build

```bash
make
./coredns -conf Corefile
```

---

## Metrics (Prometheus)

| Metric | Type | Description |
|---|---|---|
| `coredns_latency_requests_total` | Counter | DNS requests handled |
| `coredns_latency_redis_lookup_duration_seconds` | Histogram | Redis lookup time |

Enable with the `prometheus` plugin in your Corefile:

```corefile
prometheus :9153
```

---

## Running tests

```bash
go test ./... -v -race
```

Tests use [miniredis](https://github.com/alicebob/miniredis) — no live Redis
instance required.

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                     CoreDNS                         │
│                                                     │
│  DNS query ──▶ [latency plugin]                     │
│                    │                                │
│            lowestLatencyIP()                        │
│                    │                                │
│         ┌──────────┴──────────┐                     │
│         │ sorted_set          │                     │
│         │ ZRANGE key 0 0      │                     │
│         │ (O(log N) best IP)  │                     │
│         └──────────┬──────────┘                     │
│                    │                                │
│           build A/AAAA response                     │
│                    │                                │
│  DNS response ◀────┘                                │
└─────────────────────────────────────────────────────┘
             │
             ▼ (on cache miss / fallback)
         next plugin
```
