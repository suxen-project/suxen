package httpx

import (
	"log/slog"
	"net/http"
)

const (
	// ControlPlaneMetricFormat is the Prometheus format label for non-repository
	// routes such as /api/v1 and probes.
	ControlPlaneMetricFormat = "control"
	// NoRepositoryMetricLabel is the Prometheus repository label when a request
	// is not scoped to a repository.
	NoRepositoryMetricLabel = "none"
)

// ErrorResponseFormat selects problem+json versus the OCI distribution envelope.
type ErrorResponseFormat uint8

const (
	// ErrorResponseFormatProblem writes RFC 7807 problem+json.
	ErrorResponseFormatProblem ErrorResponseFormat = iota
	// ErrorResponseFormatOCI writes the Docker/OCI error envelope.
	ErrorResponseFormatOCI
)

// ErrorResponseWriter is implemented by the compositor's status-tracking writer
// so shared helpers can switch envelope format and report internal errors.
type ErrorResponseWriter interface {
	SetErrorResponseFormat(ErrorResponseFormat)
	CurrentErrorResponseFormat() ErrorResponseFormat
	ReportInternalError(error)
}

// MetricLabelWriter records Prometheus/access-log repository labels.
type MetricLabelWriter interface {
	SetMetricLabels(repository string, format string)
	SetLogRepository(repository string)
}

// StatusWriter tracks status, request correlation, and metric labels for one
// request. The compositor wraps inbound writers with this type.
type StatusWriter struct {
	http.ResponseWriter
	status              int
	wroteHeader         bool
	errorResponseFormat ErrorResponseFormat
	Log                 *slog.Logger
	request             *RequestLog
	metricRepository    string
	metricFormat        string
}

// NewStatusWriter tracks w for one request.
func NewStatusWriter(w http.ResponseWriter, log *slog.Logger, request *RequestLog) *StatusWriter {
	return &StatusWriter{
		ResponseWriter:   w,
		status:           http.StatusOK,
		Log:              log,
		request:          request,
		metricRepository: NoRepositoryMetricLabel,
		metricFormat:     ControlPlaneMetricFormat,
	}
}

// Status is the HTTP status written to the client, defaulting to 200.
func (w *StatusWriter) Status() int {
	return w.status
}

// MetricRepository is the Prometheus repository label for this request.
func (w *StatusWriter) MetricRepository() string {
	return w.metricRepository
}

// MetricFormat is the Prometheus format label for this request.
func (w *StatusWriter) MetricFormat() string {
	return w.metricFormat
}

// WriteHeader records final response statuses while forwarding interim responses.
func (w *StatusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

// Write records the implicit 200 that net/http sends before the first body write.
func (w *StatusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// FlushError keeps status tracking intact when ResponseController flushes a
// response before its first body write. Unsupported flushes do not commit it.
func (w *StatusWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return err
}

// SetErrorResponseFormat implements ErrorResponseWriter.
func (w *StatusWriter) SetErrorResponseFormat(format ErrorResponseFormat) {
	w.errorResponseFormat = format
}

// CurrentErrorResponseFormat implements ErrorResponseWriter.
func (w *StatusWriter) CurrentErrorResponseFormat() ErrorResponseFormat {
	return w.errorResponseFormat
}

// ReportInternalError implements ErrorResponseWriter.
func (w *StatusWriter) ReportInternalError(err error) {
	if w.Log == nil {
		return
	}
	attributes := append(w.RequestLogAttributes(), "error", err)
	w.Log.Error("request failed", attributes...)
}

// SetMetricLabels implements MetricLabelWriter.
func (w *StatusWriter) SetMetricLabels(repository string, format string) {
	w.metricRepository = repository
	w.metricFormat = format
	if w.request != nil {
		w.request.Repository = repository
		w.request.Format = format
	}
}

// SetLogRepository implements MetricLabelWriter.
func (w *StatusWriter) SetLogRepository(repository string) {
	if w.request != nil {
		w.request.Repository = repository
	}
}

// RequestLogAttributes is the shared access-log field list.
func (w *StatusWriter) RequestLogAttributes() []any {
	requestID := ""
	subject := "anonymous"
	if w.request != nil {
		requestID = w.request.RequestID
		subject = w.request.Subject
	}
	repository := ""
	format := ControlPlaneMetricFormat
	if w.request != nil {
		repository = w.request.Repository
		format = w.request.Format
	}
	return []any{
		"request_id", requestID,
		"subject", subject,
		"repository", repository,
		"format", format,
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *StatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// SelectErrorResponseFormat switches the envelope used by WriteProblem.
func SelectErrorResponseFormat(w http.ResponseWriter, format ErrorResponseFormat) {
	responseWriter, ok := w.(ErrorResponseWriter)
	if !ok {
		return
	}
	responseWriter.SetErrorResponseFormat(format)
}

// SetRequestMetricLabels records repository metric labels when w supports them.
func SetRequestMetricLabels(w http.ResponseWriter, repository string, format string) {
	tracked, ok := w.(MetricLabelWriter)
	if !ok {
		return
	}
	tracked.SetMetricLabels(repository, format)
}

// SetRequestLogRepository records the repository name for access logs.
func SetRequestLogRepository(w http.ResponseWriter, repository string) {
	tracked, ok := w.(MetricLabelWriter)
	if !ok {
		return
	}
	tracked.SetLogRepository(repository)
}

func currentErrorResponseFormat(w http.ResponseWriter) ErrorResponseFormat {
	responseWriter, ok := w.(ErrorResponseWriter)
	if !ok {
		return ErrorResponseFormatProblem
	}
	return responseWriter.CurrentErrorResponseFormat()
}

func reportInternalError(w http.ResponseWriter, err error) {
	responseWriter, ok := w.(ErrorResponseWriter)
	if !ok {
		return
	}
	responseWriter.ReportInternalError(err)
}
