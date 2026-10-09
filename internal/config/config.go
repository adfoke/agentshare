// Package config resolves the runtime configuration from flags and environment
// variables. Precedence is flag > environment > built-in default, so a systemd
// unit can set the environment once and an operator can still override a single
// value on the command line.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is prepended to every environment variable name agentshare reads.
const EnvPrefix = "AGENTSHARE_"

// Config is the effective configuration of one running instance.
type Config struct {
	// Listen is the address the HTTP server binds to.
	Listen string
	// DataDir holds the metadata database, the blob store and (when no admin
	// token is supplied) the generated root token file.
	DataDir string

	// BaseURL is the public prefix used to build absolute URLs in responses.
	// Empty means "derive it from each request".
	BaseURL string

	// AdminToken is the root credential. Empty means "load or generate one in
	// DataDir".
	AdminToken string

	// MaxUpload caps the size of a single upload in bytes. 0 disables the cap.
	MaxUpload int64
	// MaxTTL caps every requested lifetime. 0 disables the cap.
	MaxTTL time.Duration
	// DefaultTTL is applied when an upload asks for no explicit lifetime.
	// 0 means uploaded files never expire on their own.
	DefaultTTL time.Duration

	// CORSOrigin is the value of Access-Control-Allow-Origin. "*" is the
	// default because authentication is header-based, never cookie-based.
	CORSOrigin string

	// TrustedProxies is the CIDR list whose X-Forwarded-For is believed.
	TrustedProxies []string
	// BehindProxy enables honouring X-Forwarded-Proto / X-Forwarded-Host when
	// building URLs. Off by default: with it off, a client cannot influence the
	// URLs agentshare prints back at it.
	BehindProxy bool

	// JanitorEvery is how often expired rows are swept and orphaned blobs are
	// collected. 0 disables the janitor entirely.
	JanitorEvery time.Duration

	LogLevel  string // debug | info | warn | error
	LogFormat string // json | text

	Debug       bool
	ShowVersion bool
}

// ErrHelp is returned when the user asked for help; main exits 0.
var ErrHelp = flag.ErrHelp

func env(key, def string) string {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok && v != "" {
		return v
	}
	return def
}

// Load parses args (without the program name) into a Config.
func Load(args []string) (*Config, error) {
	cfg := &Config{}

	var (
		maxUploadRaw = env("MAX_UPLOAD", "5G")
		maxTTLRaw    = env("MAX_TTL", "0")
		defTTLRaw    = env("DEFAULT_TTL", "0")
		janitorRaw   = env("JANITOR_INTERVAL", "1m")
		behindRaw    = env("BEHIND_PROXY", "false")
		debugRaw     = env("DEBUG", "false")
		proxiesRaw   = env("TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	)

	fs := flag.NewFlagSet("agentshare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.Listen, "listen", env("LISTEN", "127.0.0.1:8080"),
		"address to bind, e.g. 0.0.0.0:8080")
	fs.StringVar(&cfg.DataDir, "data-dir", env("DATA_DIR", "./data"),
		"directory holding the database and the blob store")
	fs.StringVar(&cfg.BaseURL, "base-url", env("BASE_URL", ""),
		"public URL prefix used to build absolute links (default: derive per request)")
	fs.StringVar(&cfg.AdminToken, "admin-token", env("ADMIN_TOKEN", ""),
		"root token; empty means load data-dir/admin.token or generate one")
	fs.StringVar(&maxUploadRaw, "max-upload", maxUploadRaw,
		"maximum size of one upload, e.g. 512M, 5G, 0 for unlimited")
	fs.StringVar(&maxTTLRaw, "max-ttl", maxTTLRaw,
		"cap on every requested lifetime, e.g. 720h (0 = no cap)")
	fs.StringVar(&defTTLRaw, "default-ttl", defTTLRaw,
		"lifetime applied when an upload does not ask for one (0 = keep forever)")
	fs.StringVar(&cfg.CORSOrigin, "cors-origin", env("CORS_ORIGIN", "*"),
		"Access-Control-Allow-Origin value; empty disables CORS headers")
	fs.StringVar(&proxiesRaw, "trusted-proxies", proxiesRaw,
		"comma separated CIDRs whose X-Forwarded-For is trusted")
	fs.StringVar(&behindRaw, "behind-proxy", behindRaw,
		"honour X-Forwarded-Proto/Host when building URLs")
	fs.StringVar(&janitorRaw, "janitor-interval", janitorRaw,
		"how often to sweep expired records and orphaned blobs (0 = disabled)")
	fs.StringVar(&cfg.LogLevel, "log-level", env("LOG_LEVEL", "info"),
		"debug | info | warn | error")
	fs.StringVar(&cfg.LogFormat, "log-format", env("LOG_FORMAT", defaultLogFormat()),
		"json | text")
	fs.StringVar(&debugRaw, "debug", debugRaw, "enable debug logging and gin's debug mode")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print the version and exit")

	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "agentshare - agent-native file exchange over a plain HTTP API.\n\n")
		fmt.Fprintf(out, "Usage:\n  agentshare [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nEnvironment: every flag above also reads %s<NAME>,\n", EnvPrefix)
		fmt.Fprintf(out, "e.g. %sLISTEN, %sDATA_DIR, %sADMIN_TOKEN.\n", EnvPrefix, EnvPrefix, EnvPrefix)
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (agentshare takes flags only)", fs.Arg(0))
	}
	if cfg.ShowVersion {
		return cfg, nil
	}

	var err error
	if cfg.MaxUpload, err = ParseSize(maxUploadRaw); err != nil {
		return nil, fmt.Errorf("--max-upload: %w", err)
	}
	if cfg.MaxTTL, err = ParseDuration(maxTTLRaw); err != nil {
		return nil, fmt.Errorf("--max-ttl: %w", err)
	}
	if cfg.DefaultTTL, err = ParseDuration(defTTLRaw); err != nil {
		return nil, fmt.Errorf("--default-ttl: %w", err)
	}
	if cfg.JanitorEvery, err = ParseDuration(janitorRaw); err != nil {
		return nil, fmt.Errorf("--janitor-interval: %w", err)
	}
	if cfg.BehindProxy, err = strconv.ParseBool(strings.TrimSpace(behindRaw)); err != nil {
		return nil, fmt.Errorf("--behind-proxy: %w", err)
	}
	if cfg.Debug, err = strconv.ParseBool(strings.TrimSpace(debugRaw)); err != nil {
		return nil, fmt.Errorf("--debug: %w", err)
	}

	if cfg.MaxTTL > 0 && cfg.DefaultTTL > cfg.MaxTTL {
		cfg.DefaultTTL = cfg.MaxTTL
	}

	for _, p := range strings.Split(proxiesRaw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.TrustedProxies = append(cfg.TrustedProxies, p)
		}
	}

	switch strings.ToLower(cfg.LogLevel) {
	case "debug", "info", "warn", "error":
		cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	default:
		return nil, fmt.Errorf("--log-level: want debug|info|warn|error, got %q", cfg.LogLevel)
	}
	switch strings.ToLower(cfg.LogFormat) {
	case "json", "text":
		cfg.LogFormat = strings.ToLower(cfg.LogFormat)
	default:
		return nil, fmt.Errorf("--log-format: want json|text, got %q", cfg.LogFormat)
	}
	if cfg.Debug {
		cfg.LogLevel = "debug"
	}

	return cfg, nil
}

func defaultLogFormat() string {
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return "text"
	}
	return "json"
}

// ParseSize understands plain byte counts and the K/M/G/T suffixes
// (binary multiples: 1K = 1024). A trailing "B" is ignored, so "5GB" works too.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, errors.New("empty size")
	}
	t = strings.TrimSuffix(t, "B")
	if t == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := int64(1)
	switch t[len(t)-1] {
	case 'K':
		mult, t = 1<<10, t[:len(t)-1]
	case 'M':
		mult, t = 1<<20, t[:len(t)-1]
	case 'G':
		mult, t = 1<<30, t[:len(t)-1]
	case 'T':
		mult, t = 1<<40, t[:len(t)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: negative", s)
	}
	return n * mult, nil
}

// ParseDuration extends time.ParseDuration with day and week suffixes, because
// "7d" and "2w" are what people actually type for a file lifetime. A bare "0"
// and the words "never"/"none" all mean "no expiry".
func ParseDuration(s string) (time.Duration, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "", "0", "never", "none", "off":
		return 0, nil
	}
	if strings.HasSuffix(t, "d") || strings.HasSuffix(t, "w") {
		unit := 24 * time.Hour
		if strings.HasSuffix(t, "w") {
			unit = 7 * 24 * time.Hour
		}
		n, err := strconv.ParseFloat(t[:len(t)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		if n < 0 {
			return 0, fmt.Errorf("invalid duration %q: negative", s)
		}
		return time.Duration(n * float64(unit)), nil
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (want e.g. 30m, 24h, 7d, 2w)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: negative", s)
	}
	return d, nil
}
