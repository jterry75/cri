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
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	winio "github.com/Microsoft/go-winio"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

const binaryIOProcStartTimeout = 10 * time.Second
const binaryIOProcTermTimeout = 10 * time.Second // Give logger process solid 10 seconds for cleanup

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

func (dc *delayedConnection) unblockConnectionWaiters() {
	defer dc.once.Do(func() {
		dc.wg.Done()
	})
}

func (dc *delayedConnection) Close() error {
	dc.l.Close()
	if dc.con != nil {
		return dc.con.Close()
	}
	dc.unblockConnectionWaiters()
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
			dc.unblockConnectionWaiters()
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
			dc.unblockConnectionWaiters()
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
			dc.unblockConnectionWaiters()
		}()
	}

	return p, &wgCloser{
		wg:     &sync.WaitGroup{},
		set:    set,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

type binaryCloser struct {
	cmd            *exec.Cmd
	signalFileName string
}

func (this *binaryCloser) Close() error {
	if this.cmd == nil || this.cmd.Process == nil {
		return nil
	}

	os.Remove(this.signalFileName)

	done := make(chan error, 1)
	defer close(done)

	go func() {
		done <- this.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(binaryIOProcTermTimeout):
		log.L.Warn("failed to wait for customer logger process to exit, killing")

		err := this.cmd.Process.Kill()
		if err != nil {
			return errors.Wrap(err, "failed to kill customer logger process")
		}

		return nil
	}
}

func newBinaryLogger(id string, fifos *cio.FIFOSet, binaryPath string, labels map[string]string) (_ *wgCloser, err error) {
	var (
		set         []io.Closer
		ctx, cancel = context.WithCancel(context.Background())
	)

	started := make(chan bool)
	defer close(started)

	defer func() {
		if err != nil {
			for _, f := range set {
				f.Close()
			}
			cancel()
		}
	}()

	signalFileName, err := getSignalFileName(id)
	if err != nil {
		log.L.WithError(err).Errorf("failed to create tempory signal file %s", signalFileName)
		return nil, err
	}

	labelData, err := json.Marshal(labels)
	if err != nil {
		log.L.WithError(err).Errorf("failed to serialize labels")
		return
	}

	labelStr := string(labelData)
	cmd := exec.Command(binaryPath, fifos.Stdout, fifos.Stderr, signalFileName, labelStr)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		for start := time.Now(); time.Now().Sub(start) < binaryIOProcStartTimeout; {
			if _, err := os.Stat(signalFileName); os.IsNotExist(err) {
				time.Sleep(time.Second / 2)
			} else {
				started <- true
				return
			}
		}
		started <- false
	}()

	// Wait until the logger started
	if !<-started {
		log.L.WithError(err).Errorf("failed to create signal file %s", signalFileName)
		return nil, err
	}

	set = append(set, &binaryCloser{
		cmd:            cmd,
		signalFileName: signalFileName,
	})

	return &wgCloser{
		wg:     &sync.WaitGroup{},
		set:    set,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func getSignalFileName(id string) (string, error) {
	tempdir, err := ioutil.TempDir("", id)
	return fmt.Sprintf("%s\\logsignal-%s", tempdir, id), err
}
