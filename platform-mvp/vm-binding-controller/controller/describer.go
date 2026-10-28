package controller

import (
	"context"
	"fmt"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

var clusterProfileGVK = schema.GroupVersionKind{Group: "multicluster.x-k8s.io", Version: "v1alpha1", Kind: "ClusterProfile"}

// ClusterDescriber supplies the hub-visible facts about the spoke serving a region.
type ClusterDescriber interface {
	Describe(ctx context.Context, region string, spoke cluster.Cluster) (domain.ClusterInfo, error)
}

// ProfileDescriber reads the region's ClusterProfile on the hub and asks the
// spoke API for its Kubernetes version. Profiles are read uncached, so the
// hub only needs get on clusterprofiles.
type ProfileDescriber struct {
	Hub       client.Reader
	Namespace string // namespace holding the ClusterProfiles
	// Version returns the spoke's Kubernetes version; nil uses API discovery.
	Version func(cluster.Cluster) (string, error)
}

func (d *ProfileDescriber) Describe(ctx context.Context, region string, spoke cluster.Cluster) (domain.ClusterInfo, error) {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(clusterProfileGVK)
	if err := d.Hub.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: region}, profile); err != nil {
		return domain.ClusterInfo{}, fmt.Errorf("getting ClusterProfile %s/%s: %w", d.Namespace, region, err)
	}
	version := d.Version
	if version == nil {
		version = serverVersion
	}
	v, err := version(spoke)
	if err != nil {
		return domain.ClusterInfo{}, err
	}
	return adapter.ClusterInfo(region, profile, v), nil
}

func serverVersion(spoke cluster.Cluster) (string, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(spoke.GetConfig())
	if err != nil {
		return "", fmt.Errorf("building discovery client: %w", err)
	}
	info, err := dc.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("reading spoke version: %w", err)
	}
	return info.GitVersion, nil
}
