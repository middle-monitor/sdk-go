package middlemonitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ── Sampler ───────────────────────────────────────────────────────────────────

func TestSampler_Description(t *testing.T) {
	cfg := newTestConfig()
	s := NewSampler(cfg)
	desc := s.Description()
	if desc == "" {
		t.Error("Description should not be empty")
	}
}

func TestSampler_ShouldSample_ErrorAttr(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 0 // Would normally drop
	cfg.Sampling.Traces.AlwaysSampleErrors = true

	s := &Sampler{config: cfg}
	params := sdktrace.SamplingParameters{
		Name: "/api/handler",
		Attributes: []attribute.KeyValue{
			attribute.Bool("error", true),
		},
	}
	result := s.ShouldSample(params)
	if result.Decision != sdktrace.RecordAndSample {
		t.Error("should sample spans with error=true when AlwaysSampleErrors is true")
	}
}

func TestSampler_ShouldSample_HTTPStatusError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 0
	cfg.Sampling.Traces.AlwaysSampleErrors = true

	s := &Sampler{config: cfg}
	params := sdktrace.SamplingParameters{
		Name: "/api/handler",
		Attributes: []attribute.KeyValue{
			attribute.Int64("http.status_code", 500),
		},
	}
	result := s.ShouldSample(params)
	if result.Decision != sdktrace.RecordAndSample {
		t.Error("should sample spans with http.status_code >= 400 when AlwaysSampleErrors is true")
	}
}

func TestSampler_ShouldSample_Drop(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 0
	cfg.Sampling.Traces.AlwaysSampleErrors = false

	s := &Sampler{config: cfg}
	params := sdktrace.SamplingParameters{
		Name: "/api/handler",
	}
	result := s.ShouldSample(params)
	if result.Decision != sdktrace.Drop {
		t.Error("should drop when percentage=0 and no error")
	}
}

func TestSampler_ShouldSample_NilAttributes(t *testing.T) {
	cfg := newTestConfig()
	cfg.Sampling.Traces.Percentage = 100

	s := &Sampler{config: cfg}
	params := sdktrace.SamplingParameters{
		Name:       "/api",
		Attributes: nil,
	}
	result := s.ShouldSample(params)
	if result.Decision != sdktrace.RecordAndSample {
		t.Error("should sample when percentage=100 and no attributes")
	}
}

// ── loggingTraceExporter ─────────────────────────────────────────────────────

type mockSpanExporter struct {
	failExport bool
}

func (m *mockSpanExporter) ExportSpans(_ context.Context, _ []sdktrace.ReadOnlySpan) error {
	if m.failExport {
		return errors.New("export failed")
	}
	return nil
}

func (m *mockSpanExporter) Shutdown(_ context.Context) error {
	return nil
}

func TestLoggingTraceExporter_ExportSpans_Success(t *testing.T) {
	inner := &mockSpanExporter{failExport: false}
	exp := &loggingTraceExporter{exporter: inner, endpoint: "localhost:4318"}
	err := exp.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoggingTraceExporter_ExportSpans_NonEmpty(t *testing.T) {
	inner := &mockSpanExporter{failExport: false}
	exp := &loggingTraceExporter{exporter: inner, endpoint: "localhost:4318"}

	// Create a real span so ExportSpans receives a non-empty slice (covers the log.Printf branch)
	res, _ := resource.New(context.Background())
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(10*time.Millisecond)),
	)
	_, span := tp.Tracer("test").Start(context.Background(), "test-span")
	span.End()
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

func TestLoggingTraceExporter_ExportSpans_Error(t *testing.T) {
	inner := &mockSpanExporter{failExport: true}
	exp := &loggingTraceExporter{exporter: inner, endpoint: "localhost:4318"}
	err := exp.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{})
	if err == nil {
		t.Error("expected error from inner exporter")
	}
}

func TestLoggingTraceExporter_Shutdown(t *testing.T) {
	inner := &mockSpanExporter{}
	exp := &loggingTraceExporter{exporter: inner, endpoint: "localhost:4318"}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Errorf("unexpected shutdown error: %v", err)
	}
}

// ── Client.Shutdown ───────────────────────────────────────────────────────────

func TestShutdown_NilProviders(t *testing.T) {
	c := &Client{tp: nil, meterProvider: nil}
	err := c.Shutdown(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestShutdown_WithProviders(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if err2 := c.Shutdown(context.Background()); err2 != nil {
		t.Errorf("unexpected shutdown error: %v", err2)
	}
}

// ── GetTracer / GetMeter ──────────────────────────────────────────────────────

func TestGetTracer(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	if c.GetTracer() == nil {
		t.Error("GetTracer should not return nil")
	}
}

func TestGetMeter(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	c, err := NewClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewClientWithConfig: %v", err)
	}
	// GetMeter returns the global meter set by otel.SetMeterProvider — not nil
	m := c.GetMeter()
	_ = m
}

// ── Init / InitWithConfig / InitSimple / GetGlobalClient / GetGlobalConfig ────

func TestInit_WithConfig(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	if err := Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if GetGlobalClient() == nil {
		t.Error("expected non-nil global client after Init")
	}
	if GetGlobalConfig() == nil {
		t.Error("expected non-nil global config after Init")
	}
}

func TestInit_Idempotent(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	if err := Init(cfg); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	// Second Init should be a no-op (sync.Once)
	if err := Init(cfg); err != nil {
		t.Fatalf("second Init: %v", err)
	}
}

func TestInit_NilConfig_NoEnv(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	// ConfigFromEnv always defaults to localhost:8080 when no env vars are set,
	// so Init(nil) succeeds (client may fail to export later, but init succeeds).
	_ = Init(nil)
	// Verify that a config was set (endpoint defaults to localhost:8080)
	if GetGlobalConfig() == nil {
		t.Error("expected non-nil global config with default endpoint")
	}
}

func TestInitWithConfig(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := InitWithConfig(srv.URL, "svc", "tok"); err != nil {
		t.Fatalf("InitWithConfig: %v", err)
	}
	if GetGlobalClient() == nil {
		t.Error("expected non-nil global client")
	}
}

func TestInitSimple_NoEnv(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	// ConfigFromEnv defaults to localhost:8080 when no env vars are set → always succeeds
	err := InitSimple()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGetGlobalClient_AutoInit_NoEnv(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	// Auto-init with default endpoint (localhost:8080) → returns non-nil client
	client := GetGlobalClient()
	if client == nil {
		t.Error("expected non-nil client after auto-init with defaults")
	}
}

func TestGetGlobalConfig_AutoInit_NoEnv(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	// Auto-init with default endpoint → returns non-nil config
	cfg := GetGlobalConfig()
	if cfg == nil {
		t.Error("expected non-nil config after auto-init with defaults")
	}
}
