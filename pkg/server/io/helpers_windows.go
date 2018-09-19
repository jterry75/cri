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

package io

import (
	"io"
	"net"
	"sync"

	winio "github.com/Microsoft/go-winio"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

type delayedConnection struct {
	l    net.Listener
	con  net.Conn
	wg   sync.WaitGroup
	once sync.Once
}

func (dc *delayedConnection) Write(p []byte) (int, error) {
	dc.wg.Wait()
	if dc.con != nil {
		return dc.con.Write(p)
	}
	return 0, errors.New("use of closed network connection")
}

func (dc *delayedConnection) Read(p []byte) (int, error) {
	dc.wg.Wait()
	if dc.con != nil {
		return dc.con.Read(p)
	}
	return 0, errors.New("use of closed network connection")
}

func (dc *delayedConnection) Close() error {
	defer dc.once.Do(func() {
		dc.wg.Done()
	})
	dc.l.Close()
	if dc.con != nil {
		return dc.con.Close()
	}
	return nil
}

// newStdioPipes creates actual fifos for stdio.
func newStdioPipes(fifos *cio.FIFOSet) (_ *stdioPipes, _ *wgCloser, err error) {
	var (
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
		l, err := winio.ListenPipe(fifos.Stdin, nil)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to create stdin pipe %s", fifos.Stdin)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if err != nil {
				dc.Close()
			}
		}()
		set = append(set, l)
		p.stdin = dc

		go func() {
			c, err := l.Accept()
			if err != nil {
				dc.Close()
				log.L.WithError(err).Errorf("failed to accept stdin connection on %s", fifos.Stdin)
				return
			}
			dc.con = c
		}()
	}

	if fifos.Stdout != "" {
		l, err := winio.ListenPipe(fifos.Stdout, nil)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to create stdout pipe %s", fifos.Stdout)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if err != nil {
				dc.Close()
			}
		}()
		set = append(set, l)
		p.stdout = dc

		go func() {
			c, err := l.Accept()
			if err != nil {
				dc.Close()
				log.L.WithError(err).Errorf("failed to accept stdout connection on %s", fifos.Stdout)
				return
			}
			dc.con = c
		}()
	}

	if fifos.Stderr != "" {
		l, err := winio.ListenPipe(fifos.Stderr, nil)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to create stderr pipe %s", fifos.Stderr)
		}
		dc := &delayedConnection{
			l: l,
		}
		dc.wg.Add(1)
		defer func() {
			if err != nil {
				dc.Close()
			}
		}()
		set = append(set, l)
		p.stderr = dc

		go func() {
			c, err := l.Accept()
			if err != nil {
				dc.Close()
				log.L.WithError(err).Errorf("failed to accept stderr connection on %s", fifos.Stderr)
				return
			}
			dc.con = c
		}()
	}

	return p, &wgCloser{
		wg:     &sync.WaitGroup{},
		set:    set,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}
