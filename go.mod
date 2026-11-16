module github.com/mludvig/imap-mcp

go 1.25.0

// IMAP4rev2 VANISHED backport: see docs/adr/0004-backport-imap4rev2-vanished.md.
replace github.com/emersion/go-imap/v2 => ./third_party/go-imap

require (
	github.com/BurntSushi/toml v1.4.0
	github.com/emersion/go-imap/v2 v2.0.0-beta.9
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/gofrs/flock v0.13.1
	github.com/google/jsonschema-go v0.4.3
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/text v0.41.0
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
