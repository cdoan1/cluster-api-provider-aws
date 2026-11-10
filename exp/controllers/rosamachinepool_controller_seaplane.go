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
	"fmt"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplaneclientset "github.com/openshift-online/rosa-seaplane-api/clientset"
	"github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/feature"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/rosa"
	clusterv1beta1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/cluster-api/util/annotations"
	v1beta1conditions "sigs.k8s.io/cluster-api/util/deprecated/v1beta1/conditions"
)

// newSeaplaneNodePoolClient builds the Platform API v2 client for the
// ROSAMachinePool reconciler, honouring the injectable NewSeaplaneClient
// factory.
func (r *ROSAMachinePoolReconciler) newSeaplaneNodePoolClient(
	ctx context.Context, rosaControlPlaneScope *scope.ROSAControlPlaneScope,
) (seaplaneclientset.Interface, error) {
	if r.NewSeaplaneClient == nil {
		return nil, fmt.Errorf("failed to create seaplane client: NewSeaplaneClient is nil")
	}
	return r.NewSeaplaneClient(ctx, rosaControlPlaneScope.Session(), rosaControlPlaneScope.ControlPlane.Spec.Region)
}

// reconcileSeaplaneNormal reconciles the ROSAMachinePool against the v2
// Seaplane Platform API instead of OCM. It is the machine pool counterpart of
// the ROSAControlPlane seaplane path: create the NodePool on first reconcile,
// reconcile mutable fields, then poll its phase until Ready. Node upgrades
// (release image changes) and ProviderIDList reconciliation are not
// implemented yet.
func (r *ROSAMachinePoolReconciler) reconcileSeaplaneNormal(
	ctx context.Context, machinePoolScope *scope.RosaMachinePoolScope,
	rosaControlPlaneScope *scope.ROSAControlPlaneScope,
) (ctrl.Result, error) {
	machinePoolScope.Info("Reconciling ROSAMachinePool via Seaplane Platform API v2")

	seaplaneClient, err := r.newSeaplaneNodePoolClient(ctx, rosaControlPlaneScope)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create seaplane client: %w", err)
	}

	clusterName := rosaControlPlaneScope.ControlPlane.Spec.RosaClusterName
	nodePoolName := rosa.SeaplaneNodePoolName(clusterName, machinePoolScope.RosaMachinePool.Spec.NodePoolName)
	nodePools := seaplaneClient.SeaplaneV1alpha1().NodePools()

	nodePool, err := nodePools.Get(ctx, nodePoolName, platform.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get seaplane node pool %q: %w", nodePoolName, err)
	}

	if nodePool == nil || apierrors.IsNotFound(err) {
		if err := r.createSeaplaneNodePool(ctx, machinePoolScope, rosaControlPlaneScope, seaplaneClient); err != nil {
			v1beta1conditions.MarkFalse(machinePoolScope.RosaMachinePool,
				expinfrav1.RosaMachinePoolReadyCondition,
				expinfrav1.RosaMachinePoolReconciliationFailedReason,
				clusterv1beta1.ConditionSeverityError,
				"failed to create ROSAMachinePool: %s", err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to create seaplane node pool: %w", err)
		}
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	return r.updateSeaplaneNodePoolStatus(ctx, machinePoolScope, nodePools, nodePool)
}

// createSeaplaneNodePool resolves the parent cluster inputs and creates the
// NodePool via the Platform API.
func (r *ROSAMachinePoolReconciler) createSeaplaneNodePool(
	ctx context.Context, machinePoolScope *scope.RosaMachinePoolScope,
	rosaControlPlaneScope *scope.ROSAControlPlaneScope, seaplaneClient seaplaneclientset.Interface,
) error {
	clusterName := rosaControlPlaneScope.ControlPlane.Spec.RosaClusterName
	cluster, err := seaplaneClient.SeaplaneV1alpha1().Clusters().Get(ctx, clusterName, platform.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get parent cluster %q: %w", clusterName, err)
	}

	awsPlatform := cluster.Spec.HostedCluster.Platform.AWS
	if awsPlatform == nil {
		return fmt.Errorf("cluster %q has no AWS platform configuration", clusterName)
	}

	instanceProfile := rosa.SeaplaneInstanceProfile(rosaControlPlaneScope.ControlPlane.Spec.RolesRef)
	if instanceProfile == "" {
		return fmt.Errorf("cannot derive worker instance profile from control plane roles ref")
	}

	releaseImage := cluster.Spec.HostedCluster.Release.Image
	if mpVersion := machinePoolScope.RosaMachinePool.Spec.Version; mpVersion != "" && mpVersion != cluster.Status.Version {
		return fmt.Errorf(
			"rosaMachinePoolSpec.version %q does not match cluster version %q; node pool upgrades are not reconciled by the seaplane path",
			mpVersion, cluster.Status.Version)
	}

	subnetID := machinePoolScope.RosaMachinePool.Spec.Subnet
	if subnetID == "" && awsPlatform.CloudProviderConfig != nil && awsPlatform.CloudProviderConfig.Subnet != nil {
		subnetID = strVal(awsPlatform.CloudProviderConfig.Subnet.ID)
	}

	nodePool, err := rosa.BuildSeaplaneNodePool(rosa.SeaplaneNodePoolParams{
		MachinePoolSpec: machinePoolScope.RosaMachinePool.Spec,
		CAPIReplicas:    machinePoolScope.MachinePool.Spec.Replicas,
		ClusterName:     clusterName,
		ReleaseImage:    releaseImage,
		SubnetID:        subnetID,
		InstanceProfile: instanceProfile,
	})
	if err != nil {
		return err
	}

	created, err := seaplaneClient.SeaplaneV1alpha1().NodePools().Create(ctx, nodePool, platform.CreateOptions{})
	if err != nil {
		return rosa.WithSeaplaneAPIErrorDetails(err)
	}

	machinePoolScope.Info("seaplane node pool created", "name", created.Name, "uid", created.UID)
	machinePoolScope.RosaMachinePool.Status.ID = string(created.UID)
	machinePoolScope.RosaMachinePool.Status.Ready = false
	return nil
}

// updateSeaplaneNodePoolStatus reconciles mutable fields of an existing
// NodePool and maps its phase onto the ROSAMachinePool status and conditions.
func (r *ROSAMachinePoolReconciler) updateSeaplaneNodePoolStatus(
	ctx context.Context, machinePoolScope *scope.RosaMachinePoolScope,
	nodePools platform.NodePoolInterface, nodePool *seaplanev1alpha1.NodePool,
) (ctrl.Result, error) {
	rosaMachinePool := machinePoolScope.RosaMachinePool
	machinePool := machinePoolScope.MachinePool

	nodePool, err := r.reconcileSeaplaneNodePoolUpdates(ctx, machinePoolScope, nodePools, nodePool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile seaplane node pool updates: %w", err)
	}

	var currentReplicas int32
	if nodePool.Status.Replicas != nil {
		currentReplicas = *nodePool.Status.Replicas
	}

	if annotations.ReplicasManagedByExternalAutoscaler(machinePool) {
		// Set MachinePool replicas to rosa autoscaling replicas
		if machinePool.Spec.Replicas != nil && *machinePool.Spec.Replicas != currentReplicas {
			machinePoolScope.Info("Setting MachinePool replicas to rosa autoscaling replicas",
				"local", *machinePool.Spec.Replicas,
				"external", currentReplicas)
			machinePool.Spec.Replicas = &currentReplicas
			if err := machinePoolScope.PatchCAPIMachinePoolObject(ctx); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	rosaMachinePool.Status.Replicas = currentReplicas

	switch nodePool.Status.Phase {
	case seaplanev1alpha1.NodePoolPhaseReady:
		v1beta1conditions.MarkTrue(rosaMachinePool, expinfrav1.RosaMachinePoolReadyCondition)
		rosaMachinePool.Status.Ready = true
		return ctrl.Result{}, nil
	case seaplanev1alpha1.NodePoolPhaseDeleting:
		v1beta1conditions.MarkFalse(rosaMachinePool,
			expinfrav1.RosaMachinePoolReadyCondition,
			expinfrav1.WaitingForNodePoolReason,
			clusterv1beta1.ConditionSeverityError,
			"node pool is deleting")
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	default:
		v1beta1conditions.MarkFalse(rosaMachinePool,
			expinfrav1.RosaMachinePoolReadyCondition,
			expinfrav1.WaitingForNodePoolReason,
			clusterv1beta1.ConditionSeverityInfo,
			"waiting for node pool to become ready: phase %s", nodePool.Status.Phase)
		machinePoolScope.Info("waiting for seaplane node pool to become ready", "phase", nodePool.Status.Phase)
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}
}

// reconcileSeaplaneNodePoolUpdates applies the mutable fields of the desired
// NodePool (replicas, autoscaling, auto repair, labels, tags, drain timeout)
// to the existing one, mirroring the OCM path's updateNodePool. It returns
// the NodePool as reported by the Platform API after any update; when no
// field drifted the input NodePool is returned unchanged.
func (r *ROSAMachinePoolReconciler) reconcileSeaplaneNodePoolUpdates(
	ctx context.Context, machinePoolScope *scope.RosaMachinePoolScope,
	nodePools platform.NodePoolInterface, nodePool *seaplanev1alpha1.NodePool,
) (*seaplanev1alpha1.NodePool, error) {
	desired, err := rosa.BuildSeaplaneNodePool(rosa.SeaplaneNodePoolParams{
		MachinePoolSpec: machinePoolScope.RosaMachinePool.Spec,
		CAPIReplicas:    machinePoolScope.MachinePool.Spec.Replicas,
		ClusterName:     nodePool.Spec.NodePool.ClusterName,
		ReleaseImage:    nodePool.Spec.NodePool.Release.Image,
		SubnetID:        strVal(nodePool.Spec.NodePool.Platform.AWS.Subnet.ID),
		InstanceProfile: nodePool.Spec.NodePool.Platform.AWS.InstanceProfile,
	})
	if err != nil {
		return nil, err
	}

	if !rosa.UpdateSeaplaneNodePoolMutableFields(nodePool, desired) {
		return nodePool, nil
	}

	machinePoolScope.Info("updating seaplane node pool", "name", nodePool.Name)
	updated, err := nodePools.Update(ctx, nodePool, platform.UpdateOptions{})
	if err != nil {
		return nil, rosa.WithSeaplaneAPIErrorDetails(err)
	}
	return updated, nil
}

// reconcileSeaplaneDelete deletes the NodePool via the Platform API. The
// finalizer is removed once deletion is accepted; the Platform API cascades
// the instance cleanup.
func (r *ROSAMachinePoolReconciler) reconcileSeaplaneDelete(
	ctx context.Context, machinePoolScope *scope.RosaMachinePoolScope,
	rosaControlPlaneScope *scope.ROSAControlPlaneScope,
) error {
	machinePoolScope.Info("Reconciling deletion of RosaMachinePool via Seaplane Platform API v2")

	seaplaneClient, err := r.newSeaplaneNodePoolClient(ctx, rosaControlPlaneScope)
	if err != nil {
		return fmt.Errorf("failed to create seaplane client: %w", err)
	}

	clusterName := rosaControlPlaneScope.ControlPlane.Spec.RosaClusterName
	nodePoolName := rosa.SeaplaneNodePoolName(clusterName, machinePoolScope.RosaMachinePool.Spec.NodePoolName)
	nodePools := seaplaneClient.SeaplaneV1alpha1().NodePools()

	nodePool, err := nodePools.Get(ctx, nodePoolName, platform.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get seaplane node pool %q: %w", nodePoolName, err)
	}
	if nodePool == nil || apierrors.IsNotFound(err) {
		// node pool is deleted, removing finalizer.
		controllerutil.RemoveFinalizer(machinePoolScope.RosaMachinePool, expinfrav1.RosaMachinePoolFinalizer)
		return nil
	}

	if nodePool.Status.Phase != seaplanev1alpha1.NodePoolPhaseDeleting {
		if err := nodePools.Delete(ctx, nodePoolName, platform.DeleteOptions{}); err != nil {
			return rosa.WithSeaplaneAPIErrorDetails(err)
		}
		machinePoolScope.Info("Successfully deleted seaplane NodePool")
	}

	// Mirror the OCM path: the Platform API accepts the delete request and
	// cascades instance cleanup, so the finalizer can be removed once the
	// request is accepted.
	controllerutil.RemoveFinalizer(machinePoolScope.RosaMachinePool, expinfrav1.RosaMachinePoolFinalizer)
	return nil
}

// seaplaneMachinePoolEnabled reports whether the seaplane v2 reconciliation
// path is active for machine pools.
func seaplaneMachinePoolEnabled() bool {
	return feature.Gates.Enabled(feature.ROSASeaplaneV2)
}

// strVal is a nil-safe string pointer dereference.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
