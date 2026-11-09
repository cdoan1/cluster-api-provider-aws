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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
)

const (
	testReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"
	testSubnetID     = "subnet-0123456789abcdef0"
	testVPCID        = "vpc-0123456789abcdef0"
	testZone         = "us-east-1a"
)

func seaplaneTestControlPlaneSpec() rosacontrolplanev1.RosaControlPlaneSpec {
	return rosacontrolplanev1.RosaControlPlaneSpec{
		RosaClusterName: "test-cluster",
		Region:          "us-east-1",
		Version:         "4.19.0",
		Subnets:         []string{testSubnetID, "subnet-0fedcba9876543210"},
		AvailabilityZones: []string{
			testZone,
		},
		OIDCID: "test-oidc-id",
		RolesRef: rosacontrolplanev1.AWSRolesRef{
			IngressARN:              "arn:aws:iam::123456789012:role/test-cluster-ingress",
			ImageRegistryARN:        "arn:aws:iam::123456789012:role/test-cluster-image-registry",
			StorageARN:              "arn:aws:iam::123456789012:role/test-cluster-ebs-csi",
			NetworkARN:              "arn:aws:iam::123456789012:role/test-cluster-network-config",
			KubeCloudControllerARN:  "arn:aws:iam::123456789012:role/test-cluster-cloud-controller-manager",
			NodePoolManagementARN:   "arn:aws:iam::123456789012:role/test-cluster-node-pool-management",
			ControlPlaneOperatorARN: "arn:aws:iam::123456789012:role/test-cluster-control-plane-operator",
		},
	}
}

func TestBuildSeaplaneCluster(t *testing.T) {
	g := NewWithT(t)

	cluster, err := BuildSeaplaneCluster(
		seaplaneTestControlPlaneSpec(),
		nil,
		nil,
		testReleaseImage,
		SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID, Zone: testZone},
		"example.com",
	)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cluster.Name).To(Equal("test-cluster"))
	g.Expect(cluster.Spec.OidcConfigID).To(Equal("test-oidc-id"))
	g.Expect(cluster.Spec.HostedCluster.Release.Image).To(Equal(testReleaseImage))
	g.Expect(string(cluster.Spec.HostedCluster.Platform.Type)).To(Equal("AWS"))
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS).ToNot(BeNil())
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS.Region).To(Equal("us-east-1"))
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS.RolesRef.IngressARN).To(
		Equal("arn:aws:iam::123456789012:role/test-cluster-ingress"))
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS.CloudProviderConfig.VPC).To(Equal(testVPCID))
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS.CloudProviderConfig.Zone).To(Equal(testZone))
	g.Expect(*cluster.Spec.HostedCluster.Platform.AWS.CloudProviderConfig.Subnet.ID).To(Equal(testSubnetID))
	g.Expect(string(cluster.Spec.HostedCluster.Platform.AWS.EndpointAccess)).To(Equal("Public"))
	g.Expect(cluster.Spec.HostedCluster.DNS.BaseDomain).To(Equal("example.com"))
	g.Expect(string(cluster.Spec.HostedCluster.Networking.NetworkType)).To(Equal("OVNKubernetes"))
	g.Expect(cluster.Spec.HostedCluster.Networking.MachineNetwork).To(HaveLen(1))
	g.Expect(cluster.Spec.HostedCluster.Networking.MachineNetwork[0].CIDR.String()).To(Equal("10.0.0.0/16"))
	g.Expect(cluster.Spec.HostedCluster.Networking.ClusterNetwork).To(HaveLen(1))
	g.Expect(cluster.Spec.HostedCluster.Networking.ClusterNetwork[0].CIDR.String()).To(Equal("10.128.0.0/14"))
	g.Expect(cluster.Spec.HostedCluster.Networking.ClusterNetwork[0].HostPrefix).To(Equal(int32(23)))
	g.Expect(cluster.Spec.HostedCluster.Networking.ServiceNetwork[0].CIDR.String()).To(Equal("172.30.0.0/16"))
	g.Expect(cluster.Spec.DeleteProtection).To(BeNil())
}

func TestBuildSeaplaneCluster_PrivateEndpoint(t *testing.T) {
	g := NewWithT(t)

	spec := seaplaneTestControlPlaneSpec()
	spec.EndpointAccess = rosacontrolplanev1.Private
	cluster, err := BuildSeaplaneCluster(spec, nil, nil, testReleaseImage,
		SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID}, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(cluster.Spec.HostedCluster.Platform.AWS.EndpointAccess)).To(Equal("Private"))
	g.Expect(cluster.Spec.HostedCluster.DNS.BaseDomain).To(BeEmpty())
}

func TestBuildSeaplaneCluster_NetworkOverrides(t *testing.T) {
	g := NewWithT(t)

	spec := seaplaneTestControlPlaneSpec()
	spec.Network = &rosacontrolplanev1.NetworkSpec{
		MachineCIDR: "192.168.0.0/16",
		PodCIDR:     "10.10.0.0/16",
		ServiceCIDR: "172.31.0.0/16",
	}
	cluster, err := BuildSeaplaneCluster(spec, nil, nil, testReleaseImage,
		SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID}, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cluster.Spec.HostedCluster.Networking.MachineNetwork[0].CIDR.String()).To(Equal("192.168.0.0/16"))
	g.Expect(cluster.Spec.HostedCluster.Networking.ClusterNetwork[0].CIDR.String()).To(Equal("10.10.0.0/16"))
	g.Expect(cluster.Spec.HostedCluster.Networking.ServiceNetwork[0].CIDR.String()).To(Equal("172.31.0.0/16"))
}

func TestBuildSeaplaneCluster_DeleteProtectionAndTags(t *testing.T) {
	g := NewWithT(t)

	spec := seaplaneTestControlPlaneSpec()
	spec.DeleteProtection = rosacontrolplanev1.DeleteProtectionEnabled
	spec.AdditionalTags = map[string]string{"owner": "capa"}
	cluster, err := BuildSeaplaneCluster(spec, nil, nil, testReleaseImage,
		SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID}, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(*cluster.Spec.DeleteProtection).To(BeTrue())
	g.Expect(cluster.Spec.Tags).To(Equal(map[string]string{"owner": "capa"}))
}

func TestBuildSeaplaneCluster_RoleConfigPrecedence(t *testing.T) {
	g := NewWithT(t)

	roleConfig := &expinfrav1.ROSARoleConfig{
		Status: expinfrav1.ROSARoleConfigStatus{
			OIDCID: "role-config-oidc-id",
			OperatorRolesRef: rosacontrolplanev1.AWSRolesRef{
				IngressARN:             "arn:aws:iam::123456789012:role/rc-ingress",
				KubeCloudControllerARN: "arn:aws:iam::123456789012:role/rc-ccm",
				NodePoolManagementARN:  "arn:aws:iam::123456789012:role/rc-npm",
			},
		},
	}
	cluster, err := BuildSeaplaneCluster(seaplaneTestControlPlaneSpec(), roleConfig, nil, testReleaseImage,
		SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID}, "")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(cluster.Spec.OidcConfigID).To(Equal("role-config-oidc-id"))
	g.Expect(cluster.Spec.HostedCluster.Platform.AWS.RolesRef.IngressARN).To(
		Equal("arn:aws:iam::123456789012:role/rc-ingress"))
}

func TestBuildSeaplaneCluster_Errors(t *testing.T) {
	tests := []struct {
		name       string
		release    string
		subnetInfo SeaplaneSubnetInfo
		mutate     func(*rosacontrolplanev1.RosaControlPlaneSpec)
		wantErr    string
	}{
		{
			name:       "missing release image",
			release:    "",
			subnetInfo: SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID},
			wantErr:    "release image is required",
		},
		{
			name:    "missing subnet",
			release: testReleaseImage,
			wantErr: "control plane subnet is required",
		},
		{
			name:       "missing vpc",
			release:    testReleaseImage,
			subnetInfo: SeaplaneSubnetInfo{SubnetID: testSubnetID},
			wantErr:    "VPC of control plane subnet",
		},
		{
			name:       "missing oidc id",
			release:    testReleaseImage,
			subnetInfo: SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID},
			mutate: func(s *rosacontrolplanev1.RosaControlPlaneSpec) {
				s.OIDCID = ""
			},
			wantErr: "OIDC config ID is required",
		},
		{
			name:       "missing roles ref",
			release:    testReleaseImage,
			subnetInfo: SeaplaneSubnetInfo{SubnetID: testSubnetID, VPCID: testVPCID},
			mutate: func(s *rosacontrolplanev1.RosaControlPlaneSpec) {
				s.RolesRef = rosacontrolplanev1.AWSRolesRef{}
			},
			wantErr: "operator roles ref is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			spec := seaplaneTestControlPlaneSpec()
			if tt.mutate != nil {
				tt.mutate(&spec)
			}
			_, err := BuildSeaplaneCluster(spec, nil, nil, tt.release, tt.subnetInfo, "")
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
		})
	}
}

func TestSeaplaneClusterSubnetID(t *testing.T) {
	t.Run("from spec subnets", func(t *testing.T) {
		g := NewWithT(t)
		subnetID, err := SeaplaneClusterSubnetID(seaplaneTestControlPlaneSpec(), nil)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(subnetID).To(Equal(testSubnetID))
	})

	t.Run("empty subnets", func(t *testing.T) {
		g := NewWithT(t)
		spec := rosacontrolplanev1.RosaControlPlaneSpec{}
		_, err := SeaplaneClusterSubnetID(spec, nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("Subnets is empty"))
	})

	t.Run("from rosa network", func(t *testing.T) {
		g := NewWithT(t)
		spec := seaplaneTestControlPlaneSpec()
		spec.ROSANetworkRef = &corev1.LocalObjectReference{Name: "test-net"}
		rosaNet := &expinfrav1.ROSANetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "test-net"},
			Status: expinfrav1.ROSANetworkStatus{
				Subnets: []expinfrav1.ROSANetworkSubnet{
					{AvailabilityZone: testZone, PublicSubnet: "subnet-public", PrivateSubnet: "subnet-private"},
				},
			},
		}
		subnetID, err := SeaplaneClusterSubnetID(spec, rosaNet)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(subnetID).To(Equal("subnet-public"))
	})

	t.Run("rosa network without subnets", func(t *testing.T) {
		g := NewWithT(t)
		spec := seaplaneTestControlPlaneSpec()
		spec.ROSANetworkRef = &corev1.LocalObjectReference{Name: "test-net"}
		_, err := SeaplaneClusterSubnetID(spec, &expinfrav1.ROSANetwork{})
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("rosa network ref without network", func(t *testing.T) {
		g := NewWithT(t)
		spec := seaplaneTestControlPlaneSpec()
		spec.ROSANetworkRef = &corev1.LocalObjectReference{Name: "test-net"}
		_, err := SeaplaneClusterSubnetID(spec, nil)
		g.Expect(err).To(HaveOccurred())
	})
}
