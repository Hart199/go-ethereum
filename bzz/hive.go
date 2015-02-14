package bzz

type peer struct {
	*bzzProtocol
	address    []byte
	addressKey string
}

// This is a mock implementation with a fixed peer pool with no distinction between peers
type hive struct {
	pool map[string]peer
}

func newHive() *hive {
	return &hive{
		pool: make(map[string]peer),
	}
}

func (self *hive) addPeer(p peer) {
	self.pool[p.addressKey] = p
}

func (self *hive) removePeer(p peer) {
	delete(self.pool, p.addressKey)
}

// Retrieve a list of live peers that are closer to target than us
func (self *hive) getPeers(target Key) (peers []peer) {

	for _, value := range self.pool {
		peers = append(peers, value)
	}
	return
}

func (self *hive) addPeers(req *peersMsgData) (err error) {
	return
}
