package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// Limits and defaults of [Config].
const (
	// DefaultRowLimit is the row limit when [Config.RowLimit] is zero.
	DefaultRowLimit = 100
	// MaxRowLimit is the highest accepted [Config.RowLimit].
	MaxRowLimit = 1000
	// DefaultStatementTimeout is used when [Config.StatementTimeout] is zero.
	DefaultStatementTimeout = 5 * time.Second
	// MinStatementTimeout and MaxStatementTimeout bound
	// [Config.StatementTimeout].
	MinStatementTimeout = 100 * time.Millisecond
	MaxStatementTimeout = 60 * time.Second

	// maxConfigBytes bounds a config file.
	maxConfigBytes = 4 << 20
)

var fingerprintRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Config is the runtime configuration of one domain. It is the contract
// between the operator and "kabr serve": the operator renders it as JSON
// (for example into a ConfigMap mounted at /etc/kabr/domain.json):
//
//	{
//	  "domain": "film",
//	  "plan": "pagila",
//	  "catalogFingerprint": "sha256:...",
//	  "schemas": ["public"],
//	  "rowLimit": 100,
//	  "statementTimeout": "5s",
//	  "tables": ["public.actor", "public.film"],
//	  "sensitiveColumns": [
//	    {"table": "public.actor", "column": "last_name", "class": "pii", "action": "mask"}
//	  ]
//	}
//
// It never holds a credential: the DSN and the hash key come from the
// environment or from files (see [Options]).
type Config struct {
	// Domain is the domain name, a DNS-1123 label.
	Domain string `json:"domain"`
	// Plan is the name of the approved plan (metadata.name), a DNS-1123
	// subdomain. Informational: logs and the OpenAPI document.
	Plan string `json:"plan"`
	// CatalogFingerprint is the approved catalog fingerprint
	// (spec.source.catalogFingerprint, "sha256:<64 hex>"). The runtime
	// refuses to serve data while the live catalog has another one.
	CatalogFingerprint string `json:"catalogFingerprint"`
	// Schemas are the database schemas introspected to compute the
	// fingerprint: the schemas of every table of the plan, not only of this
	// domain, because the fingerprint covers the whole catalog the plan was
	// approved for. Empty means the schemas of Tables.
	Schemas []string `json:"schemas,omitempty"`
	// RowLimit is the maximum number of rows of one response, 1..1000.
	// Zero means [DefaultRowLimit].
	RowLimit int `json:"rowLimit,omitempty"`
	// StatementTimeout bounds every query, 100ms..60s, as a Go duration
	// string ("5s"). Zero means [DefaultStatementTimeout].
	StatementTimeout Duration `json:"statementTimeout,omitzero"`
	// Tables are the served table IDs ("schema.table").
	Tables []string `json:"tables"`
	// SensitiveColumns are the sensitive columns of the domain, exactly as
	// in the plan.
	SensitiveColumns []plan.SensitiveColumn `json:"sensitiveColumns,omitempty"`
}

// Duration is a [time.Duration] encoded in JSON as a Go duration string.
type Duration time.Duration

// MarshalJSON implements [json.Marshaler].
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements [json.Unmarshaler]; it accepts only a string
// such as "5s" or "1500ms".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as \"5s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// EffectiveRowLimit returns RowLimit, or [DefaultRowLimit] when it is zero.
func (c *Config) EffectiveRowLimit() int {
	if c.RowLimit == 0 {
		return DefaultRowLimit
	}
	return c.RowLimit
}

// EffectiveStatementTimeout returns StatementTimeout, or
// [DefaultStatementTimeout] when it is zero.
func (c *Config) EffectiveStatementTimeout() time.Duration {
	if c.StatementTimeout == 0 {
		return DefaultStatementTimeout
	}
	return time.Duration(c.StatementTimeout)
}

// EffectiveSchemas returns Schemas, or the sorted schemas of Tables when it
// is empty.
func (c *Config) EffectiveSchemas() []string {
	if len(c.Schemas) > 0 {
		return slices.Clone(c.Schemas)
	}
	var out []string
	for _, id := range c.Tables {
		if s, _, ok := splitTableID(id); ok && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// splitTableID splits "schema.table" at the first dot, like the plan does.
func splitTableID(id string) (sch, name string, ok bool) {
	sch, name, ok = strings.Cut(id, ".")
	return sch, name, ok && sch != "" && name != ""
}

// validClass and validAction mirror the enums of package plan.
func validClass(c plan.SensitiveClass) bool {
	switch c {
	case plan.ClassPII, plan.ClassCredential, plan.ClassFinancial, plan.ClassOther:
		return true
	}
	return false
}

func validAction(a plan.Action) bool {
	switch a {
	case plan.ActionMask, plan.ActionDrop, plan.ActionHash:
		return true
	}
	return false
}

// Validate checks the configuration and returns every problem, joined with
// [errors.Join]. Zero RowLimit and StatementTimeout are valid and mean their
// defaults. Validate does not modify c.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !plan.IsDNSLabel(c.Domain) {
		add("domain: %q is not a valid DNS-1123 label", c.Domain)
	}
	if !plan.IsDNSSubdomain(c.Plan) {
		add("plan: %q is not a valid DNS-1123 subdomain", c.Plan)
	}
	if !fingerprintRE.MatchString(c.CatalogFingerprint) {
		add("catalogFingerprint: must match sha256:<64 hex>, got %q", c.CatalogFingerprint)
	}
	if c.RowLimit != 0 && (c.RowLimit < 1 || c.RowLimit > MaxRowLimit) {
		add("rowLimit: must be between 1 and %d, got %d", MaxRowLimit, c.RowLimit)
	}
	if d := time.Duration(c.StatementTimeout); d != 0 && (d < MinStatementTimeout || d > MaxStatementTimeout) {
		add("statementTimeout: must be between %s and %s, got %s", MinStatementTimeout, MaxStatementTimeout, d)
	}

	schemas := map[string]bool{}
	for i, s := range c.Schemas {
		switch {
		case s == "" || strings.TrimSpace(s) != s:
			add("schemas[%d]: %q is not a valid schema name", i, s)
		case schemas[s]:
			add("schemas[%d]: duplicate schema %q", i, s)
		}
		schemas[s] = true
	}

	if len(c.Tables) == 0 {
		add("tables: at least one table is required")
	}
	tables := map[string]bool{}
	for i, id := range c.Tables {
		sch, name, ok := splitTableID(id)
		switch {
		case !ok || strings.TrimSpace(sch) != sch || strings.TrimSpace(name) != name:
			add("tables[%d]: %q is not a table ID \"schema.table\"", i, id)
		case tables[id]:
			add("tables[%d]: duplicate table %q", i, id)
		case len(c.Schemas) > 0 && !schemas[sch]:
			add("tables[%d]: schema of %q is not listed in schemas", i, id)
		}
		tables[id] = true
	}

	seen := map[[2]string]bool{}
	for i, sc := range c.SensitiveColumns {
		path := fmt.Sprintf("sensitiveColumns[%d]", i)
		if !tables[sc.Table] {
			add("%s.table: %q is not listed in tables", path, sc.Table)
		}
		if strings.TrimSpace(sc.Column) == "" {
			add("%s.column: must not be empty", path)
		}
		if !validClass(sc.Class) {
			add("%s.class: invalid class %q", path, sc.Class)
		}
		if !validAction(sc.Action) {
			add("%s.action: invalid action %q", path, sc.Action)
		}
		key := [2]string{sc.Table, sc.Column}
		if seen[key] {
			add("%s: duplicate entry for %s.%s", path, sc.Table, sc.Column)
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}

// configKeys and sensitiveKeys are the exact JSON keys accepted by
// [DecodeConfig] (encoding/json alone would also accept "Domain").
var (
	configKeys = []string{"domain", "plan", "catalogFingerprint", "schemas", "rowLimit",
		"statementTimeout", "tables", "sensitiveColumns"}
	sensitiveKeys = []string{"table", "column", "class", "action", "source", "rationale"}
)

// DecodeConfig reads one JSON [Config] strictly: exactly one JSON value,
// no unknown, duplicate or differently-cased keys. The result is validated
// with [Config.Validate]; defaults are not filled in (see the Effective*
// methods).
func DecodeConfig(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("serve: read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("serve: config is larger than %d bytes", maxConfigBytes)
	}
	if err := checkConfigKeys(data); err != nil {
		return nil, fmt.Errorf("serve: decode config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("serve: decode config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("serve: decode config: unexpected data after the JSON object")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("serve: invalid config: %w", err)
	}
	return &c, nil
}

// LoadConfig reads and decodes the config file at path (see [DecodeConfig]).
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the operator-provided --config
	if err != nil {
		return nil, fmt.Errorf("serve: open config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return DecodeConfig(f)
}

// checkConfigKeys walks the JSON tokens: no object anywhere may repeat a
// key, and the top-level object and each sensitiveColumns item may only use
// the exact, case-sensitive field names.
func checkConfigKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("config must be a JSON object")
	}
	return walkObject(dec, "", configKeys)
}

// walkObject consumes the members of an object whose '{' was read; allowed,
// when non-nil, is its exact key set.
func walkObject(dec *json.Decoder, path string, allowed []string) error {
	where := ""
	if path != "" {
		where = path + ": "
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("%sduplicate key %q", where, key)
		}
		seen[key] = true
		if allowed != nil && !slices.Contains(allowed, key) {
			return fmt.Errorf("%sunknown key %q", where, key)
		}
		var itemKeys []string
		if path == "" && key == "sensitiveColumns" {
			itemKeys = sensitiveKeys
		}
		child := key
		if path != "" {
			child = path + "." + key
		}
		if tok, err = dec.Token(); err != nil {
			return err
		}
		if err := walkToken(dec, tok, child, nil, itemKeys); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

// walkToken consumes the value that starts with tok. objKeys restricts the
// keys of the value if it is an object, itemKeys those of the objects
// directly inside it if it is an array.
func walkToken(dec *json.Decoder, tok json.Token, path string, objKeys, itemKeys []string) error {
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		return walkObject(dec, path, objKeys)
	case '[':
		for i := 0; dec.More(); i++ {
			item, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkToken(dec, item, fmt.Sprintf("%s[%d]", path, i), itemKeys, nil); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	return nil
}

// ConfigFromPlan builds the [Config] of one domain of an approved plan. It
// refuses a plan whose approval does not verify ([plan.VerifyApproval]: not
// approved, approved before digests existed, or edited after approval; the
// error wraps the matching plan sentinel), that fails
// [plan.DomainPlan.ValidateStructure], or that has no such domain. Schemas
// are the schemas of every table of the plan (the fingerprint covers the
// whole catalog); RowLimit and StatementTimeout are set to their defaults.
//
// The approval digest is an integrity check, not authentication: whoever
// can write the plan can also recompute the digest. Who may approve is
// decided by access control on the plan (file review, Kubernetes RBAC).
func ConfigFromPlan(p *plan.DomainPlan, domain string) (*Config, error) {
	if p == nil {
		return nil, errors.New("serve: plan is nil")
	}
	if err := plan.VerifyApproval(p); err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	if err := p.ValidateStructure(); err != nil {
		return nil, fmt.Errorf("serve: plan %q is invalid: %w", p.Metadata.Name, err)
	}
	d := p.Domain(domain)
	if d == nil {
		names := make([]string, 0, len(p.Spec.Domains))
		for _, dd := range p.Spec.Domains {
			names = append(names, dd.Name)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("serve: plan %q has no domain %q (domains: %s)",
			p.Metadata.Name, domain, strings.Join(names, ", "))
	}
	c := &Config{
		Domain:             d.Name,
		Plan:               p.Metadata.Name,
		CatalogFingerprint: p.Spec.Source.CatalogFingerprint,
		RowLimit:           DefaultRowLimit,
		StatementTimeout:   Duration(DefaultStatementTimeout),
		Tables:             make([]string, 0, len(d.Tables)),
		SensitiveColumns:   slices.Clone(d.SensitiveColumns),
	}
	for _, dd := range p.Spec.Domains {
		for _, t := range dd.Tables {
			if s, _, ok := splitTableID(t.Name); ok && !slices.Contains(c.Schemas, s) {
				c.Schemas = append(c.Schemas, s)
			}
		}
	}
	slices.Sort(c.Schemas)
	for _, t := range d.Tables {
		c.Tables = append(c.Tables, t.Name)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("serve: config for domain %q is invalid: %w", domain, err)
	}
	return c, nil
}
