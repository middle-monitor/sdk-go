package middlemonitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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

	// ConfigFromEnv always defaults to api.middlemonitor.io when no env vars are set,
	// so Init(nil) succeeds (client may fail to export later, but init succeeds).
	_ = Init(nil)
	// Verify that a config was set (endpoint defaults to api.middlemonitor.io)
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

	// ConfigFromEnv defaults to api.middlemonitor.io when no env vars are set → always succeeds
	err := InitSimple()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// An application that never opted in must not start exporting: without a token
// there is nothing to authenticate against, so auto-init would silently ship
// data to the default public endpoint on the first middleware call.
func TestGetGlobalClient_NoToken_StaysNil(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")

	if client := GetGlobalClient(); client != nil {
		t.Error("expected nil client when MIDDLE_MONITOR_TOKEN is unset")
	}
}

func TestGetGlobalConfig_NoToken_StaysNil(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")

	if cfg := GetGlobalConfig(); cfg != nil {
		t.Error("expected nil config when MIDDLE_MONITOR_TOKEN is unset")
	}
}

// A configured application still gets the convenience of auto-init.
func TestGetGlobalClient_WithToken_AutoInits(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "tok")
	t.Setenv("MIDDLE_MONITOR_API_URL", "http://127.0.0.1:1")

	if client := GetGlobalClient(); client == nil {
		t.Error("expected auto-init to build a client when a token is set")
	}
	if cfg := GetGlobalConfig(); cfg == nil {
		t.Error("expected auto-init to expose the config when a token is set")
	}
}

// A failed init must not disable the SDK for the process lifetime: sync.Once
// used to swallow every later attempt and return nil to the caller.
func TestInit_FailureIsRetryable(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	t.Setenv("MIDDLE_MONITOR_TOKEN", "tok")
	t.Setenv("MIDDLE_MONITOR_API_URL", "http://127.0.0.1:1")
	t.Setenv("MIDDLE_MONITOR_TRACES_SAMPLING", "not-a-number")

	if err := Init(nil); err == nil {
		t.Fatal("expected init to fail on an unparseable sampling value")
	}

	t.Setenv("MIDDLE_MONITOR_TRACES_SAMPLING", "0.5")
	if err := Init(nil); err != nil {
		t.Errorf("retry after a failed init should succeed, got %v", err)
	}
	if GetGlobalClient() == nil {
		t.Error("expected a client after the successful retry")
	}
}

// TestInit_RegistersW3CPropagator: without a registered global propagator,
// middleware Extract calls are no-ops and distributed traces never link.
func TestInit_RegistersW3CPropagator(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	otlpSrv := startOTLPServer(t)
	defer otlpSrv.Close()
	if err := Init(NewConfig(otlpSrv.URL, "svc", "tok")); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	header := http.Header{}
	header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(header))
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("W3C trace context not extracted, got %v", sc)
	}
}
