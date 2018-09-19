// +build windows

/*
Copyright 2017 The Kubernetes Authors.

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

package config

import "github.com/containerd/containerd"

// DefaultConfig returns default configurations of cri plugin.
func DefaultConfig() PluginConfig {
	return PluginConfig{
		CniConfig: CniConfig{
			NetworkPluginBinDir:       "C:\\containerd\\data\\cni\\bin",
			NetworkPluginConfDir:      "C:\\containerd\\data\\cni\\config",
			NetworkPluginConfTemplate: "",
		},
		ContainerdConfig: ContainerdConfig{
			Snapshotter: containerd.DefaultSnapshotter,
			DefaultRuntime: Runtime{
				Type:   "io.containerd.runhcs.v1",
				Engine: "",
				Root:   "",
			},
		},
		StreamServerAddress: "127.0.0.1",
		StreamServerPort:    "0",
		EnableTLSStreaming:  false,
		X509KeyPairStreaming: X509KeyPairStreaming{
			TLSKeyFile:  "",
			TLSCertFile: "",
		},
		SandboxImage:            "docker.io/microsoft/nanoserver-insider:latest", // "k8s.gcr.io/pause:3.1",
		StatsCollectPeriod:      10,
		MaxContainerLogLineSize: 16 * 1024,
		Registry: Registry{
			Mirrors: map[string]Mirror{
				"docker.io": {
					Endpoints: []string{"https://registry-1.docker.io"},
				},
			},
		},
	}
}
