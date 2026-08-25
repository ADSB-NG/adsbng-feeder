// Command adsbng-feeder forwards a local ADS-B Beast stream to the ADSBNG
// contributor ingest gateway over authenticated TLS.
//
// It is deliberately small and contains no ADS-B decoding, no analytics, no
// database access, and no ADSBNG infrastructure credentials. See docs/SECURITY.md.
//
// Usage:
//
//	adsbng-feeder [--config PATH]
//	adsbng-feeder --check-config [--config PATH]
//	adsbng-feeder --verify [--config PATH]
//	adsbng-feeder --version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/adsbng/adsbng-feeder/internal/config"
	"github.com/adsbng/adsbng-feeder/internal/feeder"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0-dev"

// verifyTimeout bounds the whole --verify probe (TLS dial + Hello/Ack).
const verifyTimeout = 30 * time.Second

func main() {
	var (
		cfgPath     = flag.String("config", config.DefaultPath, "path to config file")
		checkConfig = flag.Bool("check-config", false, "validate configuration and exit")
		doVerify    = flag.Bool("verify", false, "verify the station token with the gateway and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("adsbng-feeder %s\n", version)
		return
	}

	// Log to stdout: journald captures it, and keeping stderr clean means a
	// non-empty stderr is a real problem worth alerting on.
	lg := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// Config errors are the single most common support case, so make the
		// remedy explicit rather than just naming the failure.
		fmt.Fprintf(os.Stderr, "adsbng-feeder: configuration error: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nExpected a config file at %s, for example:\n\n", *cfgPath)
		fmt.Fprint(os.Stderr, exampleConfig)
		fmt.Fprintf(os.Stderr, "\nEquivalent environment variables: ADSBNG_STATION_ID, "+
			"ADSBNG_STATION_TOKEN, ADSBNG_INGEST_HOST, ADSBNG_INGEST_PORT, BEAST_SOURCE\n")
		os.Exit(2)
	}

	if *checkConfig {
		fmt.Printf("configuration OK: %s\n", cfg.Redacted())
		if cfg.InsecureSkipVerify {
			fmt.Println("\nWARNING: insecure_skip_verify = true — TLS verification is DISABLED.")
			fmt.Println("This is DEVELOPMENT ONLY. Never run a real station this way.")
		}
		return
	}

	if *doVerify {
		// Authenticate to the gateway and report whether this is an active
		// station, without opening the local Beast source or streaming anything.
		// install.sh runs this before it enables the service. The exit codes are
		// contractual: 0 active, 3 rejected (unknown/revoked), 4 unreachable.
		vctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
		defer cancel()
		receiverID, verr := feeder.New(cfg, version, lg).Verify(vctx)
		switch {
		case verr == nil:
			fmt.Printf("verified: the gateway accepted this station as %s\n", receiverID)
			return
		case errors.Is(verr, feeder.ErrGatewayRejected):
			fmt.Fprintln(os.Stderr, "verification failed: the gateway rejected this token "+
				"(unknown or revoked — not an active station).")
			os.Exit(3)
		default:
			fmt.Fprintf(os.Stderr, "verification could not complete: %v\n", verr)
			fmt.Fprintln(os.Stderr, "The gateway could not be reached, so the token was not "+
				"checked — usually a network, DNS, TLS, or firewall problem, not a bad token.")
			os.Exit(4)
		}
	}

	// SIGINT/SIGTERM cancel the context so systemd stop/restart is clean.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	f := feeder.New(cfg, version, lg)
	if err := f.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		lg.Printf("exiting: %v", err)
		os.Exit(1)
	}
	lg.Printf("shutdown complete")
}

const exampleConfig = `  # /etc/adsbng-feeder/config.toml   (chmod 600)
  station_id   = "LAGOS_01"
  token        = "<the station token ADSBNG issued you>"
  gateway      = "ingest.adsbng.app:443"
  beast_source = "127.0.0.1:30005"
`
