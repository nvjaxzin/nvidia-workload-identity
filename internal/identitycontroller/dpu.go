// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identitycontroller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"dsx-identity/internal/identity"
	"dsx-identity/internal/spireidentity"
)

const (
	dpuMachineIDLabel = "carbide.nvidia.com/dpu-machine-id"
	dpuPluginName     = "dpu_hw"
	maxDPUSerialLen   = 64
)

// DPUGVK identifies the externally owned hardware inventory source.
var DPUGVK = schema.GroupVersionKind{
	Group:   "provisioning.dpu.nvidia.com",
	Version: "v1alpha1",
	Kind:    "DPU",
}

// DPUReconciler maps each read-only DPU source to its paired BM entry. The
// machine ID label is the BM resource ID and OperatorTenant is its tenant. The
// parent agent is /spire/agent/dpu_hw/<serial>, matching the DPF-provisioned
// NodeAttestor. Configured Kubernetes selectors identify the IMDS container
// that calls the Workload API through SPIFFE CSI.
type DPUReconciler struct {
	client.Client
	Config   Config
	Recorder record.EventRecorder
}

func (r *DPUReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("dsx-dpu-identity").
		Watches(&spirev1alpha1.ClusterStaticEntry{}, handler.EnqueueRequestsFromMapFunc(requestForSource(DPUGVK.Group, DPUGVK.Kind))).
		Build(r)
	if err != nil {
		return err
	}
	if err := watchSourceWhenAvailable(mgr, c, DPUGVK, &handler.EnqueueRequestForObject{}, r.Config.SourceAPIPollInterval); err != nil {
		return err
	}
	return watchSourceWhenAvailable(mgr, c, BareMetalInstanceGVK, handler.EnqueueRequestsFromMapFunc(r.requestsForDPU), r.Config.SourceAPIPollInterval)
}

func (r *DPUReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	return reconcileSource(ctx, r.Client, r.Recorder, request, DPUGVK, r.desiredEntry)
}

func (r *DPUReconciler) desiredEntry(ctx context.Context, dpu *unstructured.Unstructured) (*spirev1alpha1.ClusterStaticEntry, error) {
	td, err := r.Config.trustDomain()
	if err != nil {
		return nil, err
	}
	machineID, err := dpuMachineID(dpu)
	if err != nil {
		return nil, err
	}
	rawSerial, found, err := unstructured.NestedString(dpu.Object, "spec", "serialNumber")
	if err != nil {
		return nil, fmt.Errorf("DPU spec.serialNumber: %w", err)
	}
	assigned, err := r.hasTenantBareMetalInstance(ctx, machineID)
	if err != nil {
		return nil, err
	}
	if assigned {
		return nil, nil
	}
	// Controller-managed DPU, VM, and bare-metal identities have no Pod service
	// label, so their service comes from global.dsx.defaultServiceName. td
	// supplies the trust domain and r.Config.Service supplies /service/<service>.
	spiffeID, err := spireidentity.ID(identity.Subject{
		Tenant:       r.Config.OperatorTenant,
		Service:      r.Config.Service,
		ResourceType: identity.ResourceBM,
		ResourceID:   machineID,
	}, td)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("DPU spec.serialNumber is required")
	}
	serial, err := normalizeDPUSerial(rawSerial)
	if err != nil {
		return nil, fmt.Errorf("DPU spec.serialNumber: %w", err)
	}
	// The paired DPU agent is attested under dpu_hw with this hardware serial.
	parentID, err := spiffeid.FromSegments(td, "spire", "agent", dpuPluginName, serial)
	if err != nil {
		return nil, fmt.Errorf("DPU parent ID: %w", err)
	}
	return newEntry(dpu, spiffeID, parentID, "dsx-imds", r.Config.ClassName, slices.Clone(r.Config.DPUMetadataSelectors))
}

// normalizeDPUSerial matches the DPF dpu_hw NodeAttestor identity contract.
func normalizeDPUSerial(raw string) (string, error) {
	serial := strings.ToLower(strings.TrimSpace(raw))
	if serial == "" {
		return "", fmt.Errorf("serial is empty after trimming")
	}
	if len(serial) > maxDPUSerialLen {
		return "", fmt.Errorf("serial length %d exceeds maximum %d", len(serial), maxDPUSerialLen)
	}
	for _, r := range serial {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)) {
			return "", fmt.Errorf("serial contains invalid character %q (allowed: RFC 3986 unreserved)", r)
		}
	}
	return serial, nil
}

func (r *DPUReconciler) hasTenantBareMetalInstance(ctx context.Context, dpuMachineID string) (bool, error) {
	// A complete tenant assignment suppresses the operator fallback even when
	// its optional workload role is invalid.
	instances := &unstructured.UnstructuredList{}
	instances.SetGroupVersionKind(BareMetalInstanceGVK.GroupVersion().WithKind(BareMetalInstanceGVK.Kind + "List"))
	if err := r.List(ctx, instances); err != nil {
		if meta.IsNoMatchError(err) {
			available, discoveryErr := sourceAPIAvailable(r.RESTMapper(), BareMetalInstanceGVK)
			if discoveryErr != nil {
				return false, fmt.Errorf("discover BareMetalInstance API: %w", discoveryErr)
			}
			if !available {
				return false, nil
			}
		}
		return false, fmt.Errorf("list BareMetalInstances: %w", err)
	}
	for i := range instances.Items {
		instance := &instances.Items[i]
		if !instance.GetDeletionTimestamp().IsZero() {
			continue
		}
		machineID, found, err := unstructured.NestedString(instance.Object, "status", "primary_dpu", "machine_id")
		if err != nil {
			return false, fmt.Errorf("BareMetalInstance status.primary_dpu.machine_id: %w", err)
		}
		if found && machineID == dpuMachineID {
			// Role validity controls workload registration, not hardware assignment.
			assignment := instance.DeepCopy()
			annotations := assignment.GetAnnotations()
			delete(annotations, RoleAnnotation)
			assignment.SetAnnotations(annotations)
			if _, err := (&LaunchLayerReconciler{Config: r.Config}).bareMetalInstanceEntry(assignment); err == nil {
				return true, nil
			}
		}
	}
	return false, nil
}

func dpuMachineID(dpu *unstructured.Unstructured) (string, error) {
	machineID := dpu.GetLabels()[dpuMachineIDLabel]
	if machineID == "" {
		return "", fmt.Errorf("DPU label %q is required", dpuMachineIDLabel)
	}
	if !identity.ValidResourceID(machineID) {
		return "", fmt.Errorf("invalid DPU machine ID %q", machineID)
	}
	return machineID, nil
}

func (r *DPUReconciler) requestsForDPU(ctx context.Context, source client.Object) []ctrl.Request {
	// BM assignment changes requeue the paired DPU so it can add or remove the
	// operator fallback identity.
	bm, ok := source.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	machineID, found, err := unstructured.NestedString(bm.Object, "status", "primary_dpu", "machine_id")
	if err != nil || !found || machineID == "" {
		return nil
	}
	dpus := &unstructured.UnstructuredList{}
	dpus.SetGroupVersionKind(DPUGVK.GroupVersion().WithKind(DPUGVK.Kind + "List"))
	if err := r.List(ctx, dpus, client.MatchingLabels{dpuMachineIDLabel: machineID}); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(dpus.Items))
	for i := range dpus.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&dpus.Items[i])})
	}
	return requests
}
