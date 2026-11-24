//go:build e2e

// Package e2e is the end-to-end test of Kabr M3 on a local kind cluster:
// the operator turns the approved Pagila DomainPlan into one read-only REST
// API per domain, and the APIs serve rows with pagination and masking.
//
// It runs against a cluster prepared by "make e2e" (kind-up + demo-k8s) and
// is excluded from every other build by the e2e build tag. It uses only the
// standard library and the kubectl binary; every kubectl call passes
// --kubeconfig and --context explicitly, taken from the environment:
//
//	KIND_KUBECONFIG              kubeconfig of the kind cluster (required)
//	KUBE_CONTEXT                 its context, e.g. kind-kabr (required)
//	KABR_E2E_NAMESPACE           namespace of the DomainPlan (default kabr-demo)
//	KABR_E2E_OPERATOR_NAMESPACE  namespace of the operator (default kabr-system)
//	KABR_E2E_PLAN                DomainPlan name (default pagila)
//	KABR_E2E_READY_TIMEOUT       wait for the DomainAPIs, Go duration (default 5m)
//	KABR_E2E_PLAN_MANIFEST       the applied DomainPlan manifest, re-applied after
//	                             the fail-closed subtest (required)
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Domains of examples/pagila.plan.yaml; the operator names each DomainAPI
// (and its Deployment, Service and NetworkPolicy) "<plan>-<domain>".
var domains = []string{"city", "film", "store"}

// apiPort is the port of the per-domain Service.
const apiPort = 8080

type env struct {
	kubeconfig, context string
	ns, operatorNS      string
	plan                string
	planManifest        string
	readyTimeout        time.Duration
}

func loadEnv(t *testing.T) env {
	t.Helper()
	e := env{
		kubeconfig: os.Getenv("KIND_KUBECONFIG"),
		context:    os.Getenv("KUBE_CONTEXT"),
		ns:         getenv("KABR_E2E_NAMESPACE", "kabr-demo"),
		operatorNS: getenv("KABR_E2E_OPERATOR_NAMESPACE", "kabr-system"),
		plan:       getenv("KABR_E2E_PLAN", "pagila"),
		// The fail-closed subtest edits the live DomainPlan and restores
		// it from this manifest (bin/pagila.domainplan.yaml in make e2e).
		planManifest: os.Getenv("KABR_E2E_PLAN_MANIFEST"),
	}
	if e.kubeconfig == "" || e.context == "" {
		t.Fatal("KIND_KUBECONFIG and KUBE_CONTEXT must be set (run through \"make e2e\" or \"make e2e-test\"); " +
			"the test never falls back to ~/.kube/config or the current context")
	}
	if e.planManifest == "" {
		t.Fatal("KABR_E2E_PLAN_MANIFEST must name the applied DomainPlan manifest (make e2e-test sets it)")
	}
	if _, err := os.Stat(e.planManifest); err != nil {
		t.Fatalf("KABR_E2E_PLAN_MANIFEST: %v", err)
	}
	d, err := time.ParseDuration(getenv("KABR_E2E_READY_TIMEOUT", "5m"))
	if err != nil || d <= 0 {
		t.Fatalf("KABR_E2E_READY_TIMEOUT: invalid duration %q", os.Getenv("KABR_E2E_READY_TIMEOUT"))
	}
	e.readyTimeout = d
	return e
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// kubectlCmd builds a kubectl command with the explicit kubeconfig and
// context; nothing ambient is ever used.
func (e env) kubectlCmd(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--kubeconfig", e.kubeconfig, "--context", e.context}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	// Belt and braces: even a kubectl plugin or exec credential helper sees
	// only the kind kubeconfig.
	cmd.Env = append(os.Environ(), "KUBECONFIG="+e.kubeconfig)
	return cmd
}

// kubectl runs kubectl with a timeout and returns its combined output.
func (e env) kubectl(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := e.kubectlCmd(ctx, args...).CombinedOutput()
	return string(out), err
}

func (e env) name(domain string) string { return e.plan + "-" + domain }

func TestKabrE2E(t *testing.T) {
	e := loadEnv(t)
	t.Cleanup(func() {
		if t.Failed() {
			e.diagnostics(t)
		}
	})

	t.Run("DomainAPIsReady", e.waitDomainAPIsReady)
	if t.Failed() {
		return
	}

	t.Run("NetworkPolicies", func(t *testing.T) {
		for _, d := range domains {
			n := e.name(d)
			out, err := e.kubectl(30*time.Second, "-n", e.ns, "get", "networkpolicy", n, "-o", "json")
			if err != nil {
				t.Errorf("NetworkPolicy %s: %v\n%s", n, err, out)
				continue
			}
			var np struct {
				Spec struct {
					PolicyTypes []string `json:"policyTypes"`
					Egress      []struct {
						To    []json.RawMessage `json:"to"`
						Ports []json.RawMessage `json:"ports"`
					} `json:"egress"`
				} `json:"spec"`
			}
			if err := json.Unmarshal([]byte(out), &np); err != nil {
				t.Errorf("NetworkPolicy %s: decode: %v", n, err)
				continue
			}
			if !contains(np.Spec.PolicyTypes, "Egress") {
				t.Errorf("NetworkPolicy %s does not restrict egress (policyTypes %v)", n, np.Spec.PolicyTypes)
			}
			// Egress must be allow-listed: at least one rule (the database),
			// and no rule without peers ("to" empty or absent allows every
			// destination, "{}" included).
			if len(np.Spec.Egress) == 0 {
				t.Errorf("NetworkPolicy %s has no egress rules (the database must be allowed explicitly)", n)
			}
			for i, r := range np.Spec.Egress {
				if len(r.To) == 0 {
					t.Errorf("NetworkPolicy %s: egress[%d] has no \"to\" peers, so it allows every destination", n, i)
				}
				for j, peer := range r.To {
					if p := strings.TrimSpace(string(peer)); p == "{}" || p == "null" {
						t.Errorf("NetworkPolicy %s: egress[%d].to[%d] is empty (%s), so it allows everything", n, i, j, p)
					}
				}
			}
		}
	})

	t.Run("Film", func(t *testing.T) {
		base := e.portForward(t, e.name("film"))
		mustStatus(t, base+"/readyz", http.StatusOK, 60*time.Second)
		mustStatus(t, base+"/healthz", http.StatusOK, 10*time.Second)

		page1 := getList(t, base+"/v1/public/film?limit=5")
		if len(page1.Items) != 5 {
			t.Fatalf("film page 1: got %d items, want exactly 5", len(page1.Items))
		}
		if page1.NextCursor == nil || *page1.NextCursor == "" {
			t.Fatal("film page 1: nextCursor is null; Pagila has 1000 films")
		}
		page2 := getList(t, base+"/v1/public/film?limit=5&cursor="+url.QueryEscape(*page1.NextCursor))
		if len(page2.Items) != 5 {
			t.Fatalf("film page 2: got %d items, want 5", len(page2.Items))
		}
		ids1, ids2 := ids(t, page1.Items, "film_id"), ids(t, page2.Items, "film_id")
		for id := range ids2 {
			if ids1[id] {
				t.Errorf("film page 2 repeats film_id %s of page 1 (cursor not followed)", id)
			}
		}

		// Sensitive columns of the film domain: actor first/last name, mask.
		actors := getList(t, base+"/v1/public/actor?limit=20")
		if len(actors.Items) == 0 {
			t.Fatal("actor: no rows")
		}
		for i, a := range actors.Items {
			for _, c := range []string{"first_name", "last_name"} {
				if v, ok := a[c]; !ok || v != "***" {
					t.Errorf("actor[%d].%s = %v, want masked \"***\"", i, c, v)
				}
			}
		}

		body := get(t, base+"/openapi.json", http.StatusOK)
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("/openapi.json is not valid JSON: %v", err)
		}
		if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.") {
			t.Errorf("/openapi.json: openapi = %q, want 3.x", v)
		}

		get(t, base+"/v1/public/no_such_table", http.StatusNotFound)
		// A table of another domain is not served here either.
		get(t, base+"/v1/public/customer", http.StatusNotFound)
	})

	t.Run("Store", func(t *testing.T) {
		base := e.portForward(t, e.name("store"))
		mustStatus(t, base+"/readyz", http.StatusOK, 60*time.Second)

		// staff.password: action drop -> the key must not exist at all.
		raw := get(t, base+"/v1/public/staff?limit=100", http.StatusOK)
		staff := decodeList(t, raw)
		if len(staff.Items) == 0 {
			t.Fatal("staff: no rows (Pagila has 2)")
		}
		for i, s := range staff.Items {
			if _, ok := s["password"]; ok {
				t.Errorf("staff[%d] contains the dropped column password", i)
			}
			for _, c := range []string{"email", "first_name", "last_name"} {
				if v, ok := s[c]; !ok || v != "***" {
					t.Errorf("staff[%d].%s = %v (present %t), want masked \"***\"", i, c, v, ok)
				}
			}
			// username: class credential (action hash in the plan). The
			// runtime drops a credential whatever its action, so the key
			// must not exist at all, hashed or clear.
			if v, ok := s["username"]; ok {
				t.Errorf("staff[%d] contains the credential column username (%v); credentials are always dropped", i, v)
			}
		}
		if bytes.Contains(raw, []byte(`"username"`)) {
			t.Error("staff response contains a \"username\" key")
		}
		if bytes.Contains(raw, []byte(`"password"`)) {
			t.Error("staff response contains a \"password\" key")
		}
		// Pagila staff emails all end in this domain; none may leak.
		if bytes.Contains(bytes.ToLower(raw), []byte("@sakilastaff.com")) {
			t.Error("staff response leaks a clear email address")
		}

		// customer: email/first_name/last_name are masked.
		raw = get(t, base+"/v1/public/customer?limit=50", http.StatusOK)
		customers := decodeList(t, raw)
		if len(customers.Items) == 0 {
			t.Fatal("customer: no rows")
		}
		for i, c := range customers.Items {
			if v, ok := c["email"]; !ok || v != "***" {
				t.Errorf("customer[%d].email = %v, want masked \"***\"", i, v)
			}
			for _, col := range []string{"first_name", "last_name"} {
				if v, ok := c[col]; !ok || v != "***" {
					t.Errorf("customer[%d].%s = %v, want masked \"***\"", i, col, v)
				}
			}
		}
		// Pagila customer emails all end in this domain; none may leak.
		if bytes.Contains(bytes.ToLower(raw), []byte("@sakilacustomer.org")) {
			t.Error("customer response leaks a clear email address")
		}

		get(t, base+"/v1/public/no_such_table", http.StatusNotFound)

		// The only hashed column of the store domain is a credential,
		// which the runtime drops: the hash key Secret must not be mounted.
		out, err := e.kubectl(30*time.Second, "-n", e.ns, "get", "deployment", e.name("store"), "-o", "json")
		if err != nil {
			t.Fatalf("get deployment %s: %v\n%s", e.name("store"), err, out)
		}
		if strings.Contains(out, "pagila-hash-key") || strings.Contains(out, "--hash-key-file") {
			t.Errorf("deployment %s mounts the hash key although no column is hashed", e.name("store"))
		}
	})

	t.Run("City", func(t *testing.T) {
		base := e.portForward(t, e.name("city"))
		mustStatus(t, base+"/readyz", http.StatusOK, 60*time.Second)
		l := getList(t, base+"/v1/public/city?limit=3")
		if len(l.Items) != 3 {
			t.Errorf("city: got %d items, want 3", len(l.Items))
		}
		get(t, base+"/v1/public/film", http.StatusNotFound)
	})
	if t.Failed() {
		return
	}

	// The runtime config comes from the re-verified DomainPlan, never from
	// the DomainAPI spec: clearing the DomainAPI's sensitiveColumns changes
	// neither the rendered config nor what is served, and the DomainPlan
	// reconciler restores the copy.
	t.Run("MaskingSurvivesDomainAPIEdit", func(t *testing.T) {
		n := e.name("store")
		before := e.configJSON(t, n)
		gen := func() int {
			out, err := e.kubectl(15*time.Second, "-n", e.ns, "get", "domainapis.kabr.dev", n, "-o", "jsonpath={.metadata.generation}")
			g, cerr := strconv.Atoi(strings.TrimSpace(out))
			if err != nil || cerr != nil {
				t.Fatalf("generation of DomainAPI %s: %v %v\n%s", n, err, cerr, out)
			}
			return g
		}
		genBefore := gen()
		out, err := e.kubectl(30*time.Second, "-n", e.ns, "patch", "domainapis.kabr.dev", n,
			"--type=json", "-p", `[{"op":"remove","path":"/spec/sensitiveColumns"}]`)
		if err != nil {
			t.Fatalf("patch DomainAPI %s: %v\n%s", n, err, out)
		}
		waitFor(t, 60*time.Second, "DomainAPI "+n+" spec.sensitiveColumns restored by the operator", func() (bool, string) {
			out, err := e.kubectl(15*time.Second, "-n", e.ns, "get", "domainapis.kabr.dev", n,
				"-o", "jsonpath={.spec.sensitiveColumns[*].column}")
			return err == nil && strings.Contains(out, "email"), out
		})
		// Two spec changes: the edit, then the operator's restore.
		if g := gen(); g < genBefore+2 {
			t.Errorf("DomainAPI %s generation %d, want >= %d (edit + restore)", n, g, genBefore+2)
		}
		if after := e.configJSON(t, n); after != before {
			t.Errorf("ConfigMap %s-config changed after editing the DomainAPI spec", n)
		}
		base := e.portForward(t, n)
		mustStatus(t, base+"/readyz", http.StatusOK, 60*time.Second)
		raw := get(t, base+"/v1/public/customer?limit=50", http.StatusOK)
		for i, c := range decodeList(t, raw).Items {
			for _, col := range []string{"email", "first_name", "last_name"} {
				if v, ok := c[col]; !ok || v != "***" {
					t.Errorf("customer[%d].%s = %v after the DomainAPI edit, want masked \"***\"", i, col, v)
				}
			}
		}
		if bytes.Contains(bytes.ToLower(raw), []byte("@sakilacustomer.org")) {
			t.Error("customer response leaks a clear email address after the DomainAPI edit")
		}
	})

	// Editing the approved DomainPlan (dropping a sensitive column) breaks
	// its approval digest: the operator deletes every DomainAPI of the plan
	// and their workloads. Re-applying the approved manifest restores them.
	t.Run("EditedPlanFailsClosed", func(t *testing.T) {
		path := e.sensitiveColumnPath(t, "store", "public.customer", "email")
		patch := fmt.Sprintf(`[{"op":"test","path":"%[1]s/column","value":"email"},{"op":"remove","path":"%[1]s"}]`, path)
		out, err := e.kubectl(30*time.Second, "-n", e.ns, "patch", "domainplans.kabr.dev", e.plan, "--type=json", "-p", patch)
		if err != nil {
			t.Fatalf("patch DomainPlan %s: %v\n%s", e.plan, err, out)
		}
		t.Cleanup(func() { e.restorePlan(t) })

		waitFor(t, 60*time.Second, "DomainPlan "+e.plan+" Ready=False/EditedAfterApproval", func() (bool, string) {
			out, err := e.kubectl(15*time.Second, "-n", e.ns, "get", "domainplans.kabr.dev", e.plan,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}`)
			return err == nil && out == "False/EditedAfterApproval", out
		})
		sel := "kabr.dev/plan=" + e.plan
		waitFor(t, 2*time.Minute, "every DomainAPI, Deployment and Service of the plan deleted", func() (bool, string) {
			out, err := e.kubectl(15*time.Second, "-n", e.ns, "get", "domainapis.kabr.dev,deployments,services,networkpolicies,configmaps",
				"-l", sel, "-o", "name")
			return err == nil && strings.TrimSpace(out) == "", out
		})

		e.restorePlan(t)
		e.waitDomainAPIsReady(t)
		base := e.portForward(t, e.name("store"))
		mustStatus(t, base+"/readyz", http.StatusOK, 60*time.Second)
		for i, c := range getList(t, base+"/v1/public/customer?limit=5").Items {
			if v := c["email"]; v != "***" {
				t.Errorf("customer[%d].email = %v after restoring the plan, want masked \"***\"", i, v)
			}
		}
	})
}

// waitDomainAPIsReady waits until every DomainAPI exists, is Ready and its
// Deployment rolled out. The operator creates the DomainAPIs asynchronously
// after the DomainPlan is applied, so "wait --for=condition" alone (which
// fails at once on a missing object) would race with it.
func (e env) waitDomainAPIsReady(t *testing.T) {
	t.Helper()
	for _, d := range domains {
		n := e.name(d)
		out, err := e.kubectl(e.readyTimeout+30*time.Second, "-n", e.ns, "wait",
			"--for=create", "domainapis.kabr.dev/"+n, "--timeout="+e.readyTimeout.String())
		if err != nil {
			t.Fatalf("DomainAPI %s was not created within %s: %v\n%s", n, e.readyTimeout, err, out)
		}
		out, err = e.kubectl(e.readyTimeout+30*time.Second, "-n", e.ns, "wait",
			"--for=condition=Ready", "domainapis.kabr.dev/"+n, "--timeout="+e.readyTimeout.String())
		if err != nil {
			t.Fatalf("DomainAPI %s did not become Ready within %s: %v\n%s", n, e.readyTimeout, err, out)
		}
		// rollout status also needs the Deployment to exist.
		out, err = e.kubectl(e.readyTimeout+30*time.Second, "-n", e.ns, "wait",
			"--for=create", "deployment/"+n, "--timeout="+e.readyTimeout.String())
		if err != nil {
			t.Fatalf("Deployment %s was not created within %s: %v\n%s", n, e.readyTimeout, err, out)
		}
		out, err = e.kubectl(3*time.Minute, "-n", e.ns, "rollout", "status", "deployment/"+n, "--timeout=150s")
		if err != nil {
			t.Fatalf("Deployment %s did not roll out: %v\n%s", n, err, out)
		}
	}
}

// restorePlan re-applies the approved DomainPlan manifest (idempotent).
func (e env) restorePlan(t *testing.T) {
	t.Helper()
	out, err := e.kubectl(60*time.Second, "apply", "-f", e.planManifest)
	if err != nil {
		t.Errorf("re-apply %s: %v\n%s", e.planManifest, err, out)
	}
}

// configJSON returns the rendered runtime config of a DomainAPI.
func (e env) configJSON(t *testing.T, api string) string {
	t.Helper()
	out, err := e.kubectl(30*time.Second, "-n", e.ns, "get", "configmap", api+"-config", "-o", `jsonpath={.data.domain\.json}`)
	if err != nil || out == "" {
		t.Fatalf("get configmap %s-config: %v\n%s", api, err, out)
	}
	return out
}

// sensitiveColumnPath returns the JSON pointer of a sensitive column of a
// domain in the live DomainPlan.
func (e env) sensitiveColumnPath(t *testing.T, domain, table, column string) string {
	t.Helper()
	out, err := e.kubectl(30*time.Second, "-n", e.ns, "get", "domainplans.kabr.dev", e.plan, "-o", "json")
	if err != nil {
		t.Fatalf("get DomainPlan %s: %v\n%s", e.plan, err, out)
	}
	var dp struct {
		Spec struct {
			Domains []struct {
				Name             string `json:"name"`
				SensitiveColumns []struct {
					Table  string `json:"table"`
					Column string `json:"column"`
				} `json:"sensitiveColumns"`
			} `json:"domains"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &dp); err != nil {
		t.Fatalf("decode DomainPlan: %v", err)
	}
	for i, d := range dp.Spec.Domains {
		if d.Name != domain {
			continue
		}
		for j, c := range d.SensitiveColumns {
			if c.Table == table && c.Column == column {
				return fmt.Sprintf("/spec/domains/%d/sensitiveColumns/%d", i, j)
			}
		}
	}
	t.Fatalf("DomainPlan %s has no sensitive column %s.%s in domain %s", e.plan, table, column, domain)
	return ""
}

// waitFor polls cond every 2s until it holds, for at most timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (last: %.500s)", timeout, what, last)
		}
		time.Sleep(2 * time.Second)
	}
}

// --- port-forward -------------------------------------------------------------

// portForward forwards a free local port to svc/<name>:8080 and returns the
// base URL; the forward is stopped when the test ends.
func (e env) portForward(t *testing.T, svc string) string {
	t.Helper()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := e.kubectlCmd(ctx, "-n", e.ns, "port-forward", "--address", "127.0.0.1",
		"svc/"+svc, fmt.Sprintf("%d:%d", port, apiPort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("port-forward svc/%s: %v", svc, err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		signalled := false
		for sc.Scan() {
			if !signalled && strings.HasPrefix(sc.Text(), "Forwarding from") {
				close(ready)
				signalled = true
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatalf("port-forward svc/%s did not start within 30s: %s", svc, stderr.String())
	}
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the
// kubectl process (os/exec copies stderr in its own goroutine) and the
// reads of the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// --- HTTP helpers ---------------------------------------------------------------

var client = &http.Client{Timeout: 15 * time.Second}

type list struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

// get fetches u and fails unless the status is want.
func get(t *testing.T, u string, want int) []byte {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		t.Fatalf("GET %s: read body: %v", u, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d; body: %.500s", u, resp.StatusCode, want, body)
	}
	return body
}

func getList(t *testing.T, u string) list {
	t.Helper()
	return decodeList(t, get(t, u, http.StatusOK))
}

func decodeList(t *testing.T, body []byte) list {
	t.Helper()
	var l list
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("decode list: %v; body: %.500s", err, body)
	}
	if l.Items == nil {
		t.Fatalf("list response has no items array; body: %.500s", body)
	}
	return l
}

// mustStatus polls u until it answers want, for at most timeout (the
// runtime answers 503 until its first catalog check succeeds).
func mustStatus(t *testing.T, u string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "no response"
	for {
		resp, err := client.Get(u)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = fmt.Sprintf("status %d: %.300s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: no %d within %s (last: %s)", u, want, timeout, last)
		}
		time.Sleep(2 * time.Second)
	}
}

func ids(t *testing.T, items []map[string]any, col string) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(items))
	for i, it := range items {
		v, ok := it[col]
		if !ok {
			t.Fatalf("item %d has no %s", i, col)
		}
		out[fmt.Sprint(v)] = true
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// --- diagnostics ----------------------------------------------------------------

// diagnostics prints the state of the plan, the domain APIs and the
// operator. Secrets are never dumped (only their names).
func (e env) diagnostics(t *testing.T) {
	t.Helper()
	run := func(args ...string) {
		out, err := e.kubectl(60*time.Second, args...)
		if err != nil {
			out += "\n(error: " + err.Error() + ")"
		}
		t.Logf("$ kubectl %s\n%s", strings.Join(args, " "), out)
	}
	run("-n", e.ns, "get", "datasources.kabr.dev,domainplans.kabr.dev,domainapis.kabr.dev,deploy,svc,networkpolicy,pods,secrets", "-o", "wide")
	run("-n", e.ns, "get", "domainplans.kabr.dev", e.plan, "-o", "jsonpath={.status}")
	for _, d := range domains {
		n := e.name(d)
		run("-n", e.ns, "get", "domainapis.kabr.dev", n, "-o", "jsonpath={.status}")
		run("-n", e.ns, "describe", "deployment", n)
		run("-n", e.ns, "logs", "deployment/"+n, "--all-containers", "--tail=100")
		run("-n", e.ns, "get", "networkpolicy", n, "-o", "yaml")
	}
	run("-n", e.ns, "describe", "pods")
	run("-n", e.ns, "get", "events", "--sort-by=.lastTimestamp")
	run("-n", e.operatorNS, "get", "pods", "-o", "wide")
	run("-n", e.operatorNS, "logs", "deployment/kabr-operator", "--all-containers", "--tail=300")
}
