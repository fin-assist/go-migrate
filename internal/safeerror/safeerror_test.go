package safeerror

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type secretError struct{ secret string }

func (e *secretError) Error() string { return e.secret }
func TestUnitSafeTextRetainsDiagnosticIdentity(t *testing.T) {
	cause := &secretError{secret: "secret-SQL-password-detail"}
	err := Wrap("operation failed", cause)
	for _, format := range []string{"%v", "%+v", "%s", "%q"} {
		if text := fmt.Sprintf(format, err); strings.Contains(text, cause.secret) || !strings.Contains(text, "operation failed") {
			t.Fatal(text)
		}
	}
	var typed *secretError
	if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != cause {
		t.Fatal("diagnostic identity lost")
	}
	if Wrap("stage", nil) != nil {
		t.Fatal("nil wrapped")
	}
}
