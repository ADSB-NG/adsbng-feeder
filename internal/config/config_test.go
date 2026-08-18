package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The properties these tests defend, in order of how much damage getting them
// wrong would do:
//
//  1. TLS verification is on unless someone explicitly turns it off. A default
//     that skipped verification would hand every contributor's token to whoever
//     answers the port.
//  2. The token never reaches a log line, a diagnostic string, or an error.
//  3. A typo in the config file is an error, not a silently ignored line — the
//     feeder runs unattended on a machine whose operator cannot read our source.

// A token shaped like a real one: the provisioning CLI issues adsbng_ plus 43
// base64url characters. Fake, but the right shape for masking tests.
const sampleToken = "adsbng_QMHDBnZKcvBpZfLzOaWTAEmjrCKGwUdILoRhXeSJyPu"

// isolateEnv neutralises every environment variable Load consults, so a
// developer with ADSBNG_* exported in their shell gets the same results as CI.
// The loader treats "" as unset, which is what makes this work.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ADSBNG_STATION_ID",
		"ADSBNG_STATION_TOKEN",
		"ADSBNG_INGEST_HOST",
		"ADSBNG_INGEST_PORT",
		"BEAST_SOURCE",
		"ADSBNG_CA_FILE",
		"ADSBNG_INSECURE_SKIP_VERIFY",
	} {
		t.Setenv(k, "")
	}
}

// writeConfig puts body in a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// minimalConfig is a config file that passes validation, for tests that want to
// vary one thing.
const minimalConfig = `
receiver_id = "TEST_ABUJA"
token = "` + sampleToken + `"
gateway = "ingest.example.org:8443"
`

// ---------------------------------------------------------------------------
// the security-relevant defaults
// ---------------------------------------------------------------------------

// TestInsecureSkipVerifyIsNotTheDefault is the single most important test in
// this package. The spec forbids shipping a feeder that skips certificate
// verification by default, because doing so would make every station's token
// interceptable by anyone on the path.
func TestInsecureSkipVerifyIsNotTheDefault(t *testing.T) {
	isolateEnv(t)

	if defaults().InsecureSkipVerify {
		t.Fatal("defaults() enables InsecureSkipVerify")
	}

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load minimal config: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("a config file that does not mention insecure_skip_verify produced " +
			"a feeder with certificate verification disabled")
	}

	// And with no file at all, configured purely from the environment.
	t.Setenv("ADSBNG_STATION_TOKEN", sampleToken)
	t.Setenv("ADSBNG_INGEST_HOST", "ingest.example.org")
	envOnly, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("load from env only: %v", err)
	}
	if envOnly.InsecureSkipVerify {
		t.Error("env-only configuration produced InsecureSkipVerify=true")
	}
}

// TestInsecureSkipVerifyRequiresAnExplicitTrue proves the escape hatch exists
// but has to be asked for by name. It is the only supported way to run against
// an unverifiable certificate, and it is documented as development-only.
func TestInsecureSkipVerifyRequiresAnExplicitTrue(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, minimalConfig+"insecure_skip_verify = true\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("insecure_skip_verify = true was not honoured")
	}

	// A non-boolean must fail loudly rather than being coerced either way. A
	// value silently read as false would be a confusing outage; read as true it
	// would be a silent downgrade.
	if _, err := Load(writeConfig(t, minimalConfig+"insecure_skip_verify = yesplease\n")); err == nil {
		t.Error("a non-boolean insecure_skip_verify was accepted")
	}
}

func TestDefaultsMatchTheDocumentedContributorSetup(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// readsb's Beast output, which is what CONTRIBUTOR_SETUP.md tells people to
	// enable. If these change, that document is wrong.
	if cfg.BeastHost != "127.0.0.1" || cfg.BeastPort != 30005 {
		t.Errorf("default Beast source is %s, want 127.0.0.1:30005", cfg.BeastAddr())
	}
	if cfg.BeastAddr() != "127.0.0.1:30005" {
		t.Errorf("BeastAddr() = %q", cfg.BeastAddr())
	}
	if cfg.Gateway() != "ingest.example.org:8443" {
		t.Errorf("Gateway() = %q", cfg.Gateway())
	}
}

func TestGatewayPortDefaultsTo443(t *testing.T) {
	isolateEnv(t)

	// A bare hostname is the common case in the install prompt: 443 is the port
	// that survives restrictive outbound firewalls, which is why it is the
	// default for a NAT-bound contributor box.
	cfg, err := Load(writeConfig(t, `
token = "`+sampleToken+`"
gateway = "ingest.example.org"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GatewayPort != 443 {
		t.Errorf("gateway port = %d for a bare hostname, want 443", cfg.GatewayPort)
	}
}

// ---------------------------------------------------------------------------
// the token must not leak
// ---------------------------------------------------------------------------

// TestMaskTokenNeverRevealsTheToken covers the function every log line and
// diagnostic in the feeder is required to go through.
func TestMaskTokenNeverRevealsTheToken(t *testing.T) {
	masked := MaskToken(sampleToken)

	if strings.Contains(masked, sampleToken) {
		t.Fatalf("MaskToken returned the token verbatim: %q", masked)
	}
	// The body after the prefix must not appear at all, in whole or in any
	// substantial part. 12 characters is far more than a support conversation
	// needs and far less than is safe to print.
	body := strings.TrimPrefix(sampleToken, "adsbng_")
	if len(body) < 12 {
		t.Fatal("sampleToken is too short for this test to mean anything")
	}
	if strings.Contains(masked, body[:12]) {
		t.Errorf("MaskToken leaked the start of the token body: %q", masked)
	}
	if strings.Contains(masked, body[len(body)-12:]) {
		t.Errorf("MaskToken leaked the end of the token body: %q", masked)
	}
	// It should still be useful: enough to distinguish two tokens.
	if !strings.Contains(masked, "50 chars") {
		t.Errorf("MaskToken = %q, expected it to report the length", masked)
	}

	// Degenerate inputs must not panic, and must fall back to reporting only a
	// length. (A substring check is useless here: "<1 chars>" trivially contains
	// a one-character input, so assert the exact documented shape instead.)
	if got := MaskToken(""); got != "<unset>" {
		t.Errorf("MaskToken(%q) = %q, want <unset>", "", got)
	}
	for _, tok := range []string{"a", "ab", "abc", "abcd"} {
		want := fmt.Sprintf("<%d chars>", len(tok))
		if got := MaskToken(tok); got != want {
			t.Errorf("MaskToken(%q) = %q, want %q — a token too short to mask "+
				"must be summarised, never echoed", tok, got, want)
		}
	}
}

// TestRedactedNeverContainsTheToken guards the whole-config summary the feeder
// prints at startup and on --check-config.
func TestRedactedNeverContainsTheToken(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	line := cfg.Redacted()

	if strings.Contains(line, sampleToken) {
		t.Fatalf("Redacted() printed the token: %q", line)
	}
	body := strings.TrimPrefix(sampleToken, "adsbng_")
	if strings.Contains(line, body[:12]) {
		t.Errorf("Redacted() leaked part of the token: %q", line)
	}
	// It must still be a useful diagnostic, or people will print the config
	// themselves instead.
	for _, want := range []string{"TEST_ABUJA", "ingest.example.org:8443", "127.0.0.1:30005"} {
		if !strings.Contains(line, want) {
			t.Errorf("Redacted() = %q, missing %q", line, want)
		}
	}
}

// ---------------------------------------------------------------------------
// parsing: a typo must be an error, not a no-op
// ---------------------------------------------------------------------------

func TestUnknownKeyIsRejected(t *testing.T) {
	isolateEnv(t)

	// The motivating case: a misspelled token key would otherwise leave the
	// feeder running with no credential and no explanation.
	_, err := Load(writeConfig(t, `
receiver_id = "TEST_ABUJA"
toekn = "`+sampleToken+`"
gateway = "ingest.example.org:8443"
`))
	if err == nil {
		t.Fatal("a misspelled key was silently ignored")
	}
	if !strings.Contains(err.Error(), "toekn") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestMalformedFileIsRejected(t *testing.T) {
	isolateEnv(t)

	cases := []struct {
		name, body, wantSubstr string
	}{
		{"toml table", "[station]\ntoken = \"x\"\n", "tables are not supported"},
		{"no equals", "token\n", "expected key = value"},
		{"empty key", "= value\n", "empty key"},
		{"non-numeric port", minimalConfig + "beast_port = thirty\n", "not a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not explain the problem (want %q)", err, tc.wantSubstr)
			}
		})
	}
}

func TestParsingHandlesRealWorldFileShapes(t *testing.T) {
	isolateEnv(t)

	// Comments, blank lines, single quotes, inconsistent spacing, a trailing
	// comment after a value — all of which appear in files people hand-edit.
	cfg, err := Load(writeConfig(t, `
# ADSBNG feeder configuration

receiver_id='TEST_LAGOS'
token   =    "`+sampleToken+`"    # issued 2026-08-18, keep secret
gateway_host = ingest.example.org
gateway_port = 8443

beast_source = "127.0.0.1:30105"   # readsb
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StationID != "TEST_LAGOS" {
		t.Errorf("StationID = %q, want TEST_LAGOS (single quotes not stripped?)", cfg.StationID)
	}
	if cfg.Token != sampleToken {
		t.Errorf("token was mangled: got %d chars, want %d", len(cfg.Token), len(sampleToken))
	}
	if cfg.Gateway() != "ingest.example.org:8443" {
		t.Errorf("Gateway() = %q", cfg.Gateway())
	}
	if cfg.BeastAddr() != "127.0.0.1:30105" {
		t.Errorf("BeastAddr() = %q", cfg.BeastAddr())
	}
}

// TestCommentStrippingDoesNotCorruptAQuotedValue matters because a token is
// opaque base64url today, but the comment-stripping rule has to be safe for
// whatever a value contains tomorrow.
func TestCommentStrippingDoesNotCorruptAQuotedValue(t *testing.T) {
	isolateEnv(t)

	weird := "abc#def#ghi"
	cfg, err := Load(writeConfig(t, `
token = "`+weird+`"
gateway = "ingest.example.org"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Token != weird {
		t.Errorf("quoted value with '#' became %q, want %q", cfg.Token, weird)
	}
}

func TestKeyAliasesAreAccepted(t *testing.T) {
	isolateEnv(t)

	// The provisioning output and earlier drafts used different names for the
	// same fields. Accepting both costs nothing and avoids a support thread.
	cfg, err := Load(writeConfig(t, `
station_id = "TEST_ABUJA"
station_token = "`+sampleToken+`"
ingest_host = "ingest.example.org"
ingest_port = 8443
beast_host = "192.168.1.50"
beast_port = 30005
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StationID != "TEST_ABUJA" || cfg.Token != sampleToken {
		t.Errorf("aliases not applied: %s", cfg.Redacted())
	}
	if cfg.Gateway() != "ingest.example.org:8443" || cfg.BeastAddr() != "192.168.1.50:30005" {
		t.Errorf("aliases not applied: %s", cfg.Redacted())
	}
}

// TestOverlappingFileKeysResolveDeterministically is a regression test for a bug
// that was only visible about half the time it ran.
//
// applyFile used to range over the parsed key/value map, and Go randomizes map
// iteration order. A config setting both `gateway = "host:8443"` and
// `gateway_port = 0` therefore came out as port 8443 or port 0 depending on
// nothing at all: sometimes the invalid port was validated and rejected,
// sometimes the composite key overwrote it and the feeder started happily. On a
// contributor's machine that is a feeder that works until it is restarted.
//
// The rule now is that the narrower key wins, whichever order the lines appear
// in, so the pairs below all have exactly one right answer.
func TestOverlappingFileKeysResolveDeterministically(t *testing.T) {
	isolateEnv(t)

	head := "token = \"" + sampleToken + "\"\n"
	for _, tc := range []struct {
		name      string
		body      string
		wantGate  string
		wantBeast string
	}{
		{
			name:      "gateway_port overrides the port inside gateway",
			body:      head + "gateway = \"ingest.example.org:8443\"\ngateway_port = 9443\n",
			wantGate:  "ingest.example.org:9443",
			wantBeast: "127.0.0.1:30005",
		},
		{
			name:      "order in the file does not matter",
			body:      head + "gateway_port = 9443\ngateway = \"ingest.example.org:8443\"\n",
			wantGate:  "ingest.example.org:9443",
			wantBeast: "127.0.0.1:30005",
		},
		{
			name:      "gateway_host overrides the host inside gateway",
			body:      head + "gateway = \"wrong.example.org:8443\"\ngateway_host = \"ingest.example.org\"\n",
			wantGate:  "ingest.example.org:8443",
			wantBeast: "127.0.0.1:30005",
		},
		{
			name:      "beast_port overrides the port inside beast_source",
			body:      head + "gateway = \"ingest.example.org\"\nbeast_source = \"127.0.0.1:30005\"\nbeast_port = 30105\n",
			wantGate:  "ingest.example.org:443",
			wantBeast: "127.0.0.1:30105",
		},
		{
			name:      "beast_host overrides the host inside beast_source",
			body:      head + "gateway = \"ingest.example.org\"\nbeast_source = \"127.0.0.1:30005\"\nbeast_host = \"192.168.1.50\"\n",
			wantGate:  "ingest.example.org:443",
			wantBeast: "192.168.1.50:30005",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Gateway() != tc.wantGate {
				t.Errorf("Gateway() = %q, want %q", cfg.Gateway(), tc.wantGate)
			}
			if cfg.BeastAddr() != tc.wantBeast {
				t.Errorf("BeastAddr() = %q, want %q", cfg.BeastAddr(), tc.wantBeast)
			}
		})
	}
}

// TestDuplicateAliasesAreRejected covers the other half of the same problem.
// Broad and narrow keys have a defensible precedence; two spellings of one
// setting do not, so rather than pick a winner at random the loader says so.
func TestDuplicateAliasesAreRejected(t *testing.T) {
	isolateEnv(t)

	for _, body := range []string{
		"token = \"" + sampleToken + "\"\nstation_token = \"" + sampleToken + "\"\ngateway = \"ingest.example.org\"\n",
		minimalConfig + "station_id = \"TEST_LAGOS\"\n",
		minimalConfig + "gateway_port = 9443\ningest_port = 9444\n",
		minimalConfig + "gateway_host = \"a.example.org\"\ningest_host = \"b.example.org\"\n",
	} {
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("two names for one setting were accepted:\n%s", body)
			continue
		}
		if !strings.Contains(err.Error(), "same setting") {
			t.Errorf("unexpected error for a duplicated setting: %v", err)
		}
		if strings.Contains(err.Error(), sampleToken) {
			t.Errorf("error message contains the token: %v", err)
		}
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	isolateEnv(t)
	t.Setenv("ADSBNG_STATION_TOKEN", sampleToken)
	t.Setenv("ADSBNG_INGEST_HOST", "ingest.example.org")

	// systemd installs may configure the feeder entirely through environment
	// variables in a drop-in, with no config file present at all.
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("a missing config file was treated as fatal: %v", err)
	}
	if cfg.Token != sampleToken {
		t.Error("environment token was not applied")
	}
}

// ---------------------------------------------------------------------------
// environment precedence
// ---------------------------------------------------------------------------

func TestEnvironmentOverridesTheFile(t *testing.T) {
	isolateEnv(t)

	path := writeConfig(t, minimalConfig+`
beast_source = "127.0.0.1:30005"
`)
	envToken := "adsbng_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

	t.Setenv("ADSBNG_STATION_ID", "TEST_LAGOS")
	t.Setenv("ADSBNG_STATION_TOKEN", envToken)
	t.Setenv("ADSBNG_INGEST_HOST", "other.example.org")
	t.Setenv("ADSBNG_INGEST_PORT", "9443")
	t.Setenv("BEAST_SOURCE", "10.0.0.9:30006")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StationID != "TEST_LAGOS" {
		t.Errorf("StationID = %q, want the env value", cfg.StationID)
	}
	if cfg.Token != envToken {
		t.Error("token was not overridden by the environment")
	}
	if cfg.Gateway() != "other.example.org:9443" {
		t.Errorf("Gateway() = %q, want the env values", cfg.Gateway())
	}
	if cfg.BeastAddr() != "10.0.0.9:30006" {
		t.Errorf("BeastAddr() = %q, want the env value", cfg.BeastAddr())
	}
}

func TestBadEnvironmentValuesAreRejected(t *testing.T) {
	isolateEnv(t)
	path := writeConfig(t, minimalConfig)

	t.Run("port", func(t *testing.T) {
		t.Setenv("ADSBNG_INGEST_PORT", "https")
		if _, err := Load(path); err == nil {
			t.Error("a non-numeric ADSBNG_INGEST_PORT was accepted")
		}
	})
	t.Run("insecure flag", func(t *testing.T) {
		t.Setenv("ADSBNG_INSECURE_SKIP_VERIFY", "sure")
		if _, err := Load(path); err == nil {
			t.Error("a non-boolean ADSBNG_INSECURE_SKIP_VERIFY was accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestValidateRequiresATokenAndGateway(t *testing.T) {
	isolateEnv(t)

	// No token: the feeder cannot authenticate, so starting up and retrying
	// forever would just be an unexplained outage.
	_, err := Load(writeConfig(t, "gateway = \"ingest.example.org\"\n"))
	if err == nil {
		t.Fatal("a config with no token was accepted")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should name the missing token: %v", err)
	}

	// No gateway: nowhere to send data.
	_, err = Load(writeConfig(t, "token = \""+sampleToken+"\"\n"))
	if err == nil {
		t.Fatal("a config with no gateway host was accepted")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error should name the missing gateway: %v", err)
	}
}

// TestValidateErrorNeverContainsTheToken closes the last easy leak: an error
// message that helpfully quotes the config it rejected.
func TestValidateErrorNeverContainsTheToken(t *testing.T) {
	isolateEnv(t)

	// Every rejection path that has a token present in the config.
	bodies := []string{
		minimalConfig + "gateway_port = 70000\n",
		minimalConfig + "beast_port = 0\n",
		minimalConfig + "ca_file = \"/nonexistent/ca.pem\"\n",
		minimalConfig + "insecure_skip_verify = true\nca_file = \"/nonexistent/ca.pem\"\n",
		minimalConfig + "toekn = \"x\"\n",
	}
	for _, body := range bodies {
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("config was accepted but should not have been:\n%s", body)
			continue
		}
		if strings.Contains(err.Error(), sampleToken) {
			t.Errorf("error message contains the token: %v", err)
		}
	}
}

func TestValidateRejectsOutOfRangePorts(t *testing.T) {
	isolateEnv(t)

	for _, body := range []string{
		minimalConfig + "gateway_port = 0\n",
		minimalConfig + "gateway_port = 65536\n",
		minimalConfig + "beast_port = -1\n",
		minimalConfig + "beast_port = 99999\n",
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("out-of-range port accepted:\n%s", body)
		}
	}
}

// TestValidateRejectsCAFileWithInsecureSkipVerify catches a configuration that
// looks careful and is not: pinning a CA while also disabling verification means
// verification is off, and the operator would believe otherwise.
func TestValidateRejectsCAFileWithInsecureSkipVerify(t *testing.T) {
	isolateEnv(t)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("-not-a-real-certificate-\n"), 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	// Each alone is fine.
	if _, err := Load(writeConfig(t, minimalConfig+"ca_file = \""+ca+"\"\n")); err != nil {
		t.Errorf("ca_file alone was rejected: %v", err)
	}
	if _, err := Load(writeConfig(t, minimalConfig+"insecure_skip_verify = true\n")); err != nil {
		t.Errorf("insecure_skip_verify alone was rejected: %v", err)
	}

	// Together they are contradictory.
	_, err := Load(writeConfig(t, minimalConfig+
		"ca_file = \""+ca+"\"\ninsecure_skip_verify = true\n"))
	if err == nil {
		t.Fatal("ca_file combined with insecure_skip_verify was accepted")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error does not explain the contradiction: %v", err)
	}
}

func TestValidateRejectsAMissingCAFile(t *testing.T) {
	isolateEnv(t)

	// Failing at startup beats failing every reconnect attempt with a TLS error
	// the contributor cannot interpret.
	_, err := Load(writeConfig(t, minimalConfig+"ca_file = \"/no/such/ca.pem\"\n"))
	if err == nil {
		t.Fatal("a ca_file that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "ca.pem") {
		t.Errorf("error should name the missing file: %v", err)
	}
}

// TestStationIDIsOptional documents that the local label is not load-bearing.
// Identity comes from the token; a station with no station_id at all still
// works, and is attributed correctly by the gateway.
func TestStationIDIsOptional(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(writeConfig(t, `
token = "`+sampleToken+`"
gateway = "ingest.example.org:8443"
`))
	if err != nil {
		t.Fatalf("a config with no station_id was rejected: %v", err)
	}
	if cfg.StationID != "" {
		t.Errorf("StationID = %q, want empty", cfg.StationID)
	}
}
