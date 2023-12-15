// Copyright 2023 The go-ethereum Authors
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

package request

type Request interface {
	Overlaps(Request) bool
}

type RequestId uint64

type ServerAndId struct {
	Server Server
	Id     RequestId
}

type AnsweredRequest struct {
	ServerAndId
	Request Request
	Reply   any
}

// one per sync process
type RequestTracker struct {
	servers            serverSet // one per trigger
	noTimeout, timeout map[ServerAndId]Request
}

func (p *RequestTracker) TryRequest(request Request) (ServerAndId, bool) {
	if !p.canSend(request) {
		return ServerAndId{}, false
	}
	id, ok := p.servers.TryRequest(request)
	if ok {
		p.noTimeout[id] = request
	}
	return id, ok
}

func (p *RequestTracker) softTimeout(id ServerAndId) {
	req, ok := p.noTimeout[id]
	if !ok {
		return
	}
	delete(p.noTimeout, id)
	p.timeout[id] = req
}

func (p *RequestTracker) removePending(id ServerAndId) {
	delete(p.noTimeout, id)
	delete(p.timeout, id)
}

func (p *RequestTracker) canSend(req Request) bool {
	for _, r := range p.noTimeout {
		if req.Overlaps(r) {
			return false
		}
	}
	return true
}

func (p *RequestTracker) getPending(id ServerAndId) Request {
	if req, ok := p.noTimeout[id]; ok {
		return req
	}
	return p.timeout[id]
}

func (p *RequestTracker) ProcessEvents(events []ServerEvent) []AnsweredRequest {
	var answered []AnsweredRequest
	for _, e := range events {
		id := ServerAndId{Server: e.Server, Id: e.Event.ReqId}
		switch e.Event.Type {
		case EvValidResponse:
			if request := p.getPending(id); request != nil {
				p.removePending(id)
				answered = append(answered, AnsweredRequest{
					ServerAndId: id,
					Request:     request,
					Reply:       e.Event.Data,
				})
			}
		case EvInvalidResponse, EvHardTimeout:
			p.removePending(id)
		case EvSoftTimeout:
			p.softTimeout(id)
		}
	}
	return answered
}
