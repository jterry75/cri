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
	"syscall"
)

const (
	// SysKillSignal is the Windows representation of unix.SIGKILL
	SysKillSignal = syscall.Signal(0x9)
	// SysTermSignal is the Windows representation of unix.SIGTERM
	SysTermSignal = syscall.Signal(0xf)

	// SysMS_NOEXEC is the Windows representation of unix.MS_NOEXEC
	SysMS_NOEXEC = 0x8
	// SysMS_NOSUID is the Windows representation of unix.MS_NOSUID
	SysMS_NOSUID = 0x2
	// SysMS_NODEV is the Windows representation of unix.MS_NODEV
	SysMS_NODEV = 0x4
)
