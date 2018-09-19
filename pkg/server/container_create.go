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

package server

import (
	"strings"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

// setOCIProcessArgs sets process args. It returns error if the final arg list
// is empty.
func setOCIProcessArgs(g *generate.Generator, config *runtime.ContainerConfig, imageConfig *imagespec.ImageConfig) error {
	command, args := config.GetCommand(), config.GetArgs()
	// The following logic is migrated from https://github.com/moby/moby/blob/master/daemon/commit.go
	// TODO(random-liu): Clearly define the commands overwrite behavior.
	if len(command) == 0 {
		// Copy array to avoid data race.
		if len(args) == 0 {
			args = append([]string{}, imageConfig.Cmd...)
		}
		if command == nil {
			command = append([]string{}, imageConfig.Entrypoint...)
		}
	}
	if len(command) == 0 && len(args) == 0 {
		return errors.New("no command specified")
	}
	g.SetProcessArgs(append(command, args...))
	return nil
}

// addImageEnvs adds environment variables from image config. It returns error if
// an invalid environment variable is encountered.
func addImageEnvs(g *generate.Generator, imageEnvs []string) error {
	for _, e := range imageEnvs {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) != 2 {
			return errors.Errorf("invalid environment variable %q", e)
		}
		g.AddProcessEnv(kv[0], kv[1])
	}
	return nil
}
