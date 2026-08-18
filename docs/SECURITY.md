# Security — ADSBNG feeder

The trust boundary and threat model, stated plainly enough to check.

This document is written to be read by two different people: a contributor
deciding whether to run ADSBNG's software as a root-installed service on their own
network, and an ADSBNG engineer checking that a hostile contributor cannot reach
anything. Both need the same facts, so both get the same document.

---

## The trust boundary in one sentence

ADSBNG treats your machine as untrusted and this program holds nothing worth
stealing; you should treat ADSBNG's binary as third-party software and this
document tells you exactly what it does.

Neither half of that is a courtesy. The boundary is a design constraint that shows
up in the code, and the rest of this document is the detail.

```
      YOUR SIDE — untrusted by ADSBNG      │      ADSBNG SIDE — private
                                           │
  SDR → readsb → :30005 → adsbng-feeder ───┼──TLS──▶ gateway → Core decoder → DB
                          ▲                │         ▲
                          │                │         │
            this repository, all of it     │    none of this is in this repository
            one station token              │    identity decided here, from the token
                                           │
                            the boundary ──┘
```

Nothing crosses left-to-right except Beast frames and a station token. Nothing
crosses right-to-left except a `receiver_id`, a heartbeat interval, and keepalives.

---

## Threat model

### Adversary 1: a fully compromised contributor machine

**ADSBNG's assumption is that this has already happened.** Someone has root on
your Pi, has read `config.toml`, has your token, and can run modified feeder code.

What they get:

- Your station token, and therefore the ability to submit arbitrary Beast frames
  attributed to **your** station.
- The ability to exhaust **your own** rate limit.
- The `receiver_id` the gateway assigns you, and the gateway's hostname. Neither
  is a secret.

What they do **not** get, and the reason for each:

| Not obtainable | Why not |
| --- | --- |
| ADSBNG's database credentials | Not in this repository, not in the config file, not sent over the connection, not derivable from anything the feeder holds. The feeder has no database client of any kind. |
| Any other ADSBNG application secret — payment, email, messaging, push-notification, AI, or link-signing | Same: none of them exist anywhere on the contributor side. The feeder's entire configuration surface is six settings, all listed in `config.toml.example`: a label, a token, a gateway address, a Beast address, and two TLS options. There is no key of any other kind for it to hold. |
| The ADSBNG decoder or analytics source | Not in this repository. The feeder forwards Beast frames without decoding them, precisely so it does not need any of that logic. |
| Another station's token | Tokens are per station. The feeder never receives any token but its own, and the gateway never sends one. |
| Another station's data | The connection is one-directional for data. There is no read path, no query interface, and no API to call. The gateway sends a feeder nothing but its own identity and keepalives. |
| The ability to feed *as* another station | Identity is derived from a hash lookup of the presented token. There is no field a client controls that affects attribution — see "receiver_id spoofing" below. |
| Access to ADSBNG's network | The connection is outbound to one TLS port. The feeder is a client on both of its sockets; nothing in the ADSBNG estate connects back to it. |

The honest summary: a compromised contributor machine can lie about aircraft. That
is the entire blast radius, it is inherent to accepting data from other people, and
it is handled downstream by ADSBNG's own plausibility checks — not by trusting the
feeder.

### Adversary 2: a contributor who reads and modifies the feeder

Expected, and explicitly fine. Read every line; that is why the repository exists
in this shape. Nothing here is obfuscated, because nothing here is load-bearing for
security. Modify it, replace it with your own client, speak the protocol in
[ARCHITECTURE.md](ARCHITECTURE.md) by hand — the gateway's answer is the same,
because **every control that matters is enforced on the server**:

- Identity comes from the token, server-side. A patched feeder claiming
  `station_id_hint: "ABUJA_01"` changes a log line and nothing else.
- Rate limits are applied by the gateway per station, and are not something a
  client can raise.
- Frame validity is re-checked by the gateway. The feeder's framing is treated as
  a claim, not a guarantee.
- Revocation is server-side and immediate. A patched feeder cannot decline to be
  revoked.

There is no client-side check in this program whose removal gains an attacker
anything. If you find one, that is a bug — please report it.

### Adversary 3: the network between you and ADSBNG

An attacker on your LAN, your ISP, or anywhere on the path.

- **The connection is TLS 1.2 or better, with certificate verification on.** The
  feeder sends its token only after verification succeeds — which is the whole
  reason for verifying, since an unverified connection is a stranger you have
  already given the password to.
- **The hostname is checked.** `ServerName` is set from the configured gateway
  host; a valid certificate for some other domain does not work.
- **A private CA narrows trust rather than removing it.** Setting `ca_file`
  replaces the system root store with your bundle, so the gateway certificate must
  chain to that and nothing else.
- **`insecure_skip_verify` defaults to off**, is documented as DEVELOPMENT ONLY in
  three places, cannot be combined with `ca_file`, and logs a four-line warning on
  every connection while enabled. With it on, anyone who can intercept the
  connection can impersonate the gateway and collect your token. Do not use it.

What is *not* protected: the hop from your decoder to the feeder. Beast on
127.0.0.1 is unauthenticated plaintext, as it is for every other feeder client on
your machine. If you point `beast_source` at another host, that hop is plaintext
on your LAN. Keep it local, or accept that your own network can see your own
aircraft data.

### Adversary 4: another process on the contributor's machine

An unprivileged user or a compromised unrelated service on your own box.

- `config.toml` is mode **`0600`**, owned by `adsbng`, inside a `0750` directory.
  Reading the token requires being `adsbng` or root. This is the only thing
  protecting the token at rest — there is no keyring, no encryption-at-rest, and
  pretending otherwise would be theatre, since a service that must read a
  credential unattended at boot has to be able to read it.
- The service runs as the unprivileged `adsbng` user: no login shell, no home
  directory, no sudo, and no capabilities at all
  (`CapabilityBoundingSet=`/`AmbientCapabilities=` are both empty).
- **The token is never logged.** Everywhere the feeder prints configuration it
  prints `MaskToken`, which reveals a 4-character prefix and a length: enough to
  tell two tokens apart in a support conversation, not enough to replay one. There
  is a test that walks the AST to check no code path reaches a formatting call with
  the `Token` field, so this cannot regress quietly.
- The config parser deliberately does not echo the offending text when a `token =`
  line is malformed, because an error message is a much easier thing to leak into a
  forum post than a config file.

### Adversary 5: ADSBNG, from your point of view

You are installing a binary as a root-enabled systemd service. It is reasonable to
ask what it can do, and the answer should be checkable rather than promised.

- **It opens no inbound port.** Both sockets are outbound. Verify with
  `ss -ltnp | grep adsbng` — nothing. Your router configuration does not change and
  nothing new is reachable from the internet.
- **It writes nothing to disk.** The systemd unit grants no writable path at all:
  `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`. There is no state
  directory because there is no state.
- **It reads nothing of yours.** `ProtectHome=true` puts home directories out of
  reach; `ProtectProc=invisible` and `ProcSubset=pid` stop it inspecting other
  processes. It reads its own config file and one TCP socket.
- **It runs as a nobody.** `User=adsbng`, no capabilities, `NoNewPrivileges=true`,
  `MemoryDenyWriteExecute=true`, and a `SystemCallFilter` limited to
  `@system-service` minus `@privileged @resources @obsolete @mount @debug
  @cpu-emulation @swap`.
- **It sends only Beast frames and counters.** The heartbeat carries four numbers:
  frames, bytes, discarded, uptime. No hostname, no system inventory, no
  telemetry. Read `internal/feeder/feeder.go` — the whole outbound surface is two
  message types.
- **The installer does not phone home.** `install.sh` makes no network request
  unless you explicitly set `ADSBNG_DOWNLOAD_BASE`, installs nothing but this
  binary, and does not touch your readsb configuration.

Every systemd restriction in the unit file was checked against actual operation
rather than copied from a hardening checklist, because a restriction that breaks
the service is worse than one that is missing — it gets removed wholesale by the
next person to debug it.

---

## Why the contributor cannot forge a receiver_id

This is the property everything else rests on, so it is worth being precise.

The `Hello` message contains a `station_id_hint`. It is untrusted, advisory, and
used only for a log line — its doc comment says so, and the gateway's code path for
identity does not read it. Instead:

1. The gateway hashes the presented token with SHA-256.
2. It looks that hash up in its station registry.
3. The `receiver_id` on the matching row is the station's identity, for that
   connection, for its whole lifetime.
4. That `receiver_id` selects which loopback Beast listener the frames are written
   to, and each listener feeds a Core decoder started with the matching
   `RECEIVER_ID`.

Attribution is therefore a property of *which socket the bytes came out of* on
ADSBNG's side, not of anything in the data. Two stations' frames are never in the
same byte stream, so there is no code path that could mix them up — not a check
that could be forgotten, but an absence of the shared channel that would make the
mistake possible. A frame from one station cannot become another station's data
even if the gateway had a bug, short of one that rewires its own listeners.

Tokens are 256 bits from `crypto/rand`, which is what allows the registry to store
a plain SHA-256 rather than a stretched hash: there is no dictionary to attack.
Comparison is constant-time. The registry stores only hashes — the plaintext token
exists in exactly two places, ADSBNG's provisioning output at the moment of issue,
and your `config.toml`.

---

## What the gateway will not tell you

Authentication failures return `unauthorized` and nothing more. The gateway does
not distinguish "no such token" from "this token was revoked", because that
difference is an oracle for which tokens exist. Operators can see the real reason
in the gateway's own log; contributors cannot, and support has to fill the gap.
This is a deliberate trade of diagnosability for a smaller information leak, and
[OPERATIONS.md](OPERATIONS.md#gateway-rejected-this-station-unauthorized) says so
where a contributor will actually hit it.

---

## Accepted risks

Stated rather than hidden, because a threat model with no accepted risks is not a
threat model.

| Risk | Position |
| --- | --- |
| The token is plaintext on the contributor's disk | Unavoidable for an unattended service that must authenticate at boot. Mitigated by `0600`/`adsbng` ownership and by the token granting nothing but "submit frames as this station". Rotation is cheap. |
| A contributor can submit fabricated ADS-B frames | Inherent to accepting third-party data. Handled downstream by ADSBNG's plausibility checks, not by trusting the feeder. Per-station attribution means bad data is traceable to one station and that station can be revoked. |
| Local Beast is unauthenticated plaintext | Standard for the entire ADS-B ecosystem, and out of scope for this program: readsb published that port before the feeder existed. Keep it on loopback. |
| A token in a screenshot or forum post | The single most likely real-world failure. Mitigated by masking the token in every log line, refusing to log token-shaped strings, and telling contributors what to send when reporting a problem. |
| `insecure_skip_verify` reaching a real station | Off by default, warned about loudly and repeatedly, mutually exclusive with `ca_file`. Not removable outright without breaking the acceptance tests, which need a throwaway certificate. |

---

## Reporting a security problem

Contact ADSBNG directly rather than opening a public issue. Two things we
particularly want to hear about:

- Any way a feeder can influence how its data is attributed.
- Any way the feeder discloses its token — in a log line, an error message, a
  crash dump, or a support-bundle command this documentation suggests running.

If you think your token has leaked, ask for a rotation. It is a one-line operation
on our side and the old token stops working immediately.

---

## For ADSBNG engineers

The mirror of this document, covering the gateway's own threat model, its
dependency surface, and what a gateway compromise would and would not reach, is
`docs/SECURITY.md` in the private `adsbng-ingest` repository. The two are meant to
be read together; this one is the half a contributor can verify.
