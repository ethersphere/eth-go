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

	ethereum           *Ethereum
	lastBlockReceived  time.Time
	doneFetchingHashes bool

	td               *big.Int
	bestHash         []byte
	lastReceivedHash []byte
	requestedHashes  [][]byte

	catchingUp         bool
	diverted           bool
	blocksRequested    int
	lastRequestedBlock *ethchain.Block
}

func NewEthProtocol(peer *p2p.Peer) *EthProtocol {
	self := &EthProtocol{
		peer: peer,
	}

	return self
}

func (self *EthProtocol) Start() {
	self.peer.Write("", self.statusMsg())
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
	if msg.Code() == HandshakeMsg {
		self.handleStatus(msg)
	} else {
		if !self.CheckState(statusReceived) {
			self.peerError(ProtocolBreach, "message code %v not allowed", msg.Code())
			close(response)
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

		case BlockHashesMsg:
			self.catchingUp = true

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
				blockPool.FetchHashes(self)
			} else {
				logger.Infof("Found common hash (%x...)\n", self.lastReceivedHash[0:4])
				self.doneFetchingHashes = true
			}

		case BlocksMsg:
			self.catchingUp = true

			blockPool := self.ethereum.blockPool

			it := data.NewIterator()
			for it.Next() {
				block := ethchain.NewBlockFromRlpValue(it.Value())
				blockPool.Add(block, p)

				self.lastBlockReceived = time.Now()
			}
		case NewBlockMsg:
			var (
				blockPool = self.ethereum.blockPool
				block     = ethchain.NewBlockFromRlpValue(data.Get(0))
				td        = data.Get(1).BigInt()
			)

			if td.Cmp(blockPool.td) > 0 {
				blockPool.AddNew(block, p)
			}

		default:
			self.peerError(InvalidMsgCode, "unknown message code %v", msg.Code())
		}
	}
	close(response)
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

func (self *EthProtocol) handleStatus(msg *p2p.Msg) {
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
		self.peerError(InvalidNetworkId, "%d (!=%d)", networkId, NetworkId)
		return
	}

	if protocolVersion != ProtocolVersion {
		self.peerError(p2p.InvalidProtocolVersion, "%d (!=%d)", protocolVersion, ProtocolVersion)
		return
	}

	// Get the td and last hash
	self.td = td
	self.bestHash = bestHash
	self.lastReceivedHash = bestHash

	self.state = statusReceived

	// Compare the total TD with the blockchain TD. If remote is higher
	// fetch hashes from highest TD node.
	self.ethereum.BlockPool().FetchHashes(self)

	ethlogger.Infof("Peer is [eth] capable (%d/%d). TD = %v ~ %x", protocolVersion, networkId, self.td, self.bestHash)

}
