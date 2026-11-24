package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Error codes of the JSON error body {"error": code, "message": ...}.
const (
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeInvalidFilter    = "invalid_filter"
	codeInvalidCursor    = "invalid_cursor"
	codeInvalidLimit     = "invalid_limit"
	codeInvalidQuery     = "invalid_query"
	codeInvalidValue     = "invalid_value"
	codeNotReady         = "not_ready"
	codeUnavailable      = "unavailable"
	codeTimeout          = "timeout"
	codeInternal         = "internal"
)

// apiError is an error answered to the client. Message is safe to show:
// it never holds SQL, the DSN or driver text.
type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, msg string) *apiError {
	return &apiError{Status: status, Code: code, Message: msg}
}

var errInternal = newAPIError(http.StatusInternalServerError, codeInternal, "internal error")

// classifyDBError maps an error of a data query to the client answer and
// reports whether the cause belongs in the log at error level (true) or is a
// client fault / transient condition (false). Server error text is never
// copied into the answer.
func classifyDBError(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae, false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "57014": // query_canceled: statement_timeout
			return newAPIError(http.StatusGatewayTimeout, codeTimeout, "the query exceeded the statement timeout"), false
		case strings.HasPrefix(pgErr.Code, "22"): // data_exception: a bad bind value
			return newAPIError(http.StatusBadRequest, codeInvalidValue,
				"a filter, key or cursor value is not valid for the column type"), false
		case pgErr.Code == "53300", pgErr.Code == "57P01", pgErr.Code == "57P03",
			strings.HasPrefix(pgErr.Code, "08"): // too many connections, shutdown, connection exceptions
			return newAPIError(http.StatusServiceUnavailable, codeUnavailable, "the database is unavailable"), true
		}
		return errInternal, true
	}
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return newAPIError(http.StatusGatewayTimeout, codeTimeout, "the request timed out"), false
	}
	if errors.Is(err, errPoolClosed) {
		return newAPIError(http.StatusServiceUnavailable, codeUnavailable, "the server is shutting down"), false
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return newAPIError(http.StatusServiceUnavailable, codeUnavailable, "the database is unavailable"), true
	}
	return errInternal, true
}

// writeJSON writes a JSON body with the headers every answer carries.
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, e *apiError) {
	body, _ := json.Marshal(e) // two strings: cannot fail
	writeJSON(w, e.Status, append(body, '\n'))
}
