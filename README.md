# ADSBNG Feeder

A small Linux program that forwards your receiver's ADS-B data to ADSBNG.

It reads the raw Beast output your decoder already produces (readsb, dump1090-fa,
or anything else that speaks Beast), opens one outbound TLS connection to the
ADSBNG ingest gateway, authenticates with your station token, and forwards the
bytes. That is the whole job.

- **One static binary.** No Node.js, no Python, no Docker, no runtime to install.
- **Zero external dependencies.** Go standard library only — see [go.mod](go.mod).
- **Outbound only.** Nothing to port-forward, no VPN, no inbound listener. Works
  behind NAT and CGNAT.
- **~10 MB of RAM.** Fine on a Pi Zero.
- **No ADSBNG secrets.** This repository contains no database credentials, no API
  keys, and none of the ADSBNG decoder or analytics code. Read every line if you
  like — that is the point. See [docs/SECURITY.md](docs/SECURITY.md).

Supported: Debian, Ubuntu, Raspberry Pi OS, and anything else with systemd on
`amd64`, `arm64`, `armv7`, or `armv6`.

---

## What it looks like

Your machine, top to bottom:

```
┌─────────────────────────┐
│ Contributor Linux box   │
│                         │
│ readsb                  │
│     ↓                   │
│ ADSBNG feeder           │
│                         │
│ station token ONLY      │
└─────────────────────────┘
```

And where the data goes after it leaves you:

```
                    INTERNET
                       │
                       │ TLS
                       ▼
             ┌──────────────────┐
             │ ADSBNG Gateway   │
             │                  │
             │ token → receiver │
             └────────┬─────────┘
                      │
                 trusted Beast
                      │
                      ▼
             ┌──────────────────┐
             │ ADSBNG Core      │
             │ proprietary      │
             │ decoder          │
             └────────┬─────────┘
                      │
                      ▼
                  Database
```

Everything below the dashed line of the internet is ADSBNG's side. You run only
the box in the first diagram. The gateway decides which station you are from your
token — the feeder cannot claim an identity, and does not try to.

---

## Install

You need three things from ADSBNG first:

| Item | Example |
| --- | --- |
| Receiver ID | `LAGOS_01` |
| Station token | a long `adsbng_…` string, shown to you once |
| Gateway address | `ingest.adsbng.app:443` |

Then download the installer from ADSBNG and run it:

```bash
curl -fsSL https://adsbng.app/feeder/install.sh -o install.sh
sudo bash install.sh
```

It downloads the right binary for your architecture, verifies it against a
published `SHA256SUMS`, creates an unprivileged `adsbng` system user, writes
`/etc/adsbng-feeder/config.toml` with mode `0600`, installs the systemd unit,
and starts the service — prompting for the three values above if you have not
already written a config file.

Prefer to build it yourself, or work from a clone or release archive? Run the
installer from inside the tree and it uses the local binary with no download:

```bash
./scripts/build.sh          # only needed when installing from source
sudo ./install.sh
```

Full walkthrough, including how to get readsb to expose Beast on port 30005:
**[docs/CONTRIBUTOR_SETUP.md](docs/CONTRIBUTOR_SETUP.md)**.

## Check it is working

```bash
sudo systemctl status adsbng-feeder
sudo journalctl -u adsbng-feeder -f
```

A healthy feeder logs a handshake and then settles into a periodic counter line:

```
adsbng-feeder 0.1.0 starting: station=LAGOS_01 gateway=ingest.adsbng.app:443 beast=127.0.0.1:30005 token=adsb…<50 chars> ca_file="" insecure_skip_verify=false
connected to local Beast source 127.0.0.1:30005
authenticated to gateway ingest.adsbng.app:443 — server=adsbng-ingest/0.1.0 receiver_id=LAGOS_01
status: frames=1204 bytes=18060 discarded=0 uptime=30s
```

`status:` repeats every 30 seconds with the frames it has forwarded. Individual
frames are never logged. If the connection drops you will see it reconnect.

## Configuration

`/etc/adsbng-feeder/config.toml`, documented key by key in
[config.toml.example](config.toml.example):

```toml
station_id   = "LAGOS_01"
token        = "<the station token ADSBNG issued you>"
gateway      = "ingest.adsbng.app:443"
beast_source = "127.0.0.1:30005"
```

Every key has an environment-variable equivalent (`ADSBNG_STATION_ID`,
`ADSBNG_STATION_TOKEN`, `ADSBNG_INGEST_HOST`, `ADSBNG_INGEST_PORT`,
`BEAST_SOURCE`, `ADSBNG_CA_FILE`, `ADSBNG_INSECURE_SKIP_VERIFY`), which take
precedence over the file. Validate a change before restarting:

```bash
sudo adsbng-feeder --check-config --config /etc/adsbng-feeder/config.toml
sudo systemctl restart adsbng-feeder
```

## Commands

```bash
adsbng-feeder --version
adsbng-feeder --check-config [--config PATH]
adsbng-feeder --verify [--config PATH]         # confirm the token with the gateway
sudo systemctl {status,restart,stop,start} adsbng-feeder
sudo journalctl -u adsbng-feeder -f
sudo ./install.sh --stop                       # stop the service (stays installed)
sudo ./install.sh --uninstall                  # remove the feeder from this machine
```

Troubleshooting each failure mode: **[docs/OPERATIONS.md](docs/OPERATIONS.md)**.

## Documentation

| | |
| --- | --- |
| [docs/CONTRIBUTOR_SETUP.md](docs/CONTRIBUTOR_SETUP.md) | Install and first connection, start to finish |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Day-to-day running, log messages, troubleshooting |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How the feeder works internally and on the wire |
| [docs/SECURITY.md](docs/SECURITY.md) | Trust boundary and threat model, stated plainly |

## Trust boundary, in one paragraph

ADSBNG treats your machine as untrusted, and this program is built so that you
lose nothing by that. The only credential it holds is your own station token,
which grants exactly one capability: submitting ADS-B frames as your own station.
It cannot read another station's data, reach ADSBNG's database, or affect any
other contributor. Conversely, nothing in this repository gives you access to
ADSBNG's internals — not because anything is obfuscated, but because the security
boundary is enforced on the server, so there is nothing here worth hiding. The
long version, including what an attacker who fully owns your machine can and
cannot do, is in [docs/SECURITY.md](docs/SECURITY.md).

## Building from source

Go 1.24 or newer, nothing else:

```bash
./scripts/build.sh              # all architectures into ./dist
./scripts/build.sh 1.2.0        # with an explicit version string
TARGETS=linux/arm64 ./scripts/build.sh
./scripts/test.sh               # gofmt, vet, and the test suite
```

`CGO_ENABLED=0` throughout, so one machine cross-compiles every target. That is
the practical reason for the zero-dependency rule: cross-compiling for an armv6
Pi stops being straightforward the moment cgo is involved.

## Licence and support

Contact ADSBNG for support, to report a problem with the feeder, or to have your
station token rotated if you think it has leaked.
