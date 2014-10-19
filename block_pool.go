package eth

import (
	"bytes"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/eth-go/ethchain"
	"github.com/ethereum/eth-go/ethlog"
	"github.com/ethereum/eth-go/ethutil"
	"github.com/ethereum/eth-go/p2p"
)

var poollogger = ethlog.NewLogger("BPOOL")

type block struct {
	from      *p2p.Peer
	peer      *p2p.Peer
	block     *ethchain.Block
	reqAt     time.Time
	requested int
}

type BlockPool struct {
	mut    sync.Mutex
	tdlock sync.Mutex

	eth ethchain.EthManager

	hashSyncLock  sync.Mutex
	hashSyncGroup *sync.WaitGroup
	syncLock      sync.Mutex
	syncGroup     *sync.WaitGroup

	hashes [][]byte
	pool   map[string]*block

	td   *big.Int
	quit chan bool

	fetchingHashes    bool
	downloadStartedAt time.Time

	ChainLength, BlocksProcessed int

	peer *p2p.Peer
}

func NewBlockPool(eth ethchain.EthManager) *BlockPool {
	return &BlockPool{
		eth:  eth,
		pool: make(map[string]*block),
		td:   ethutil.Big0,
		quit: make(chan bool),
	}
}

func (self *BlockPool) Len() int {
	return len(self.hashes)
}

func (self *BlockPool) Reset() {
	self.pool = make(map[string]*block)
	self.hashes = nil
}

func (self *BlockPool) HasLatestHash() bool {
	self.mut.Lock()
	defer self.mut.Unlock()
	return self.pool[string(self.eth.BlockChain().CurrentBlock.Hash())] != nil
}

func (self *BlockPool) HasCommonHash(hash []byte) bool {
	return self.eth.BlockChain().GetBlock(hash) != nil
}

func (self *BlockPool) Blocks() (blocks ethchain.Blocks) {
	for _, item := range self.pool {
		if item.block != nil {
			blocks = append(blocks, item.block)
		}
	}

	return
}

func (self *BlockPool) TD() *big.Int {
	self.tdlock.Lock()
	defer self.tdlock.Unlock()
	return self.td
}

func (self *BlockPool) AddPeer(td *big.Int, peer *p2p.Peer) {
	self.tdlock.Lock()
	defer self.tdlock.Unlock()
	if self.peer == nil && td.Cmp(self.td) >= 0 {
		self.peer = peer
		self.td = td
		poollogger.Debugf("Found suitable peer (%v)\n", td)
	}
	if self.peer != nil && td.Cmp(self.td) > 0 {
		// self.peer.HashSync(false)
		self.peer = peer
		self.td = td
		poollogger.Debugf("Found peer with higher td (%v > %v)\n", td, self.td)
	}
	return
}

func (self *BlockPool) AddHash(hash []byte, peer *p2p.Peer) {
	self.mut.Lock()
	defer self.mut.Unlock()

	if self.pool[string(hash)] == nil {
		self.pool[string(hash)] = &block{peer, nil, nil, time.Now(), 0}

		self.hashes = append([][]byte{hash}, self.hashes...)
	}
}

func (self *BlockPool) AddNewBlock(td *big.Int, b *ethchain.Block, peer *p2p.Peer) (fetchHashes bool) {
	self.tdlock.Lock()
	defer self.tdlock.Unlock()
	if td.Cmp(self.td) > 0 {
		fetchHashes = self.AddBlock(b, peer)
	}
	return
}

func (self *BlockPool) AddBlock(b *ethchain.Block, peer *p2p.Peer) (fetchHashes bool) {
	self.mut.Lock()
	defer self.mut.Unlock()

	hash := string(b.Hash())

	requested := self.pool[hash] != nil

	if requested {
		self.pool[hash].block = b
	} else {
		if known := self.eth.BlockChain().HasBlock(b.Hash()); !known {
			poollogger.Infof("Got unrequested block (%x...)\n", hash[0:4])
			self.hashes = append(self.hashes, b.Hash())
			self.pool[hash] = &block{peer, peer, b, time.Now(), 0}
			if !self.eth.BlockChain().HasBlock(b.PrevHash) && self.pool[string(b.PrevHash)] == nil && !self.fetchingHashes {
				poollogger.Infof("Unknown chain, requesting (%x...)\n", b.PrevHash[0:4])
				fetchHashes = true
			}
		}
	}
	self.BlocksProcessed++
	return
}

func (self *BlockPool) Remove(hash []byte) {
	self.mut.Lock()
	defer self.mut.Unlock()

	self.hashes = ethutil.DeleteFromByteSlice(self.hashes, hash)
	delete(self.pool, string(hash))
}

func (self *BlockPool) DistributeHashes() {
	self.mut.Lock()
	defer self.mut.Unlock()

	// var (
	// 	peerLen = self.eth.peers.Len()
	// 	amount  = 256 * peerLen
	// 	dist    = make(map[*p2p.Peer][][]byte)
	// )

	// num := int(math.Min(float64(amount), float64(len(self.pool))))
	// for i, j := 0, 0; i < len(self.hashes) && j < num; i++ {
	// 	hash := self.hashes[i]
	// 	item := self.pool[string(hash)]

	// 	if item != nil && item.block == nil {
	// 		var peer *Peer
	// 		lastFetchFailed := time.Since(item.reqAt) > 5*time.Second

	// 		// Handle failed requests
	// 		if lastFetchFailed && item.requested > 5 && item.peer != nil {
	// 			if item.requested < 100 {
	// 				// Select peer the hash was retrieved off
	// 				peer = item.from
	// 			} else {
	// 				// Remove it
	// 				self.hashes = ethutil.DeleteFromByteSlice(self.hashes, hash)
	// 				delete(self.pool, string(hash))
	// 			}
	// 		} else if lastFetchFailed || item.peer == nil {
	// 			// Find a suitable, available peer
	// 			eachPeer(self.eth.peers, func(p *Peer, v *list.Element) {
	// 				if peer == nil && len(dist[p]) < amount/peerLen {
	// 					peer = p
	// 				}
	// 			})
	// 		}

	// 		if peer != nil {
	// 			item.reqAt = time.Now()
	// 			item.peer = peer
	// 			item.requested++

	// 			dist[peer] = append(dist[peer], hash)
	// 		}
	// 	}
	// }

	// for peer, hashes := range dist {
	// 	peer.FetchBlocks(hashes)
	// }

	// if len(dist) > 0 {
	// 	self.downloadStartedAt = time.Now()
	// }
}

func (self *BlockPool) Start() {
	go self.chainThread()
}

func (self *BlockPool) Stop() {
	close(self.quit)
}

func (self *BlockPool) HashSync() *sync.WaitGroup {
	self.hashSyncLock.Lock()
	defer self.hashSyncLock.Unlock()
	if self.hashSyncGroup == nil {
		self.hashSyncGroup = &sync.WaitGroup{}
		self.hashSyncGroup.Add(1)
		go func() {
			self.hashSyncGroup.Wait()
			self.hashSyncGroup = nil
			if len(self.hashes) > 0 {
				self.DistributeHashes()
			}

			if self.ChainLength < len(self.hashes) {
				self.ChainLength = len(self.hashes)
			}
		}()
	} else {
		self.hashSyncGroup.Add(1)
	}
	return self.hashSyncGroup
}

func (self *BlockPool) HashSyncing() (syncing bool) {
	self.hashSyncLock.Lock()
	defer self.hashSyncLock.Unlock()
	if self.hashSyncGroup != nil {
		syncing = true
	}
	return
}

func (self *BlockPool) ChainSync() *sync.WaitGroup {
	self.syncLock.Lock()
	defer self.syncLock.Unlock()
	if self.syncGroup == nil {
		self.syncGroup = &sync.WaitGroup{}
		self.syncGroup.Add(1)
		self.eth.EventMux().Post(ChainSyncEvent{true})
		go func() {
			self.syncGroup.Wait()
			self.syncLock.Lock()
			defer self.syncLock.Unlock()
			self.eth.EventMux().Post(ChainSyncEvent{false})
			self.syncGroup = nil
		}()
	} else {
		self.syncGroup.Add(1)
	}
	return self.syncGroup
}

func (self *BlockPool) chainThread() {
	procTimer := time.NewTicker(500 * time.Millisecond)
out:
	for {
		select {
		case <-self.quit:
			break out
		case <-procTimer.C:
			blocks := self.Blocks()
			ethchain.BlockBy(ethchain.Number).Sort(blocks)

			// Find common block
			for i, block := range blocks {
				if self.eth.BlockChain().HasBlock(block.PrevHash) {
					blocks = blocks[i:]
					break
				}
			}

			if len(blocks) > 0 {
				if self.eth.BlockChain().HasBlock(blocks[0].PrevHash) {
					for i, block := range blocks[1:] {
						// NOTE: The Ith element in this loop refers to the previous block in
						// outer "blocks"
						if bytes.Compare(block.PrevHash, blocks[i].Hash()) != 0 {
							blocks = blocks[:i]

							break
						}
					}
				} else {
					blocks = nil
				}
			}

			var err error
			for i, block := range blocks {
				err = self.eth.StateManager().Process(block, false)
				if err != nil {
					poollogger.Infoln(err)
					poollogger.Debugf("Block #%v failed (%x...)\n", block.Number, block.Hash()[0:4])
					poollogger.Debugln(block)

					blocks = blocks[i:]

					break
				}

				self.Remove(block.Hash())
			}

			if err != nil {
				self.Reset()

				poollogger.Debugf("Punishing peer for supplying bad chain (%v)\n", self.peer.Address)
				// This peer gave us bad hashes and made us fetch a bad chain, therefor he shall be punished.
				//self.eth.BlacklistPeer(self.peer.Pubkey)
				self.peer.PeerErrorChan() <- p2p.NewPeerError(p2p.ProtocolBreach, "bad chain")
				self.td = ethutil.Big0
				self.peer = nil
			}
		}
	}
}
