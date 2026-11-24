package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// routes builds the handler chain: request log, panic recovery, method
// guard, then the router.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("/v1/{schema}/{table}", s.handleList)
	mux.HandleFunc("/v1/{schema}/{table}/{pk}", s.handleGet)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			// Segments past the table may be key values: keep them out of the log.
			infoOf(r).path = "/v1/{unmatched}"
		}
		writeError(w, newAPIError(http.StatusNotFound, codeNotFound, "no such endpoint"))
	})
	return s.logRequests(s.recoverPanics(methodGuard(mux)))
}

// methodGuard answers 405 to anything but GET and HEAD, for every path.
func methodGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, newAPIError(http.StatusMethodNotAllowed, codeMethodNotAllowed, "only GET and HEAD are allowed"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				s.log.Error("panic in handler", "panic", fmt.Sprint(v))
				writeError(w, errInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// reqInfo collects what the access log may record about a request: table,
// row count and filter column names. Never values: filter values, keys and
// cursors may be personal data, so the query string is never logged and the
// key segment of a get-by-key path is replaced with "{pk}".
type reqInfo struct {
	table   string
	path    string // logged instead of the URL path when set
	rows    int
	data    bool
	filters []string
}

type reqInfoKey struct{}

func infoOf(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

const maxLoggedPath = 256

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri)))
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		path := r.URL.Path
		if ri.path != "" {
			path = ri.path
		}
		if len(path) > maxLoggedPath {
			path = path[:maxLoggedPath] + "..."
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", path),
			slog.Int("status", sw.status),
			slog.Float64("durationMs", float64(time.Since(start).Microseconds())/1000),
		}
		if ri.table != "" {
			attrs = append(attrs, slog.String("table", ri.table))
		}
		if ri.data {
			attrs = append(attrs, slog.Int("rows", ri.rows))
		}
		if len(ri.filters) > 0 {
			attrs = append(attrs, slog.Any("filters", ri.filters))
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		s.log.LogAttrs(r.Context(), level, "request", attrs...)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`+"\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	st := s.state.Load()
	if !st.ready {
		writeError(w, newAPIError(http.StatusServiceUnavailable, codeNotReady, st.category))
		return
	}
	if time.Since(time.Unix(0, s.lastDBOK.Load())) > readyPingAfter {
		ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
		err := s.db.ping(ctx)
		cancel()
		if err != nil {
			s.log.Warn("readiness ping failed", "error", s.scrub(err))
			writeError(w, newAPIError(http.StatusServiceUnavailable, codeNotReady, notReadyDatabase))
			return
		}
		s.lastDBOK.Store(time.Now().UnixNano())
	}
	body, _ := json.Marshal(map[string]any{
		"status": "ready", "domain": s.cfg.Domain, "plan": s.cfg.Plan, "tables": len(st.model.tables),
	})
	writeJSON(w, http.StatusOK, append(body, '\n'))
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	st := s.state.Load()
	if !st.ready {
		writeError(w, notReadyError)
		return
	}
	writeJSON(w, http.StatusOK, st.openapi)
}

var notReadyError = newAPIError(http.StatusServiceUnavailable, codeNotReady,
	"the domain is not ready (see /readyz)")

// readyTable returns the ready model and the allowlisted table addressed by
// the request, or writes the error answer and returns nil.
func (s *Server) readyTable(w http.ResponseWriter, r *http.Request) (*model, *table) {
	st := s.state.Load()
	if !st.ready {
		writeError(w, notReadyError)
		return nil, nil
	}
	t := st.model.table(r.PathValue("schema"), r.PathValue("table"))
	if t == nil {
		writeError(w, newAPIError(http.StatusNotFound, codeNotFound, "unknown table"))
		return nil, nil
	}
	return st.model, t
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ri := infoOf(r)
	m, t := s.readyTable(w, r)
	if t == nil {
		return
	}
	ri.table, ri.data = t.id, true
	q, names, aerr := parseListQuery(t, r.URL.RawQuery, m.rowLimit)
	ri.filters = names
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	sql, args := buildList(t, q)
	ctx, cancel := context.WithTimeout(r.Context(), s.stmtTimeout+requestSlack)
	defer cancel()

	var (
		buf  = append(make([]byte, 0, 4096), `{"items":[`...)
		n    int
		more bool
		next []string
	)
	err := s.db.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		dests := newScanDests(t.columns)
		for rows.Next() {
			if n == q.limit {
				more = true // the extra row only tells that a next page exists
				break
			}
			if err = rows.Scan(dests...); err != nil {
				return err
			}
			if n > 0 {
				buf = append(buf, ',')
			}
			if buf, err = appendRow(buf, t.columns, dests, m.hashKey); err != nil {
				return err
			}
			n++
		}
		if more {
			next = make([]string, len(t.pk))
			for i, ci := range t.pk {
				if next[i], err = keyString(dests[ci]); err != nil {
					return err
				}
			}
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		s.failQuery(w, r, t, err)
		return
	}
	s.lastDBOK.Store(time.Now().UnixNano())
	ri.rows = n
	buf = append(buf, `],"nextCursor":`...)
	if more {
		buf = appendString(buf, encodeCursor(next))
	} else {
		buf = append(buf, "null"...)
	}
	writeJSON(w, http.StatusOK, append(buf, "}\n"...))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	ri := infoOf(r)
	// The key may be personal data: never log it, whatever the outcome.
	ri.path = "/v1/" + url.PathEscape(r.PathValue("schema")) + "/" + url.PathEscape(r.PathValue("table")) + "/{pk}"
	m, t := s.readyTable(w, r)
	if t == nil {
		return
	}
	ri.table, ri.data = t.id, true
	if len(t.pk) != 1 {
		writeError(w, newAPIError(http.StatusNotFound, codeNotFound,
			"this table has a composite primary key: list it and filter on the key columns"))
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, newAPIError(http.StatusBadRequest, codeInvalidQuery,
			"query parameters are not supported on this endpoint"))
		return
	}
	key := r.PathValue("pk")
	pk := &t.columns[t.pk[0]]
	if err := checkTypedValue(pk, key); err != nil {
		writeError(w, newAPIError(http.StatusBadRequest, codeInvalidValue, "invalid key: "+err.Error()))
		return
	}
	sql, args := buildGet(t, key)
	ctx, cancel := context.WithTimeout(r.Context(), s.stmtTimeout+requestSlack)
	defer cancel()
	var buf []byte
	err := s.db.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			dests := newScanDests(t.columns)
			if err = rows.Scan(dests...); err != nil {
				return err
			}
			if buf, err = appendRow(nil, t.columns, dests, m.hashKey); err != nil {
				return err
			}
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		s.failQuery(w, r, t, err)
		return
	}
	s.lastDBOK.Store(time.Now().UnixNano())
	if buf == nil {
		writeError(w, newAPIError(http.StatusNotFound, codeNotFound, "no row with this key"))
		return
	}
	ri.rows = 1
	writeJSON(w, http.StatusOK, append(buf, '\n'))
}

// failQuery answers a failed data query. Details go to the log only.
func (s *Server) failQuery(w http.ResponseWriter, r *http.Request, t *table, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client went away; nobody reads an answer
	}
	ae, serious := classifyDBError(err)
	if serious {
		s.log.Error("query failed", "table", t.id, "error", s.scrub(err))
	} else {
		s.log.Info("query rejected", "table", t.id, "status", ae.Status, "error", s.scrub(err))
	}
	writeError(w, ae)
}

// parseListQuery validates the query string of a list request against the
// allowlist of t. It returns the accepted filter column names for the log.
func parseListQuery(t *table, rawQuery string, rowLimit int) (listQuery, []string, *apiError) {
	q := listQuery{limit: rowLimit}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		return q, nil, newAPIError(http.StatusBadRequest, codeInvalidQuery, "malformed query string")
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var names []string
	for _, k := range keys {
		if len(vals[k]) != 1 {
			return q, names, newAPIError(http.StatusBadRequest, codeInvalidQuery,
				"query parameter "+quoteParam(k)+" is repeated")
		}
		v := vals[k][0]
		switch k {
		case paramLimit:
			if v == "" {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return q, names, newAPIError(http.StatusBadRequest, codeInvalidLimit, "limit must be an integer")
			}
			q.limit = min(max(n, 1), rowLimit)
		case paramCursor:
			if v == "" {
				continue
			}
			after, err := decodeCursor(v, len(t.pk))
			if err != nil {
				return q, names, newAPIError(http.StatusBadRequest, codeInvalidCursor,
					"invalid cursor: pass the nextCursor of a previous page unchanged")
			}
			q.after = after
		default:
			c := t.column(k)
			if c == nil || !c.filterable() {
				// Same answer for unknown, dropped, sensitive and
				// non-filterable columns: no existence oracle.
				return q, names, newAPIError(http.StatusBadRequest, codeInvalidFilter,
					"column "+quoteParam(k)+" does not exist or is not filterable")
			}
			if err := checkTypedValue(c, v); err != nil {
				return q, names, newAPIError(http.StatusBadRequest, codeInvalidValue,
					"invalid value for column "+quoteParam(c.name)+": "+err.Error())
			}
			q.filters = append(q.filters, filterArg{col: t.byName[c.name], value: v})
			names = append(names, c.name)
		}
	}
	slices.SortFunc(q.filters, func(a, b filterArg) int { return a.col - b.col })
	return q, names, nil
}

// checkTypedValue rejects values that cannot be valid for the column before
// they reach the database. The server still parses every value with the
// column type (its errors become 400 too); this only gives clearer messages
// for the common cases.
func checkTypedValue(c *column, v string) error {
	if err := checkValue(v); err != nil {
		return err
	}
	if c.kind == kindInt {
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return errors.New("not an integer")
		}
	}
	return nil
}

// quoteParam quotes a request-supplied name for an error message, shortened.
func quoteParam(s string) string {
	const maxRunes = 64
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = string(r[:maxRunes]) + "..."
	}
	return strconv.Quote(s)
}
