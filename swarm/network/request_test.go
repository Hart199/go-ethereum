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

package network

import (
	"context"
	crand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/protocols"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	p2ptest "github.com/ethereum/go-ethereum/p2p/testing"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/swarm/storage"
)

func TestStreamerRetrieveRequest(t *testing.T) {
	// TODO: we only need streamer
	tester, streamer, _, teardown, err := newStreamerTester(t)
	defer teardown()
	if err != nil {
		t.Fatal(err)
	}

	peerID := tester.IDs[0]

	streamer.delivery.RequestFromPeers(hash0[:], true)

	err = tester.TestExchanges(p2ptest.Exchange{
		Label: "RetrieveRequestMsg",
		Expects: []p2ptest.Expect{
			p2ptest.Expect{
				Code: 5,
				Msg: &RetrieveRequestMsg{
					Key:       hash0[:],
					SkipCheck: true,
				},
				Peer: peerID,
			},
		},
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
}

func TestStreamerUpstreamRetrieveRequestMsgExchangeWithoutStore(t *testing.T) {
	// TODO: we only need streamer
	tester, streamer, _, teardown, err := newStreamerTester(t)
	defer teardown()
	if err != nil {
		t.Fatal(err)
	}

	peerID := tester.IDs[0]

	chunk := storage.NewChunk(storage.Key(hash0[:]), nil)

	peer := streamer.getPeer(peerID)

	peer.handleSubscribeMsg(&SubscribeMsg{
		Stream:   retrieveRequestStream,
		Key:      nil,
		From:     0,
		To:       0,
		Priority: Top,
	})

	err = tester.TestExchanges(p2ptest.Exchange{
		Label: "RetrieveRequestMsg",
		Triggers: []p2ptest.Trigger{
			p2ptest.Trigger{
				Code: 5,
				Msg: &RetrieveRequestMsg{
					Key: chunk.Key[:],
				},
				Peer: peerID,
			},
		},
		Expects: []p2ptest.Expect{
			p2ptest.Expect{
				Code: 1,
				Msg: &OfferedHashesMsg{
					HandoverProof: nil,
					Hashes:        nil,
					From:          0,
					To:            0,
				},
				Peer: peerID,
			},
		},
	})

	expectedError := "exchange 0: 'RetrieveRequestMsg' timed out"
	if err == nil || err.Error() != expectedError {
		t.Fatalf("Expected error %v, got %v", expectedError, err)
	}
}

// upstream request server receives a retrieve Request and responds with
// offered hashes or delivery if skipHash is set to true
func TestStreamerUpstreamRetrieveRequestMsgExchange(t *testing.T) {
	// TODO: we only need streamer
	tester, streamer, localStore, teardown, err := newStreamerTester(t)
	defer teardown()
	if err != nil {
		t.Fatal(err)
	}

	peerID := tester.IDs[0]
	peer := streamer.getPeer(peerID)

	peer.handleSubscribeMsg(&SubscribeMsg{
		Stream:   retrieveRequestStream,
		Key:      nil,
		From:     0,
		To:       0,
		Priority: Top,
	})

	hash := storage.Key(hash0[:])
	chunk := storage.NewChunk(hash, nil)
	chunk.SData = hash
	localStore.Put(chunk)
	chunk.WaitToStore()

	err = tester.TestExchanges(p2ptest.Exchange{
		Label: "RetrieveRequestMsg",
		Triggers: []p2ptest.Trigger{
			p2ptest.Trigger{
				Code: 5,
				Msg: &RetrieveRequestMsg{
					Key: hash,
				},
				Peer: peerID,
			},
		},
		Expects: []p2ptest.Expect{
			p2ptest.Expect{
				Code: 1,
				Msg: &OfferedHashesMsg{
					HandoverProof: nil,
					Hashes:        hash,
					From:          0,
					// TODO: why is this 32???
					To:     32,
					Key:    []byte{},
					Stream: retrieveRequestStream,
				},
				Peer: peerID,
			},
		},
	})

	if err != nil {
		t.Fatal(err)
	}

	hash = storage.Key(hash1[:])
	chunk = storage.NewChunk(hash, nil)
	chunk.SData = hash1[:]
	localStore.Put(chunk)
	chunk.WaitToStore()

	err = tester.TestExchanges(p2ptest.Exchange{
		Label: "RetrieveRequestMsg",
		Triggers: []p2ptest.Trigger{
			p2ptest.Trigger{
				Code: 5,
				Msg: &RetrieveRequestMsg{
					Key:       hash,
					SkipCheck: true,
				},
				Peer: peerID,
			},
		},
		Expects: []p2ptest.Expect{
			p2ptest.Expect{
				Code: 6,
				Msg: &ChunkDeliveryMsg{
					Key:   hash,
					SData: hash,
				},
				Peer: peerID,
			},
		},
	})

	if err != nil {
		t.Fatal(err)
	}
}

// serviceName is used with the exec adapter so the exec'd binary knows which
// service to execute
const serviceName = "delivery"

var services = adapters.Services{
	serviceName: newService,
}

var (
	adapter  = flag.String("adapter", "sim", "type of simulation: sim|socket|exec|docker")
	loglevel = flag.Int("loglevel", 5, "verbosity of logs")
)

type roundRobinStore struct {
	index  uint32
	stores []storage.ChunkStore
}

func newRoundRobinStore(stores ...storage.ChunkStore) *roundRobinStore {
	return &roundRobinStore{
		stores: stores,
	}
}

func (rrs *roundRobinStore) Get(key storage.Key) (*storage.Chunk, error) {
	return nil, errors.New("get not well defined on round robin store")
}

func (rrs *roundRobinStore) Put(chunk *storage.Chunk) {
	i := atomic.AddUint32(&rrs.index, 1)
	idx := int(i) % len(rrs.stores)
	log.Trace(fmt.Sprintf("put %v into localstore %v", chunk.Key, idx))
	rrs.stores[idx].Put(chunk)
}

func (rrs *roundRobinStore) Close() {
	for _, store := range rrs.stores {
		store.Close()
	}
}

func init() {
	flag.Parse()
	// register the Delivery service which will run as a devp2p
	// protocol when using the exec adapter
	adapters.RegisterServices(services)

	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*loglevel), log.StreamHandler(os.Stderr, log.TerminalFormat(false))))
}

func testSimulation(t *testing.T, simf func(adapters.NodeAdapter) (*simulations.StepResult, error)) {
	var err error
	var result *simulations.StepResult
	startedAt := time.Now()

	switch *adapter {
	case "sim":
		t.Logf("simadapter")
		result, err = simf(adapters.NewSimAdapter(services))
	case "socket":
		result, err = simf(adapters.NewSocketAdapter(services))
	case "exec":
		baseDir, err0 := ioutil.TempDir("", "swarm-test")
		if err0 != nil {
			t.Fatal(err0)
		}
		defer os.RemoveAll(baseDir)
		result, err = simf(adapters.NewExecAdapter(baseDir))
	case "docker":
		adapter, err0 := adapters.NewDockerAdapter()
		if err0 != nil {
			t.Fatal(err0)
		}
		result, err = simf(adapter)
	default:
		t.Fatal("adapter needs to be one of sim, socket, exec, docker")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Simulation with %d nodes passed in %s", len(result.Passes), result.FinishedAt.Sub(result.StartedAt))
	var min, max time.Duration
	var sum int
	for _, pass := range result.Passes {
		duration := pass.Sub(result.StartedAt)
		if sum == 0 || duration < min {
			min = duration
		}
		if duration > max {
			max = duration
		}
		sum += int(duration.Nanoseconds())
	}
	t.Logf("Min: %s, Max: %s, Average: %s", min, max, time.Duration(sum/len(result.Passes))*time.Nanosecond)
	finishedAt := time.Now()
	t.Logf("Setup: %s, shutdown: %s", result.StartedAt.Sub(startedAt), finishedAt.Sub(result.FinishedAt))
}

func TestDeliveryFromNodes(t *testing.T) {
	testSimulation(t, testDeliveryFromNodes)
}

var (
	delivery    *Delivery
	localStores []storage.ChunkStore
	fileHash    storage.Key
	nodeCount   int
)

func setLocalStores(n int) (func(), error) {
	var datadirs []string
	localStores = make([]storage.ChunkStore, n)
	var err error
	for i := 0; i < n; i++ {
		// TODO: remove temp datadir after test
		var datadir string
		datadir, err = ioutil.TempDir("", "streamer")
		if err != nil {
			break
		}
		var localStore *storage.LocalStore
		localStore, err = storage.NewTestLocalStore(datadir)
		if err != nil {
			break
		}
		datadirs = append(datadirs, datadir)
		localStores[i] = localStore
	}
	teardown := func() {
		for _, datadir := range datadirs {
			os.RemoveAll(datadir)
		}
	}
	return teardown, err
}

func mustReadAll(dpa *storage.DPA, hash storage.Key) (int, error) {
	r := dpa.Retrieve(fileHash)
	buf := make([]byte, 1024)
	var n, total int
	var err error
	for (total == 0 || n > 0) && err == nil {
		log.Warn(fmt.Sprintf("reading %v bytes at offset %v", len(buf), total))
		n, err = r.ReadAt(buf, int64(total))
		total += n
	}
	log.Warn(fmt.Sprintf("read %v bytes at offset %v", len(buf), total))
	if err != nil && err != io.EOF {
		return total, err
	}
	return total, nil
}

func testDeliveryFromNodes(adapter adapters.NodeAdapter) (*simulations.StepResult, error) {
	nodes := 2
	conns := 0
	size := 8100
	skipCheck := true

	trigger := func(net *simulations.Network) chan discover.NodeID {
		triggerC := make(chan discover.NodeID)
		ticker := time.NewTicker(500 * time.Millisecond)
		go func() {
			defer ticker.Stop()
			for range ticker.C {
				triggerC <- net.Nodes[0].ID()
			}
		}()
		return triggerC
	}

	action := func(net *simulations.Network) func(context.Context) error {
		rrdpa := storage.NewDPA(newRoundRobinStore(localStores[1:]...), storage.NewChunkerParams())
		rrdpa.Start()
		dpacs := storage.NewDpaChunkStore(localStores[0].(*storage.LocalStore), func(chunk *storage.Chunk) error { return delivery.RequestFromPeers(chunk.Key[:], skipCheck) })
		dpa := storage.NewDPA(dpacs, storage.NewChunkerParams())
		dpa.Start()
		return func(context.Context) error {
			defer rrdpa.Stop()
			hash, wait, err := rrdpa.Store(crand.Reader, int64(size))
			if err != nil {
				return err
			}
			wait()
			fileHash = hash
			go func() {
				defer dpa.Stop()
				log.Debug(fmt.Sprintf("retrieve %v", fileHash))
				n, err := mustReadAll(dpa, fileHash)
				log.Debug(fmt.Sprintf("retrieved %v", fileHash), "read", n, "err", err)
			}()
			return nil
		}
	}

	check := func(net *simulations.Network, dpa *storage.DPA) func(ctx context.Context, id discover.NodeID) (bool, error) {
		return func(ctx context.Context, id discover.NodeID) (bool, error) {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			default:
			}
			log.Warn(fmt.Sprintf("try to locally retrieve %v", fileHash))
			total, err := mustReadAll(dpa, fileHash)
			if err != nil || total != size {
				log.Warn(fmt.Sprintf("number of bytes read %v/%v (error: %v)", total, size, err))
				return false, nil
			}
			return true, nil
			// node := net.GetNode(id)
			// if node == nil {
			// 	return false, fmt.Errorf("unknown node: %s", id)
			// }
			// client, err := node.Client()
			// if err != nil {
			// 	return false, fmt.Errorf("error getting node client: %s", err)
			// }
			// var response int
			// if err := client.Call(&response, "test_haslocal", hash); err != nil {
			// 	return false, fmt.Errorf("error getting bzz_has response: %s", err)
			// }
			// log.Debug(fmt.Sprintf("node has: %v\n%v", id, response))
			// return response == 0, nil
		}
	}

	result, err := runSimulation(nodes, conns, action, trigger, check, adapter)
	if err != nil {
		return nil, fmt.Errorf("Setting up simulation failed: %v", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("Simulation failed: %s", result.Error)
	}
	return result, err
}

func runSimulation(nodes, conns int, action func(*simulations.Network) func(context.Context) error, trigger func(*simulations.Network) chan discover.NodeID, check func(*simulations.Network, *storage.DPA) func(context.Context, discover.NodeID) (bool, error), adapter adapters.NodeAdapter) (*simulations.StepResult, error) {
	// create network
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID:             "0",
		DefaultService: serviceName,
	})
	defer net.Shutdown()
	teardown, err := setLocalStores(nodes)
	defer teardown()
	if err != nil {
		return nil, err
	}
	ids := make([]discover.NodeID, nodes)
	nodeCount = 0
	for i := 0; i < nodes; i++ {
		node, err := net.NewNode()
		if err != nil {
			return nil, fmt.Errorf("error starting node: %s", err)
		}
		if err := net.Start(node.ID()); err != nil {
			return nil, fmt.Errorf("error starting node %s: %s", node.ID().TerminalString(), err)
		}
		ids[i] = node.ID()
	}

	// run a simulation which connects the 10 nodes in a ring and waits
	// for full peer discovery
	var addrs [][]byte
	wg := sync.WaitGroup{}
	for i := range ids {
		// collect the overlay addresses, to
		addrs = append(addrs, ToOverlayAddr(ids[i].Bytes()))
		for j := 0; j < conns; j++ {
			var k int
			if j == 0 {
				k = i - 1
			} else {
				k = rand.Intn(len(ids))
			}
			if i > 0 {
				wg.Add(1)
				go func(i, k int) {
					defer wg.Done()
					net.Connect(ids[i], ids[k])
				}(i, k)
			}
		}
	}
	wg.Wait()
	log.Debug(fmt.Sprintf("nodes: %v", len(addrs)))

	// 64 nodes ~ 1min
	// 128 nodes ~
	dpa := storage.NewDPA(localStores[0], storage.NewChunkerParams())
	dpa.Start()
	timeout := 300 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := simulations.NewSimulation(net).Run(ctx, &simulations.Step{
		Action:  action(net),
		Trigger: trigger(net),
		Expect: &simulations.Expectation{
			Nodes: ids,
			Check: check(net, dpa),
		},
	})
	return result, nil
}

func newService(ctx *adapters.ServiceContext) (node.Service, error) {
	id := ctx.Config.ID
	addr := NewAddrFromNodeID(id)
	kad := NewKademlia(addr.Over(), NewKadParams())
	localStore := localStores[nodeCount]
	dbAccess := NewDbAccess(localStore.(*storage.LocalStore))
	streamer := NewStreamer(NewDelivery(kad, dbAccess))
	if nodeCount == 0 {
		delivery = streamer.delivery
	}
	nodeCount++
	run := func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
		bzzPeer := &bzzPeer{
			Peer:      protocols.NewPeer(p, rw, StreamerSpec),
			localAddr: addr,
			BzzAddr:   NewAddrFromNodeID(p.ID()),
		}
		kad.On(bzzPeer)
		streamer.Subscribe(p.ID(), retrieveRequestStream, nil, 0, 0, Top, true)
		return streamer.Run(bzzPeer)
	}

	return &testDeliveryService{
		run: run,
	}, nil
}

type testDeliveryService struct {
	run func(p *p2p.Peer, rw p2p.MsgReadWriter) error
}

func (tds *testDeliveryService) Protocols() []p2p.Protocol {
	return []p2p.Protocol{
		{
			Name:    StreamerSpec.Name,
			Version: StreamerSpec.Version,
			Length:  StreamerSpec.Length(),
			Run:     tds.run,
			// NodeInfo: ,
			// PeerInfo: ,
		},
	}
}

func (b *testDeliveryService) APIs() []rpc.API {
	return []rpc.API{}
}

func (b *testDeliveryService) Start(server *p2p.Server) error {
	return nil
}

func (b *testDeliveryService) Stop() error {
	return nil
}