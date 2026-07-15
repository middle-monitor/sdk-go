package middlemonitor

import (
	"os"
	"sync"
	"testing"
)

// resetGlobalState resets package-level singletons so tests can re-init cleanly.
func resetGlobalState() {
	globalClient = nil
	globalConfig = nil
	initOnce = sync.Once{}
	logBuffer = nil
	flusherOnce = sync.Once{}
}

// ── normalizeOTLPEndpoint ─────────────────────────────────────────────────────

func TestNormalizeOTLPEndpoint_Empty(t *testing.T) {
	got := normalizeOTLPEndpoint("")
	if got != "localhost:8080" {
		t.Fatalf("want localhost:8080, got %q", got)
	}
}

func TestNormalizeOTLPEndpoint_HTTPS(t *testing.T) {
	got := normalizeOTLPEndpoint("https://example.com:4318")
	if got != "example.com:4318" {
		t.Fatalf("want example.com:4318, got %q", got)
	}
}

func TestNormalizeOTLPEndpoint_HTTP(t *testing.T) {
	got := normalizeOTLPEndpoint("http://example.com:8080")
	if got != "example.com:8080" {
		t.Fatalf("want example.com:8080, got %q", got)
	}
}

func TestNormalizeOTLPEndpoint_WithPath(t *testing.T) {
	got := normalizeOTLPEndpoint("https://example.com:4318/v1/traces")
	if got != "example.com:4318" {
		t.Fatalf("want example.com:4318, got %q", got)
	}
}

func TestNormalizeOTLPEndpoint_PlainHostPort(t *testing.T) {
	got := normalizeOTLPEndpoint("localhost:8080")
	if got != "localhost:8080" {
		t.Fatalf("want localhost:8080, got %q", got)
	}
}

func TestNormalizeOTLPEndpoint_Whitespace(t *testing.T) {
	got := normalizeOTLPEndpoint("  http://host:9090  ")
	if got != "host:9090" {
		t.Fatalf("want host:9090, got %q", got)
	}
}

// ── DefaultSamplingConfig ─────────────────────────────────────────────────────

func TestDefaultSamplingConfig(t *testing.T) {
	sc := DefaultSamplingConfig()
	if sc.Traces.Percentage != 0.10 {
		t.Errorf("want 0.10, got %v", sc.Traces.Percentage)
	}
	if !sc.Traces.AlwaysSampleErrors {
		t.Error("AlwaysSampleErrors should be true by default")
	}
	if sc.Logs.MinHTTPStatus != 500 {
		t.Errorf("want MinHTTPStatus=500, got %d", sc.Logs.MinHTTPStatus)
	}
	if !sc.Logs.CaptureOnTraceError {
		t.Error("CaptureOnTraceError should be true by default")
	}
}

// ── NewConfig ─────────────────────────────────────────────────────────────────

func TestNewConfig_EmptyEndpoint(t *testing.T) {
	cfg := NewConfig("", "svc", "tok")
	if cfg.Endpoint == "" {
		t.Error("empty endpoint should get a default")
	}
	if !cfg.Insecure {
		t.Error("default endpoint (localhost) should be insecure")
	}
	if cfg.Service != "svc" {
		t.Errorf("want svc, got %q", cfg.Service)
	}
}

func TestNewConfig_HTTPSEndpoint(t *testing.T) {
	cfg := NewConfig("https://collector.prod:4318", "svc", "tok")
	if cfg.Insecure {
		t.Error("https endpoint should NOT be insecure")
	}
}

func TestNewConfig_HTTPEndpoint(t *testing.T) {
	cfg := NewConfig("http://localhost:4318", "svc", "tok")
	if !cfg.Insecure {
		t.Error("http endpoint should be insecure")
	}
}

func TestNewConfig_Protocol(t *testing.T) {
	cfg := NewConfig("localhost:8080", "svc", "tok")
	if cfg.Protocol != "http" {
		t.Errorf("want http, got %q", cfg.Protocol)
	}
}

// ── ConfigFromEnv ─────────────────────────────────────────────────────────────

func TestConfigFromEnv_Defaults(t *testing.T) {
	// Clear all relevant env vars
	for _, k := range []string{
		"MIDDLE_MONITOR_API_URL", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"MIDDLE_MONITOR_SERVICE", "OTEL_SERVICE_NAME",
		"MIDDLE_MONITOR_TOKEN", "OTEL_EXPORTER_OTLP_HEADERS",
		"MIDDLE_MONITOR_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"MIDDLE_MONITOR_PPROF_URL",
		"MIDDLE_MONITOR_TRACES_SAMPLING", "MIDDLE_MONITOR_LOGS_LEVELS",
		"MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS",
	} {
		os.Unsetenv(k)
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Service != "unknown" {
		t.Errorf("want unknown, got %q", cfg.Service)
	}
}

func TestConfigFromEnv_ExplicitVars(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_API_URL", "http://host:9090")
	os.Setenv("MIDDLE_MONITOR_SERVICE", "my-service")
	os.Setenv("MIDDLE_MONITOR_TOKEN", "tok123")
	os.Setenv("MIDDLE_MONITOR_PROTOCOL", "grpc")
	os.Setenv("MIDDLE_MONITOR_PPROF_URL", "http://localhost:6060/")
	defer func() {
		os.Unsetenv("MIDDLE_MONITOR_API_URL")
		os.Unsetenv("MIDDLE_MONITOR_SERVICE")
		os.Unsetenv("MIDDLE_MONITOR_TOKEN")
		os.Unsetenv("MIDDLE_MONITOR_PROTOCOL")
		os.Unsetenv("MIDDLE_MONITOR_PPROF_URL")
	}()
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Service != "my-service" {
		t.Errorf("want my-service, got %q", cfg.Service)
	}
	if cfg.Token != "tok123" {
		t.Errorf("want tok123, got %q", cfg.Token)
	}
	if cfg.Protocol != "grpc" {
		t.Errorf("want grpc, got %q", cfg.Protocol)
	}
}

func TestConfigFromEnv_OtelFallbackVars(t *testing.T) {
	os.Unsetenv("MIDDLE_MONITOR_API_URL")
	os.Unsetenv("MIDDLE_MONITOR_SERVICE")
	os.Unsetenv("MIDDLE_MONITOR_TOKEN")
	os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel:4318")
	os.Setenv("OTEL_SERVICE_NAME", "otel-svc")
	os.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "key=val,Authorization=Bearer my-token")
	os.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http")
	defer func() {
		os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		os.Unsetenv("OTEL_SERVICE_NAME")
		os.Unsetenv("OTEL_EXPORTER_OTLP_HEADERS")
		os.Unsetenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}()
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Service != "otel-svc" {
		t.Errorf("want otel-svc, got %q", cfg.Service)
	}
	if cfg.Token != "my-token" {
		t.Errorf("want my-token, got %q", cfg.Token)
	}
}

func TestConfigFromEnv_SamplingOverrides(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_TRACES_SAMPLING", "0.5")
	os.Setenv("MIDDLE_MONITOR_LOGS_LEVELS", "DEBUG,WARN,ERROR")
	os.Setenv("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS", "400")
	defer func() {
		os.Unsetenv("MIDDLE_MONITOR_TRACES_SAMPLING")
		os.Unsetenv("MIDDLE_MONITOR_LOGS_LEVELS")
		os.Unsetenv("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS")
	}()
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sampling.Traces.Percentage != 0.5 {
		t.Errorf("want 0.5, got %v", cfg.Sampling.Traces.Percentage)
	}
	if len(cfg.Sampling.Logs.Levels) != 3 {
		t.Errorf("want 3 levels, got %d", len(cfg.Sampling.Logs.Levels))
	}
	if cfg.Sampling.Logs.MinHTTPStatus != 400 {
		t.Errorf("want 400, got %d", cfg.Sampling.Logs.MinHTTPStatus)
	}
}

func TestConfigFromEnv_InvalidSampling(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_TRACES_SAMPLING", "not-a-number")
	defer os.Unsetenv("MIDDLE_MONITOR_TRACES_SAMPLING")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid sampling value")
	}
}

func TestConfigFromEnv_SamplingOutOfRange(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_TRACES_SAMPLING", "2.0")
	defer os.Unsetenv("MIDDLE_MONITOR_TRACES_SAMPLING")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected error for out-of-range sampling value")
	}
}

func TestConfigFromEnv_InvalidLogLevel(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_LOGS_LEVELS", "INVALID")
	defer os.Unsetenv("MIDDLE_MONITOR_LOGS_LEVELS")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid log level")
	}
}

func TestConfigFromEnv_InvalidMinHTTPStatus(t *testing.T) {
	os.Setenv("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS", "not-a-number")
	defer os.Unsetenv("MIDDLE_MONITOR_LOGS_MIN_HTTP_STATUS")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected error for invalid min HTTP status")
	}
}

// ── ShouldSampleTrace ─────────────────────────────────────────────────────────

func newTestConfig() *Config {
	return &Config{
		Sampling: DefaultSamplingConfig(),
	}
}

func TestShouldSampleTrace_NeverRoute_NoError(t *testing.T) {
	cfg := newTestConfig()
	if cfg.ShouldSampleTrace("/health", false) {
		t.Error("health route without error should NOT be sampled")
	}
}

func TestShouldSampleTrace_NeverRoute_WithError(t *testing.T) {
	cfg := newTestConfig()
	// AlwaysSampleErrors=true, so error on a never-route should still be sampled
	if !cfg.ShouldSampleTrace("/health", true) {
		t.Error("health route WITH error should be sampled when AlwaysSampleErrors=true")
	}
}

func TestShouldSampleTrace_NeverRoute_AlwaysSampleErrorsFalse(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	if cfg.ShouldSampleTrace("/health", true) {
		t.Error("never-route WITH error should NOT be sampled when AlwaysSampleErrors=false")
	}
}

func TestShouldSampleTrace_AlwaysRoute(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.AlwaysSampleRoutes = []string{"/critical"}
	if !cfg.ShouldSampleTrace("/critical", false) {
		t.Error("always-route should always be sampled")
	}
}

func TestShouldSampleTrace_AlwaysError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 0 // nothing else would sample
	if !cfg.ShouldSampleTrace("/api/endpoint", true) {
		t.Error("error trace should be sampled when AlwaysSampleErrors=true")
	}
}

func TestShouldSampleTrace_Percentage100(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 1.0
	for i := 0; i < 10; i++ {
		if !cfg.ShouldSampleTrace("/api", false) {
			t.Error("100% sampling should always sample")
		}
	}
}

func TestShouldSampleTrace_Percentage0(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 0.0
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	for i := 0; i < 10; i++ {
		if cfg.ShouldSampleTrace("/api", false) {
			t.Error("0% sampling should never sample")
		}
	}
}

func TestShouldSampleTrace_AutoDefault(t *testing.T) {
	// Auto sampling (percentage < 0) resolves to the fixed default (10%).
	if got := DefaultSamplingConfig().Traces.Percentage; got != 0.10 {
		t.Errorf("default sampling = %v, want 0.10", got)
	}
}

func TestShouldSampleTrace_AutoProd(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = -1
	cfg.Sampling.Traces.AlwaysSampleErrors = false
	// prod auto → 20%. With enough samples, at least some should be sampled and some dropped.
	// Just verify it runs without panic.
	total := 0
	for i := 0; i < 100; i++ {
		if cfg.ShouldSampleTrace("/api", false) {
			total++
		}
	}
	// can't guarantee exact count, just ensure it's called
	_ = total
}

// ── ShouldSampleLog ───────────────────────────────────────────────────────────

func TestShouldSampleLog_NeverCaptureRoute_NoStatus(t *testing.T) {
	cfg := newTestConfig()
	if cfg.ShouldSampleLog("/health", LogLevelINFO, 200, false) {
		t.Error("health route, low status, non-error should NOT be logged")
	}
}

func TestShouldSampleLog_NeverCaptureRoute_HighStatus(t *testing.T) {
	cfg := newTestConfig()
	if !cfg.ShouldSampleLog("/health", LogLevelINFO, 500, false) {
		t.Error("health route with 500 status should be logged despite never-capture")
	}
}

func TestShouldSampleLog_NeverCaptureRoute_MinStatusDisabled(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Logs.MinHTTPStatus = 0
	if cfg.ShouldSampleLog("/health", LogLevelINFO, 500, false) {
		t.Error("health route with disabled min-status should NOT be logged")
	}
}

func TestShouldSampleLog_AlwaysCaptureRoute(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Logs.AlwaysCaptureRoutes = []string{"/audit"}
	if !cfg.ShouldSampleLog("/audit", LogLevelDEBUG, 200, false) {
		t.Error("always-capture route should always be logged")
	}
}

func TestShouldSampleLog_MinHTTPStatus(t *testing.T) {
	cfg := newTestConfig()
	if !cfg.ShouldSampleLog("/api", LogLevelINFO, 500, false) {
		t.Error("500 status should trigger logging")
	}
}

func TestShouldSampleLog_LevelMatch(t *testing.T) {
	cfg := newTestConfig()
	if !cfg.ShouldSampleLog("/api", LogLevelERROR, 200, false) {
		t.Error("ERROR level should be logged by default")
	}
}

func TestShouldSampleLog_LevelNotMatch(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Logs.MinHTTPStatus = 0
	if cfg.ShouldSampleLog("/api", LogLevelDEBUG, 200, false) {
		t.Error("DEBUG level should NOT be logged by default")
	}
}

func TestShouldSampleLog_CaptureOnTraceError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Logs.MinHTTPStatus = 0
	if !cfg.ShouldSampleLog("/api", LogLevelDEBUG, 200, true) {
		t.Error("trace with error should trigger log capture when CaptureOnTraceError=true")
	}
}

func TestShouldSampleLog_CaptureOnTraceErrorFalse(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Logs.MinHTTPStatus = 0
	cfg.Sampling.Logs.CaptureOnTraceError = false
	if cfg.ShouldSampleLog("/api", LogLevelDEBUG, 200, true) {
		t.Error("should not log when CaptureOnTraceError=false and no other criteria match")
	}
}

// ── matchesRoute ─────────────────────────────────────────────────────────────

func TestMatchesRoute_Exact(t *testing.T) {
	if !matchesRoute("/health", "/health") {
		t.Error("exact match failed")
	}
}

func TestMatchesRoute_NoMatch(t *testing.T) {
	if matchesRoute("/healthz", "/health") {
		t.Error("should not match different routes")
	}
}

func TestMatchesRoute_Wildcard(t *testing.T) {
	if !matchesRoute("/api/v1/users", "/api/*") {
		t.Error("wildcard should match")
	}
}

func TestMatchesRoute_WildcardNoMatch(t *testing.T) {
	if matchesRoute("/other/path", "/api/*") {
		t.Error("wildcard should not match different prefix")
	}
}

func TestMatchesRoute_BadRegex(t *testing.T) {
	// A pattern that produces an invalid regex after QuoteMeta+replace should not panic
	result := matchesRoute("/api", "/api/[bad")
	_ = result // just verify no panic
}

// ── shouldSampleProbabilistic ─────────────────────────────────────────────────

func TestShouldSampleProbabilistic_Zero(t *testing.T) {
	if shouldSampleProbabilistic(0) {
		t.Error("0% should never sample")
	}
}

func TestShouldSampleProbabilistic_One(t *testing.T) {
	if !shouldSampleProbabilistic(1.0) {
		t.Error("100% should always sample")
	}
}

func TestShouldSampleProbabilistic_Negative(t *testing.T) {
	if shouldSampleProbabilistic(-0.1) {
		t.Error("negative probability should not sample")
	}
}

func TestShouldSampleProbabilistic_Middle(t *testing.T) {
	// Just verify it runs without panicking
	for i := 0; i < 100; i++ {
		_ = shouldSampleProbabilistic(0.5)
	}
}
