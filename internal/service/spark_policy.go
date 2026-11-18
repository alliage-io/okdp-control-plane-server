package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"

	compiler "github.com/okdp/okdp-compiler"
	"k8s.io/apimachinery/pkg/util/validation"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// A SparkApplication is built by the server from an allow-list, never
// passed through: its driver and executors run as the ServiceAccount of the
// project's spark-rbac instance, their image is one the platform allows,
// their sparkConf keys are Spark's own, none of the spark.kubernetes.* keys
// that mount volumes, pick a ServiceAccount or a pod template, or anything
// that names a host path or a security context.

// sparkRBACService is the catalog service whose instance creates the
// ServiceAccount the Spark jobs of its namespace run as (platform-charts
// spark-rbac, its parameter serviceAccountName, the chart value
// serviceAccount.name).
const sparkRBACService = "spark-rbac"

// sparkConfPrefixes are the sparkConf keys a request may set: Spark's own
// settings, not the ones of the Kubernetes scheduler backend.
var sparkConfPrefixes = []string{
	"spark.app.name", "spark.archives", "spark.broadcast.", "spark.checkpoint.",
	"spark.cleaner.", "spark.default.parallelism", "spark.driver.",
	"spark.dynamicAllocation.", "spark.eventLog.", "spark.executor.",
	"spark.executorEnv.", "spark.files", "spark.hadoop.", "spark.io.",
	"spark.jars", "spark.kryo.", "spark.kryoserializer.", "spark.locality.",
	"spark.memory.", "spark.network.", "spark.pyspark.", "spark.python.",
	"spark.r.", "spark.rdd.", "spark.redaction.", "spark.reducer.",
	"spark.rpc.", "spark.scheduler.", "spark.serializer", "spark.shuffle.",
	"spark.speculation", "spark.sql.", "spark.storage.", "spark.streaming.",
	"spark.submit.pyFiles", "spark.task.", "spark.ui.",
}

// sparkKubernetesKeys are the spark.kubernetes.* keys a request may set:
// labels, resources and the allocation of the executors.
var sparkKubernetesKeys = []string{
	"spark.kubernetes.driver.label.", "spark.kubernetes.executor.label.",
	"spark.kubernetes.memoryOverheadFactor",
	"spark.kubernetes.driver.request.cores", "spark.kubernetes.driver.limit.cores",
	"spark.kubernetes.executor.request.cores", "spark.kubernetes.executor.limit.cores",
	"spark.kubernetes.allocation.batch.size", "spark.kubernetes.allocation.batch.delay",
	"spark.kubernetes.executor.deleteOnTermination",
}

// sparkConfRefused are words no sparkConf key may hold, whatever its
// prefix (compared in lower case).
var sparkConfRefused = []string{
	"volumes", "serviceaccount", "podtemplate", "hostpath", "securitycontext",
	"privileged", "hostnetwork", "hostpid", "hostipc", "capabilities",
	"nodeselector", "toleration", "authenticate", "spark.master",
	"spark.submit.deploymode", "spark.kubernetes.namespace",
	"spark.kubernetes.container.image", "spark.kubernetes.file.upload",
}

// checkSparkConf refuses the sparkConf keys out of the allow-list.
func checkSparkConf(conf map[string]string) error {
	var refused []string
	for key := range conf {
		if !sparkConfKeyAllowed(key) {
			refused = append(refused, key)
		}
	}
	if refused == nil {
		return nil
	}
	slices.Sort(refused)
	return invalid("sparkConf: the keys %s are not allowed: a request sets Spark's own settings, the server those of the pods (ServiceAccount, volumes, pod templates, security context)", strings.Join(refused, ", "))
}

func sparkConfKeyAllowed(key string) bool {
	lower := strings.ToLower(key)
	for _, word := range sparkConfRefused {
		if strings.Contains(lower, strings.ToLower(word)) {
			return false
		}
	}
	for _, p := range sparkKubernetesKeys {
		if key == p || (strings.HasSuffix(p, ".") && strings.HasPrefix(key, p)) {
			return true
		}
	}
	if strings.HasPrefix(key, "spark.kubernetes.") {
		return false
	}
	for _, p := range sparkConfPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// sparkTypes are the application types of the operator.
var sparkTypes = []string{"Scala", "Java", "Python", "R"}

// sparkPolicy is what a project's SparkApplications run with.
type sparkPolicy struct {
	// serviceAccount is the ServiceAccount of the project's spark-rbac
	// instance.
	serviceAccount string
	// images and registry are the images the platform offers
	// (global.okdp.sparkOperator.spark.images) and its registry
	// (sparkOperator.image.registry); both empty, any image.
	images   []string
	registry string
}

// policy reads the policy of project: its spark-rbac instance in the
// compile of the tree, the images of the platform values.
func (s *DefaultSparkService) policy(ctx context.Context, project string) (*sparkPolicy, error) {
	p := &sparkPolicy{}
	if s.tree == nil {
		return nil, invalid("project %q: the ServiceAccount of its Spark jobs is unknown: no tree to read its %s instance from", project, sparkRBACService)
	}
	t, err := s.tree(ctx)
	if _, ok := errors.AsType[*compiler.LoadError](err); ok {
		t, err = &compiler.Tree{}, nil
	}
	if err != nil {
		return nil, err
	}
	p.serviceAccount = sparkServiceAccount(t, project)
	if p.serviceAccount == "" {
		return nil, invalid("project %q has no %s instance: its Spark jobs have no ServiceAccount to run as; deploy the %s service in the project first", project, sparkRBACService, sparkRBACService)
	}
	cfg, err := s.contextRepo.GetSparkConfig(ctx)
	if err != nil {
		slog.Debug("No sparkOperator in the platform values: the Spark images are not restricted", "error", err)
		return p, nil
	}
	p.registry = strings.TrimSuffix(cfg.Image.Registry, "/")
	for _, img := range cfg.Spark.Images {
		if img.Image != "" {
			p.images = append(p.images, img.Image)
		}
	}
	return p, nil
}

// sparkServiceAccount is the ServiceAccount the compiled spark-rbac
// instance of project creates (its chart value serviceAccount.name), ""
// when the project has none.
func sparkServiceAccount(t *compiler.Tree, project string) string {
	for _, ti := range t.Instances {
		r := ti.Outcome.Result
		if ti.Namespace() != project || r == nil || r.Service != sparkRBACService {
			continue
		}
		values, _ := r.Values[sparkRBACService]["serviceAccount"].(map[string]any)
		if name, _ := values["name"].(string); name != "" {
			return name
		}
	}
	return ""
}

// checkImage refuses an image the platform does not offer.
func (p *sparkPolicy) checkImage(image string) error {
	if image == "" {
		return invalid("image is required")
	}
	if len(p.images) == 0 && p.registry == "" {
		return nil
	}
	if slices.Contains(p.images, image) || (p.registry != "" && strings.HasPrefix(image, p.registry+"/")) {
		return nil
	}
	return invalid("image %q is not one the platform allows: one of global.okdp.sparkOperator.spark.images, or of the registry %q", image, p.registry)
}

// checkSparkRequest refuses a request out of the allow-list.
func (p *sparkPolicy) checkSparkRequest(req models.SparkAppRequest) error {
	if errs := validation.IsDNS1123Subdomain(req.Name); len(errs) > 0 {
		return invalid("invalid name %q: %s", req.Name, errs[0])
	}
	if !slices.Contains(sparkTypes, req.Type) {
		return invalid("type %q: one of %s", req.Type, strings.Join(sparkTypes, ", "))
	}
	if req.Mode != "" && req.Mode != "cluster" {
		return invalid("mode %q: only cluster", req.Mode)
	}
	if err := p.checkImage(req.Image); err != nil {
		return err
	}
	return checkSparkConf(req.SparkConf)
}

// sparkManifest is what a SparkApplication YAML may hold: the fields of the
// form (models.SparkAppRequest) and a few of the operator's that change
// nothing of the pods' privileges. Any other field is refused.
type sparkManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Type                string            `json:"type"`
		Mode                string            `json:"mode,omitempty"`
		Image               string            `json:"image"`
		ImagePullPolicy     string            `json:"imagePullPolicy,omitempty"`
		MainClass           string            `json:"mainClass,omitempty"`
		MainApplicationFile string            `json:"mainApplicationFile,omitempty"`
		Arguments           []string          `json:"arguments,omitempty"`
		SparkVersion        string            `json:"sparkVersion,omitempty"`
		SparkConf           map[string]string `json:"sparkConf,omitempty"`
		RestartPolicy       *struct {
			Type string `json:"type"`
		} `json:"restartPolicy,omitempty"`
		Deps *struct {
			Jars    []string `json:"jars,omitempty"`
			PyFiles []string `json:"pyFiles,omitempty"`
			Files   []string `json:"files,omitempty"`
		} `json:"deps,omitempty"`
		Driver   sparkManifestPod `json:"driver"`
		Executor struct {
			sparkManifestPod
			Instances *int32 `json:"instances,omitempty"`
		} `json:"executor"`
	} `json:"spec"`
}

type sparkManifestPod struct {
	Cores          *int32 `json:"cores,omitempty"`
	CoreLimit      string `json:"coreLimit,omitempty"`
	Memory         string `json:"memory,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
}

// parseSparkManifest reads a SparkApplication YAML into what the server
// builds from: its fields out of sparkManifest are refused, a namespace or
// a ServiceAccount other than the project's too.
func parseSparkManifest(raw, project, serviceAccount string) (*sparkManifest, error) {
	doc, err := k8syaml.YAMLToJSON([]byte(raw))
	if err != nil {
		return nil, invalid("invalid YAML: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var m sparkManifest
	if err := dec.Decode(&m); err != nil {
		return nil, invalid("the SparkApplication holds what the server does not build (%v): name, the form's fields, imagePullPolicy, restartPolicy, deps and the driver's coreLimit only", err)
	}
	if m.APIVersion != crd.SparkAppAPIVersion || m.Kind != crd.SparkAppKind {
		return nil, invalid("expected apiVersion %s kind %s, got %s %s", crd.SparkAppAPIVersion, crd.SparkAppKind, m.APIVersion, m.Kind)
	}
	if m.Metadata.Namespace != "" && m.Metadata.Namespace != project {
		return nil, invalid("metadata.namespace %q: a SparkApplication of project %q runs in its namespace", m.Metadata.Namespace, project)
	}
	for _, sa := range []string{m.Spec.Driver.ServiceAccount, m.Spec.Executor.ServiceAccount} {
		if sa != "" && sa != serviceAccount {
			return nil, invalid("serviceAccount %q: the Spark jobs of project %q run as %q", sa, project, serviceAccount)
		}
	}
	switch m.Spec.ImagePullPolicy {
	case "", "Always", "IfNotPresent", "Never":
	default:
		return nil, invalid("imagePullPolicy %q: Always, IfNotPresent or Never", m.Spec.ImagePullPolicy)
	}
	if m.Spec.RestartPolicy != nil {
		switch m.Spec.RestartPolicy.Type {
		case "", "Never", "OnFailure", "Always":
		default:
			return nil, invalid("restartPolicy.type %q: Never, OnFailure or Always", m.Spec.RestartPolicy.Type)
		}
	}
	return &m, nil
}

// request is the form's request of a manifest.
func (m *sparkManifest) request() models.SparkAppRequest {
	return models.SparkAppRequest{
		Name:                m.Metadata.Name,
		Type:                m.Spec.Type,
		Mode:                m.Spec.Mode,
		Image:               m.Spec.Image,
		MainClass:           m.Spec.MainClass,
		MainApplicationFile: m.Spec.MainApplicationFile,
		Arguments:           m.Spec.Arguments,
		SparkVersion:        m.Spec.SparkVersion,
		DriverCores:         m.Spec.Driver.Cores,
		DriverMemory:        m.Spec.Driver.Memory,
		ExecutorInstances:   m.Spec.Executor.Instances,
		ExecutorCores:       m.Spec.Executor.Cores,
		ExecutorMemory:      m.Spec.Executor.Memory,
		SparkConf:           m.Spec.SparkConf,
	}
}

// extras sets on app the manifest's fields the form has none of.
func (m *sparkManifest) extras(app *crd.SparkApplication) {
	app.Spec.ImagePullPolicy = m.Spec.ImagePullPolicy
	app.Spec.Driver.CoreLimit = m.Spec.Driver.CoreLimit
	app.Spec.Executor.CoreLimit = m.Spec.Executor.CoreLimit
	if m.Spec.RestartPolicy != nil {
		app.Spec.RestartPolicy.Type = m.Spec.RestartPolicy.Type
	}
	if m.Spec.Deps != nil {
		app.Spec.Deps = crd.Dependencies{Jars: m.Spec.Deps.Jars, PyFiles: m.Spec.Deps.PyFiles, Files: m.Spec.Deps.Files}
	}
}
