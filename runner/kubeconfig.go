// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	kubeconfigFilename = "kubeconfig"
	kubeconfigActor    = "ax-actor"
	kubeconfigToken    = "ax-placeholder"
	kubeconfigPerm     = 0o600
)

type kubeconfigDoc struct {
	APIVersion     string               `yaml:"apiVersion"`
	Kind           string               `yaml:"kind"`
	CurrentContext string               `yaml:"current-context"`
	Clusters       []kubeconfigCluster  `yaml:"clusters"`
	Contexts       []kubeconfigContext  `yaml:"contexts"`
	Users          []kubeconfigUserItem `yaml:"users"`
}

type kubeconfigCluster struct {
	Name    string                `yaml:"name"`
	Cluster kubeconfigClusterSpec `yaml:"cluster"`
}

type kubeconfigClusterSpec struct {
	Server               string `yaml:"server"`
	CertificateAuthority string `yaml:"certificate-authority,omitempty"`
}

type kubeconfigContext struct {
	Name    string                `yaml:"name"`
	Context kubeconfigContextSpec `yaml:"context"`
}

type kubeconfigContextSpec struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type kubeconfigUserItem struct {
	Name string             `yaml:"name"`
	User kubeconfigUserSpec `yaml:"user"`
}

type kubeconfigUserSpec struct {
	Token string `yaml:"token"`
}

// WriteKubeconfig writes a credential-free kubeconfig under dir for k8s-api-proxy access and
// returns its path. It returns ("", nil) when k is nil. The egress gateway replaces the
// placeholder token with the Actor JWT outside the sandbox.
func WriteKubeconfig(dir string, k *v1alpha1.KubernetesAccess, caBundle string) (string, error) {
	return writeKubeconfig(dir, k, caBundle)
}

func writeKubeconfig(dir string, k *v1alpha1.KubernetesAccess, caBundle string) (string, error) {
	if k == nil {
		return "", nil
	}
	endpoint := strings.TrimRight(strings.TrimSpace(k.GetEndpoint()), "/")
	if endpoint == "" {
		endpoint = v1alpha1.DefaultKubeProxyEndpoint
	}

	clusters := slices.Clone(k.GetClusters())
	defaultCluster := strings.TrimSpace(k.GetDefaultCluster())
	if len(clusters) == 0 {
		if defaultCluster != "" {
			clusters = []string{defaultCluster}
		} else {
			clusters = []string{v1alpha1.DefaultKubeCluster}
		}
	}
	if defaultCluster == "" {
		defaultCluster = clusters[0]
	}

	var caPath string
	if caBundle != "" {
		if _, err := os.Stat(caBundle); err == nil {
			caPath = caBundle
		}
	}

	doc := kubeconfigDoc{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: defaultCluster,
		Users: []kubeconfigUserItem{{
			Name: kubeconfigActor,
			User: kubeconfigUserSpec{Token: kubeconfigToken},
		}},
	}
	for _, c := range clusters {
		doc.Clusters = append(doc.Clusters, kubeconfigCluster{
			Name: c,
			Cluster: kubeconfigClusterSpec{
				Server:               endpoint + "/clusters/" + c,
				CertificateAuthority: caPath,
			},
		})
		doc.Contexts = append(doc.Contexts, kubeconfigContext{
			Name: c,
			Context: kubeconfigContextSpec{
				Cluster: c,
				User:    kubeconfigActor,
			},
		})
	}

	data, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("marshaling kubeconfig: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating kubeconfig directory %s: %w", dir, err)
	}
	dest := filepath.Join(dir, kubeconfigFilename)
	if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("kubeconfig path %s is a symbolic link", dest)
	}
	if err := os.WriteFile(dest, data, kubeconfigPerm); err != nil {
		return "", fmt.Errorf("writing kubeconfig %s: %w", dest, err)
	}
	return dest, nil
}
