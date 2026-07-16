package middlemonitor

import (
	"errors"
	"fmt"
)

var (
	ErrNotInitialized = errors.New("client not initialized")
	ErrConfigMissing  = errors.New("config not initialized")
	ErrConfigRequired = errors.New("endpoint and token required")

	// client.go
	ErrResourceCreate = errors.New("failed to create resource")
	ErrTraceExport    = errors.New("failed to create trace exporter")
	ErrMetricExport   = errors.New("failed to create metric exporter")
	ErrConfigLoad     = errors.New("failed to load config")
	ErrClientCreate   = errors.New("failed to create client")
	ErrTracerShutdown = errors.New("failed to shutdown tracer provider")
	ErrMeterShutdown  = errors.New("failed to shutdown meter provider")
	ErrShutdown       = errors.New("shutdown failed")

	// log.go
	ErrMarshal       = errors.New("marshal failed")
	ErrRequestCreate = errors.New("failed to create request")
	ErrLogSend       = errors.New("failed to send logs")
	ErrLogExport     = errors.New("log export failed")

	// profile.go
	ErrPprofRequest   = errors.New("failed to create pprof request")
	ErrProfileFetch   = errors.New("failed to fetch profile")
	ErrProfileRead    = errors.New("failed to read profile")
	ErrProfileEmpty   = errors.New("empty profile data")
	ErrMultipartBuild = errors.New("multipart build failed")
	ErrUploadRequest  = errors.New("failed to create upload request")
	ErrProfileUpload  = errors.New("profile upload failed")

	// config.go
	ErrSamplingConfig    = errors.New("failed to parse sampling config")
	ErrSamplingValue     = errors.New("invalid sampling value")
	ErrSamplingRange     = errors.New("sampling must be between -1 and 1")
	ErrLogLevel          = errors.New("invalid log level")
	ErrMinHTTPStatus     = errors.New("invalid min http status")
	ErrErrorSubmit       = errors.New("failed to submit error to backend")
)

// HTTPStatusError carries a non-OK status code from an HTTP response.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Body)
}
