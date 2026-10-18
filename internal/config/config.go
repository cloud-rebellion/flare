// Package config loads Flare's runtime configuration from the environment.
// Production refuses to start when a security-critical secret is missing or
// left at its insecure default, per the repo security rules.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Environment string
	// Release is the version plus a full Git revision when the trusted builder
	// provided one. The server checks runtime declarations against the binary.
	Release string
	// SourceCommit is the exact Git revision declared by the trusted image
	// builder and stamped into the executable. Empty means no revision was
	// supplied; it is never inferred from the version tag.
	SourceCommit string
	Port         string
	BaseURL      string
	// MCPAllowedOrigins adds explicit browser origins for alternate Flare hosts.
	// BASE_URL is always the primary MCP origin; this does not enable CORS.
	MCPAllowedOrigins []string

	DatabaseURL   string
	DBMaxConns    int32
	DBMinConns    int32
	RetentionDays int

	// IngestRatePerMin caps events per minute per DSN key (or per IP when no
	// key), so a captured DSN cannot flood the write path.
	IngestRatePerMin int

	// ConnectorSyncEnabled opts the built-in provider scheduler in. It is off by
	// default because enabling it makes outbound provider calls for every active
	// installation with a stored credential. ConnectorSyncInterval controls the
	// bounded poll cadence when enabled.
	ConnectorSyncEnabled  bool
	ConnectorSyncInterval time.Duration
	// OAuth app registration is operator-owned. Empty values disable the
	// self-service HubSpot install flow; no workspace user supplies them.
	HubSpotOAuthClientID     string
	HubSpotOAuthClientSecret string

	// Cold-tier Parquet. Local dir by default (self-host friendly); S3/MinIO opt-in.
	ParquetDir string

	// AllowDropWithoutExport lets retention drop aged partitions even when the
	// cold tier is unreachable. Default false, which is fail-closed: partitions
	// pile up (disk grows, loudly logged) rather than telemetry being destroyed
	// without ever being archived. An operator running deliberately without a
	// cold tier sets FLARE_ALLOW_DROP_WITHOUT_EXPORT=true to restore pruning.
	// It cannot be inferred: FLARE_PARQUET_DIR always has a default value, so
	// "cold tier configured" is indistinguishable from "cold tier defaulted".
	AllowDropWithoutExport bool

	// AllowPrivateAIEndpoint disables the SSRF guard on the tenant-supplied
	// BYOAI base_url. The guard used to be tied to ENVIRONMENT=production, which
	// is backwards: development is the DEFAULT for a self-hosted deployment, so
	// the protection was off exactly where it was least likely to be noticed.
	// It is now on everywhere unless this is explicitly set, which is what a
	// developer pointing at a local Ollama/vLLM needs.
	AllowPrivateAIEndpoint bool

	// AllowSignup keeps POST /api/auth/register open after the first user
	// exists. Off by default: the route is the self-host FIRST-RUN path, and
	// users.sql has carried a CountUsers query documented as the "pre-auth
	// bootstrap check (is this a fresh install)" since it was written, with no
	// caller. Open registration on a deployed instance lets anyone mint an org,
	// and every new org carries its own INGEST_RATE_PER_MIN budget, so ingest
	// capacity scales with the number of orgs an attacker creates.
	//
	// Turn it on only where public signup is the product.
	AllowSignup bool

	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3UseSSL    bool

	SessionKey         string
	CSRFKey            string
	SessionLifetime    time.Duration
	SessionIdleTimeout time.Duration
	DisableCSRF        bool

	RedisURL string

	// SMTP for transactional email (alert delivery, password reset). Optional:
	// when SMTPHost/SMTPFrom are unset, email features quietly no-op so a
	// self-host without a mail server still runs.
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPass     string
	SMTPFrom     string
	SMTPFromName string
	SMTPTLS      string // "tls" (implicit 465) | "starttls" (587) | "none" (dev)

	// SecretKey (FLARE_SECRET_KEY) encrypts integration secrets at rest (BYOAI
	// key, OIDC client secret, GitHub token, channel webhook URLs). Optional and
	// fail-safe: when unset those secrets are stored plaintext (current behavior)
	// rather than the service failing. Provisioned by the deploy pipeline.
	SecretKey string
	// StageVerifyURL is an operator-owned, fixed HTTPS attestation endpoint.
	// Project admins supply only scoped Stage credentials and identity binding,
	// never the outbound destination.
	StageVerifyURL string
	// StageVerifyPeerIP pins an operator-approved peer (for example Stage's
	// Tailscale address). Only this exact IP may bypass the private-IP dial
	// guard; the HTTPS URL hostname still controls TLS certificate validation.
	StageVerifyPeerIP string
}

func (c Config) IsProduction() bool { return c.Environment == "production" }

// EmailEnabled reports whether transactional email can be sent.
func (c Config) EmailEnabled() bool { return c.SMTPHost != "" && c.SMTPFrom != "" }

func Load() (Config, error) {
	c := Config{
		Environment:              env("ENVIRONMENT", "development"),
		Port:                     env("PORT", "8080"),
		BaseURL:                  env("BASE_URL", "http://localhost:8080"),
		DatabaseURL:              env("DATABASE_URL", ""),
		DBMaxConns:               int32(envInt("DB_MAX_CONNS", 20)),
		DBMinConns:               int32(envInt("DB_MIN_CONNS", 2)),
		RetentionDays:            envInt("RETENTION_DAYS", 30),
		IngestRatePerMin:         envInt("INGEST_RATE_PER_MIN", 1200),
		ConnectorSyncEnabled:     isTrue(env("FLARE_CONNECTOR_SYNC_ENABLED", "")),
		ConnectorSyncInterval:    envDuration("FLARE_CONNECTOR_SYNC_INTERVAL", 5*time.Minute),
		HubSpotOAuthClientID:     env("FLARE_HUBSPOT_OAUTH_CLIENT_ID", ""),
		HubSpotOAuthClientSecret: env("FLARE_HUBSPOT_OAUTH_CLIENT_SECRET", ""),
		ParquetDir:               env("FLARE_PARQUET_DIR", "data/parquet"),
		S3Endpoint:               env("FLARE_PARQUET_S3_ENDPOINT", ""),
		S3Bucket:                 env("FLARE_PARQUET_S3_BUCKET", "flare"),
		S3AccessKey:              env("FLARE_PARQUET_S3_ACCESS_KEY", ""),
		S3SecretKey:              env("FLARE_PARQUET_S3_SECRET_KEY", ""),
		S3Region:                 env("FLARE_PARQUET_S3_REGION", "us-east-1"),
		// Fail CLOSED: any spelling other than an explicit false keeps TLS on.
		// Comparing against "true" meant "TRUE", "1" or a typo silently sent
		// object-storage credentials in the clear.
		S3UseSSL:               !isFalse(env("FLARE_PARQUET_S3_USE_SSL", "true")),
		AllowPrivateAIEndpoint: isTrue(env("FLARE_ALLOW_PRIVATE_AI_ENDPOINT", "")),
		AllowSignup:            isTrue(env("FLARE_ALLOW_SIGNUP", "")),
		SessionKey:             env("SESSION_KEY", ""),
		CSRFKey:                env("CSRF_KEY", ""),
		SessionLifetime:        time.Duration(envInt("SESSION_LIFETIME_HOURS", 720)) * time.Hour,
		SessionIdleTimeout:     time.Duration(envInt("SESSION_IDLE_HOURS", 168)) * time.Hour,
		DisableCSRF:            isTrue(env("DISABLE_CSRF", "")),
		RedisURL:               env("REDIS_URL", ""),
		SMTPHost:               env("SMTP_HOST", ""),
		SMTPPort:               envInt("SMTP_PORT", 587),
		SMTPUser:               env("SMTP_USER", ""),
		SMTPPass:               env("SMTP_PASS", ""),
		SMTPFrom:               env("SMTP_FROM", ""),
		SMTPFromName:           env("SMTP_FROM_NAME", "Flare"),
		SMTPTLS:                env("SMTP_TLS", "starttls"),
		SecretKey:              env("FLARE_SECRET_KEY", ""),
		StageVerifyURL:         env("FLARE_STAGE_VERIFY_URL", ""),
		StageVerifyPeerIP:      env("FLARE_STAGE_VERIFY_PEER_IP", ""),
		// isTrue, not == "true". Both of these currently fail SAFE, but three
		// booleans parsed three ways in one struct literal is how the next one
		// lands on the wrong side.
		AllowDropWithoutExport: isTrue(env("FLARE_ALLOW_DROP_WITHOUT_EXPORT", "")),
	}

	var err error
	c.MCPAllowedOrigins, err = parseMCPAllowedOrigins(os.Getenv("FLARE_MCP_ALLOWED_ORIGINS"), c.IsProduction())
	if err != nil {
		return c, fmt.Errorf("FLARE_MCP_ALLOWED_ORIGINS: %w", err)
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if (c.HubSpotOAuthClientID == "") != (c.HubSpotOAuthClientSecret == "") {
		return c, fmt.Errorf("FLARE_HUBSPOT_OAUTH_CLIENT_ID and FLARE_HUBSPOT_OAUTH_CLIENT_SECRET must be configured together")
	}
	if c.HubSpotOAuthClientID != "" {
		base, err := url.Parse(c.BaseURL)
		if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" ||
			(base.Scheme != "https" && !(base.Scheme == "http" && !c.IsProduction() && (base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1"))) {
			return c, fmt.Errorf("BASE_URL must be an HTTPS origin (or localhost HTTP in development) when HubSpot OAuth is enabled")
		}
	}

	// In production every secret must be explicitly set. Auth is never optional.
	if c.IsProduction() {
		var missing []string
		if c.SessionKey == "" {
			missing = append(missing, "SESSION_KEY")
		}
		if c.CSRFKey == "" {
			missing = append(missing, "CSRF_KEY")
		}
		// FLARE_SECRET_KEY encrypts integration secrets (BYOAI key, OIDC client
		// secret, GitHub token, webhook URLs) at rest. Without it those land in
		// the DB as plaintext, so a DB dump or backup leak exposes live
		// third-party credentials. Fail closed in production, never silently.
		if c.SecretKey == "" {
			missing = append(missing, "FLARE_SECRET_KEY")
		}
		if len(missing) > 0 {
			return c, fmt.Errorf("production requires: %s", strings.Join(missing, ", "))
		}
		// Present is not the same as set.
		//
		// "Required in production" was implemented as "non-empty", and
		// .env.example ships your_session_key_here_change_me,
		// your_csrf_key_32_bytes_change_me! and your_secret_key_here_change_me,
		// all of which are non-empty and all of which are PUBLISHED in the
		// open-source mirror. An instance provisioned from the example
		// therefore booted production with a publicly known CSRF signing key,
		// a publicly known session key and a publicly known at-rest key, and
		// nothing said a word. FLARE_SECRET_KEY=x also booted cleanly on one
		// byte of entropy.
		for _, k := range []struct {
			name, value string
			minBytes    int
		}{
			{"SESSION_KEY", c.SessionKey, 32},
			{"CSRF_KEY", c.CSRFKey, 32},
			{"FLARE_SECRET_KEY", c.SecretKey, 32},
		} {
			if isPlaceholderSecret(k.value) {
				return c, fmt.Errorf("%s is still the placeholder shipped in .env.example, "+
					"which is public. Generate one: openssl rand -base64 32", k.name)
			}
			if len(k.value) < k.minBytes {
				return c, fmt.Errorf("%s must be at least %d bytes (got %d). "+
					"Generate one: openssl rand -base64 32", k.name, k.minBytes, len(k.value))
			}
		}
	} else {
		// Dev-only fallbacks so the service boots without ceremony.
		if c.SessionKey == "" {
			c.SessionKey = "dev-session-key-not-for-production"
		}
		if c.CSRFKey == "" {
			// gorilla/csrf requires exactly 32 bytes or it panics at startup.
			c.CSRFKey = "dev-csrf-key-not-for-production0"
		}
	}

	return c, nil
}

// placeholderMarkers are the substrings the shipped examples use. Matched on
// the VALUE, so a real key can never trip them by accident: 32 bytes from
// openssl does not contain "change_me".
var placeholderMarkers = []string{
	"change_me", "changeme", "your_", "_here", "replace_me", "example",
}

// isPlaceholderSecret reports whether a value is one of the documented
// examples rather than a real secret.
func isPlaceholderSecret(v string) bool {
	l := strings.ToLower(v)
	for _, m := range placeholderMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// isTrue / isFalse accept the spellings operators actually type, so a boolean
// flag never silently takes the unsafe branch on a capitalisation difference.
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
