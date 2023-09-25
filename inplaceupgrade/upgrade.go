package inplaceupgrade

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
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

func buildMachineNodes(ctx context.Context, c client.Client, machines collections.Machines) ([]*machineNode, error) {
	mn := make([]*machineNode, 0, len(machines))

	for _, m := range machines {
		n := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: m.Status.NodeRef.Namespace, Name: m.Status.NodeRef.Name}, n); err != nil {
			return nil, err
		}

		mn = append(mn, &machineNode{
			machine: m,
			node:    n,
		})
	}

	// Sort nodes by name to have reproducible logic
	slices.SortFunc(mn, func(a, b *machineNode) int {
		if a.node.Name < b.node.Name {
			return 1
		} else if a.node.Name > b.node.Name {
			return -1
		}

		return 0
	})

	return mn, nil
}

func setup(ctx context.Context, c client.Client) error {
	logger := ctrl.LoggerFrom(ctx)
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "eksa-system"}},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "eks-d-upgrader",
				Namespace: "eksa-system",
			},
		},
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "drainer",
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Resources: []string{"pods"},
					Verbs:     []string{"get", "list", "delete"},
				},
				{
					APIGroups: []string{""},
					Resources: []string{"pods/eviction"},
					Verbs:     []string{"create", "delete"},
				},
				{
					APIGroups: []string{""},
					Resources: []string{"nodes"},
					Verbs:     []string{"get", "list", "patch"},
				},
				{
					APIGroups: []string{"apps"},
					Resources: []string{"daemonsets"},
					Verbs:     []string{"get", "list"},
				},
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "eks-d-upgrader-drainer",
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Namespace: "eksa-system",
					Name:      "eks-d-upgrader",
				},
			},
			RoleRef: rbacv1.RoleRef{
				Kind:     "ClusterRole",
				Name:     "drainer",
				APIGroup: "rbac.authorization.k8s.io",
			},
		},
	}

	for _, obj := range objs {
		logger.Info("Creating setup resource", "obj", klog.KObj(obj))
		if err := createNoError(ctx, c, obj); err != nil {
			return err
		}
	}

	return nil
}

func createNoError(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

type machineNode struct {
	machine *clusterv1.Machine
	node    *corev1.Node
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
	}

	if err := m.upgrade(ctx); err != nil {
		return err
	}

	u.nodesUpgraded++
	cpUpgrade.Status.Upgraded = u.nodesUpgraded
	if err := u.managementClient.Status().Update(ctx, cpUpgrade); err != nil {
		return err
	}

	return nil
}

func upgradeFirstControlPlanePod(nodeName string, version kubernetesVersionBundle) *corev1.Pod {
	p := upgradePod(nodeName)
	p.Spec.InitContainers = containersForUpgrade(version.upgraderImage, nodeName, "kubeadm_in_first_cp", version.kubernetesVersion)
	p.Spec.Containers = []corev1.Container{printAndCleanupContainer()}

	return p
}

func upgradeRestControlPlanePod(nodeName string, version kubernetesVersionBundle) *corev1.Pod {
	p := upgradePod(nodeName)
	p.Spec.InitContainers = containersForUpgrade(version.upgraderImage, nodeName, "kubeadm_in_rest_cp")
	p.Spec.Containers = []corev1.Container{printAndCleanupContainer()}

	return p
}

type machineUpgrader struct {
	*machineNode

	workloadClient, managementClient client.Client

	upgraderGroupName, upgraderGroupNamespace string

	newVersion kubernetesVersionBundle

	upgrader *corev1.Pod
}

func (u *machineUpgrader) upgrade(ctx context.Context) error {
	// TODO: add owner reference here to the machine
	nodeUpgrade := &inplacev1.NodeUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", u.node.Name, u.upgraderGroupName),
			Namespace: u.upgraderGroupNamespace,
		},
		Spec: inplacev1.NodeUpgradeSpec{
			Machine: *objToRef(u.machine),
			Node:    *objToRef(u.node),
			NewVersion: inplacev1.KubernetesVersionBundle{
				KubernetesVersion: u.newVersion.kubernetesVersion,
				KubeletVersion:    u.newVersion.kubeletVersion,
				UpgraderImage:     u.newVersion.upgraderImage,
			},
		},
	}

	if err := u.managementClient.Create(ctx, nodeUpgrade); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	// read after create to be able to update later
	k := client.ObjectKeyFromObject(nodeUpgrade)
	nodeUpgrade = &inplacev1.NodeUpgrade{}
	if err := u.managementClient.Get(ctx, k, nodeUpgrade); err != nil {
		return err
	}

	if err := u.upgradeNode(ctx, nodeUpgrade); err != nil {
		return err
	}

	nodeUpgrade.Status.Completed = true
	nodeUpgrade.Status.Phase = "Completed"

	if err := u.managementClient.Status().Update(ctx, nodeUpgrade); err != nil {
		return err
	}

	return nil
}

var containerNameToPhase = map[string]string{
	"components-copier":        "Copying components",
	"containerd-upgrader":      "Upgrading containerd",
	"cni-plugins-upgrader":     "Upgrading CNI plugins",
	"kubeadm-upgrader":         "Kubeadm upgrade",
	"drain":                    "Draining",
	"kubelet-kubectl-upgrader": "Upgrading kubelet",
	"uncordon":                 "Uncordoning",
}

func phase(containerName string) string {
	p, ok := containerNameToPhase[containerName]
	if !ok {
		return containerName
	}

	return p
}

func (u *machineUpgrader) upgradeNode(ctx context.Context, nodeUpgrade *inplacev1.NodeUpgrade) error {
	logger := ctrl.LoggerFrom(ctx)
	logger = logger.WithValues("node", klog.KObj(u.node), "pod", klog.KObj(u.upgrader))
	ctx = ctrl.LoggerInto(ctx, logger)
	logger.Info("Upgrading node")

	key := client.ObjectKeyFromObject(u.upgrader)
	pod := &corev1.Pod{}

	if err := u.workloadClient.Get(ctx, key, pod); apierrors.IsNotFound(err) {
		if u.node.Status.NodeInfo.KubeletVersion == u.newVersion.kubeletVersion {
			logger.Info("Node was already in desired version", "kubeletVersion", u.newVersion.kubeletVersion)
			return nil
		}

		logger.Info("Creating upgrader")
		if err := u.workloadClient.Create(ctx, u.upgrader); err != nil {
			log.Fatal(err)
		}
	} else if err == nil {
		logger.Info("Pod exists, skipping creation")
	} else {
		return err
	}

	for _, c := range u.upgrader.Spec.InitContainers {
		nodeUpgrade.Status.Phase = phase(c.Name)

		if err := u.managementClient.Status().Update(ctx, nodeUpgrade); err != nil {
			return err
		}

		if err := waitForInitContainerToFinish(ctx, u.workloadClient, u.upgrader, c); err != nil {
			return err
		}
	}

	nodeUpgrade.Status.Phase = "Cleaning up"
	if err := u.managementClient.Status().Update(ctx, nodeUpgrade); err != nil {
		return err
	}

	if err := waitForPodToFinish(ctx, u.workloadClient, u.upgrader); err != nil {
		return err
	}

	logger.Info("Deleting upgrader pod after success")
	if err := u.workloadClient.Delete(ctx, pod, &client.DeleteOptions{}); err != nil {
		return err
	}

	return nil
}

func waitForInitContainerToFinish(ctx context.Context, c client.Client, pod *corev1.Pod, container corev1.Container) error {
	logger := ctrl.LoggerFrom(ctx)
	logger = logger.WithValues("container", container.Name)
	key := client.ObjectKeyFromObject(pod)
	pod = &corev1.Pod{}
waiter:
	for {
		if err := c.Get(ctx, key, pod); apierrors.IsNotFound(err) {
			logger.Info("Upgrader pod doesn't exist yet, retrying")
			continue
		} else if isConnectionRefusedAPIServer(err) {
			logger.Info("API server is not up, probably is restarting because of upgrade, retrying")
			time.Sleep(5 * time.Second)
			continue
		} else if err != nil {
			return err
		}

		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != container.Name {
				continue
			}

			switch {
			case status.State.Waiting != nil:
				logger.Info("Container is waiting", "reason", status.State.Waiting.Reason)
			case status.State.Running != nil:
				logger.Info("Container is running", "age", time.Since(status.State.Running.StartedAt.Time))
			case status.State.Terminated != nil:
				logger.Info("Container has finished", "reason", status.State.Terminated.Reason)
				break waiter
			default:
				logger.Info("Container is waiting with no data available")
			}
		}

		time.Sleep(5 * time.Second)
	}

	return nil
}

func waitForPodToFinish(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	logger := ctrl.LoggerFrom(ctx)
	key := client.ObjectKeyFromObject(pod)
	pod = &corev1.Pod{}

waiter:
	for {
		if err := c.Get(ctx, key, pod); apierrors.IsNotFound(err) {
			logger.Info("Upgrader pod doesn't exist yet, retrying")
			continue
		} else if isConnectionRefusedAPIServer(err) {
			logger.Info("API server is not up, probably is restarting because of upgrade, retrying")
			time.Sleep(5 * time.Second)
			continue
		} else if err != nil {
			return err
		}

		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			logger.Info("Upgrader Pod succeed, upgrade process for node is done")
			break waiter
		case corev1.PodFailed:
			logger.Info("Upgrader Pod has failed", "failureReason", pod.Status.Reason)
			return errors.Errorf("upgrader Pod %s has failed: %s", pod.Name, pod.Status.Reason)
		default:
			logger.Info("Upgrader Pod has not finished yet", "phase", pod.Status.Phase)
		}

		time.Sleep(5 * time.Second)
	}

	return nil
}
