// Package config loads and validates the feeder's configuration.
//
// Sources, lowest precedence first:
//
//  1. built-in defaults
//  2. the config file (default /etc/adsbng-feeder/config.toml)
//  3. environment variables (ADSBNG_*, BEAST_SOURCE)
//
// Within the file, keys are applied in the fixed order given by fileKeys rather
// than in the order they appear, so a broad key and a narrow one that overlap
// (`gateway` and `gateway_port`) always resolve the same way.
//
// The file format is a deliberate subset of TOML: flat `key = value` pairs,
// `#` comments, optional quotes. No tables, no arrays. Parsing it takes ~60
// lines of stdlib, which is cheaper than adding a dependency to a binary that
// runs on other people's machines. Anything fancier than this subset is a
// config error rather than a silently ignored line.
package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

// DefaultPath is where the installer writes the config file.
const DefaultPath = "/etc/adsbng-feeder/config.toml"

// Config is the feeder's runtime configuration.
type Config struct {
	// StationID is a local label, sent to the gateway only as a diagnostic
	// hint. It does NOT establish identity — the gateway derives the real
	// receiver_id from the token. Getting this wrong mislabels your own logs
	// and nothing else.
	StationID string

	// Token authenticates this station. High-entropy, issued by ADSBNG,
	// shown once at provisioning time. Treat it like a password.
	Token string

	// Gateway is the ADSBNG ingest endpoint as host:port.
	GatewayHost string
	GatewayPort int

	// BeastSource is the local Beast TCP source, typically readsb on
	// 127.0.0.1:30005.
	BeastHost string
	BeastPort int

	// CAFile, when set, is a PEM bundle used INSTEAD of the system roots.
	// This is the supported way to run against a private/self-signed CA:
	// certificate verification stays fully enabled.
	CAFile string

	// InsecureSkipVerify disables TLS certificate verification.
	//
	// DEVELOPMENT ONLY. This makes the connection trivially interceptable and
	// leaks the station token to whoever answers the port. It must never be
	// set on a real station; the feeder logs a loud warning every time it
	// connects with this enabled.
	InsecureSkipVerify bool
}

// Gateway returns the gateway endpoint as host:port.
func (c *Config) Gateway() string {
	return net.JoinHostPort(c.GatewayHost, strconv.Itoa(c.GatewayPort))
}

// BeastAddr returns the local Beast source as host:port.
func (c *Config) BeastAddr() string {
	return net.JoinHostPort(c.BeastHost, strconv.Itoa(c.BeastPort))
}

// Redacted returns a one-line summary safe for logs: everything except the
// token, which is never printed in full anywhere in this program.
func (c *Config) Redacted() string {
	return fmt.Sprintf("station=%s gateway=%s beast=%s token=%s ca_file=%q insecure_skip_verify=%t",
		c.StationID, c.Gateway(), c.BeastAddr(), MaskToken(c.Token), c.CAFile, c.InsecureSkipVerify)
}

// MaskToken renders a token for human eyes without disclosing it. Only the
// length and a 4-character prefix survive, which is enough to tell two tokens
// apart in a support conversation and not enough to replay one.
func MaskToken(tok string) string {
	if tok == "" {
		return "<unset>"
	}
	if len(tok) <= 4 {
		return fmt.Sprintf("<%d chars>", len(tok))
	}
	return fmt.Sprintf("%s…<%d chars>", tok[:4], len(tok))
}

func defaults() *Config {
	return &Config{
		GatewayPort: 443,
		BeastHost:   "127.0.0.1",
		BeastPort:   30005,
	}
}

// Load reads the config file at path (missing file is not an error — env alone
// can be enough), applies environment overrides, then validates.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		kv, err := parseFile(path)
		if err != nil {
			return nil, err
		}
		if err := cfg.applyFile(kv, path); err != nil {
			return nil, err
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseFile reads the flat key=value subset described in the package comment.
// A nil map with a nil error means the file does not exist.
func parseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	kv := make(map[string]string)
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") {
			return nil, fmt.Errorf("%s:%d: TOML tables are not supported (flat key = value only)", path, line)
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, line)
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, line)
		}
		val, err := parseValue(s[eq+1:])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %w", path, line, key, err)
		}
		kv[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return kv, nil
}

// parseValue interprets the right-hand side of a `key = value` line.
//
// Two shapes are supported, which between them cover what people actually type:
//
//	key = value      -- bare; a '#' begins a comment
//	key = "value"    -- quoted, so spaces and '#' are data, and a comment may
//	                    follow the closing quote
//
// An unterminated quote is an error rather than a literal. A stray quote
// silently embedded in a token produces an authentication failure at the
// gateway with nothing in the feeder's log to explain it, which is the worst
// possible way for this to go wrong.
func parseValue(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	if q := raw[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(raw[1:], q)
		if end < 0 {
			return "", fmt.Errorf("unterminated %c quote", q)
		}
		val := raw[1 : 1+end]
		switch trailer := strings.TrimSpace(raw[end+2:]); {
		case trailer == "", strings.HasPrefix(trailer, "#"):
			return val, nil
		default:
			// Deliberately does not echo the trailer: on a `token = ...` line
			// anything we print risks ending up in a log or a support ticket.
			return "", fmt.Errorf("unexpected text after the closing %c quote", q)
		}
	}

	if h := strings.IndexByte(raw, '#'); h >= 0 {
		raw = strings.TrimSpace(raw[:h])
	}
	return raw, nil
}

// fileKeys lists every accepted config-file key, the order keys are applied in,
// and which names are aliases for one another.
//
// The order has to be stated somewhere explicit. Ranging over the parsed map
// would leave it to Go's randomized map iteration, so a file setting both
// `gateway = "host:443"` and `gateway_port = 8443` would pick a different port
// from one restart to the next. That presents as intermittent connection
// failures on a machine we cannot log into, which is about the worst shape a bug
// can take in this program.
//
// Broad keys come before the narrow ones that overlap them, so the narrower key
// wins: `gateway` sets host and port together, then gateway_host/gateway_port
// override either half. `gateway = "ingest.adsbng.app"` with `gateway_port =
// 8443` therefore means what it looks like it means, and so does the same pair
// with the port written into `gateway`.
var fileKeys = []struct {
	// names[0] is canonical; the rest are accepted aliases, because the
	// provisioning output and earlier drafts spelled some of these differently.
	names []string
	apply func(c *Config, v string) error
}{
	{[]string{"receiver_id", "station_id"}, func(c *Config, v string) error {
		// Only a local label under either name; see Config.StationID.
		c.StationID = v
		return nil
	}},
	{[]string{"token", "station_token"}, func(c *Config, v string) error {
		c.Token = v
		return nil
	}},
	{[]string{"gateway"}, func(c *Config, v string) error {
		host, port, err := splitHostPort(v, 443)
		if err != nil {
			return err
		}
		c.GatewayHost, c.GatewayPort = host, port
		return nil
	}},
	{[]string{"gateway_host", "ingest_host"}, func(c *Config, v string) error {
		c.GatewayHost = v
		return nil
	}},
	{[]string{"gateway_port", "ingest_port"}, func(c *Config, v string) error {
		n, err := atoiPort(v)
		if err != nil {
			return err
		}
		c.GatewayPort = n
		return nil
	}},
	{[]string{"beast_source"}, func(c *Config, v string) error {
		host, port, err := splitHostPort(v, 30005)
		if err != nil {
			return err
		}
		c.BeastHost, c.BeastPort = host, port
		return nil
	}},
	{[]string{"beast_host"}, func(c *Config, v string) error {
		c.BeastHost = v
		return nil
	}},
	{[]string{"beast_port"}, func(c *Config, v string) error {
		n, err := atoiPort(v)
		if err != nil {
			return err
		}
		c.BeastPort = n
		return nil
	}},
	{[]string{"ca_file"}, func(c *Config, v string) error {
		c.CAFile = v
		return nil
	}},
	{[]string{"insecure_skip_verify"}, func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("not a boolean: %q", v)
		}
		c.InsecureSkipVerify = b
		return nil
	}},
}

// fileKeyNames is every name fileKeys accepts, for the unknown-key check.
var fileKeyNames = func() map[string]bool {
	m := make(map[string]bool)
	for _, key := range fileKeys {
		for _, name := range key.names {
			m[name] = true
		}
	}
	return m
}()

func atoiPort(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", v)
	}
	return n, nil
}

func (c *Config) applyFile(kv map[string]string, path string) error {
	if kv == nil {
		return nil
	}

	// Unknown keys are rejected rather than ignored: a typo'd `toekn = ...`
	// that silently does nothing is far worse to debug than a startup error.
	// Sorted, so a file with two typos always names the same one first.
	var unknown []string
	for k := range kv {
		if !fileKeyNames[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: unknown config key %q", path, unknown[0])
	}

	for _, key := range fileKeys {
		var set []string
		for _, name := range key.names {
			if _, ok := kv[name]; ok {
				set = append(set, name)
			}
		}
		// Two names for one setting cannot both be present. Unlike the
		// broad/narrow pairs above, pure aliases have no precedence between them
		// that would not be a coin toss, so this is a config error.
		if len(set) > 1 {
			return fmt.Errorf("%s: %q and %q are two names for the same setting; use only one",
				path, set[0], set[1])
		}
		if len(set) == 0 {
			continue
		}
		if err := key.apply(c, kv[set[0]]); err != nil {
			return fmt.Errorf("%s: %s: %w", path, set[0], err)
		}
	}
	return nil
}

func (c *Config) applyEnv() error {
	if v := os.Getenv("ADSBNG_STATION_ID"); v != "" {
		c.StationID = v
	}
	if v := os.Getenv("ADSBNG_STATION_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("ADSBNG_INGEST_HOST"); v != "" {
		c.GatewayHost = v
	}
	if v := os.Getenv("ADSBNG_INGEST_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("ADSBNG_INGEST_PORT: not a number: %q", v)
		}
		c.GatewayPort = n
	}
	if v := os.Getenv("BEAST_SOURCE"); v != "" {
		host, port, err := splitHostPort(v, 30005)
		if err != nil {
			return fmt.Errorf("BEAST_SOURCE: %w", err)
		}
		c.BeastHost, c.BeastPort = host, port
	}
	if v := os.Getenv("ADSBNG_CA_FILE"); v != "" {
		c.CAFile = v
	}
	if v := os.Getenv("ADSBNG_INSECURE_SKIP_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("ADSBNG_INSECURE_SKIP_VERIFY: not a boolean: %q", v)
		}
		c.InsecureSkipVerify = b
	}
	return nil
}

// Validate checks that the configuration can actually be used to connect.
func (c *Config) Validate() error {
	var missing []string
	if c.Token == "" {
		missing = append(missing, "token (ADSBNG_STATION_TOKEN)")
	}
	if c.GatewayHost == "" {
		missing = append(missing, "gateway host (ADSBNG_INGEST_HOST)")
	}
	if c.BeastHost == "" {
		missing = append(missing, "beast host (BEAST_SOURCE)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if err := checkPort("gateway port", c.GatewayPort); err != nil {
		return err
	}
	if err := checkPort("beast port", c.BeastPort); err != nil {
		return err
	}
	if c.CAFile != "" {
		if _, err := os.Stat(c.CAFile); err != nil {
			return fmt.Errorf("ca_file %s: %w", c.CAFile, err)
		}
	}
	if c.CAFile != "" && c.InsecureSkipVerify {
		return fmt.Errorf("ca_file and insecure_skip_verify are mutually exclusive: " +
			"pin the CA (verification on) or skip verification, not both")
	}
	return nil
}

func checkPort(what string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("%s out of range: %d", what, p)
	}
	return nil
}

// splitHostPort accepts "host", "host:port", or a bracketed IPv6 literal, and
// falls back to defPort when no port is given.
func splitHostPort(s string, defPort int) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("empty address")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// No port present (or an IPv6 literal without brackets).
		return strings.Trim(s, "[]"), defPort, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("bad port %q", portStr)
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty host in %q", s)
	}
	return host, port, nil
}
