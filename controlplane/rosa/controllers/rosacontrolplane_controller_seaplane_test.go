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
	rosaaws "github.com/openshift/rosa/pkg/aws"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/rosa"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fakeSeaplaneClientset adapts the generated fake clientset to the top-level
// seaplane clientset interface.
type fakeSeaplaneClientset struct {
	inner *seaplanefake.Clientset
}

func (f *fakeSeaplaneClientset) SeaplaneV1alpha1() platform.V1alpha1PublicInterface {
	return platform.NewV1alpha1PublicClient(f.inner.V1alpha1Public())
}

func seaplaneTestCreator() *rosaaws.Creator {
	return &rosaaws.Creator{
		AccountID: "123456789012",
		ARN:       "arn:aws:iam::123456789012:user/test",
		Partition: "aws",
	}
}

// newSeaplaneTestScope builds a ROSAControlPlaneScope backed by a fake k8s
// client with a controller identity, mirroring the scope test setup.
func newSeaplaneTestScope(t *testing.T, controlPlane *rosacontrolplanev1.ROSAControlPlane) (*scope.ROSAControlPlaneScope, func()) {
	t.Helper()
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(infrav1.AddToScheme(scheme)).To(Succeed())
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(clusterControllerIdentity, controlPlane).
		Build()

	ownerCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "owner-cluster"},
	}

	rosaScope, err := scope.NewROSAControlPlaneScope(scope.ROSAControlPlaneScopeParams{
		Client:         fakeClient,
		Cluster:        ownerCluster,
		ControlPlane:   controlPlane,
		ControllerName: "rosacontrolplane",
		Logger:         logger.NewLogger(klog.Background()),
	})
	g.Expect(err).ToNot(HaveOccurred())
	return rosaScope, func() { _ = rosaScope.Close() }
}

func newSeaplaneTestControlPlane() *rosacontrolplanev1.ROSAControlPlane {
	return &rosacontrolplanev1.ROSAControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane"},
		Spec: rosacontrolplanev1.RosaControlPlaneSpec{
			RosaClusterName:   "test-cluster",
			Region:            "us-east-1",
			Version:           "4.19.0",
			Subnets:           []string{"subnet-0123456789abcdef0"},
			AvailabilityZones: []string{"us-east-1a"},
			OIDCID:            "test-oidc-id",
			InstallerRoleARN:  "arn:aws:iam::123456789012:role/test-installer",
			SupportRoleARN:    "arn:aws:iam::123456789012:role/test-support",
			WorkerRoleARN:     "arn:aws:iam::123456789012:role/test-worker",
			RolesRef:          seaplaneTestRolesRef(),
			IdentityRef:       nil, // falls back to controller identity
			EndpointAccess:    rosacontrolplanev1.Public,
			DefaultMachinePoolSpec: rosacontrolplanev1.DefaultMachinePoolSpec{
				InstanceType: "m5.xlarge",
			},
		},
	}
}

func seaplaneTestRolesRef() rosacontrolplanev1.AWSRolesRef {
	return rosacontrolplanev1.AWSRolesRef{
		IngressARN:              "arn:aws:iam::123456789012:role/test-cluster-ingress",
		ImageRegistryARN:        "arn:aws:iam::123456789012:role/test-cluster-image-registry",
		StorageARN:              "arn:aws:iam::123456789012:role/test-cluster-ebs-csi",
		NetworkARN:              "arn:aws:iam::123456789012:role/test-cluster-network-config",
		KubeCloudControllerARN:  "arn:aws:iam::123456789012:role/test-cluster-cloud-controller-manager",
		NodePoolManagementARN:   "arn:aws:iam::123456789012:role/test-cluster-node-pool-management",
		ControlPlaneOperatorARN: "arn:aws:iam::123456789012:role/test-cluster-control-plane-operator",
	}
}

// stubDescribeSeaplaneSubnet replaces the EC2 subnet lookup with a static
// answer and restores it after the test.
func stubDescribeSeaplaneSubnet(t *testing.T) {
	t.Helper()
	original := describeSeaplaneSubnet
	describeSeaplaneSubnet = func(_ context.Context, _ awsv2.Config, _ string) (rosa.SeaplaneSubnetInfo, error) {
		return rosa.SeaplaneSubnetInfo{
			SubnetID: "subnet-0123456789abcdef0",
			VPCID:    "vpc-0123456789abcdef0",
			Zone:     "us-east-1a",
		}, nil
	}
	t.Cleanup(func() { describeSeaplaneSubnet = original })
}

func TestReconcileSeaplaneNormal_Create(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(rosa.SeaplaneReleaseImageEnvVar, "quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64")
	stubDescribeSeaplaneSubnet(t)

	controlPlane := newSeaplaneTestControlPlane()
	rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
	defer closeScope()

	fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset()}
	r := ROSAControlPlaneReconciler{
		Client: rosaScope.Client,
		NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
			return fakeCs, nil
		},
	}

	res, err := r.reconcileSeaplaneNormal(context.Background(), rosaScope, seaplaneTestCreator())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(defaultRequeueInterval))

	created, err := fakeCs.SeaplaneV1alpha1().Clusters().Get(
		context.Background(), "test-cluster", platform.GetOptions{})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(created.Spec.OidcConfigID).To(Equal("test-oidc-id"))
	g.Expect(created.Spec.HostedCluster.Platform.AWS.RolesRef.IngressARN).To(
		Equal("arn:aws:iam::123456789012:role/test-cluster-ingress"))
	g.Expect(created.Spec.HostedCluster.Platform.AWS.CloudProviderConfig.VPC).To(Equal("vpc-0123456789abcdef0"))
	g.Expect(created.Spec.HostedCluster.Release.Image).To(
		Equal("quay.io/openshift-release-dev/ocp-release:4.19.0-x86_64"))

	g.Expect(controlPlane.Status.ID).To(Equal(string(created.UID)))
	g.Expect(controlPlane.Status.Ready).To(BeFalse())
}

func TestReconcileSeaplaneNormal_Ready(t *testing.T) {
	g := NewWithT(t)

	controlPlane := newSeaplaneTestControlPlane()
	rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
	defer closeScope()

	cluster := &seaplanev1alpha1.Cluster{}
	cluster.Name = "test-cluster"
	cluster.UID = "test-uid"
	cluster.Status.Phase = seaplanev1alpha1.ClusterPhaseReady
	cluster.Status.Version = "4.19.0"
	cluster.Status.ControlPlaneEndpoint.Host = "api.test-cluster.example.com"
	cluster.Status.ControlPlaneEndpoint.Port = 6443

	fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset(cluster)}
	r := ROSAControlPlaneReconciler{
		Client: rosaScope.Client,
		NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
			return fakeCs, nil
		},
	}

	res, err := r.reconcileSeaplaneNormal(context.Background(), rosaScope, seaplaneTestCreator())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(BeZero())

	g.Expect(controlPlane.Status.Ready).To(BeTrue())
	g.Expect(controlPlane.Status.ID).To(Equal("test-uid"))
	g.Expect(controlPlane.Status.Version).To(Equal("4.19.0"))
	g.Expect(controlPlane.Spec.ControlPlaneEndpoint.Host).To(Equal("api.test-cluster.example.com"))
	g.Expect(controlPlane.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(6443)))
}

func TestReconcileSeaplaneNormal_Provisioning(t *testing.T) {
	g := NewWithT(t)

	controlPlane := newSeaplaneTestControlPlane()
	rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
	defer closeScope()

	cluster := &seaplanev1alpha1.Cluster{}
	cluster.Name = "test-cluster"
	cluster.UID = "test-uid"
	cluster.Status.Phase = seaplanev1alpha1.ClusterPhaseProvisioning

	fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset(cluster)}
	r := ROSAControlPlaneReconciler{
		Client: rosaScope.Client,
		NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
			return fakeCs, nil
		},
	}

	res, err := r.reconcileSeaplaneNormal(context.Background(), rosaScope, seaplaneTestCreator())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(defaultRequeueInterval))
	g.Expect(controlPlane.Status.Ready).To(BeFalse())
}

func TestReconcileSeaplaneDelete(t *testing.T) {
	newCluster := func() *seaplanev1alpha1.Cluster {
		cluster := &seaplanev1alpha1.Cluster{}
		cluster.Name = "test-cluster"
		cluster.UID = "test-uid"
		cluster.Status.Phase = seaplanev1alpha1.ClusterPhaseReady
		return cluster
	}

	t.Run("deletes existing cluster", func(t *testing.T) {
		g := NewWithT(t)
		controlPlane := newSeaplaneTestControlPlane()
		controlPlane.Finalizers = []string{ROSAControlPlaneFinalizer}
		rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
		defer closeScope()

		fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset(newCluster())}
		r := ROSAControlPlaneReconciler{
			Client: rosaScope.Client,
			NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
				return fakeCs, nil
			},
		}

		res, err := r.reconcileSeaplaneDelete(context.Background(), rosaScope, seaplaneTestCreator())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(defaultRequeueInterval))

		_, err = fakeCs.SeaplaneV1alpha1().Clusters().Get(
			context.Background(), "test-cluster", platform.GetOptions{})
		g.Expect(err).To(HaveOccurred())
		// finalizer is kept until the cluster is confirmed deleted
		g.Expect(controlPlane.Finalizers).To(ContainElement(ROSAControlPlaneFinalizer))
	})

	t.Run("removes finalizer when cluster is gone", func(t *testing.T) {
		g := NewWithT(t)
		controlPlane := newSeaplaneTestControlPlane()
		controlPlane.Finalizers = []string{ROSAControlPlaneFinalizer}
		rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
		defer closeScope()

		fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset()}
		r := ROSAControlPlaneReconciler{
			Client: rosaScope.Client,
			NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
				return fakeCs, nil
			},
		}

		_, err := r.reconcileSeaplaneDelete(context.Background(), rosaScope, seaplaneTestCreator())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(controlPlane.Finalizers).ToNot(ContainElement(ROSAControlPlaneFinalizer))
	})

	t.Run("keeps cluster when delete protection is enabled", func(t *testing.T) {
		g := NewWithT(t)
		controlPlane := newSeaplaneTestControlPlane()
		controlPlane.Finalizers = []string{ROSAControlPlaneFinalizer}
		rosaScope, closeScope := newSeaplaneTestScope(t, controlPlane)
		defer closeScope()

		cluster := newCluster()
		cluster.Spec.DeleteProtection = ptr.To(true)
		fakeCs := &fakeSeaplaneClientset{inner: seaplanefake.NewSimpleClientset(cluster)}
		r := ROSAControlPlaneReconciler{
			Client: rosaScope.Client,
			NewSeaplaneClient: func(_ awsv2.Config, _ *rosaaws.Creator, _ string) (seaplaneclientset.Interface, error) {
				return fakeCs, nil
			},
		}

		_, err := r.reconcileSeaplaneDelete(context.Background(), rosaScope, seaplaneTestCreator())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(controlPlane.Finalizers).ToNot(ContainElement(ROSAControlPlaneFinalizer))

		_, err = fakeCs.SeaplaneV1alpha1().Clusters().Get(
			context.Background(), "test-cluster", platform.GetOptions{})
		g.Expect(err).ToNot(HaveOccurred())
	})
}
