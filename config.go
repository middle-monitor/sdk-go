package middlemonitor

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
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

	// PprofURL is the base URL of the process's pprof HTTP server (e.g. http://localhost:6060).
	// Used by CaptureCPUProfile/CaptureHeapProfile to fetch profiles. Empty defaults to http://localhost:6060.
	PprofURL string

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
		PprofURL: "",     // default applied when capturing (http://localhost:6060)
		Sampling: DefaultSamplingConfig(),
	}
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

	if pprofURL := os.Getenv("MIDDLE_MONITOR_PPROF_URL"); pprofURL != "" {
		config.PprofURL = strings.TrimSuffix(strings.TrimSpace(pprofURL), "/")
	} else {
		config.PprofURL = "http://localhost:6060"
	}

	// Parse sampling configuration from environment
	if err := config.parseSamplingFromEnv(); err != nil {
		return nil, fmt.Errorf("failed to parse sampling config: %w", err)
	}

	return config, nil
}

// parseSamplingFromEnv parses sampling configuration from environment variables
func (c *Config) parseSamplingFromEnv() error {
	// Traces sampling
	if tracesPercentage := os.Getenv("MIDDLE_MONITOR_TRACES_SAMPLING"); tracesPercentage != "" {
		percentage, err := strconv.ParseFloat(tracesPercentage, 64)
		if err != nil {
			return fmt.Errorf("invalid MIDDLE_MONITOR_TRACES_SAMPLING: %w", err)
		}
		if percentage < -1 || percentage > 1 {
			return fmt.Errorf("MIDDLE_MONITOR_TRACES_SAMPLING must be between -1 and 1")
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
				return fmt.Errorf("invalid log level: %s", levelStr)
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
			return fmt.Errorf("invalid MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS: %w", err)
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
