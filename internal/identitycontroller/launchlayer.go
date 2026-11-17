// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identitycontroller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"dsx-identity/internal/identity"
	"dsx-identity/internal/spireidentity"
)

const (
	OrgIDAnnotation = "launchlayer.nvidia.com/org-id"
	RoleAnnotation  = "launchlayer.nvidia.com/role"
)

var rolePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func launchLayerRolePath(source *unstructured.Unstructured) ([]string, error) {
	role := source.GetAnnotations()[RoleAnnotation]
	if role == "" {
		return nil, nil
	}
	if !rolePattern.MatchString(role) {
		return nil, fmt.Errorf("invalid workload role %q", role)
	}
	return []string{"role", role}, nil
}

var podUIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

var (
	VirtualInstanceGVK = schema.GroupVersionKind{
		Group: "launchlayer.nvidia.com", Version: "v1alpha1", Kind: "VirtualInstance",
	}
	BareMetalInstanceGVK = schema.GroupVersionKind{
		Group: "launchlayer.nvidia.com", Version: "v1alpha1", Kind: "BareMetalInstance",
	}
)

type LaunchLayerReconciler struct {
	client.Client
	Config    Config
	Recorder  record.EventRecorder
	SourceGVK schema.GroupVersionKind
}

func (r *LaunchLayerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	name, err := launchLayerControllerName(r.SourceGVK)
	if err != nil {
		return err
	}
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		Watches(&spirev1alpha1.ClusterStaticEntry{}, handler.EnqueueRequestsFromMapFunc(requestForSource(r.SourceGVK.Group, r.SourceGVK.Kind))).
		Build(r)
	if err != nil {
		return err
	}
	return watchSourceWhenAvailable(mgr, c, r.SourceGVK, &handler.EnqueueRequestForObject{}, r.Config.SourceAPIPollInterval)
}

func (r *LaunchLayerReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	return reconcileSource(ctx, r.Client, r.Recorder, request, r.SourceGVK, r.desiredEntry)
}

func (r *LaunchLayerReconciler) desiredEntry(_ context.Context, source *unstructured.Unstructured) (*spirev1alpha1.ClusterStaticEntry, error) {
	switch source.GroupVersionKind() {
	case VirtualInstanceGVK:
		return r.virtualInstanceEntry(source)
	case BareMetalInstanceGVK:
		return r.bareMetalInstanceEntry(source)
	default:
		return nil, fmt.Errorf("unsupported LaunchLayer source %s", source.GroupVersionKind())
	}
}

type virtualInstanceSource struct {
	Spec struct {
		InstanceID string `json:"instance_id"`
		NodeName   string `json:"node_name"`
	} `json:"spec"`
	Status struct {
		PodUID string `json:"pod_uid"`
	} `json:"status"`
}

func (r *LaunchLayerReconciler) virtualInstanceEntry(source *unstructured.Unstructured) (*spirev1alpha1.ClusterStaticEntry, error) {
	var instance virtualInstanceSource
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(source.Object, &instance); err != nil {
		return nil, fmt.Errorf("VirtualInstance: %w", err)
	}
	if instance.Spec.InstanceID == "" {
		return nil, errors.New("VirtualInstance spec.instance_id is required")
	}
	if !identity.ValidResourceID(instance.Spec.NodeName) {
		return nil, fmt.Errorf("invalid VirtualInstance spec.node_name %q", instance.Spec.NodeName)
	}
	if !podUIDPattern.MatchString(instance.Status.PodUID) {
		return nil, fmt.Errorf("VirtualInstance status.pod_uid %q is not a canonical UUID", instance.Status.PodUID)
	}

	rolePath, err := launchLayerRolePath(source)
	if err != nil {
		return nil, err
	}
	td, err := r.Config.trustDomain()
	if err != nil {
		return nil, err
	}
	// Controller-managed DPU, VM, and bare-metal identities have no Pod service
	// label, so their service comes from global.dsx.defaultServiceName. td
	// supplies the trust domain and r.Config.Service supplies /service/<service>.
	spiffeID, err := spireidentity.ID(identity.Subject{
		Tenant:       source.GetAnnotations()[OrgIDAnnotation],
		Service:      r.Config.Service,
		ResourceType: identity.ResourceVM,
		ResourceID:   instance.Spec.InstanceID,
		Path:         rolePath,
	}, td)
	if err != nil {
		return nil, err
	}
	// The virt-launcher Pod uses the worker's dsx_imds-attested host agent.
	parentID, err := spiffeid.FromSegments(td,
		"spire", "agent", "dsx_imds", "tenant", r.Config.OperatorTenant, "bm", instance.Spec.NodeName)
	if err != nil {
		return nil, fmt.Errorf("host parent ID: %w", err)
	}
	// Pod UID binds the entry to this virt-launcher instance across name reuse.
	return newEntry(source, spiffeID, parentID, "dsx-imds-vm", r.Config.ClassName,
		[]string{"k8s:pod-uid:" + instance.Status.PodUID})
}

type bareMetalInstanceSource struct {
	Spec struct {
		InstanceID string `json:"instance_id"`
	} `json:"spec"`
	Status struct {
		PrimaryDPU *struct {
			MachineID    string `json:"machine_id"`
			SerialNumber string `json:"serial_number"`
		} `json:"primary_dpu"`
	} `json:"status"`
}

func (r *LaunchLayerReconciler) bareMetalInstanceEntry(source *unstructured.Unstructured) (*spirev1alpha1.ClusterStaticEntry, error) {
	var instance bareMetalInstanceSource
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(source.Object, &instance); err != nil {
		return nil, fmt.Errorf("BareMetalInstance: %w", err)
	}
	if instance.Spec.InstanceID == "" {
		return nil, errors.New("BareMetalInstance spec.instance_id is required")
	}
	if instance.Status.PrimaryDPU == nil {
		return nil, errors.New("BareMetalInstance status.primary_dpu is required")
	}
	if !identity.ValidResourceID(instance.Status.PrimaryDPU.MachineID) {
		return nil, fmt.Errorf("invalid BareMetalInstance status.primary_dpu.machine_id %q", instance.Status.PrimaryDPU.MachineID)
	}
	serial, err := normalizeDPUSerial(instance.Status.PrimaryDPU.SerialNumber)
	if err != nil {
		return nil, fmt.Errorf("BareMetalInstance status.primary_dpu.serial_number: %w", err)
	}

	rolePath, err := launchLayerRolePath(source)
	if err != nil {
		return nil, err
	}
	td, err := r.Config.trustDomain()
	if err != nil {
		return nil, err
	}
	// Controller-managed DPU, VM, and bare-metal identities have no Pod service
	// label, so their service comes from global.dsx.defaultServiceName. td
	// supplies the trust domain and r.Config.Service supplies /service/<service>.
	spiffeID, err := spireidentity.ID(identity.Subject{
		Tenant:       source.GetAnnotations()[OrgIDAnnotation],
		Service:      r.Config.Service,
		ResourceType: identity.ResourceBM,
		ResourceID:   instance.Spec.InstanceID,
		Path:         rolePath,
	}, td)
	if err != nil {
		return nil, err
	}
	// The BM identity is served by its primary DPU's dpu_hw-attested agent.
	parentID, err := spiffeid.FromSegments(td, "spire", "agent", dpuPluginName, serial)
	if err != nil {
		return nil, fmt.Errorf("DPU parent ID: %w", err)
	}
	return newEntry(source, spiffeID, parentID, "dsx-imds", r.Config.ClassName,
		slices.Clone(r.Config.DPUMetadataSelectors))
}

func launchLayerControllerName(gvk schema.GroupVersionKind) (string, error) {
	switch gvk {
	case VirtualInstanceGVK:
		return "launch-layer-virtual-instance", nil
	case BareMetalInstanceGVK:
		return "launch-layer-bare-metal-instance", nil
	default:
		return "", fmt.Errorf("unsupported LaunchLayer source %s", gvk)
	}
}
