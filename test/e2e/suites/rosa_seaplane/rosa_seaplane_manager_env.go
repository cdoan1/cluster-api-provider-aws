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
	"fmt"
	"os"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoretry "k8s.io/client-go/util/retry"

	"sigs.k8s.io/cluster-api-provider-aws/v2/test/e2e/shared"
)

// seaplaneManagerEnvVars are the environment variables consumed by the capa
// manager when the ROSASeaplaneV2 feature gate is enabled. They are patched
// into the controller deployment by this suite rather than declared in
// config/manager, because clusterctl treats a clusterctl variable with an
// empty default (e.g. the optional SEAPLANE_BASE_DOMAIN) as missing and
// fails variable substitution for every user of the provider components.
var seaplaneManagerEnvVars = []string{
	"SEAPLANE_URL",
	"SEAPLANE_RELEASE_IMAGE",
	"SEAPLANE_BASE_DOMAIN",
}

// patchSeaplaneManagerEnv injects the SEAPLANE_* environment variables into
// the capa controller manager deployment after clusterctl init. All variables
// are applied in a single update (with conflict retries) so the manager only
// rolls once, before the first spec reconciles.
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
