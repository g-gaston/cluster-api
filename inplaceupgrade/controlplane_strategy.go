// Package inplaceupgrade provides in place upgrade functionality
// This should probably not leave in the capi repo, since it's just
// one of many ways to implement an upgrade strategy. This is just here
// for capi managed kubernetes nodes.
// for the PoC.
package inplaceupgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/controlplane/upgrade"
	inplacev1 "sigs.k8s.io/cluster-api/inplaceupgrade/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/collections"
)

// ControlPlaneExternalStrategy implements a control plane external upgrade strategy
// by upgrading the machines in-place.
type ControlPlaneExternalStrategy struct {
	client  client.Client
	tracker *remote.ClusterCacheTracker
	queue   chan *inplacev1.ControlPlaneUpgrade
}

func NewControlPlaneExternalStrategy(client client.Client, tracker *remote.ClusterCacheTracker) *ControlPlaneExternalStrategy {
	s := &ControlPlaneExternalStrategy{
		client:  client,
		tracker: tracker,
		queue:   make(chan *inplacev1.ControlPlaneUpgrade, 30),
	}

	go s.workQueue()

	return s
}

func (s *ControlPlaneExternalStrategy) Call(ctx context.Context, req upgrade.ExternalStrategyRequest) (*upgrade.ExternalStrategyResponse, error) {
	logger := ctrl.LoggerFrom(ctx)
	logger = logger.WithValues("externalStrategy", "inplaceUpgrades")
	ctx = ctrl.LoggerInto(ctx, logger)

	machineSpecs, err := readMachineSpecs(ctx, s.client, req.MachinesRequireUpgrade)
	if err != nil {
		return nil, err
	}

	accepted, reason, err := s.acceptUpgradeRequest(ctx, machineSpecs, req)
	if err != nil {
		return nil, err
	}

	if !accepted {
		return &upgrade.ExternalStrategyResponse{
			Accepted: false,
			Reason:   reason,
		}, nil
	}

	newVersion := supportedVersions[*req.NewMachine.Machine.Spec.Version]

	cpUpgradeName := strings.ReplaceAll(
		// TODO: use control plane generation to make this unique?
		fmt.Sprintf("%s-%s", req.ControlPlane.Name, newVersion.kubernetesVersion),
		".", "-",
	)

	logger.Info("Request for upgrade", "annotationNewKubeadmClusterConfig", req.NewMachine.Machine.Annotations[controlplanev1.KubeadmClusterConfigurationAnnotation])

	cpUpgrade := &inplacev1.ControlPlaneUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cpUpgradeName,
			Namespace: req.ControlPlane.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         req.ControlPlane.APIVersion,
					Kind:               req.ControlPlane.Kind,
					Name:               req.ControlPlane.Name,
					UID:                req.ControlPlane.UID,
					BlockOwnerDeletion: pointer.Bool(true),
					Controller:         pointer.Bool(true),
				},
			},
		},
		Spec: inplacev1.ControlPlaneUpgradeSpec{
			Cluster:                *objToRef(req.Cluster),
			ControlPlane:           *req.ControlPlane,
			MachinesRequireUpgrade: machinesToObjRefs(req.MachinesRequireUpgrade),
			NewVersion: inplacev1.KubernetesVersionBundle{
				KubernetesVersion: newVersion.kubernetesVersion,
				KubeletVersion:    newVersion.kubeletVersion,
				UpgraderImage:     newVersion.upgraderImage,
			},
			// This might be empty for other CP providers, that's ok, we will only use it when necessary
			KubeadmClusterConfig: req.NewMachine.Machine.Annotations[controlplanev1.KubeadmClusterConfigurationAnnotation],
			EtcdVersion:          getLocalEtcdVersionIfSet(req.NewMachine.BootstrapConfig),
			CoreDNSVersion:       getCoreDNSVersionIfSet(req.NewMachine.BootstrapConfig),
		},
	}

	if err := s.client.Create(ctx, cpUpgrade); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}

	if apierrors.IsAlreadyExists(err) {
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(cpUpgrade), cpUpgrade); err != nil {
			return nil, err
		}

		if cpUpgrade.Status.Ready {
			logger.Info("Control plane upgrade has already been completed, no need to enqueue", "upgrade", klog.KObj(cpUpgrade))
			return &upgrade.ExternalStrategyResponse{Accepted: true}, nil
		}
	}

	s.queue <- cpUpgrade

	logger.Info("Control plane upgrade accepted")
	return &upgrade.ExternalStrategyResponse{Accepted: true}, nil
}

func getLocalEtcdVersionIfSet(bootstrapConfig *unstructured.Unstructured) *string {
	return getNestedStringIfSet(bootstrapConfig, "spec", "clusterConfiguration", "etcd", "local", "imageTag")
}

func getCoreDNSVersionIfSet(bootstrapConfig *unstructured.Unstructured) *string {
	return getNestedStringIfSet(bootstrapConfig, "spec", "clusterConfiguration", "dns", "imageTag")
}

func getNestedStringIfSet(o *unstructured.Unstructured, path ...string) *string {
	v, ok, _ := unstructured.NestedString(o.Object, path...)
	if !ok {
		return nil
	}
	return &v
}

var machineAllowedChanges = [][]string{
	{"version"},
	{"nodeDeletionTimeout"},
	{"providerID"},
	{"bootstrap", "configRef", "name"},
	{"bootstrap", "configRef", "uid"},
	{"bootstrap", "dataSecretName"},
	{"infrastructureRef", "name"},
	{"infrastructureRef", "uid"},
}

var acceptedBootstrapConfigAllowedPaths = map[string][][]string{
	"KubeadmConfig": {
		{"clusterConfiguration", "etcd", "local", "imageTag"},
		{"clusterConfiguration", "dns", "imageTag"},
		{"clusterConfiguration", "networking", "podSubnet"},
		{"clusterConfiguration", "networking", "serviceSubnet"},
		{"clusterConfiguration", "clusterName"},
		{"clusterConfiguration", "controlPlaneEndpoint"},
		{"clusterConfiguration", "kubernetesVersion"},
		{"clusterConfiguration", "controlPlaneEndpoint"},
		{"clusterConfiguration", "kubernetesVersion"},
		{"joinConfiguration", "controlPlane"},
		{"joinConfiguration", "discovery", "bootstrapToken"},
	},
}

var kubeadmConfigExtraPathsForFirstCPNode = [][]string{
	{"initConfiguration"},
	{"joinConfiguration", "nodeRegistration", "criSocket"},
	{"joinConfiguration", "nodeRegistration", "imagePullPolicy"},
	{"joinConfiguration", "nodeRegistration", "kubeletExtraArgs", "anonymous-auth"},
	{"joinConfiguration", "nodeRegistration", "kubeletExtraArgs", "cloud-provider"},
	{"joinConfiguration", "nodeRegistration", "kubeletExtraArgs", "read-only-port"},
	{"joinConfiguration", "nodeRegistration", "kubeletExtraArgs", "tls-cipher-suites"},
	{"joinConfiguration", "nodeRegistration", "name"},
}

var kubeadmConfigExtraPathsForInit = [][]string{
	{"initConfiguration", "nodeRegistration", "criSocket"},
	{"initConfiguration", "nodeRegistration", "imagePullPolicy"},
	{"initConfiguration", "nodeRegistration", "kubeletExtraArgs", "anonymous-auth"},
	{"initConfiguration", "nodeRegistration", "kubeletExtraArgs", "cloud-provider"},
	{"initConfiguration", "nodeRegistration", "kubeletExtraArgs", "read-only-port"},
	{"initConfiguration", "nodeRegistration", "kubeletExtraArgs", "tls-cipher-suites"},
	{"initConfiguration", "nodeRegistration", "name"},
}

var acceptedInfraMachineAllowedPaths = map[string][][]string{
	"VSphereMachine": {
		{"providerID"},
	},
}

// acceptUpgradeRequest checks if the upgrade request is supported by this strategy.
// Right now it only supports updating the machine k8s version.
// TODO: allow for vspheremachine template field to change.
func (s *ControlPlaneExternalStrategy) acceptUpgradeRequest(ctx context.Context, machineSpecs []upgrade.MachineSpec, req upgrade.ExternalStrategyRequest) (accepted bool, reason string, err error) {
	if _, ok := supportedVersions[*req.NewMachine.Machine.Spec.Version]; !ok {
		return false, fmt.Sprintf("New kubernetes version %s is not supported", *req.NewMachine.Machine.Spec.Version), nil
	}

	for _, spec := range machineSpecs {
		if _, ok := acceptedBootstrapConfigAllowedPaths[spec.BootstrapConfig.GetKind()]; !ok {
			return false, fmt.Sprintf("Bootstrap config kind %s is not supported", spec.BootstrapConfig.GetKind()), nil
		}

		if _, ok := acceptedInfraMachineAllowedPaths[spec.InfraMachine.GetKind()]; !ok {
			return false, fmt.Sprintf("Infra machine kind %s is not supported", spec.InfraMachine.GetKind()), nil
		}

		s := spec
		changes, err := computeAllChanges(&s, req.NewMachine)
		if err != nil {
			return false, "", err
		}

		allowed := &machineSpedChanges{
			machinePaths:         machineAllowedChanges,
			infraMachinePaths:    acceptedInfraMachineAllowedPaths[spec.InfraMachine.GetKind()],
			bootstrapConfigPaths: append([][]string(nil), acceptedBootstrapConfigAllowedPaths[spec.BootstrapConfig.GetKind()]...),
		}

		if spec.BootstrapConfig.GetKind() == "KubeadmConfig" {
			old, new := &bootstrapv1.KubeadmConfig{}, &bootstrapv1.KubeadmConfig{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec.BootstrapConfig.Object, old); err != nil {
				return false, "", err
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(req.NewMachine.BootstrapConfig.Object, new); err != nil {
				return false, "", err
			}

			// this was probably the first CP node, which only has initConfigurationSet
			if old.Spec.JoinConfiguration == nil {
				allowed.bootstrapConfigPaths = append(allowed.bootstrapConfigPaths, kubeadmConfigExtraPathsForFirstCPNode...)
			}
		}

		notAllowed := listNotAllowedChanges(
			&s,
			changes,
			allowed,
		)
		if len(notAllowed) > 0 {
			return false, fmt.Sprintf("Requested changes are not supported: %s", strings.Join(notAllowed, ", ")), nil
		}
	}

	return true, "", nil
}

var supportedVersions = map[string]kubernetesVersionBundle{
	"v1.27.5-eks-1-27-12": {
		kubernetesVersion: "v1.27.5-eks-1-27-12",
		kubeletVersion:    "v1.27.5-eks-1b84072",
		upgraderImage:     "public.ecr.aws/i0f3w2d9/eks-d-in-place-upgrader:v1-27-eks-d-12",
	},
}

func (s *ControlPlaneExternalStrategy) workQueue() {
	logger := ctrl.Log
	logger = logger.WithName("inplaceControlPlaneUpgrader")
	for req := range s.queue {
		ctx := context.TODO()
		ctx = ctrl.LoggerInto(ctx, logger)

		cp, err := s.getControlPlane(ctx, req)
		if err != nil {
			logger.Error(err, "Reading cp definition, requeueing hopping this is transient")
			s.queue <- req
			time.Sleep(time.Second)
			continue
		}

		workloadClusterClient, err := s.tracker.GetClient(ctx, client.ObjectKeyFromObject(cp.Cluster))
		if err != nil {
			logger.Error(err, "Failed getting workload cluster client, requeueing hopping this is transient")
			s.queue <- req
			time.Sleep(time.Second)
			continue
		}

		newVersion := supportedVersions[req.Spec.NewVersion.KubernetesVersion]

		upgrader := controlPlaneUpgrader{
			request:          req,
			machines:         cp.MachinesRequireUpgrade,
			workloadClient:   workloadClusterClient,
			managementClient: s.client,
			newVersion:       newVersion,
		}

		if err := upgrader.run(ctx); err != nil {
			logger.Error(err, "Failed upgrading machines, requeueing hopping this is transient")
			s.queue <- req
			time.Sleep(time.Second)
			continue
		}

		if err := s.updateMachinesSpecToMeetRequest(ctx, cp, req); err != nil {
			logger.Error(err, "Failed updating version machines, requeueing hopping this is transient")
			s.queue <- req
			time.Sleep(time.Second)
			continue
		}
	}

	logger.Info("ERROR: Queue worker finishing, which should never happen")
}

// TODO: make this part of upgrader.run so we can update each machine as it gets upgraded as opposed to all of them at the end
// This way the upgrade process is also reflected in the Machine and KCP objects
func (s *ControlPlaneExternalStrategy) updateMachinesSpecToMeetRequest(ctx context.Context, cp *controlPlane, req *inplacev1.ControlPlaneUpgrade) error {
	logger := ctrl.LoggerFrom(ctx)
	specs, err := readMachineSpecs(ctx, s.client, cp.MachinesRequireUpgrade)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		m := &clusterv1.Machine{}
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(spec.Machine), m); err != nil {
			return err
		}

		if updatedMachine, updated := updateMachineIfNeeded(spec.Machine, req); updated {
			logger.Info("Updating upgraded machine to match new upgraded spec", "machine", klog.KObj(updatedMachine))
			if err := s.client.Update(ctx, updatedMachine); err != nil {
				return err
			}
		}

		if updatedBootstrap, updated, err := updateBootstrapConfigIfNeeded(spec.BootstrapConfig, req); err != nil {
			return err
		} else if updated {
			logger.Info("Updating bootstrap config to match new upgraded spec", "bootstrapConfig", klog.KObj(updatedBootstrap))
			if err := s.client.Update(ctx, updatedBootstrap); err != nil {
				return err
			}
		}
	}

	return nil
}

func updateMachineIfNeeded(old *clusterv1.Machine, upgrade *inplacev1.ControlPlaneUpgrade) (*clusterv1.Machine, bool) {
	updatedMachine := old.DeepCopy()
	kubeVersion := upgrade.Spec.NewVersion.KubernetesVersion

	var changed bool
	if *updatedMachine.Spec.Version != kubeVersion {
		updatedMachine.Spec.Version = pointer.String(kubeVersion)
		changed = true
	}

	if configAnnotation, ok := updatedMachine.Annotations[controlplanev1.KubeadmClusterConfigurationAnnotation]; ok &&
		configAnnotation != upgrade.Spec.KubeadmClusterConfig {
		updatedMachine.Annotations[controlplanev1.KubeadmClusterConfigurationAnnotation] = upgrade.Spec.KubeadmClusterConfig
		changed = true
	}

	return updatedMachine, changed
}

func updateBootstrapConfigIfNeeded(oldU *unstructured.Unstructured, upgrade *inplacev1.ControlPlaneUpgrade) (*unstructured.Unstructured, bool, error) {
	if oldU.GetKind() != "KubeadmConfig" {
		return oldU, false, nil
	}

	old := &bootstrapv1.KubeadmConfig{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(oldU.Object, old); err != nil {
		return nil, false, err
	}
	var changed bool
	if upgrade.Spec.EtcdVersion != nil && (old.Spec.ClusterConfiguration.Etcd.Local.ImageTag != *upgrade.Spec.EtcdVersion) {
		old.Spec.ClusterConfiguration.Etcd.Local.ImageTag = *upgrade.Spec.EtcdVersion
		changed = true
	}
	if upgrade.Spec.CoreDNSVersion != nil && (old.Spec.ClusterConfiguration != nil && old.Spec.ClusterConfiguration.DNS.ImageTag != *upgrade.Spec.CoreDNSVersion) {
		if old.Spec.ClusterConfiguration == nil {
			old.Spec.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{}
		}
		old.Spec.ClusterConfiguration.DNS.ImageTag = *upgrade.Spec.CoreDNSVersion
		changed = true
	}

	if !changed {
		return oldU, false, nil
	}

	newU, err := runtime.DefaultUnstructuredConverter.ToUnstructured(old)
	if err != nil {
		return nil, false, err
	}

	return &unstructured.Unstructured{Object: newU}, changed, nil
}

// objToRef returns a reference to the given object.
func objToRef(obj client.Object) *corev1.ObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return &corev1.ObjectReference{
		Kind:       gvk.Kind,
		APIVersion: gvk.GroupVersion().String(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}

func machinesToObjRefs(machines collections.Machines) []corev1.ObjectReference {
	refs := make([]corev1.ObjectReference, 0, len(machines))
	for _, m := range machines {
		refs = append(refs, *objToRef(m))
	}

	return refs
}

type controlPlane struct {
	Cluster                *clusterv1.Cluster
	ControlPlane           *corev1.ObjectReference
	MachinesRequireUpgrade collections.Machines
}

func (s *ControlPlaneExternalStrategy) getControlPlane(ctx context.Context, upgradeRequest *inplacev1.ControlPlaneUpgrade) (*controlPlane, error) {
	cluster := &clusterv1.Cluster{}
	if err := s.client.Get(ctx, keyFromRef(upgradeRequest.Spec.Cluster), cluster); err != nil {
		return nil, err
	}

	machines := make([]*clusterv1.Machine, 0, len(upgradeRequest.Spec.MachinesRequireUpgrade))
	for _, mr := range upgradeRequest.Spec.MachinesRequireUpgrade {
		m := &clusterv1.Machine{}
		if err := s.client.Get(ctx, keyFromRef(mr), m); err != nil {
			return nil, err
		}

		machines = append(machines, m)
	}

	return &controlPlane{
		Cluster:                cluster,
		ControlPlane:           &upgradeRequest.Spec.ControlPlane,
		MachinesRequireUpgrade: collections.FromMachines(machines...),
	}, nil
}

func keyFromRef(ref corev1.ObjectReference) client.ObjectKey {
	return client.ObjectKey{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}
}
