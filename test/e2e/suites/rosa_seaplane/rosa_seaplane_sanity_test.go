//go:build e2e
// +build e2e

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

package rosa_seaplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	seaplaneclientset "github.com/openshift-online/rosa-seaplane-api/clientset"
	spplatform "github.com/openshift-online/rosa-seaplane-api/clientset/platform"
	sprest "github.com/openshift-online/rosa-seaplane-api/clientset/rest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	expinfrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/exp/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/test/e2e/shared"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	clusterReadyTimeout           = 90 * time.Minute
	clusterDeletedTimeout         = 60 * time.Minute
	nodepoolReadyTimeout          = 45 * time.Minute
	oidcConfigReadyTimeout        = 15 * time.Minute
	operatorRolesDeletedTimeout   = 10 * time.Minute
	pollInterval                  = 30 * time.Second
	seaplaneSanityNamespacePrefix = "rosa-seaplane-sanity"
)

// awsRegionRE matches standard and GovCloud AWS region names within a URL
// hostname, mirroring the rosa CLI and Platform API SDK region extraction.
var awsRegionRE = regexp.MustCompile(`[a-z]+-(?:[a-z]+-)+\d+`)

// operatorRoleSpec mirrors the rosa CLI seaplane operator-role specs.
type operatorRoleSpec struct {
	suffix            string
	serviceAccounts   []string
	managedPolicyArns []string
	isWorkerRole      bool
}

// seaplaneOperatorRoleSpecs mirrors getHCPOperatorRoles from the rosa CLI
// (cmd/create/operatorroles/by_prefix_seaplane.go).
func seaplaneOperatorRoleSpecs() []operatorRoleSpec {
	return []operatorRoleSpec{
		{
			suffix:            "ingress",
			serviceAccounts:   []string{"system:serviceaccount:openshift-ingress-operator:ingress-operator"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSAIngressOperatorPolicy"},
		},
		{
			suffix:            "cloud-controller-manager",
			serviceAccounts:   []string{"system:serviceaccount:kube-system:kube-controller-manager"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSAKubeControllerPolicy"},
		},
		{
			suffix:            "ebs-csi",
			serviceAccounts:   []string{"system:serviceaccount:openshift-cluster-csi-drivers:aws-ebs-csi-driver-controller-sa"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSAAmazonEBSCSIDriverOperatorPolicy"},
		},
		{
			suffix: "image-registry",
			serviceAccounts: []string{
				"system:serviceaccount:openshift-image-registry:cluster-image-registry-operator",
				"system:serviceaccount:openshift-image-registry:registry",
			},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSAImageRegistryOperatorPolicy"},
		},
		{
			suffix:            "network-config",
			serviceAccounts:   []string{"system:serviceaccount:openshift-cloud-network-config-controller:cloud-credentials"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSACloudNetworkConfigOperatorPolicy"},
		},
		{
			suffix:            "control-plane-operator",
			serviceAccounts:   []string{"system:serviceaccount:kube-system:control-plane-operator"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSAControlPlaneOperatorPolicy"},
		},
		{
			suffix:            "node-pool-management",
			serviceAccounts:   []string{"system:serviceaccount:kube-system:capa-controller-manager"},
			managedPolicyArns: []string{"arn:aws:iam::aws:policy/service-role/ROSANodePoolManagementPolicy"},
		},
		{
			suffix: "ROSA-Worker-Role",
			managedPolicyArns: []string{
				"arn:aws:iam::aws:policy/service-role/ROSAWorkerInstancePolicy",
				"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
			},
			isWorkerRole: true,
		},
	}
}

func requireEnv(name string) string {
	value := os.Getenv(name)
	ExpectWithOffset(1, value).ToNot(BeEmpty(), fmt.Sprintf("environment variable %s must be set", name))
	return value
}

func envOrDefault(name, defaultValue string) string {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue
	}
	return value
}

// seaplaneTestContext carries the AWS identity and clients shared by the spec.
type seaplaneTestContext struct {
	seaplaneClient seaplaneclientset.Interface
	iamClient      *iam.Client
	region         string
	accountID      string
	partition      string
}

func newSeaplaneTestContext(ctx context.Context) *seaplaneTestContext {
	seaplaneURL := requireEnv("SEAPLANE_URL")
	region := awsRegionRE.FindString(seaplaneURL)
	ExpectWithOffset(1, region).ToNot(BeEmpty(),
		fmt.Sprintf("cannot derive AWS region from SEAPLANE_URL %q", seaplaneURL))

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	ExpectWithOffset(1, err).ToNot(HaveOccurred())

	identity, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	ExpectWithOffset(1, identity.Account).ToNot(BeNil())
	ExpectWithOffset(1, identity.Arn).ToNot(BeNil())

	partition := "aws"
	if parts := strings.Split(*identity.Arn, ":"); len(parts) > 1 {
		partition = parts[1]
	}

	client, err := seaplaneclientset.NewForConfig(&sprest.Config{
		Host:      seaplaneURL,
		Region:    region,
		AccountID: *identity.Account,
		CallerARN: *identity.Arn,
		AWSConfig: awsCfg,
	})
	ExpectWithOffset(1, err).ToNot(HaveOccurred())

	return &seaplaneTestContext{
		seaplaneClient: client,
		iamClient:      iam.NewFromConfig(awsCfg),
		region:         region,
		accountID:      *identity.Account,
		partition:      partition,
	}
}

// createSeaplaneOidcConfig creates a managed OIDC config and returns its
// UID and issuer URL. The config's phase stays Pending until a cluster is
// created and associated with it — mirroring the terraform-provider-rhcs
// sanity test, do NOT wait for Ready here; wait after the cluster is applied.
func (c *seaplaneTestContext) createSeaplaneOidcConfig(ctx context.Context, name string) (uid string, issuerURL string) {
	created, err := c.seaplaneClient.SeaplaneV1alpha1().OidcConfigs().Create(ctx, &v1alpha1.OidcConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.OidcConfigSpec{Type: "managed"},
	}, spplatform.CreateOptions{})
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	ExpectWithOffset(1, created).ToNot(BeNil())
	ExpectWithOffset(1, created.Spec.IssuerUrl).ToNot(BeEmpty(),
		"the OIDC issuer URL must be available immediately after creation")

	ginkgo.By(fmt.Sprintf("OIDC config %s created (phase %s, issuer %s)",
		name, created.Status.Phase, created.Spec.IssuerUrl))
	return string(created.UID), created.Spec.IssuerUrl
}

// waitForSeaplaneOidcConfigReady waits for a managed OIDC config to reach
// Ready, which only happens once a cluster has been created and associated
// with it.
func (c *seaplaneTestContext) waitForSeaplaneOidcConfigReady(ctx context.Context, name string) {
	ExpectWithOffset(1, c.seaplaneClient.SeaplaneV1alpha1().OidcConfigs().WaitUntil(
		ctx, name,
		func(config *v1alpha1.OidcConfig) bool {
			if config == nil {
				return false
			}
			ginkgo.By(fmt.Sprintf("[wait] OIDC config %s phase: %s", name, config.Status.Phase))
			return config.Status.Phase == v1alpha1.OidcConfigPhaseReady
		},
		pollInterval, oidcConfigReadyTimeout,
	)).To(Succeed())
}

func (c *seaplaneTestContext) deleteSeaplaneOidcConfig(ctx context.Context, name string) {
	err := c.seaplaneClient.SeaplaneV1alpha1().OidcConfigs().Delete(ctx, name, spplatform.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
}

// createSeaplaneOperatorRoles creates the operator and worker roles for the
// given prefix, mirroring `rosa create operator-roles --seaplane`.
func (c *seaplaneTestContext) createSeaplaneOperatorRoles(ctx context.Context, prefix, oidcIssuerURL string) {
	issuerDomain := oidcIssuerDomainFromURL(oidcIssuerURL)

	for _, spec := range seaplaneOperatorRoleSpecs() {
		roleName := fmt.Sprintf("%s-%s", prefix, spec.suffix)
		trustPolicy := c.operatorRoleTrustPolicy(spec, issuerDomain)

		tags := []iamtypes.Tag{
			{Key: ptr.To("red-hat-managed"), Value: ptr.To("true")},
			{Key: ptr.To("hypershiftPolicies"), Value: ptr.To("true")},
			{Key: ptr.To("rosaOperatorRolePrefix"), Value: ptr.To(prefix)},
			{Key: ptr.To("red-hat-managed-policies"), Value: ptr.To("true")},
		}

		_, err := c.iamClient.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 ptr.To(roleName),
			AssumeRolePolicyDocument: ptr.To(trustPolicy),
			Description:              ptr.To(fmt.Sprintf("Operator role for ROSA HCP cluster %s (%s)", prefix, spec.suffix)),
			Tags:                     tags,
		})
		if isAlreadyExists(err) {
			continue
		}
		ExpectWithOffset(1, err).ToNot(HaveOccurred())

		for _, policyArn := range spec.managedPolicyArns {
			_, err := c.iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
				RoleName:  ptr.To(roleName),
				PolicyArn: ptr.To(policyArn),
			})
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
		}

		if spec.isWorkerRole {
			_, err := c.iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
				InstanceProfileName: ptr.To(roleName),
				Tags:                tags,
			})
			if !isAlreadyExists(err) {
				ExpectWithOffset(1, err).ToNot(HaveOccurred())
			}
			_, err = c.iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
				InstanceProfileName: ptr.To(roleName),
				RoleName:            ptr.To(roleName),
			})
			if err != nil && !isAlreadyExists(err) {
				ExpectWithOffset(1, err).ToNot(HaveOccurred())
			}
		}
	}
}

func (c *seaplaneTestContext) deleteSeaplaneOperatorRoles(ctx context.Context, prefix string) {
	for _, spec := range seaplaneOperatorRoleSpecs() {
		roleName := fmt.Sprintf("%s-%s", prefix, spec.suffix)
		Eventually(func() error {
			if spec.isWorkerRole {
				_, err := c.iamClient.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
					InstanceProfileName: ptr.To(roleName),
					RoleName:            ptr.To(roleName),
				})
				if err != nil && !isNoSuchEntity(err) {
					return err
				}
				_, err = c.iamClient.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
					InstanceProfileName: ptr.To(roleName),
				})
				if err != nil && !isNoSuchEntity(err) {
					return err
				}
			}
			// IAM refuses to delete a role that still has policies attached;
			// detach everything (managed policies and inline) first.
			attached, err := c.iamClient.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
				RoleName: ptr.To(roleName),
			})
			if err != nil && !isNoSuchEntity(err) {
				return err
			}
			for _, policy := range attached.AttachedPolicies {
				_, err := c.iamClient.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
					RoleName:  ptr.To(roleName),
					PolicyArn: policy.PolicyArn,
				})
				if err != nil && !isNoSuchEntity(err) {
					return err
				}
			}
			_, err = c.iamClient.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: ptr.To(roleName)})
			if err != nil && !isNoSuchEntity(err) {
				return err
			}
			return nil
		}, operatorRolesDeletedTimeout, pollInterval).Should(Succeed())
	}
}

// operatorRoleTrustPolicy builds the role trust policy document for a spec,
// mirroring the rosa CLI seaplane operator-role creation.
func (c *seaplaneTestContext) operatorRoleTrustPolicy(spec operatorRoleSpec, issuerDomain string) string {
	if spec.isWorkerRole {
		return `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Service": ["ec2.amazonaws.com"]
    },
    "Action": "sts:AssumeRole"
  }]
}`
	}

	var subject any
	if len(spec.serviceAccounts) == 1 {
		subject = spec.serviceAccounts[0]
	} else {
		subject = spec.serviceAccounts
	}
	subjectJSON, err := json.Marshal(subject)
	ExpectWithOffset(2, err).ToNot(HaveOccurred())

	return fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:%s:iam::%s:oidc-provider/%s"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "%s:sub": %s,
        "%s:aud": "openshift"
      }
    }
  }]
}`,
		c.partition, c.accountID, issuerDomain,
		issuerDomain, string(subjectJSON), issuerDomain,
	)
}

// oidcIssuerDomainFromURL derives the OIDC issuer domain (host + path without
// scheme) from the issuer URL, mirroring the rosa CLI helper.
func oidcIssuerDomainFromURL(issuerURL string) string {
	parsed, err := url.ParseRequestURI(issuerURL)
	ExpectWithOffset(1, err).ToNot(HaveOccurred())
	return parsed.Host + parsed.Path
}

func isAlreadyExists(err error) bool {
	var alreadyExists *iamtypes.EntityAlreadyExistsException
	return errors.As(err, &alreadyExists)
}

func isNoSuchEntity(err error) bool {
	var noSuchEntity *iamtypes.NoSuchEntityException
	return errors.As(err, &noSuchEntity)
}

var _ = ginkgo.Describe("[rosa-seaplane] Seaplane sanity", func() {
	var (
		ctx       context.Context
		namespace *corev1.Namespace
		spCtx     *seaplaneTestContext
	)

	ginkgo.BeforeEach(func() {
		Expect(e2eCtx.Environment.BootstrapClusterProxy).ToNot(BeNil(), "BootstrapClusterProxy can't be nil")
		ctx = context.TODO()
		namespace = shared.SetupSpecNamespace(ctx, seaplaneSanityNamespacePrefix, e2eCtx)
	})

	ginkgo.It("creates and deletes a seaplane v2 cluster via ROSAControlPlane", func() {
		requireEnv("SEAPLANE_URL")
		releaseImage := requireEnv("SEAPLANE_RELEASE_IMAGE")
		subnetID := requireEnv("SEAPLANE_SUBNET_ID")
		clusterName := envOrDefault("SEAPLANE_CLUSTER_NAME", fmt.Sprintf("sp-e2e-%d", time.Now().Unix()))
		operatorRolesPrefix := envOrDefault("SEAPLANE_OPERATOR_ROLES_PREFIX", clusterName)
		oidcConfigName := clusterName + ".oidc"

		spCtx = newSeaplaneTestContext(ctx)

		// ── Phase 1: OIDC config ────────────────────────────────────────
		ginkgo.By("Creating managed OIDC config via the Platform API")
		oidcUID, oidcIssuerURL := spCtx.createSeaplaneOidcConfig(ctx, oidcConfigName)
		ginkgo.DeferCleanup(func(ctx context.Context) {
			ginkgo.By("Teardown: delete OIDC config")
			spCtx.deleteSeaplaneOidcConfig(ctx, oidcConfigName)
		})

		// ── Phase 2: operator roles ─────────────────────────────────────
		ginkgo.By("Creating operator roles via AWS IAM")
		spCtx.createSeaplaneOperatorRoles(ctx, operatorRolesPrefix, oidcIssuerURL)
		ginkgo.DeferCleanup(func(ctx context.Context) {
			ginkgo.By("Teardown: delete operator roles")
			spCtx.deleteSeaplaneOperatorRoles(ctx, operatorRolesPrefix)
		}, ginkgo.NodeTimeout(operatorRolesDeletedTimeout))

		rolesRef := rosacontrolplanev1.AWSRolesRef{
			IngressARN:              fmt.Sprintf("arn:%s:iam::%s:role/%s-ingress", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			ImageRegistryARN:        fmt.Sprintf("arn:%s:iam::%s:role/%s-image-registry", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			StorageARN:              fmt.Sprintf("arn:%s:iam::%s:role/%s-ebs-csi", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			NetworkARN:              fmt.Sprintf("arn:%s:iam::%s:role/%s-network-config", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			KubeCloudControllerARN:  fmt.Sprintf("arn:%s:iam::%s:role/%s-cloud-controller-manager", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			NodePoolManagementARN:   fmt.Sprintf("arn:%s:iam::%s:role/%s-node-pool-management", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
			ControlPlaneOperatorARN: fmt.Sprintf("arn:%s:iam::%s:role/%s-control-plane-operator", spCtx.partition, spCtx.accountID, operatorRolesPrefix),
		}

		// ── Phase 3: ROSAControlPlane ───────────────────────────────────
		ginkgo.By("Applying Cluster and ROSAControlPlane to the management cluster")
		bootstrapClient := e2eCtx.Environment.BootstrapClusterProxy.GetClient()

		ownerCluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: namespace.Name,
			},
			// An all-zero ClusterSpec is omitted by its omitzero JSON tag, which the
			// v1beta2 Cluster CRD rejects with "spec: Required value"; set a minimal
			// cluster network so the spec serializes non-empty.
			Spec: clusterv1.ClusterSpec{
				ClusterNetwork: clusterv1.ClusterNetwork{
					Services: clusterv1.NetworkRanges{CIDRBlocks: []string{"172.30.0.0/16"}},
					Pods:     clusterv1.NetworkRanges{CIDRBlocks: []string{"10.128.0.0/14"}},
				},
			},
		}
		Expect(bootstrapClient.Create(ctx, ownerCluster)).To(Succeed())

		rosaControlPlane := &rosacontrolplanev1.ROSAControlPlane{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName,
				Namespace: namespace.Name,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Cluster",
						Name:       ownerCluster.Name,
						UID:        ownerCluster.UID,
					},
				},
			},
			Spec: rosacontrolplanev1.RosaControlPlaneSpec{
				RosaClusterName: clusterName,
				Region:          spCtx.region,
				Version:         "4.19.0",
				Subnets:         []string{subnetID},
				OIDCID:          oidcUID,
				RolesRef:        rolesRef,
				ReleaseImage:    releaseImage,
				EndpointAccess:  rosacontrolplanev1.Public,
			},
		}
		Expect(bootstrapClient.Create(ctx, rosaControlPlane)).To(Succeed())

		// ── Phase 4: wait for the cluster to become ready ───────────────
		// ── Wait for OIDC config Ready (after cluster association) ──────
		// A managed OIDC config only reaches Ready once the cluster is created
		// and associated with it, mirroring the TF sanity test's ordering.
		ginkgo.By("Waiting for the OIDC config to reach Ready after cluster association")
		spCtx.waitForSeaplaneOidcConfigReady(ctx, oidcConfigName)

		ginkgo.By("Waiting for the ROSAControlPlane to become ready")
		Eventually(func() (bool, error) {
			cp := &rosacontrolplanev1.ROSAControlPlane{}
			if err := bootstrapClient.Get(ctx, types.NamespacedName{
				Name: clusterName, Namespace: namespace.Name,
			}, cp); err != nil {
				return false, err
			}
			return cp.Status.Ready, nil
		}, clusterReadyTimeout, pollInterval).Should(BeTrue())

		// Cross-check the cluster state via the Platform API.
		cluster, err := spCtx.seaplaneClient.SeaplaneV1alpha1().Clusters().Get(ctx, clusterName, spplatform.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(cluster.Status.Phase).To(Equal(v1alpha1.ClusterPhaseReady))

		cp := &rosacontrolplanev1.ROSAControlPlane{}
		Expect(bootstrapClient.Get(ctx, types.NamespacedName{
			Name: clusterName, Namespace: namespace.Name,
		}, cp)).To(Succeed())
		Expect(cp.Status.ID).To(Equal(string(cluster.UID)))

		// ── Phase 5: node pools ────────────────────────────────────────
		// np1 and np2 mirror the terraform-provider-rhcs sanity test: two
		// pools with distinct replica counts, both verified through the
		// ROSAMachinePool status and cross-checked via the Platform API.
		np1Name := "np1"
		np2Name := "np2"
		instanceType := envOrDefault("SEAPLANE_NODEPOOL_INSTANCE_TYPE", "m5.xlarge")
		np1Replicas := int32(2)
		np2Replicas := int32(1)

		createMachinePoolPair := func(mpName string, replicas int32) (*clusterv1.MachinePool, *expinfrav1.ROSAMachinePool) {
			capiMP := &clusterv1.MachinePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + "-" + mpName,
					Namespace: namespace.Name,
					Labels: map[string]string{
						clusterv1.ClusterNameLabel: clusterName,
					},
				},
				Spec: clusterv1.MachinePoolSpec{
					ClusterName: clusterName,
					Replicas:    ptr.To(replicas),
				},
			}
			Expect(bootstrapClient.Create(ctx, capiMP)).To(Succeed())

			rosaMP := &expinfrav1.ROSAMachinePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName + "-" + mpName,
					Namespace: namespace.Name,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: clusterv1.GroupVersion.String(),
							Kind:       "MachinePool",
							Name:       capiMP.Name,
							UID:        capiMP.UID,
						},
					},
				},
				Spec: expinfrav1.RosaMachinePoolSpec{
					NodePoolName: mpName,
					InstanceType: instanceType,
					Subnet:       subnetID,
					AutoRepair:   true,
				},
			}
			Expect(bootstrapClient.Create(ctx, rosaMP)).To(Succeed())
			return capiMP, rosaMP
		}

		ginkgo.By("Applying MachinePool and ROSAMachinePool np1")
		np1MP, np1RosaMP := createMachinePoolPair(np1Name, np1Replicas)
		ginkgo.By("Applying MachinePool and ROSAMachinePool np2")
		np2MP, np2RosaMP := createMachinePoolPair(np2Name, np2Replicas)

		waitMachinePoolReady := func(mpName string) {
			ginkgo.By(fmt.Sprintf("Waiting for ROSAMachinePool %s to become ready", mpName))
			Eventually(func() (bool, error) {
				mp := &expinfrav1.ROSAMachinePool{}
				if err := bootstrapClient.Get(ctx, types.NamespacedName{
					Name: clusterName + "-" + mpName, Namespace: namespace.Name,
				}, mp); err != nil {
					return false, err
				}
				return mp.Status.Ready, nil
			}, nodepoolReadyTimeout, pollInterval).Should(BeTrue())
		}
		waitMachinePoolReady(np1Name)
		waitMachinePoolReady(np2Name)

		// Cross-check the node pools via the Platform API.
		for _, npName := range []string{np1Name, np2Name} {
			np, err := spCtx.seaplaneClient.SeaplaneV1alpha1().NodePools().Get(
				ctx, clusterName+"."+npName, spplatform.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(np.Status.Phase).To(Equal(v1alpha1.NodePoolPhaseReady))
		}

		// ── Phase 6: delete ─────────────────────────────────────────────
		ginkgo.By("Deleting the ROSAMachinePools")
		Expect(bootstrapClient.Delete(ctx, np1RosaMP)).To(Succeed())
		Expect(bootstrapClient.Delete(ctx, np2RosaMP)).To(Succeed())

		for _, mpName := range []string{np1Name, np2Name} {
			Eventually(func() (bool, error) {
				err := bootstrapClient.Get(ctx, types.NamespacedName{
					Name: clusterName + "-" + mpName, Namespace: namespace.Name,
				}, &expinfrav1.ROSAMachinePool{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}, clusterDeletedTimeout, pollInterval).Should(BeTrue())
		}

		ginkgo.By("Deleting the CAPI MachinePools")
		Expect(bootstrapClient.Delete(ctx, np1MP)).To(Succeed())
		Expect(bootstrapClient.Delete(ctx, np2MP)).To(Succeed())

		ginkgo.By("Deleting the ROSAControlPlane")
		Expect(bootstrapClient.Delete(ctx, cp)).To(Succeed())

		Eventually(func() (bool, error) {
			err := bootstrapClient.Get(ctx, types.NamespacedName{
				Name: clusterName, Namespace: namespace.Name,
			}, &rosacontrolplanev1.ROSAControlPlane{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}, clusterDeletedTimeout, pollInterval).Should(BeTrue())

		// Verify the seaplane cluster is gone from the Platform API.
		Eventually(func() (bool, error) {
			_, err := spCtx.seaplaneClient.SeaplaneV1alpha1().Clusters().Get(ctx, clusterName, spplatform.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}, clusterDeletedTimeout, pollInterval).Should(BeTrue())
	})

	ginkgo.AfterEach(func() {
		shared.DumpSpecResourcesAndCleanup(ctx, "", namespace, e2eCtx)
	})
})
