package config

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "0", want: 0},
		{in: "1024", want: 1024},
		{in: "512K", want: 512 << 10},
		{in: "5M", want: 5 << 20},
		{in: "5G", want: 5 << 30},
		{in: "2GB", want: 2 << 30},
		{in: " 5g ", want: 5 << 30},
		{in: "1T", want: 1 << 40},
		{in: "abc", bad: true},
		{in: "-1", bad: true},
		{in: "", bad: true},
		{in: "G", bad: true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		// Every spelling of "no expiry" has to land on zero, because zero is
		// what decides whether a file is ever deleted.
		{in: "", want: 0},
		{in: "0", want: 0},
		{in: "never", want: 0},
		{in: "none", want: 0},
		{in: "30m", want: 30 * time.Minute},
		{in: "24h", want: 24 * time.Hour},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "1.5d", want: 36 * time.Hour},
		{in: "-5m", bad: true},
		{in: "banana", bad: true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLoadDefaultsAndFlags(t *testing.T) {
	t.Setenv("AGENTSHARE_LISTEN", "")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" || cfg.MaxUpload != 5<<30 || cfg.DefaultTTL != 0 {
		t.Fatalf("defaults look wrong: %+v", cfg)
	}
	// The data directory default must be relative: the service should be
	// runnable from any directory without writing outside it.
	if cfg.DataDir != "./data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}

	cfg, err = Load([]string{"--listen", "0.0.0.0:9000", "--max-upload", "1M", "--janitor-interval", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:9000" || cfg.MaxUpload != 1<<20 || cfg.JanitorEvery != 0 {
		t.Fatalf("flags ignored: %+v", cfg)
	}
}

func TestEnvIsRead(t *testing.T) {
	t.Setenv("AGENTSHARE_LISTEN", "0.0.0.0:7777")
	t.Setenv("AGENTSHARE_ADMIN_TOKEN", "env-token")
	t.Setenv("AGENTSHARE_DEFAULT_TTL", "12h")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:7777" || cfg.AdminToken != "env-token" || cfg.DefaultTTL != 12*time.Hour {
		t.Fatalf("environment ignored: %+v", cfg)
	}

	// A flag beats the environment.
	cfg, err = Load([]string{"--listen", "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:1" {
		t.Fatalf("flag did not override the environment: %q", cfg.Listen)
	}
}

func TestDefaultTTLIsClampedToMaxTTL(t *testing.T) {
	// Otherwise the server would refuse its own default.
	cfg, err := Load([]string{"--default-ttl", "30d", "--max-ttl", "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultTTL != time.Hour {
		t.Fatalf("DefaultTTL = %v, want it clamped to the cap", cfg.DefaultTTL)
	}
}

func TestBadValuesAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"--max-upload", "lots"},
		{"--janitor-interval", "sometimes"},
		{"--log-level", "loud"},
		{"--log-format", "yaml"},
		{"--behind-proxy", "maybe"},
		{"unexpected-positional"},
	} {
		if _, err := Load(args); err == nil {
			t.Errorf("Load(%v) should have failed", args)
		}
	}
}

func TestTrustedProxiesParsing(t *testing.T) {
	cfg, err := Load([]string{"--trusted-proxies", "127.0.0.1/32, ::1/128 ,10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 3 || cfg.TrustedProxies[2] != "10.0.0.0/8" {
		t.Fatalf("TrustedProxies = %v", cfg.TrustedProxies)
	}
}
