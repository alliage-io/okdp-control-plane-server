package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	SparkAppAPIVersion = "sparkoperator.k8s.io/v1beta2"
	SparkAppKind       = "SparkApplication"
)

func GetSparkAppGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "sparkoperator.k8s.io",
		Version:  "v1beta2",
		Resource: "sparkapplications",
	}
}

type SparkApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SparkApplicationSpec   `json:"spec,omitempty"`
	Status SparkApplicationStatus `json:"status,omitempty"`
}

type SparkApplicationSpec struct {
	Type                string            `json:"type"`
	Mode                string            `json:"mode"`
	Image               string            `json:"image"`
	MainClass           string            `json:"mainClass,omitempty"`
	MainApplicationFile string            `json:"mainApplicationFile,omitempty"`
	Arguments           []string          `json:"arguments,omitempty"`
	SparkVersion        string            `json:"sparkVersion,omitempty"`
	SparkConf           map[string]string `json:"sparkConf,omitempty"`
	Driver              SparkPodSpec      `json:"driver,omitempty"`
	Executor            ExecutorSpec      `json:"executor,omitempty"`
	RestartPolicy       RestartPolicy     `json:"restartPolicy,omitempty"`
	Deps                Dependencies      `json:"deps,omitempty"`
	TimeToLiveSeconds   *int64            `json:"timeToLiveSeconds,omitempty"`
}

type SparkPodSpec struct {
	Cores          *int32            `json:"cores,omitempty"`
	Memory         string            `json:"memory,omitempty"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

type ExecutorSpec struct {
	SparkPodSpec `json:",inline"`
	Instances    *int32 `json:"instances,omitempty"`
}

type RestartPolicy struct {
	Type string `json:"type,omitempty"`
}

type Dependencies struct {
	Jars    []string `json:"jars,omitempty"`
	PyFiles []string `json:"pyFiles,omitempty"`
	Files   []string `json:"files,omitempty"`
}

type SparkApplicationStatus struct {
	AppState           ApplicationState  `json:"applicationState,omitempty"`
	ExecutorState      map[string]string `json:"executorState,omitempty"`
	DriverInfo         DriverInfo        `json:"driverInfo,omitempty"`
	SparkApplicationID string            `json:"sparkApplicationId,omitempty"`
}

type ApplicationState struct {
	State        string `json:"state,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type DriverInfo struct {
	PodName          string `json:"podName,omitempty"`
	WebUIAddress     string `json:"webUIAddress,omitempty"`
	WebUIPort        int32  `json:"webUIPort,omitempty"`
	WebUIServiceName string `json:"webUIServiceName,omitempty"`
}
