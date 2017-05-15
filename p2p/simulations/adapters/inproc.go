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
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/rpc"
)

// SimAdapter is a NodeAdapter which creates in-memory nodes and connects them
// using an in-memory p2p.MsgReadWriter pipe
type SimAdapter struct {
	mtx      sync.RWMutex
	nodes    map[discover.NodeID]*SimNode
	services map[string]ServiceFunc
}

// NewSimAdapter creates a SimAdapter which is capable of running in-memory
// nodes running any of the given services (the service to run on a particular
// node is passed to the NewNode function in the NodeConfig)
func NewSimAdapter(services map[string]ServiceFunc) *SimAdapter {
	return &SimAdapter{
		nodes:    make(map[discover.NodeID]*SimNode),
		services: services,
	}
}

// Name returns the name of the adapter for logging purpoeses
func (s *SimAdapter) Name() string {
	return "sim-adapter"
}

// NewNode returns a new SimNode using the given config
func (s *SimAdapter) NewNode(config *NodeConfig) (Node, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// check a node with the ID doesn't already exist
	id := config.Id
	if _, exists := s.nodes[id.NodeID]; exists {
		return nil, fmt.Errorf("node already exists: %s", id)
	}

	// check the service is valid and initialize it
	serviceFunc, exists := s.services[config.Service]
	if !exists {
		return nil, fmt.Errorf("unknown node service %q", config.Service)
	}

	node := &SimNode{
		Id:          id,
		adapter:     s,
		serviceFunc: serviceFunc,
		peers:       make(map[discover.NodeID]MsgReadWriteCloser),
		dropPeers:   make(chan struct{}),
	}
	s.nodes[id.NodeID] = node
	return node, nil
}

// GetNode returns the node with the given ID if it exists
func (s *SimAdapter) GetNode(id discover.NodeID) (*SimNode, bool) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	node, ok := s.nodes[id]
	return node, ok
}

// MsgReadWriteCloser wraps a MsgReadWriter with the addition of a Close method
// so we can simulate the closing of a p2p connection (which usually happens by
/// closing the underlying TCP connection)
type MsgReadWriteCloser interface {
	p2p.MsgReadWriter
	Close() error
}

// SimNode is an in-memory node which connects to other SimNodes using an
// in-memory p2p.MsgReadWriter pipe, running an underlying service protocol
// directly over that pipe.
//
// It implements the p2p.Server interface so it can be used transparently
// by the underlying service.
type SimNode struct {
	lock        sync.RWMutex
	Id          *NodeId
	adapter     *SimAdapter
	running     node.Service
	serviceFunc ServiceFunc
	peers       map[discover.NodeID]MsgReadWriteCloser
	peerFeed    event.Feed
	client      *rpc.Client
	rpcMux      *rpcMux

	// dropPeers is used to force peer disconnects when
	// the node is stopped
	dropPeers chan struct{}
}

// Addr returns the node's discovery address
func (self *SimNode) Addr() []byte {
	return []byte(self.Node().String())
}

// Node returns a discover.Node representing the SimNode
func (self *SimNode) Node() *discover.Node {
	return discover.NewNode(self.Id.NodeID, nil, 0, 0)
}

// Client returns an rpc.Client which can be used to communicate with the
// underlying service (it is set once the node has started)
func (self *SimNode) Client() (*rpc.Client, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.client == nil {
		return nil, errors.New("RPC not started")
	}
	return self.client, nil
}

// ServeRPC serves RPC requests over the given connection using the node's
// RPC multiplexer
func (self *SimNode) ServeRPC(conn net.Conn) error {
	self.lock.Lock()
	mux := self.rpcMux
	self.lock.Unlock()
	if mux == nil {
		return errors.New("RPC not started")
	}
	mux.Serve(conn)
	return nil
}

// Start initializes the service, starts the RPC handler and then starts
// the service
func (self *SimNode) Start(snapshot []byte) error {
	service := self.serviceFunc(self.Id, snapshot)

	// for simplicity, only support single protocol services (simulating
	// multiple protocols on the same peer is extra effort, and we don't
	// currently run any simulations which run multiple protocols)
	if len(service.Protocols()) != 1 {
		return errors.New("service must have a single protocol")
	}

	self.dropPeers = make(chan struct{})
	if err := self.startRPC(service); err != nil {
		return err
	}
	self.running = service
	return service.Start(&simServer{self})
}

// simServer wraps a SimNode but modifies the Start method signature so that
// it implements the p2p.Server interface (the Start method is never actually
// called when using the SimAdapter)
type simServer struct {
	*SimNode
}

func (s *simServer) Start() error {
	return nil
}

// Stop stops the RPC handler, stops the underlying service and disconnects
// any currently connected peers
func (self *SimNode) Stop() error {
	self.stopRPC()
	close(self.dropPeers)
	return self.running.Stop()
}

// Running returns whether or not the service is running
func (self *SimNode) Running() bool {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.running != nil
}

// Service returns the running node.Service
func (self *SimNode) Service() node.Service {
	return self.running
}

// startRPC starts an RPC server and connects to it using an in-process RPC
// client
func (self *SimNode) startRPC(service node.Service) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.client != nil {
		return errors.New("RPC already started")
	}

	// add SimAdminAPI so that the network can call the
	// AddPeer, RemovePeer and PeerEvents RPC methods
	apis := append(service.APIs(), []rpc.API{
		{
			Namespace: "admin",
			Version:   "1.0",
			Service:   &SimAdminAPI{self},
		},
	}...)

	// start the RPC handler
	handler := rpc.NewServer()
	for _, api := range apis {
		if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
			return fmt.Errorf("error registering RPC: %s", err)
		}
	}

	// create an in-process RPC multiplexer
	pipe1, pipe2 := net.Pipe()
	go handler.ServeCodec(rpc.NewJSONCodec(pipe1), rpc.OptionMethodInvocation|rpc.OptionSubscriptions)
	self.rpcMux = newRPCMux(pipe2)

	// create an in-process RPC client
	self.client = self.rpcMux.Client()

	return nil
}

// stopRPC closes the node's RPC client
func (self *SimNode) stopRPC() {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.client != nil {
		self.client.Close()
		self.client = nil
		self.rpcMux = nil
	}
}

// RemovePeer removes the given node as a peer by looking up the corresponding
// p2p.MsgReadWriter pipe and closing it (which will cause both the local
// and peer Protocol.Run functions to exit)
func (self *SimNode) RemovePeer(peer *discover.Node) {
	self.lock.Lock()
	defer self.lock.Unlock()
	peerRW, exists := self.peers[peer.ID]
	if !exists {
		return
	}
	peerRW.Close()
	delete(self.peers, peer.ID)
	log.Trace(fmt.Sprintf("dropped peer %v", peer.ID))
}

// AddPeer adds the given node as a peer by creating a p2p.MsgReadWriter pipe
// and running both the local and peer's Protocol.Run function over the pipe
func (self *SimNode) AddPeer(peer *discover.Node) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if _, exists := self.peers[peer.ID]; exists {
		return
	}
	peerNode, exists := self.adapter.GetNode(peer.ID)
	if !exists {
		panic(fmt.Sprintf("unknown peer: %s", peer.ID))
	}
	if !peerNode.Running() {
		return
	}
	p1, p2 := p2p.MsgPipe()
	localRW := p2p.NewMsgEventer(p1, &self.peerFeed, peer.ID)
	peerRW := p2p.NewMsgEventer(p2, &self.peerFeed, self.Id.NodeID)
	self.peers[peer.ID] = peerRW
	peerNode.RunProtocol(self, peerRW)
	self.RunProtocol(peerNode, localRW)
}

// SubscribeEvents subscribes the given channel to p2p peer events
func (self *SimNode) SubscribeEvents(ch chan *p2p.PeerEvent) event.Subscription {
	return self.peerFeed.Subscribe(ch)
}

// PeerCount returns the number of currently connected peers
func (self *SimNode) PeerCount() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.peers)
}

// NodeInfo returns information about the node
func (self *SimNode) NodeInfo() *p2p.NodeInfo {
	self.lock.Lock()
	defer self.lock.Unlock()
	info := &p2p.NodeInfo{
		ID:        self.Id.String(),
		Enode:     self.Node().String(),
		Protocols: make(map[string]interface{}),
	}
	if self.running != nil {
		for _, proto := range self.running.Protocols() {
			nodeInfo := interface{}("unknown")
			if query := proto.NodeInfo; query != nil {
				nodeInfo = proto.NodeInfo()
			}
			info.Protocols[proto.Name] = nodeInfo
		}
	}
	return info
}

// PeersInfo is a stub so that SimNode implements p2p.Server
func (self *SimNode) PeersInfo() (info []*p2p.PeerInfo) {
	return nil
}

// Snapshot creates a snapshot of the running service
func (self *SimNode) Snapshot() ([]byte, error) {
	self.lock.Lock()
	service := self.running
	self.lock.Unlock()
	if service == nil {
		return nil, errors.New("service not running")
	}
	return SnapshotAPI{service}.Snapshot()
}

// RunProtocol runs the underlying service's protocol with the peer using the
// given MsgReadWriteCloser, emitting peer add / drop events for peer event
// subscribers
func (self *SimNode) RunProtocol(peer *SimNode, rw MsgReadWriteCloser) {
	// close the rw if the node is stopped to disconnect the peer
	go func() {
		<-self.dropPeers
		log.Trace("dropping peer", "self.id", self.Id, "peer.id", peer.Id)
		rw.Close()
	}()

	id := peer.Id
	log.Trace(fmt.Sprintf("protocol starting on peer %v (connection with %v)", self.Id, id))
	protocol := self.running.Protocols()[0]
	p := p2p.NewPeer(id.NodeID, id.Label(), []p2p.Cap{})
	go func() {
		// emit peer add event
		self.peerFeed.Send(&p2p.PeerEvent{
			Type: p2p.PeerEventTypeAdd,
			Peer: id.NodeID,
		})

		// run the protocol
		err := protocol.Run(p, rw)

		// remove the peer
		self.RemovePeer(peer.Node())
		log.Trace(fmt.Sprintf("protocol quit on peer %v (connection with %v broken: %v)", self.Id, id, err))

		// emit peer drop event
		self.peerFeed.Send(&p2p.PeerEvent{
			Type:  p2p.PeerEventTypeDrop,
			Peer:  id.NodeID,
			Error: err.Error(),
		})
	}()
}

// SimAdminAPI implements the AddPeer and RemovePeer RPC methods (API
// compatible with node.PrivateAdminAPI)
type SimAdminAPI struct {
	*SimNode
}

func (api *SimAdminAPI) AddPeer(url string) (bool, error) {
	node, err := discover.ParseNode(url)
	if err != nil {
		return false, fmt.Errorf("invalid enode: %v", err)
	}
	api.SimNode.AddPeer(node)
	return true, nil
}

func (api *SimAdminAPI) RemovePeer(url string) (bool, error) {
	node, err := discover.ParseNode(url)
	if err != nil {
		return false, fmt.Errorf("invalid enode: %v", err)
	}
	api.SimNode.RemovePeer(node)
	return true, nil
}

// PeerEvents creates an RPC subscription which receives peer events from the
// underlying p2p.Server
func (api *SimAdminAPI) PeerEvents(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	go func() {
		events := make(chan *p2p.PeerEvent)
		sub := api.SubscribeEvents(events)
		defer sub.Unsubscribe()

		for {
			select {
			case event := <-events:
				notifier.Notify(rpcSub.ID, event)
			case <-sub.Err():
				return
			case <-rpcSub.Err():
				return
			case <-notifier.Closed():
				return
			}
		}
	}()

	return rpcSub, nil
}