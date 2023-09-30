package inplaceupgrade

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

const (
	upgradeScript = "/foo/eksa-upgrades/scripts/upgrade.sh"
)

func containersForUpgrade(image, nodeName string, kubeadmUpgradeCommand ...string) []corev1.Container {
	return []corev1.Container{
		copierContainer(image),
		nsenterContainer("containerd-upgrader", image, upgradeScript, "upgrade_containerd"),
		nsenterContainer("cni-plugins-upgrader", image, upgradeScript, "cni_plugins"),
		nsenterContainer("kubeadm-upgrader", image, append([]string{upgradeScript}, kubeadmUpgradeCommand...)...),
		drainerContainer(image, nodeName),
		nsenterContainer("kubelet-kubectl-upgrader", image, upgradeScript, "kubelet_and_kubectl"),
		uncordonContainer(image, nodeName),
	}
}

func printAndCleanupContainer(image string) corev1.Container {
	return nsenterContainer(image, "post-upgrade-status", upgradeScript, "print_status_and_cleanup")
}

func upgradePod(nodeName string) *corev1.Pod {
	dirOrCreate := corev1.HostPathDirectoryOrCreate
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("eks-d-upgrader-%s", nodeName),
			Namespace: "eksa-system",
			Labels: map[string]string{
				"ekd-d-upgrader": "true",
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "eks-d-upgrader",
			RestartPolicy:      corev1.RestartPolicyOnFailure,
			NodeName:           nodeName,
			HostPID:            true,
			Volumes: []corev1.Volume{
				{
					Name: "host-components",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/foo",
							Type: &dirOrCreate,
						},
					},
				},
			},
		},
	}
}

func copierContainer(image string) corev1.Container {
	return corev1.Container{
		Name:    "components-copier",
		Image:   image,
		Command: []string{"cp"},
		Args:    []string{"-r", "/eksa-upgrades", "/usr/host"},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "host-components",
				MountPath: "/usr/host",
			},
		},
		ImagePullPolicy: corev1.PullAlways,
	}
}

func nsenterContainer(containerName, image string, command ...string) corev1.Container {
	c := baseNsenterContainer(image)
	c.Name = containerName
	c.Args = append(c.Args, command...)
	return c
}

func baseNsenterContainer(image string) corev1.Container {
	return corev1.Container{
		Image:   image,
		Command: []string{"nsenter"},
		Args: []string{
			"--target",
			"1",
			"--mount",
			"--uts",
			"--ipc",
			"--net",
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: pointer.Bool(true),
		},
	}
}

func drainerContainer(image, nodeName string) corev1.Container {
	return corev1.Container{
		Name:            "drain",
		Image:           image,
		Command:         []string{"/eksa-upgrades/binaries/kubernetes/usr/bin/kubectl"},
		Args:            []string{"drain", nodeName, "--ignore-daemonsets", "--pod-selector", "!ekd-d-upgrader"},
		ImagePullPolicy: corev1.PullAlways,
	}
}

func uncordonContainer(image, nodeName string) corev1.Container {
	return corev1.Container{
		Name:            "uncordon",
		Image:           image,
		Command:         []string{"/eksa-upgrades/binaries/kubernetes/usr/bin/kubectl"},
		Args:            []string{"uncordon", nodeName},
		ImagePullPolicy: corev1.PullAlways,
	}
}
