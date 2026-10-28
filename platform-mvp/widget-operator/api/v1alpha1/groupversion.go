package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group/version these types belong to — must match
// deploy/platform-mvp/chart/spoke/templates/widget-operator.yaml's CRD group and version.
var GroupVersion = schema.GroupVersion{Group: "platform.example.com", Version: "v1alpha1"}

var (
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&Widget{}, &WidgetList{})
}
