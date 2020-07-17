// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package server

// ConnectionStatus represents the connection status of clients.
type ConnectionStatus int

const (
	Connected ConnectionStatus = iota
	Active
	Inactive
	Disconnected
)

// ClientStatus is a wrapper of connection status with a few additional fields
type ClientStatus struct {
	status ConnectionStatus

	// Additional "immutable" fields. These fields should be
	// set when the structure is created.
	ipaddr string
}

func NewClientStatus(status ConnectionStatus, ipaddr string) *ClientStatus {
	return &ClientStatus{
		status: status,
		ipaddr: ipaddr,
	}
}

// WithStatus updates the client status.
func (s *ClientStatus) WithStatus(status ConnectionStatus) *ClientStatus {
	return &ClientStatus{
		status: status,
		ipaddr: s.ipaddr,
	}
}

// IsStatus returns an indicator whether the status is matched.
func (s *ClientStatus) IsStatus(status ConnectionStatus) bool {
	return s.status == status
}
