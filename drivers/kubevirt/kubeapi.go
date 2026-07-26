package kubevirt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	kvv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
)

const (
	LabelManaged       = "openshell.io/managed"
	LabelSandboxName   = "openshell.io/sandbox-name"
	AnnotSandboxID     = "openshell.io/sandbox-id-value"
	AnnotSandboxName   = "openshell.io/sandbox-name-value"
	AnnotNamespace     = "openshell.io/sandbox-namespace"
	AnnotWorkspace     = "openshell.io/workspace-value"
	AnnotGatewayID     = "openshell.io/gateway-id-value"
	AnnotRequestDigest = "openshell.io/request-digest"
	cloudInitKey       = "userdata"
	defaultPageSize    = int64(200)
)

var (
	vmGVR           = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR          = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	dataVolumeGVR   = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
	dataSourceGVR   = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datasources"}
	instanceTypeGVR = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclusterinstancetypes"}
	preferenceGVR   = schema.GroupVersionResource{Group: "instancetype.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclusterpreferences"}
)

// KubeAPIProvider is the Kubernetes/KubeVirt adapter. It is scoped to one
// configured namespace; sandbox metadata can never select a resource namespace.
type KubeAPIProvider struct {
	config  Config
	core    kubernetes.Interface
	dynamic dynamic.Interface
}

type uncertainCreateError struct{ err error }

func (e *uncertainCreateError) Error() string {
	return "Kubernetes create outcome is uncertain: " + e.err.Error()
}
func (e *uncertainCreateError) Unwrap() error { return e.err }

func NewKubeAPIProvider(restConfig *rest.Config, config Config) (*KubeAPIProvider, error) {
	if restConfig == nil {
		return nil, errors.New("kubernetes REST config is required")
	}
	coreClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("construct Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("construct dynamic Kubernetes client: %w", err)
	}
	return NewKubeAPIProviderFromClients(config, coreClient, dynamicClient), nil
}

// NewKubeAPIProviderFromClients is intended for adapter tests.
func NewKubeAPIProviderFromClients(config Config, coreClient kubernetes.Interface, dynamicClient dynamic.Interface) *KubeAPIProvider {
	return &KubeAPIProvider{config: config, core: coreClient, dynamic: dynamicClient}
}

func (p *KubeAPIProvider) CheckReady(ctx context.Context) error {
	return retryWithBackoff(ctx, func() error { return p.checkReadyOnce(ctx) })
}

func (p *KubeAPIProvider) checkReadyOnce(ctx context.Context) error {
	if _, err := p.core.CoreV1().Namespaces().Get(ctx, p.config.Namespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get target namespace %s: %w", p.config.Namespace, err)
	}
	if _, err := p.dynamic.Resource(dataSourceGVR).Namespace(p.config.BootSourceNamespace).Get(ctx, p.config.BootSource, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get boot DataSource %s/%s: %w", p.config.BootSourceNamespace, p.config.BootSource, err)
	}
	if _, err := p.dynamic.Resource(instanceTypeGVR).Get(ctx, p.config.DefaultInstanceType, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get cluster instancetype %s: %w", p.config.DefaultInstanceType, err)
	}
	if p.config.DefaultPreference != "" {
		if _, err := p.dynamic.Resource(preferenceGVR).Get(ctx, p.config.DefaultPreference, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("get cluster preference %s: %w", p.config.DefaultPreference, err)
		}
	}
	if p.config.StorageClass != "" {
		if _, err := p.core.StorageV1().StorageClasses().Get(ctx, p.config.StorageClass, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("get StorageClass %s: %w", p.config.StorageClass, err)
		}
	}
	checks := []authorizationv1.ResourceAttributes{
		{Namespace: p.config.Namespace, Group: "", Resource: "secrets", Verb: "create"},
		{Namespace: p.config.Namespace, Group: "", Resource: "secrets", Verb: "get"},
		{Namespace: p.config.Namespace, Group: "", Resource: "secrets", Verb: "update"},
		{Namespace: p.config.Namespace, Group: "", Resource: "secrets", Verb: "delete"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "list"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "get"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "create"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "update"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "patch"},
		{Namespace: p.config.Namespace, Group: vmGVR.Group, Resource: vmGVR.Resource, Verb: "delete"},
		{Namespace: p.config.Namespace, Group: vmiGVR.Group, Resource: vmiGVR.Resource, Verb: "list"},
		{Namespace: p.config.Namespace, Group: dataVolumeGVR.Group, Resource: dataVolumeGVR.Resource, Verb: "delete"},
	}
	for _, attributes := range checks {
		review, err := p.core.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("check %s %s permission: %w", attributes.Verb, attributes.Resource, err)
		}
		if !review.Status.Allowed {
			return fmt.Errorf("service account may not %s %s in namespace %s: %s", attributes.Verb, attributes.Resource, p.config.Namespace, review.Status.Reason)
		}
	}
	return nil
}

func (p *KubeAPIProvider) Launch(ctx context.Context, spec VMSpec) (VMInstance, error) {
	vm, secret, err := buildResources(p.config.Namespace, spec)
	if err != nil {
		return VMInstance{}, err
	}
	secretCreated, err := p.createOrVerifySecret(ctx, secret)
	if err != nil {
		return VMInstance{}, err
	}
	createdVM, _, err := p.createOrVerifyVM(ctx, vm)
	if err != nil {
		var uncertain *uncertainCreateError
		if secretCreated && !errors.As(err, &uncertain) {
			_ = p.core.CoreV1().Secrets(p.config.Namespace).Delete(context.WithoutCancel(ctx), secret.Name, metav1.DeleteOptions{})
		}
		return VMInstance{}, err
	}
	if err := p.ownSecret(ctx, secret.Name, createdVM); err != nil {
		cleanupErr := p.Terminate(context.WithoutCancel(ctx), createdVM.GetName())
		return VMInstance{}, errors.Join(fmt.Errorf("attach cloud-init Secret to VM: %w", err), cleanupErr)
	}
	return normalizeVM(createdVM, nil), nil
}

func buildResources(namespace string, spec VMSpec) (*kvv1.VirtualMachine, *corev1.Secret, error) {
	if len(spec.ResourceKey) < 32 || len(spec.RequestDigest) != 64 {
		return nil, nil, errors.New("invalid deterministic resource or request digest")
	}
	diskSize, err := resource.ParseQuantity(spec.DiskSize)
	if err != nil {
		return nil, nil, fmt.Errorf("parse disk size: %w", err)
	}
	vmName := "openshell-" + strings.ToLower(spec.ResourceKey[:32])
	secretName, dataVolumeName := vmName+"-cloudinit", vmName+"-root"
	objectLabels := map[string]string{
		LabelManaged: "true", LabelGatewayID: labelValue(spec.GatewayID),
		LabelSandboxID: labelValue(spec.SandboxID), LabelSandboxName: labelValue(spec.Name),
	}
	annotations := map[string]string{
		AnnotSandboxID: spec.SandboxID, AnnotSandboxName: spec.Name, AnnotNamespace: spec.SandboxNamespace,
		AnnotWorkspace: spec.Workspace, AnnotGatewayID: spec.GatewayID, AnnotCreatedAt: spec.CreatedAt.UTC().Format(time.RFC3339Nano),
		AnnotMaxLifetime: spec.MaxLifetime, AnnotRequestDigest: spec.RequestDigest,
	}
	runStrategy := kvv1.RunStrategyRerunOnFailure
	storage := &cdiv1.StorageSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: diskSize}}}
	if spec.StorageClass != "" {
		storage.StorageClassName = &spec.StorageClass
	}
	dvLabels := make(map[string]string, len(objectLabels)+len(spec.StorageLabels))
	for k, v := range objectLabels {
		dvLabels[k] = v
	}
	for k, v := range spec.StorageLabels {
		dvLabels[k] = v
	}
	dvAnnotations := make(map[string]string, len(annotations)+len(spec.StorageAnnotations))
	for k, v := range annotations {
		dvAnnotations[k] = v
	}
	for k, v := range spec.StorageAnnotations {
		dvAnnotations[k] = v
	}

	vm := &kvv1.VirtualMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: namespace, Labels: objectLabels, Annotations: annotations},
		Spec: kvv1.VirtualMachineSpec{
			RunStrategy:  &runStrategy,
			Instancetype: &kvv1.InstancetypeMatcher{Name: spec.InstanceType, Kind: "VirtualMachineClusterInstancetype"},
			DataVolumeTemplates: []kvv1.DataVolumeTemplateSpec{{
				ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName, Labels: dvLabels, Annotations: dvAnnotations},
				Spec: cdiv1.DataVolumeSpec{SourceRef: &cdiv1.DataVolumeSourceRef{
					Kind: cdiv1.DataVolumeDataSource, Namespace: &spec.BootSourceNamespace, Name: spec.BootSource,
				}, Storage: storage},
			}},
			Template: &kvv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: objectLabels, Annotations: annotations},
				Spec: kvv1.VirtualMachineInstanceSpec{
					Domain: kvv1.DomainSpec{Devices: kvv1.Devices{
						Disks: []kvv1.Disk{
							{Name: "rootdisk", DiskDevice: kvv1.DiskDevice{Disk: &kvv1.DiskTarget{Bus: kvv1.DiskBusVirtio}}},
							{Name: "cloudinit", DiskDevice: kvv1.DiskDevice{Disk: &kvv1.DiskTarget{Bus: kvv1.DiskBusVirtio}}},
						},
						Interfaces: []kvv1.Interface{{Name: "default", InterfaceBindingMethod: kvv1.InterfaceBindingMethod{Masquerade: &kvv1.InterfaceMasquerade{}}}},
					}},
					Networks: []kvv1.Network{{Name: "default", NetworkSource: kvv1.NetworkSource{Pod: &kvv1.PodNetwork{}}}},
					Volumes: []kvv1.Volume{
						{Name: "rootdisk", VolumeSource: kvv1.VolumeSource{DataVolume: &kvv1.DataVolumeSource{Name: dataVolumeName}}},
						{Name: "cloudinit", VolumeSource: kvv1.VolumeSource{CloudInitNoCloud: &kvv1.CloudInitNoCloudSource{UserDataSecretRef: &corev1.LocalObjectReference{Name: secretName}}}},
					},
				},
			},
		},
	}
	if spec.Preference != "" {
		vm.Spec.Preference = &kvv1.PreferenceMatcher{Name: spec.Preference, Kind: "VirtualMachineClusterPreference"}
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace, Labels: objectLabels, Annotations: annotations},
		Immutable:  &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{cloudInitKey: []byte(spec.CloudInit)},
	}
	return vm, secret, nil
}

func (p *KubeAPIProvider) createOrVerifySecret(ctx context.Context, desired *corev1.Secret) (bool, error) {
	created := false
	err := retryWithBackoff(ctx, func() error {
		_, createErr := p.core.CoreV1().Secrets(p.config.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		if createErr == nil {
			created = true
			return nil
		}
		if !apierrors.IsAlreadyExists(createErr) && !isTransientKubernetesError(createErr) {
			return createErr
		}
		existing, getErr := p.core.CoreV1().Secrets(p.config.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) && isTransientKubernetesError(createErr) {
			return createErr
		}
		if getErr != nil {
			if isTransientKubernetesError(createErr) {
				return &uncertainCreateError{err: getErr}
			}
			return getErr
		}
		if existing.Annotations[AnnotRequestDigest] != desired.Annotations[AnnotRequestDigest] || string(existing.Data[cloudInitKey]) != string(desired.Data[cloudInitKey]) {
			return apierrors.NewConflict(corev1.Resource("secrets"), desired.Name, errors.New("existing Secret belongs to a different request"))
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("create cloud-init Secret: %w", err)
	}
	return created, nil
}

func (p *KubeAPIProvider) createOrVerifyVM(ctx context.Context, desired *kvv1.VirtualMachine) (*unstructured.Unstructured, bool, error) {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return nil, false, fmt.Errorf("encode VirtualMachine: %w", err)
	}
	var result *unstructured.Unstructured
	created := false
	err = retryWithBackoff(ctx, func() error {
		createdVM, createErr := p.dynamic.Resource(vmGVR).Namespace(p.config.Namespace).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
		if createErr == nil {
			result, created = createdVM, true
			return nil
		}
		if !apierrors.IsAlreadyExists(createErr) && !isTransientKubernetesError(createErr) {
			return createErr
		}
		existing, getErr := p.dynamic.Resource(vmGVR).Namespace(p.config.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) && isTransientKubernetesError(createErr) {
			return createErr
		}
		if getErr != nil {
			if isTransientKubernetesError(createErr) {
				return &uncertainCreateError{err: getErr}
			}
			return getErr
		}
		if existing.GetAnnotations()[AnnotRequestDigest] != desired.Annotations[AnnotRequestDigest] || existing.GetLabels()[LabelSandboxID] != desired.Labels[LabelSandboxID] {
			return apierrors.NewConflict(schema.GroupResource{Group: vmGVR.Group, Resource: vmGVR.Resource}, desired.Name, errors.New("existing VM belongs to a different request"))
		}
		result = existing
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("create VirtualMachine: %w", err)
	}
	return result, created, nil
}

func (p *KubeAPIProvider) ownSecret(ctx context.Context, secretName string, vm *unstructured.Unstructured) error {
	return retryWithBackoff(ctx, func() error {
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			secret, err := p.core.CoreV1().Secrets(p.config.Namespace).Get(ctx, secretName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			controller := true
			secret.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: vm.GetName(), UID: vm.GetUID(),
				Controller: &controller,
			}}
			_, err = p.core.CoreV1().Secrets(p.config.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
			return err
		})
	})
}

func (p *KubeAPIProvider) List(ctx context.Context, filter VMFilter) ([]VMInstance, error) {
	selector := labels.Set{LabelManaged: "true"}
	if filter.GatewayID != "" {
		selector[LabelGatewayID] = labelValue(filter.GatewayID)
	}
	if filter.SandboxID != "" {
		selector[LabelSandboxID] = labelValue(filter.SandboxID)
	}
	selectorText := selector.AsSelector().String()
	vmObjects, err := p.listAll(ctx, vmGVR, selectorText)
	if err != nil {
		return nil, fmt.Errorf("list VirtualMachines: %w", err)
	}
	vmiObjects, err := p.listAll(ctx, vmiGVR, selectorText)
	if err != nil {
		return nil, fmt.Errorf("list VirtualMachineInstances: %w", err)
	}
	vmis := make(map[string]*unstructured.Unstructured, len(vmiObjects))
	for i := range vmiObjects {
		vmis[vmiObjects[i].GetName()] = &vmiObjects[i]
	}
	instances := make([]VMInstance, 0, len(vmObjects))
	for i := range vmObjects {
		instance := normalizeVM(&vmObjects[i], vmis[vmObjects[i].GetName()])
		if filter.Name != "" && instance.Name != filter.Name {
			continue
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (p *KubeAPIProvider) listAll(ctx context.Context, gvr schema.GroupVersionResource, selector string) ([]unstructured.Unstructured, error) {
	var items []unstructured.Unstructured
	continueToken := ""
	for {
		var page *unstructured.UnstructuredList
		err := retryWithBackoff(ctx, func() error {
			var err error
			page, err = p.dynamic.Resource(gvr).Namespace(p.config.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: selector, Limit: defaultPageSize, Continue: continueToken,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		continueToken = page.GetContinue()
		if continueToken == "" {
			return items, nil
		}
	}
}

func (p *KubeAPIProvider) Stop(ctx context.Context, name string) error {
	return retryWithBackoff(ctx, func() error {
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			vm, err := p.dynamic.Resource(vmGVR).Namespace(p.config.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if err := unstructured.SetNestedField(vm.Object, string(kvv1.RunStrategyHalted), "spec", "runStrategy"); err != nil {
				return err
			}
			_, err = p.dynamic.Resource(vmGVR).Namespace(p.config.Namespace).Update(ctx, vm, metav1.UpdateOptions{})
			return err
		})
	})
}

func (p *KubeAPIProvider) Terminate(ctx context.Context, name string) error {
	var errs []error
	if err := retryWithBackoff(ctx, func() error { return p.deleteVM(ctx, name) }); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete VirtualMachine: %w", err))
	}
	for _, child := range []struct {
		kind string
		fn   func() error
	}{
		{"DataVolume", func() error {
			return p.dynamic.Resource(dataVolumeGVR).Namespace(p.config.Namespace).Delete(ctx, name+"-root", metav1.DeleteOptions{})
		}},
		{"Secret", func() error {
			return p.core.CoreV1().Secrets(p.config.Namespace).Delete(ctx, name+"-cloudinit", metav1.DeleteOptions{})
		}},
	} {
		if err := retryWithBackoff(ctx, child.fn); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", child.kind, err))
		}
	}
	return errors.Join(errs...)
}

func (p *KubeAPIProvider) deleteVM(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationForeground
	return p.dynamic.Resource(vmGVR).Namespace(p.config.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
}

func normalizeVM(raw, rawVMI *unstructured.Unstructured) VMInstance {
	annotations := raw.GetAnnotations()
	state, _, _ := unstructured.NestedString(raw.Object, "status", "printableStatus")
	if raw.GetDeletionTimestamp() != nil {
		state = "Terminating"
	}
	instance := VMInstance{
		VMName: raw.GetName(), SandboxID: annotations[AnnotSandboxID], Name: annotations[AnnotSandboxName],
		Namespace: annotations[AnnotNamespace], Workspace: annotations[AnnotWorkspace], State: state,
		CreatedAt: raw.GetCreationTimestamp().Time,
	}
	if instance.SandboxID == "" {
		if hash := raw.GetLabels()[LabelSandboxID]; hash != "" {
			instance.SandboxID = "hash:" + hash
		} else {
			instance.SandboxID = "orphan:" + raw.GetName()
		}
	}
	if instance.Name == "" {
		instance.Name = raw.GetName()
	}
	if parsed, err := time.Parse(time.RFC3339Nano, annotations[AnnotCreatedAt]); err == nil {
		instance.CreatedAt = parsed
	}
	if rawVMI != nil {
		instance.VMIPhase, _, _ = unstructured.NestedString(rawVMI.Object, "status", "phase")
		interfaces, _, _ := unstructured.NestedSlice(rawVMI.Object, "status", "interfaces")
		if len(interfaces) != 0 {
			if iface, ok := interfaces[0].(map[string]any); ok {
				instance.IP, _, _ = unstructured.NestedString(iface, "ipAddress")
			}
		}
		if instance.State == "" {
			instance.State = instance.VMIPhase
		}
	}
	if instance.State == "" {
		runStrategy, _, _ := unstructured.NestedString(raw.Object, "spec", "runStrategy")
		if runStrategy == string(kvv1.RunStrategyHalted) {
			instance.State = "Stopped"
		} else {
			instance.State = "Starting"
		}
	}
	return instance
}

func labelValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:16])
}

func isTransientKubernetesError(err error) bool {
	return apierrors.IsTooManyRequests(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err)
}

func retryWithBackoff(ctx context.Context, fn func() error) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: .2, Steps: 5, Cap: 2 * time.Second}, func(context.Context) (bool, error) {
		err := fn()
		if err == nil {
			return true, nil
		}
		if !isTransientKubernetesError(err) {
			return false, err
		}
		lastErr = err
		return false, nil
	})
	if wait.Interrupted(err) && lastErr != nil {
		return lastErr
	}
	return err
}

var _ VMProvider = (*KubeAPIProvider)(nil)
