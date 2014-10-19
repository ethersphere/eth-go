package eth

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/eth-go/p2p"
)

const (
	ProtocolVersion = 0x1c
	// 0x00 // PoC-1
	// 0x01 // PoC-2
	// 0x07 // PoC-3
	// 0x09 // PoC-4
	// 0x17 // PoC-5
	// 0x1c // PoC-6
	NetworkId = 0
)

const (
	StatusMsg = iota
	GetTxMsg
	TxMsg
	GetBlockHashesMsg
	BlockHashesMsg
	GetBlocksMsg
	BlocksMsg
	NewBlockMsg
	offset = 8
)

type ProtocolState uint8

const (
	nullState = iota
	statusReceived
)

type EthProtocol struct {
	peer      *p2p.Peer
	state     ProtocolState
	stateLock sync.RWMutex

	hashSyncLock  sync.Mutex
	hashSyncGroup sync.WaitGroup
	syncLock      sync.Mutex
	syncGroup     sync.WaitGroup

	ethereum *Ethereum

	td       *big.Int
	bestHash []byte
	// lastReceivedHash []byte
	// requestedHashes  [][]byte

	lastBlockReceived  time.Time
	blocksRequested    int
	lastRequestedBlock *ethchain.Block
}

func NewEthProtocol(peer *p2p.Peer) *EthProtocol {
	return &EthProtocol{
		peer: peer,
	}
}

func (self *EthProtocol) Start() {
	self.peer.Write("", self.statusMsg())
}

func (self *EthProtocol) Stop() {
	self.HashSync(false)
	self.Sync(false)
}

func (self *EthProtocol) statusMsg() *p2p.Msg {
	msg, _ := p2p.NewMsg(StatusMsg,
		uint32(ProtocolVersion),
		uint32(NetVersion),
		self.ethereum.BlockChain().TD,
		self.ethereum.BlockChain().CurrentBlock.Hash(),
		self.ethereum.BlockChain().Genesis().Hash(),
	)
}

func (self *EthProtocol) Name() string {
	return "eth"
}

func (self *EthProtocol) Offset() MsgCode {
	return offset
}

func (self *EthProtocol) CheckState(state ProtocolState) bool {
	self.stateLock.RLock()
	self.stateLock.RUnlock()
	if self.state != state {
		return false
	} else {
		return true
	}
}

func (self *EthProtocol) HandleIn(msg *Msg, response chan *Msg) {
	defer close(response)
	if msg.Code() == HandshakeMsg {
		self.handleStatus(msg, response)
	} else {
		if !self.CheckState(statusReceived) {
			self.peerError(ProtocolBreach, "message code %v not allowed", msg.Code())
			return
		}
		data := msg.Data()
		switch msg.Code() {
		//
		case GetTxsMsg:
			// Get the current transactions of the pool
			txs := self.ethereum.TxPool().CurrentTransactions()
			// Get the RlpData values from the txs
			txsInterface := make([]interface{}, len(txs))
			for i, tx := range txs {
				txsInterface[i] = tx.RlpData()
			}
			out, _ := p2p.NewMsg(TxMsg, txsInterface...)
			response <- out

		case TxMsg:
			// If the message was a transaction queue the transaction
			// in the TxPool where it will undergo validation and
			// processing when a new block is found
			for i := 0; i < data.Len(); i++ {
				tx := ethchain.NewTransactionFromValue(data.Get(i))
				self.ethereum.TxPool().QueueTransaction(tx)
			}
		case GetBlockHashesMsg:
			if data.Len() < 2 {
				self.peerError(p2p.InvalidMsg, "argument length invalid %d (expect >2)", data.Len())
			}

			hash := data.Get(0).Bytes()
			amount := data.Get(1).Uint()

			hashes := self.ethereum.BlockChain().GetChainHashesFromHash(hash, amount)

			out, _ := p2p.NewMsg(BlockHashesMsg, ethutil.ByteSliceToInterface(hashes)...)
			response <- out
		case BlockHashesMsg:
			self.Sync(true) // ? what for after all?

			blockPool := self.ethereum.blockPool
			foundCommonHash := false

			it := data.NewIterator()
			for it.Next() {
				hash := it.Value().Bytes()
				self.lastReceivedHash = hash
				if blockPool.HasCommonHash(hash) {
					foundCommonHash = true
					break
				}
				blockPool.AddHash(hash, self)
			}

			if !foundCommonHash {
				// fetching more hashes but o need to check TD with blockpool
				self.FetchHashes(response, lastReceivedHash)
			} else {
				logger.Infof("Found common hash (%x...)\n", self.lastReceivedHash[0:4])
				self.HashSync(false)
			}

		case GetBlocksMsg:
			// Limit to max 300 blocks
			max := int(math.Min(float64(data.Len()), 300.0))
			var blocks []interface{}

			for i := 0; i < max; i++ {
				hash := data.Get(i).Bytes()
				block := self.ethereum.BlockChain().GetBlock(hash)
				if block != nil {
					blocks = append(blocks, block.Value().Raw())
				}
			}
			out, _ := p2p.NewMsg(BlocksMsg, blocks...)
			response <- out

		case BlocksMsg:
			self.Sync(true) // what for?

			it := data.NewIterator()
			for it.Next() {
				block := ethchain.NewBlockFromRlpValue(it.Value())
				blockPool.AddBlock(block, self.peer)
			}
			self.lastBlockReceived = time.Now()
		case NewBlockMsg:
			var (
				blockPool = self.ethereum.blockPool
				block     = ethchain.NewBlockFromRlpValue(data.Get(0))
				td        = data.Get(1).BigInt()
			)

			// this should reset td and offer blockpool as candidate new peer?
			if blockPool.AddNewBlock(td, block, self.peer) {
				out, _ := p2p.NewMsg(GetBlockHashesMsg, block.Hash(), uint32(256))
				response <- out
			}

		default:
			self.peerError(p2p.InvalidMsgCode, "unknown message code %v", msg.Code())
		}
	}
}

func (self *EthProtocol) Sync(sync bool) {
	self.synclock.Lock()
	defer self.synclock.Lock()
	if self.syncGroup == nil {

		if sync {
			self.syncGroup = self.ethereum.BlockPool().Sync()
		}
	} else {
		if !sync {
			self.syncGroup.Done()
			self.syncGroup = nil
		}
	}
}

func (self *EthProtocol) HashSync(sync bool) {
	self.hashSyncLock.Lock()
	defer self.hashSyncLock.Lock()
	if self.hashSyncGroup == nil {
		if sync {
			self.hashSyncGroup = self.ethereum.BlockPool().HashSync()
		}
	} else {
		if !sync {
			self.hashSyncGroup.Done()
			self.hashSyncGroup = nil
		}
	}
}

func (self *EthProtocol) HandleOut(msg *p2p.Msg) (allowed bool) {
	// somewhat overly paranoid
	allowed = msg.Code() == StatusMsg || msg.Code() < self.Offset() && self.CheckState(statusReceived)
	return
}

func (self *EthProtocol) peerError(errorCode p2p.ErrorCode, format string, v ...interface{}) {
	err := NewPeerError(errorCode, format, v...)
	logger.Warnln(err)
	fmt.Println(self.peer, err)
	if self.peer != nil {
		self.peer.PeerErrorChan() <- err
	}
}

func (self *EthProtocol) handleStatus(msg *p2p.Msg, response chan *p2p.Msg) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.state != nullState {
		self.peerError(ProtocolBreach, "extra status")
		return
	}
	c := msg.Data

	var (
		protocolVersion = c.Get(0).Uint()
		networkId       = c.Get(1).Uint()
		td              = c.Get(2).BigInt()
		bestHash        = c.Get(3).Bytes()
		genesis         = c.Get(4).Bytes()
	)

	if bytes.Compare(self.ethereum.BlockChain().Genesis().Hash(), genesis) != 0 {
		self.peerError(p2p.InvalidGenesis, "%x", genesis)
		return
	}

	if networkId != NetworkId {
		self.peerError(p2p.InvalidNetworkId, "%d (!=%d)", networkId, NetworkId)
		return
	}

	if protocolVersion != ProtocolVersion {
		self.peerError(p2p.InvalidProtocolVersion, "%d (!=%d)", protocolVersion, ProtocolVersion)
		return
	}

	self.td = td
	self.bestHash = bestHash

	self.state = statusReceived

	if self.ethereum.BlockPool().AddPeer(self.td, self.peer) {
		// peer.HashSync(false)?
	}
	// this should only be called if this is higher difficulty no?
	self.FetchHashes(response, bestHash)

	ethlogger.Infof("Peer is [eth] capable (%d/%d). TD = %v ~ %x", protocolVersion, networkId, self.td, self.bestHash)

}

func (self *EthProtocol) FetchHashes(response chan *p2p.Msg, lastReceivedHash []byte) {

	if !self.ethereum.BlockPool().HasLatestHash() {
		self.HashSync(true)
		const amount = 256
		ethlogger.Debugf("Fetching hashes (%d) %x...\n", amount, lastReceivedHash[0:4])
		msg, _ := p2p.NewMsg(p2p.GetBlockHashesMsg, lastReceivedHash, uint32(amount))
		response <- msg
	}
}
