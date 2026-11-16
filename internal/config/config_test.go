package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const credentials = `[imap]
host = "imap.example.com"
username = "private-user"
password = "private-password"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, credentials))
	if err != nil {
		t.Fatal(err)
	}
	imap := cfg.IMAP()
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CachePath() != filepath.Join(cacheDir, "imap-mcp", "headers.sqlite3") {
		t.Fatalf("unexpected cache path default: %q", cfg.CachePath())
	}
	if !cfg.Readonly() || cfg.TrashMailbox() != "Trash" {
		t.Fatalf("unexpected policy defaults: readonly=%v trash=%q", cfg.Readonly(), cfg.TrashMailbox())
	}
	if imap.Host() != "imap.example.com" || imap.Username() != "private-user" || imap.Password() != "private-password" {
		t.Fatal("connection credentials were not preserved")
	}
	if imap.TLS() != "tls" || imap.Port() != 993 || imap.CAFile() != "" {
		t.Fatalf("unexpected connection defaults: tls=%q port=%d ca_file=%q", imap.TLS(), imap.Port(), imap.CAFile())
	}
	if imap.ConnectTimeout() != 10*time.Second || imap.OperationTimeout() != 60*time.Second {
		t.Fatalf("unexpected timeout defaults: %v %v", imap.ConnectTimeout(), imap.OperationTimeout())
	}
	tlsConfig := imap.TLSConfig()
	if tlsConfig.MinVersion != tls.VersionTLS12 || tlsConfig.ServerName != imap.Host() || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs != nil {
		t.Fatal("TLS defaults must verify certificates and hostnames using system roots")
	}
	tlsConfig.MinVersion = tls.VersionTLS10
	tlsConfig.ServerName = "other.example.com"
	tlsConfig.InsecureSkipVerify = true
	next := cfg.IMAP().TLSConfig()
	if next.MinVersion != tls.VersionTLS12 || next.ServerName != imap.Host() || next.InsecureSkipVerify {
		t.Fatal("mutating TLS settings changed the startup snapshot")
	}
}

func TestLoadCachePaths(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "headers.sqlite3")
	for _, tt := range []struct {
		name string
		path string
	}{
		{"relative", "cache/headers.sqlite3"},
		{"absolute", absolute},
		{"empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configPath := writeConfig(t, credentials+"\n[cache]\npath = "+strconv.Quote(tt.path)+"\n")
			cfg, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			want := tt.path
			if want == "" {
				cacheDir, err := os.UserCacheDir()
				if err != nil {
					t.Fatal(err)
				}
				want = filepath.Join(cacheDir, "imap-mcp", "headers.sqlite3")
			} else if !filepath.IsAbs(want) {
				want = filepath.Join(filepath.Dir(configPath), want)
			}
			if cfg.CachePath() != want {
				t.Fatalf("cache path = %q, want %q", cfg.CachePath(), want)
			}
			if err := os.WriteFile(configPath, []byte(credentials+"\n[cache]\npath = \"replacement.sqlite3\"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if cfg.CachePath() != want {
				t.Fatal("config file changes affected the loaded cache path")
			}
		})
	}
}

func TestLoadEmptyCacheTable(t *testing.T) {
	if _, err := Load(writeConfig(t, credentials+"\n[cache]\n")); err != nil {
		t.Fatalf("empty cache table must use the default path: %v", err)
	}
}

func TestLoadExample(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatalf("example configuration is invalid: %v", err)
	}
	if !cfg.Readonly() || cfg.TrashMailbox() != "Trash" || cfg.CachePath() == "" {
		t.Fatal("example must retain readonly and persistent-cache defaults")
	}
}

func TestLoadExplicitSettings(t *testing.T) {
	cfg, err := Load(writeConfig(t, `readonly = false
trash_mailbox = "Deleted Items"
`+credentials+`tls = "starttls"
port = 1143
connect_timeout_seconds = 20
operation_timeout_seconds = 120
`))
	if err != nil {
		t.Fatal(err)
	}
	imap := cfg.IMAP()
	if cfg.Readonly() || cfg.TrashMailbox() != "Deleted Items" {
		t.Fatal("explicit policy settings were not respected")
	}
	if imap.TLS() != "starttls" || imap.Port() != 1143 {
		t.Fatal("explicit transport settings were not respected")
	}
	if imap.ConnectTimeout() != 20*time.Second || imap.OperationTimeout() != 120*time.Second {
		t.Fatal("explicit timeouts were not respected")
	}
}

func TestLoadSTARTTLSDefaultPort(t *testing.T) {
	cfg, err := Load(writeConfig(t, credentials+`tls = "starttls"`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP().Port() != 143 || cfg.IMAP().TLSConfig().InsecureSkipVerify {
		t.Fatal("STARTTLS must default to port 143 with certificate verification")
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	overflow := strconv.FormatInt(math.MaxInt64/int64(time.Second)+1, 10)
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown root", "cache_path = \"private-password\"\n" + credentials, "unknown keys"},
		{"unknown table", credentials + "\n[storage]\npath = \"headers.db\"\n", "unknown keys"},
		{"unknown empty table", credentials + "\n[storage]\n", "unknown keys"},
		{"unknown cache", credentials + "\n[cache]\nkey = \"private-password\"\n", "unknown keys"},
		{"unknown cache nested table", credentials + "\n[cache.extra]\n", "unknown keys"},
		{"invalid cache path type", credentials + "\n[cache]\npath = false\n", "field type"},
		{"unknown nested table", credentials + "\n[imap.extra]\n", "unknown keys"},
		{"unknown imap", credentials + "insecure_skip_verify = true", "unknown keys"},
		{"invalid readonly type", "readonly = \"false\"\n" + credentials, "field type"},
		{"invalid trash type", "trash_mailbox = false\n" + credentials, "field type"},
		{"empty trash", "trash_mailbox = \"\"\n" + credentials, "trash_mailbox"},
		{"empty file", "", "imap.host"},
		{"missing host", strings.ReplaceAll(credentials, "host = \"imap.example.com\"\n", ""), "imap.host"},
		{"blank host", strings.ReplaceAll(credentials, "imap.example.com", " "), "imap.host"},
		{"missing username", strings.ReplaceAll(credentials, "username = \"private-user\"\n", ""), "imap.username"},
		{"blank username", strings.ReplaceAll(credentials, "private-user", " "), "imap.username"},
		{"missing password", strings.ReplaceAll(credentials, "password = \"private-password\"\n", ""), "imap.password"},
		{"empty password", strings.ReplaceAll(credentials, "private-password", ""), "imap.password"},
		{"invalid username type", strings.ReplaceAll(credentials, "\"private-user\"", "123"), "field type"},
		{"invalid password type", strings.ReplaceAll(credentials, "\"private-password\"", "false"), "field type"},
		{"invalid host type", strings.ReplaceAll(credentials, "\"imap.example.com\"", "true"), "field type"},
		{"zero port", credentials + "port = 0", "imap.port"},
		{"negative port", credentials + "port = -1", "imap.port"},
		{"high port", credentials + "port = 65536", "imap.port"},
		{"invalid port type", credentials + "port = \"private-password\"", "field type"},
		{"fractional port", credentials + "port = 993.5", "field type"},
		{"plaintext", credentials + "tls = \"plain\"", "imap.tls"},
		{"empty TLS", credentials + "tls = \"\"", "imap.tls"},
		{"invalid TLS type", credentials + "tls = false", "field type"},
		{"zero connect timeout", credentials + "connect_timeout_seconds = 0", "imap.connect_timeout_seconds"},
		{"negative connect timeout", credentials + "connect_timeout_seconds = -1", "imap.connect_timeout_seconds"},
		{"connect timeout overflow", credentials + "connect_timeout_seconds = " + overflow, "imap.connect_timeout_seconds"},
		{"invalid connect timeout type", credentials + "connect_timeout_seconds = \"private-password\"", "field type"},
		{"zero operation timeout", credentials + "operation_timeout_seconds = 0", "imap.operation_timeout_seconds"},
		{"negative operation timeout", credentials + "operation_timeout_seconds = -1", "imap.operation_timeout_seconds"},
		{"operation timeout overflow", credentials + "operation_timeout_seconds = " + overflow, "imap.operation_timeout_seconds"},
		{"fractional operation timeout", credentials + "operation_timeout_seconds = 1.5", "field type"},
		{"malformed TOML", credentials + "\"private-password private-user\" = [", "invalid TOML"},
		{"secret unknown key", credentials + "\"private-password private-user\" = true", "unknown keys"},
		{"missing CA file", credentials + "ca_file = \"private-password-private-user.pem\"", "imap.ca_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.content))
			if err == nil || cfg != nil {
				t.Fatal("invalid configuration was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "private-user") || strings.Contains(err.Error(), "private-password") {
				t.Fatal("configuration error revealed credentials")
			}
		})
	}
}

func TestLoadPreservesPasswordWhitespace(t *testing.T) {
	cfg, err := Load(writeConfig(t, strings.ReplaceAll(credentials, "private-password", " ")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP().Password() != " " {
		t.Fatal("password whitespace must be preserved")
	}
}

func TestLoadTimeoutBoundary(t *testing.T) {
	seconds := math.MaxInt64 / int64(time.Second)
	cfg, err := Load(writeConfig(t, credentials+"connect_timeout_seconds = "+strconv.FormatInt(seconds, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP().ConnectTimeout() != time.Duration(seconds)*time.Second {
		t.Fatal("valid timeout boundary was not preserved")
	}
}

func testCertificate(t *testing.T, serial int64) (*x509.Certificate, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "test CA"},
		DNSNames:              []string{"imap.example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestLoadRelativeCAAndSnapshot(t *testing.T) {
	path := writeConfig(t, credentials+"ca_file = \"root.pem\"")
	caPath := filepath.Join(filepath.Dir(path), "root.pem")
	cert, certificatePEM := testCertificate(t, 1)
	if err := os.WriteFile(caPath, certificatePEM, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP().CAFile() != caPath {
		t.Fatalf("relative CA path resolved to %q, want %q", cfg.IMAP().CAFile(), caPath)
	}
	tlsConfig := cfg.IMAP().TLSConfig()
	if _, err := cert.Verify(x509.VerifyOptions{Roots: tlsConfig.RootCAs, DNSName: tlsConfig.ServerName}); err != nil {
		t.Fatalf("configured CA cannot verify the server certificate: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: tlsConfig.RootCAs, DNSName: "other.example.com"}); err == nil {
		t.Fatal("certificate verification must reject an incorrect hostname")
	}

	originalRoots := tlsConfig.RootCAs.Clone()
	otherCertificate, _ := testCertificate(t, 2)
	tlsConfig.RootCAs.AddCert(otherCertificate)
	if !cfg.IMAP().TLSConfig().RootCAs.Equal(originalRoots) {
		t.Fatal("mutating returned certificate roots changed the startup snapshot")
	}
	if err := os.WriteFile(caPath, []byte("invalid replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if !cfg.IMAP().TLSConfig().RootCAs.Equal(originalRoots) {
		t.Fatal("CA file changes affected the loaded startup snapshot")
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "no valid certificates") {
		t.Fatal("invalid CA contents must be rejected on the next load")
	}
}

func TestLoadMissingFileIsSanitized(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "private-user-private-password.toml"))
	if err == nil {
		t.Fatal("missing configuration file was accepted")
	}
	if strings.Contains(err.Error(), "private-user") || strings.Contains(err.Error(), "private-password") {
		t.Fatal("file error revealed sensitive path information")
	}
}
