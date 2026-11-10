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

package controllers

import (
	"context"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	. "github.com/onsi/gomega"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplaneclientset "github.com/openshift-online/rosa-seaplane-api/clientset"
	seaplanefake "github.com/openshift-online/rosa-seaplane-api/clientset/generated/fake"
	"github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fakeMachinePoolSeaplaneClientset adapts the generated fake clientset to the
// top-level seaplane clientset interface.
type fakeMachinePoolSeaplaneClientset struct {
	inner *seaplanefake.Clientset
}

func (f *fakeMachinePoolSeaplaneClientset) SeaplaneV1alpha1() platform.V1alpha1PublicInterface {
	return platform.NewV1alpha1PublicClient(f.inner.V1alpha1Public())
}

// newSeaplaneMachinePoolScopes builds RosaMachinePool and ROSAControlPlane
// scopes backed by a fake k8s client with a controller identity.
func newSeaplaneMachinePoolScopes(t *testing.T) (*scope.RosaMachinePoolScope, *scope.ROSAControlPlaneScope, *fakeMachinePoolSeaplaneClientset) {
	t.Helper()
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(infrav1.AddToScheme(scheme)).To(Succeed())
	g.Expect(expinfrav1.AddToScheme(scheme)).To(Succeed())
	g.Expect(rosacontrolplanev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())

	clusterControllerIdentity := &infrav1.AWSClusterControllerIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: infrav1.AWSClusterControllerIdentitySpec{
			AWSClusterIdentitySpec: infrav1.AWSClusterIdentitySpec{
				AllowedNamespaces: &infrav1.AllowedNamespaces{},
			},
		},
	}

	controlPlane := newSeaplaneMachinePoolTestControlPlane()
	machinePool := newSeaplaneMachinePoolTestCAPIMachinePool()
	rosaMachinePool := newSeaplaneMachinePoolTestRosaMachinePool()

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(clusterControllerIdentity, controlPlane, machinePool, rosaMachinePool).
		Build()

	log := logger.NewLogger(klog.Background())
	ownerCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "owner-cluster"},
	}

	machinePoolScope, err := scope.NewRosaMachinePoolScope(scope.RosaMachinePoolScopeParams{
		Client:          fakeClient,
		ControllerName:  rosaMachinePoolControllerName,
		Cluster:         ownerCluster,
		ControlPlane:    controlPlane,
		MachinePool:     machinePool,
		RosaMachinePool: rosaMachinePool,
		Logger:          log,
	})
	g.Expect(err).ToNot(HaveOccurred())

	rosaControlPlaneScope, err := scope.NewROSAControlPlaneScope(scope.ROSAControlPlaneScopeParams{
		Client:         fakeClient,
		Cluster:        ownerCluster,
		ControlPlane:   controlPlane,
		ControllerName: rosaControlPlaneScopeControllerName,
		Logger:         log,
	})
	g.Expect(err).ToNot(HaveOccurred())

	return machinePoolScope, rosaControlPlaneScope, &fakeMachinePoolSeaplaneClientset{inner: seaplanefake.NewSimpleClientset()}
}

func newSeaplaneMachinePoolTestControlPlane() *rosacontrolplanev1.ROSAControlPlane {
	return &rosacontrolplanev1.ROSAControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane"},
		Spec: rosacontrolplanev1.RosaControlPlaneSpec{
			RosaClusterName: "test-cluster",
			Region:          "us-east-1",
			Version:         "4.19.0",
			OIDCID:          "test-oidc-id",
			RolesRef: rosacontrolplanev1.AWSRolesRef{
				NodePoolManagementARN: "arn:aws:iam::123456789012:role/test-cluster-node-pool-management",
			},
			Subnets:        []string{"subnet-0123456789abcdef0"},
			EndpointAccess: rosacontrolplanev1.Public,
		},
		Status: rosacontrolplanev1.RosaControlPlaneStatus{
			ID:    "test-cluster-uid",
			Ready: true,
		},
	}
}

func newSeaplaneMachinePoolTestCAPIMachinePool() *clusterv1.MachinePool {
	return &clusterv1.MachinePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mp"},
		Spec: clusterv1.MachinePoolSpec{
			Replicas: ptr.To(int32(2)),
		},
	}
}

func newSeaplaneMachinePoolTestRosaMachinePool() *expinfrav1.ROSAMachinePool {
	return &expinfrav1.ROSAMachinePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rmp"},
		Spec: expinfrav1.RosaMachinePoolSpec{
			NodePoolName: "workers",
			InstanceType: "m5.xlarge",
			Subnet:       "subnet-0123456789abcdef0",
			AutoRepair:   true,
			AdditionalTags: map[string]string{
				"owner": "capa",
			},
		},
	}
}

// seaplaneParentCluster returns a Platform API cluster matching the test
// control plane.
func seaplaneParentCluster() *seaplanev1alpha1.Cluster {
	cluster := &seaplanev1alpha1.Cluster{}
	cluster.Name = "test-cluster"
	cluster.UID = "test-cluster-uid"
	cluster.Status.Version = "4.19.0"
	cluster.Status.Phase = seaplanev1alpha1.ClusterPhaseReady
	cluster.Spec.HostedCluster.Release.Image = "quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"
	subnetID := "subnet-0123456789abcdef0"
	cluster.Spec.HostedCluster.Platform.AWS = &seaplanev1alpha1.AWSPlatformSpec{
		Region: "us-east-1",
		CloudProviderConfig: &hypershiftv1beta1.AWSCloudProviderConfig{
			VPC:    "vpc-0123456789abcdef0",
			Subnet: &hypershiftv1beta1.AWSResourceReference{ID: &subnetID},
		},
	}
	return cluster
}

// testSubnetRef returns the worker subnet reference used by the fake node pools.
func testSubnetRef() hypershiftv1beta1.AWSResourceReference {
	subnetID := "subnet-0123456789abcdef0"
	return hypershiftv1beta1.AWSResourceReference{ID: &subnetID}
}

func newSeaplaneMachinePoolReconciler(
	rosaControlPlaneScope *scope.ROSAControlPlaneScope, fakeCs *fakeMachinePoolSeaplaneClientset,
) ROSAMachinePoolReconciler {
	return ROSAMachinePoolReconciler{
		Client: rosaControlPlaneScope.Client,
		NewSeaplaneClient: func(_ context.Context, _ awsv2.Config, _ string) (seaplaneclientset.Interface, error) {
			return fakeCs, nil
		},
	}
}

func TestReconcileSeaplaneMachinePoolNormal_Create(t *testing.T) {
	g := NewWithT(t)

	machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
	defer func() { _ = machinePoolScope.Close() }()

	// The parent cluster must exist on the Platform API.
	g.Expect(fakeCs.inner.Tracker().Create(
		seaplanev1alpha1.SchemeGroupVersion.WithResource("clusters"), seaplaneParentCluster(), "")).To(Succeed())

	r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)

	res, err := r.reconcileSeaplaneNormal(context.Background(), machinePoolScope, rosaControlPlaneScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(defaultRequeueInterval))

	created, err := fakeCs.SeaplaneV1alpha1().NodePools().Get(
		context.Background(), "test-cluster.workers", platform.GetOptions{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(created.Spec.NodePool.ClusterName).To(Equal("test-cluster"))
	g.Expect(created.Spec.NodePool.Platform.AWS.InstanceProfile).To(Equal("test-cluster-ROSA-Worker-Role"))
	g.Expect(*created.Spec.NodePool.Platform.AWS.Subnet.ID).To(Equal("subnet-0123456789abcdef0"))
	g.Expect(created.Spec.NodePool.Release.Image).To(
		Equal("quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"))
	g.Expect(*created.Spec.NodePool.Replicas).To(Equal(int32(2)))

	g.Expect(machinePoolScope.RosaMachinePool.Status.ID).To(Equal(string(created.UID)))
}

func TestReconcileSeaplaneMachinePoolNormal_Ready(t *testing.T) {
	g := NewWithT(t)

	machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
	defer func() { _ = machinePoolScope.Close() }()

	nodePool := &seaplanev1alpha1.NodePool{}
	nodePool.Name = "test-cluster.workers"
	nodePool.UID = "test-np-uid"
	nodePool.Spec.NodePool.ClusterName = "test-cluster"
	nodePool.Spec.NodePool.Platform.Type = "AWS"
	nodePool.Spec.NodePool.Platform.AWS = &seaplanev1alpha1.AWSNodePoolPlatform{
		InstanceType:    "m5.xlarge",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
		Subnet:          testSubnetRef(),
	}
	nodePool.Spec.NodePool.Replicas = ptr.To(int32(2))
	nodePool.Spec.AutoRepair = ptr.To(true)
	nodePool.Status.Phase = seaplanev1alpha1.NodePoolPhaseReady
	nodePool.Status.Replicas = ptr.To(int32(2))

	g.Expect(fakeCs.inner.Tracker().Create(
		seaplanev1alpha1.SchemeGroupVersion.WithResource("nodepools"), nodePool, "")).To(Succeed())

	r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)

	res, err := r.reconcileSeaplaneNormal(context.Background(), machinePoolScope, rosaControlPlaneScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeZero())
	g.Expect(machinePoolScope.RosaMachinePool.Status.Ready).To(BeTrue())
	g.Expect(machinePoolScope.RosaMachinePool.Status.Replicas).To(Equal(int32(2)))
}

func TestReconcileSeaplaneMachinePoolNormal_Provisioning(t *testing.T) {
	g := NewWithT(t)

	machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
	defer func() { _ = machinePoolScope.Close() }()

	nodePool := &seaplanev1alpha1.NodePool{}
	nodePool.Name = "test-cluster.workers"
	nodePool.Spec.NodePool.ClusterName = "test-cluster"
	nodePool.Spec.NodePool.Platform.Type = "AWS"
	nodePool.Spec.NodePool.Platform.AWS = &seaplanev1alpha1.AWSNodePoolPlatform{
		InstanceType:    "m5.xlarge",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
		Subnet:          testSubnetRef(),
	}
	nodePool.Spec.NodePool.Replicas = ptr.To(int32(2))
	nodePool.Spec.AutoRepair = ptr.To(true)
	nodePool.Status.Phase = seaplanev1alpha1.NodePoolPhaseProvisioning

	g.Expect(fakeCs.inner.Tracker().Create(
		seaplanev1alpha1.SchemeGroupVersion.WithResource("nodepools"), nodePool, "")).To(Succeed())

	r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)

	res, err := r.reconcileSeaplaneNormal(context.Background(), machinePoolScope, rosaControlPlaneScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(defaultRequeueInterval))
	g.Expect(machinePoolScope.RosaMachinePool.Status.Ready).To(BeFalse())
}

func TestReconcileSeaplaneMachinePoolNormal_UpdateReplicasDrift(t *testing.T) {
	g := NewWithT(t)

	machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
	defer func() { _ = machinePoolScope.Close() }()

	nodePool := &seaplanev1alpha1.NodePool{}
	nodePool.Name = "test-cluster.workers"
	nodePool.UID = "test-np-uid"
	nodePool.Spec.NodePool.ClusterName = "test-cluster"
	nodePool.Spec.NodePool.Platform.Type = "AWS"
	nodePool.Spec.NodePool.Platform.AWS = &seaplanev1alpha1.AWSNodePoolPlatform{
		InstanceType:    "m5.xlarge",
		InstanceProfile: "test-cluster-ROSA-Worker-Role",
		Subnet:          testSubnetRef(),
	}
	// Drift: live pool has 4 replicas, the CR wants 2.
	nodePool.Spec.NodePool.Replicas = ptr.To(int32(4))
	nodePool.Spec.AutoRepair = ptr.To(true)
	nodePool.Spec.Tags = map[string]string{"owner": "capa"}
	nodePool.Status.Phase = seaplanev1alpha1.NodePoolPhaseReady

	g.Expect(fakeCs.inner.Tracker().Create(
		seaplanev1alpha1.SchemeGroupVersion.WithResource("nodepools"), nodePool, "")).To(Succeed())

	r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)

	_, err := r.reconcileSeaplaneNormal(context.Background(), machinePoolScope, rosaControlPlaneScope)
	g.Expect(err).ToNot(HaveOccurred())

	updated, err := fakeCs.SeaplaneV1alpha1().NodePools().Get(
		context.Background(), "test-cluster.workers", platform.GetOptions{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(*updated.Spec.NodePool.Replicas).To(Equal(int32(2)))
	g.Expect(machinePoolScope.RosaMachinePool.Status.Ready).To(BeTrue())
}

func TestReconcileSeaplaneMachinePoolDelete(t *testing.T) {
	newNodePool := func() *seaplanev1alpha1.NodePool {
		nodePool := &seaplanev1alpha1.NodePool{}
		nodePool.Name = "test-cluster.workers"
		nodePool.UID = "test-np-uid"
		nodePool.Spec.NodePool.ClusterName = "test-cluster"
		nodePool.Spec.NodePool.Platform.Type = "AWS"
		nodePool.Spec.NodePool.Platform.AWS = &seaplanev1alpha1.AWSNodePoolPlatform{
			InstanceType:    "m5.xlarge",
			InstanceProfile: "test-cluster-ROSA-Worker-Role",
			Subnet:          testSubnetRef(),
		}
		nodePool.Spec.NodePool.Replicas = ptr.To(int32(2))
		nodePool.Spec.AutoRepair = ptr.To(true)
		nodePool.Status.Phase = seaplanev1alpha1.NodePoolPhaseReady
		return nodePool
	}

	t.Run("deletes existing node pool", func(t *testing.T) {
		g := NewWithT(t)
		machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
		machinePoolScope.RosaMachinePool.Finalizers = []string{expinfrav1.RosaMachinePoolFinalizer}
		defer func() { _ = machinePoolScope.Close() }()

		g.Expect(fakeCs.inner.Tracker().Create(
			seaplanev1alpha1.SchemeGroupVersion.WithResource("nodepools"), newNodePool(), "")).To(Succeed())

		r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)
		g.Expect(r.reconcileSeaplaneDelete(context.Background(), machinePoolScope, rosaControlPlaneScope)).To(Succeed())

		_, err := fakeCs.SeaplaneV1alpha1().NodePools().Get(
			context.Background(), "test-cluster.workers", platform.GetOptions{})
		g.Expect(err).To(HaveOccurred())
		g.Expect(machinePoolScope.RosaMachinePool.Finalizers).ToNot(ContainElement(expinfrav1.RosaMachinePoolFinalizer))
	})

	t.Run("removes finalizer when node pool is gone", func(t *testing.T) {
		g := NewWithT(t)
		machinePoolScope, rosaControlPlaneScope, fakeCs := newSeaplaneMachinePoolScopes(t)
		machinePoolScope.RosaMachinePool.Finalizers = []string{expinfrav1.RosaMachinePoolFinalizer}
		defer func() { _ = machinePoolScope.Close() }()

		r := newSeaplaneMachinePoolReconciler(rosaControlPlaneScope, fakeCs)
		g.Expect(r.reconcileSeaplaneDelete(context.Background(), machinePoolScope, rosaControlPlaneScope)).To(Succeed())
		g.Expect(machinePoolScope.RosaMachinePool.Finalizers).ToNot(ContainElement(expinfrav1.RosaMachinePoolFinalizer))
	})
}
