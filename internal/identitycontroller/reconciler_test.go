// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identitycontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"dsx-identity/internal/dsxidentity"
	"dsx-identity/internal/spireidentity"
)

var testDPUMetadataSelectors = []string{
	"k8s:ns:dsx-imds",
	"k8s:sa:metadata-service",
	"k8s:container-name:metadata-service",
}

var testConfig = Config{
	TrustDomain:           "k2.sjc-1.nvidia.com",
	Service:               "dsx",
	OperatorTenant:        "operator-1",
	ClassName:             "dsx-identity-dsx-identity",
	DPUMetadataSelectors:  testDPUMetadataSelectors,
	SourceAPIPollInterval: time.Hour,
}

func TestSourceAPIAvailable(t *testing.T) {
	t.Parallel()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: BareMetalInstanceGVK.Group, Resource: "baremetalinstances"}, "", errors.New("denied"))
	discoveryFailure := errors.New("discovery unavailable")
	for _, test := range []struct {
		name        string
		version     string
		discoverErr error
		afterMiss   bool
		available   bool
	}{
		{name: "absent"},
		{name: "supported", version: "v1alpha1", available: true},
		{name: "unsupported installed version", version: "v1beta1"},
		{name: "discovery failure", discoverErr: discoveryFailure},
		{name: "authorization failure", discoverErr: forbidden},
		{name: "discovery failure after version miss", discoverErr: discoveryFailure, afterMiss: true},
		{name: "authorization failure after version miss", discoverErr: forbidden, afterMiss: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapper := launchLayerTestMapper(test.version)
			if test.discoverErr != nil {
				mapper = mappingErrorMapper{RESTMapper: mapper, err: test.discoverErr, afterMiss: test.afterMiss}
			}
			available, err := sourceAPIAvailable(mapper, BareMetalInstanceGVK)
			if available != test.available {
				t.Fatalf("available = %v, want %v", available, test.available)
			}
			switch {
			case test.discoverErr != nil:
				if !errors.Is(err, test.discoverErr) {
					t.Fatalf("error = %v, want %v", err, test.discoverErr)
				}
			case test.version == "v1beta1":
				if !meta.IsNoMatchError(err) {
					t.Fatalf("unsupported version error = %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDPUDesiredEntryFailsClosedOnAssignmentLookupErrors(t *testing.T) {
	t.Parallel()
	noMatch := &meta.NoKindMatchError{GroupKind: BareMetalInstanceGVK.GroupKind(), SearchedVersions: []string{BareMetalInstanceGVK.Version}}
	resource := schema.GroupResource{Group: BareMetalInstanceGVK.Group, Resource: "baremetalinstances"}
	forbidden := apierrors.NewForbidden(resource, "", errors.New("denied"))
	for _, test := range []struct {
		name        string
		version     string
		listErr     error
		discoverErr error
		assigned    bool
		wantEntry   bool
		wantErr     bool
	}{
		{name: "absent API", listErr: noMatch, wantEntry: true},
		{name: "unsupported installed version", version: "v1beta1", listErr: noMatch, wantErr: true},
		{name: "supported API with mapping failure", version: "v1alpha1", listErr: noMatch, wantErr: true},
		{name: "forbidden lookup", version: "v1alpha1", listErr: forbidden, wantErr: true},
		{name: "forbidden discovery after missing mapping", listErr: noMatch, discoverErr: forbidden, wantErr: true},
		{name: "unavailable discovery after missing mapping", listErr: noMatch, discoverErr: errors.New("discovery unavailable"), wantErr: true},
		{name: "HTTP not found is not API absence", listErr: apierrors.NewNotFound(resource, ""), wantErr: true},
		{name: "supported API without assignment", version: "v1alpha1", wantEntry: true},
		{name: "tenant assignment suppresses operator entry", version: "v1alpha1", assigned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dpu := &unstructured.Unstructured{}
			dpu.SetGroupVersionKind(DPUGVK)
			dpu.SetName("dpu-1")
			dpu.SetLabels(map[string]string{dpuMachineIDLabel: "machine-1"})
			setSourceSpecField(t, dpu, "serialNumber", "52TF43G001G")
			mapper := launchLayerTestMapper(test.version)
			if test.discoverErr != nil {
				mapper = mappingErrorMapper{RESTMapper: mapper, err: test.discoverErr}
			}
			builder := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper)
			if test.assigned {
				builder = builder.WithObjects(newBareMetalInstance("assigned-host", "tenant-system", "940863e9-1e6f-40ea-9104-c46a7b041ce5", "tenant-2", "instance-1", "machine-1", "52TF43G001G"))
			}
			c := builder.WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if test.listErr != nil {
						return test.listErr
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
			entry, err := (&DPUReconciler{Client: c, Config: testConfig}).desiredEntry(context.Background(), dpu)
			if (err != nil) != test.wantErr {
				t.Fatalf("desiredEntry error = %v, want error %v", err, test.wantErr)
			}
			if (entry != nil) != test.wantEntry {
				t.Fatalf("entry = %#v, want entry %v", entry, test.wantEntry)
			}
			if test.discoverErr != nil && !errors.Is(err, test.discoverErr) {
				t.Fatalf("discovery error = %v, want %v", err, test.discoverErr)
			}
			if entry != nil {
				assertEntry(t, entry, "spiffe://k2.sjc-1.nvidia.com/tenant/operator-1/service/dsx/bm/machine-1", "spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/52tf43g001g", "dsx-imds", testDPUMetadataSelectors)
			}
		})
	}
}

func launchLayerTestMapper(version string) meta.RESTMapper {
	gvk := BareMetalInstanceGVK
	gvk.Version = version
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gvk.GroupVersion()})
	if version != "" {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

type mappingErrorMapper struct {
	meta.RESTMapper
	err       error
	afterMiss bool
}

func (m mappingErrorMapper) RESTMapping(kind schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.afterMiss && len(versions) != 0 {
		return m.RESTMapper.RESTMapping(kind, versions...)
	}
	return nil, m.err
}

func TestConfigRejectsInvalidDPUMetadataSelectors(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		selectors []string
		want      string
	}{
		{name: "missing", want: "at least one DPU metadata selector is required"},
		{name: "empty", selectors: []string{""}, want: "DPU metadata selector must not be empty"},
		{name: "Unix", selectors: []string{"unix:path:/usr/local/bin/metadata-service"}, want: "must use the Kubernetes WorkloadAttestor"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig
			config.DPUMetadataSelectors = test.selectors
			if err := config.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestDPUReconcileCreatesPairedBMIdentityAndCleansUp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dpu := &unstructured.Unstructured{}
	dpu.SetGroupVersionKind(DPUGVK)
	dpu.SetName("dpu-1")
	dpu.SetUID(types.UID("f6e44986-13f5-4778-8424-1f0514a0f20c"))
	dpu.SetLabels(map[string]string{dpuMachineIDLabel: "machine-1"})
	setSourceSpecField(t, dpu, "serialNumber", "52TF43G001G")
	c := &applyClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dpu).Build()}
	r := &DPUReconciler{Client: c, Config: testConfig, Recorder: record.NewFakeRecorder(4)}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: dpu.GetName()}}
	if _, err := r.desiredEntry(ctx, dpu); err != nil {
		t.Fatal(err)
	}

	if result, err := r.Reconcile(ctx, request); err != nil || result.Requeue {
		t.Fatalf("DPU reconciliation = %#v, %v", result, err)
	}
	storedDPU := &unstructured.Unstructured{}
	storedDPU.SetGroupVersionKind(DPUGVK)
	if err := c.Get(ctx, request.NamespacedName, storedDPU); err != nil {
		t.Fatal(err)
	}
	if len(storedDPU.GetFinalizers()) != 0 {
		t.Fatal("DPU source was finalized by the identity controller")
	}
	entry := getEntry(t, c, childName(dpu))
	assertEntry(t, entry,
		"spiffe://k2.sjc-1.nvidia.com/tenant/operator-1/service/dsx/bm/machine-1",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/52tf43g001g",
		"dsx-imds",
		testDPUMetadataSelectors,
	)
	if len(entry.OwnerReferences) != 0 {
		t.Fatal("cluster-scoped entry must not have an owner reference")
	}
	if entry.Labels["identity.dsx.nvidia.com/source-uid"] != string(dpu.GetUID()) {
		t.Fatalf("unexpected source UID label: %q", entry.Labels["identity.dsx.nvidia.com/source-uid"])
	}
	assertSourceAnnotations(t, entry, DPUGVK.Group, DPUGVK.Kind, "", dpu.GetName())
	assertApplyCalls(t, c, 1)

	requests := requestForSource(DPUGVK.Group, DPUGVK.Kind)(ctx, entry)
	if len(requests) != 1 || requests[0] != request {
		t.Fatalf("child mapped to %#v, want %#v", requests, request)
	}
	if err := c.Delete(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, requests[0]); err != nil {
		t.Fatal(err)
	}
	entry = getEntry(t, c, childName(dpu))
	assertEntry(t, entry,
		"spiffe://k2.sjc-1.nvidia.com/tenant/operator-1/service/dsx/bm/machine-1",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/52tf43g001g",
		"dsx-imds",
		testDPUMetadataSelectors,
	)
	assertApplyCalls(t, c, 2)

	invalidAssignment := newBareMetalInstance("invalid-assignment", "tenant-system", "2bf2c1e4-b919-43dc-85f9-f46926a20fc7",
		"INVALID", "instance-1", "machine-1", "52TF43G001G")
	if err := c.Create(ctx, invalidAssignment); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	getEntry(t, c, childName(dpu))
	if err := c.Delete(ctx, invalidAssignment); err != nil {
		t.Fatal(err)
	}

	assigned := newBareMetalInstance("assigned-host", "tenant-system", "940863e9-1e6f-40ea-9104-c46a7b041ce5",
		"tenant-2", "instance-1", "machine-1", "52TF43G001G")
	if err := c.Create(ctx, assigned); err != nil {
		t.Fatal(err)
	}
	requests = r.requestsForDPU(ctx, assigned)
	if len(requests) != 1 || requests[0] != request {
		t.Fatalf("BareMetalInstance mapped to %#v, want %#v", requests, request)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: entry.Name}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("operator entry remained after tenant assignment: %v", err)
	}
	if err := c.Delete(ctx, assigned); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	entry = getEntry(t, c, childName(dpu))

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(DPUGVK)
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(ctx, request); err != nil || result.Requeue {
		t.Fatalf("DPU deletion reconciliation = %#v, %v", result, err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: entry.Name}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("entry was not deleted: %v", err)
	}
	deletedDPU := &unstructured.Unstructured{}
	deletedDPU.SetGroupVersionKind(DPUGVK)
	if err := c.Get(ctx, request.NamespacedName, deletedDPU); !apierrors.IsNotFound(err) {
		t.Fatalf("DPU source was not deleted: %v", err)
	}
}

func TestDPUDesiredEntryRequiresValidSource(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		machineID string
		serial    any
		want      string
	}{
		{name: "missing machine ID", serial: "52TF43G001G", want: dpuMachineIDLabel},
		{name: "invalid machine ID", machineID: "invalid/id", serial: "52TF43G001G", want: "invalid DPU machine ID"},
		{name: "missing serial", machineID: "machine-1", want: "spec.serialNumber is required"},
		{name: "wrong serial type", machineID: "machine-1", serial: int64(1), want: "spec.serialNumber"},
		{name: "invalid serial", machineID: "machine-1", serial: "invalid serial", want: "invalid character"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dpu := &unstructured.Unstructured{}
			dpu.SetGroupVersionKind(DPUGVK)
			dpu.SetName("dpu-1")
			if test.machineID != "" {
				dpu.SetLabels(map[string]string{dpuMachineIDLabel: test.machineID})
			}
			if test.serial != nil {
				setSourceSpecField(t, dpu, "serialNumber", test.serial)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
			_, err := (&DPUReconciler{Client: c, Config: testConfig}).desiredEntry(context.Background(), dpu)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("desiredEntry() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLaunchLayerReconcileLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	host := newBareMetalInstance("host-source", "tenant-system", "7ae7fe75-e671-4b0e-a76d-ef02bb5b040b",
		"tenant-2", "host-1", "dpu-1", "dpu-1")
	vm := newVirtualInstance("vm-source", "tenant-system", "e35e41ae-a112-4f64-8841-298c2a6662b0",
		"tenant-2", "vm-1", "host-1", "e35e41ae-a112-4f64-8841-298c2a6662b0")
	recorder := record.NewFakeRecorder(8)
	c := &applyClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(host, vm).Build()}
	hostReconciler := &LaunchLayerReconciler{
		Client: c, Config: testConfig, Recorder: recorder, SourceGVK: BareMetalInstanceGVK,
	}
	vmReconciler := &LaunchLayerReconciler{
		Client: c, Config: testConfig, Recorder: recorder, SourceGVK: VirtualInstanceGVK,
	}

	hostRequest := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(host)}
	vmRequest := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
	for _, reconcile := range []struct {
		reconciler *LaunchLayerReconciler
		request    ctrl.Request
	}{{hostReconciler, hostRequest}, {vmReconciler, vmRequest}} {
		if result, err := reconcile.reconciler.Reconcile(ctx, reconcile.request); err != nil || result.Requeue {
			t.Fatalf("LaunchLayer reconciliation = %#v, %v", result, err)
		}
	}
	for _, source := range []struct {
		gvk     schema.GroupVersionKind
		request ctrl.Request
	}{{BareMetalInstanceGVK, hostRequest}, {VirtualInstanceGVK, vmRequest}} {
		stored := &unstructured.Unstructured{}
		stored.SetGroupVersionKind(source.gvk)
		if err := c.Get(ctx, source.request.NamespacedName, stored); err != nil {
			t.Fatal(err)
		}
		if len(stored.GetFinalizers()) != 0 {
			t.Fatal("LaunchLayer source was finalized by the identity controller")
		}
	}

	hostEntry := getEntry(t, c, childName(host))
	assertEntry(t, hostEntry,
		"spiffe://k2.sjc-1.nvidia.com/tenant/tenant-2/service/dsx/bm/host-1",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/dpu-1",
		"dsx-imds",
		testDPUMetadataSelectors,
	)
	vmEntry := getEntry(t, c, childName(vm))
	assertEntry(t, vmEntry,
		"spiffe://k2.sjc-1.nvidia.com/tenant/tenant-2/service/dsx/vm/vm-1",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dsx_imds/tenant/operator-1/bm/host-1",
		"dsx-imds-vm",
		[]string{"k8s:pod-uid:e35e41ae-a112-4f64-8841-298c2a6662b0"},
	)
	assertSourceAnnotations(t, hostEntry, BareMetalInstanceGVK.Group, BareMetalInstanceGVK.Kind, host.GetNamespace(), host.GetName())
	assertSourceAnnotations(t, vmEntry, VirtualInstanceGVK.Group, VirtualInstanceGVK.Kind, vm.GetNamespace(), vm.GetName())
	assertApplyCalls(t, c, 2)
	invalidPodUID := vm.DeepCopy()
	if err := unstructured.SetNestedField(invalidPodUID.Object, "not-a-uid", "status", "pod_uid"); err != nil {
		t.Fatal(err)
	}
	if _, err := vmReconciler.desiredEntry(ctx, invalidPodUID); err == nil {
		t.Fatal("expected invalid Pod UID to be rejected")
	}
	hostEntry.Spec.ParentID = "spiffe://drift.invalid/spire/agent/wrong"
	if err := c.Update(ctx, hostEntry); err != nil {
		t.Fatal(err)
	}
	driftRequests := requestForSource(BareMetalInstanceGVK.Group, BareMetalInstanceGVK.Kind)(ctx, hostEntry)
	if len(driftRequests) != 1 || driftRequests[0] != hostRequest {
		t.Fatalf("drifted child mapped to %#v, want %#v", driftRequests, hostRequest)
	}
	if _, err := hostReconciler.Reconcile(ctx, driftRequests[0]); err != nil {
		t.Fatal(err)
	}
	hostEntry = getEntry(t, c, hostEntry.Name)
	assertEntry(t, hostEntry,
		"spiffe://k2.sjc-1.nvidia.com/tenant/tenant-2/service/dsx/bm/host-1",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/dpu-1",
		"dsx-imds",
		testDPUMetadataSelectors,
	)
	assertApplyCalls(t, c, 3)

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(BareMetalInstanceGVK)
	if err := c.Get(ctx, hostRequest.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	updated.SetAnnotations(map[string]string{OrgIDAnnotation: "tenant-3"})
	setSourceSpecField(t, updated, "instance_id", "host-2")
	if err := unstructured.SetNestedField(updated.Object, "dpu-2", "status", "primary_dpu", "machine_id"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(updated.Object, "dpu-2", "status", "primary_dpu", "serial_number"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := hostReconciler.Reconcile(ctx, hostRequest); err != nil {
		t.Fatal(err)
	}
	assertEntry(t, getEntry(t, c, hostEntry.Name),
		"spiffe://k2.sjc-1.nvidia.com/tenant/tenant-3/service/dsx/bm/host-2",
		"spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/dpu-2",
		"dsx-imds",
		testDPUMetadataSelectors,
	)
	assertApplyCalls(t, c, 4)

	if err := c.Get(ctx, hostRequest.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(updated.Object, "invalid serial", "status", "primary_dpu", "serial_number"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := hostReconciler.Reconcile(ctx, hostRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: hostEntry.Name}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid update retained stale entry: %v", err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning InvalidIdentity") {
			t.Fatalf("unexpected warning event: %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected invalid identity event")
	}

	deletingVM := &unstructured.Unstructured{}
	deletingVM.SetGroupVersionKind(VirtualInstanceGVK)
	if err := c.Get(ctx, vmRequest.NamespacedName, deletingVM); err != nil {
		t.Fatal(err)
	}
	c.childDeleteFailures = 1
	if err := c.Delete(ctx, deletingVM); err != nil {
		t.Fatal(err)
	}
	if _, err := vmReconciler.Reconcile(ctx, vmRequest); !errors.Is(err, errInjectedChildDelete) {
		t.Fatalf("first child deletion error = %v, want %v", err, errInjectedChildDelete)
	}
	if result, err := vmReconciler.Reconcile(ctx, vmRequest); err != nil || result.Requeue {
		t.Fatalf("LaunchLayer deletion reconciliation = %#v, %v", result, err)
	}
	if err := c.Get(ctx, vmRequest.NamespacedName, deletingVM); !apierrors.IsNotFound(err) {
		t.Fatalf("LaunchLayer source was not deleted: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: vmEntry.Name}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VM entry was not deleted: %v", err)
	}
}

func TestLaunchLayerConflictsRecoverAfterSourceCorrection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	vmA := newVirtualInstance("vm-a", "tenant-system", "3f94f124-72cd-467b-9b83-b5f536a8620d",
		"tenant-2", "vm-a", "worker-1", "1e605955-c4b7-4090-b12a-8fcf3b9f334d")
	vmB := newVirtualInstance("vm-b", "tenant-system", "db1f2eb8-7ec4-4a72-af0e-089f8d6dc427",
		"tenant-2", "vm-b", "worker-1", "1e605955-c4b7-4090-b12a-8fcf3b9f334d")
	vmC := newVirtualInstance("vm-c", "tenant-system", "93fc5f2d-5ba2-4f77-a67c-c8ac260cd70a",
		"tenant-2", "vm-b", "worker-1", "93fc5f2d-5ba2-4f77-a67c-c8ac260cd70a")
	c := &applyClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(vmA, vmB, vmC).Build()}
	r := &LaunchLayerReconciler{
		Client: c, Config: testConfig, Recorder: record.NewFakeRecorder(8), SourceGVK: VirtualInstanceGVK,
	}
	requestA := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vmA)}
	requestB := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vmB)}
	requestC := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vmC)}

	if _, err := r.Reconcile(ctx, requestA); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, requestB); err == nil || !strings.Contains(err.Error(), "Pod UID overlaps") {
		t.Fatalf("overlapping Pod UID error = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: childName(vmB)}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("conflicting Pod UID entry exists: %v", err)
	}

	updateSourceField(t, ctx, c, requestB.NamespacedName, VirtualInstanceGVK,
		"2e605955-c4b7-4090-b12a-8fcf3b9f334d", "status", "pod_uid")
	if _, err := r.Reconcile(ctx, requestB); err != nil {
		t.Fatalf("Pod UID conflict did not recover: %v", err)
	}
	getEntry(t, c, childName(vmB))

	if _, err := r.Reconcile(ctx, requestC); err == nil || !strings.Contains(err.Error(), "SPIFFE ID") {
		t.Fatalf("duplicate SPIFFE ID error = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: childName(vmC)}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
		t.Fatalf("duplicate SPIFFE ID entry exists: %v", err)
	}

	updateSourceField(t, ctx, c, requestC.NamespacedName, VirtualInstanceGVK,
		"vm-c", "spec", "instance_id")
	if _, err := r.Reconcile(ctx, requestC); err != nil {
		t.Fatalf("SPIFFE ID conflict did not recover: %v", err)
	}
	getEntry(t, c, childName(vmC))
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spirev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newVirtualInstance(name, namespace, uid, tenantID, instanceID, nodeName, podUID string) *unstructured.Unstructured {
	source := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"instance_id": instanceID, "node_name": nodeName},
		"status": map[string]any{"pod_uid": podUID},
	}}
	source.SetGroupVersionKind(VirtualInstanceGVK)
	source.SetName(name)
	source.SetNamespace(namespace)
	source.SetUID(types.UID(uid))
	source.SetAnnotations(map[string]string{OrgIDAnnotation: tenantID})
	return source
}

func newBareMetalInstance(name, namespace, uid, tenantID, instanceID, dpuMachineID, dpuSerial string) *unstructured.Unstructured {
	source := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"instance_id": instanceID},
		"status": map[string]any{"primary_dpu": map[string]any{
			"machine_id": dpuMachineID, "serial_number": dpuSerial,
		}},
	}}
	source.SetGroupVersionKind(BareMetalInstanceGVK)
	source.SetName(name)
	source.SetNamespace(namespace)
	source.SetUID(types.UID(uid))
	source.SetAnnotations(map[string]string{OrgIDAnnotation: tenantID})
	return source
}

func setSourceSpecField(t *testing.T, source *unstructured.Unstructured, name string, value any) {
	t.Helper()
	if err := unstructured.SetNestedField(source.Object, value, "spec", name); err != nil {
		t.Fatal(err)
	}
}

func updateSourceField(t *testing.T, ctx context.Context, c client.Client, key client.ObjectKey, gvk schema.GroupVersionKind, value any, fields ...string) {
	t.Helper()
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, key, source); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(source.Object, value, fields...); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
}

func getEntry(t *testing.T, c client.Client, name string) *spirev1alpha1.ClusterStaticEntry {
	t.Helper()
	entry := &spirev1alpha1.ClusterStaticEntry{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

func assertEntry(t *testing.T, entry *spirev1alpha1.ClusterStaticEntry, spiffeID, parentID, hint string, selectors []string) {
	t.Helper()
	if entry.Spec.SPIFFEID != spiffeID {
		t.Fatalf("SPIFFE ID = %q, want %q", entry.Spec.SPIFFEID, spiffeID)
	}
	if entry.Spec.ParentID != parentID {
		t.Fatalf("parent ID = %q, want %q", entry.Spec.ParentID, parentID)
	}
	if entry.Spec.Hint != hint {
		t.Fatalf("hint = %q, want %q", entry.Spec.Hint, hint)
	}
	if entry.Spec.ClassName != testConfig.ClassName {
		t.Fatalf("class name = %q, want %q", entry.Spec.ClassName, testConfig.ClassName)
	}
	if len(entry.Spec.Selectors) != len(selectors) {
		t.Fatalf("selectors = %#v, want %#v", entry.Spec.Selectors, selectors)
	}
	for i := range selectors {
		if entry.Spec.Selectors[i] != selectors[i] {
			t.Fatalf("selectors = %#v, want %#v", entry.Spec.Selectors, selectors)
		}
	}
}

func assertSourceAnnotations(t *testing.T, entry *spirev1alpha1.ClusterStaticEntry, group, kind, namespace, name string) {
	t.Helper()
	want := map[string]string{
		sourceGroupAnnotation:     group,
		sourceKindAnnotation:      kind,
		sourceNamespaceAnnotation: namespace,
		sourceNameAnnotation:      name,
	}
	for key, value := range want {
		if entry.Annotations[key] != value {
			t.Fatalf("annotation %q = %q, want %q", key, entry.Annotations[key], value)
		}
	}
}

func assertApplyCalls(t *testing.T, c *applyClient, want int) {
	t.Helper()
	if len(c.applyCalls) != want {
		t.Fatalf("server-side apply calls = %d, want %d", len(c.applyCalls), want)
	}
	for i, call := range c.applyCalls {
		if call.patchType != types.ApplyPatchType || call.fieldManager != fieldOwner || !call.force {
			t.Fatalf("apply call %d = type %q, manager %q, force %v", i, call.patchType, call.fieldManager, call.force)
		}
	}
}

// applyClient gives controller-runtime's fake client the API server's
// create-or-update behavior for server-side apply patches.
type applyClient struct {
	client.Client
	applyCalls          []applyCall
	childDeleteFailures int
}

type applyCall struct {
	patchType    types.PatchType
	fieldManager string
	force        bool
}

var errInjectedChildDelete = errors.New("injected ClusterStaticEntry deletion failure")

func (c *applyClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if patch.Type() != types.ApplyPatchType {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	options := (&client.PatchOptions{}).ApplyOptions(opts)
	c.applyCalls = append(c.applyCalls, applyCall{
		patchType:    patch.Type(),
		fieldManager: options.FieldManager,
		force:        options.Force != nil && *options.Force,
	})
	current := obj.DeepCopyObject().(client.Object)
	err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), current)
	if apierrors.IsNotFound(err) {
		return c.Client.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(current.GetResourceVersion())
	return c.Client.Update(ctx, obj)
}

func (c *applyClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*spirev1alpha1.ClusterStaticEntry); ok && c.childDeleteFailures > 0 {
		c.childDeleteFailures--
		return errInjectedChildDelete
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestConfigSourceAPIPollInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Hour, time.Minute, time.Nanosecond, 0, -time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			config := testConfig
			config.SourceAPIPollInterval = interval
			if err := config.Validate(); (err != nil) != (interval <= 0) {
				t.Fatalf("Validate with interval %v = %v", interval, err)
			}
		})
	}
}

func TestLaunchLayerRoleLifecycle(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bm", "vm"} {
		t.Run(kind, func(t *testing.T) {
			source := newBareMetalInstance("source", "tenant-system", "7ae7fe75-e671-4b0e-a76d-ef02bb5b040b", "tenant-2", "instance-1", "dpu-1", "dpu-1")
			parent := "spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/dpu-1"
			hint := "dsx-imds"
			selectors := testDPUMetadataSelectors
			if kind == "vm" {
				source = newVirtualInstance("source", "tenant-system", "e35e41ae-a112-4f64-8841-298c2a6662b0", "tenant-2", "instance-1", "host-1", "e35e41ae-a112-4f64-8841-298c2a6662b0")
				parent = "spiffe://k2.sjc-1.nvidia.com/spire/agent/dsx_imds/tenant/operator-1/bm/host-1"
				hint = "dsx-imds-vm"
				selectors = []string{"k8s:pod-uid:e35e41ae-a112-4f64-8841-298c2a6662b0"}
			}
			c := &applyClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(source).Build()}
			r := &LaunchLayerReconciler{Client: c, Config: testConfig, SourceGVK: source.GroupVersionKind()}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(source)}
			for _, tc := range []struct {
				role    string
				invalid bool
			}{
				{"", false}, {"trainer_1-gpu", false}, {"a", false}, {strings.Repeat("a", 63), false},
				{"Trainer", true}, {"a/b", true}, {"a.b", true}, {" a", true}, {"a%2fb", true}, {"_a", true}, {strings.Repeat("a", 64), true},
				{"corrected", false}, {"", false},
			} {
				source.SetAnnotations(map[string]string{OrgIDAnnotation: "tenant-2", "launchlayer.nvidia.com/role": tc.role})
				if err := c.Update(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				if tc.invalid {
					entry := &spirev1alpha1.ClusterStaticEntry{}
					if err := c.Get(t.Context(), client.ObjectKey{Name: childName(source)}, entry); !apierrors.IsNotFound(err) {
						t.Fatalf("invalid role %q retained entry: %v", tc.role, err)
					}
					continue
				}
				suffix := ""
				if tc.role != "" {
					suffix = "/role/" + tc.role
				}
				entry := getEntry(t, c, childName(source))
				assertEntry(t, entry, "spiffe://k2.sjc-1.nvidia.com/tenant/tenant-2/service/dsx/"+kind+"/instance-1"+suffix, parent, hint, selectors)
				id, err := spiffeid.FromString(entry.Spec.SPIFFEID)
				if err != nil {
					t.Fatal(err)
				}
				subject, err := spireidentity.Parse(id)
				if err != nil {
					t.Fatal(err)
				}
				exchanged, err := dsxidentity.ID(subject, "ds9", "nvid.id")
				if err != nil {
					t.Fatal(err)
				}
				if want := "spiffe://tenant-2.ds9.dsx.nvid.id/" + kind + "/instance-1" + suffix; exchanged.String() != want {
					t.Fatalf("exchanged subject = %s, want %s", exchanged, want)
				}
				claims, err := dsxidentity.Claims(subject, "ds9", "nvid.id")
				if err != nil {
					t.Fatal(err)
				}
				if tc.role != "" {
					last := claims.Resource[len(claims.Resource)-1]
					if last.ResourceType != "role" || last.ResourceID != tc.role {
						t.Fatalf("role claim pair = %#v", last)
					}
				} else if len(claims.Resource) != 1 {
					t.Fatalf("unexpected resource pairs: %#v", claims.Resource)
				}
			}
			annotations := source.GetAnnotations()
			delete(annotations, "launchlayer.nvidia.com/role")
			source.SetAnnotations(annotations)
			if err := c.Update(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			assertEntry(t, getEntry(t, c, childName(source)), "spiffe://k2.sjc-1.nvidia.com/tenant/tenant-2/service/dsx/"+kind+"/instance-1", parent, hint, selectors)
		})
	}
}

func TestInvalidBareMetalRoleDoesNotRestoreOperatorIdentity(t *testing.T) {
	t.Parallel()
	dpu := &unstructured.Unstructured{}
	dpu.SetGroupVersionKind(DPUGVK)
	dpu.SetName("dpu-role")
	dpu.SetLabels(map[string]string{dpuMachineIDLabel: "machine-1"})
	setSourceSpecField(t, dpu, "serialNumber", "52TF43G001G")
	bm := newBareMetalInstance("assigned-host", "tenant-system", "940863e9-1e6f-40ea-9104-c46a7b041ce5", "tenant-2", "instance-1", "machine-1", "52TF43G001G")
	c := &applyClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dpu, bm).Build()}
	dr := &DPUReconciler{Client: c, Config: testConfig}
	br := &LaunchLayerReconciler{Client: c, Config: testConfig, SourceGVK: BareMetalInstanceGVK}
	for _, tc := range []struct {
		role    string
		invalid bool
	}{{"trainer", false}, {"Trainer", true}, {"corrected", false}, {"a/b", true}, {"", false}} {
		bm.SetAnnotations(map[string]string{OrgIDAnnotation: "tenant-2", RoleAnnotation: tc.role})
		if err := c.Update(t.Context(), bm); err != nil {
			t.Fatal(err)
		}
		for _, reconcile := range []struct {
			r interface {
				Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
			}
			object client.Object
		}{{br, bm}, {dr, dpu}} {
			if _, err := reconcile.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(reconcile.object)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: childName(dpu)}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
			t.Fatalf("role %q restored operator fallback: %v", tc.role, err)
		}
		if tc.invalid {
			if err := c.Get(t.Context(), client.ObjectKey{Name: childName(bm)}, &spirev1alpha1.ClusterStaticEntry{}); !apierrors.IsNotFound(err) {
				t.Fatalf("invalid role %q retained tenant entry: %v", tc.role, err)
			}
		} else {
			getEntry(t, c, childName(bm))
		}
	}
	if err := c.Delete(t.Context(), bm); err != nil {
		t.Fatal(err)
	}
	if _, err := br.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(bm)}); err != nil {
		t.Fatal(err)
	}
	if _, err := dr.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dpu)}); err != nil {
		t.Fatal(err)
	}
	assertEntry(t, getEntry(t, c, childName(dpu)), "spiffe://k2.sjc-1.nvidia.com/tenant/operator-1/service/dsx/bm/machine-1", "spiffe://k2.sjc-1.nvidia.com/spire/agent/dpu_hw/52tf43g001g", "dsx-imds", testDPUMetadataSelectors)
}
