package inplaceupgrade

import (
	"context"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inplacev1 "sigs.k8s.io/cluster-api/inplaceupgrade/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/collections"
)

type kubernetesVersionBundle struct {
	kubernetesVersion, kubeletVersion, upgraderImage string
}

type controlPlaneUpgrader struct {
	request                          *inplacev1.ControlPlaneUpgrade
	machines                         collections.Machines
	workloadClient, managementClient client.Client
	newVersion                       kubernetesVersionBundle

	nodesUpgraded int64
}

func (u *controlPlaneUpgrader) run(ctx context.Context) error {
	// first update status with number of pending nodes
	cpUpgrade := &inplacev1.ControlPlaneUpgrade{}
	if err := u.managementClient.Get(ctx, client.ObjectKeyFromObject(u.request), cpUpgrade); err != nil {
		return err
	}

	cpUpgrade.Status.RequireUpgrade = int64(len(u.machines))
	if err := u.managementClient.Status().Update(ctx, cpUpgrade); err != nil {
		return err
	}

	if err := setup(ctx, u.workloadClient); err != nil {
		return err
	}

	nodes, err := buildMachineNodes(ctx, u.workloadClient, u.machines)
	if err != nil {
		return err
	}

	firstCPNode := nodes[0]
	if err := u.upgradeMachine(ctx, cpUpgrade, firstCPNode, upgradeFirstControlPlanePod); err != nil {
		return err
	}

	for _, node := range nodes[1:] {
		n := node
		if err := u.upgradeMachine(ctx, cpUpgrade, n, upgradeRestControlPlanePod); err != nil {
			return errors.Wrap(err, "upgrading CP node")
		}
	}

	cpUpgrade.Status.Ready = true
	return u.managementClient.Status().Update(ctx, cpUpgrade)
}

type podGenerator func(nodeName string, version kubernetesVersionBundle) *corev1.Pod

func (u *controlPlaneUpgrader) upgradeMachine(ctx context.Context, cpUpgrade *inplacev1.ControlPlaneUpgrade, target *machineNode, buildPod podGenerator) error {
	upgrader := buildPod(target.node.Name, u.newVersion)
	m := &machineUpgrader{
		machineNode:            target,
		workloadClient:         u.workloadClient,
		managementClient:       u.managementClient,
		upgraderGroupName:      cpUpgrade.Name,
		upgraderGroupNamespace: cpUpgrade.Namespace,
		upgrader:               upgrader,
		newVersion:             u.newVersion,
		etcdVersion:            u.request.Spec.EtcdVersion,
	}

	if err := m.upgrade(ctx); err != nil {
		return err
	}

	u.nodesUpgraded++
	cpUpgrade.Status.Upgraded = u.nodesUpgraded

	return u.managementClient.Status().Update(ctx, cpUpgrade)
}

func upgradeFirstControlPlanePod(nodeName string, version kubernetesVersionBundle) *corev1.Pod {
	p := upgradePod(nodeName)
	p.Spec.InitContainers = containersForUpgrade(version.upgraderImage, nodeName, "kubeadm_in_first_cp", version.kubernetesVersion)
	p.Spec.Containers = []corev1.Container{printAndCleanupContainer(version.upgraderImage)}

	return p
}

func upgradeRestControlPlanePod(nodeName string, version kubernetesVersionBundle) *corev1.Pod {
	p := upgradePod(nodeName)
	p.Spec.InitContainers = containersForUpgrade(version.upgraderImage, nodeName, "kubeadm_in_rest_cp")
	p.Spec.Containers = []corev1.Container{printAndCleanupContainer(version.upgraderImage)}

	return p
}
