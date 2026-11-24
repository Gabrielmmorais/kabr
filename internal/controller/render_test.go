package controller

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

var testFingerprint = "sha256:" + strings.Repeat("ab", 32)

func testPlanCR(t *testing.T) *kabrv1.DomainPlan {
	t.Helper()
	p := &plan.DomainPlan{
		APIVersion: plan.APIVersion, Kind: plan.Kind, Metadata: plan.Metadata{Name: "shop"},
		Spec: plan.Spec{
			Source: plan.Source{Dialect: "postgres", Database: "shop", SchemaVersion: "kabr.dev/v1alpha1", CatalogFingerprint: testFingerprint},
			Domains: []plan.Domain{
				{Name: "sales", Tables: []plan.TableAssignment{{Name: "sales.order"}, {Name: "sales.item"}},
					SensitiveColumns: []plan.SensitiveColumn{
						{Table: "sales.order", Column: "card", Class: plan.ClassFinancial, Action: plan.ActionHash},
					}},
				{Name: "people", Tables: []plan.TableAssignment{{Name: "public.customer"}},
					SensitiveColumns: []plan.SensitiveColumn{
						{Table: "public.customer", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask},
					}},
				{Name: "audit", Tables: []plan.TableAssignment{{Name: "logs.event"}}},
			},
		},
	}
	if err := plan.Approve(p, "reviewer", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	cr, err := kabrv1.DomainPlanFromPlan(p, "ns", "shop-db")
	if err != nil {
		t.Fatal(err)
	}
	return cr
}

func testDataSource() *kabrv1.DataSource {
	return &kabrv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-db", Namespace: "ns"},
		Spec: kabrv1.DataSourceSpec{
			Driver:           kabrv1.DriverPostgres,
			SecretRef:        kabrv1.DSNSecretRef{Name: "shop-reader", Key: "uri"},
			HashKeySecretRef: &kabrv1.HashKeySecretRef{Name: "shop-hash"},
			Network: kabrv1.DataSourceNetwork{
				Port: 5432, CIDRs: []string{"10.0.12.0/24"},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "db"}},
			},
		},
	}
}

func testAPI(t *testing.T, cr *kabrv1.DomainPlan, domain string) *kabrv1.DomainAPI {
	t.Helper()
	name, err := DomainAPIName(cr.Name, domain)
	if err != nil {
		t.Fatal(err)
	}
	return &kabrv1.DomainAPI{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace},
		Spec:       DomainAPISpecFor(cr, cr.ToPlan().Domain(domain)),
	}
}

func TestDomainAPIName(t *testing.T) {
	if n, err := DomainAPIName("pagila", "film"); err != nil || n != "pagila-film" {
		t.Errorf("got %q %v", n, err)
	}
	for _, tt := range [][2]string{
		{strings.Repeat("p", 40), strings.Repeat("d", 30)}, // too long
		{"a.b", "film"},   // dot
		{"1plan", "film"}, // must start with a letter (Service name)
	} {
		if _, err := DomainAPIName(tt[0], tt[1]); err == nil {
			t.Errorf("DomainAPIName(%q, %q) accepted", tt[0], tt[1])
		}
	}
}

func TestDomainAPISpecFor(t *testing.T) {
	cr := testPlanCR(t)
	cr.Spec.Replicas = new(int32) // explicit 0
	spec := DomainAPISpecFor(cr, cr.ToPlan().Domain("sales"))
	if want := []string{"logs", "public", "sales"}; !slices.Equal(spec.Schemas, want) {
		t.Errorf("schemas = %v, want %v (all schemas of the plan)", spec.Schemas, want)
	}
	if spec.Replicas != 0 || spec.RowLimit != kabrv1.DefaultRowLimit ||
		spec.CatalogFingerprint != testFingerprint || spec.DataSourceRef.Name != "shop-db" || spec.PlanRef.Name != "shop" {
		t.Errorf("spec = %+v", spec)
	}
	if !slices.Equal(spec.Tables, []string{"sales.item", "sales.order"}) || len(spec.SensitiveColumns) != 1 {
		t.Errorf("tables %v sensitive %v", spec.Tables, spec.SensitiveColumns)
	}
}

func TestEncodeConfigHasNoSecrets(t *testing.T) {
	cr := testPlanCR(t)
	cfg, err := serve.ConfigFromPlan(cr.ToPlan(), "sales")
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := serve.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("round trip: %+v != %+v", got, cfg)
	}
	for _, bad := range []string{"postgres://", "password", "dsn", "shop-reader", "shop-hash"} {
		if strings.Contains(strings.ToLower(string(data)), bad) {
			t.Errorf("config contains %q:\n%s", bad, data)
		}
	}
	if ConfigChecksum(data) == ConfigChecksum(append(data, ' ')) || len(ConfigChecksum(data)) != 64 {
		t.Error("checksum must change with the config")
	}
	cfg.Domain = "Bad_Name"
	if _, err := EncodeConfig(cfg); err == nil {
		t.Error("EncodeConfig accepted a config the runtime would refuse")
	}
}

func TestRenderDeployment(t *testing.T) {
	cr := testPlanCR(t)
	api := testAPI(t, cr, "sales")
	api.Spec.Replicas = 3
	ds := testDataSource()
	dep := RenderDeployment(api, DeploymentInput{Image: "kabr:test", ConfigChecksum: "abc", DataSource: ds, MountHashKey: true})

	if *dep.Spec.Replicas != 3 || dep.Spec.Template.Annotations[AnnotationConfigChecksum] != "abc" {
		t.Errorf("replicas/annotation: %v %v", *dep.Spec.Replicas, dep.Spec.Template.Annotations)
	}
	if !reflect.DeepEqual(dep.Spec.Selector.MatchLabels, map[string]string{LabelDomainAPI: "shop-sales"}) {
		t.Errorf("selector = %v", dep.Spec.Selector.MatchLabels)
	}
	pod := dep.Spec.Template.Spec
	c := pod.Containers[0]
	wantArgs := []string{"serve", "--config", "/etc/kabr/domain.json", "--dsn-file", "/var/run/secrets/kabr/dsn",
		"--hash-key-file", "/var/run/secrets/kabr-hash/key", "--addr", ":8080"}
	if !slices.Equal(c.Args, wantArgs) || c.Image != "kabr:test" || len(c.Env) != 0 || len(c.EnvFrom) != 0 {
		t.Errorf("args %v image %q env %v", c.Args, c.Image, c.Env)
	}
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v
	}
	if s := vols[dsnVolume].Secret; s == nil || s.SecretName != "shop-reader" || *s.DefaultMode != 0o400 ||
		!reflect.DeepEqual(s.Items, []corev1.KeyToPath{{Key: "uri", Path: "dsn"}}) {
		t.Errorf("dsn volume = %+v", vols[dsnVolume])
	}
	if s := vols[hashKeyVolume].Secret; s == nil || s.SecretName != "shop-hash" || s.Items[0].Key != "key" || *s.DefaultMode != 0o400 {
		t.Errorf("hash key volume = %+v", vols[hashKeyVolume])
	}
	if cm := vols[configVolume].ConfigMap; cm == nil || cm.Name != "shop-sales-config" {
		t.Errorf("config volume = %+v", vols[configVolume])
	}
	for _, m := range c.VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is writable", m.Name)
		}
	}
	if c.LivenessProbe.HTTPGet.Path != "/healthz" || c.ReadinessProbe.HTTPGet.Path != "/readyz" ||
		c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("probes/ports: %+v %+v %+v", c.LivenessProbe, c.ReadinessProbe, c.Ports)
	}
	psc, sc := pod.SecurityContext, c.SecurityContext
	if !*psc.RunAsNonRoot || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		!*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		*pod.AutomountServiceAccountToken || *pod.TerminationGracePeriodSeconds < 20 {
		t.Errorf("security: pod %+v container %+v", psc, sc)
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Cpu().IsZero() {
		t.Error("resources not set")
	}

	// Without hashed columns the hash key is not mounted at all.
	dep = RenderDeployment(api, DeploymentInput{Image: "kabr:test", DataSource: ds})
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == hashKeyVolume {
			t.Error("hash key mounted although no column is hashed")
		}
	}
	if slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--hash-key-file") {
		t.Error("--hash-key-file without a hash key")
	}
}

func TestRenderServiceAndNetworkPolicy(t *testing.T) {
	cr := testPlanCR(t)
	api := testAPI(t, cr, "people")
	svc := RenderService(api)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[0].TargetPort.StrVal != "http" ||
		svc.Spec.Selector[LabelDomainAPI] != api.Name {
		t.Errorf("service = %+v", svc.Spec)
	}
	if ServiceURL(svc.Name, "ns") != "http://shop-people.ns.svc:8080" {
		t.Error(ServiceURL(svc.Name, "ns"))
	}

	np := RenderNetworkPolicy(api, testDataSource())
	checkNetworkPolicy(t, np, api.Name, 5432, 2)

	// CIDRs only.
	ds := testDataSource()
	ds.Spec.Network.PodSelector, ds.Spec.Network.NamespaceSelector = nil, nil
	checkNetworkPolicy(t, RenderNetworkPolicy(api, ds), api.Name, 5432, 1)
}

// checkNetworkPolicy asserts the shape every DomainAPI NetworkPolicy must
// have: own pods only, ingress TCP 8080 only, egress DNS + DB port only.
func checkNetworkPolicy(t *testing.T, np *networkingv1.NetworkPolicy, name string, dbPort int32, dbPeers int) {
	t.Helper()
	s := np.Spec
	if !reflect.DeepEqual(s.PodSelector.MatchLabels, map[string]string{LabelDomainAPI: name}) {
		t.Errorf("podSelector = %v", s.PodSelector)
	}
	if !slices.Equal(s.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) {
		t.Errorf("policyTypes = %v", s.PolicyTypes)
	}
	if len(s.Ingress) != 1 || len(s.Ingress[0].Ports) != 1 || s.Ingress[0].Ports[0].Port.IntVal != 8080 ||
		*s.Ingress[0].Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("ingress = %+v", s.Ingress)
	}
	if len(s.Egress) != 2 {
		t.Fatalf("egress rules = %+v", s.Egress)
	}
	dns, db := s.Egress[0], s.Egress[1]
	if len(dns.To) != 1 || dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" ||
		dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" || len(dns.Ports) != 2 {
		t.Errorf("dns rule = %+v", dns)
	}
	for _, p := range dns.Ports {
		if p.Port.IntVal != 53 {
			t.Errorf("dns port %v", p.Port)
		}
	}
	if len(db.To) != dbPeers || len(db.Ports) != 1 || db.Ports[0].Port.IntVal != dbPort || *db.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("db rule = %+v", db)
	}
	for _, r := range s.Egress {
		if len(r.To) == 0 || len(r.Ports) == 0 {
			t.Errorf("egress rule without peers or ports allows too much: %+v", r)
		}
	}
}

func TestDeploymentReadiness(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	if ok, reason, _ := deploymentReadiness(dep, 0); !ok || reason != ReasonScaledToZero {
		t.Errorf("scaled to zero: %v %s", ok, reason)
	}
	if ok, reason, _ := deploymentReadiness(dep, 2); ok || reason != ReasonProgressing {
		t.Errorf("new deployment: %v %s", ok, reason)
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}
	if ok, reason, _ := deploymentReadiness(dep, 2); !ok || reason != ReasonAvailable {
		t.Errorf("available: %v %s", ok, reason)
	}
	dep.Status.ObservedGeneration = 1
	if ok, _, _ := deploymentReadiness(dep, 2); ok {
		t.Error("stale observedGeneration counted as ready")
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Message: "deadline exceeded"},
	}}
	if ok, reason, msg := deploymentReadiness(dep, 1); ok || reason != ReasonDegraded || !strings.Contains(msg, "deadline exceeded") {
		t.Errorf("degraded: %v %s %s", ok, reason, msg)
	}
}

func TestApprovalReasonAndVerifyPlan(t *testing.T) {
	cr := testPlanCR(t)
	if err := verifyPlan(cr); err != nil {
		t.Fatal(err)
	}
	edited := cr.DeepCopy()
	edited.Spec.Domains[1].SensitiveColumns = nil
	forged := cr.DeepCopy()
	forged.Spec.Approval.Digest = "sha256:" + strings.Repeat("0", 64)
	noDigest := cr.DeepCopy()
	noDigest.Spec.Approval.Digest = ""
	none := cr.DeepCopy()
	none.Spec.Approval = nil
	badName := cr.DeepCopy()
	badName.Name = "Bad_Name"
	for _, tt := range []struct {
		cr   *kabrv1.DomainPlan
		want string
	}{
		{edited, ReasonEditedAfterApproval}, {forged, ReasonEditedAfterApproval},
		{noDigest, ReasonApprovalDigestMissing}, {none, ReasonNotApproved}, {badName, ReasonInvalidPlan},
	} {
		err := verifyPlan(tt.cr)
		if err == nil || approvalReason(err) != tt.want {
			t.Errorf("%s: got %v (%s)", tt.want, err, approvalReason(err))
		}
	}
}

func TestValidateDataSourceSpec(t *testing.T) {
	if r, _ := validateDataSourceSpec(&testDataSource().Spec); r != "" {
		t.Errorf("valid spec rejected: %s", r)
	}
	for name, mut := range map[string]func(*kabrv1.DataSourceSpec){
		"driver": func(s *kabrv1.DataSourceSpec) { s.Driver = "mysql" },
		"secret": func(s *kabrv1.DataSourceSpec) { s.SecretRef.Name = "" },
		"port":   func(s *kabrv1.DataSourceSpec) { s.Network.Port = 0 },
		"no peers": func(s *kabrv1.DataSourceSpec) {
			s.Network.CIDRs, s.Network.PodSelector, s.Network.NamespaceSelector = nil, nil, nil
		},
		"ns selector": func(s *kabrv1.DataSourceSpec) { s.Network.PodSelector = nil },
		"hash name":   func(s *kabrv1.DataSourceSpec) { s.HashKeySecretRef.Name = "" },
		"cidr /0 v4":  func(s *kabrv1.DataSourceSpec) { s.Network.CIDRs = []string{"10.0.0.0/8", "0.0.0.0/0"} },
		"cidr /0 v6":  func(s *kabrv1.DataSourceSpec) { s.Network.CIDRs = []string{"::/0"} },
		"bad cidr":    func(s *kabrv1.DataSourceSpec) { s.Network.CIDRs = []string{"10.0.0.300/24"} },
	} {
		s := testDataSource().Spec
		mut(&s)
		if r, _ := validateDataSourceSpec(&s); r != ReasonInvalidSpec {
			t.Errorf("%s: reason %q", name, r)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("é", 10), 8); len(got) > 8 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q", got)
	}
	if truncate("short", 10) != "short" {
		t.Error("short message changed")
	}
}

// The hash key is mounted only when the runtime will hash a column: kabr
// serve drops a credential whatever its action, so a credential with action
// hash must not get the key mounted.
func TestUsesHash(t *testing.T) {
	cases := []struct {
		name string
		cols []plan.SensitiveColumn
		want bool
	}{
		{"none", nil, false},
		{"mask only", []plan.SensitiveColumn{{Class: plan.ClassPII, Action: plan.ActionMask}}, false},
		{"pii hash", []plan.SensitiveColumn{{Class: plan.ClassPII, Action: plan.ActionHash}}, true},
		{"credential hash", []plan.SensitiveColumn{{Class: plan.ClassCredential, Action: plan.ActionHash}}, false},
		{"credential hash and financial hash", []plan.SensitiveColumn{
			{Class: plan.ClassCredential, Action: plan.ActionHash},
			{Class: plan.ClassFinancial, Action: plan.ActionHash},
		}, true},
	}
	for _, c := range cases {
		if got := usesHash(&serve.Config{SensitiveColumns: c.cols}); got != c.want {
			t.Errorf("%s: usesHash = %t, want %t", c.name, got, c.want)
		}
	}
}
