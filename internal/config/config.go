package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is an immutable snapshot of the configuration loaded at startup.
type Config struct {
	readonly     bool
	trashMailbox string
	cachePath    string
	imap         IMAPConfig
}

func (c *Config) Readonly() bool       { return c.readonly }
func (c *Config) TrashMailbox() string { return c.trashMailbox }
func (c *Config) CachePath() string    { return c.cachePath }
func (c *Config) IMAP() IMAPConfig     { return c.imap }

// IMAPConfig holds connection settings. TLSConfig returns a separate mutable copy.
type IMAPConfig struct {
	host             string
	port             int
	username         string
	password         string
	tlsMode          string
	caFile           string
	connectTimeout   time.Duration
	operationTimeout time.Duration
	rootCAs          *x509.CertPool
}

func (c IMAPConfig) Host() string                    { return c.host }
func (c IMAPConfig) Port() int                       { return c.port }
func (c IMAPConfig) Username() string                { return c.username }
func (c IMAPConfig) Password() string                { return c.password }
func (c IMAPConfig) TLS() string                     { return c.tlsMode }
func (c IMAPConfig) CAFile() string                  { return c.caFile }
func (c IMAPConfig) ConnectTimeout() time.Duration   { return c.connectTimeout }
func (c IMAPConfig) OperationTimeout() time.Duration { return c.operationTimeout }

// TLSConfig enables certificate and hostname verification for TLS and STARTTLS.
func (c IMAPConfig) TLSConfig() *tls.Config {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: c.host,
	}
	if c.rootCAs != nil {
		tlsConfig.RootCAs = c.rootCAs.Clone()
	}
	return tlsConfig
}

type fileConfig struct {
	Readonly     *bool   `toml:"readonly"`
	TrashMailbox *string `toml:"trash_mailbox"`
	Cache        struct {
		Path string `toml:"path"`
	} `toml:"cache"`
	IMAP struct {
		Host                    string  `toml:"host"`
		Port                    *int    `toml:"port"`
		Username                string  `toml:"username"`
		Password                string  `toml:"password"`
		TLS                     *string `toml:"tls"`
		CAFile                  string  `toml:"ca_file"`
		ConnectTimeoutSeconds   *int64  `toml:"connect_timeout_seconds"`
		OperationTimeoutSeconds *int64  `toml:"operation_timeout_seconds"`
	} `toml:"imap"`
}

// Load reads and validates a TOML file without including its contents in errors.
func Load(path string) (*Config, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("cannot resolve configuration path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read configuration file")
	}
	var input fileConfig
	metadata, err := toml.Decode(string(data), &input)
	if err != nil {
		return nil, errors.New("invalid TOML configuration or field type")
	}
	if len(metadata.Undecoded()) != 0 {
		return nil, errors.New("configuration contains unknown keys")
	}

	cfg := &Config{readonly: true, trashMailbox: "Trash"}
	if input.Readonly != nil {
		cfg.readonly = *input.Readonly
	}
	if input.TrashMailbox != nil {
		cfg.trashMailbox = *input.TrashMailbox
	}
	if strings.TrimSpace(cfg.trashMailbox) == "" {
		return nil, errors.New("trash_mailbox must not be empty")
	}

	imap := &cfg.imap
	imap.host = input.IMAP.Host
	imap.username = input.IMAP.Username
	imap.password = input.IMAP.Password
	if strings.TrimSpace(imap.host) == "" {
		return nil, errors.New("imap.host is required")
	}
	if strings.TrimSpace(imap.username) == "" {
		return nil, errors.New("imap.username is required")
	}
	if imap.password == "" {
		return nil, errors.New("imap.password is required")
	}

	// Automatic transport selection and port defaults are defined in docs/adr/0007-imap-transport-modes.md.
	imap.tlsMode = "auto"
	if input.IMAP.TLS != nil {
		imap.tlsMode = *input.IMAP.TLS
	}
	switch imap.tlsMode {
	case "auto", "tls":
		imap.port = 993
	case "starttls", "none":
		imap.port = 143
	default:
		return nil, errors.New("imap.tls must be auto, tls, starttls, or none")
	}
	if input.IMAP.Port != nil {
		imap.port = *input.IMAP.Port
	}
	if imap.port < 1 || imap.port > 65535 {
		return nil, errors.New("imap.port must be between 1 and 65535")
	}

	imap.connectTimeout, err = timeout(input.IMAP.ConnectTimeoutSeconds, 10)
	if err != nil {
		return nil, errors.New("imap.connect_timeout_seconds must be positive and fit a time.Duration")
	}
	imap.operationTimeout, err = timeout(input.IMAP.OperationTimeoutSeconds, 60)
	if err != nil {
		return nil, errors.New("imap.operation_timeout_seconds must be positive and fit a time.Duration")
	}

	if input.IMAP.CAFile != "" {
		imap.caFile = input.IMAP.CAFile
		if !filepath.IsAbs(imap.caFile) {
			imap.caFile = filepath.Join(filepath.Dir(path), imap.caFile)
		}
		pem, err := os.ReadFile(imap.caFile)
		if err != nil {
			return nil, errors.New("cannot read imap.ca_file")
		}
		imap.rootCAs, err = x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("cannot load system certificate authorities")
		}
		if !imap.rootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("imap.ca_file contains no valid certificates")
		}
	}
	cfg.cachePath = input.Cache.Path
	if cfg.cachePath == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return nil, errors.New("cannot resolve user cache directory; set cache.path explicitly")
		}
		cfg.cachePath = filepath.Join(cacheDir, "imap-mcp", "headers.sqlite3")
	}
	if !filepath.IsAbs(cfg.cachePath) {
		cfg.cachePath = filepath.Join(filepath.Dir(path), cfg.cachePath)
	}
	return cfg, nil
}

func timeout(seconds *int64, fallback int64) (time.Duration, error) {
	value := fallback
	if seconds != nil {
		value = *seconds
	}
	if value <= 0 || value > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("invalid timeout")
	}
	return time.Duration(value) * time.Second, nil
}
