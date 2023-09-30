package inplaceupgrade

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controlplane/upgrade"
	inplacev1 "sigs.k8s.io/cluster-api/inplaceupgrade/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/collections"
)

func readMachineSpecs(ctx context.Context, c client.Client, machines collections.Machines) ([]upgrade.MachineSpec, error) {
	specs := make([]upgrade.MachineSpec, 0, len(machines))
	for _, m := range machines {
		bootstrap, err := external.Get(ctx, c, m.Spec.Bootstrap.ConfigRef, m.Spec.Bootstrap.ConfigRef.Namespace)
		if err != nil {
			return nil, err
		}

		infraMachine, err := external.Get(ctx, c, &m.Spec.InfrastructureRef, m.Spec.InfrastructureRef.Namespace)
		if err != nil {
			return nil, err
		}

		specs = append(specs, upgrade.MachineSpec{
			Machine:         m,
			BootstrapConfig: bootstrap,
			InfraMachine:    infraMachine,
		})
	}

	return specs, nil
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

type machineNode struct {
	machine *clusterv1.Machine
	node    *corev1.Node
}

type machineUpgrader struct {
	*machineNode

	workloadClient, managementClient client.Client

	upgraderGroupName, upgraderGroupNamespace string

	newVersion  kubernetesVersionBundle
	etcdVersion *string

	upgrader *corev1.Pod
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

func (u *machineUpgrader) upgrade(ctx context.Context) error {
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

	nodeUpgrade := &inplacev1.NodeUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", u.node.Name, u.upgraderGroupName),
			Namespace: u.upgraderGroupNamespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(u.machine, clusterv1.GroupVersion.WithKind("Machine")),
			},
		},
		Spec: inplacev1.NodeUpgradeSpec{
			Machine: *objToRef(u.machine),
			Node:    *objToRef(u.node),
			NewVersion: inplacev1.KubernetesVersionBundle{
				KubernetesVersion: u.newVersion.kubernetesVersion,
				KubeletVersion:    u.newVersion.kubeletVersion,
				UpgraderImage:     u.newVersion.upgraderImage,
			},
			EtcdVersion: u.etcdVersion,
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

	nodeUpgrade.Status.Completed = true
	nodeUpgrade.Status.Phase = "Completed"

	if err := u.managementClient.Status().Update(ctx, nodeUpgrade); err != nil {
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
