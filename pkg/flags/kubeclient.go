// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: The Kubernetes Authors
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

/*
 * Copyright The Kubernetes Authors
 * Modifications Copyright 2026 Tenstorrent USA, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package flags

import (
	"fmt"

	"github.com/urfave/cli/v2"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeClientConfig groups the CLI flags that control how the driver talks to
// the Kubernetes API server.
type KubeClientConfig struct {
	KubeConfig   string
	KubeAPIQPS   float64
	KubeAPIBurst int
}

// ClientSets bundles the typed clients exposed to the rest of the driver.
type ClientSets struct {
	Core coreclientset.Interface
}

// Flags returns the CLI flags backing the Kubernetes client configuration.
func (k *KubeClientConfig) Flags() []cli.Flag {
	flags := []cli.Flag{
		&cli.StringFlag{
			Category:    "Kubernetes client:",
			Name:        "kubeconfig",
			Usage:       "Absolute path to the `KUBECONFIG` file. Either this flag or the KUBECONFIG env variable need to be set if the driver is being run out of cluster.",
			Destination: &k.KubeConfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.Float64Flag{
			Category:    "Kubernetes client:",
			Name:        "kube-api-qps",
			Usage:       "`QPS` to use while communicating with the Kubernetes apiserver.",
			Value:       5,
			Destination: &k.KubeAPIQPS,
			EnvVars:     []string{"KUBE_API_QPS"},
		},
		&cli.IntFlag{
			Category:    "Kubernetes client:",
			Name:        "kube-api-burst",
			Usage:       "`Burst` to use while communicating with the Kubernetes apiserver.",
			Value:       10,
			Destination: &k.KubeAPIBurst,
			EnvVars:     []string{"KUBE_API_BURST"},
		},
	}

	return flags
}

// NewClientSetConfig builds the rest.Config used by the typed clients,
// preferring in-cluster configuration when KUBECONFIG is not provided.
func (k *KubeClientConfig) NewClientSetConfig() (*rest.Config, error) {
	var csconfig *rest.Config

	var err error
	if k.KubeConfig == "" {
		csconfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("create in-cluster client configuration: %v", err)
		}
	} else {
		csconfig, err = clientcmd.BuildConfigFromFlags("", k.KubeConfig)
		if err != nil {
			return nil, fmt.Errorf("create out-of-cluster client configuration: %v", err)
		}
	}

	csconfig.QPS = float32(k.KubeAPIQPS)
	csconfig.Burst = k.KubeAPIBurst

	return csconfig, nil
}

// NewClientSets builds the typed clients used by the driver.
func (k *KubeClientConfig) NewClientSets() (ClientSets, error) {
	csconfig, err := k.NewClientSetConfig()
	if err != nil {
		return ClientSets{}, fmt.Errorf("create client configuration: %v", err)
	}

	coreclient, err := coreclientset.NewForConfig(csconfig)
	if err != nil {
		return ClientSets{}, fmt.Errorf("create core client: %v", err)
	}

	return ClientSets{
		Core: coreclient,
	}, nil
}
