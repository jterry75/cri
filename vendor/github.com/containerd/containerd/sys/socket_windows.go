// +build windows

/*
   Copyright The containerd Authors.

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

package sys

import (
	"net"
	"net/url"
	"strings"

	"github.com/Microsoft/go-winio"
	"github.com/pkg/errors"
)

// GetLocalListener returns a Listener out of a named pipe or tcp socket.
// `path` must be of the form of `\\.\pipe\<pipename>` or tcp://<address>:port.
// (see https://msdn.microsoft.com/en-us/library/windows/desktop/aa365150)
func GetLocalListener(path string, uid, gid int) (net.Listener, error) {
	if strings.HasPrefix(path, "\\\\") {
		return winio.ListenPipe(path, nil)
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "tcp":
		return net.Listen("tcp", u.Host)
	}
	return nil, errors.Errorf("unsupported protocol '%s'", u.Scheme)
}
