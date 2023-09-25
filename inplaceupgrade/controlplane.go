// Package inplaceupgrade provides in place upgrade functionality
// for capi managed kubernetes nodes.
// This should probably not leave in the capi repo, since it's just
// one of many ways to implement an upgrade strategy. This is just here
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
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
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
		queue:   make(chan *inplacev1.ControlPlaneUpgrade, 1000),
	}

	go s.workQueue()

	return s
}

func (s *ControlPlaneExternalStrategy) Call(ctx context.Context, req upgrade.ExternalStrategyRequest) (*upgrade.ExternalStrategyResponse, error) {
	logger := ctrl.LoggerFrom(ctx)
	logger = logger.WithValues("externalStrategy", "inplaceUpgrades")

	accepted, reason, err := s.acceptUpgradeRequest(req)
	if err != nil {
		return nil, err
	}

	if !accepted {
		logger.Info("Control plane upgrade not accepted", "reason", reason)
		return &upgrade.ExternalStrategyResponse{
			Accepted: false,
			Reason:   reason,
		}, nil
	}

	newVersion := supportedVersions[*req.NewMachine.Machine.Spec.Version]

	cpUpgradeName := strings.ReplaceAll(
		// TODO: don't use resource version to avoid duplicated entries when the status changes. Either use generation or don't use anything and assume k8s version is enough
		fmt.Sprintf("%s-%s", req.ControlPlane.Name, newVersion.kubernetesVersion),
		".", "-",
	)

	// TODO: add owner reference here to the KCP
	cpUpgrade := &inplacev1.ControlPlaneUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cpUpgradeName,
			Namespace: req.ControlPlane.Namespace,
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

// acceptUpgradeRequest checks if the upgrade request is supported by this strategy.
// Right now it only supports updating the machine k8s version.
// TODO: verify that content of infrastructureRef and bootstrap.configRef don't change
// TODO: allow for etcd version in kubeadmconfig cluster configuration to change
// TODO: allow for vspheremachine template field to change
func (s *ControlPlaneExternalStrategy) acceptUpgradeRequest(req upgrade.ExternalStrategyRequest) (accepted bool, reason string, err error) {
	if _, ok := supportedVersions[*req.NewMachine.Machine.Spec.Version]; !ok {
		return false, fmt.Sprintf("new kubernetes version %s is not supported", *req.NewMachine.Machine.Spec.Version), nil
	}

	for _, machine := range req.MachinesRequireUpgrade {
		changedPaths, err := notAllowedPathChanged(machine.Spec, req.NewMachine.Machine.Spec, machineAllowedChanges)
		if err != nil {
			return false, "", err
		}

		if len(changedPaths) > 0 {
			paths := make([]string, 0, len(changedPaths))
			for _, p := range changedPaths {
				paths = append(paths, strings.Join(p, "."))
			}
			return false, fmt.Sprintf("not supported changes for machines %s: %s", machine.Name, strings.Join(paths, "|")), nil
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

		if err := s.updateMachinesVersion(ctx, cp, newVersion); err != nil {
			logger.Error(err, "Failed updating version machines, requeueing hopping this is transient")
			s.queue <- req
			time.Sleep(time.Second)
			continue
		}
	}
}

func (s *ControlPlaneExternalStrategy) updateMachinesVersion(ctx context.Context, cp *controlPlane, version kubernetesVersionBundle) error {
	logger := ctrl.LoggerFrom(ctx)
	for _, machine := range cp.MachinesRequireUpgrade {
		m := &clusterv1.Machine{}
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(machine), m); err != nil {
			return err
		}

		logger.Info("Updating upgraded machine's version in spec", "machine", klog.KObj(machine), "version", version.kubernetesVersion)
		m.Spec.Version = pointer.String(version.kubernetesVersion)
		if err := s.client.Update(ctx, m); err != nil {
			return err
		}

		// TODO: update bootstrap config with new kubeadmconfig if etcd version has changed
	}

	return nil
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
