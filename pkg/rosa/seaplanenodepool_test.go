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
	"time"

	. "github.com/onsi/gomega"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
)

func nodePoolTestMachinePoolSpec() expinfrav1.RosaMachinePoolSpec {
	return expinfrav1.RosaMachinePoolSpec{
		NodePoolName: "workers",
		InstanceType: "m5.xlarge",
		Subnet:       "subnet-0123456789abcdef0",
		AutoRepair:   true,
		Labels:       map[string]string{"node-role": "worker"},
		AdditionalTags: map[string]string{
			"owner": "capa",
		},
		TuningConfigs: []string{"tuning-1"},
		VolumeSize:    300,
		NodeDrainGracePeriod: &metav1.Duration{
			Duration: 5 * time.Minute,
		},
	}
}

func nodePoolTestRolesRef() rosacontrolplanev1.AWSRolesRef {
	return rosacontrolplanev1.AWSRolesRef{
		NodePoolManagementARN: "arn:aws:iam::123456789012:role/test-cluster-node-pool-management",
	}
}

func TestSeaplaneNodePoolName(t *testing.T) {
	g := NewWithT(t)
	g.Expect(SeaplaneNodePoolName("test-cluster", "workers")).To(Equal("test-cluster.workers"))
}

func TestSeaplaneInstanceProfile(t *testing.T) {
	tests := []struct {
		name     string
		rolesRef rosacontrolplanev1.AWSRolesRef
		want     string
	}{
		{
			name:     "derives profile from prefix",
			rolesRef: nodePoolTestRolesRef(),
			want:     "test-cluster-ROSA-Worker-Role",
		},
		{
			name:     "empty roles ref",
			rolesRef: rosacontrolplanev1.AWSRolesRef{},
			want:     "",
		},
		{
			name: "unparseable arn",
			rolesRef: rosacontrolplanev1.AWSRolesRef{
				NodePoolManagementARN: "arn:aws:iam::123456789012:role/some-other-role",
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(SeaplaneInstanceProfile(tt.rolesRef)).To(Equal(tt.want))
		})
	}
}

func TestBuildSeaplaneNodePool(t *testing.T) {
	g := NewWithT(t)

	replicas := int32(3)
	np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
		MachinePoolSpec: nodePoolTestMachinePoolSpec(),
		CAPIReplicas:    &replicas,
		ClusterName:     "test-cluster",
		ReleaseImage:    "quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64",
		SubnetID:        "subnet-0123456789abcdef0",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
	})
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(np.Name).To(Equal("test-cluster.workers"))
	g.Expect(np.Spec.NodePool.ClusterName).To(Equal("test-cluster"))
	g.Expect(np.Spec.NodePool.Release.Image).To(Equal("quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"))
	g.Expect(string(np.Spec.NodePool.Platform.Type)).To(Equal("AWS"))
	g.Expect(np.Spec.NodePool.Platform.AWS).ToNot(BeNil())
	g.Expect(np.Spec.NodePool.Platform.AWS.InstanceType).To(Equal("m5.xlarge"))
	g.Expect(np.Spec.NodePool.Platform.AWS.InstanceProfile).To(Equal("test-cluster-ROSA-Worker-Role"))
	g.Expect(*np.Spec.NodePool.Platform.AWS.Subnet.ID).To(Equal("subnet-0123456789abcdef0"))
	g.Expect(np.Spec.NodePool.Platform.AWS.RootVolume).ToNot(BeNil())
	g.Expect(np.Spec.NodePool.Platform.AWS.RootVolume.Size).To(Equal(int64(300)))
	g.Expect(*np.Spec.NodePool.Replicas).To(Equal(int32(3)))
	g.Expect(np.Spec.NodePool.AutoScaling).To(BeNil())
	g.Expect(np.Spec.Labels).To(Equal(map[string]string{"node-role": "worker"}))
	g.Expect(np.Spec.Tags).To(Equal(map[string]string{"owner": "capa"}))
	g.Expect(*np.Spec.AutoRepair).To(BeTrue())
	g.Expect(string(np.Spec.EC2InstanceMetadataHttpTokens)).To(Equal("required"))
	g.Expect(np.Spec.NodePool.TuningConfig).To(Equal([]corev1.LocalObjectReference{{Name: "tuning-1"}}))
	g.Expect(*np.Spec.NodePool.NodeDrainTimeout).To(Equal(mustDuration(t, "5m")))
}

func TestBuildSeaplaneNodePool_Autoscaling(t *testing.T) {
	g := NewWithT(t)

	spec := nodePoolTestMachinePoolSpec()
	spec.Autoscaling = &rosacontrolplanev1.AutoScaling{
		MinReplicas: 2,
		MaxReplicas: 5,
	}
	np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
		MachinePoolSpec: spec,
		ClusterName:     "test-cluster",
		SubnetID:        "subnet-0123456789abcdef0",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(np.Spec.NodePool.AutoScaling).ToNot(BeNil())
	g.Expect(*np.Spec.NodePool.AutoScaling.Min).To(Equal(int32(2)))
	g.Expect(np.Spec.NodePool.AutoScaling.Max).To(Equal(int32(5)))
	g.Expect(np.Spec.NodePool.Replicas).To(BeNil())
}

func TestBuildSeaplaneNodePool_DefaultReplicas(t *testing.T) {
	g := NewWithT(t)

	np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
		MachinePoolSpec: nodePoolTestMachinePoolSpec(),
		ClusterName:     "test-cluster",
		SubnetID:        "subnet-0123456789abcdef0",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(*np.Spec.NodePool.Replicas).To(Equal(int32(1)))
}

func TestBuildSeaplaneNodePool_Spot(t *testing.T) {
	g := NewWithT(t)

	spec := nodePoolTestMachinePoolSpec()
	spec.SpotMarketOptions = &expinfrav1.RosaSpotMarketOptions{MaxPrice: ptr.To("0.50")}
	np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
		MachinePoolSpec: spec,
		ClusterName:     "test-cluster",
		SubnetID:        "subnet-0123456789abcdef0",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(np.Spec.NodePool.Platform.AWS.Placement).ToNot(BeNil())
	g.Expect(np.Spec.NodePool.Platform.AWS.Placement.MarketType).To(Equal(hypershiftv1beta1.MarketTypeSpot))
	g.Expect(np.Spec.NodePool.Platform.AWS.Placement.Spot.MaxPrice).To(Equal("0.50"))
}

func TestBuildSeaplaneNodePool_TaintsAndImageType(t *testing.T) {
	g := NewWithT(t)

	spec := nodePoolTestMachinePoolSpec()
	spec.ImageType = "Windows"
	spec.Taints = []expinfrav1.RosaTaint{
		{Key: "key1", Value: "value1", Effect: corev1.TaintEffectNoSchedule},
	}
	np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
		MachinePoolSpec: spec,
		ClusterName:     "test-cluster",
		SubnetID:        "subnet-0123456789abcdef0",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(np.Spec.NodePool.Platform.AWS.ImageType)).To(Equal("Windows"))
	g.Expect(np.Spec.NodePool.Taints).To(HaveLen(1))
	g.Expect(np.Spec.NodePool.Taints[0].Key).To(Equal("key1"))
	g.Expect(np.Spec.NodePool.Taints[0].Effect).To(Equal(corev1.TaintEffectNoSchedule))
}

func TestBuildSeaplaneNodePool_Errors(t *testing.T) {
	tests := []struct {
		name            string
		clusterName     string
		instanceProfile string
		subnetID        string
		wantErr         string
	}{
		{name: "missing cluster name", instanceProfile: "p", subnetID: "s", wantErr: "cluster name is required"},
		{name: "missing instance profile", clusterName: "c", subnetID: "s", wantErr: "worker instance profile is required"},
		{name: "missing subnet", clusterName: "c", instanceProfile: "p", wantErr: "subnet is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
				MachinePoolSpec: nodePoolTestMachinePoolSpec(),
				ClusterName:     tt.clusterName,
				SubnetID:        tt.subnetID,
				InstanceProfile: tt.instanceProfile,
			})
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
		})
	}
}

func TestUpdateSeaplaneNodePoolMutableFields(t *testing.T) {
	replicas := int32(2)
	newReplicas := int32(5)

	base := func() *seaplanev1alpha1.NodePool {
		np, err := BuildSeaplaneNodePool(SeaplaneNodePoolParams{
			MachinePoolSpec: nodePoolTestMachinePoolSpec(),
			CAPIReplicas:    &replicas,
			ClusterName:     "test-cluster",
			ReleaseImage:    "quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64",
			SubnetID:        "subnet-0123456789abcdef0",
			InstanceProfile: "test-cluster-ROSA-Worker-Role",
		})
		if err != nil {
			t.Fatal(err)
		}
		return np
	}

	t.Run("no drift", func(t *testing.T) {
		g := NewWithT(t)
		current, desired := base(), base()
		g.Expect(UpdateSeaplaneNodePoolMutableFields(current, desired)).To(BeFalse())
	})

	t.Run("replicas drift", func(t *testing.T) {
		g := NewWithT(t)
		current, desired := base(), base()
		desired.Spec.NodePool.Replicas = &newReplicas
		g.Expect(UpdateSeaplaneNodePoolMutableFields(current, desired)).To(BeTrue())
		g.Expect(*current.Spec.NodePool.Replicas).To(Equal(int32(5)))
	})

	t.Run("labels drift", func(t *testing.T) {
		g := NewWithT(t)
		current, desired := base(), base()
		desired.Spec.Labels = map[string]string{"changed": "true"}
		g.Expect(UpdateSeaplaneNodePoolMutableFields(current, desired)).To(BeTrue())
		g.Expect(current.Spec.Labels).To(Equal(map[string]string{"changed": "true"}))
	})

	t.Run("tags drift", func(t *testing.T) {
		g := NewWithT(t)
		current, desired := base(), base()
		desired.Spec.Tags = nil
		g.Expect(UpdateSeaplaneNodePoolMutableFields(current, desired)).To(BeTrue())
		g.Expect(current.Spec.Tags).To(BeNil())
	})

	t.Run("release image drift is ignored", func(t *testing.T) {
		g := NewWithT(t)
		current, desired := base(), base()
		desired.Spec.NodePool.Release.Image = "quay.io/openshift-release-dev/ocp-release:4.20.0-x86_64"
		g.Expect(UpdateSeaplaneNodePoolMutableFields(current, desired)).To(BeFalse())
		g.Expect(current.Spec.NodePool.Release.Image).To(
			Equal("quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"))
	})
}

func mustDuration(t *testing.T, s string) metav1.Duration {
	t.Helper()
	parsed, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return metav1.Duration{Duration: parsed}
}
