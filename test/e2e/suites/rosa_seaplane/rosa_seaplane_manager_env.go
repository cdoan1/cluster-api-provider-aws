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
// the capa controller manager deployment after clusterctl init. Changing the
// pod environment triggers a rollout, so the manager picks the values up
// before the first spec reconciles.
func patchSeaplaneManagerEnv(e2eCtx *shared.E2EContext) {
	patched := 0
	for _, name := range seaplaneManagerEnvVars {
		value, set := os.LookupEnv(name)
		if !set || value == "" {
			continue
		}
		patchManagerEnv(e2eCtx, name, value)
		patched++
	}
	ginkgo.By(fmt.Sprintf("Patched %d SEAPLANE_* environment variables into the capa manager deployment", patched))
}

func patchManagerEnv(e2eCtx *shared.E2EContext, name, value string) {
	deployment := &appsv1.Deployment{}
	err := e2eCtx.Environment.BootstrapClusterProxy.GetClient().Get(
		context.TODO(), types.NamespacedName{
			Namespace: "capa-system",
			Name:      "capa-controller-manager",
		}, deployment)
	Expect(err).ToNot(HaveOccurred(), "failed to get the capa controller manager deployment")

	container := &deployment.Spec.Template.Spec.Containers[0]
	for i, env := range container.Env {
		if env.Name == name {
			container.Env[i].Value = value
			updateManagerDeployment(e2eCtx, deployment)
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
	updateManagerDeployment(e2eCtx, deployment)
}

func updateManagerDeployment(e2eCtx *shared.E2EContext, deployment *appsv1.Deployment) {
	err := e2eCtx.Environment.BootstrapClusterProxy.GetClient().Update(context.TODO(), deployment)
	Expect(err).ToNot(HaveOccurred(), "failed to update the capa controller manager deployment")
}
