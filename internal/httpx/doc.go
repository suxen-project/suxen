// Package httpx contains shared HTTP response helpers used by the control plane,
// OCI distribution API, and identity endpoints. Those packages cannot import
// internal/server, so JSON, problem+json, OCI error envelopes, and pagination
// live here.
package httpx
