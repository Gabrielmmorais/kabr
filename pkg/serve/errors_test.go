package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifyDBError(t *testing.T) {
	pg := func(code string) error {
		return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, Message: "secret detail: " + code})
	}
	tests := []struct {
		name    string
		err     error
		status  int
		code    string
		serious bool
	}{
		{"statement timeout", pg("57014"), http.StatusGatewayTimeout, codeTimeout, false},
		{"bad text", pg("22P02"), http.StatusBadRequest, codeInvalidValue, false},
		{"out of range", pg("22003"), http.StatusBadRequest, codeInvalidValue, false},
		{"bad datetime", pg("22007"), http.StatusBadRequest, codeInvalidValue, false},
		{"bad encoding", pg("22021"), http.StatusBadRequest, codeInvalidValue, false},
		{"permission", pg("42501"), http.StatusInternalServerError, codeInternal, true},
		{"read only", pg("25006"), http.StatusInternalServerError, codeInternal, true},
		{"connection", pg("08006"), http.StatusServiceUnavailable, codeUnavailable, true},
		{"too many", pg("53300"), http.StatusServiceUnavailable, codeUnavailable, true},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, codeTimeout, false},
		{"pool closed", errPoolClosed, http.StatusServiceUnavailable, codeUnavailable, false},
		{"api error", newAPIError(http.StatusNotFound, codeNotFound, "x"), http.StatusNotFound, codeNotFound, false},
		{"anything else", errors.New("boom"), http.StatusInternalServerError, codeInternal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ae, serious := classifyDBError(tt.err)
			if ae.Status != tt.status || ae.Code != tt.code || serious != tt.serious {
				t.Errorf("got %d %s %v, want %d %s %v", ae.Status, ae.Code, serious, tt.status, tt.code, tt.serious)
			}
			if strings.Contains(ae.Message, "secret detail") {
				t.Errorf("server text leaked into the answer: %q", ae.Message)
			}
		})
	}
}
