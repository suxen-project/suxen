// Package startup marks dependency failures that can be retried during startup.
package startup

import "errors"

// Retryable wraps a failure caused by an unavailable dependency. Errors without
// this marker stop startup; callers must opt in at the dependency boundary.
type Retryable struct{ Err error }

func (e *Retryable) Error() string { return e.Err.Error() }
func (e *Retryable) Unwrap() error { return e.Err }

func Retry(err error) error {
	if err == nil {
		return nil
	}
	if ShouldRetry(err) {
		return err
	}
	return &Retryable{Err: err}
}

func ShouldRetry(err error) bool {
	var retryable *Retryable
	return errors.As(err, &retryable)
}
