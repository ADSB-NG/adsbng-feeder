# Operations — ADSBNG feeder

Running the feeder day to day, reading its logs, and diagnosing the things that
actually go wrong.

---

## The commands

```bash
sudo systemctl status adsbng-feeder      # is it running?
sudo systemctl restart adsbng-feeder     # after a config change
sudo systemctl stop adsbng-feeder        # stop until next boot
sudo systemctl start adsbng-feeder
sudo systemctl disable --now adsbng-feeder   # stop feeding permanently
sudo journalctl -u adsbng-feeder -f      # follow the log
sudo journalctl -u adsbng-feeder -n 50   # last 50 lines
sudo journalctl -u adsbng-feeder --since '1 hour ago'

adsbng-feeder --version
adsbng-feeder --check-config             # validate /etc/adsbng-feeder/config.toml
adsbng-feeder --check-config --config /path/to/other.toml
```

`--check-config` parses and validates without connecting to anything, and prints
the resolved configuration with the token masked. It is safe to run at any time,
and the systemd unit runs it as `ExecStartPre`, so a broken config fails at start
with a legible message instead of a restart loop.

## Changing the configuration

```bash
sudo -e /etc/adsbng-feeder/config.toml         # or your editor of choice
sudo adsbng-feeder --check-config
sudo systemctl restart adsbng-feeder
```

Two things to keep right:

- **Permissions.** The file holds your token. It must stay `0600` and owned by
  `adsbng`. Some editors write a new file and lose the mode:
  ```bash
  sudo chown adsbng:adsbng /etc/adsbng-feeder/config.toml
  sudo chmod 600           /etc/adsbng-feeder/config.toml
  ```
- **Unknown keys are errors.** A typo'd key name is a startup failure, not a
  silently ignored line. That is deliberate — `toekn = "…"` quietly doing nothing
  is far worse to debug than a clear refusal to start.

Every key also has an environment variable, and the environment wins over the
file. If a setting appears not to take effect, check for a systemd drop-in:

```bash
sudo systemctl show adsbng-feeder -p Environment
sudo systemd-delta --type=extended | grep adsbng
```

## Reading the log

A healthy start, in order:

```
adsbng-feeder 0.1.0 starting: station=LAGOS_01 gateway=ingest.adsbng.app:443 beast=127.0.0.1:30005 token=adsb…<50 chars> ca_file="" insecure_skip_verify=false
connected to local Beast source 127.0.0.1:30005
authenticated to gateway ingest.adsbng.app:443 — server=adsbng-ingest/0.1.0 receiver_id=LAGOS_01
status: frames=1204 bytes=18060 discarded=0 uptime=30s
```

The `status:` line repeats every thirty seconds. Read it as follows:

| Field | Meaning |
| --- | --- |
| `frames` | Beast frames forwarded to the gateway since this connection opened |
| `bytes` | those frames in bytes — Beast averages about 15 bytes a frame |
| `discarded` | bytes your local source produced that were not valid Beast framing |
| `uptime` | how long the current gateway session has lasted |

`frames` rising is the one number that matters. A small non-zero `discarded` after
a restart is normal — the feeder joins your decoder's stream mid-frame and drops
bytes until it finds the next frame boundary. `discarded` climbing steadily while
`frames` stays flat means the port you configured is not producing Beast.

Two lines you may also see, both benign:

```
note: gateway assigned receiver_id="LAGOS_01", local station_id label is "LAGOS_1" (the gateway's value is authoritative)
```
Your `station_id` does not match the receiver_id your token is registered
against. Not an error — identity comes from the token — but it means your local
label is a typo. Fix it so your own logs read correctly.

```
session ended after 4h12m3s: read from gateway: …
reconnecting in 1s
```
The connection dropped and is being retried. Backoff starts at 1 second and
doubles to a 30-second ceiling; a session that survived more than a minute resets
it, so a long-lived station recovers from a blip immediately rather than waiting
out an old backoff.

---

## When something is wrong

Find your log line in the left column.

### `no data from Beast source 127.0.0.1:30005 for 2m0s (is readsb running and is it configured for Beast output?)`

The feeder connected but your decoder sent nothing for two minutes.

```bash
systemctl status readsb        # or dump1090-fa
ss -ltnp | grep 30005
timeout 5 nc 127.0.0.1 30005 | wc -c
```

`0` bytes from that last command with the decoder running means your receiver is
seeing no aircraft — an antenna, cable, or gain problem, not a feeder problem. At
night in quiet airspace two minutes with no frames is possible; the feeder will
reconnect and carry on.

### `beast source 127.0.0.1:30005: connection refused`

Nothing is listening. Your decoder is not running, or its Beast output port is
disabled, or it is on another address. See
[CONTRIBUTOR_SETUP.md step 1](CONTRIBUTOR_SETUP.md#step-1--get-a-decoder-producing-beast-output).

### `local source does not look like a Beast stream (176 unparseable bytes buffered) — is 127.0.0.1:30005 really a Beast output port?`

Something is listening and sending data, but it is not Beast. The usual cause is
pointing at the wrong port: readsb serves several formats, and 30003 is SBS/BaseStation
text, not Beast. You want the `--net-bo-port`, conventionally **30005**.

### `gateway rejected this station: unauthorized`

The gateway declined your token. In order of likelihood:

1. The token is wrong — a truncated copy/paste, or a stray quote inside the
   quoted value. Check the length: `sudo grep -c . /etc/adsbng-feeder/config.toml`
   will not tell you, but `adsbng-feeder --check-config` prints
   `token=adsb…<N chars>`, and ADSBNG can tell you what `N` should be.
2. Your station was revoked. Contact ADSBNG.
3. You are using a token that has since been rotated.

The message is deliberately vague, and ADSBNG cannot make it more specific: the
gateway does not distinguish "no such token" from "revoked token" on the wire,
because that difference would let anyone probe which tokens exist. When in doubt,
ask us — we can see which of the three it is from our side.

### `gateway ingest.adsbng.app:443: x509: certificate signed by unknown authority`

TLS verification failed, and the feeder refused to proceed rather than hand your
token to an unverified server. Either your system trust store is stale
(`sudo apt install --reinstall ca-certificates`), your clock is badly wrong
(`timedatectl` — an expired-or-not-yet-valid certificate looks like this), or
something is intercepting the connection. Do **not** reach for
`insecure_skip_verify` to make this go away; it turns a refusal to leak your token
into a decision to leak it. If ADSBNG has asked you to run against a private CA,
set `ca_file` instead, which keeps verification on.

### `gateway ingest.adsbng.app:443: dial tcp: i/o timeout`

Outbound TCP to the gateway port is blocked, or the address is wrong.

```bash
getent hosts ingest.adsbng.app
timeout 10 nc -vz ingest.adsbng.app 443
```

Nothing needs to be forwarded *inbound* for the feeder to work — this is purely
about outbound reachability.

### `WARNING: insecure_skip_verify is ENABLED — TLS certificate verification is OFF.`

Exactly what it says, and it appears on every connection. Remove
`insecure_skip_verify` from your config and restart. This option exists for the
project's own acceptance tests against a throwaway certificate; on a real station
it means anyone able to intercept your traffic can impersonate the gateway and
collect your token.

### The service will not start at all

```bash
sudo systemctl status adsbng-feeder
sudo journalctl -u adsbng-feeder -n 30
sudo adsbng-feeder --check-config
```

`ExecStartPre` runs `--check-config`, so a configuration error shows up as a
`status=2` exit with the reason on the line above. `missing required
configuration: token (ADSBNG_STATION_TOKEN)` means the token is empty or the file
was not found at all — check that `/etc/adsbng-feeder/config.toml` exists and is
readable by the `adsbng` user.

### It runs but ADSBNG says my station is offline

Check the `status:` line's `frames=` counter. If it is rising, the feeder is
delivering data and the problem is downstream of you — tell us, with a copy of
one of those lines. If `frames=` is not rising, work back up this list.

---

## Resource use

Steady state is around 10 MB of RSS and a fraction of a percent of one core; the
feeder does no decoding, only framing and forwarding. Bandwidth is a few kilobits
per second — tens of megabytes a day for a busy station.

```bash
systemctl status adsbng-feeder | grep Memory
```

If you see growth over days, that is a bug worth reporting. There are two bounded
buffers in the whole program and neither is allowed to grow: the local read buffer
is fixed, and the frame-boundary carry is capped at four frames' worth before the
feeder gives up and reports that the source is not Beast.

## What is installed where

| Path | |
| --- | --- |
| `/usr/local/bin/adsbng-feeder` | the binary |
| `/etc/adsbng-feeder/config.toml` | your config and token, mode `0600`, owned by `adsbng` |
| `/etc/systemd/system/adsbng-feeder.service` | the unit |
| `/usr/share/doc/adsbng-feeder/` | this documentation and `config.toml.example` |

No state, cache, or spool directory: the feeder writes nothing to disk, and the
systemd unit gives it no writable path at all. Restarting it loses nothing but the
frames in flight.

## Upgrading

Install the new binary over the old one and restart:

```bash
sudo ./install.sh          # keeps your existing config.toml
sudo systemctl restart adsbng-feeder
adsbng-feeder --version
```

`install.sh` will not overwrite an existing `config.toml` unless you pass
`--force-config`, so an upgrade cannot lose your token.

## Reporting a problem

Include the output of these three, which contain no secrets — the feeder masks
the token everywhere it prints, and refuses to log anything token-shaped:

```bash
adsbng-feeder --version
adsbng-feeder --check-config
sudo journalctl -u adsbng-feeder -n 50 --no-pager
```

Read the output before sending it anyway. **Never** paste your `config.toml`
itself into an issue, a forum, or a support thread — that file has your token in
plaintext.
