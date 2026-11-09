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

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	ec2svc "github.com/aws/aws-sdk-go-v2/service/ec2"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplaneclientset "github.com/openshift-online/rosa-seaplane-api/clientset"
	"github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	rosaaws "github.com/openshift/rosa/pkg/aws"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/feature"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/rosa"
	clusterv1beta1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	v1beta1conditions "sigs.k8s.io/cluster-api/util/deprecated/v1beta1/conditions"
)

// seaplaneEnabled reports whether the seaplane v2 reconciliation path is active.
func seaplaneEnabled() bool {
	return feature.Gates.Enabled(feature.ROSASeaplaneV2)
}

// reconcileSeaplaneNormal reconciles the ROSAControlPlane against the v2
// Seaplane Platform API instead of OCM. It is the minimal phase-1
// implementation: create the cluster on first reconcile, then poll its phase
// until Ready. Updates (spec drift, log forwarders, component routes,
// external auth, kubeconfig secret) are not reconciled yet.
func (r *ROSAControlPlaneReconciler) reconcileSeaplaneNormal(
	ctx context.Context, rosaScope *scope.ROSAControlPlaneScope, creator *rosaaws.Creator,
) (res ctrl.Result, reterr error) {
	rosaScope.Info("Reconciling ROSAControlPlane via Seaplane Platform API v2")

	seaplaneClient, err := r.newSeaplaneClient(rosaScope, creator)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create seaplane client: %w", err)
	}

	clusters := seaplaneClient.SeaplaneV1alpha1().Clusters()
	name := rosaScope.ControlPlane.Spec.RosaClusterName

	cluster, err := clusters.Get(ctx, name, platform.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get seaplane cluster %q: %w", name, err)
	}

	if cluster == nil || apierrors.IsNotFound(err) {
		if err := r.createSeaplaneCluster(ctx, rosaScope, seaplaneClient); err != nil {
			v1beta1conditions.MarkFalse(rosaScope.ControlPlane,
				rosacontrolplanev1.ROSAControlPlaneReadyCondition,
				rosacontrolplanev1.ReconciliationFailedReason,
				clusterv1beta1.ConditionSeverityError,
				"%s", err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to create seaplane cluster: %w", err)
		}
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}

	return r.updateSeaplaneClusterStatus(rosaScope, cluster)
}

// newSeaplaneClient builds the Platform API v2 client, honouring the
// injectable NewSeaplaneClient factory.
func (r *ROSAControlPlaneReconciler) newSeaplaneClient(
	rosaScope *scope.ROSAControlPlaneScope, creator *rosaaws.Creator,
) (seaplaneclientset.Interface, error) {
	if r.NewSeaplaneClient == nil {
		return nil, fmt.Errorf("failed to create seaplane client: NewSeaplaneClient is nil")
	}
	return r.NewSeaplaneClient(rosaScope.Session(), creator, rosaScope.ControlPlane.Spec.Region)
}

// createSeaplaneCluster resolves the control plane inputs and creates the
// cluster via the Platform API.
func (r *ROSAControlPlaneReconciler) createSeaplaneCluster(
	ctx context.Context, rosaScope *scope.ROSAControlPlaneScope, seaplaneClient seaplaneclientset.Interface,
) error {
	rosaRoleConfig, err := r.reconcileRosaRoleConfig(ctx, rosaScope)
	if err != nil {
		return fmt.Errorf("cannot reconcile RosaRoleConfig: %w", err)
	}

	rosaNet := &expinfrav1.ROSANetwork{}
	if rosaScope.ControlPlane.Spec.ROSANetworkRef != nil {
		objKey := client.ObjectKey{
			Name:      rosaScope.ControlPlane.Spec.ROSANetworkRef.Name,
			Namespace: rosaScope.ControlPlane.Namespace,
		}
		if err := rosaScope.Client.Get(ctx, objKey, rosaNet); err != nil {
			return fmt.Errorf("failed to fetch ROSANetwork: %w", err)
		}
	}

	subnetID, err := rosa.SeaplaneClusterSubnetID(rosaScope.ControlPlane.Spec, rosaNet)
	if err != nil {
		return err
	}

	subnetInfo, err := describeSeaplaneSubnet(ctx, rosaScope.Session(), subnetID)
	if err != nil {
		return err
	}

	cluster, err := rosa.BuildSeaplaneCluster(
		rosaScope.ControlPlane.Spec,
		rosaRoleConfig,
		rosaNet,
		rosa.SeaplaneReleaseImage(),
		subnetInfo,
		rosa.SeaplaneBaseDomain(),
	)
	if err != nil {
		return err
	}

	created, err := seaplaneClient.SeaplaneV1alpha1().Clusters().Create(ctx, cluster, platform.CreateOptions{})
	if err != nil {
		return rosa.WithSeaplaneAPIErrorDetails(err)
	}

	rosaScope.Info("seaplane cluster created", "name", created.Name, "uid", created.UID)
	rosaScope.ControlPlane.Status.ID = string(created.UID)
	rosaScope.ControlPlane.Status.Ready = false
	return nil
}

// describeSeaplaneSubnet resolves the VPC and availability zone of the control
// plane subnet via EC2, mirroring the rosa CLI create cluster flow. It is a
// package-level var so tests can stub it without real AWS calls.
var describeSeaplaneSubnet = func(
	ctx context.Context, awsCfg awsv2.Config, subnetID string,
) (rosa.SeaplaneSubnetInfo, error) {
	out, err := ec2svc.NewFromConfig(awsCfg).DescribeSubnets(ctx, &ec2svc.DescribeSubnetsInput{
		SubnetIds: []string{subnetID},
	})
	if err != nil {
		return rosa.SeaplaneSubnetInfo{}, fmt.Errorf("failed to describe subnet %q: %w", subnetID, err)
	}
	if len(out.Subnets) == 0 {
		return rosa.SeaplaneSubnetInfo{}, fmt.Errorf("subnet %q not found", subnetID)
	}
	info := rosa.SeaplaneSubnetInfo{
		SubnetID: subnetID,
		VPCID:    strVal(out.Subnets[0].VpcId),
		Zone:     strVal(out.Subnets[0].AvailabilityZone),
	}
	if info.VPCID == "" {
		return rosa.SeaplaneSubnetInfo{}, fmt.Errorf("subnet %q has no VPC ID", subnetID)
	}
	return info, nil
}

// updateSeaplaneClusterStatus maps the Platform API cluster phase onto the
// ROSAControlPlane status and conditions.
func (r *ROSAControlPlaneReconciler) updateSeaplaneClusterStatus(
	rosaScope *scope.ROSAControlPlaneScope, cluster *seaplanev1alpha1.Cluster,
) (ctrl.Result, error) {
	rosaScope.ControlPlane.Status.ID = string(cluster.UID)
	rosaScope.ControlPlane.Status.Version = cluster.Status.Version
	rosaScope.ControlPlane.Status.Ready = false

	switch cluster.Status.Phase {
	case seaplanev1alpha1.ClusterPhaseReady:
		v1beta1conditions.MarkTrue(rosaScope.ControlPlane, rosacontrolplanev1.ROSAControlPlaneReadyCondition)
		rosaScope.ControlPlane.Status.Ready = true
		rosaScope.ControlPlane.Spec.ControlPlaneEndpoint.Host = cluster.Status.ControlPlaneEndpoint.Host
		rosaScope.ControlPlane.Spec.ControlPlaneEndpoint.Port = cluster.Status.ControlPlaneEndpoint.Port
		return ctrl.Result{}, nil
	case seaplanev1alpha1.ClusterPhaseDeleting:
		v1beta1conditions.MarkFalse(rosaScope.ControlPlane,
			rosacontrolplanev1.ROSAControlPlaneReadyCondition,
			string(cluster.Status.Phase),
			clusterv1beta1.ConditionSeverityError,
			"cluster is deleting")
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	default:
		v1beta1conditions.MarkFalse(rosaScope.ControlPlane,
			rosacontrolplanev1.ROSAControlPlaneReadyCondition,
			string(cluster.Status.Phase),
			clusterv1beta1.ConditionSeverityInfo,
			"waiting for cluster to become ready")
		rosaScope.Info("waiting for seaplane cluster to become ready", "phase", cluster.Status.Phase)
		return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
	}
}

// reconcileSeaplaneDelete deletes the cluster via the Platform API and waits
// for it to disappear before removing the finalizer.
func (r *ROSAControlPlaneReconciler) reconcileSeaplaneDelete(
	ctx context.Context, rosaScope *scope.ROSAControlPlaneScope, creator *rosaaws.Creator,
) (res ctrl.Result, reterr error) {
	rosaScope.Info("Reconciling ROSAControlPlane delete via Seaplane Platform API v2")

	seaplaneClient, err := r.newSeaplaneClient(rosaScope, creator)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create seaplane client: %w", err)
	}

	clusters := seaplaneClient.SeaplaneV1alpha1().Clusters()
	name := rosaScope.ControlPlane.Spec.RosaClusterName

	cluster, err := clusters.Get(ctx, name, platform.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get seaplane cluster %q: %w", name, err)
	}
	if cluster == nil || apierrors.IsNotFound(err) {
		// cluster is deleted, removing finalizer.
		controllerutil.RemoveFinalizer(rosaScope.ControlPlane, ROSAControlPlaneFinalizer)
		return ctrl.Result{}, nil
	}

	if cluster.Spec.DeleteProtection != nil && *cluster.Spec.DeleteProtection {
		rosaScope.Info("Delete protection is enabled, removing finalizer without deleting the ROSA cluster")
		controllerutil.RemoveFinalizer(rosaScope.ControlPlane, ROSAControlPlaneFinalizer)
		return ctrl.Result{}, nil
	}

	if cluster.Status.Phase != seaplanev1alpha1.ClusterPhaseDeleting {
		if err := clusters.Delete(ctx, name, platform.DeleteOptions{}); err != nil {
			v1beta1conditions.MarkFalse(rosaScope.ControlPlane,
				rosacontrolplanev1.ROSAControlPlaneReadyCondition,
				rosacontrolplanev1.ROSAControlPlaneDeletionFailedReason,
				clusterv1beta1.ConditionSeverityError,
				"failed to delete ROSAControlPlane: %s", err.Error())
			return ctrl.Result{}, err
		}
	}

	v1beta1conditions.MarkFalse(rosaScope.ControlPlane,
		rosacontrolplanev1.ROSAControlPlaneReadyCondition,
		string(cluster.Status.Phase),
		clusterv1beta1.ConditionSeverityInfo,
		"deleting")
	rosaScope.ControlPlane.Status.Ready = false
	rosaScope.Info("waiting for seaplane cluster to be deleted")
	// Requeue to remove the finalizer when the cluster is fully deleted.
	return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
}

// strVal is a nil-safe string pointer dereference.
func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
