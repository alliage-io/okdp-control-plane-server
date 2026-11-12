package service

import (
	"context"
	"errors"
	"testing"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// sparkStore records the SparkApplications created.
type sparkStore struct {
	repository.SparkAppRepository
	created []*crd.SparkApplication
	stored  *crd.SparkApplication
	updated *crd.SparkApplication
}

func (f *sparkStore) Create(_ context.Context, _ string, app *crd.SparkApplication) error {
	f.created = append(f.created, app)
	return nil
}

func (f *sparkStore) Get(context.Context, string, string) (*crd.SparkApplication, error) {
	return f.stored, nil
}

func (f *sparkStore) Update(_ context.Context, _ string, app *crd.SparkApplication) error {
	f.updated = app
	return nil
}

type sparkPlatform struct {
	repository.PlatformRepository
	config *models.SparkConfig
}

func (p sparkPlatform) GetSparkConfig(context.Context) (*models.SparkConfig, error) {
	if p.config == nil {
		return nil, errors.New("sparkOperator not found in the platform values (global.okdp)")
	}
	return p.config, nil
}

func (sparkPlatform) GetIngressSuffix(context.Context) (string, error) { return "", nil }

// sparkTree is a tree whose project demo has a spark-rbac instance creating
// the ServiceAccount sa.
func sparkTree(sa string) *compiler.Tree {
	rbac := &compiler.TreeInstance{}
	rbac.Inputs.Namespace, rbac.Inputs.Release = "demo", "spark-rbac"
	rbac.Outcome.Result = &compiler.Result{Service: "spark-rbac", Values: map[string]map[string]any{
		"spark-rbac": {"serviceAccount": map[string]any{"create": true, "name": sa}},
	}}
	return &compiler.Tree{Instances: []*compiler.TreeInstance{rbac}}
}

func sparkUnderTest(tree *compiler.Tree, config *models.SparkConfig) (*DefaultSparkService, *sparkStore) {
	store := &sparkStore{}
	svc := NewDefaultSparkService(store, sparkPlatform{config: config}, k8sfake.NewClientset(), func(context.Context) (*compiler.Tree, error) { return tree, nil })
	return svc, store
}

func sparkPi() models.SparkAppRequest {
	return models.SparkAppRequest{
		Name: "spark-pi", Type: "Scala", Image: "quay.io/okdp/spark-py:spark-3.5.6",
		MainClass: "org.apache.spark.examples.SparkPi", SparkConf: map[string]string{"spark.sql.shuffle.partitions": "4"},
	}
}

func TestASparkApplicationRunsAsTheProjectsServiceAccount(t *testing.T) {
	svc, store := sparkUnderTest(sparkTree("spark-jobs"), nil)
	_, err := svc.SubmitApp(context.Background(), "demo", sparkPi())
	require.NoError(t, err)
	require.Len(t, store.created, 1)
	app := store.created[0]
	assert.Equal(t, "spark-jobs", app.Spec.Driver.ServiceAccount)
	assert.Equal(t, "spark-jobs", app.Spec.Executor.ServiceAccount)
	assert.Equal(t, "demo", app.Namespace)
	assert.Equal(t, "4", app.Spec.SparkConf["spark.sql.shuffle.partitions"])

	// A project without spark-rbac has no ServiceAccount to run as.
	svc, _ = sparkUnderTest(&compiler.Tree{}, nil)
	_, err = svc.SubmitApp(context.Background(), "demo", sparkPi())
	assert.True(t, IsValidationError(err), "%v", err)
}

func TestSparkConfKeysAreAllowListed(t *testing.T) {
	svc, store := sparkUnderTest(sparkTree("spark"), nil)
	for _, key := range []string{
		"spark.kubernetes.driver.volumes.hostPath.root.mount.path",
		"spark.kubernetes.executor.volumes.hostPath.root.options.path",
		"spark.kubernetes.authenticate.driver.serviceAccountName",
		"spark.kubernetes.driver.podTemplateFile",
		"spark.kubernetes.namespace",
		"spark.kubernetes.container.image",
		"spark.kubernetes.node.selector.node-role.kubernetes.io/control-plane",
		"spark.master",
		"some.other.key",
	} {
		req := sparkPi()
		req.SparkConf = map[string]string{key: "/"}
		_, err := svc.SubmitApp(context.Background(), "demo", req)
		assert.True(t, IsValidationError(err), "%s: %v", key, err)
	}
	assert.Empty(t, store.created)

	req := sparkPi()
	req.SparkConf = map[string]string{"spark.kubernetes.driver.label.team": "a", "spark.executor.memoryOverhead": "1g", "spark.hadoop.fs.s3a.endpoint": "http://s3"}
	_, err := svc.SubmitApp(context.Background(), "demo", req)
	require.NoError(t, err)
}

func TestTheImageIsOneThePlatformAllows(t *testing.T) {
	config := &models.SparkConfig{
		Image: models.SparkConfigImage{Registry: "quay.io/okdp"},
		Spark: models.SparkConfigSpark{Images: []models.SparkImage{{Label: "3.5", Image: "mirror.example/spark:3.5"}}},
	}
	svc, _ := sparkUnderTest(sparkTree("spark"), config)
	for image, ok := range map[string]bool{
		"quay.io/okdp/spark-py:spark-3.5.6": true,
		"mirror.example/spark:3.5":          true,
		"evil.example/root:latest":          false,
		"quay.io/okdpx/spark":               false,
	} {
		req := sparkPi()
		req.Image = image
		_, err := svc.SubmitApp(context.Background(), "demo", req)
		assert.Equal(t, ok, err == nil, "%s: %v", image, err)
	}
}

func TestAManifestIsReadIntoTheForm(t *testing.T) {
	svc, store := sparkUnderTest(sparkTree("spark"), nil)
	_, err := svc.SubmitAppYAML(context.Background(), "demo", models.SparkAppYAMLRequest{YAML: `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: spark-pi, namespace: demo}
spec:
  type: Scala
  mode: cluster
  image: quay.io/okdp/spark-py:spark-3.5.6
  imagePullPolicy: Always
  mainClass: org.apache.spark.examples.SparkPi
  mainApplicationFile: local:///opt/spark/examples/jars/spark-examples.jar
  sparkVersion: "3.5.6"
  restartPolicy: {type: Never}
  driver: {cores: 1, coreLimit: 1200m, memory: 512m, serviceAccount: spark}
  executor: {cores: 1, instances: 2, memory: 512m}
`})
	require.NoError(t, err)
	require.Len(t, store.created, 1)
	app := store.created[0]
	assert.Equal(t, "spark", app.Spec.Executor.ServiceAccount)
	assert.Equal(t, "1200m", app.Spec.Driver.CoreLimit)
	assert.Equal(t, "Never", app.Spec.RestartPolicy.Type)

	for name, manifest := range map[string]string{
		"hostPath volume": `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: escape}
spec:
  type: Scala
  image: quay.io/okdp/spark-py:spark-3.5.6
  volumes: [{name: root, hostPath: {path: /}}]
  driver: {volumeMounts: [{name: root, mountPath: /host}]}
`,
		"privileged": `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: escape}
spec:
  type: Scala
  image: quay.io/okdp/spark-py:spark-3.5.6
  driver: {securityContext: {privileged: true}}
`,
		"another ServiceAccount": `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: escape}
spec:
  type: Scala
  image: quay.io/okdp/spark-py:spark-3.5.6
  driver: {serviceAccount: argocd-application-controller}
`,
		"another namespace": `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: escape, namespace: kube-system}
spec: {type: Scala, image: quay.io/okdp/spark-py:spark-3.5.6}
`,
		"hostPath through sparkConf": `
apiVersion: sparkoperator.k8s.io/v1beta2
kind: SparkApplication
metadata: {name: escape}
spec:
  type: Scala
  image: quay.io/okdp/spark-py:spark-3.5.6
  sparkConf: {spark.kubernetes.driver.volumes.hostPath.root.mount.path: /host}
`,
	} {
		_, err := svc.SubmitAppYAML(context.Background(), "demo", models.SparkAppYAMLRequest{YAML: manifest})
		assert.True(t, IsValidationError(err), "%s: %v", name, err)
	}
	assert.Len(t, store.created, 1, "nothing refused was created")
}

func TestAnUpdateKeepsThePolicy(t *testing.T) {
	svc, store := sparkUnderTest(sparkTree("spark"), nil)
	store.stored = &crd.SparkApplication{Spec: crd.SparkApplicationSpec{
		Image:  "quay.io/okdp/spark-py:spark-3.5.6",
		Driver: crd.SparkPodSpec{ServiceAccount: "cluster-admin-sa"},
	}}
	_, err := svc.UpdateApp(context.Background(), "demo", "spark-pi", models.SparkAppUpdateRequest{SparkConf: map[string]string{"spark.kubernetes.driver.podTemplateFile": "/x"}})
	assert.True(t, IsValidationError(err), "%v", err)

	_, err = svc.UpdateApp(context.Background(), "demo", "spark-pi", models.SparkAppUpdateRequest{DriverMemory: "1g"})
	require.NoError(t, err)
	assert.Equal(t, "spark", store.updated.Spec.Driver.ServiceAccount, "the stored ServiceAccount is replaced")
}
