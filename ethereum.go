package eth

import (
	"net"
	"path"
	"sync"

	"github.com/ethereum/eth-go/ethchain"
	"github.com/ethereum/eth-go/ethcrypto"
	"github.com/ethereum/eth-go/ethlog"
	"github.com/ethereum/eth-go/ethminer"
	"github.com/ethereum/eth-go/ethrpc"
	"github.com/ethereum/eth-go/ethstate"
	"github.com/ethereum/eth-go/ethutil"
	"github.com/ethereum/eth-go/event"
	"github.com/ethereum/eth-go/p2p"
)

var logger = ethlog.NewLogger("SERV")

type Ethereum struct {
	// Channel for shutting down the ethereum
	shutdownChan chan bool
	quit         chan bool

	// DB interface
	db ethutil.Database
	// State manager for processing new blocks and managing the over all states
	stateManager *ethchain.StateManager
	// The transaction pool. Transaction can be pushed on this pool
	// for later including in the blocks
	txPool *ethchain.TxPool
	// The canonical chain
	blockChain *ethchain.BlockChain
	// The block pool
	blockPool *BlockPool
	// Event
	eventMux *event.TypeMux

	// Nonce
	Nonce uint64

	Addr net.Addr
	Port string

	blacklist p2p.Blacklist
	server    *p2p.Server
	txSub     event.Subscription
	blockSub  event.Subscription

	// Capabilities for outgoing peers
	// serverCaps Caps
	peersFile string

	Mining bool

	listening bool

	RpcServer *ethrpc.JsonRpcServer

	keyManager *ethcrypto.KeyManager

	clientIdentity p2p.ClientIdentity

	synclock  sync.Mutex
	syncGroup sync.WaitGroup

	filterMu sync.RWMutex
	filterId int
	filters  map[int]*ethchain.Filter
}

func New(db ethutil.Database, identity p2p.ClientIdentity, keyManager *ethcrypto.KeyManager, caps Caps, usePnp bool, port string, maxPeers int) (*Ethereum, error) {

	saveProtocolVersion(db)
	ethutil.Config.Db = db

	// FIXME:
	blacklist := &p2p.BlacklistMap{}
	// Sorry Py person. I must blacklist. you perform badly
	blacklist.Put(ethutil.Hex2Bytes("64656330303561383532336435376331616537643864663236623336313863373537353163636634333530626263396330346237336262623931383064393031"))

	// setup network and p2p layer
	var natType p2p.NATType
	network := p2p.NewTCPNetwork(natType)
	addr, err := network.ParseAddr(net.JoinHostPort("0.0.0.0", port))
	if err != nil {
		return nil, err
	}
	handlers := make(p2p.Handlers)
	server := p2p.New(network, addr, identity, handlers, maxPeers, blacklist)

	peersFile := path.Join(ethutil.Config.ExecPath, "known_peers.json")

	nonce, _ := ethutil.RandomUint64()

	eth := &Ethereum{
		shutdownChan: make(chan bool),
		quit:         make(chan bool),
		db:           db,
		Nonce:        nonce,
		// serverCaps:     caps,
		server:         server,
		peersFile:      peersFile,
		keyManager:     keyManager,
		clientIdentity: identity,
		blacklist:      blacklist,
		filters:        make(map[int]*ethchain.Filter),
	}

	eth.blockChain = ethchain.NewBlockChain(db)
	eth.stateManager = ethchain.NewStateManager(eth)
	eth.blockPool = NewBlockPool(eth)
	eth.txPool = ethchain.NewTxPool(eth)

	// tart the tx pool
	eth.txPool.Start()

	return eth, nil
}

// Broadcast the transaction to the rest of the peers

func (self *Ethereum) Broadcast(msg *p2p.Msg) {
	self.server.Broadcast("eth", msg)
}

func (self *Ethereum) ConnectToPeer(addr string) {
	address, err := self.server.ParseAddr(addr)
	if err == nil {
		self.server.PeerConnect(address)
	}
}

func (s *Ethereum) EventMux() *event.TypeMux {
	return s.eventMux
}

func (s *Ethereum) KeyManager() *ethcrypto.KeyManager {
	return s.keyManager
}

func (s *Ethereum) ClientIdentity() p2p.ClientIdentity {
	return s.clientIdentity
}

func (s *Ethereum) BlockChain() *ethchain.BlockChain {
	return s.blockChain
}

func (s *Ethereum) StateManager() *ethchain.StateManager {
	return s.stateManager
}

func (s *Ethereum) TxPool() *ethchain.TxPool {
	return s.txPool
}

func (s *Ethereum) BlockPool() *BlockPool {
	return s.blockPool
}

func (self *Ethereum) Db() ethutil.Database {
	return self.db
}

// func (s *Ethereum) ServerCaps() Caps {
// 	return s.serverCaps
// }

func (s *Ethereum) IsMining() bool {
	return s.Mining
}

func (s *Ethereum) IsListening() bool {
	return s.listening
}

func (s *Ethereum) PeerCount() int {
	return s.server.PeerCount()
}

func (s *Ethereum) Peers() []*p2p.Peer {
	return s.server.Peers()
}

// Start the ethereum
func (s *Ethereum) Start(seed bool) {
	s.blockPool.Start()
	s.stateManager.Start()

	go s.filterLoop()

	// broadcast transactions
	s.txSub = s.eventMux.Subscribe(ethchain.TxEvent{})
	go s.txBroadcastLoop()

	// broadcast mined blocks
	s.blockSub = s.eventMux.Subscribe(ethminer.NewMinedBlockEvent{})
	go s.blockBroadcastLoop()

	//FIXME:
	bootstrap := true
	if seed {
		go Seed(s.peersFile, bootstrap, func(addr net.Addr) { s.server.PeerConnect(addr) })
	}
	logger.Infoln("Server started")
}

func (s *Ethereum) Stop() {
	// Close the database
	defer s.db.Close()

	// WritePeers(s.peersFile, s.server.PeerAddresses())
	close(s.quit)

	s.txSub.Unsubscribe()    // quits txBroadcastLoop
	s.blockSub.Unsubscribe() // quits blockBroadcastLoop

	if s.RpcServer != nil {
		s.RpcServer.Stop()
	}
	s.txPool.Stop()
	s.stateManager.Stop()
	s.eventMux.Stop()
	s.blockPool.Stop()

	logger.Infoln("Server stopped")
	close(s.shutdownChan)
}

// This function will wait for a shutdown and resumes main thread execution
func (s *Ethereum) WaitForShutdown() {
	<-s.shutdownChan
}

// now tx broadcasting is taken out of txPool
// handled here via subscription, efficiency?
func (self *Ethereum) txBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range self.txSub.Chan() {
		event := obj.(ethchain.TxEvent)
		if event.Type == ethchain.TxPre {
			msg, _ := p2p.NewMsg(TxMsg, event.Tx.RlpData())
			self.Broadcast(msg)
		}
	}
}

func (self *Ethereum) blockBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range self.txSub.Chan() {
		event := obj.(ethminer.NewMinedBlockEvent)
		msg, _ := p2p.NewMsg(NewBlockMsg, event.Block.Value().Val)
		self.Broadcast(msg)
	}
}

// InstallFilter adds filter for blockchain events.
// The filter's callbacks will run for matching blocks and messages.
// The filter should not be modified after it has been installed.
func (self *Ethereum) InstallFilter(filter *ethchain.Filter) (id int) {
	self.filterMu.Lock()
	id = self.filterId
	self.filters[id] = filter
	self.filterId++
	self.filterMu.Unlock()
	return id
}

func (self *Ethereum) UninstallFilter(id int) {
	self.filterMu.Lock()
	delete(self.filters, id)
	self.filterMu.Unlock()
}

// GetFilter retrieves a filter installed using InstallFilter.
// The filter may not be modified.
func (self *Ethereum) GetFilter(id int) *ethchain.Filter {
	self.filterMu.RLock()
	defer self.filterMu.RUnlock()
	return self.filters[id]
}

func (self *Ethereum) filterLoop() {
	// Subscribe to events
	events := self.eventMux.Subscribe(ethchain.NewBlockEvent{}, ethstate.Messages(nil))
	for event := range events.Chan() {
		switch event.(type) {
		case ethchain.NewBlockEvent:
			self.filterMu.RLock()
			for _, filter := range self.filters {
				if filter.BlockCallback != nil {
					e := event.(ethchain.NewBlockEvent)
					filter.BlockCallback(e.Block)
				}
			}
			self.filterMu.RUnlock()
		case ethstate.Messages:
			self.filterMu.RLock()
			for _, filter := range self.filters {
				if filter.MessageCallback != nil {
					e := event.(ethstate.Messages)
					msgs := filter.FilterMessages(e)
					if len(msgs) > 0 {
						filter.MessageCallback(msgs)
					}
				}
			}
			self.filterMu.RUnlock()
		}
	}
}

func saveProtocolVersion(db ethutil.Database) {
	d, _ := db.Get([]byte("ProtocolVersion"))
	protocolVersion := ethutil.NewValue(d).Uint()

	if protocolVersion == 0 {
		db.Put([]byte("ProtocolVersion"), ethutil.NewValue(ProtocolVersion).Bytes())
	}
}
