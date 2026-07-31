package middlemonitor

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// LogLevel represents a log level
type LogLevel string

const (
	LogLevelDEBUG LogLevel = "DEBUG"
	LogLevelINFO  LogLevel = "INFO"
	LogLevelWARN  LogLevel = "WARN"
	LogLevelERROR LogLevel = "ERROR"
	LogLevelFATAL LogLevel = "FATAL"
	LogLevelPANIC LogLevel = "PANIC"
)

// Config represents the Middle-Monitor SDK configuration
type Config struct {
	// OTLP endpoint (OTEL Collector or backend)
	Endpoint string

	// Insecure disables TLS (use HTTP instead of HTTPS). Set true for http:// or localhost.
	Insecure bool

	// Service information
	Service string
	Token   string

	// Export protocol (grpc or http, default: http)
	Protocol string

	// PprofURL points CaptureCPUProfile/CaptureHeapProfile at an external pprof
	// HTTP server (e.g. http://localhost:6060) instead of profiling this
	// process. Empty (the default) profiles in-process, with no socket to expose.
	PprofURL string

	// Hostname labels every export with the host the data comes from, so a CPU
	// or memory anomaly on a host correlates with the traffic of the services
	// running on it. Defaults to os.Hostname() — inside a container that is the
	// container ID, so set MIDDLE_MONITOR_HOSTNAME to the host as Middle-Monitor
	// names it, otherwise nothing joins the two sides.
	Hostname string

	// DisableHTTPErrorReporting stops the HTTP middlewares from submitting 5xx
	// responses to the Errors view. Set it when the application already reports
	// its own errors: otherwise every failure is recorded twice, once by the
	// application with the real cause and once by the middleware with whatever
	// the response body happens to carry. Panics are still always reported.
	DisableHTTPErrorReporting bool

	// ClientIP decides what the HTTP middlewares record as the caller's address
	// on each request log. An IP address is personal data: the default keeps the
	// network (203.0.113.0) and drops the host part, which still tells a scanner
	// apart from real traffic. Set ClientIPFull only with a documented legal
	// basis, ClientIPOff to record nothing.
	ClientIP ClientIPMode

	// Sampling configuration
	Sampling SamplingConfig
}

// SamplingConfig configures sampling for traces and logs
type SamplingConfig struct {
	// Sampling for traces
	Traces TracesSamplingConfig

	// Sampling for logs
	Logs LogsSamplingConfig
}

// TracesSamplingConfig configures trace sampling
type TracesSamplingConfig struct {
	// Percentage of traces to sample (0.0 - 1.0)
	// -1 means auto (uses the default percentage)
	Percentage float64

	// Always sample traces with errors (default: true)
	AlwaysSampleErrors bool

	// Routes to always sample (default: empty = all)
	AlwaysSampleRoutes []string

	// Routes to never sample (default: ["/health", "/metrics", "/ready"])
	NeverSampleRoutes []string
}

// LogsSamplingConfig configures log sampling
type LogsSamplingConfig struct {
	// Log levels to capture (default: [ERROR, FATAL, PANIC])
	Levels []LogLevel

	// Capture logs for HTTP status >= this code (default: 500)
	// 4xx are customer-facing; only 5xx (and panics) are reported by default.
	// 0 = disable HTTP status filtering
	MinHTTPStatus int

	// Capture all logs linked to a trace with error (default: true)
	CaptureOnTraceError bool

	// Routes to always capture logs (default: empty = all)
	AlwaysCaptureRoutes []string

	// Routes to never capture logs (default: ["/health", "/metrics", "/ready"])
	NeverCaptureRoutes []string
}

// DefaultSamplingConfig returns the default sampling configuration.
func DefaultSamplingConfig() SamplingConfig {
	// Default trace sampling. Errors are always sampled regardless (see below).
	// Override with MIDDLE_MONITOR_TRACES_SAMPLING or the Sampling config.
	percentage := 0.10

	return SamplingConfig{
		Traces: TracesSamplingConfig{
			Percentage:         percentage,
			AlwaysSampleErrors: true,
			AlwaysSampleRoutes: []string{},
			NeverSampleRoutes:  []string{"/health", "/metrics", "/ready", "/healthz", "/readyz"},
		},
		Logs: LogsSamplingConfig{
			Levels:              []LogLevel{LogLevelERROR, LogLevelFATAL, LogLevelPANIC},
			MinHTTPStatus:       500,
			CaptureOnTraceError: true,
			AlwaysCaptureRoutes: []string{},
			NeverCaptureRoutes:  []string{"/health", "/metrics", "/ready", "/healthz", "/readyz"},
		},
	}
}

// NewConfig creates a new configuration with defaults
func NewConfig(endpoint, service, token string) *Config {
	// Default OTLP endpoint to the Middle-Monitor ingestion endpoint so that
	// traces/logs are sent even when MIDDLE_MONITOR_API_URL is not set.
	usedDefault := false
	if endpoint == "" {
		endpoint = "https://api.middlemonitor.io"
		usedDefault = true
	}
	// Insecure = use HTTP (no TLS). Only an explicit http:// endpoint is insecure.
	insecure := strings.HasPrefix(endpoint, "http://")
	// OTLP WithEndpoint expects "host:port" without scheme
	normalized := normalizeOTLPEndpoint(endpoint)
	if usedDefault {
		log.Printf("[Middle-Monitor] Using default OTLP endpoint %s (set MIDDLE_MONITOR_API_URL if your backend uses a different URL)", normalized)
	}
	return &Config{
		Endpoint: normalized,
		Insecure: insecure,
		Service:  service,
		Token:    token,
		Protocol: "http", // default to http
		PprofURL: "",     // empty profiles this process, no external pprof server
		Hostname: defaultHostname(),
		ClientIP: ClientIPAnonymized,
		Sampling: DefaultSamplingConfig(),
	}
}

// hostnameFromOS is resolved once: it cannot change while the process runs. An
// unresolvable hostname is not an error — the data is exported without a host
// label rather than with a wrong one.
var hostnameFromOS = sync.OnceValue(func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
})

// defaultHostname returns the host label for exports: MIDDLE_MONITOR_HOSTNAME
// when the deployment sets it, the OS hostname otherwise. Resolved here rather
// than in ConfigFromEnv alone so InitWithConfig, which never reads the
// environment, still labels its exports with the right host.
func defaultHostname() string {
	if h := strings.TrimSpace(os.Getenv("MIDDLE_MONITOR_HOSTNAME")); h != "" {
		return h
	}
	return hostnameFromOS()
}

// normalizeOTLPEndpoint returns "host:port" for OTLP WithEndpoint (no scheme, no path)
func normalizeOTLPEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "api.middlemonitor.io"
	}
	// Strip scheme
	if strings.HasPrefix(raw, "https://") {
		raw = raw[len("https://"):]
	} else if strings.HasPrefix(raw, "http://") {
		raw = raw[len("http://"):]
	}
	// Strip path if any
	if idx := strings.Index(raw, "/"); idx >= 0 {
		raw = raw[:idx]
	}
	return raw
}

// ConfigFromEnv creates configuration from environment variables
func ConfigFromEnv() (*Config, error) {
	endpoint := os.Getenv("MIDDLE_MONITOR_API_URL")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		if endpoint == "" {
			endpoint = "https://api.middlemonitor.io" // Default: Middle-Monitor ingestion endpoint
		}
	}

	service := os.Getenv("MIDDLE_MONITOR_SERVICE")
	if service == "" {
		service = os.Getenv("OTEL_SERVICE_NAME")
		if service == "" {
			service = "unknown"
		}
	}

	token := os.Getenv("MIDDLE_MONITOR_TOKEN")
	if token == "" {
		token = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
		// Parse headers if provided (format: "key=value,key2=value2")
		if token != "" && strings.Contains(token, "=") {
			parts := strings.Split(token, ",")
			for _, part := range parts {
				kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
				if len(kv) == 2 && strings.ToLower(kv[0]) == "authorization" {
					token = strings.TrimPrefix(kv[1], "Bearer ")
					break
				}
			}
		}
	}

	protocol := os.Getenv("MIDDLE_MONITOR_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
		if protocol == "" {
			protocol = "http"
		}
	}

	config := NewConfig(endpoint, service, token)
	config.Protocol = protocol

	// Only an explicit URL switches profiling to an external pprof server; the
	// empty default profiles this process directly.
	if pprofURL := os.Getenv("MIDDLE_MONITOR_PPROF_URL"); pprofURL != "" {
		config.PprofURL = strings.TrimSuffix(strings.TrimSpace(pprofURL), "/")
	}

	if v := os.Getenv("MIDDLE_MONITOR_DISABLE_HTTP_ERROR_REPORTING"); v != "" {
		disable, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("MIDDLE_MONITOR_DISABLE_HTTP_ERROR_REPORTING: %w", ErrBoolValue)
		}
		config.DisableHTTPErrorReporting = disable
	}

	if v := os.Getenv("MIDDLE_MONITOR_CLIENT_IP"); v != "" {
		mode := ClientIPMode(strings.ToLower(strings.TrimSpace(v)))
		switch mode {
		case ClientIPAnonymized, ClientIPFull, ClientIPOff:
			config.ClientIP = mode
		default:
			return nil, fmt.Errorf("MIDDLE_MONITOR_CLIENT_IP: %w", ErrClientIPMode)
		}
	}

	// Parse sampling configuration from environment
	if err := config.parseSamplingFromEnv(); err != nil {
		return nil, fmt.Errorf("parse sampling: %w", ErrSamplingConfig)
	}

	return config, nil
}

// parseSamplingFromEnv parses sampling configuration from environment variables
func (c *Config) parseSamplingFromEnv() error {
	// Traces sampling
	if tracesPercentage := os.Getenv("MIDDLE_MONITOR_TRACES_SAMPLING"); tracesPercentage != "" {
		percentage, err := strconv.ParseFloat(tracesPercentage, 64)
		if err != nil {
			return fmt.Errorf("MIDDLE_MONITOR_TRACES_SAMPLING: %w", ErrSamplingValue)
		}
		if percentage < -1 || percentage > 1 {
			return ErrSamplingRange
		}
		c.Sampling.Traces.Percentage = percentage
	}

	// Logs levels
	if logsLevels := os.Getenv("MIDDLE_MONITOR_LOGS_LEVELS"); logsLevels != "" {
		levels := []LogLevel{}
		for _, levelStr := range strings.Split(logsLevels, ",") {
			level := LogLevel(strings.TrimSpace(strings.ToUpper(levelStr)))
			switch level {
			case LogLevelDEBUG, LogLevelINFO, LogLevelWARN, LogLevelERROR, LogLevelFATAL, LogLevelPANIC:
				levels = append(levels, level)
			default:
				return fmt.Errorf("%s: %w", levelStr, ErrLogLevel)
			}
		}
		if len(levels) > 0 {
			c.Sampling.Logs.Levels = levels
		}
	}

	// Min HTTP status
	if minHTTPStatus := os.Getenv("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS"); minHTTPStatus != "" {
		status, err := strconv.Atoi(minHTTPStatus)
		if err != nil {
			return fmt.Errorf("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS: %w", ErrMinHTTPStatus)
		}
		c.Sampling.Logs.MinHTTPStatus = status
	}

	return nil
}

// ShouldSampleTrace determines if a trace should be sampled
func (c *Config) ShouldSampleTrace(route string, hasError bool) bool {
	// Never sample certain routes (unless error and AlwaysSampleErrors)
	for _, pattern := range c.Sampling.Traces.NeverSampleRoutes {
		if matchesRoute(route, pattern) {
			if c.Sampling.Traces.AlwaysSampleErrors && hasError {
				return true
			}
			return false
		}
	}

	// Always sample certain routes
	for _, pattern := range c.Sampling.Traces.AlwaysSampleRoutes {
		if matchesRoute(route, pattern) {
			return true
		}
	}

	// Always sample errors
	if c.Sampling.Traces.AlwaysSampleErrors && hasError {
		return true
	}

	// Probabilistic sampling
	percentage := c.Sampling.Traces.Percentage
	if percentage < 0 {
		// Auto: use the default sampling percentage.
		percentage = DefaultSamplingConfig().Traces.Percentage
	}

	// Simple probabilistic sampling (can be improved with trace ID based sampling)
	// For now, we'll use a simple random check
	return percentage >= 1.0 || (percentage > 0 && shouldSampleProbabilistic(percentage))
}

// ShouldSampleLog determines if a log should be sampled
func (c *Config) ShouldSampleLog(route string, level LogLevel, httpStatus int, traceHasError bool) bool {
	// Never capture certain routes (unless error status >= MinHTTPStatus)
	for _, pattern := range c.Sampling.Logs.NeverCaptureRoutes {
		if matchesRoute(route, pattern) {
			if c.Sampling.Logs.MinHTTPStatus > 0 && httpStatus >= c.Sampling.Logs.MinHTTPStatus {
				return true
			}
			return false
		}
	}

	// Always capture certain routes
	for _, pattern := range c.Sampling.Logs.AlwaysCaptureRoutes {
		if matchesRoute(route, pattern) {
			return true
		}
	}

	// Capture if HTTP status >= min
	if c.Sampling.Logs.MinHTTPStatus > 0 && httpStatus >= c.Sampling.Logs.MinHTTPStatus {
		return true
	}

	// Capture if log level matches
	for _, configLevel := range c.Sampling.Logs.Levels {
		if level == configLevel {
			return true
		}
	}

	// Capture if trace has error
	if c.Sampling.Logs.CaptureOnTraceError && traceHasError {
		return true
	}

	return false
}

// matchesRoute checks if a route matches a pattern (supports wildcards)
func matchesRoute(route, pattern string) bool {
	// Exact match
	if route == pattern {
		return true
	}

	// Simple wildcard support (*)
	if strings.Contains(pattern, "*") {
		regexPattern := "^" + regexp.QuoteMeta(pattern) + "$"
		regexPattern = strings.ReplaceAll(regexPattern, "\\*", ".*")
		matched, err := regexp.MatchString(regexPattern, route)
		if err == nil && matched {
			return true
		}
	}

	return false
}

// shouldSampleProbabilistic performs probabilistic sampling by drawing a random
// value in [0,1) and keeping the span when it falls under the configured rate.
// (Trace-ID-based sampling is handled at the OTel Sampler level; this is the
// per-decision fallback for spans the SDK evaluates directly.)
func shouldSampleProbabilistic(percentage float64) bool {
	if percentage <= 0 {
		return false
	}
	if percentage >= 1.0 {
		return true
	}
	return rand.Float64() < percentage
}
