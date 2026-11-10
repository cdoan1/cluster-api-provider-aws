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

package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/feature"
)

// seaplaneTestControlPlane returns a ROSAControlPlane with the fields the
// seaplane v2 path requires: OIDC config, operator roles and a subnet, but no
// OCM account roles and no explicit availability zones.
func seaplaneTestControlPlane() *rosacontrolplanev1.ROSAControlPlane {
	return &rosacontrolplanev1.ROSAControlPlane{
		Spec: rosacontrolplanev1.RosaControlPlaneSpec{
			RosaClusterName: "test-cluster",
			Region:          "us-east-1",
			Version:         "4.19.0",
			VersionGate:     rosacontrolplanev1.WaitForAcknowledge,
			OIDCID:          "test-oidc-id",
			Subnets:         []string{"subnet-0123456789abcdef0"},
			RolesRef: rosacontrolplanev1.AWSRolesRef{
				IngressARN:              "arn:aws:iam::123456789012:role/test-cluster-ingress",
				ImageRegistryARN:        "arn:aws:iam::123456789012:role/test-cluster-image-registry",
				StorageARN:              "arn:aws:iam::123456789012:role/test-cluster-ebs-csi",
				NetworkARN:              "arn:aws:iam::123456789012:role/test-cluster-network-config",
				KubeCloudControllerARN:  "arn:aws:iam::123456789012:role/test-cluster-cloud-controller-manager",
				NodePoolManagementARN:   "arn:aws:iam::123456789012:role/test-cluster-node-pool-management",
				ControlPlaneOperatorARN: "arn:aws:iam::123456789012:role/test-cluster-control-plane-operator",
			},
		},
	}
}

func TestROSAControlPlaneWebhookSeaplaneMode(t *testing.T) {
	g := NewWithT(t)
	w := &ROSAControlPlane{}

	// Enable the seaplane gate for the duration of the test.
	g.Expect(feature.MutableGates.Set("ROSASeaplaneV2=true")).To(Succeed())
	t.Cleanup(func() {
		_ = feature.MutableGates.Set("ROSASeaplaneV2=false")
	})

	t.Run("accepts seaplane spec without OCM account roles or AZs", func(t *testing.T) {
		g := NewWithT(t)
		_, err := w.ValidateCreate(context.Background(), seaplaneTestControlPlane())
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("still requires the operator roles on the seaplane path", func(t *testing.T) {
		g := NewWithT(t)
		r := seaplaneTestControlPlane()
		r.Spec.RolesRef.NodePoolManagementARN = ""
		_, err := w.ValidateCreate(context.Background(), r)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("nodePoolManagementARN"))
	})

	t.Run("still requires the OIDC config on the seaplane path", func(t *testing.T) {
		g := NewWithT(t)
		r := seaplaneTestControlPlane()
		r.Spec.OIDCID = ""
		_, err := w.ValidateCreate(context.Background(), r)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("oidcID"))
	})

	t.Run("still requires subnets on the seaplane path", func(t *testing.T) {
		g := NewWithT(t)
		r := seaplaneTestControlPlane()
		r.Spec.Subnets = nil
		_, err := w.ValidateCreate(context.Background(), r)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("subnets"))
	})
}

func TestROSAControlPlaneWebhookOCMModeUnchanged(t *testing.T) {
	g := NewWithT(t)
	w := &ROSAControlPlane{}

	// Gate disabled: the classic OCM requirements apply.
	g.Expect(feature.Gates.Enabled(feature.ROSASeaplaneV2)).To(BeFalse())

	_, err := w.ValidateCreate(context.Background(), seaplaneTestControlPlane())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("installerRoleARN"))
	g.Expect(err.Error()).To(ContainSubstring("availabilityZones"))
}
