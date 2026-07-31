package middlemonitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// newLogTestConfig returns a Config pointing at the given test server.
func newLogTestConfig(serverURL string) *Config {
	cfg := NewConfig(serverURL, "test-svc", "tok")
	return cfg
}

// ── buildLogRecord ────────────────────────────────────────────────────────────

func TestBuildLogRecord_AllLevels(t *testing.T) {
	cfg := newTestConfig()
	levels := []LogLevel{
		LogLevelDEBUG, LogLevelINFO, LogLevelWARN,
		LogLevelERROR, LogLevelFATAL, LogLevelPANIC,
	}
	for _, lvl := range levels {
		rec := buildLogRecord(context.Background(), lvl, "test message", map[string]string{"k": "v"}, cfg)
		if rec == nil {
			t.Errorf("level %s: got nil record", lvl)
		}
		if rec.SeverityText != string(lvl) {
			t.Errorf("level %s: want SeverityText=%s, got %s", lvl, lvl, rec.SeverityText)
		}
		if rec.Body == nil {
			t.Errorf("level %s: body should not be nil", lvl)
		}
	}
}

func TestBuildLogRecord_NoAttrs(t *testing.T) {
	cfg := newTestConfig()
	rec := buildLogRecord(context.Background(), LogLevelINFO, "msg", nil, cfg)
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
	if len(rec.Attributes) != 0 {
		t.Errorf("want 0 attrs, got %d", len(rec.Attributes))
	}
}

// ── logLevelToSeverity ────────────────────────────────────────────────────────

func TestLogLevelToSeverity_AllLevels(t *testing.T) {
	cases := []struct {
		level    LogLevel
		wantNot  logspb.SeverityNumber
		wantZero bool
	}{
		{LogLevelDEBUG, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelINFO, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelWARN, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelERROR, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelFATAL, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
		{LogLevelPANIC, logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED, false},
	}
	for _, tc := range cases {
		sev := logLevelToSeverity(tc.level)
		if sev == logspb.SeverityNumber_SEVERITY_NUMBER_UNSPECIFIED {
			t.Errorf("level %s: unexpected UNSPECIFIED severity", tc.level)
		}
	}
}

func TestLogLevelToSeverity_Default(t *testing.T) {
	sev := logLevelToSeverity(LogLevel("UNKNOWN"))
	if sev != logspb.SeverityNumber_SEVERITY_NUMBER_INFO {
		t.Errorf("want INFO for unknown level, got %v", sev)
	}
}

// ── sendLogs ─────────────────────────────────────────────────────────────────

func TestSendLogs_Empty(t *testing.T) {
	cfg := newTestConfig()
	err := sendLogs(context.Background(), nil, cfg)
	if err != nil {
		t.Errorf("empty records should be no-op, got: %v", err)
	}
}

func TestSendLogs_Success(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "test error", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Error("test server did not receive the request")
	}
}

func TestSendLogs_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)

	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for non-200 status")
	}
}

func TestSendLogs_BadURL(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "not-a-host:99999"
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for unreachable host")
	}
}

// ── flushLogs ─────────────────────────────────────────────────────────────────

func TestFlushLogs_NilClient(t *testing.T) {
	cfg := newTestConfig()
	// Should not panic
	flushLogs(context.Background(), nil, cfg)
}

func TestFlushLogs_NilConfig(t *testing.T) {
	// Should not panic
	flushLogs(context.Background(), &Client{}, nil)
}

func TestFlushLogs_EmptyBuffer(t *testing.T) {
	cfg := newTestConfig()
	client := &Client{}
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
	// Should not panic and should be a no-op
	flushLogs(context.Background(), client, cfg)
}

func TestFlushLogs_WithRecords_ServerDown(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "localhost:19999" // nothing listening
	cfg.Insecure = true
	client := &Client{}

	logBufferMu.Lock()
	logBuffer = []*logspb.LogRecord{
		buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg),
	}
	logBufferMu.Unlock()

	// Should not panic — records are re-added on failure
	flushLogs(context.Background(), client, cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
}

// ── startLogFlusher ───────────────────────────────────────────────────────────

func TestStartLogFlusher_Idempotent(t *testing.T) {
	// Reset the flusherOnce so this test can exercise the code path
	flusherOnce = sync.Once{}
	startLogFlusher()
	// Calling again should be safe (idempotent)
	startLogFlusher()
}

// ── Log / LogSync / FlushLogs (global API, nil client path) ──────────────────

func TestLog_NoClient(t *testing.T) {
	resetGlobalState()
	// Should not panic when no global client is set
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

func TestLogSync_NoClient(t *testing.T) {
	resetGlobalState()
	// No token: auto-init stays off and GetGlobalClient() returns nil
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrNotInitialized {
		t.Errorf("want ErrNotInitialized, got %v", err)
	}
}

func TestLogSync_NoConfig(t *testing.T) {
	resetGlobalState()
	// No token, then a client with no config so GetGlobalConfig() returns nil
	t.Setenv("MIDDLE_MONITOR_TOKEN", "")
	globalClient = &Client{}
	defer resetGlobalState()
	err := LogSync(context.Background(), LogLevelINFO, "msg", nil)
	if err != ErrConfigMissing {
		t.Errorf("want ErrConfigMissing, got %v", err)
	}
}

func TestFlushLogs_GlobalAPI(t *testing.T) {
	resetGlobalState()
	// Should not panic
	FlushLogs(context.Background())
}

// TestStartLogFlusher_GoroutineBody covers log.go:35-37 — the goroutine body that
// fires on each ticker tick. We shorten the interval so the test is fast.
// Shutdown must end the flusher goroutine. Left running, it keeps ticking for
// the rest of the process lifetime and flushes into providers already torn down
// — a leak in every long-lived process that restarts its client.
func TestShutdown_StopsLogFlusher(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	var flushes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&flushes, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	old := logFlushInterval
	logFlushInterval = 20 * time.Millisecond
	defer func() { logFlushInterval = old }()

	if err := Init(newLogTestConfig(srv.URL)); err != nil {
		t.Fatalf("init: %v", err)
	}
	startLogFlusher()

	// A buffered record gives the ticker something to send.
	Log(context.Background(), LogLevelERROR, "before shutdown", nil)
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt64(&flushes) == 0 {
		t.Fatal("expected the flusher to have sent the buffered log")
	}

	client := GetGlobalClient()
	if client == nil {
		t.Fatal("expected an initialized client")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Buffer another record: a stopped flusher must never pick it up. Appended
	// directly, because Log() restarts the flusher on purpose.
	settled := atomic.LoadInt64(&flushes)
	logBufferMu.Lock()
	logBuffer = append(logBuffer, buildLogRecord(context.Background(), LogLevelERROR, "after shutdown", nil, GetGlobalConfig()))
	logBufferMu.Unlock()

	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt64(&flushes); n != settled {
		t.Errorf("flusher still ticking after shutdown: %d flushes, want %d", n, settled)
	}
}

func TestStartLogFlusher_GoroutineBody(t *testing.T) {
	stopLogFlusher()
	old := logFlushInterval
	logFlushInterval = 20 * time.Millisecond
	defer func() {
		stopLogFlusher()
		logFlushInterval = old
	}()
	startLogFlusher()
	time.Sleep(80 * time.Millisecond)
}

// TestLog_NilClient_Blocked covers log.go:46-48 — early return when client is nil.
func TestLog_NilClient_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "") // auto-init stays off, client stays nil
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_NilConfig_Blocked covers log.go:50-52 — early return when config is nil.
func TestLog_NilConfig_Blocked(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	t.Setenv("MIDDLE_MONITOR_TOKEN", "") // auto-init stays off
	globalClient = &Client{}
	Log(context.Background(), LogLevelINFO, "msg", nil)
}

// TestLog_ShouldFlush covers log.go:64-66 — the flush goroutine trigger when the
// buffer reaches logBufferSize entries.
func TestLog_ShouldFlush(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	Init(cfg)

	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()

	for i := 0; i < logBufferSize+1; i++ {
		Log(context.Background(), LogLevelINFO, "fill", nil)
	}
	time.Sleep(100 * time.Millisecond)
}

// TestLogSync_Success covers log.go:79-80 — the happy path of LogSync when both
// client and config are initialized.
func TestLogSync_Success(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	if err := Init(cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}

	err := LogSync(context.Background(), LogLevelINFO, "hello", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSendLogs_BadURL_InvalidHost covers log.go:193-195 — http.NewRequestWithContext
// error when the endpoint produces an invalid URL.
func TestSendLogs_BadURL_InvalidHost(t *testing.T) {
	cfg := newTestConfig()
	cfg.Endpoint = "[invalid"
	cfg.Insecure = true
	rec := buildLogRecord(context.Background(), LogLevelERROR, "err", nil, cfg)
	err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg)
	if err == nil {
		t.Error("expected error for invalid endpoint URL")
	}
}

// A log carrying its span lets the backend join it to the trace timeline; without
// the ids the record is orphaned in OpenSearch, which is what makes correlated
// incident debugging work at all.
func TestBuildLogRecord_CarriesActiveSpan(t *testing.T) {
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}

	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))

	rec := buildLogRecord(ctx, LogLevelERROR, "boom", nil, newTestConfig())

	if string(rec.TraceId) != string(traceID[:]) {
		t.Errorf("trace id not propagated: got %x, want %x", rec.TraceId, traceID[:])
	}
	if string(rec.SpanId) != string(spanID[:]) {
		t.Errorf("span id not propagated: got %x, want %x", rec.SpanId, spanID[:])
	}
}

// Logs emitted outside any span must stay unlinked rather than carry zeroed ids,
// which the backend would index as a real trace and never match.
func TestBuildLogRecord_NoSpanLeavesIdsEmpty(t *testing.T) {
	rec := buildLogRecord(context.Background(), LogLevelINFO, "no span", nil, newTestConfig())

	if len(rec.TraceId) != 0 {
		t.Errorf("expected no trace id, got %x", rec.TraceId)
	}
	if len(rec.SpanId) != 0 {
		t.Errorf("expected no span id, got %x", rec.SpanId)
	}
}

// ── logHTTPRequest ────────────────────────────────────────────────────────────

// bufferedRecords returns a snapshot of the pending log buffer.
func bufferedRecords() []*logspb.LogRecord {
	logBufferMu.Lock()
	defer logBufferMu.Unlock()
	out := make([]*logspb.LogRecord, len(logBuffer))
	copy(out, logBuffer)
	return out
}

// initForRequestLogs points the SDK at a server that swallows exports, so tests
// can assert on what was buffered rather than on the wire.
func initForRequestLogs(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := NewConfig(srv.URL, "svc", "tok")
	cfg.Insecure = true
	if err := Init(cfg); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	logBufferMu.Lock()
	logBuffer = nil
	logBufferMu.Unlock()
}

func recordAttr(rec *logspb.LogRecord, key string) string {
	for _, kv := range rec.Attributes {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

// A 5xx must reach the Logs view carrying the cause, so an operator reading the
// log line knows what failed without opening the trace or the Errors view.
func TestLogHTTPRequest_5xxCarriesCauseAndAttributes(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "POST", "/api/orders", 500, 120*time.Millisecond, true, "pq: duplicate key", "")

	recs := bufferedRecords()
	if len(recs) != 1 {
		t.Fatalf("want 1 buffered record, got %d", len(recs))
	}
	rec := recs[0]
	if got := rec.Body.GetStringValue(); got != "POST /api/orders 500: pq: duplicate key" {
		t.Errorf("unexpected body: %q", got)
	}
	if rec.SeverityText != string(LogLevelERROR) {
		t.Errorf("want ERROR severity, got %q", rec.SeverityText)
	}
	if got := recordAttr(rec, "http.status_code"); got != "500" {
		t.Errorf("want status attribute 500, got %q", got)
	}
	if got := recordAttr(rec, "http.route"); got != "/api/orders" {
		t.Errorf("want route attribute, got %q", got)
	}
	if got := recordAttr(rec, "duration_ms"); got != "120" {
		t.Errorf("want duration_ms 120, got %q", got)
	}
}

// Successful traffic must stay out of the Logs view by default: request volume
// is what traces are for, and logging every 2xx would drown the failures the
// correlation is looking for.
func TestLogHTTPRequest_2xxNotCapturedByDefault(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/api/orders", 200, time.Millisecond, false, "", "")

	if recs := bufferedRecords(); len(recs) != 0 {
		t.Fatalf("2xx must not be logged by default, got %d record(s)", len(recs))
	}
}

// A 4xx is a failed request too: it is the signal behind an auth or quota storm,
// so it is captured — as WARN, since the caller is at fault, not the service.
func TestLogHTTPRequest_4xxCapturedAsWarn(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/api/orders", 429, time.Millisecond, true, "", "")

	recs := bufferedRecords()
	if len(recs) != 1 {
		t.Fatalf("want 1 buffered record, got %d", len(recs))
	}
	if recs[0].SeverityText != string(LogLevelWARN) {
		t.Errorf("want WARN severity, got %q", recs[0].SeverityText)
	}
}

// Health probes run every few seconds on every service; capturing them would
// dominate the Logs view and shift the baseline the correlation compares against.
func TestLogHTTPRequest_NeverCaptureRouteStaysOut(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/health", 200, time.Millisecond, false, "", "")

	if recs := bufferedRecords(); len(recs) != 0 {
		t.Fatalf("/health must not be logged, got %d record(s)", len(recs))
	}
}

// The "HTTP 500" fallback carries no information; appending it would repeat the
// status already in the line instead of naming a cause.
func TestLogHTTPRequest_GenericCauseNotAppended(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/api/x", 500, time.Millisecond, true, "HTTP 500", "")

	recs := bufferedRecords()
	if len(recs) != 1 {
		t.Fatalf("want 1 buffered record, got %d", len(recs))
	}
	if got := recs[0].Body.GetStringValue(); got != "GET /api/x 500" {
		t.Errorf("generic cause should be dropped, got %q", got)
	}
}

// The address is what turns a wall of 404s into "one host is scanning us", so it
// has to travel on the log line itself, next to the route that was probed.
func TestLogHTTPRequest_CarriesClientIP(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/wp-login.php", 404, time.Millisecond, true, "", "203.0.113.0")

	recs := bufferedRecords()
	if len(recs) != 1 {
		t.Fatalf("want 1 buffered record, got %d", len(recs))
	}
	if got := recordAttr(recs[0], "client.ip"); got != "203.0.113.0" {
		t.Errorf("want client.ip attribute, got %q", got)
	}
}

// With collection off the attribute must be absent, not present and empty:
// an empty value still says a request came from somewhere we chose not to record.
func TestLogHTTPRequest_NoClientIPAttributeWhenEmpty(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	initForRequestLogs(t)

	logHTTPRequest(context.Background(), GetGlobalConfig(), "GET", "/api/x", 500, time.Millisecond, true, "boom", "")

	recs := bufferedRecords()
	if len(recs) != 1 {
		t.Fatalf("want 1 buffered record, got %d", len(recs))
	}
	for _, kv := range recs[0].Attributes {
		if kv.Key == "client.ip" {
			t.Errorf("client.ip must be absent, got %q", kv.Value.GetStringValue())
		}
	}
}

func TestLogHTTPRequest_NilConfig(t *testing.T) {
	resetGlobalState()
	defer resetGlobalState()
	// Must not panic nor buffer anything
	logHTTPRequest(context.Background(), nil, "GET", "/api/x", 500, time.Millisecond, true, "boom", "")
	if recs := bufferedRecords(); len(recs) != 0 {
		t.Fatalf("want no record, got %d", len(recs))
	}
}

func TestHTTPStatusToLevel(t *testing.T) {
	cases := []struct {
		status int
		want   LogLevel
	}{
		{200, LogLevelINFO},
		{301, LogLevelINFO},
		{404, LogLevelWARN},
		{499, LogLevelWARN},
		{500, LogLevelERROR},
		{503, LogLevelERROR},
	}
	for _, c := range cases {
		if got := httpStatusToLevel(c.status); got != c.want {
			t.Errorf("status %d: want %s, got %s", c.status, c.want, got)
		}
	}
}

// ── Host label on the log resource ────────────────────────────────────────────

// The host label is the join key of the whole correlation story: host CPU or
// memory on one side, the traffic of the services running on that host on the
// other. Dropped from the resource, the backend indexes an empty hostname and
// the two sides can never be lined up.
func TestSendLogs_ResourceCarriesHostName(t *testing.T) {
	received := make(chan *collectorlogs.ExportLogsServiceRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req collectorlogs.ExportLogsServiceRequest
		if proto.Unmarshal(body, &req) == nil {
			select {
			case received <- &req:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	cfg.Hostname = "host4"
	rec := buildLogRecord(context.Background(), LogLevelERROR, "boom", nil, cfg)

	if err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case req := <-received:
		got := ""
		for _, kv := range req.ResourceLogs[0].Resource.Attributes {
			if kv.Key == "host.name" {
				got = kv.Value.GetStringValue()
			}
		}
		if got != "host4" {
			t.Errorf("want host.name host4, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test server did not receive the export")
	}
}

// An empty host label would be indexed as a real (empty) hostname and match no
// host, so it must be left out rather than sent blank.
func TestSendLogs_OmitsUnresolvedHostName(t *testing.T) {
	received := make(chan *collectorlogs.ExportLogsServiceRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req collectorlogs.ExportLogsServiceRequest
		if proto.Unmarshal(body, &req) == nil {
			select {
			case received <- &req:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newLogTestConfig(srv.URL)
	cfg.Insecure = true
	cfg.Hostname = ""
	rec := buildLogRecord(context.Background(), LogLevelERROR, "boom", nil, cfg)

	if err := sendLogs(context.Background(), []*logspb.LogRecord{rec}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case req := <-received:
		for _, kv := range req.ResourceLogs[0].Resource.Attributes {
			if kv.Key == "host.name" {
				t.Errorf("host.name should be absent, got %q", kv.Value.GetStringValue())
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test server did not receive the export")
	}
}
