/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rosa

import (
	"fmt"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	hypershiftipnet "github.com/openshift/hypershift/api/util/ipnet"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
)

// Default CIDRs used when RosaControlPlaneSpec.Network does not specify them.
// They match the rosa CLI defaults for ROSA hosted control plane clusters.
const (
	defaultSeaplaneMachineCIDR = "10.0.0.0/16"
	defaultSeaplanePodCIDR     = "10.128.0.0/14"
	defaultSeaplaneServiceCIDR = "172.30.0.0/16"

	// defaultSeaplaneHostPrefix is the node pod CIDR prefix allocated from the
	// pod network.
	defaultSeaplaneHostPrefix = 23
)

// SeaplaneSubnetInfo carries the control plane placement derived from the
// first subnet of the cluster.
type SeaplaneSubnetInfo struct {
	// SubnetID is the subnet used for control plane cloud resources.
	SubnetID string
	// VPCID is the VPC the subnet belongs to.
	VPCID string
	// Zone is the availability zone of the subnet.
	Zone string
}

// BuildSeaplaneCluster maps a RosaControlPlane spec onto a Seaplane v2
// Platform API Cluster create object. It is the seaplane equivalent of
// buildOCMClusterSpec. releaseImage is the OCP release payload image and
// subnetInfo describes the first (control plane) subnet. The DNS base domain
// is intentionally not set: the Platform API manages DNS allocation and
// rejects customer-supplied values with
// "spec.hostedCluster.dns.baseDomain: field is platform-managed".
func BuildSeaplaneCluster(
	controlPlaneSpec rosacontrolplanev1.RosaControlPlaneSpec,
	roleConfig *expinfrav1.ROSARoleConfig,
	rosaNet *expinfrav1.ROSANetwork,
	releaseImage string,
	subnetInfo SeaplaneSubnetInfo,
) (*seaplanev1alpha1.Cluster, error) {
	if releaseImage == "" {
		return nil, fmt.Errorf("seaplane: release image is required (set rosaControlPlaneSpec.releaseImage or %s)", SeaplaneReleaseImageEnvVar)
	}
	if subnetInfo.SubnetID == "" {
		return nil, fmt.Errorf("seaplane: control plane subnet is required")
	}
	if subnetInfo.VPCID == "" {
		return nil, fmt.Errorf("seaplane: VPC of control plane subnet %q is required", subnetInfo.SubnetID)
	}

	oidcID := controlPlaneSpec.OIDCID
	rolesRef := controlPlaneSpec.RolesRef
	if roleConfig != nil {
		if roleConfig.Status.OIDCID != "" {
			oidcID = roleConfig.Status.OIDCID
		}
		if !isEmptyOperatorRolesRef(roleConfig.Status.OperatorRolesRef) {
			rolesRef = roleConfig.Status.OperatorRolesRef
		}
	}
	if oidcID == "" {
		return nil, fmt.Errorf("seaplane: OIDC config ID is required (set rosaControlPlaneSpec.oidcID or use rosaRoleConfigRef)")
	}
	if isEmptyRolesRef(rolesRef) {
		return nil, fmt.Errorf("seaplane: operator roles ref is required (set rosaControlPlaneSpec.rolesRef or use rosaRoleConfigRef)")
	}

	networking, err := seaplaneNetworking(controlPlaneSpec.Network)
	if err != nil {
		return nil, err
	}

	cluster := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: controlPlaneSpec.RosaClusterName},
		Spec: seaplanev1alpha1.ClusterSpec{
			OidcConfigID: oidcID,
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Release: hypershiftv1beta1.Release{
					Image: releaseImage,
				},
				Platform: seaplanev1alpha1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &seaplanev1alpha1.AWSPlatformSpec{
						Region:   controlPlaneSpec.Region,
						RolesRef: toHypershiftRolesRef(rolesRef),
						CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
							VPC: subnetInfo.VPCID,
							Zone: func() string {
								if subnetInfo.Zone != "" {
									return subnetInfo.Zone
								}
								// Fall back to the first availability zone of the spec.
								if len(controlPlaneSpec.AvailabilityZones) > 0 {
									return controlPlaneSpec.AvailabilityZones[0]
								}
								return ""
							}(),
							Subnet: &hypershiftv1beta1.AWSResourceReference{
								ID: ptr.To(subnetInfo.SubnetID),
							},
						},
						EndpointAccess: toHypershiftEndpointAccess(controlPlaneSpec.EndpointAccess),
					},
				},
				Networking: networking,
			},
		},
	}

	if controlPlaneSpec.DeleteProtection == rosacontrolplanev1.DeleteProtectionEnabled {
		cluster.Spec.DeleteProtection = ptr.To(true)
	}

	if len(controlPlaneSpec.AdditionalTags) > 0 {
		cluster.Spec.Tags = map[string]string{}
		for k, v := range controlPlaneSpec.AdditionalTags {
			cluster.Spec.Tags[k] = v
		}
	}

	return cluster, nil
}

// SeaplaneClusterSubnetID returns the control plane subnet ID for the given
// spec, resolving from spec.Subnets or the referenced ROSANetwork, mirroring
// the subnet resolution of buildOCMClusterSpec.
func SeaplaneClusterSubnetID(
	controlPlaneSpec rosacontrolplanev1.RosaControlPlaneSpec,
	rosaNet *expinfrav1.ROSANetwork,
) (string, error) {
	if controlPlaneSpec.ROSANetworkRef == nil {
		if len(controlPlaneSpec.Subnets) == 0 {
			return "", fmt.Errorf("RosaControlPlaneSpec.Subnets is empty")
		}
		return controlPlaneSpec.Subnets[0], nil
	}
	if rosaNet == nil {
		return "", fmt.Errorf("rosaNetworkRef is set but ROSANetwork is nil")
	}
	if len(rosaNet.Status.Subnets) == 0 {
		return "", fmt.Errorf("ROSANetwork %s has no subnets in status", rosaNet.Name)
	}
	return rosaNet.Status.Subnets[0].PublicSubnet, nil
}

// seaplaneNetworking maps the RosaControlPlane network spec onto the
// HyperShift cluster networking, applying rosa CLI defaults for unset CIDRs.
func seaplaneNetworking(network *rosacontrolplanev1.NetworkSpec) (hypershiftv1beta1.ClusterNetworking, error) {
	machineCIDR := defaultSeaplaneMachineCIDR
	podCIDR := defaultSeaplanePodCIDR
	serviceCIDR := defaultSeaplaneServiceCIDR
	if network != nil {
		if network.MachineCIDR != "" {
			machineCIDR = network.MachineCIDR
		}
		if network.PodCIDR != "" {
			podCIDR = network.PodCIDR
		}
		if network.ServiceCIDR != "" {
			serviceCIDR = network.ServiceCIDR
		}
	}

	machineNet, err := hypershiftipnet.ParseCIDR(machineCIDR)
	if err != nil {
		return hypershiftv1beta1.ClusterNetworking{}, fmt.Errorf("invalid machineCIDR %q: %w", machineCIDR, err)
	}
	podNet, err := hypershiftipnet.ParseCIDR(podCIDR)
	if err != nil {
		return hypershiftv1beta1.ClusterNetworking{}, fmt.Errorf("invalid podCIDR %q: %w", podCIDR, err)
	}
	serviceNet, err := hypershiftipnet.ParseCIDR(serviceCIDR)
	if err != nil {
		return hypershiftv1beta1.ClusterNetworking{}, fmt.Errorf("invalid serviceCIDR %q: %w", serviceCIDR, err)
	}

	return hypershiftv1beta1.ClusterNetworking{
		NetworkType: hypershiftv1beta1.OVNKubernetes,
		MachineNetwork: []hypershiftv1beta1.MachineNetworkEntry{
			{CIDR: *machineNet},
		},
		ClusterNetwork: []hypershiftv1beta1.ClusterNetworkEntry{
			{
				CIDR:       *podNet,
				HostPrefix: int32(defaultSeaplaneHostPrefix),
			},
		},
		ServiceNetwork: []hypershiftv1beta1.ServiceNetworkEntry{
			{CIDR: *serviceNet},
		},
	}, nil
}

// toHypershiftRolesRef maps the RosaControlPlane AWSRolesRef onto the
// HyperShift equivalent.
func toHypershiftRolesRef(rolesRef rosacontrolplanev1.AWSRolesRef) hypershiftv1beta1.AWSRolesRef {
	return hypershiftv1beta1.AWSRolesRef{
		IngressARN:              rolesRef.IngressARN,
		ImageRegistryARN:        rolesRef.ImageRegistryARN,
		StorageARN:              rolesRef.StorageARN,
		NetworkARN:              rolesRef.NetworkARN,
		KubeCloudControllerARN:  rolesRef.KubeCloudControllerARN,
		NodePoolManagementARN:   rolesRef.NodePoolManagementARN,
		ControlPlaneOperatorARN: rolesRef.ControlPlaneOperatorARN,
	}
}

// toHypershiftEndpointAccess maps the RosaControlPlane endpoint access onto
// the HyperShift equivalent.
func toHypershiftEndpointAccess(endpointAccess rosacontrolplanev1.RosaEndpointAccessType) hypershiftv1beta1.AWSEndpointAccessType {
	if endpointAccess == rosacontrolplanev1.Private {
		return hypershiftv1beta1.Private
	}
	return hypershiftv1beta1.Public
}

func isEmptyRolesRef(rolesRef rosacontrolplanev1.AWSRolesRef) bool {
	return rolesRef == rosacontrolplanev1.AWSRolesRef{}
}

func isEmptyOperatorRolesRef(rolesRef rosacontrolplanev1.AWSRolesRef) bool {
	return isEmptyRolesRef(rolesRef)
}
