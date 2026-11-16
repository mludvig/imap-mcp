package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArgumentErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--config", "relative.toml"}, {"--config", "/missing.toml", "extra"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 2 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatalf("args %v: code=%d stderr=%q", args, code, stderr.String())
		}
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stderr); code != 0 {
		t.Fatalf("help code = %d", code)
	}
}

func TestConfigurationErrorsDoNotExposeCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private-config.toml")
	for _, contents := range []string{
		"[imap]\npassword = \"super-secret",
		"[imap]\nhost = \"example.test\"\nusername = \"test\"\npassword = \"super-secret\"\n[cache]\npath = \"/dev/null/secret-database\"\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := run(context.Background(), []string{"--config", path}, &stderr); code != 1 {
			t.Fatalf("code = %d", code)
		}
		for _, secret := range []string{"super-secret", "private-config", "secret-database"} {
			if strings.Contains(stderr.String(), secret) {
				t.Fatalf("error disclosed %q: %s", secret, stderr.String())
			}
		}
	}
}
