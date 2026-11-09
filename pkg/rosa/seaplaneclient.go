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
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	seaplaneclientset "github.com/openshift-online/rosa-seaplane-api/clientset"
	seaplanerest "github.com/openshift-online/rosa-seaplane-api/clientset/rest"
	rosaaws "github.com/openshift/rosa/pkg/aws"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// SeaplaneURLEnvVar is the environment variable that holds the v2 Seaplane
	// Platform API base URL (e.g. https://abc123.execute-api.us-east-1.amazonaws.com).
	SeaplaneURLEnvVar = "SEAPLANE_URL"

	// SeaplaneReleaseImageEnvVar is the environment variable that holds the
	// OCP release payload image used when creating a Seaplane v2 cluster.
	// The RosaControlPlane API has no release-image field yet; until one is
	// added, the release image is taken from the environment.
	SeaplaneReleaseImageEnvVar = "SEAPLANE_RELEASE_IMAGE"

	// SeaplaneBaseDomainEnvVar optionally holds the DNS base domain to use
	// when creating a Seaplane v2 cluster. When empty, the Platform API
	// operator allocates a DNS reservation automatically.
	SeaplaneBaseDomainEnvVar = "SEAPLANE_BASE_DOMAIN"
)

// awsRegionRE matches standard and GovCloud AWS region names within a URL
// hostname, e.g. us-east-1, ap-southeast-1, us-gov-east-1.
var awsRegionRE = regexp.MustCompile(`[a-z]+-(?:[a-z]+-)+\d+`)

// SeaplaneURL returns the Platform API v2 base URL from the environment.
func SeaplaneURL() string {
	return os.Getenv(SeaplaneURLEnvVar)
}

// SeaplaneReleaseImage returns the OCP release payload image from the environment.
func SeaplaneReleaseImage() string {
	return os.Getenv(SeaplaneReleaseImageEnvVar)
}

// SeaplaneBaseDomain returns the DNS base domain from the environment.
func SeaplaneBaseDomain() string {
	return os.Getenv(SeaplaneBaseDomainEnvVar)
}

// ValidateSeaplaneURL returns an error if rawURL is empty, cannot be parsed,
// or its scheme is not HTTPS.
func ValidateSeaplaneURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("%s is not set", SeaplaneURLEnvVar)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", SeaplaneURLEnvVar, rawURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s must use HTTPS, got %q", SeaplaneURLEnvVar, rawURL)
	}
	return nil
}

// ExtractSeaplaneRegion parses the AWS region from a Platform API endpoint
// URL. It returns an error when the URL contains no recognizable region.
func ExtractSeaplaneRegion(rawURL string) (string, error) {
	if err := ValidateSeaplaneURL(rawURL); err != nil {
		return "", err
	}
	u, _ := url.Parse(rawURL)
	region := awsRegionRE.FindString(u.Hostname())
	if region == "" {
		return "", fmt.Errorf("cannot derive AWS region from %s %q", SeaplaneURLEnvVar, rawURL)
	}
	return region, nil
}

// NewSeaplaneClient builds a Platform API v2 clientset from the AWS session
// credentials and creator identity used for reconciliation. Requests are
// signed with SigV4; the account ID is taken from the creator. The region is
// resolved from region when set, falling back to extraction from the
// Platform API URL.
func NewSeaplaneClient(awsCfg awsv2.Config, creator *rosaaws.Creator, region string) (seaplaneclientset.Interface, error) {
	if creator == nil {
		return nil, fmt.Errorf("seaplane: creator must not be nil")
	}

	rawURL := SeaplaneURL()
	if err := ValidateSeaplaneURL(rawURL); err != nil {
		return nil, err
	}

	// Resolve region: explicit region takes precedence, then extracted from URL.
	if region == "" {
		var err error
		region, err = ExtractSeaplaneRegion(rawURL)
		if err != nil {
			return nil, err
		}
	} else if urlRegion, err := ExtractSeaplaneRegion(rawURL); err == nil && urlRegion != region {
		return nil, fmt.Errorf(
			"region %s does not match region in %s (%s)",
			region, SeaplaneURLEnvVar, urlRegion,
		)
	}

	return seaplaneclientset.NewForConfig(&seaplanerest.Config{
		Host:      rawURL,
		Region:    region,
		AccountID: creator.AccountID,
		CallerARN: creator.ARN,
		AWSConfig: awsCfg,
	})
}

// WithSeaplaneAPIErrorDetails includes field-level validation causes
// returned by the Platform API, mirroring the rosa CLI's
// seaplane.WithAPIErrorDetails.
func WithSeaplaneAPIErrorDetails(err error) error {
	var statusError apierrors.APIStatus
	if !errors.As(err, &statusError) {
		return err
	}
	details := statusError.Status().Details
	if details == nil || len(details.Causes) == 0 {
		return err
	}
	causes := make([]string, 0, len(details.Causes))
	for _, cause := range details.Causes {
		if cause.Field != "" {
			causes = append(causes, fmt.Sprintf("%s: %s", cause.Field, cause.Message))
		} else {
			causes = append(causes, cause.Message)
		}
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(causes, "; "))
}
