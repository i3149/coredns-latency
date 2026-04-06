# coredns-latency

A [CoreDNS](https://coredns.io) plugin that **returns the IP address with the
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
3. It picks the member with the **smallest score / value** (= lowest latency in ms).
4. It synthesises an `A` (or `AAAA`) record and returns it immediately.

Latency values in Redis are typically written by a **sidecar prober** that
periodically measures round-trip time to each candidate IP (e.g. using ICMP or
TCP SYN probes) and updates the scores.

---

## Redis data models

### `sorted_set` (default) — recommended

```
Key   : "latency:<fqdn>"          e.g.  latency:api.example.com.
Type  : Sorted Set
Score : latency in milliseconds   (float64)
Member: IP address string

# Write
ZADD latency:api.example.com. 12.5 "10.0.0.1"
ZADD latency:api.example.com.  8.3 "10.0.0.2"
ZADD latency:api.example.com. 35.0 "10.0.0.3"

# Read (done internally by the plugin)
ZRANGE latency:api.example.com. 0 0 WITHSCORES
# → "10.0.0.2"  8.3
```

**Why sorted sets?** O(log N) writes, O(1) minimum retrieval — perfect for
frequently updated latency scores.

### `hash` — alternative

```
Key   : "latency:<fqdn>"
Type  : Hash
Field : IP address string
Value : latency in milliseconds   (string-encoded float)

HSET latency:api.example.com. 10.0.0.1 12.5
HSET latency:api.example.com. 10.0.0.2  8.3
```

The plugin scans all fields with `HGETALL` and finds the minimum. Use this if
your prober already writes hashes.

---

## Corefile syntax

```corefile
latency [ZONES...] {
    redis_addr     <host:port>     # default: localhost:6379
    redis_password <password>      # default: (none)
    redis_db       <int>           # default: 0
    redis_timeout  <duration>      # default: 500ms
    key_prefix     <string>        # default: "latency:"
    key_format     sorted_set|hash # default: sorted_set
    ttl            <seconds>       # default: 5
    fallback                       # pass to next plugin if no Redis data
}
```

| Option | Default | Description |
|---|---|---|
| `redis_addr` | `localhost:6379` | Redis server address |
| `redis_password` | _(empty)_ | Redis AUTH password |
| `redis_db` | `0` | Redis logical database |
| `redis_timeout` | `500ms` | Dial / read / write timeout |
| `key_prefix` | `latency:` | Prepended to the FQDN to form the Redis key |
| `key_format` | `sorted_set` | Redis data structure (`sorted_set` or `hash`) |
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
latency:github.com/yourorg/coredns-latency
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

## Sidecar prober example (Python)

```python
"""
Minimal latency prober — measures TCP connect time and updates Redis.
Run this on a schedule (e.g. every 10 s via cron or a loop).
"""
import time, socket, redis

HOSTS = {
    "api.example.com.": ["10.0.0.1", "10.0.0.2", "10.0.0.3"],
}
PORT   = 443     # probe port
TRIALS = 3       # average over N trials

r = redis.Redis(host="localhost", decode_responses=True)

def probe(ip: str, port: int) -> float:
    """Return average TCP connect latency in ms, or 9999 on failure."""
    times = []
    for _ in range(TRIALS):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(1)
            t0 = time.perf_counter()
            s.connect((ip, port))
            times.append((time.perf_counter() - t0) * 1000)
            s.close()
        except OSError:
            times.append(9999)
    return sum(times) / len(times)

while True:
    pipe = r.pipeline()
    for fqdn, ips in HOSTS.items():
        key = f"latency:{fqdn}"
        for ip in ips:
            lat = probe(ip, PORT)
            pipe.zadd(key, {ip: lat})
            print(f"{fqdn} {ip} {lat:.1f}ms")
    pipe.execute()
    time.sleep(10)
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
│         ┌──────────┴──────────┐                    │
│         │ sorted_set          │ hash                │
│         │ ZRANGE key 0 0      │ HGETALL key         │
│         │ (O(log N) best IP)  │ (O(N) linear scan)  │
│         └──────────┬──────────┘                    │
│                    │                                │
│           build A/AAAA response                     │
│                    │                                │
│  DNS response ◀────┘                               │
└─────────────────────────────────────────────────────┘
             │
             ▼ (on cache miss / fallback)
         next plugin
```