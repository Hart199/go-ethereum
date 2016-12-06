// Copyright 2016 The go-ethereum Authors
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

package adapters

import (
	"fmt"
	"net"

	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
)

// devp2p RLPx underlay support

type RLPx struct {
	net  *p2p.Server
	addr []byte
	m    Messenger
}

type RLPxMessenger struct{}

func NewRLPx(addr []byte, srv *p2p.Server, m Messenger, cluster bool) {
	return &RLPx{
		net:     srv,
		addr:    addr,
		m:       m,
		cluster: bool,
	}
}

func (*RLPxMessenger) SendMsg(w p2p.MsgWriter, code uint64, msg interface{}) error {
	return p2p.Send(w, code, msg)
}

func (*RLPxMessenger) ReadMsg(r p2p.MsgReader) (p2p.Msg, error) {
	return r.ReadMsg()
}

func (*RLPxMessenger) NewPipe() (p2p.MsgReadWriter, p2p.MsgReadWriter) {
	return nil, nil
}

func (*RLPxMessenger) Close(rw p2p.MsgReadWriter) {}

func (self *RLPx) LocalAddr() []byte {
	return self.addr
}

func (self *RLPx) Connect(enode []byte) error {
	// TCP/UDP node address encoded with enode url scheme
	// <node-id>@<ip-address>:<tcp-port>(?udp=<udp-port>)
	node, err := discover.ParseNode(string(enode))
	if err != nil {
		return fmt.Errorf("invalid node URL: %v", err)
	}
	self.net.AddPeer(node)
	return nil
}

func (self *RLPx) Messenger() Messenger {
	return self.m
}

func (self *RLPx) RunProtocol(p *p2p.Peer, rw p2p.MsgReadWriter) error {
	return nil
}

func (self *RLPx) Disconnect(p *p2p.Peer, rw p2p.MsgReadWriter) error {
	p.Disconnect(p2p.DiscSubprotocolError)
	return self.network.DidDisconnect(self.Id, id)
}

// ParseAddr take two arguments, advertised in handshake and the one set on the peer struct
// and constructs the remote address object
func (self *RLPx) ParseAddr(s []byte, remoteAddr string) ([]byte, error) {

	// returns self advertised node connection info (listening address w enodes)
	// IP will get repaired on the other end if missing
	// or resolved via ID by discovery at dialout
	n, err := discover.ParseNode(string(s))
	if err != nil {
		return nil, err
	}

	// repair reported address if IP missing
	if n.IP.IsUnspecified() {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			return nil, err
		}
		n.IP = net.ParseIP(host)
	}
	return []byte(n.String()), nil
}
