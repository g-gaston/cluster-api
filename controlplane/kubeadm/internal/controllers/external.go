package controllers

import (
	"context"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal"
	"sigs.k8s.io/cluster-api/controlplane/upgrade"
	"sigs.k8s.io/cluster-api/util/collections"
)

func (r *KubeadmControlPlaneReconciler) upgradeControlPlaneWithExternal(
	ctx context.Context,
	controlPlane *internal.ControlPlane,
	machinesRequireUpgrade collections.Machines,
) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	req, err := r.buildExternalUpgradeRequest(
		ctx, controlPlane, machinesRequireUpgrade,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	resp, err := r.ExternalUpgrade.CallUntilAccepted(ctx, *req)
	if err != nil {
		return ctrl.Result{}, err
	}

	// none of the registered strategies accepted the upgrade, check for a fallback
	if resp == nil {
		externalUpdate := controlPlane.KCP.Spec.RolloutStrategy.ExternalUpdate
		if externalUpdate != nil && externalUpdate.FallbackRolling != nil {
			logger.Info("No external strategy accepted the upgrade, using rolling upgrade as a fallback")
			return r.upgradeControlPlaneWithRolling(ctx, controlPlane, machinesRequireUpgrade, externalUpdate.FallbackRolling)
		}
		logger.Info("No external strategy accepted the upgrade and no fallback strategy is configured, upgrade won't continue")
	}

	return ctrl.Result{}, nil
}

func (r *KubeadmControlPlaneReconciler) buildExternalUpgradeRequest(
	ctx context.Context,
	controlPlane *internal.ControlPlane,
	machinesRequireUpgrade collections.Machines,
) (*upgrade.ExternalStrategyRequest, error) {
	newMachineSpec, err := r.generateNewMachine(ctx, controlPlane)
	if err != nil {
		return nil, err
	}
	rawKubeadmConfig, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newMachineSpec.kubeadmConfig)
	if err != nil {
		return nil, err
	}

	return &upgrade.ExternalStrategyRequest{
		Cluster:                controlPlane.Cluster,
		ControlPlane:           objToRef(controlPlane.KCP),
		MachinesRequireUpgrade: machinesRequireUpgrade,
		NewMachine: &upgrade.NewMachineSpec{
			Machine:         newMachineSpec.machine,
			BootstrapConfig: &unstructured.Unstructured{Object: rawKubeadmConfig},
			InfraMachine:    newMachineSpec.infraMachine,
		},
	}, nil
}

type machineSpec struct {
	machine       *clusterv1.Machine
	kubeadmConfig *bootstrapv1.KubeadmConfig
	infraMachine  *unstructured.Unstructured
}

func (r *KubeadmControlPlaneReconciler) generateNewMachine(ctx context.Context, controlPlane *internal.ControlPlane) (*machineSpec, error) {
	templateRef := &controlPlane.KCP.Spec.MachineTemplate.InfrastructureRef
	namespace := controlPlane.KCP.Namespace

	// Since the cloned resource should eventually have a controller ref for the Machine, we create an
	// OwnerReference here without the Controller field set
	infraCloneOwner := &metav1.OwnerReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       kubeadmControlPlaneKind,
		Name:       controlPlane.KCP.Name,
		UID:        controlPlane.KCP.UID,
	}

	from, err := external.Get(ctx, r.Client, templateRef, namespace)
	if err != nil {
		return nil, err
	}

	generateTemplateInput := &external.GenerateTemplateInput{
		Template:    from,
		TemplateRef: templateRef,
		Namespace:   namespace,
		ClusterName: controlPlane.Cluster.Name,
		OwnerRef:    infraCloneOwner,
		Labels:      internal.ControlPlaneMachineLabelsForCluster(controlPlane.KCP, controlPlane.Cluster.Name),
		Annotations: controlPlane.KCP.Spec.MachineTemplate.ObjectMeta.Annotations,
	}
	infraMachine, err := external.GenerateTemplate(generateTemplateInput)
	if err != nil {
		return nil, err
	}

	bootstrapSpec := controlPlane.JoinControlPlaneConfig()
	bootstrapConfig := generateKubeadmConfig(controlPlane.KCP, controlPlane.Cluster, bootstrapSpec)

	infraRef := external.GetObjectReference(infraMachine)
	bootstrapRef := &corev1.ObjectReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "KubeadmConfig",
		Name:       bootstrapConfig.GetName(),
		Namespace:  bootstrapConfig.GetNamespace(),
		UID:        bootstrapConfig.GetUID(),
	}

	machine, err := r.computeDesiredMachine(
		controlPlane.KCP, controlPlane.Cluster, infraRef, bootstrapRef, nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Machine: failed to compute desired Machine")
	}

	return &machineSpec{
		machine:       machine,
		kubeadmConfig: bootstrapConfig,
		infraMachine:  infraMachine,
	}, nil
}

func objToRef(obj client.Object) *corev1.ObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return &corev1.ObjectReference{
		Kind:            gvk.Kind,
		APIVersion:      gvk.GroupVersion().String(),
		Namespace:       obj.GetNamespace(),
		Name:            obj.GetName(),
		ResourceVersion: obj.GetResourceVersion(),
	}
}
