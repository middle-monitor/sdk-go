# Middle-Monitor SDK for Go

Drop-in SDK for error reporting with Middle-Monitor. Auto-configured via environment variables, with automatic error and panic capture.

**Documentation:** [middlemonitor.io/docs#sdk](https://middlemonitor.io/docs#sdk)

## Installation

```bash
go get github.com/middle-monitor/sdk-go
```

## Automatic configuration (recommended)

Set the environment variables and the SDK initializes itself:

```bash
export MIDDLE_MONITOR_API_URL="https://api.middlemonitor.io"
export MIDDLE_MONITOR_TOKEN="your_token"
export MIDDLE_MONITOR_SERVICE="my-service"
```

## Usage with Echo (recommended)

```go
package main

import (
    "github.com/labstack/echo/v4"
    "github.com/middle-monitor/sdk-go"
)

func main() {
    e := echo.New()

    // One line to enable automatic capture
    e.Use(middlemonitor.EchoMiddleware())

    e.GET("/", func(c echo.Context) error {
        return fmt.Errorf("an error") // Captured automatically
    })

    e.Logger.Fatal(e.Start(":8080"))
}
```

## Usage with Gin

```go
package main

import (
    "github.com/gin-gonic/gin"
    "github.com/middle-monitor/sdk-go"
)

func main() {
    r := gin.Default()

    // One line to enable automatic capture
    r.Use(middlemonitor.GinMiddleware())

    r.GET("/", func(c *gin.Context) {
        panic("a panic") // Captured automatically
    })

    r.Run(":8080")
}
```

## Usage with net/http (ServeMux, gorilla/mux, chi, ...)

```go
package main

import (
    "net/http"

    "github.com/middle-monitor/sdk-go"
)

func main() {
    mux := http.NewServeMux()

    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        panic("a panic") // Captured automatically
    })

    // One line to enable automatic capture
    http.ListenAndServe(":8080", middlemonitor.HTTPMiddleware(mux))
}
```

Works with any router built on `http.Handler`, e.g. gorilla/mux: `r.Use(middlemonitor.HTTPMiddleware)`.

## Panic capture at startup

```go
package main

import (
    "github.com/middle-monitor/sdk-go"
)

func main() {
    defer middlemonitor.CapturePanicGlobal()

    panic("a startup panic") // Captured automatically
}
```

## Manual setup (optional)

```go
package main

import (
    "github.com/middle-monitor/sdk-go"
)

func main() {
    middlemonitor.InitWithConfig(
        "https://api.middlemonitor.io",
        "my-service",
        "your_token",
    )

    err := doSomething()
    if err != nil {
        middlemonitor.ReportError(err)
    }
}
```

## Logging

Send structured logs to Middle-Monitor:

```go
// Buffered (async, batched)
middlemonitor.Log(ctx, middlemonitor.LogLevelINFO, "Message", map[string]string{
    "key": "value",
})

// Immediate (sync)
middlemonitor.LogSync(ctx, middlemonitor.LogLevelERROR, "Critical error", nil)

// Flush before shutdown
middlemonitor.FlushLogs(ctx)
```

Levels: `LogLevelDEBUG`, `LogLevelINFO`, `LogLevelWARN`, `LogLevelERROR`, `LogLevelFATAL`, `LogLevelPANIC`.

## Global functions

- `middlemonitor.ReportError(err)` — report an error
- `middlemonitor.ReportErrorWithDetails(err, file, line)` — report with file/line details
- `middlemonitor.CapturePanicGlobal()` — capture a panic (use with defer)
- `middlemonitor.Log(ctx, level, message, attrs)` — send a buffered log
- `middlemonitor.LogSync(ctx, level, message, attrs)` — send a log immediately
- `middlemonitor.FlushLogs(ctx)` — flush the log buffer before shutdown

## Environment variables

| Variable | Description | Required | Default |
|---|---|---|---|
| `MIDDLE_MONITOR_API_URL` | Middle-Monitor ingestion URL | Recommended | `https://api.middlemonitor.io` |
| `MIDDLE_MONITOR_TOKEN` | Authentication token | Recommended | — |
| `MIDDLE_MONITOR_SERVICE` | Service name | No | `"unknown"` |
| `MIDDLE_MONITOR_DISABLE_HTTP_ERROR_REPORTING` | Stop the middlewares from reporting 5xx | No | `false` |
| `MIDDLE_MONITOR_PPROF_URL` | Scrape an external pprof server instead of profiling in-process | No | — |

`MIDDLE_MONITOR_TOKEN` also acts as the opt-in switch: with no token set, the SDK does not initialize itself and every entry point is a no-op, so an application that never configured Middle-Monitor never sends anything.

Only **server errors (5xx)** and **panics** are reported. **4xx** responses (401, 404, etc.) are treated as client errors and are not sent.

### Applications that report their own errors

The HTTP middlewares submit every 5xx to the Errors view, building the message from the response body. If your application already reports its errors from its own error helper, you get two entries per failure — one with the real cause, one generic. Disable the middleware's half:

```go
cfg := middlemonitor.NewConfig(apiURL, service, token)
cfg.DisableHTTPErrorReporting = true
middlemonitor.Init(cfg)
```

Panics are still reported.

## Profiling

`CaptureCPUProfile` and `CaptureHeapProfile` profile the running process directly — no pprof HTTP server to start and secure:

```go
client := middlemonitor.GetGlobalClient()
client.CaptureCPUProfile(ctx, 30*time.Second)
client.CaptureHeapProfile(ctx)
```

Set `MIDDLE_MONITOR_PPROF_URL` (or `Config.PprofURL`) only to profile a *different* process through its pprof endpoint.

## Troubleshooting

### "failed to upload metrics" / "connection refused"

The SDK sends traces, logs, and metrics to the ingestion endpoint (OTLP: `/v1/traces`, `/v1/logs`, `/v1/metrics`). A connection error means the SDK can't reach it. Check that `MIDDLE_MONITOR_API_URL` points at your ingestion endpoint and that outbound HTTPS is allowed:

```bash
export MIDDLE_MONITOR_API_URL="https://api.middlemonitor.io"
```
