package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

// Paths and names inside the DomainAPI pods (the "kabr serve" contract).
const (
	// HTTPPort is the port of "kabr serve" and of the Service.
	HTTPPort int32 = 8080
	// HTTPPortName names the container and Service port.
	HTTPPortName = "http"
	// ConfigFileName is the ConfigMap key and file name of the domain config.
	ConfigFileName = "domain.json"

	configDir       = "/etc/kabr"
	dsnDir          = "/var/run/secrets/kabr"
	hashKeyDir      = "/var/run/secrets/kabr-hash"
	dsnFile         = "dsn"
	hashKeyFile     = "key"
	containerName   = "api"
	configVolume    = "config"
	dsnVolume       = "dsn"
	hashKeyVolume   = "hash-key"
	configMapSuffix = "-config"

	// runtimeUID is the non-root user (and group) of the runtime pods; it
	// matches the distroless "nonroot" user of the kabr image. fsGroup makes
	// the 0400 Secret files readable by it (the kubelet adds group read).
	runtimeUID int64 = 65532
	// terminationGracePeriod covers the runtime's 15s graceful drain.
	terminationGracePeriod int64 = 30
)

// DomainAPIName is the name of the DomainAPI of domain in plan:
// "<plan>-<domain>". It must be a DNS-1035 label (it is also the name of a
// Service), at most 63 characters.
func DomainAPIName(planName, domain string) (string, error) {
	name := planName + "-" + domain
	if errs := validation.IsDNS1035Label(name); len(errs) > 0 {
		return "", fmt.Errorf("DomainAPI name %q for domain %q is not a valid DNS-1035 label: %s",
			name, domain, strings.Join(errs, "; "))
	}
	return name, nil
}

// PlanSchemas returns the sorted, unique schemas of every table of the plan.
func PlanSchemas(s *plan.Spec) []string {
	var out []string
	for _, d := range s.Domains {
		for _, t := range d.Tables {
			if sch, _, ok := strings.Cut(t.Name, "."); ok && sch != "" && !slices.Contains(out, sch) {
				out = append(out, sch)
			}
		}
	}
	slices.Sort(out)
	return out
}

// DomainAPISpecFor returns the DomainAPI spec of domain d of the (verified)
// plan cr. rowLimit and replicas are always set explicitly from the plan's
// effective values.
func DomainAPISpecFor(cr *kabrv1.DomainPlan, d *plan.Domain) kabrv1.DomainAPISpec {
	tables := make([]string, 0, len(d.Tables))
	for _, t := range d.Tables {
		tables = append(tables, t.Name)
	}
	return kabrv1.DomainAPISpec{
		Domain:             d.Name,
		PlanRef:            kabrv1.LocalObjectReference{Name: cr.Name},
		DataSourceRef:      cr.Spec.DataSourceRef,
		CatalogFingerprint: cr.Spec.Source.CatalogFingerprint,
		Tables:             tables,
		Schemas:            PlanSchemas(&cr.Spec.Spec),
		SensitiveColumns:   slices.Clone(d.SensitiveColumns),
		RowLimit:           cr.Spec.EffectiveRowLimit(),
		Replicas:           cr.Spec.EffectiveReplicas(),
	}
}

// childLabels are the labels of every object rendered for api.
func childLabels(api *kabrv1.DomainAPI) map[string]string {
	l := selectorLabels(api)
	l[LabelManagedBy] = ManagedByValue
	l[LabelName] = LabelNameValue
	l[LabelComponent] = ComponentServer
	l[LabelInstance] = api.Name
	l[LabelPlan] = api.Spec.PlanRef.Name
	l[LabelDomain] = api.Spec.Domain
	return l
}

// selectorLabels select the pods of api (Deployment selector, Service and
// NetworkPolicy). They never change for a given DomainAPI.
func selectorLabels(api *kabrv1.DomainAPI) map[string]string {
	return map[string]string{LabelDomainAPI: api.Name}
}

// ConfigMapName is the name of the ConfigMap of a DomainAPI.
func ConfigMapName(api string) string { return api + configMapSuffix }

// EncodeConfig renders cfg as the domain.json of the runtime. The result is
// decoded back with serve.DecodeConfig, so the operator never ships a config
// the runtime would refuse. The config holds no secret material: the DSN and
// the hash key are mounted from Secrets as files.
func EncodeConfig(cfg *serve.Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode domain config: %w", err)
	}
	data = append(data, '\n')
	if _, err := serve.DecodeConfig(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return data, nil
}

// ConfigChecksum is the hex SHA-256 of a rendered config.
func ConfigChecksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RenderConfigMap returns the desired ConfigMap of api holding data.
func RenderConfigMap(api *kabrv1.DomainAPI, data []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: ConfigMapName(api.Name), Namespace: api.Namespace, Labels: childLabels(api),
		},
		Data: map[string]string{ConfigFileName: string(data)},
	}
}

// usesHash reports whether any sensitive column of cfg is hashed.
func usesHash(cfg *serve.Config) bool {
	for _, sc := range cfg.SensitiveColumns {
		if sc.Action == plan.ActionHash {
			return true
		}
	}
	return false
}

// DeploymentInput is what [RenderDeployment] needs besides the DomainAPI.
type DeploymentInput struct {
	// Image is the operator's runtime image (never taken from the DomainAPI).
	Image          string
	ConfigChecksum string
	DataSource     *kabrv1.DataSource
	// MountHashKey mounts the DataSource's hash key Secret (only when the
	// domain hashes a column and the DataSource has one).
	MountHashKey bool
}

// RenderDeployment returns the desired Deployment of api.
func RenderDeployment(api *kabrv1.DomainAPI, in DeploymentInput) *appsv1.Deployment {
	ds := in.DataSource
	dsnKey := ds.Spec.SecretRef.Key
	if dsnKey == "" {
		dsnKey = kabrv1.DefaultDSNSecretKey
	}
	args := []string{"serve",
		"--config", configDir + "/" + ConfigFileName,
		"--dsn-file", dsnDir + "/" + dsnFile,
	}
	volumes := []corev1.Volume{
		{Name: configVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: ConfigMapName(api.Name)},
			Items:                []corev1.KeyToPath{{Key: ConfigFileName, Path: ConfigFileName}},
			DefaultMode:          ptr.To[int32](0o444),
		}}},
		{Name: dsnVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ds.Spec.SecretRef.Name,
			Items:       []corev1.KeyToPath{{Key: dsnKey, Path: dsnFile}},
			DefaultMode: ptr.To[int32](0o400),
		}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: configVolume, MountPath: configDir, ReadOnly: true},
		{Name: dsnVolume, MountPath: dsnDir, ReadOnly: true},
	}
	if h := ds.Spec.HashKeySecretRef; in.MountHashKey && h != nil {
		key := h.Key
		if key == "" {
			key = kabrv1.DefaultHashKeySecretKey
		}
		args = append(args, "--hash-key-file", hashKeyDir+"/"+hashKeyFile)
		volumes = append(volumes, corev1.Volume{Name: hashKeyVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  h.Name,
				Items:       []corev1.KeyToPath{{Key: key, Path: hashKeyFile}},
				DefaultMode: ptr.To[int32](0o400),
			}}})
		mounts = append(mounts, corev1.VolumeMount{Name: hashKeyVolume, MountPath: hashKeyDir, ReadOnly: true})
	}
	args = append(args, "--addr", fmt.Sprintf(":%d", HTTPPort))

	probe := func(path string, period, delay int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: path, Port: intstr.FromString(HTTPPortName), Scheme: corev1.URISchemeHTTP,
			}},
			InitialDelaySeconds: delay, PeriodSeconds: period, TimeoutSeconds: 2, FailureThreshold: 3, SuccessThreshold: 1,
		}
	}
	labels := childLabels(api)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(api.Spec.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(api)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{AnnotationConfigChecksum: in.ConfigChecksum},
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr.To(false),
					EnableServiceLinks:            ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To(terminationGracePeriod),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(runtimeUID),
						RunAsGroup:     ptr.To(runtimeUID),
						FSGroup:        ptr.To(runtimeUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            containerName,
						Image:           in.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            args,
						Ports: []corev1.ContainerPort{{
							Name: HTTPPortName, ContainerPort: HTTPPort, Protocol: corev1.ProtocolTCP,
						}},
						LivenessProbe:  probe("/healthz", 10, 5),
						ReadinessProbe: probe("/readyz", 5, 2),
						VolumeMounts:   mounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							ReadOnlyRootFilesystem:   ptr.To(true),
							AllowPrivilegeEscalation: ptr.To(false),
							RunAsNonRoot:             ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// RenderService returns the desired ClusterIP Service of api.
func RenderService(api *kabrv1.DomainAPI) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace, Labels: childLabels(api)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(api),
			Ports: []corev1.ServicePort{{
				Name: HTTPPortName, Port: HTTPPort, TargetPort: intstr.FromString(HTTPPortName), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// RenderNetworkPolicy returns the desired NetworkPolicy of api: it selects
// only this API's pods, allows ingress on TCP 8080 only (from any source:
// tightening ingress is planned for M4) and egress ONLY to the DataSource
// network on its TCP port plus cluster DNS (UDP and TCP 53 to kube-system
// pods labeled k8s-app=kube-dns).
func RenderNetworkPolicy(api *kabrv1.DomainAPI, ds *kabrv1.DataSource) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	net := ds.Spec.Network
	var dbPeers []networkingv1.NetworkPolicyPeer
	for _, c := range net.CIDRs {
		dbPeers = append(dbPeers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
	}
	if net.PodSelector != nil {
		dbPeers = append(dbPeers, networkingv1.NetworkPolicyPeer{
			PodSelector:       net.PodSelector.DeepCopy(),
			NamespaceSelector: net.NamespaceSelector.DeepCopy(),
		})
	}
	port := func(p corev1.Protocol, n int32) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: ptr.To(p), Port: ptr.To(intstr.FromInt32(n))}
	}
	egress := []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{port(udp, 53), port(tcp, 53)},
	}}
	// Without a peer the rule would allow every destination; the CRD and
	// validateDataSourceSpec require one, and this guard keeps it that way.
	if len(dbPeers) > 0 {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: dbPeers, Ports: []networkingv1.NetworkPolicyPort{port(tcp, net.Port)},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace, Labels: childLabels(api)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorLabels(api)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{port(tcp, HTTPPort)},
			}},
			Egress: egress,
		},
	}
}

// ServiceURL is the in-cluster base URL of a DomainAPI's Service.
func ServiceURL(svc, ns string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", svc, ns, HTTPPort)
}

// deploymentReadiness maps a Deployment to the Ready condition of its
// DomainAPI.
func deploymentReadiness(dep *appsv1.Deployment, want int32) (ok bool, reason, msg string) {
	st := dep.Status
	if want == 0 {
		return true, ReasonScaledToZero, "spec.replicas is 0"
	}
	if st.ObservedGeneration >= dep.Generation && st.UpdatedReplicas == want && st.AvailableReplicas == want {
		return true, ReasonAvailable, fmt.Sprintf("%d/%d replicas available", st.AvailableReplicas, want)
	}
	msg = fmt.Sprintf("%d/%d replicas available", st.AvailableReplicas, want)
	reason = ReasonProgressing
	for _, c := range st.Conditions {
		switch {
		case c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue,
			c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse:
			reason = ReasonDegraded
			msg += ": " + c.Message
		case c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionFalse && c.Message != "":
			msg += ": " + c.Message
		}
	}
	return false, reason, msg
}
