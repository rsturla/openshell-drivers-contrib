package kubevirt

import (
	"context"
	"errors"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	corefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	kvv1 "kubevirt.io/api/core/v1"
)

func adapterSpec() VMSpec {
	return VMSpec{
		SandboxID: "sandbox/id with unsafe label characters", Name: "A display name", SandboxNamespace: "logical",
		Workspace: "workspace", GatewayID: "Gateway/with unsafe label characters", InstanceType: "cx1.medium",
		BootSource: "fedora", BootSourceNamespace: "openshift-virtualization-os-images", Preference: "fedora",
		StorageClass: "encrypted-rbd", CloudInit: "#cloud-config\n", ResourceKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		RequestDigest: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", DiskSize: "10Gi",
		CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC), MaxLifetime: "8h0m0s",
	}
}

func TestBuildResourcesEnforcesKubeVirtPolicy(t *testing.T) {
	vm, secret, err := buildResources("openshell", adapterSpec())
	if err != nil {
		t.Fatal(err)
	}
	if vm.APIVersion != "kubevirt.io/v1" || vm.Kind != "VirtualMachine" || vm.Namespace != "openshell" {
		t.Fatalf("unexpected VM identity: %s %s %s", vm.APIVersion, vm.Kind, vm.Namespace)
	}
	if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != kvv1.RunStrategyRerunOnFailure {
		t.Fatalf("unsafe run strategy: %v", vm.Spec.RunStrategy)
	}
	if vm.Spec.Instancetype == nil || vm.Spec.Instancetype.Kind != "VirtualMachineClusterInstancetype" || vm.Spec.Instancetype.Name != "cx1.medium" {
		t.Fatalf("bad instancetype matcher: %+v", vm.Spec.Instancetype)
	}
	if vm.Spec.Preference == nil || vm.Spec.Preference.Kind != "VirtualMachineClusterPreference" {
		t.Fatalf("bad preference matcher: %+v", vm.Spec.Preference)
	}
	if len(vm.Spec.DataVolumeTemplates) != 1 || vm.Spec.DataVolumeTemplates[0].Spec.SourceRef == nil || vm.Spec.DataVolumeTemplates[0].Spec.SourceRef.Kind != "DataSource" {
		t.Fatalf("bad DataVolume template: %+v", vm.Spec.DataVolumeTemplates)
	}
	template := vm.Spec.Template.Spec
	if len(template.Volumes) != 2 || template.Volumes[0].DataVolume == nil || template.Volumes[1].CloudInitNoCloud == nil {
		t.Fatalf("bad fixed volumes: %+v", template.Volumes)
	}
	if len(template.Domain.Devices.HostDevices) != 0 || len(template.Domain.Devices.GPUs) != 0 || len(template.ResourceClaims) != 0 || len(template.AccessCredentials) != 0 {
		t.Fatalf("privileged device/credential path present: %+v", template)
	}
	if len(template.Networks) != 1 || template.Networks[0].Pod == nil || len(template.Domain.Devices.Interfaces) != 1 || template.Domain.Devices.Interfaces[0].Masquerade == nil {
		t.Fatalf("VM does not use only pod masquerade networking: %+v", template.Networks)
	}
	if secret.Namespace != "openshell" || secret.Immutable == nil || !*secret.Immutable || string(secret.Data[cloudInitKey]) != "#cloud-config\n" {
		t.Fatalf("bad cloud-init Secret: %+v", secret)
	}
	for key, value := range vm.Labels {
		if len(value) > 63 {
			t.Fatalf("label %s too long: %d", key, len(value))
		}
	}
	if vm.Annotations[AnnotNamespace] != "logical" {
		t.Fatal("sandbox namespace metadata was not preserved")
	}
}

func TestLaunchCleansSecretWhenVMCreateFails(t *testing.T) {
	coreClient := corefake.NewSimpleClientset()
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme())
	dynamicClient.PrependReactor("create", "virtualmachines", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: vmGVR.Group, Resource: vmGVR.Resource}, "vm", errors.New("denied"))
	})
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	if _, err := provider.Launch(context.Background(), adapterSpec()); !apierrors.IsForbidden(err) {
		t.Fatalf("expected forbidden create, got %v", err)
	}
	secrets, err := coreClient.CoreV1().Secrets(testConfig().Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 0 {
		t.Fatalf("orphaned Secrets: %v, err=%v", secrets.Items, err)
	}
}

func TestLaunchIsIdempotentAndOwnsSecret(t *testing.T) {
	coreClient := corefake.NewSimpleClientset()
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme())
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	first, err := provider.Launch(context.Background(), adapterSpec())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Launch(context.Background(), adapterSpec())
	if err != nil {
		t.Fatal(err)
	}
	if first.VMName != second.VMName {
		t.Fatalf("idempotent names differ: %s %s", first.VMName, second.VMName)
	}
	secret, err := coreClient.CoreV1().Secrets(testConfig().Namespace).Get(context.Background(), first.VMName+"-cloudinit", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "VirtualMachine" || secret.OwnerReferences[0].Name != first.VMName {
		t.Fatalf("missing VM owner reference: %+v", secret.OwnerReferences)
	}
}

func TestLaunchRejectsDeterministicNameWithDifferentRequest(t *testing.T) {
	coreClient := corefake.NewSimpleClientset()
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme())
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	if _, err := provider.Launch(context.Background(), adapterSpec()); err != nil {
		t.Fatal(err)
	}
	changed := adapterSpec()
	changed.RequestDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	changed.CloudInit = "#cloud-config\n# changed request\n"
	if _, err := provider.Launch(context.Background(), changed); !apierrors.IsConflict(err) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
}

func TestLaunchOwnerReferenceFailureCompensatesVMAndSecret(t *testing.T) {
	coreClient := corefake.NewSimpleClientset()
	coreClient.PrependReactor("update", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "secret", errors.New("conflict"))
	})
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme())
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	if _, err := provider.Launch(context.Background(), adapterSpec()); err == nil {
		t.Fatal("expected owner-reference update failure")
	}
	vmName := "openshell-" + adapterSpec().ResourceKey[:32]
	if _, err := dynamicClient.Resource(vmGVR).Namespace(testConfig().Namespace).Get(context.Background(), vmName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VM was not compensated: %v", err)
	}
	if _, err := coreClient.CoreV1().Secrets(testConfig().Namespace).Get(context.Background(), vmName+"-cloudinit", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret was not compensated: %v", err)
	}
}

func TestIdempotentLaunchOwnerReferenceFailureCompensatesExistingResources(t *testing.T) {
	coreClient := corefake.NewSimpleClientset()
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme())
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	instance, err := provider.Launch(context.Background(), adapterSpec())
	if err != nil {
		t.Fatal(err)
	}
	coreClient.PrependReactor("update", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, instance.VMName+"-cloudinit", errors.New("denied"))
	})
	if _, err := provider.Launch(context.Background(), adapterSpec()); !apierrors.IsForbidden(err) {
		t.Fatalf("expected owner-reference failure, got %v", err)
	}
	if _, err := dynamicClient.Resource(vmGVR).Namespace(testConfig().Namespace).Get(context.Background(), instance.VMName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("existing VM was not compensated: %v", err)
	}
	if _, err := coreClient.CoreV1().Secrets(testConfig().Namespace).Get(context.Background(), instance.VMName+"-cloudinit", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("existing Secret was not compensated: %v", err)
	}
}

func TestStopPatchesRunStrategyAndTerminateDeletesChildren(t *testing.T) {
	spec := adapterSpec()
	vm, secret, err := buildResources(testConfig().Namespace, spec)
	if err != nil {
		t.Fatal(err)
	}
	vmObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(vm)
	if err != nil {
		t.Fatal(err)
	}
	dv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": vm.Name + "-root", "namespace": testConfig().Namespace},
	}}
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: vmObject}, dv)
	coreClient := corefake.NewSimpleClientset(secret)
	provider := NewKubeAPIProviderFromClients(testConfig(), coreClient, dynamicClient)
	if err := provider.Stop(context.Background(), vm.Name); err != nil {
		t.Fatal(err)
	}
	stopped, err := dynamicClient.Resource(vmGVR).Namespace(testConfig().Namespace).Get(context.Background(), vm.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runStrategy, _, _ := unstructured.NestedString(stopped.Object, "spec", "runStrategy")
	if runStrategy != string(kvv1.RunStrategyHalted) {
		t.Fatalf("VM was not halted: %q", runStrategy)
	}
	if err := provider.Terminate(context.Background(), vm.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(vmGVR).Namespace(testConfig().Namespace).Get(context.Background(), vm.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VM still exists: %v", err)
	}
	if _, err := dynamicClient.Resource(dataVolumeGVR).Namespace(testConfig().Namespace).Get(context.Background(), vm.Name+"-root", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("DataVolume still exists: %v", err)
	}
	if _, err := coreClient.CoreV1().Secrets(testConfig().Namespace).Get(context.Background(), secret.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret still exists: %v", err)
	}
	if err := provider.Terminate(context.Background(), vm.Name); err != nil {
		t.Fatalf("termination is not idempotent: %v", err)
	}
}

func TestListFollowsContinueAndRejectsPartialResult(t *testing.T) {
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
	})
	vmCalls := 0
	dynamicClient.PrependReactor("list", "virtualmachines", func(action ktesting.Action) (bool, runtime.Object, error) {
		vmCalls++
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if vmCalls == 1 {
			if options.Limit != defaultPageSize || options.Continue != "" {
				t.Fatalf("bad first page options: %+v", options)
			}
			return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineList", "metadata": map[string]any{"continue": "next"}}}, nil
		}
		if options.Continue != "next" {
			t.Fatalf("continue token not propagated: %+v", options)
		}
		return true, nil, apierrors.NewServiceUnavailable("page two failed")
	})
	provider := NewKubeAPIProviderFromClients(testConfig(), corefake.NewSimpleClientset(), dynamicClient)
	if instances, err := provider.List(context.Background(), VMFilter{GatewayID: "gw"}); err == nil || instances != nil {
		t.Fatalf("partial list was accepted: instances=%v err=%v", instances, err)
	}
	// retryWithBackoff uses five attempts after the successful first page.
	if vmCalls != 6 {
		t.Fatalf("unexpected page/retry calls: %d", vmCalls)
	}
}

func TestCheckReadyValidatesPrerequisitesAndPermissions(t *testing.T) {
	cfg := testConfig()
	coreClient := corefake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: cfg.StorageClass}},
	)
	coreClient.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
		review.Status.Allowed = true
		return true, review, nil
	})
	ds := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataSource", "metadata": map[string]any{"name": cfg.BootSource, "namespace": cfg.BootSourceNamespace}}}
	inst := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "instancetype.kubevirt.io/v1beta1", "kind": "VirtualMachineClusterInstancetype", "metadata": map[string]any{"name": cfg.DefaultInstanceType}}}
	dynamicClient := fake.NewSimpleDynamicClient(runtime.NewScheme(), ds, inst)
	provider := NewKubeAPIProviderFromClients(cfg, coreClient, dynamicClient)
	if err := provider.CheckReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	coreClient.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false, Reason: "denied"}}, nil
	})
	if err := provider.CheckReady(context.Background()); err == nil {
		t.Fatal("expected readiness permission failure")
	}
}

func TestNormalizeVMKeepsVMAndVMIStateSeparate(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "vm", "uid": string(types.UID("uid")), "annotations": map[string]any{AnnotSandboxID: "sb"}},
		"spec":     map[string]any{"runStrategy": "RerunOnFailure"}, "status": map[string]any{"printableStatus": "Stopped"},
	}}
	vmi := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "vm"}, "status": map[string]any{"phase": "Succeeded"}}}
	instance := normalizeVM(vm, vmi)
	if instance.State != "Stopped" || instance.VMIPhase != "Succeeded" {
		t.Fatalf("conflated VM/VMI state: %+v", instance)
	}
}
