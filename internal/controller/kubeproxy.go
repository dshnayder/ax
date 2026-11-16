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

package controller

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

const defaultKubeProxyJWTSeconds int64 = 900

// ResolveKubernetesAccess fills defaults on k and builds the Agent Substrate HTTPS egress rule
// that authorizes traffic to k8s-api-proxy and injects the Actor JWT. It returns (nil, nil, nil)
// when k is nil.
func ResolveKubernetesAccess(k *v1alpha1.KubernetesAccess, defaultEndpoint string) (*v1alpha1.KubernetesAccess, *ateapipb.EgressRule, error) {
	if k == nil {
		return nil, nil, nil
	}
	endpoint := strings.TrimRight(strings.TrimSpace(k.GetEndpoint()), "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(strings.TrimSpace(defaultEndpoint), "/")
	}
	if endpoint == "" {
		endpoint = v1alpha1.DefaultKubeProxyEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, nil, fmt.Errorf("invalid k8s-api-proxy endpoint %q", endpoint)
	}

	host := strings.ToLower(u.Hostname())
	hostnames := []string{host}
	// Pod search paths expand <svc>.<ns>.svc to <svc>.<ns>.svc.cluster.local.
	if strings.HasSuffix(host, ".svc") && strings.Count(host, ".") == 2 {
		hostnames = append(hostnames, host+".cluster.local")
	}

	var ports *ateapipb.Ports
	if p := u.Port(); p != "" && p != "443" {
		num, err := strconv.Atoi(p)
		if err != nil || num < 1 || num > 65535 {
			return nil, nil, fmt.Errorf("invalid k8s-api-proxy port %q", p)
		}
		ports = &ateapipb.Ports{Numbers: []int32{int32(num)}}
	}

	audience := strings.TrimSpace(k.GetAudience())
	if audience == "" {
		audience = endpoint
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

	resolved := &v1alpha1.KubernetesAccess{
		Clusters:       clusters,
		DefaultCluster: defaultCluster,
		Endpoint:       endpoint,
		Audience:       audience,
	}
	rule := &ateapipb.EgressRule{
		Https: &ateapipb.HttpsRule{
			Hostnames: hostnames,
			Ports:     ports,
			Effects: &ateapipb.HttpRuleEffects{
				ReplaceHeaders: []*ateapipb.CredentialHeader{{
					Header: "Authorization",
					Prefix: "Bearer ",
					ActorJwt: &ateapipb.ActorJwtSource{
						Audiences:         []string{audience},
						ExpirationSeconds: defaultKubeProxyJWTSeconds,
					},
				}},
			},
		},
	}
	return resolved, rule, nil
}

// ExpandGatewayEgress returns the full set of Substrate egress rules for gw, combining explicit
// spec.egress rules with any rule expanded from spec.kubernetes.
func ExpandGatewayEgress(gw *v1alpha1.Gateway, defaultEndpoint string) ([]*ateapipb.EgressRule, error) {
	if gw == nil {
		return nil, nil
	}
	rules := make([]*ateapipb.EgressRule, 0, len(gw.GetSpec().GetEgress())+1)
	for _, r := range gw.GetSpec().GetEgress() {
		rules = append(rules, proto.Clone(r).(*ateapipb.EgressRule))
	}
	_, k8sRule, err := ResolveKubernetesAccess(gw.GetSpec().GetKubernetes(), defaultEndpoint)
	if err != nil {
		return nil, err
	}
	if k8sRule != nil {
		rules = append(rules, k8sRule)
	}
	return rules, nil
}
