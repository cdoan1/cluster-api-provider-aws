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
	"strings"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
)

// seaplaneWorkerInstanceProfileSuffix is the worker node instance profile
// name suffix following the operator roles prefix, mirroring the rosa CLI
// convention from the Platform API IAM CloudFormation template.
const seaplaneWorkerInstanceProfileSuffix = "ROSA-Worker-Role"

// seaplaneNodePoolSuffixWorkerImageType maps the RosaMachinePoolSpec
// ImageType values onto the Platform API image type.
const (
	seaplaneImageTypeWindows = "Windows"
	seaplaneImageTypeLinux   = "Linux"
)

// SeaplaneNodePoolName returns the Platform API NodePool name for a
// RosaMachinePool child of clusterName. NodePool names follow the
// <cluster-name>.<child> convention used to resolve the parent cluster.
func SeaplaneNodePoolName(clusterName, nodePoolName string) string {
	return clusterName + "." + nodePoolName
}

// SeaplaneInstanceProfile derives the worker node instance profile name from
// a cluster's operator RolesRef, mirroring the rosa CLI's
// seaplane.InstanceProfileFromRolesRef. It returns an empty string when the
// operator roles prefix cannot be derived.
func SeaplaneInstanceProfile(rolesRef rosacontrolplanev1.AWSRolesRef) string {
	arn := rolesRef.NodePoolManagementARN
	slash := strings.LastIndex(arn, "/")
	if slash < 0 {
		return ""
	}
	roleName := arn[slash+1:]
	prefix, found := strings.CutSuffix(roleName, "-node-pool-management")
	if !found {
		return ""
	}
	return prefix + "-" + seaplaneWorkerInstanceProfileSuffix
}

// SeaplaneNodePoolParams carries the inputs resolved by the controller for
// building a Platform API NodePool.
type SeaplaneNodePoolParams struct {
	// MachinePoolSpec is the RosaMachinePool spec.
	MachinePoolSpec expinfrav1.RosaMachinePoolSpec
	// CAPIReplicas is the replicas count of the owning CAPI MachinePool.
	CAPIReplicas *int32
	// ClusterName is the Platform API name of the parent cluster.
	ClusterName string
	// ReleaseImage is the OCP release payload image for the node pool,
	// resolved from the parent cluster.
	ReleaseImage string
	// SubnetID is the worker node subnet.
	SubnetID string
	// InstanceProfile is the worker node instance profile derived from the
	// parent cluster's operator roles.
	InstanceProfile string
}

// BuildSeaplaneNodePool maps a RosaMachinePool spec onto a Seaplane v2
// Platform API NodePool create object. It is the seaplane equivalent of
// nodePoolBuilder.
func BuildSeaplaneNodePool(params SeaplaneNodePoolParams) (*seaplanev1alpha1.NodePool, error) {
	if params.ClusterName == "" {
		return nil, fmt.Errorf("seaplane: cluster name is required")
	}
	if params.MachinePoolSpec.NodePoolName == "" {
		return nil, fmt.Errorf("seaplane: nodePoolName is required")
	}
	if params.InstanceProfile == "" {
		return nil, fmt.Errorf("seaplane: worker instance profile is required")
	}
	if params.SubnetID == "" {
		return nil, fmt.Errorf("seaplane: subnet is required (set rosaMachinePoolSpec.subnet)")
	}

	spec := params.MachinePoolSpec

	np := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: SeaplaneNodePoolName(params.ClusterName, spec.NodePoolName),
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				ClusterName: params.ClusterName,
				Platform: seaplanev1alpha1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &seaplanev1alpha1.AWSNodePoolPlatform{
						InstanceType:    spec.InstanceType,
						InstanceProfile: params.InstanceProfile,
						Subnet:          hypershiftv1beta1.AWSResourceReference{ID: ptr.To(params.SubnetID)},
						SecurityGroups:  toHypershiftResourceRefs(spec.AdditionalSecurityGroups),
						RootVolume:      toSeaplaneRootVolume(spec.VolumeSize),
						ImageType:       toSeaplaneImageType(spec.ImageType),
						Placement:       toSeaplanePlacement(spec.SpotMarketOptions),
					},
				},
			},
		},
	}

	if params.ReleaseImage != "" {
		np.Spec.NodePool.Release.Image = params.ReleaseImage
	}

	if spec.Autoscaling != nil {
		np.Spec.NodePool.AutoScaling = &hypershiftv1beta1.NodePoolAutoScaling{
			Min: ptr.To(int32(spec.Autoscaling.MinReplicas)), //nolint:gosec // bounded by CRD validation
			Max: int32(spec.Autoscaling.MaxReplicas),         //nolint:gosec // bounded by CRD validation
		}
	} else {
		replicas := int32(1)
		if params.CAPIReplicas != nil {
			replicas = *params.CAPIReplicas
		}
		np.Spec.NodePool.Replicas = ptr.To(replicas)
	}

	if len(spec.Labels) > 0 {
		np.Spec.Labels = spec.Labels
	}

	if len(spec.Taints) > 0 {
		np.Spec.NodePool.Taints = make([]hypershiftv1beta1.Taint, 0, len(spec.Taints))
		for _, taint := range spec.Taints {
			np.Spec.NodePool.Taints = append(np.Spec.NodePool.Taints, hypershiftv1beta1.Taint{
				Key:    taint.Key,
				Value:  taint.Value,
				Effect: taint.Effect,
			})
		}
	}

	if len(spec.TuningConfigs) > 0 {
		np.Spec.NodePool.TuningConfig = make([]corev1.LocalObjectReference, 0, len(spec.TuningConfigs))
		for _, name := range spec.TuningConfigs {
			np.Spec.NodePool.TuningConfig = append(np.Spec.NodePool.TuningConfig, corev1.LocalObjectReference{Name: name})
		}
	}

	if spec.NodeDrainGracePeriod != nil {
		np.Spec.NodePool.NodeDrainTimeout = spec.NodeDrainGracePeriod
	}

	np.Spec.AutoRepair = ptr.To(spec.AutoRepair)
	np.Spec.EC2InstanceMetadataHttpTokens = toSeaplaneEC2MetadataHTTPTokens(spec.Ec2MetadataHTTPTokens)

	if len(spec.AdditionalTags) > 0 {
		np.Spec.Tags = make(map[string]string, len(spec.AdditionalTags))
		for k, v := range spec.AdditionalTags {
			np.Spec.Tags[k] = v
		}
	}

	return np, nil
}

// toHypershiftResourceRefs maps a list of resource IDs (security groups)
// onto HyperShift resource references.
func toHypershiftResourceRefs(ids []string) []hypershiftv1beta1.AWSResourceReference {
	if len(ids) == 0 {
		return nil
	}
	refs := make([]hypershiftv1beta1.AWSResourceReference, 0, len(ids))
	for _, id := range ids {
		idCopy := id
		refs = append(refs, hypershiftv1beta1.AWSResourceReference{ID: &idCopy})
	}
	return refs
}

// toSeaplaneRootVolume maps the RosaMachinePoolSpec volume size (GiB) onto
// the Platform API root volume.
func toSeaplaneRootVolume(volumeSize int) *seaplanev1alpha1.Volume {
	if volumeSize <= 0 {
		return nil
	}
	return &seaplanev1alpha1.Volume{
		Size: int64(volumeSize),
		Type: "gp3",
	}
}

// toSeaplaneImageType maps the RosaMachinePoolSpec image type onto the
// Platform API image type.
func toSeaplaneImageType(imageType string) hypershiftv1beta1.ImageType {
	if imageType == "Windows" {
		return hypershiftv1beta1.ImageType(seaplaneImageTypeWindows)
	}
	return hypershiftv1beta1.ImageType(seaplaneImageTypeLinux)
}

// toSeaplanePlacement maps spot market options onto the EC2 placement
// options.
func toSeaplanePlacement(spot *expinfrav1.RosaSpotMarketOptions) *hypershiftv1beta1.PlacementOptions {
	if spot == nil {
		return nil
	}
	placement := &hypershiftv1beta1.PlacementOptions{
		MarketType: hypershiftv1beta1.MarketTypeSpot,
	}
	if spot.MaxPrice != nil && *spot.MaxPrice != "" && *spot.MaxPrice != "on-demand" {
		placement.Spot = hypershiftv1beta1.SpotOptions{MaxPrice: *spot.MaxPrice}
	}
	return placement
}

// toSeaplaneEC2MetadataHTTPTokens maps the RosaMachinePoolSpec IMDS setting
// onto the Platform API value.
func toSeaplaneEC2MetadataHTTPTokens(tokens rosacontrolplanev1.Ec2MetadataHTTPTokens) seaplanev1alpha1.EC2InstanceMetadataHttpTokens {
	if tokens == rosacontrolplanev1.Ec2MetadataHTTPTokensOptional {
		return seaplanev1alpha1.EC2InstanceMetadataHttpTokensOptional
	}
	return seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired
}

// UpdateSeaplaneNodePoolMutableFields applies the mutable fields of the
// desired NodePool onto the current one, reporting whether anything changed.
// Mutable fields mirror the Platform API write-mode markers; the release
// image is intentionally excluded because changing it is a node upgrade,
// which is not reconciled here.
func UpdateSeaplaneNodePoolMutableFields(current, desired *seaplanev1alpha1.NodePool) bool {
	updated := false

	if !ptr.Equal(current.Spec.NodePool.Replicas, desired.Spec.NodePool.Replicas) {
		current.Spec.NodePool.Replicas = desired.Spec.NodePool.Replicas
		updated = true
	}
	if !equalNodePoolAutoScaling(current.Spec.NodePool.AutoScaling, desired.Spec.NodePool.AutoScaling) {
		current.Spec.NodePool.AutoScaling = desired.Spec.NodePool.AutoScaling
		updated = true
	}
	if !ptr.Equal(current.Spec.AutoRepair, desired.Spec.AutoRepair) {
		current.Spec.AutoRepair = desired.Spec.AutoRepair
		updated = true
	}
	if !equalStringMaps(current.Spec.Labels, desired.Spec.Labels) {
		current.Spec.Labels = desired.Spec.Labels
		updated = true
	}
	if !equalStringMaps(current.Spec.Tags, desired.Spec.Tags) {
		current.Spec.Tags = desired.Spec.Tags
		updated = true
	}
	if !equalDurations(current.Spec.NodePool.NodeDrainTimeout, desired.Spec.NodePool.NodeDrainTimeout) {
		current.Spec.NodePool.NodeDrainTimeout = desired.Spec.NodePool.NodeDrainTimeout
		updated = true
	}

	return updated
}

func equalNodePoolAutoScaling(a, b *hypershiftv1beta1.NodePoolAutoScaling) bool {
	if a == nil || b == nil {
		return a == b
	}
	return ptr.Equal(a.Min, b.Min) && a.Max == b.Max
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func equalDurations(a, b *metav1.Duration) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Duration == b.Duration
}
