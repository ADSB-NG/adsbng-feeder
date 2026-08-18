# Architecture — ADSBNG feeder

What this program is, how it is put together, and what it puts on the wire.

The short version: it is a pipe with authentication. Bytes come in from a local
Beast socket, get checked for frame validity, and go out over one authenticated
TLS connection. There is no decoding, no state, and no persistence.

---

## Position in the system

```
 your machine                                      ADSBNG
 ─────────────────────────────────────────────      ───────────────────────────────

 SDR ──▶ readsb ──▶ :30005 ──▶ adsbng-feeder ──TLS──▶ gateway ──▶ Core decoder ──▶ DB
         (yours)     Beast      (this program)         (private)   (private)
                                                          │
                                                     token → receiver_id
```

Everything left of the TLS arrow runs on hardware you control. Everything right of
it is ADSBNG's, and this repository contains none of it. The single point of
contact is one outbound TLS connection carrying Beast frames and a station token.

The trust boundary sits exactly on that arrow, and it is one-directional: the
gateway treats everything arriving from a feeder as an untrusted claim. See
[SECURITY.md](SECURITY.md).

---

## Packages

```
cmd/adsbng-feeder/      flags, logger, signal handling — about 100 lines
internal/config/        load and validate config file + environment
internal/beast/         Beast frame scanner
internal/feeder/        the connection lifecycle: dial, handshake, pump, reconnect
internal/proto/         the feeder↔gateway wire format
```

Zero external dependencies, enforced by a policy note in [go.mod](../go.mod). The
config-file parser is about sixty lines of stdlib rather than a TOML library, and
that trade is deliberate: this binary runs on other people's machines, on four
CPU architectures, and every dependency is a supply-chain path onto all of them.
`CGO_ENABLED=0` throughout, so one machine cross-compiles every target — which
stops being true the moment cgo appears in the graph.

---

## The connection lifecycle

`internal/feeder.Run` is a loop around `session()`, with backoff between attempts.

```
Run
 ├── warn loudly if insecure_skip_verify is set
 ├── log the redacted config
 └── forever:
       ├── session()
       │     1. dial the local Beast source        ← first, deliberately
       │     2. dial the gateway over TLS + verify
       │     3. send Hello, read Ack
       │     4. pump() until something fails
       └── sleep backoff, doubling 1s → 30s
```

**Beast is dialled before the gateway.** If your decoder is down, the feeder fails
at step 1 and never reaches the gateway, so a local outage does not spend
authentication attempts against a rate limit it shares with nobody. It also makes
the log read in the order a person would debug in.

**Backoff** starts at 1 second and doubles to a 30-second ceiling, mirroring the
schedule the ADSBNG decoder itself uses against its own Beast source. A session
that lasted longer than twice the ceiling resets the backoff to the minimum, so a
station that has been up for hours reconnects immediately after a blip instead of
inheriting an old penalty.

**Timeouts**, all in `internal/feeder`:

| | |
| --- | --- |
| `dialTimeout` | 20s — TCP connect to either side |
| `handshakeTimeout` | 20s — TLS plus Hello/Ack |
| `beastReadTimeout` | 120s — no local data for this long ends the session |
| `flushInterval` | 250ms — maximum time a frame waits to be batched |
| `readBufSize` | 16 KiB |

The 120-second local read timeout is a liveness check on your decoder, not a
performance limit. Reconnecting is cheap, and a decoder that has silently stopped
producing frames is otherwise indistinguishable from quiet airspace.

### pump

Four goroutines, one connection each way, with a mutex serialising writes to the
gateway:

```
 forward     : Beast socket → scan → batch → MsgData frames
 heartbeat   : every HeartbeatSecs (from Ack, default 30) → MsgHeartbeat + a status log line
 reader      : gateway → MsgPong, or an error if the gateway says anything unexpected
 canceller   : watches ctx, unblocks the two socket reads on shutdown
```

Frames are batched rather than written individually: a batch flushes when it
reaches half of `proto.MaxDataLen` or when `flushInterval` elapses, whichever comes
first. At a busy station that is a few writes a second instead of a few hundred;
at a quiet one the 250 ms ceiling keeps latency low enough not to matter for any
downstream use.

### Frame validation on the way out

The feeder scans the incoming byte stream into Beast frames and forwards only
whole, valid ones. Two consequences worth being explicit about:

- **Framing is preserved, content is not inspected.** The feeder does not decode
  Mode-S. It has no idea what an aircraft is. It cannot filter, alter, or
  synthesise your data, and it does not try to — that is the gateway's and the
  Core decoder's job, and duplicating any of it here would mean shipping ADSBNG's
  decoder to contributors.
- **Unparseable bytes are counted and dropped.** The scanner keeps a carry buffer
  of the trailing partial frame. If that carry ever exceeds four frames' worth
  (`beast.MaxFrameLen * 4`), the feeder concludes the port is not a Beast port and
  fails the session with a message naming the address, rather than forwarding
  garbage indefinitely. That bound is also what makes the memory ceiling
  unconditional: there is no input that makes the feeder grow.

The gateway re-scans everything anyway. Validating here is a courtesy that makes
misconfiguration visible on the contributor's own machine; it is not a security
control, and nothing on ADSBNG's side depends on it.

---

## The wire protocol

`internal/proto`, protocol version 1. Deliberately duplicated in both repositories
rather than shared as a module, so the private gateway and the public feeder have
no build-time coupling and neither can drag the other's code along. The two copies
must stay wire-compatible; changes go through `Version`.

### Beast

Unchanged from the standard everyone else uses, which is why any readsb or
dump1090 install already speaks it:

```
0x1a <type> <body…>
```

`type` is `0x31` Mode-AC, `0x32` Mode-S short, `0x33` Mode-S long. A literal
`0x1a` inside the body is doubled. Maximum frame length 44 bytes.

### Feeder ↔ gateway

A stream of length-prefixed messages over TLS. Two framings, because control
messages and bulk data have different size limits:

```
control (handshake)     [4-byte BE length][JSON]                 ≤ MaxControlLen  4 KiB
data / heartbeat        [1-byte type][3-byte BE length][body]     ≤ MaxDataLen   64 KiB
```

| Type | | |
| --- | --- | --- |
| `0x01` | `MsgData` | a batch of concatenated Beast frames |
| `0x02` | `MsgHeartbeat` | JSON counters: frames, bytes, discarded, uptime |
| `0x03` | `MsgPong` | gateway → feeder, keepalive response |

Both sides enforce both limits before allocating, and an unrecognised type byte is
given the *small* control limit rather than the large one — so a bogus type cannot
be used to induce a 64 KiB allocation per message.

**Handshake.** Exactly one `Hello`, exactly one `Ack`, then frames:

```jsonc
// → Hello
{ "proto": 1, "token": "adsbng_…", "station_id_hint": "LAGOS_01", "feeder_version": "0.1.0" }

// ← Ack
{ "ok": true, "receiver_id": "LAGOS_01", "server": "adsbng-ingest/0.1.0", "heartbeat_secs": 30 }
```

`station_id_hint` is **untrusted and advisory**. The gateway logs it for
diagnostics and must not use it to determine identity; that comes from a hash
lookup of `token`. The `receiver_id` in the `Ack` is the gateway's answer, and the
feeder treats it as authoritative — if it disagrees with the local `station_id`
label, the feeder says so in the log and carries on with the gateway's value.

A rejected handshake returns `{"ok": false, "error": "unauthorized"}`. The error
string is coarse on purpose and never distinguishes an unknown token from a
revoked one, because that difference is an oracle for which tokens exist.

---

## TLS

`internal/feeder.tlsConfig`:

- `MinVersion: tls.VersionTLS12`.
- `ServerName` from the configured gateway host, so the hostname is verified.
- **`ca_file` replaces the system roots with verification still fully on.** This
  is the supported way to run against a private CA, and it is a narrowing, not a
  weakening: the gateway certificate must chain to that bundle and nothing else.
- **`insecure_skip_verify` is the only way to disable verification**, defaults to
  off, cannot be combined with `ca_file` (a config that pins a CA *and* skips
  verification looks careful while being neither), and logs a four-line warning on
  every single connection while enabled.

The feeder sends its token only after verification succeeds. That ordering is the
point of verifying at all — an unverified connection is a stranger you have
already handed the password to.

---

## Configuration precedence

Lowest to highest: built-in defaults → config file → environment variables.

Within the file, keys are applied in a fixed table order rather than in the order
they appear or the order a map happens to iterate in. Broad keys come first and
narrow ones override them, so `gateway = "host:8443"` together with
`gateway_port = 9443` always resolves to 9443 regardless of which line comes
first. Two different spellings of the same setting (`gateway_port` and
`ingest_port`) are a config error rather than a coin toss.

This is more machinery than it sounds like it needs, and it is there because the
first version ranged over the parsed map. Go randomizes map iteration, so a config
with two overlapping keys resolved to a different port on roughly half of all
starts — a feeder that works until it is restarted, on a machine nobody at ADSBNG
can log into. `TestOverlappingFileKeysResolveDeterministically` exists to keep it
from coming back.

Unknown keys are rejected at startup for the same class of reason: a silently
ignored `toekn = "…"` is much worse to diagnose remotely than a refusal to start.

---

## What is deliberately absent

| Not here | Why |
| --- | --- |
| Any inbound listener | Nothing to port-forward, nothing new exposed on a contributor's network. Both connections are outbound. |
| Mode-S / ADS-B decoding | It would mean shipping ADSBNG's decoder to untrusted machines. Beast in, Beast out. |
| Any database client | The feeder has no notion of a database. Nothing to point at ADSBNG's. |
| Disk writes, state, spooling | Nothing to leak, nothing to corrupt, nothing to grow. The systemd unit gives it no writable path at all. |
| A self-reported receiver identity | The gateway derives identity from the token. A field the feeder controls could be forged. |
| External dependencies | Four architectures, other people's machines, cgo-free cross-compilation. |
| Retry queues / buffering across reconnects | ADS-B is a live stream; a frame that is 40 seconds late has no value. Dropping is correct and keeps memory bounded. |

---

## Tests

```bash
./scripts/test.sh          # gofmt, go vet, go test (-race where cgo is available)
```

`internal/beast` covers the scanner against truncation, escape handling, and
garbage injection. `internal/config` covers precedence, the alias rules, and the
determinism regression above. `internal/feeder` covers the session lifecycle,
backoff, and handshake failure handling against a stub gateway.

The end-to-end test — two stations, real TLS, real Beast frames, correct
attribution at the far end — lives in the private `adsbng-ingest` repository,
because it needs a gateway to test against.
