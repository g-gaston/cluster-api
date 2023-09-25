// Package upgrade package implements common functionality for upgrades
// of a control plane that might be used by controlplane providers.
package upgrade

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	"sigs.k8s.io/cluster-api/util/collections"
)

// ExternalStrategyRequest is the input to an external upgrade
// strategy implementer.
type ExternalStrategyRequest struct {
	Cluster                *clusterv1.Cluster
	ControlPlane           *corev1.ObjectReference
	MachinesRequireUpgrade collections.Machines
	NewMachine             *NewMachineSpec
}

type NewMachineSpec struct {
	Machine         *clusterv1.Machine
	BootstrapConfig *unstructured.Unstructured
	InfraMachine    *unstructured.Unstructured
}

// ExternalStrategyResponse is the response from an external
// upgrade strategy implementer.
type ExternalStrategyResponse struct {
	Accepted bool
	Reason   string
}

type ExternalStrategiesExtensionClient struct {
	extensions []ExternalStrategyExtension
}

type ExternalStrategyExtension interface {
	Call(ctx context.Context, req ExternalStrategyRequest) (*ExternalStrategyResponse, error)
}

func (c *ExternalStrategiesExtensionClient) Register(extensions ...ExternalStrategyExtension) {
	c.extensions = append(c.extensions, extensions...)
}

func (c *ExternalStrategiesExtensionClient) CallUntilAccepted(ctx context.Context, req ExternalStrategyRequest) (*ExternalStrategyResponse, error) {
	for _, e := range c.extensions {
		resp, err := e.Call(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Accepted {
			return resp, nil
		}
	}

	return nil, nil
}
