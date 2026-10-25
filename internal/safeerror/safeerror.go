// Package safeerror keeps private diagnostics out of normal error formatting.
package safeerror

// Wrap reports only the supplied constant stage. The cause remains available for
// errors.Is/As diagnostics; callers must never log or expose an unwrapped cause.
func Wrap(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return failure{stage: stage, cause: cause}
}

type failure struct {
	stage string
	cause error
}

func (e failure) Error() string { return e.stage }
func (e failure) Unwrap() error { return e.cause }
