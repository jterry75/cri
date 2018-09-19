// +build !windows

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

package io

import (
	"io"
	"sync"
	"syscall"

	"github.com/containerd/containerd/cio"
	"github.com/containerd/fifo"
	"golang.org/x/net/context"
)

// newStdioPipes creates actual fifos for stdio.
func newStdioPipes(fifos *cio.FIFOSet) (_ *stdioPipes, _ *wgCloser, err error) {
	var (
		f           io.ReadWriteCloser
		set         []io.Closer
		ctx, cancel = context.WithCancel(context.Background())
		p           = &stdioPipes{}
	)
	defer func() {
		if err != nil {
			for _, f := range set {
				f.Close()
			}
			cancel()
		}
	}()

	if fifos.Stdin != "" {
		if f, err = fifo.OpenFifo(ctx, fifos.Stdin, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); err != nil {
			return nil, nil, err
		}
		p.stdin = f
		set = append(set, f)
	}

	if f, err = fifo.OpenFifo(ctx, fifos.Stdout, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); err != nil {
		return nil, nil, err
	}
	p.stdout = f
	set = append(set, f)

	if f, err = fifo.OpenFifo(ctx, fifos.Stderr, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700); err != nil {
		return nil, nil, err
	}
	p.stderr = f
	set = append(set, f)

	return p, &wgCloser{
		wg:     &sync.WaitGroup{},
		set:    set,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}
