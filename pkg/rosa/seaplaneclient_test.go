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
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	. "github.com/onsi/gomega"
	rosaaws "github.com/openshift/rosa/pkg/aws"
)

func TestValidateSeaplaneURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "empty", url: "", wantErr: true},
		{name: "not https", url: "http://abc123.execute-api.us-east-1.amazonaws.com", wantErr: true},
		{name: "unparseable", url: "https://[::1", wantErr: true},
		{name: "valid", url: "https://abc123.execute-api.us-east-1.amazonaws.com", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			err := ValidateSeaplaneURL(tt.url)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
		})
	}
}

func TestExtractSeaplaneRegion(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "execute-api us-east-1", url: "https://abc123.execute-api.us-east-1.amazonaws.com", want: "us-east-1"},
		{name: "execute-api ap-southeast-1", url: "https://abc123.execute-api.ap-southeast-1.amazonaws.com", want: "ap-southeast-1"},
		{name: "govcloud", url: "https://abc123.execute-api.us-gov-east-1.amazonaws.com", want: "us-gov-east-1"},
		{name: "no region", url: "https://seaplane.example.com", wantErr: true},
		{name: "empty", url: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			region, err := ExtractSeaplaneRegion(tt.url)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(region).To(Equal(tt.want))
			}
		})
	}
}

func TestNewSeaplaneClient(t *testing.T) {
	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("id", "secret", ""),
	}
	creator := &rosaaws.Creator{
		AccountID: "123456789012",
		ARN:       "arn:aws:iam::123456789012:user/test",
		Partition: "aws",
	}

	t.Run("url env not set", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv(SeaplaneURLEnvVar, "")
		_, err := NewSeaplaneClient(awsCfg, creator, "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(SeaplaneURLEnvVar))
	})

	t.Run("region derived from url", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv(SeaplaneURLEnvVar, "https://abc123.execute-api.us-east-1.amazonaws.com")
		cs, err := NewSeaplaneClient(awsCfg, creator, "")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cs).ToNot(BeNil())
	})

	t.Run("explicit region matching url", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv(SeaplaneURLEnvVar, "https://abc123.execute-api.us-east-1.amazonaws.com")
		cs, err := NewSeaplaneClient(awsCfg, creator, "us-east-1")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(cs).ToNot(BeNil())
	})

	t.Run("region conflicts with url", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv(SeaplaneURLEnvVar, "https://abc123.execute-api.us-east-1.amazonaws.com")
		_, err := NewSeaplaneClient(awsCfg, creator, "eu-west-1")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("does not match"))
	})

	t.Run("nil creator", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv(SeaplaneURLEnvVar, "https://abc123.execute-api.us-east-1.amazonaws.com")
		_, err := NewSeaplaneClient(awsCfg, nil, "")
		g.Expect(err).To(HaveOccurred())
	})
}
