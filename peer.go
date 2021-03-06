package eth

import (
	"bytes"
	"container/list"
	"fmt"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/eth-go/ethchain"
	"github.com/ethereum/eth-go/ethlog"
	"github.com/ethereum/eth-go/ethutil"
	"github.com/ethereum/eth-go/ethwire"
)

var peerlogger = ethlog.NewLogger("PEER")

const (
	// The size of the output buffer for writing messages
	outputBufferSize = 50
	// Current protocol version
	ProtocolVersion = 28
	// Interval for ping/pong message
	pingPongTimer = 2 * time.Second
)

type DiscReason byte

const (
	// Values are given explicitly instead of by iota because these values are
	// defined by the wire protocol spec; it is easier for humans to ensure
	// correctness when values are explicit.
	DiscReRequested  = 0x00
	DiscReTcpSysErr  = 0x01
	DiscBadProto     = 0x02
	DiscBadPeer      = 0x03
	DiscTooManyPeers = 0x04
	DiscConnDup      = 0x05
	DiscGenesisErr   = 0x06
	DiscProtoErr     = 0x07
	DiscQuitting     = 0x08
)

var discReasonToString = []string{
	"requested",
	"TCP sys error",
	"bad protocol",
	"useless peer",
	"too many peers",
	"already connected",
	"wrong genesis block",
	"incompatible network",
	"quitting",
}

func (d DiscReason) String() string {
	if len(discReasonToString) < int(d) {
		return "Unknown"
	}

	return discReasonToString[d]
}

// Peer capabilities
type Caps byte

const (
	CapPeerDiscTy = 1 << iota
	CapTxTy
	CapChainTy

	CapDefault = CapChainTy | CapTxTy | CapPeerDiscTy
)

var capsToString = map[Caps]string{
	CapPeerDiscTy: "Peer discovery",
	CapTxTy:       "Transaction relaying",
	CapChainTy:    "Block chain relaying",
}

func (c Caps) IsCap(cap Caps) bool {
	return c&cap > 0
}

func (c Caps) String() string {
	var caps []string
	if c.IsCap(CapPeerDiscTy) {
		caps = append(caps, capsToString[CapPeerDiscTy])
	}
	if c.IsCap(CapChainTy) {
		caps = append(caps, capsToString[CapChainTy])
	}
	if c.IsCap(CapTxTy) {
		caps = append(caps, capsToString[CapTxTy])
	}

	return strings.Join(caps, " | ")
}

type Peer struct {
	// Ethereum interface
	ethereum *Ethereum
	// Net connection
	conn net.Conn
	// Output queue which is used to communicate and handle messages
	outputQueue chan *ethwire.Msg
	// Quit channel
	quit chan bool
	// Determines whether it's an inbound or outbound peer
	inbound bool
	// Flag for checking the peer's connectivity state
	connected  int32
	disconnect int32
	// Last known message send
	lastSend time.Time
	// Indicated whether a verack has been send or not
	// This flag is used by writeMessage to check if messages are allowed
	// to be send or not. If no version is known all messages are ignored.
	versionKnown bool

	// Last received pong message
	lastPong          int64
	lastBlockReceived time.Time

	host             []byte
	port             uint16
	caps             Caps
	td               *big.Int
	bestHash         []byte
	lastReceivedHash []byte
	requestedHashes  [][]byte

	// This peer's public key
	pubkey []byte

	// Indicated whether the node is catching up or not
	catchingUp      bool
	diverted        bool
	blocksRequested int

	version string

	// We use this to give some kind of pingtime to a node, not very accurate, could be improved.
	pingTime      time.Duration
	pingStartTime time.Time

	lastRequestedBlock *ethchain.Block
}

func NewPeer(conn net.Conn, ethereum *Ethereum, inbound bool) *Peer {
	pubkey := ethereum.KeyManager().PublicKey()[1:]

	return &Peer{
		outputQueue:     make(chan *ethwire.Msg, outputBufferSize),
		quit:            make(chan bool),
		ethereum:        ethereum,
		conn:            conn,
		inbound:         inbound,
		disconnect:      0,
		connected:       1,
		port:            30303,
		pubkey:          pubkey,
		blocksRequested: 10,
		caps:            ethereum.ServerCaps(),
		version:         ethereum.ClientIdentity().String(),
	}
}

func NewOutboundPeer(addr string, ethereum *Ethereum, caps Caps) *Peer {
	p := &Peer{
		outputQueue: make(chan *ethwire.Msg, outputBufferSize),
		quit:        make(chan bool),
		ethereum:    ethereum,
		inbound:     false,
		connected:   0,
		disconnect:  0,
		caps:        caps,
		version:     ethereum.ClientIdentity().String(),
	}

	// Set up the connection in another goroutine so we don't block the main thread
	go func() {
		conn, err := p.Connect(addr)
		if err != nil {
			peerlogger.Debugln("Connection to peer failed. Giving up.", err)
			p.Stop()
			return
		}
		p.conn = conn

		// Atomically set the connection state
		atomic.StoreInt32(&p.connected, 1)
		atomic.StoreInt32(&p.disconnect, 0)

		p.Start()
	}()

	return p
}

func (self *Peer) Connect(addr string) (conn net.Conn, err error) {
	const maxTries = 3
	for attempts := 0; attempts < maxTries; attempts++ {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			//peerlogger.Debugf("Peer connection failed. Retrying (%d/%d) (%s)\n", attempts+1, maxTries, addr)
			time.Sleep(time.Duration(attempts*20) * time.Second)
			continue
		}

		// Success
		return
	}

	return
}

// Getters
func (p *Peer) PingTime() string {
	return p.pingTime.String()
}
func (p *Peer) Inbound() bool {
	return p.inbound
}
func (p *Peer) LastSend() time.Time {
	return p.lastSend
}
func (p *Peer) LastPong() int64 {
	return p.lastPong
}
func (p *Peer) Host() []byte {
	return p.host
}
func (p *Peer) Port() uint16 {
	return p.port
}
func (p *Peer) Version() string {
	return p.version
}
func (p *Peer) Connected() *int32 {
	return &p.connected
}

// Setters
func (p *Peer) SetVersion(version string) {
	p.version = version
}

// Outputs any RLP encoded data to the peer
func (p *Peer) QueueMessage(msg *ethwire.Msg) {
	if atomic.LoadInt32(&p.connected) != 1 {
		return
	}
	p.outputQueue <- msg
}

func (p *Peer) writeMessage(msg *ethwire.Msg) {
	// Ignore the write if we're not connected
	if atomic.LoadInt32(&p.connected) != 1 {
		return
	}

	if !p.versionKnown {
		switch msg.Type {
		case ethwire.MsgHandshakeTy: // Ok
		default: // Anything but ack is allowed
			return
		}
	}

	peerlogger.DebugDetailf("(%v) <= %v %v\n", p.conn.RemoteAddr(), msg.Type, msg.Data)

	err := ethwire.WriteMessage(p.conn, msg)
	if err != nil {
		peerlogger.Debugln(" Can't send message:", err)
		// Stop the client if there was an error writing to it
		p.Stop()
		return
	}
}

// Outbound message handler. Outbound messages are handled here
func (p *Peer) HandleOutbound() {
	// The ping timer. Makes sure that every 2 minutes a ping is send to the peer
	pingTimer := time.NewTicker(pingPongTimer)
	serviceTimer := time.NewTicker(5 * time.Minute)

out:
	for {
		select {
		// Main message queue. All outbound messages are processed through here
		case msg := <-p.outputQueue:
			p.writeMessage(msg)
			p.lastSend = time.Now()

		// Ping timer
		case <-pingTimer.C:
			/*
				timeSince := time.Since(time.Unix(p.lastPong, 0))
				if !p.pingStartTime.IsZero() && p.lastPong != 0 && timeSince > (pingPongTimer+30*time.Second) {
					peerlogger.Infof("Peer did not respond to latest pong fast enough, it took %s, disconnecting.\n", timeSince)
					p.Stop()
					return
				}
			*/
			p.writeMessage(ethwire.NewMessage(ethwire.MsgPingTy, ""))
			p.pingStartTime = time.Now()

		// Service timer takes care of peer broadcasting, transaction
		// posting or block posting
		case <-serviceTimer.C:
			if p.caps&CapPeerDiscTy > 0 {
				msg := p.peersMessage()
				p.ethereum.BroadcastMsg(msg)
			}

		case <-p.quit:
			// Break out of the for loop if a quit message is posted
			break out
		}
	}

clean:
	// This loop is for draining the output queue and anybody waiting for us
	for {
		select {
		case <-p.outputQueue:
			// TODO
		default:
			break clean
		}
	}
}

// Inbound handler. Inbound messages are received here and passed to the appropriate methods
func (p *Peer) HandleInbound() {
	for atomic.LoadInt32(&p.disconnect) == 0 {

		// HMM?
		time.Sleep(50 * time.Millisecond)
		// Wait for a message from the peer
		msgs, err := ethwire.ReadMessages(p.conn)
		if err != nil {
			peerlogger.Debugln(err)
		}
		for _, msg := range msgs {
			peerlogger.DebugDetailf("(%v) => %v %v\n", p.conn.RemoteAddr(), msg.Type, msg.Data)

			switch msg.Type {
			case ethwire.MsgHandshakeTy:
				// Version message
				p.handleHandshake(msg)

				if p.caps.IsCap(CapPeerDiscTy) {
					p.QueueMessage(ethwire.NewMessage(ethwire.MsgGetPeersTy, ""))
				}

			case ethwire.MsgDiscTy:
				p.Stop()
				peerlogger.Infoln("Disconnect peer: ", DiscReason(msg.Data.Get(0).Uint()))
			case ethwire.MsgPingTy:
				// Respond back with pong
				p.QueueMessage(ethwire.NewMessage(ethwire.MsgPongTy, ""))
			case ethwire.MsgPongTy:
				// If we received a pong back from a peer we set the
				// last pong so the peer handler knows this peer is still
				// active.
				p.lastPong = time.Now().Unix()
				p.pingTime = time.Since(p.pingStartTime)
			case ethwire.MsgTxTy:
				// If the message was a transaction queue the transaction
				// in the TxPool where it will undergo validation and
				// processing when a new block is found
				for i := 0; i < msg.Data.Len(); i++ {
					tx := ethchain.NewTransactionFromValue(msg.Data.Get(i))
					p.ethereum.TxPool().QueueTransaction(tx)
				}
			case ethwire.MsgGetPeersTy:
				// Peer asked for list of connected peers
				p.pushPeers()
			case ethwire.MsgPeersTy:
				// Received a list of peers (probably because MsgGetPeersTy was send)
				data := msg.Data
				// Create new list of possible peers for the ethereum to process
				peers := make([]string, data.Len())
				// Parse each possible peer
				for i := 0; i < data.Len(); i++ {
					value := data.Get(i)
					peers[i] = unpackAddr(value.Get(0), value.Get(1).Uint())
				}

				// Connect to the list of peers
				p.ethereum.ProcessPeerList(peers)
			case ethwire.MsgGetTxsTy:
				// Get the current transactions of the pool
				txs := p.ethereum.TxPool().CurrentTransactions()
				// Get the RlpData values from the txs
				txsInterface := make([]interface{}, len(txs))
				for i, tx := range txs {
					txsInterface[i] = tx.RlpData()
				}
				// Broadcast it back to the peer
				p.QueueMessage(ethwire.NewMessage(ethwire.MsgTxTy, txsInterface))

			case ethwire.MsgGetBlockHashesTy:
				if msg.Data.Len() < 2 {
					peerlogger.Debugln("err: argument length invalid ", msg.Data.Len())
				}

				hash := msg.Data.Get(0).Bytes()
				amount := msg.Data.Get(1).Uint()

				hashes := p.ethereum.BlockChain().GetChainHashesFromHash(hash, amount)

				p.QueueMessage(ethwire.NewMessage(ethwire.MsgBlockHashesTy, ethutil.ByteSliceToInterface(hashes)))

			case ethwire.MsgGetBlocksTy:
				// Limit to max 300 blocks
				max := int(math.Min(float64(msg.Data.Len()), 300.0))
				var blocks []interface{}

				for i := 0; i < max; i++ {
					hash := msg.Data.Get(i).Bytes()
					block := p.ethereum.BlockChain().GetBlock(hash)
					if block != nil {
						blocks = append(blocks, block.Value().Raw())
					}
				}

				p.QueueMessage(ethwire.NewMessage(ethwire.MsgBlockTy, blocks))

			case ethwire.MsgBlockHashesTy:
				p.catchingUp = true

				blockPool := p.ethereum.blockPool

				foundCommonHash := false

				it := msg.Data.NewIterator()
				for it.Next() {
					hash := it.Value().Bytes()

					if blockPool.HasCommonHash(hash) {
						foundCommonHash = true

						break
					}

					blockPool.AddHash(hash)

					p.lastReceivedHash = hash

					p.lastBlockReceived = time.Now()
				}

				if foundCommonHash {
					p.FetchBlocks()
				} else {
					p.FetchHashes()
				}

			case ethwire.MsgBlockTy:
				p.catchingUp = true

				blockPool := p.ethereum.blockPool

				it := msg.Data.NewIterator()

				for it.Next() {
					block := ethchain.NewBlockFromRlpValue(it.Value())

					blockPool.SetBlock(block)

					p.lastBlockReceived = time.Now()
				}

				linked := blockPool.CheckLinkAndProcess(func(block *ethchain.Block) {
					p.ethereum.StateManager().Process(block, false)
				})

				if !linked {
					p.FetchBlocks()
				}
			}
		}
	}

	p.Stop()
}

func (self *Peer) FetchBlocks() {
	blockPool := self.ethereum.blockPool

	hashes := blockPool.Take(100, self)
	if len(hashes) > 0 {
		self.QueueMessage(ethwire.NewMessage(ethwire.MsgGetBlocksTy, ethutil.ByteSliceToInterface(hashes)))
	}
}

func (self *Peer) FetchHashes() {
	blockPool := self.ethereum.blockPool

	if self.td.Cmp(blockPool.td) >= 0 {
		peerlogger.Debugf("Requesting hashes from %x\n", self.lastReceivedHash)

		if !blockPool.HasLatestHash() {
			self.QueueMessage(ethwire.NewMessage(ethwire.MsgGetBlockHashesTy, []interface{}{self.lastReceivedHash, uint32(200)}))
		}
	}
}

// General update method
func (self *Peer) update() {
	serviceTimer := time.NewTicker(5 * time.Second)

out:
	for {
		select {
		case <-serviceTimer.C:
			if time.Since(self.lastBlockReceived) > 10*time.Second {
				self.catchingUp = false
			}
		case <-self.quit:
			break out
		}
	}

	serviceTimer.Stop()
}

func (p *Peer) Start() {
	peerHost, peerPort, _ := net.SplitHostPort(p.conn.LocalAddr().String())
	servHost, servPort, _ := net.SplitHostPort(p.conn.RemoteAddr().String())

	if p.inbound {
		p.host, p.port = packAddr(peerHost, peerPort)
	} else {
		p.host, p.port = packAddr(servHost, servPort)
	}

	err := p.pushHandshake()
	if err != nil {
		peerlogger.Debugln("Peer can't send outbound version ack", err)

		p.Stop()

		return
	}

	go p.HandleOutbound()
	// Run the inbound handler in a new goroutine
	go p.HandleInbound()
	// Run the general update handler
	go p.update()

	// Wait a few seconds for startup and then ask for an initial ping
	time.Sleep(2 * time.Second)
	p.writeMessage(ethwire.NewMessage(ethwire.MsgPingTy, ""))
	p.pingStartTime = time.Now()

}

func (p *Peer) Stop() {
	if atomic.AddInt32(&p.disconnect, 1) != 1 {
		return
	}

	close(p.quit)
	if atomic.LoadInt32(&p.connected) != 0 {
		p.writeMessage(ethwire.NewMessage(ethwire.MsgDiscTy, ""))
		p.conn.Close()
	}

	// Pre-emptively remove the peer; don't wait for reaping. We already know it's dead if we are here
	p.ethereum.RemovePeer(p)
}

func (p *Peer) pushHandshake() error {
	pubkey := p.ethereum.KeyManager().PublicKey()
	msg := ethwire.NewMessage(ethwire.MsgHandshakeTy, []interface{}{
		uint32(ProtocolVersion), uint32(0), []byte(p.version), byte(p.caps), p.port, pubkey[1:],
		p.ethereum.BlockChain().TD.Uint64(), p.ethereum.BlockChain().CurrentBlock.Hash(),
	})

	p.QueueMessage(msg)

	return nil
}

func (p *Peer) peersMessage() *ethwire.Msg {
	outPeers := make([]interface{}, len(p.ethereum.InOutPeers()))
	// Serialise each peer
	for i, peer := range p.ethereum.InOutPeers() {
		// Don't return localhost as valid peer
		if !net.ParseIP(peer.conn.RemoteAddr().String()).IsLoopback() {
			outPeers[i] = peer.RlpData()
		}
	}

	// Return the message to the peer with the known list of connected clients
	return ethwire.NewMessage(ethwire.MsgPeersTy, outPeers)
}

// Pushes the list of outbound peers to the client when requested
func (p *Peer) pushPeers() {
	p.QueueMessage(p.peersMessage())
}

func (p *Peer) handleHandshake(msg *ethwire.Msg) {
	c := msg.Data

	// Set pubkey
	p.pubkey = c.Get(5).Bytes()

	if p.pubkey == nil {
		peerlogger.Warnln("Pubkey required, not supplied in handshake.")
		p.Stop()
		return
	}

	usedPub := 0
	// This peer is already added to the peerlist so we expect to find a double pubkey at least once

	eachPeer(p.ethereum.Peers(), func(peer *Peer, e *list.Element) {
		if bytes.Compare(p.pubkey, peer.pubkey) == 0 {
			usedPub++
		}
	})

	if usedPub > 0 {
		peerlogger.Debugf("Pubkey %x found more then once. Already connected to client.", p.pubkey)
		p.Stop()
		return
	}

	if c.Get(0).Uint() != ProtocolVersion {
		peerlogger.Debugf("Invalid peer version. Require protocol: %d. Received: %d\n", ProtocolVersion, c.Get(0).Uint())
		p.Stop()
		return
	}

	// [PROTOCOL_VERSION, NETWORK_ID, CLIENT_ID, CAPS, PORT, PUBKEY]
	p.versionKnown = true

	// If this is an inbound connection send an ack back
	if p.inbound {
		p.port = uint16(c.Get(4).Uint())

		// Self connect detection
		pubkey := p.ethereum.KeyManager().PublicKey()
		if bytes.Compare(pubkey, p.pubkey) == 0 {
			p.Stop()

			return
		}

	}

	// Set the peer's caps
	p.caps = Caps(c.Get(3).Byte())

	// Get a reference to the peers version
	versionString := c.Get(2).Str()
	if len(versionString) > 0 {
		p.SetVersion(c.Get(2).Str())
	}

	// Get the td and last hash
	p.td = c.Get(6).BigInt()
	p.bestHash = c.Get(7).Bytes()
	p.lastReceivedHash = p.bestHash

	p.ethereum.PushPeer(p)
	p.ethereum.reactor.Post("peerList", p.ethereum.Peers())

	ethlogger.Infof("Added peer (%s) %d / %d (TD = %v ~ %x)\n", p.conn.RemoteAddr(), p.ethereum.Peers().Len(), p.ethereum.MaxPeers, p.td, p.bestHash)

	/*
		// Catch up with the connected peer
		if !p.ethereum.IsUpToDate() {
			peerlogger.Debugln("Already syncing up with a peer; sleeping")
			time.Sleep(10 * time.Second)
		}
	*/
	//p.SyncWithPeerToLastKnown()

	if p.td.Cmp(p.ethereum.BlockChain().TD) > 0 {
		p.ethereum.blockPool.AddHash(p.lastReceivedHash)
		p.FetchHashes()
	}

	peerlogger.Debugln(p)
}

func (p *Peer) String() string {
	var strBoundType string
	if p.inbound {
		strBoundType = "inbound"
	} else {
		strBoundType = "outbound"
	}
	var strConnectType string
	if atomic.LoadInt32(&p.disconnect) == 0 {
		strConnectType = "connected"
	} else {
		strConnectType = "disconnected"
	}

	return fmt.Sprintf("[%s] (%s) %v %s [%s]", strConnectType, strBoundType, p.conn.RemoteAddr(), p.version, p.caps)

}
func (p *Peer) SyncWithPeerToLastKnown() {
	p.catchingUp = false
	p.CatchupWithPeer(p.ethereum.BlockChain().CurrentBlock.Hash())
}

func (p *Peer) FindCommonParentBlock() {
	if p.catchingUp {
		return
	}

	p.catchingUp = true
	if p.blocksRequested == 0 {
		p.blocksRequested = 20
	}
	blocks := p.ethereum.BlockChain().GetChain(p.ethereum.BlockChain().CurrentBlock.Hash(), p.blocksRequested)

	var hashes []interface{}
	for _, block := range blocks {
		hashes = append(hashes, block.Hash())
	}

	msgInfo := append(hashes, uint64(len(hashes)))

	peerlogger.DebugDetailf("Asking for block from %x (%d total) from %s\n", p.ethereum.BlockChain().CurrentBlock.Hash(), len(hashes), p.conn.RemoteAddr().String())

	msg := ethwire.NewMessage(ethwire.MsgGetChainTy, msgInfo)
	p.QueueMessage(msg)
}
func (p *Peer) CatchupWithPeer(blockHash []byte) {
	if !p.catchingUp {
		// Make sure nobody else is catching up when you want to do this
		p.catchingUp = true
		msg := ethwire.NewMessage(ethwire.MsgGetChainTy, []interface{}{blockHash, uint64(100)})
		p.QueueMessage(msg)

		peerlogger.DebugDetailf("Requesting blockchain %x... from peer %s\n", p.ethereum.BlockChain().CurrentBlock.Hash()[:4], p.conn.RemoteAddr())

		msg = ethwire.NewMessage(ethwire.MsgGetTxsTy, []interface{}{})
		p.QueueMessage(msg)
	}
}

func (p *Peer) RlpData() []interface{} {
	return []interface{}{p.host, p.port, p.pubkey}
}

func packAddr(address, port string) ([]byte, uint16) {
	addr := strings.Split(address, ".")
	a, _ := strconv.Atoi(addr[0])
	b, _ := strconv.Atoi(addr[1])
	c, _ := strconv.Atoi(addr[2])
	d, _ := strconv.Atoi(addr[3])
	host := []byte{byte(a), byte(b), byte(c), byte(d)}
	prt, _ := strconv.Atoi(port)

	return host, uint16(prt)
}

func unpackAddr(value *ethutil.Value, p uint64) string {
	byts := value.Bytes()
	a := strconv.Itoa(int(byts[0]))
	b := strconv.Itoa(int(byts[1]))
	c := strconv.Itoa(int(byts[2]))
	d := strconv.Itoa(int(byts[3]))
	host := strings.Join([]string{a, b, c, d}, ".")
	port := strconv.Itoa(int(p))

	return net.JoinHostPort(host, port)
}
