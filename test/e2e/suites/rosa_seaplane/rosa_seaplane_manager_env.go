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

// Package rosa_seaplane hosts the ROSA v2 Seaplane Platform API e2e suite:
// the sanity spec and the manager environment wiring it depends on.
package rosa_seaplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/test/e2e/shared"
)

// userAWSCredentials captures the user-provided AWS_B64ENCODED_CREDENTIALS
// at package initialization — before the shared suite setup replaces the
// variable with credentials for the CloudFormation bootstrapper IAM user.
// The Seaplane Platform API rejects that bootstrapper principal (every
// controller request fails with an unrecognized 4xx), while the user's own
// identity is accepted, so the seaplane suite re-points the manager's
// credentials secret at the captured value.
var userAWSCredentials = os.Getenv("AWS_B64ENCODED_CREDENTIALS")

// seaplaneManagerEnvVars are the environment variables consumed by the capa
// manager when the ROSASeaplaneV2 feature gate is enabled. They are patched
// into the controller deployment by this suite rather than declared in
// config/manager, because clusterctl treats a clusterctl variable with an
// empty default (e.g. the optional SEAPLANE_BASE_DOMAIN) as missing and
// fails variable substitution for every user of the provider components.
var seaplaneManagerEnvVars = []string{
	"SEAPLANE_URL",
	"SEAPLANE_RELEASE_IMAGE",
}

// patchSeaplaneManagerEnv injects the SEAPLANE_* environment variables into
// the capa controller manager deployment after clusterctl init, waits for the
// resulting rollout, and ensures the default AWSClusterControllerIdentity
// exists. All variables are applied in a single update (with conflict
// retries) so the manager only rolls once, before the first spec reconciles.
func patchSeaplaneManagerEnv(e2eCtx *shared.E2EContext) {
	values := map[string]string{}
	for _, name := range seaplaneManagerEnvVars {
		if value, set := os.LookupEnv(name); set && value != "" {
			values[name] = value
		}
	}

	c := e2eCtx.Environment.BootstrapClusterProxy.GetClient()
	key := types.NamespacedName{Namespace: "capa-system", Name: "capa-controller-manager"}

	err := clientgoretry.RetryOnConflict(clientgoretry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Get(context.TODO(), key, deployment); err != nil {
			return err
		}
		container := &deployment.Spec.Template.Spec.Containers[0]
		for name, value := range values {
			setContainerEnv(container, name, value)
		}
		return c.Update(context.TODO(), deployment)
	})
	Expect(err).ToNot(HaveOccurred(), "failed to update the capa controller manager deployment")

	ginkgo.By(fmt.Sprintf("Patched %d SEAPLANE_* environment variables into the capa manager deployment", len(values)))

	// The shared suite setup overwrites AWS_B64ENCODED_CREDENTIALS with the
	// bootstrapper user's keys, which the Platform API rejects with a 403.
	// Restore the user's credentials in the manager's credentials secret.
	// The manager mounts that secret as the SDK shared credentials file and
	// the SDK caches credentials after the first retrieval, so the pod must
	// be restarted to pick the new value up.
	if userAWSCredentials != "" {
		patchManagerCredentials(c, userAWSCredentials)
		restartManagerDeployment(c, key)
	}

	// Wait for the rollout (env change and/or restart) so that the new
	// manager and its webhooks are serving before anything else touches the
	// cluster.
	waitManagerRollout(c, key)

	// The shared suite helper creates the controller identity right after
	// clusterctl init, but that can race the manager's webhook readiness
	// (failurePolicy=fail) and its error is swallowed. Ensure the identity
	// exists now, with the error surfaced.
	ensureControllerIdentity(c)
}

// restartManagerDeployment triggers a manager rollout restart so pods pick
// up the updated credentials secret (the SDK caches credentials from the
// mounted shared credentials file after first retrieval).
func restartManagerDeployment(c client.Client, key types.NamespacedName) {
	ginkgo.By("Restarting the capa manager to pick up the restored credentials")
	err := clientgoretry.RetryOnConflict(clientgoretry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if err := c.Get(context.TODO(), key, deployment); err != nil {
			return err
		}
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
		return c.Update(context.TODO(), deployment)
	})
	Expect(err).ToNot(HaveOccurred(), "failed to restart the capa controller manager deployment")
}

// waitManagerRollout waits until the capa manager deployment has rolled out
// the generation produced by the env patch and reports available replicas.
func waitManagerRollout(c client.Client, key types.NamespacedName) {
	ginkgo.By("Waiting for the capa manager rollout")
	Eventually(func() (bool, error) {
		deployment := &appsv1.Deployment{}
		if err := c.Get(context.TODO(), key, deployment); err != nil {
			return false, err
		}
		replicas := int32(1)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		return deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.UpdatedReplicas == replicas &&
			deployment.Status.AvailableReplicas == replicas, nil
	}, 5*time.Minute, 5*time.Second).Should(BeTrue(), "capa manager rollout should complete")
}

// patchManagerCredentials re-points the capa manager's credentials secret at
// the user-provided encoded credentials (a base64 ini profile), replacing the
// bootstrapper user's keys that clusterctl installed. The secret's data value
// holds the raw ini profile — the base64 envelope is a transport encoding
// handled by the API server — so the blob must be decoded before writing.
func patchManagerCredentials(c client.Client, encoded string) {
	ginkgo.By("Restoring the user's AWS credentials in the capa manager credentials secret")
	key := types.NamespacedName{Namespace: "capa-system", Name: "capa-manager-bootstrap-credentials"}

	credentials, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	Expect(err).ToNot(HaveOccurred(), "AWS_B64ENCODED_CREDENTIALS is not valid base64")

	err = clientgoretry.RetryOnConflict(clientgoretry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := c.Get(context.TODO(), key, secret); err != nil {
			return err
		}
		if bytes.Equal(secret.Data["credentials"], credentials) {
			return nil
		}
		secret.Data["credentials"] = credentials
		return c.Update(context.TODO(), secret)
	})
	Expect(err).ToNot(HaveOccurred(), "failed to update the capa manager credentials secret")
}

// ensureControllerIdentity creates the default AWSClusterControllerIdentity
// if it does not exist yet. The ROSAControlPlane scope resolves its AWS
// session through this identity.
func ensureControllerIdentity(c client.Client) {
	ginkgo.By("Ensuring the AWSClusterControllerIdentity \"default\" exists")
	identity := &infrav1.AWSClusterControllerIdentity{}
	err := c.Get(context.TODO(), types.NamespacedName{Name: infrav1.AWSClusterControllerIdentityName}, identity)
	if err == nil {
		return
	}
	if !apierrors.IsNotFound(err) {
		Expect(err).ToNot(HaveOccurred(), "failed to get the AWSClusterControllerIdentity")
	}

	identity = &infrav1.AWSClusterControllerIdentity{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       string(infrav1.ControllerIdentityKind),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: infrav1.AWSClusterControllerIdentityName,
		},
		Spec: infrav1.AWSClusterControllerIdentitySpec{
			AWSClusterIdentitySpec: infrav1.AWSClusterIdentitySpec{
				AllowedNamespaces: &infrav1.AllowedNamespaces{},
			},
		},
	}
	Expect(c.Create(context.TODO(), identity)).To(Succeed(),
		"failed to create the AWSClusterControllerIdentity \"default\"")
}

// setContainerEnv sets an environment variable on a container, replacing an
// existing entry with the same name or appending a new one.
func setContainerEnv(container *corev1.Container, name, value string) {
	for i, env := range container.Env {
		if env.Name == name {
			container.Env[i].Value = value
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}
