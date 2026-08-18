package feeder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests assert properties of the repository rather than of a running
// feeder, because the feeder's security guarantee is largely a claim about what
// is NOT in it.
//
// The claim, from the spec: a contributor may inspect, modify, or
// reverse-engineer this software, and must gain nothing from doing so. Every
// test here fails the moment that stops being true — which is the point, since
// this repository is the one artefact ADSBNG hands to untrusted third parties.

// forbiddenIdentifiers are names that must never appear in the contributor-facing
// repository. Some are credentials, some are the services those credentials
// unlock; either way, a hit means the feeder has been given knowledge of ADSBNG
// internals it has no business holding.
//
// Matching is case-insensitive, so one entry covers every spelling. That matters
// more than it sounds: this list previously held paired upper/lower entries
// compared case-sensitively, and documentation prose naming our stack in title
// case — "Paystack", "Supabase", "Telegram" — walked straight past a test whose
// own comment claimed to forbid it. Only the all-caps VAPID was caught, by
// accident of how that name is conventionally written. Entries are therefore the
// shortest distinctive stem rather than an exact environment-variable name.
var forbiddenIdentifiers = []string{
	"database_url",
	"supabase",
	"postgres", // also covers postgres://, postgresql:// and PostgreSQL in prose
	"redis",
	"service_role",
	"paystack",
	"telegram",
	"smtp",
	"nodemailer",
	"vapid",
	"anthropic",
	"report_link_secret",
	"admin_api_token",
}

// TestNoADSBNGSecretsOrServicesReferenced walks the whole module.
//
// A contributor holds this entire repository. If any of these strings appeared
// here it would tell them what to attack even when the value itself was absent —
// and if a value ever were pasted in, this is the test that catches it before the
// commit ships to other people's machines.
func TestNoADSBNGSecretsOrServicesReferenced(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !scannableFile(path) || isSelf(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		lower := strings.ToLower(body)
		rel, _ := filepath.Rel(root, path)

		for _, bad := range forbiddenIdentifiers {
			idx := strings.Index(lower, bad)
			if idx < 0 {
				continue
			}
			// Report the spelling actually in the file, not the lowercase stem,
			// so the failure points at the text to delete.
			t.Errorf("%s references %q (at byte %d).\n"+
				"This repository is distributed to untrusted contributors and must "+
				"contain no ADSBNG credential, service name, or internal endpoint. "+
				"Nothing here should know these exist.",
				rel, body[idx:idx+len(bad)], idx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// TestFeederHasNoExternalDependencies enforces the policy written at the top of
// go.mod. Every dependency is code a contributor would have to trust on our word,
// and a supply-chain compromise in one would land on machines we do not own. The
// feeder is small enough that the stdlib is genuinely sufficient.
func TestFeederHasNoExternalDependencies(t *testing.T) {
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	for i, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "require (" || strings.HasPrefix(s, "require ") {
			t.Errorf("go.mod:%d introduces a dependency (%q).\n"+
				"The feeder must build from the standard library alone. If you need "+
				"~40 lines of a library, write the 40 lines.", i+1, s)
		}
	}

	// A go.sum with content means dependencies were resolved at some point.
	if info, err := os.Stat(filepath.Join(root, "go.sum")); err == nil && info.Size() > 0 {
		t.Errorf("go.sum exists and is %d bytes — the feeder should have no modules "+
			"to verify", info.Size())
	}
}

// TestNoIngestOrCoreSourceCopiedIn guards the trust boundary in the direction
// that matters commercially: the gateway's authentication logic and the decoder
// are private, and this repository is not.
func TestNoIngestOrCoreSourceCopiedIn(t *testing.T) {
	root := moduleRoot(t)

	// Filenames that exist only in the private repositories.
	privateFiles := []string{
		// adsbng-ingest
		"store.go", "coresink.go", "gateway.go", "stations.db",
		// adsbng-decoder / adsb-server (ADSBNG Core)
		"aircraftEnricher.js", "alertSystem.js", "telegramBot.js",
		"emailTemplates.js", "companyRoutes.js", "positionStream.js",
		"aiAnalyst.js", "leaderLock.js", "workers.js",
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		for _, name := range privateFiles {
			if strings.EqualFold(d.Name(), name) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s looks like private ADSBNG source copied into the "+
					"contributor-facing repository", rel)
			}
		}
		if d.Name() == ".env" {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s exists: an environment file must never ship to contributors", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}

	// The feeder must not import the gateway module either — that would make the
	// private module a build dependency of the public one.
	if strings.Contains(readAll(t, filepath.Join(root, "go.mod")), "adsbng-ingest") {
		t.Error("go.mod references the private adsbng-ingest module")
	}
}

// TestNoDockerArtifacts holds the spec's hard requirement: this installs directly
// onto Debian, Ubuntu, and Raspberry Pi OS with systemd, and nothing may
// reintroduce containers — least of all on a contributor's Pi.
func TestNoDockerArtifacts(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{
		"Dockerfile", "docker-compose.yml", "docker-compose.yaml",
		"compose.yml", "compose.yaml", ".dockerignore",
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if strings.EqualFold(d.Name(), "k8s") || strings.EqualFold(d.Name(), "kubernetes") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s exists: the feeder is Linux-native, not containerised", rel)
				return filepath.SkipDir
			}
			return nil
		}
		for _, name := range forbidden {
			if strings.EqualFold(d.Name(), name) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s exists: the feeder is Linux-native, not containerised", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// TestNoCommittedTokensOrKeys catches the likeliest real accident: pasting a live
// station token into the example config while testing, then committing it.
func TestNoCommittedTokensOrKeys(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !scannableFile(path) || isSelf(path) {
			return nil
		}
		body := readAll(t, path)
		rel, _ := filepath.Rel(root, path)

		for _, marker := range []string{
			"-----BEGIN RSA PRIVATE KEY-----",
			"-----BEGIN EC PRIVATE KEY-----",
			"-----BEGIN PRIVATE KEY-----",
			"-----BEGIN OPENSSH PRIVATE KEY-----",
		} {
			if strings.Contains(body, marker) {
				t.Errorf("%s contains PEM private-key material", rel)
			}
		}

		// A token literal, as opposed to a prose mention of the prefix. The
		// config tests use an obviously-fake constant, so they are exempt.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, line := range strings.Split(body, "\n") {
			idx := strings.Index(line, "adsbng_")
			if idx < 0 {
				continue
			}
			if countTokenChars(line[idx+len("adsbng_"):]) >= 40 {
				t.Errorf("%s contains what looks like a real station token — if it is "+
					"real, revoke it with `adsbng-stations revoke` before anything else", rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// TestInstalledFilesNeverEnableInsecureTLS is the counterpart to the config
// package's default test. The escape hatch is allowed to exist and to be
// documented; it must not be switched on by anything we install.
//
// Docs may say the words — a warning has to name what it warns about — so only
// the artefacts that end up on a contributor's disk are scanned.
func TestInstalledFilesNeverEnableInsecureTLS(t *testing.T) {
	root := moduleRoot(t)

	installed := []string{
		"config.toml.example",
		"install.sh",
		filepath.Join("systemd", "adsbng-feeder.service"),
	}
	for _, rel := range installed {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				t.Errorf("%s is missing — a contributor install needs it", rel)
				continue
			}
			t.Fatalf("read %s: %v", rel, err)
		}

		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// A commented-out mention is documentation, which is fine.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			flat := strings.ReplaceAll(strings.ToLower(trimmed), " ", "")
			if strings.Contains(flat, "insecure_skip_verify=true") ||
				strings.Contains(flat, "adsbng_insecure_skip_verify=true") {
				t.Errorf("%s enables insecure TLS on an installed station:\n  %s", rel, trimmed)
			}
		}
	}
}

// TestTokenIsNeverPassedToAFormattingCall is the structural version of "the token
// is never logged".
//
// The runtime test on the gateway side proves tokens stay out of the journal
// there. Here the risk is different and worse: the feeder runs on a machine whose
// logs may be world-readable, shared in a support thread, or shipped to a
// third-party log service. So no token may reach a Printf-family call at all,
// except through MaskToken.
func TestTokenIsNeverPassedToAFormattingCall(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// Names whose arguments end up as human-readable text somewhere.
	formatters := map[string]bool{
		"Print": true, "Printf": true, "Println": true,
		"Fatal": true, "Fatalf": true, "Fatalln": true,
		"Panic": true, "Panicf": true, "Panicln": true,
		"Sprint": true, "Sprintf": true, "Sprintln": true,
		"Errorf": true, "Error": true, "New": true,
		"Fprint": true, "Fprintf": true, "Fprintln": true,
		"Write": true, "WriteString": true,
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Only shipped source: tests are not distributed and legitimately
		// inspect the field.
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !formatters[calleeName(call.Fun)] {
				return true
			}
			for _, arg := range call.Args {
				if leaksToken(arg) {
					t.Errorf("%s:%d passes a token to %s(...).\n"+
						"Wrap it in config.MaskToken, or do not print it. A token in a "+
						"contributor's log is a token in whatever they paste into a "+
						"support ticket.", rel, fset.Position(call.Pos()).Line,
						calleeName(call.Fun))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
}

// leaksToken reports whether expr reads a Token field outside of a masking call.
func leaksToken(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		// Do not descend into calls that consume the token safely, or into len(),
		// which reveals only a length.
		if call, ok := n.(*ast.CallExpr); ok {
			switch calleeName(call.Fun) {
			case "MaskToken", "len":
				return false
			}
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "Token", "StationToken":
				found = true
				return false
			}
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "token" {
			found = true
			return false
		}
		return true
	})
	return found
}

// calleeName reduces a call target to its final identifier: log.Printf -> Printf,
// f.lg.Fatalf -> Fatalf, Errorf -> Errorf.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return calleeName(f.X)
	}
	return ""
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// moduleRoot resolves the repository root from this package's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..")) // internal/feeder -> repo root
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	return root
}

// isSelf skips this file, which necessarily contains every string it forbids.
func isSelf(path string) bool {
	return filepath.Base(path) == "security_test.go"
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// scannableFile reports whether a path is source or configuration worth reading.
func scannableFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".md", ".sh", ".toml", ".yaml", ".yml", ".json", ".service",
		".sql", ".txt", ".conf", ".mod", ".sum", ".example":
		return true
	}
	if filepath.Ext(path) == "" {
		info, err := os.Stat(path)
		return err == nil && info.Size() < 1<<20
	}
	return false
}

// countTokenChars counts leading base64url characters, which is how a token
// literal is told apart from prose that merely mentions the prefix.
func countTokenChars(s string) int {
	n := 0
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			n++
			continue
		}
		break
	}
	return n
}
