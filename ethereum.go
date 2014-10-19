package eth

import (
	"container/list"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/eth-go/ethchain"
	"github.com/ethereum/eth-go/ethcrypto"
	"github.com/ethereum/eth-go/ethlog"
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

	// Capabilities for outgoing peers
	// serverCaps Caps
	peersFile string

	Mining bool

	listening bool

	RpcServer *ethrpc.JsonRpcServer

	keyManager *ethcrypto.KeyManager

	clientIdentity ethwire.ClientIdentity

	synclock  sync.Mutex
	syncGroup sync.WaitGroup

	filterMu sync.RWMutex
	filterId int
	filters  map[int]*ethchain.Filter
}

func New(db ethutil.Database, identity p2p.ClientIdentity, keyManager *ethcrypto.KeyManager, maxPeers int, caps Caps, usePnp bool) (*Ethereum, error) {
	var err error

	saveProtocolVersion(db)
	ethutil.Config.Db = db

	// FIXME:
	blacklist := p2p.BlacklistMap{}
	// Sorry Py person. I must blacklist. you perform badly
	s.blacklist.Put(ethutil.Hex2Bytes("64656330303561383532336435376331616537643864663236623336313863373537353163636634333530626263396330346237336262623931383064393031"))

	var natType p2p.NATType

	network := p2p.NewTCPNetwork(natType)
	handlers := make(p2p.Handlers)
	server := p2p.New(network, addr, identity, handlers, maxPeers, blacklist)

	peersFile := path.Join(ethutil.Config.ExecPath, "known_peers.json")

	nonce, _ := ethutil.RandomUint64()

	eventMux := event.NewTypeMux()
	ethereum := &Ethereum{
		shutdownChan: make(chan bool),
		quit:         make(chan bool),
		db:           db,
		eventMux:     eventMux,
		Nonce:        nonce,
		// serverCaps:     caps,
		server:         server,
		peersFile:      peersFile,
		keyManager:     keyManager,
		clientIdentity: clientIdentity,
		blacklist:      blacklist,
		filters:        make(map[int]*ethchain.Filter),
	}

	ethereum.txPool = ethchain.NewTxPool(ethereum)
	ethereum.blockChain = ethchain.NewBlockChain(ethereum)
	ethereum.blockPool = NewBlockPool(eventMux, ethereum.blockChain)
	ethereum.stateManager = ethchain.NewStateManager(ethereum)

	// tart the tx pool
	ethereum.txPool.Start()

	return ethereum, nil
}

func (s *Ethereum) EventMux() *event.TypeMux {
	return s.eventMux
}

func (s *Ethereum) KeyManager() *ethcrypto.KeyManager {
	return s.keyManager
}

func (s *Ethereum) ClientIdentity() ethwire.ClientIdentity {
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

// Start the ethereum
func (s *Ethereum) Start(seed bool) {
	s.blockPool.Start()
	s.stateManager.Start()

	go s.filterLoop()

	if seed {
		go Seed(s.peersFile, bootstrap, func(addr net.Addr) { s.Server().PeerConnect(addr) })
	}
	ethlogger.Infoln("Server started")
}

func (s *Ethereum) Stop() {
	// Close the database
	defer s.db.Close()

	WritePeers(s.peersFile, s.Server().PeerAddresses())
	close(s.quit)

	if s.RpcServer != nil {
		s.RpcServer.Stop()
	}
	s.txPool.Stop()
	s.stateManager.Stop()
	s.eventMux.Stop()
	s.blockPool.Stop()

	ethlogger.Infoln("Server stopped")
	close(s.shutdownChan)
}

// This function will wait for a shutdown and resumes main thread execution
func (s *Ethereum) WaitForShutdown() {
	<-s.shutdownChan
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
	blockChan := make(chan ethreact.Event, 5)
	messageChan := make(chan ethreact.Event, 5)
	// Subscribe to events
	reactor := self.Reactor()
	reactor.Subscribe("newBlock", blockChan)
	reactor.Subscribe("messages", messageChan)
out:
	for {
		select {
		case <-self.quit:
			break out
		case block := <-blockChan:
			if block, ok := block.Resource.(*ethchain.Block); ok {
				self.filterMu.RLock()
				for _, filter := range self.filters {
					if filter.BlockCallback != nil {
						filter.BlockCallback(block)
					}
				}
				self.filterMu.RUnlock()
			}
		case msg := <-messageChan:
			if messages, ok := msg.Resource.(ethstate.Messages); ok {
				self.filterMu.RLock()
				for _, filter := range self.filters {
					if filter.MessageCallback != nil {
						msgs := filter.FilterMessages(messages)
						if len(msgs) > 0 {
							filter.MessageCallback(msgs)
						}
					}
				}
				self.filterMu.RUnlock()
			}
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
