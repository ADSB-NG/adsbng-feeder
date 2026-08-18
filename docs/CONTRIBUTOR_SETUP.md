# Contributor setup

Getting your receiver feeding ADSBNG, from a blank Pi to a working station.

Total time: about fifteen minutes, most of it waiting for `apt`.

**Prerequisites**

- A Linux machine with systemd — Raspberry Pi OS, Debian, or Ubuntu.
- An RTL-SDR (or similar) with an antenna, receiving 1090 MHz.
- Three values from ADSBNG: your **Receiver ID**, your **station token**, and the
  **gateway address**. If you do not have them, ask before going further; the
  feeder cannot connect without them.

**Not required, at any point:** Node.js, npm, any database server, Docker, the
ADSBNG decoder, the ADSBNG analytics code, or any ADSBNG source beyond this
repository. If a setup guide ever tells you to install one of those to feed
ADSBNG, it is not from us.

---

## Step 1 — get a decoder producing Beast output

The feeder does not talk to your SDR. It reads the Beast stream your decoder
already produces, so you need a working decoder first. If you already feed
FlightAware, FlightRadar24, adsb.fi, or run readsb for your own map, you almost
certainly have this and can skip to step 2 — check with:

```bash
ss -ltnp | grep 30005
```

Output naming `readsb`, `dump1090`, or `dump1090-fa` on port 30005 means you are
done here.

### If you are starting from nothing

The simplest route is [adsb.im](https://adsb.im) — a prebuilt Raspberry Pi image
that includes readsb, configures Beast output on 30005 by default, and gives you
a web interface for the SDR gain and your location. Write the image, boot it, set
your coordinates, and confirm aircraft are appearing on its local map.

On an existing Debian or Ubuntu install, the readsb or dump1090-fa packages both
work. Whichever you use, the requirement is the same: **Beast output on TCP
127.0.0.1:30005**.

readsb needs `--net` and the Beast output port enabled. In
`/etc/default/readsb`, the relevant part of `NET_OPTIONS` looks like:

```
NET_OPTIONS="--net --net-heartbeat=60 --net-ro-size=1280 --net-ro-interval=0.2 --net-bo-port=30005"
```

`--net-bo-port=30005` is the one that matters — `bo` is "Beast out". Then:

```bash
sudo systemctl restart readsb
ss -ltnp | grep 30005
```

### Confirm real data is flowing

Before installing anything else, check that the port produces bytes:

```bash
timeout 5 nc 127.0.0.1 30005 | wc -c
```

A number in the thousands means your receiver is working. `0` means the decoder
is running but seeing no aircraft — check your antenna and gain before blaming
the feeder. Beast is a binary protocol, so do not expect readable text.

---

## Step 2 — install the feeder

From a release archive, or a clone of this repository:

```bash
# only if you are installing from source rather than a release archive
./scripts/build.sh

sudo ./install.sh
```

The installer prompts for what it needs:

```
Receiver ID (e.g. LAGOS_01) : LAGOS_01
Station token               : adsbng_…
Gateway [ingest.adsbng.app:443] :
Beast source [127.0.0.1:30005] :
```

Press Enter to accept either default. To script it instead, pass the values as
flags — and prefer `--token-file` over `--token`, because a token on a command
line is visible in `ps` and lands in your shell history:

```bash
printf '%s' 'adsbng_…' | sudo tee /root/token.txt >/dev/null
sudo chmod 600 /root/token.txt
sudo ./install.sh \
  --station-id LAGOS_01 \
  --token-file /root/token.txt \
  --gateway ingest.adsbng.app:443
sudo shred -u /root/token.txt
```

`sudo ./install.sh --help` lists every option.

### What the installer does

- Detects your CPU architecture and installs the matching binary to
  `/usr/local/bin/adsbng-feeder`.
- Creates the `adsbng` system user — no login shell, no home directory, no sudo.
- Creates `/etc/adsbng-feeder/` (mode `0750`) and writes `config.toml` inside it
  as **mode `0600`, owned by `adsbng`**. That file holds your token, and file
  permissions are what protect it.
- Installs `/etc/systemd/system/adsbng-feeder.service`, hardened as described in
  [SECURITY.md](SECURITY.md).
- Validates the config, then enables and starts the service.
- Copies this documentation to `/usr/share/doc/adsbng-feeder/`.

### What the installer does not do

It does not download unrelated software, does not install any ADSBNG application
code, does not ask for database credentials, and does not touch your readsb
configuration. It has no network access at all unless you explicitly set
`ADSBNG_DOWNLOAD_BASE` to fetch a binary.

---

## Step 3 — confirm you are connected

```bash
sudo systemctl status adsbng-feeder
sudo journalctl -u adsbng-feeder -n 20
```

What success looks like:

```
adsbng-feeder 0.1.0 starting: station=LAGOS_01 gateway=ingest.adsbng.app:443 beast=127.0.0.1:30005 token=adsb…<50 chars> ca_file="" insecure_skip_verify=false
connected to local Beast source 127.0.0.1:30005
authenticated to gateway ingest.adsbng.app:443 — server=adsbng-ingest/0.1.0 receiver_id=LAGOS_01
status: frames=1204 bytes=18060 discarded=0 uptime=30s
```

Three lines and then a `status:` counter every thirty seconds. That is the whole
steady state — the feeder logs no individual frames, so an `active (running)`
service with a rising `frames=` count is a working station. It is designed to be
boring.

If instead you see repeated connection attempts, or the service is `activating
(auto-restart)`, go to the troubleshooting table in
[OPERATIONS.md](OPERATIONS.md#when-something-is-wrong) — every failure mode the
feeder can produce is listed there with the log line that identifies it.

That's it. Your station now feeds ADSBNG on every boot.

---

## Notes on a few things people ask about

**The Receiver ID in my config is only a label?**  Yes. Your identity comes from
your token; the gateway looks the token up and uses whatever receiver_id it is
registered against. `station_id` in your config is sent along as a diagnostic
hint and nothing more. Typo it and you mislabel your own log lines — your data is
still attributed correctly. This is deliberate: it means no station can claim to
be another one, including by accident.

**Do I need to open a port on my router?**  No. Both of the feeder's connections
are outbound — one to your local decoder, one to the gateway. It never listens on
a socket, so there is nothing to forward and nothing new exposed on your network.
CGNAT is fine.

**My receiver is on a different machine from the feeder.**  Point `beast_source`
at it: `beast_source = "192.168.1.50:30005"`. Remember that Beast is
unauthenticated and unencrypted, so keep that hop on your own LAN.

**Can I feed ADSBNG and other aggregators at the same time?**  Yes. Multiple
clients can read the same Beast port; readsb is built for it. Nothing here
interferes with your existing feeds.

**How much bandwidth does this use?**  Beast is compact — roughly 15 bytes per
frame. A busy station runs at a few kilobits per second, on the order of tens of
megabytes a day. It is not a meaningful load on any normal connection.

**How do I stop feeding?**

```bash
sudo systemctl disable --now adsbng-feeder
```

To remove it entirely, also delete `/usr/local/bin/adsbng-feeder`,
`/etc/systemd/system/adsbng-feeder.service`, `/etc/adsbng-feeder/`, and
`/usr/share/doc/adsbng-feeder/`, then `sudo systemctl daemon-reload`. Tell us as
well, so we can revoke the token and stop showing your station as expected-online.

**I think my token leaked.**  Contact ADSBNG and ask for a rotation. Issuing you
a new token invalidates the old one immediately; you paste the new one into
`config.toml` and restart. A leaked token lets someone submit fake frames as your
station and nothing else — it grants no access to ADSBNG systems or to any other
contributor's data — but it should still be replaced.

---

## Where to read next

- [OPERATIONS.md](OPERATIONS.md) — running it, reading the logs, fixing problems.
- [SECURITY.md](SECURITY.md) — exactly what your token can and cannot do, and
  what ADSBNG can and cannot do to your machine. Worth reading before you run
  someone else's software as a service on your network.
