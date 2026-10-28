package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WidgetSpec is the desired state of a Widget — a stand-in payload for
// whatever a real downstream integration would carry.
type WidgetSpec struct {
	Message string `json:"message,omitempty"`
}

// WidgetPhase is the lifecycle state of a Widget.
type WidgetPhase string

const (
	WidgetPhasePending WidgetPhase = "Pending"
	WidgetPhaseReady   WidgetPhase = "Ready"
)

// WidgetStatus is the observed state of a Widget.
type WidgetStatus struct {
	Phase    WidgetPhase `json:"phase,omitempty"`
	Endpoint string      `json:"endpoint,omitempty"`
}

// Widget is a minimal reconciled resource standing in for a real downstream
// integration's payload. See .claude/plans/eager-knitting-bentley.md Phase 2.
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WidgetSpec   `json:"spec,omitempty"`
	Status WidgetStatus `json:"status,omitempty"`
}

// WidgetList contains a list of Widget.
type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

func (in *Widget) DeepCopyInto(out *Widget) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

func (in *Widget) DeepCopy() *Widget {
	if in == nil {
		return nil
	}
	out := new(Widget)
	in.DeepCopyInto(out)
	return out
}

func (in *Widget) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *WidgetList) DeepCopyInto(out *WidgetList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		items := make([]Widget, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&items[i])
		}
		out.Items = items
	}
}

func (in *WidgetList) DeepCopy() *WidgetList {
	if in == nil {
		return nil
	}
	out := new(WidgetList)
	in.DeepCopyInto(out)
	return out
}

func (in *WidgetList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
