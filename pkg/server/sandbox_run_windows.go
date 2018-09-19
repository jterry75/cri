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

package server

import (
	"github.com/containerd/containerd"
	containerdio "github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/typeurl"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	criconfig "github.com/containerd/cri/pkg/config"
	customopts "github.com/containerd/cri/pkg/containerd/opts"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	"github.com/containerd/cri/pkg/log"
	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
	"github.com/containerd/cri/pkg/util"
)

func init() {
	typeurl.Register(&sandboxstore.Metadata{},
		"github.com/containerd/cri/pkg/store/sandbox", "Metadata")
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
func (c *criService) RunPodSandbox(ctx context.Context, r *runtime.RunPodSandboxRequest) (_ *runtime.RunPodSandboxResponse, retErr error) {
	config := r.GetConfig()

	// Generate unique id and name for the sandbox and reserve the name.
	id := util.GenerateID()
	name := makeSandboxName(config.GetMetadata())
	logrus.Debugf("Generated id %q for sandbox %q", id, name)
	// Reserve the sandbox name to avoid concurrent `RunPodSandbox` request starting the
	// same sandbox.
	if err := c.sandboxNameIndex.Reserve(name, id); err != nil {
		return nil, errors.Wrapf(err, "failed to reserve sandbox name %q", name)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.sandboxNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal sandbox object.
	sandbox := sandboxstore.NewSandbox(
		sandboxstore.Metadata{
			ID:     id,
			Name:   name,
			Config: config,
		},
		sandboxstore.Status{
			State: sandboxstore.StateUnknown,
		},
	)

	// Ensure sandbox container image snapshot.
	imageName := c.getDefaultSandboxImage(config)
	image, err := c.ensureImageExists(ctx, imageName, config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get sandbox image %q", imageName)
	}

	// Setup Networking
	err = c.setupPodNetwork(&sandbox)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to setup networking for sandbox %q", id)
	}

	ociRuntime, err := c.getSandboxRuntime(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox runtime")
	}
	logrus.Debugf("Use OCI %+v for sandbox %q", ociRuntime, id)

	// Create sandbox container.
	spec, err := c.generateSandboxContainerSpec(id, config, &image.ImageSpec.Config, sandbox.NetNSPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate sandbox container spec")
	}

	sandboxLabels := buildLabels(config.Labels, containerKindSandbox)

	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(c.getDefaultSnapshotterForSandbox(config)),
		customopts.WithNewSnapshot(id, image.Image),
		containerd.WithSpec(spec),
		containerd.WithContainerLabels(sandboxLabels),
		containerd.WithContainerExtension(sandboxMetadataExtension, &sandbox.Metadata),
		containerd.WithRuntime(ociRuntime.Type, nil)}

	container, err := c.client.NewContainer(ctx, id, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create containerd container")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			if err := container.Delete(deferCtx, containerd.WithSnapshotCleanup); err != nil {
				logrus.WithError(err).Errorf("Failed to delete containerd container %q", id)
			}
		}
	}()

	// Create sandbox container root directories.
	sandboxRootDir := c.getSandboxRootDir(id)
	if err := c.os.MkdirAll(sandboxRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create sandbox root directory %q",
			sandboxRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox root directory.
			if err := c.os.RemoveAll(sandboxRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove sandbox root directory %q",
					sandboxRootDir)
			}
		}
	}()
	volatileSandboxRootDir := c.getVolatileSandboxRootDir(id)
	if err := c.os.MkdirAll(volatileSandboxRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create volatile sandbox root directory %q",
			volatileSandboxRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the volatile sandbox root directory.
			if err := c.os.RemoveAll(volatileSandboxRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove volatile sandbox root directory %q",
					volatileSandboxRootDir)
			}
		}
	}()

	// Update sandbox created timestamp.
	info, err := container.Info(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox container info")
	}
	if err := sandbox.Status.Update(func(status sandboxstore.Status) (sandboxstore.Status, error) {
		status.CreatedAt = info.CreatedAt
		return status, nil
	}); err != nil {
		return nil, errors.Wrap(err, "failed to update sandbox created timestamp")
	}

	// Add sandbox into sandbox store in UNKNOWN state.
	sandbox.Container = container
	if err := c.sandboxStore.Add(sandbox); err != nil {
		return nil, errors.Wrapf(err, "failed to add sandbox %+v into store", sandbox)
	}
	defer func() {
		// Delete sandbox from sandbox store if there is an error.
		if retErr != nil {
			c.sandboxStore.Delete(id)
		}
	}()
	// NOTE(random-liu): Sandbox state only stay in UNKNOWN state after this point
	// and before the end of this function.
	// * If `Update` succeeds, sandbox state will become READY in one transaction.
	// * If `Update` fails, sandbox will be removed from the store in the defer above.
	// * If containerd stops at any point before `Update` finishes, because sandbox
	// state is not checkpointed, it will be recovered from corresponding containerd task
	// status during restart:
	//   * If the task is running, sandbox state will be READY,
	//   * Or else, sandbox state will be NOTREADY.
	//
	// In any case, sandbox will leave UNKNOWN state, so it's safe to ignore sandbox
	// in UNKNOWN state in other functions.

	// Start sandbox container in one transaction to avoid race condition with
	// event monitor.
	if err := sandbox.Status.Update(func(status sandboxstore.Status) (_ sandboxstore.Status, retErr error) {
		// NOTE(random-liu): We should not change the sandbox state to NOTREADY
		// if `Update` fails.
		//
		// If `Update` fails, the sandbox will be cleaned up by all the defers
		// above. We should not let user see this sandbox, or else they will
		// see the sandbox disappear after the defer clean up, which may confuse
		// them.
		//
		// Given so, we should keep the sandbox in UNKNOWN state if `Update` fails,
		// and ignore sandbox in UNKNOWN state in all the inspection functions.

		// Create sandbox task in containerd.
		log.Tracef("Create sandbox container (id=%q, name=%q).",
			id, name)

		// We don't need stdio for sandbox container.
		task, err := container.NewTask(ctx, containerdio.NullIO)
		if err != nil {
			return status, errors.Wrap(err, "failed to create containerd task")
		}
		defer func() {
			if retErr != nil {
				deferCtx, deferCancel := ctrdutil.DeferContext()
				defer deferCancel()
				// Cleanup the sandbox container if an error is returned.
				// It's possible that task is deleted by event monitor.
				if _, err := task.Delete(deferCtx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
					logrus.WithError(err).Errorf("Failed to delete sandbox container %q", id)
				}
			}
		}()

		if err := task.Start(ctx); err != nil {
			return status, errors.Wrapf(err, "failed to start sandbox container task %q", id)
		}

		// Set the pod sandbox as ready after successfully start sandbox container.
		status.Pid = task.Pid()
		status.State = sandboxstore.StateReady
		return status, nil
	}); err != nil {
		return nil, errors.Wrap(err, "failed to start sandbox container")
	}

	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (c *criService) generateSandboxContainerSpec(id string, config *runtime.PodSandboxConfig,
	imageConfig *imagespec.ImageConfig, nsPath string) (*runtimespec.Spec, error) {
	ctx := ctrdutil.NamespacedContext()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, getDefaultPlatform(config), &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}
	g := newSpecGenerator(spec)

	// Apply default config from image config.
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}

	if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	if len(imageConfig.Entrypoint) == 0 && len(imageConfig.Cmd) == 0 {
		// Pause image must have entrypoint or cmd.
		return nil, errors.Errorf("invalid empty entrypoint and cmd in image config %+v", imageConfig)
	}
	// Set process commands.
	g.SetProcessArgs(append(imageConfig.Entrypoint, imageConfig.Cmd...))

	// Clear the root location since runhcs sets it on the mount path in the
	// guest.
	g.Config.Root = nil

	if getDefaultIsolation(config) == IsolationHyperV {
		// TODO: JTERRY75 - This is a hack. Setting to the empty string will
		// initialize the Windows.HyperV section which is really all we want.
		g.SetWindowsHypervUntilityVMPath("")
	}
	g.SetWindowsNetworkNamespace(nsPath)

	// Set hostname.
	g.SetHostname(config.GetHostname())

	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	g.AddAnnotation(annotations.SandboxID, id)

	return g.Config, nil
}

// getSandboxRuntime returns the runtime configuration for sandbox.
// If the sandbox contains untrusted workload, runtime for untrusted workload will be returned,
// or else default runtime will be returned.
func (c *criService) getSandboxRuntime(config *runtime.PodSandboxConfig) (criconfig.Runtime, error) {
	if untrustedWorkload(config) {
		if c.config.ContainerdConfig.UntrustedWorkloadRuntime.Type == "" {
			return criconfig.Runtime{}, errors.New("no runtime for untrusted workload is configured")
		}
		return c.config.ContainerdConfig.UntrustedWorkloadRuntime, nil
	}
	return c.config.ContainerdConfig.DefaultRuntime, nil
}
