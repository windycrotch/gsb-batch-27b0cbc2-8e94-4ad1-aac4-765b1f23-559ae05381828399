/* Copyright (c) 2016-2017 Gregor Riepl
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package streaming

import (
	"fmt"
	"sync"
)

// AccessController implements a connection broker that limits
// the maximum number of concurrent connections.
type AccessController struct {
	// maxconnections is a global limit on the number of connections.
	maxconnections uint
	// lock to protect the connection counter
	lock sync.Mutex
	// connections contains the number of active connections.
	// Must only be accessed while holding lock.
	connections uint
	// inhibit is a global connection inhibitor flag.
	// Must only be accessed while holding lock.
	inhibit bool
}

// NewAccessController creates a connection broker object that
// handles access control according to the number of connected clients.
func NewAccessController(maxconnections uint) *AccessController {
	return &AccessController{
		maxconnections: maxconnections,
	}
}

// SetInhibit allows setting and clearing the inhibit flag.
// If it is set, no further connections are accepted, irrespective of the
// maxconnections limit.
func (control *AccessController) SetInhibit(inhibit bool) {
	// protect concurrent access
	control.lock.Lock()
	control.inhibit = inhibit
	control.lock.Unlock()
}

// Connections returns the current number of active connections.
func (control *AccessController) Connections() uint {
	control.lock.Lock()
	connections := control.connections
	control.lock.Unlock()
	return connections
}

// Accept accepts an incoming connection when the maximum number of open connections
// has not been reached yet.
func (control *AccessController) Accept(remoteaddr string, streamer *Streamer) bool {
	accept := false
	var connections uint
	// protect concurrent access
	control.lock.Lock()
	// check if the limit is disabled or unreached, and no inhibit is set
	if !control.inhibit && (control.maxconnections == 0 || control.connections < control.maxconnections) {
		// and increase the counter
		control.connections++
		accept = true
	}
	// take a consistent snapshot for logging while the lock is still held
	connections = control.connections
	control.lock.Unlock()
	// print some info
	if accept {
		logger.Logkv(
			"event", eventAclAccepted,
			"remote", remoteaddr,
			"connections", connections,
			"max", control.maxconnections,
			"message", fmt.Sprintf("Accepted connection from %s, active=%d, max=%d", remoteaddr, connections, control.maxconnections),
		)
	} else {
		logger.Logkv(
			"event", eventAclDenied,
			"remote", remoteaddr,
			"connections", connections,
			"max", control.maxconnections,
			"message", fmt.Sprintf("Denied connection from %s, active=%d, max=%d", remoteaddr, connections, control.maxconnections),
		)
	}
	// return the result
	return accept
}

// Release decrements the open connections count.
//
// Each accepted connection must be released exactly once. Release is
// idempotent with respect to the counter floor: it never drops below zero,
// and it reports whether a connection was actually released.
func (control *AccessController) Release(streamer *Streamer) {
	remove := false
	var connections uint
	// protect concurrent access
	control.lock.Lock()
	if control.connections > 0 {
		// and decrease the counter
		control.connections--
		remove = true
	}
	// take a consistent snapshot for logging while the lock is still held
	connections = control.connections
	control.lock.Unlock()
	if remove {
		logger.Logkv(
			"event", eventAclRemoved,
			"connections", connections,
			"max", control.maxconnections,
			"message", fmt.Sprintf("Removed connection, active=%d, max=%d", connections, control.maxconnections),
		)
	} else {
		logger.Logkv(
			"event", eventAclError,
			"error", errorAclNoConnection,
			"message", fmt.Sprintf("Error, no connection to remove"),
		)
	}
}
