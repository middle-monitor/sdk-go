package middlemonitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	globalMu     sync.RWMutex
	globalClient *Client
	globalConfig *Config
)

// globals reads the initialized client and config under the shared lock.
func globals() (*Client, *Config) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalClient, globalConfig
}

// loggingTraceExporter wraps a SpanExporter to log export success/failure (visible in app logs).
type loggingTraceExporter struct {
	exporter sdktrace.SpanExporter
	endpoint string
}

func (e *loggingTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.exporter.ExportSpans(ctx, spans)
	if err != nil {
		slog.Error("failed to export traces", "endpoint", e.endpoint, "error", err)
		return err
	}
	if len(spans) > 0 {
		slog.Debug("exported traces", "count", len(spans), "endpoint", e.endpoint)
	}
	return nil
}

func (e *loggingTraceExporter) Shutdown(ctx context.Context) error {
	return e.exporter.Shutdown(ctx)
}

// Client represents a Middle-Monitor OpenTelemetry client
type Client struct {
	config        *Config
	tracer        trace.Tracer
	meter         metric.Meter
	tp            *sdktrace.TracerProvider
	meterProvider *sdkmetric.MeterProvider
}

// NewClientWithConfig creates a new OpenTelemetry-based client from Config
func NewClientWithConfig(cfg *Config) (*Client, error) {
	return newClient(cfg)
}

// newClient creates a new OpenTelemetry-based client (internal function)
func newClient(cfg *Config) (*Client, error) {
	// Create resource with service information
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.Service),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", ErrResourceCreate)
	}

	// Initialize trace exporter
	var traceExporter sdktrace.SpanExporter
	ctx := context.Background()
	// OTLP expects "host:port"; normalize in case config has "http://host:port"
	hostPort := normalizeOTLPEndpoint(cfg.Endpoint)
	traceOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(hostPort),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": fmt.Sprintf("Bearer %s", cfg.Token),
		}),
	}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
	}
	rawTraceExporter, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", ErrTraceExport)
	}
	traceExporter = &loggingTraceExporter{exporter: rawTraceExporter, endpoint: hostPort}

	batchOpts := []sdktrace.BatchSpanProcessorOption{}

	// Create tracer provider with sampling
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter, batchOpts...),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(NewSampler(cfg)),
	)

	otel.SetTracerProvider(tp)
	// Without a registered propagator, otel.GetTextMapPropagator() is a no-op and
	// the middlewares never link distributed traces across services.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	tracer := tp.Tracer("middle-monitor")

	// Initialize metric exporter
	var meterProvider *sdkmetric.MeterProvider
	if cfg.Protocol == "http" {
		metricOpts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(hostPort),
			otlpmetrichttp.WithHeaders(map[string]string{
				"Authorization": fmt.Sprintf("Bearer %s", cfg.Token),
			}),
		}
		if cfg.Insecure {
			metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
		}
		metricExporter, err := otlpmetrichttp.New(ctx, metricOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create metric exporter: %w", ErrMetricExport)
		}

		// Create metric reader
		reader := sdkmetric.NewPeriodicReader(metricExporter)

		// Create meter provider
		meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
		otel.SetMeterProvider(meterProvider)
	}

	meter := otel.Meter("middle-monitor")

	client := &Client{
		config:        cfg,
		tracer:        tracer,
		meter:         meter,
		tp:            tp,
		meterProvider: meterProvider,
	}

	return client, nil
}

// Init initializes the global Middle-Monitor client with OpenTelemetry. It is
// safe to call more than once: the first successful client wins. A FAILED
// attempt is not remembered, so a caller can retry once the configuration is
// fixed — with sync.Once, one early failure used to disable the SDK for the
// whole process lifetime while returning nil to every later caller.
func Init(cfg *Config) error {
	globalMu.Lock()
	defer globalMu.Unlock()

	if globalClient != nil {
		return nil
	}

	if cfg == nil {
		var err error
		cfg, err = ConfigFromEnv()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", ErrConfigLoad)
		}
	}

	client, err := NewClientWithConfig(cfg)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", ErrClientCreate)
	}

	globalClient = client
	globalConfig = cfg

	slog.Info("middlemonitor initialized", "service", cfg.Service, "endpoint", cfg.Endpoint)
	return nil
}

// InitWithConfig initializes with explicit configuration (backward compatibility)
func InitWithConfig(apiURL, service, token string) error {
	cfg := NewConfig(apiURL, service, token)
	return Init(cfg)
}

// InitSimple initializes from environment variables
func InitSimple() error {
	return Init(nil)
}

// autoInit initializes from the environment, but only when a token is set.
// Without a token there is nothing to authenticate an export with, and booting
// anyway would silently point the exporter at the default public endpoint from
// an application that never opted in — every entry point below would then start
// sending data off-host on its first call.
func autoInit() {
	if os.Getenv("MIDDLE_MONITOR_TOKEN") == "" {
		return
	}
	_ = InitSimple()
}

// GetGlobalClient returns the global client, or nil when the SDK is not
// configured. Callers must handle nil: it is the normal state of an
// application that has not opted into Middle-Monitor.
func GetGlobalClient() *Client {
	c, _ := globals()
	if c == nil {
		autoInit()
		c, _ = globals()
	}
	return c
}

// GetGlobalConfig returns the global configuration, or nil when the SDK is not
// configured.
func GetGlobalConfig() *Config {
	_, cfg := globals()
	if cfg == nil {
		autoInit()
		_, cfg = globals()
	}
	return cfg
}

// GetTracer returns the tracer from the client
func (c *Client) GetTracer() trace.Tracer {
	return c.tracer
}

// GetMeter returns the meter from the client
func (c *Client) GetMeter() metric.Meter {
	return c.meter
}

// Shutdown gracefully shuts down the client
func (c *Client) Shutdown(ctx context.Context) error {
	var errs []error

	// Stop the periodic flusher first, then drain what is left, so no tick can
	// fire against the providers being shut down below.
	stopLogFlusher()
	FlushLogs(ctx)

	if c.tp != nil {
		if err := c.tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to shutdown tracer provider: %w", ErrTracerShutdown))
		}
	}

	if c.meterProvider != nil {
		if err := c.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to shutdown meter provider: %w", ErrMeterShutdown))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %w (%v)", ErrShutdown, errs)
	}

	return nil
}

// Sampler implements OpenTelemetry sampling with custom rules
type Sampler struct {
	config *Config
}

// NewSampler creates a new sampler with the given configuration
func NewSampler(cfg *Config) sdktrace.Sampler {
	return &Sampler{config: cfg}
}

// ShouldSample implements sdktrace.Sampler
func (s *Sampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	// Extract route from span name or attributes
	route := params.Name

	// Check for error in span attributes
	hasError := false
	if params.Attributes != nil {
		for _, attr := range params.Attributes {
			if attr.Key == "error" {
				if attr.Value.AsBool() {
					hasError = true
					break
				}
			}
			if attr.Key == "http.status_code" {
				if code := attr.Value.AsInt64(); code >= 400 {
					hasError = true
					break
				}
			}
		}
	}

	// Use configuration to decide
	if s.config.ShouldSampleTrace(route, hasError) {
		return sdktrace.SamplingResult{
			Decision: sdktrace.RecordAndSample,
		}
	}

	return sdktrace.SamplingResult{
		Decision: sdktrace.Drop,
	}
}

// Description implements sdktrace.Sampler
func (s *Sampler) Description() string {
	return fmt.Sprintf("MiddleMonitorSampler(percentage=%.2f, alwaysErrors=%v)",
		s.config.Sampling.Traces.Percentage,
		s.config.Sampling.Traces.AlwaysSampleErrors)
}
