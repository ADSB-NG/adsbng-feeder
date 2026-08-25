// Package feeder implements the feeder's run loop: read Beast frames from a
// local source, forward them to the ADSBNG gateway over authenticated TLS, and
// reconnect on failure.
//
// The feeder never listens on a socket. Both connections are outbound, which is
// what lets it work behind NAT/CGNAT with no port forwarding and no VPN.
package feeder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adsbng/adsbng-feeder/internal/beast"
	"github.com/adsbng/adsbng-feeder/internal/config"
	"github.com/adsbng/adsbng-feeder/internal/proto"
)

// Backoff bounds for reconnecting to either endpoint. These mirror the ADSBNG
// decoder's own 1s→30s schedule so operators see familiar timings.
const (
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second

	// dialTimeout bounds a single connection attempt.
	dialTimeout = 20 * time.Second

	// handshakeTimeout bounds Hello→Ack.
	handshakeTimeout = 20 * time.Second

	// beastReadTimeout: a live receiver is never silent this long. Tripping it
	// means the local Beast source is wedged, so we cycle the connection.
	beastReadTimeout = 120 * time.Second

	// readBufSize is the Beast socket read size.
	readBufSize = 16 << 10

	// flushInterval bounds how long a frame waits before being sent. Beast is
	// a low-rate stream; batching briefly cuts TLS record and syscall overhead
	// without adding latency that matters for aggregate analytics.
	flushInterval = 250 * time.Millisecond
)

// ErrGatewayRejected reports that the gateway completed the handshake but refused
// the station: the token is unknown or the station is not active. It is distinct
// from a transport failure (dial/TLS/timeout/EOF), where the token's validity is
// simply unknown. Callers tell the two apart with errors.Is(err, ErrGatewayRejected).
var ErrGatewayRejected = errors.New("gateway rejected this station")

// Feeder forwards one local Beast stream to the gateway.
type Feeder struct {
	cfg     *Config
	version string
	log     *log.Logger

	// Cumulative counters across the process lifetime, for heartbeats and the
	// periodic status line.
	framesTotal    atomic.Uint64
	bytesTotal     atomic.Uint64
	discardedTotal atomic.Uint64
	started        time.Time
}

// Config aliases config.Config so callers need only this package.
type Config = config.Config

// New constructs a Feeder.
func New(cfg *Config, version string, lg *log.Logger) *Feeder {
	if lg == nil {
		lg = log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	}
	return &Feeder{cfg: cfg, version: version, log: lg, started: time.Now()}
}

// Run connects and forwards until ctx is cancelled. It returns only on
// cancellation: every other failure is retried with exponential backoff, because
// a feeder that exits on a transient network fault is a feeder that stops
// contributing until someone notices.
func (f *Feeder) Run(ctx context.Context) error {
	if f.cfg.InsecureSkipVerify {
		f.log.Printf("WARNING: insecure_skip_verify is ENABLED — TLS certificate " +
			"verification is OFF. This is DEVELOPMENT ONLY. Your station token is " +
			"exposed to anyone who can intercept this connection. Do not run a real " +
			"station like this.")
	}
	f.log.Printf("adsbng-feeder %s starting: %s", f.version, f.cfg.Redacted())

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := f.session(ctx)

		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			f.log.Printf("session ended after %s: %v", time.Since(start).Round(time.Second), err)
		default:
			f.log.Printf("session ended after %s", time.Since(start).Round(time.Second))
		}

		// A session that survived a while is evidence the endpoints are healthy,
		// so restart from the minimum delay instead of a stale long backoff.
		if time.Since(start) > 2*backoffMax {
			backoff = backoffMin
		}

		f.log.Printf("reconnecting in %s", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// session runs one full attempt: connect both ends, authenticate, pump frames.
func (f *Feeder) session(ctx context.Context) error {
	// Local Beast source first — no point authenticating to the gateway if we
	// have nothing to send, and this avoids burning auth attempts.
	bconn, err := f.dialBeast(ctx)
	if err != nil {
		return fmt.Errorf("beast source %s: %w", f.cfg.BeastAddr(), err)
	}
	defer bconn.Close()
	f.log.Printf("connected to local Beast source %s", f.cfg.BeastAddr())

	gconn, ack, err := f.dialGateway(ctx)
	if err != nil {
		return fmt.Errorf("gateway %s: %w", f.cfg.Gateway(), err)
	}
	defer gconn.Close()

	f.log.Printf("authenticated to gateway %s — server=%s receiver_id=%s",
		f.cfg.Gateway(), ack.Server, ack.ReceiverID)
	if ack.ReceiverID != "" && f.cfg.StationID != "" && ack.ReceiverID != f.cfg.StationID {
		// Not an error: identity is the gateway's to decide. Worth saying out
		// loud so a contributor who mistyped their local label can see why the
		// dashboard shows a different name.
		f.log.Printf("note: gateway assigned receiver_id=%q, local station_id label is %q "+
			"(the gateway's value is authoritative)", ack.ReceiverID, f.cfg.StationID)
	}

	return f.pump(ctx, bconn, gconn, ack)
}

// Verify performs only the gateway authentication handshake and reports whether
// the configured token belongs to an active station, then disconnects. Unlike
// session it does NOT open the local Beast source and streams nothing, so it can
// run during installation before a receiver is producing any data.
//
// On success it returns the gateway-assigned receiver id. On failure the error is
// either ErrGatewayRejected — a definitive "this is not an active station" — or a
// transport error, meaning the gateway could not be reached and the token's
// validity is unknown. Callers distinguish the two with errors.Is.
func (f *Feeder) Verify(ctx context.Context) (string, error) {
	conn, ack, err := f.dialGateway(ctx)
	if err != nil {
		return "", err
	}
	// The successful handshake is all we came for; close immediately, which also
	// ends the session we just established on the gateway.
	_ = conn.Close()
	return ack.ReceiverID, nil
}

// dialBeast opens the local Beast TCP source.
func (f *Feeder) dialBeast(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", f.cfg.BeastAddr())
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	return c, nil
}

// dialGateway opens the TLS connection and completes the auth handshake.
func (f *Feeder) dialGateway(ctx context.Context) (net.Conn, *proto.Ack, error) {
	tlsCfg, err := f.tlsConfig()
	if err != nil {
		return nil, nil, err
	}

	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: tlsCfg}
	c, err := d.DialContext(ctx, "tcp", f.cfg.Gateway())
	if err != nil {
		return nil, nil, err
	}

	// Bound the handshake so a black-holing endpoint cannot wedge us forever.
	if err := c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		c.Close()
		return nil, nil, err
	}

	hello := &proto.Hello{
		Proto:         proto.Version,
		Token:         f.cfg.Token,
		StationIDHint: f.cfg.StationID,
		FeederVersion: f.version,
	}
	if err := proto.WriteControl(c, hello); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}

	var ack proto.Ack
	if err := proto.ReadControl(c, &ack); err != nil {
		c.Close()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil, fmt.Errorf("gateway closed the connection during handshake " +
				"(check that the token is correct and the station is active)")
		}
		return nil, nil, fmt.Errorf("read ack: %w", err)
	}
	if !ack.OK {
		c.Close()
		// Deliberately surfaced verbatim; the gateway keeps this coarse. Wrapped
		// in ErrGatewayRejected so callers can classify it, but the rendered
		// string is unchanged ("gateway rejected this station: unauthorized").
		return nil, nil, fmt.Errorf("%w: %s", ErrGatewayRejected, nz(ack.Error, "unauthorized"))
	}

	// Clear the handshake deadline; the pump manages its own.
	if err := c.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, &ack, nil
}

// tlsConfig builds the client TLS configuration.
//
// Verification is ON by default and pinned to TLS 1.2+. Setting CAFile replaces
// the system roots with a private bundle but keeps verification on; that is the
// supported path for a local/self-signed CA. InsecureSkipVerify is the only way
// to disable verification and exists solely for development.
func (f *Feeder) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		ServerName: f.cfg.GatewayHost,
		MinVersion: tls.VersionTLS12,
	}
	if f.cfg.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true // DEVELOPMENT ONLY — warned about in Run.
		return cfg, nil
	}
	if f.cfg.CAFile != "" {
		pem, err := os.ReadFile(f.cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s contains no usable PEM certificates", f.cfg.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// pump forwards Beast frames until either side fails or ctx is cancelled.
func (f *Feeder) pump(ctx context.Context, bconn, gconn net.Conn, ack *proto.Ack) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Serialise writes to the gateway: the data path and the heartbeat
	// goroutine share one connection.
	var wmu sync.Mutex
	write := func(typ byte, body []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if err := gconn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return err
		}
		return proto.WriteMsg(gconn, typ, body)
	}

	errc := make(chan error, 3)
	var wg sync.WaitGroup

	// Close both connections as soon as anything fails, so blocked reads and
	// writes unwind instead of hanging until a timeout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		_ = bconn.SetDeadline(time.Now())
		_ = gconn.SetDeadline(time.Now())
	}()

	// Data path: Beast -> gateway.
	wg.Add(1)
	go func() {
		defer wg.Done()
		errc <- f.forward(ctx, bconn, write)
	}()

	// Heartbeats.
	hbEvery := time.Duration(ack.HeartbeatSecs) * time.Second
	if hbEvery <= 0 {
		hbEvery = 30 * time.Second
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errc <- f.heartbeat(ctx, hbEvery, write)
	}()

	// Reader: consume pongs and detect a closed or half-open gateway. Without
	// this the feeder would not notice a gateway-side close until its next write.
	wg.Add(1)
	go func() {
		defer wg.Done()
		errc <- f.readGateway(ctx, gconn)
	}()

	err := <-errc // first failure wins
	cancel()
	wg.Wait()
	return err
}

// forward reads Beast bytes, splits them on frame boundaries, and writes whole
// frames to the gateway in small batches.
func (f *Feeder) forward(ctx context.Context, bconn net.Conn, write func(byte, []byte) error) error {
	var (
		carry   []byte // partial frame carried between reads
		batch   []byte // whole frames awaiting flush
		rbuf    = make([]byte, readBufSize)
		lastOut = time.Now()
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := write(proto.MsgData, batch); err != nil {
			return fmt.Errorf("write to gateway: %w", err)
		}
		f.bytesTotal.Add(uint64(len(batch)))
		batch = batch[:0]
		lastOut = time.Now()
		return nil
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := bconn.SetReadDeadline(time.Now().Add(beastReadTimeout)); err != nil {
			return err
		}
		n, err := bconn.Read(rbuf)
		if n > 0 {
			// Frame-boundary scan over carry+new so we never emit a partial
			// frame and can count what we forward.
			buf := rbuf[:n]
			if len(carry) > 0 {
				buf = append(carry, buf...)
			}
			frames, consumed, discarded := beast.Scan(buf)
			for _, fr := range frames {
				// A single MsgData must stay under the protocol cap.
				if len(batch)+len(fr) > proto.MaxDataLen {
					if ferr := flush(); ferr != nil {
						return ferr
					}
				}
				batch = append(batch, fr...)
			}
			f.framesTotal.Add(uint64(len(frames)))
			if discarded > 0 {
				f.discardedTotal.Add(uint64(discarded))
			}

			// Retain the unconsumed tail. Copy it: it aliases buf, which the
			// next read overwrites.
			carry = append(carry[:0], buf[consumed:]...)
			if len(carry) > beast.MaxFrameLen*4 {
				// Nothing parseable in a buffer several frames deep means this
				// is not a Beast stream. Fail loudly rather than silently
				// forwarding garbage.
				return fmt.Errorf("local source does not look like a Beast stream "+
					"(%d unparseable bytes buffered) — is %s really a Beast output port?",
					len(carry), f.cfg.BeastAddr())
			}

			if len(batch) >= proto.MaxDataLen/2 || time.Since(lastOut) >= flushInterval {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("no data from Beast source %s for %s "+
					"(is readsb running and is it configured for Beast output?)",
					f.cfg.BeastAddr(), beastReadTimeout)
			}
			if errors.Is(err, io.EOF) {
				_ = flush()
				return fmt.Errorf("Beast source closed the connection")
			}
			return fmt.Errorf("read from Beast source: %w", err)
		}
	}
}

// heartbeat sends periodic liveness + counters.
func (f *Feeder) heartbeat(ctx context.Context, every time.Duration, write func(byte, []byte) error) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			hb := proto.HeartbeatPayload{
				Frames:     f.framesTotal.Load(),
				Bytes:      f.bytesTotal.Load(),
				Discarded:  f.discardedTotal.Load(),
				UptimeSecs: int64(time.Since(f.started).Seconds()),
			}
			body, err := marshalHeartbeat(hb)
			if err != nil {
				return err
			}
			if err := write(proto.MsgHeartbeat, body); err != nil {
				return fmt.Errorf("write heartbeat: %w", err)
			}
			f.log.Printf("status: frames=%d bytes=%d discarded=%d uptime=%ds",
				hb.Frames, hb.Bytes, hb.Discarded, hb.UptimeSecs)
		}
	}
}

// readGateway drains gateway->feeder messages so a close is noticed promptly.
func (f *Feeder) readGateway(ctx context.Context, gconn net.Conn) error {
	buf := make([]byte, 1024)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// No deadline: the gateway only speaks when we do. A dead peer surfaces
		// via TCP keepalive or the next write.
		if err := gconn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		typ, _, err := proto.ReadMsg(gconn, buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("gateway closed the connection")
			}
			return fmt.Errorf("read from gateway: %w", err)
		}
		if typ != proto.MsgPong {
			return fmt.Errorf("unexpected message type 0x%02x from gateway", typ)
		}
	}
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// marshalHeartbeat encodes a heartbeat body. Split out so the write path stays
// free of error-handling noise.
func marshalHeartbeat(hb proto.HeartbeatPayload) ([]byte, error) {
	b, err := json.Marshal(hb)
	if err != nil {
		return nil, fmt.Errorf("marshal heartbeat: %w", err)
	}
	return b, nil
}
