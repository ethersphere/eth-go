package ethp2p

import (
	"container/list"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/eth-go/ethlog"
)

const (
	processReapingTimeout        = 60
	seedTextFileUri       string = "http://www.ethereum.org/servers.poc3.txt"
	seedNodeAddress              = "54.76.56.74:30303"
)

func eachPeer(peers *list.List, callback func(*Peer, *list.Element)) {
	// Loop thru the peers and close them (if we had them)
	for e := peers.Front(); e != nil; e = e.Next() {
		if peer, ok := e.Value.(*Peer); ok {
			callback(peer, e)
		}
	}
}

type Server struct {
	peerMut sync.Mutex

	nat       NAT
	listening bool
	peers     *list.List
	MaxPeers  int
	Port      string
	quit      chan bool
}

var logger = ethlog.NewLogger("P2P")

func New(usePnp bool) *Server {
	serv := &Server{quit: make(chan bool), MaxPeers: 16}

	if usePnp {
		nat, err := Discover()
		if err != nil {
			logger.Debugln("UPnP failed", err)
		} else {
			serv.nat = nat
		}
	}

	return serv
}

func (self *Server) Start(port string, seed bool) {
	self.Port = port

	// Bind to addr and port
	ln, err := net.Listen("tcp", ":"+self.Port)
	if err != nil {
		logger.Warnf("Port %s in use. Connection listening disabled. Acting as client", self.Port)
		self.listening = false
	} else {
		self.listening = true
		// Starting accepting connections
		logger.Infoln("Ready and accepting connections")
		// Start the peer handler
		go self.peerHandler(ln)
	}

	if self.nat != nil {
		go self.upnpUpdateThread()
	}

	// Start the reaping processes
	go self.ReapDeadPeerHandler()

	if seed {
		self.Seed()
	}
	logger.Infoln("Server started")
}

func (s *Server) peerHandler(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Debugln(err)

			continue
		}

		go s.AddPeer(conn)
	}
}

func (s *Server) upnpUpdateThread() {
	// Go off immediately to prevent code duplication, thereafter we renew
	// lease every 15 minutes.
	timer := time.NewTimer(5 * time.Minute)
	lport, _ := strconv.ParseInt(s.Port, 10, 16)
	first := true
out:
	for {
		select {
		case <-timer.C:
			var err error
			_, err = s.nat.AddPortMapping("TCP", int(lport), int(lport), "eth listen port", 20*60)
			if err != nil {
				logger.Debugln("can't add UPnP port mapping:", err)
				break out
			}
			if first && err == nil {
				_, err = s.nat.GetExternalAddress()
				if err != nil {
					logger.Debugln("UPnP can't get external address:", err)
					continue out
				}
				first = false
			}
			timer.Reset(time.Minute * 15)
		case <-s.quit:
			break out
		}
	}

	timer.Stop()

	if err := s.nat.DeletePortMapping("TCP", int(lport), int(lport)); err != nil {
		logger.Debugln("unable to remove UPnP port mapping:", err)
	} else {
		logger.Debugln("succesfully disestablished UPnP port mapping")
	}
}

func (s *Server) ReapDeadPeerHandler() {
	reapTimer := time.NewTicker(processReapingTimeout * time.Second)

	for {
		select {
		case <-reapTimer.C:
			eachPeer(s.peers, func(p *Peer, e *list.Element) {
				if atomic.LoadInt32(&p.disconnect) == 1 || (p.inbound && (time.Now().Unix()-p.lastPong) > int64(5*time.Minute)) {
					s.removePeerElement(e)
				}
			})
		}
	}
}

func (s *Server) Seed() {
	logger.Debugln("Retrieving seed nodes")

	// Eth-Go Bootstrapping
	ips, er := net.LookupIP("seed.bysh.me")
	if er == nil {
		peers := []string{}
		for _, ip := range ips {
			node := fmt.Sprintf("%s:%d", ip.String(), 30303)
			logger.Debugln("Found DNS Go Peer:", node)
			peers = append(peers, node)
		}
		s.ProcessPeerList(peers)
	}

	// Official DNS Bootstrapping
	_, nodes, err := net.LookupSRV("eth", "tcp", "ethereum.org")
	if err == nil {
		peers := []string{}
		// Iterate SRV nodes
		for _, n := range nodes {
			target := n.Target
			port := strconv.Itoa(int(n.Port))
			// Resolve target to ip (Go returns list, so may resolve to multiple ips?)
			addr, err := net.LookupHost(target)
			if err == nil {
				for _, a := range addr {
					// Build string out of SRV port and Resolved IP
					peer := net.JoinHostPort(a, port)
					logger.Debugln("Found DNS Bootstrap Peer:", peer)
					peers = append(peers, peer)
				}
			} else {
				logger.Debugln("Couldn't resolve :", target)
			}
		}
		// Connect to Peer list
		s.ProcessPeerList(peers)
	}

	// XXX tmp
	s.ConnectToPeer(seedNodeAddress)
}

func (s *Server) ProcessPeerList(addrs []string) {
	for _, addr := range addrs {
		// TODO Probably requires some sanity checks
		s.ConnectToPeer(addr)
	}
}

func (s *Server) AddPeer(conn net.Conn) {
	peer := NewPeer(conn, s, true)

	if peer != nil {
		if s.peers.Len() < s.MaxPeers {
			peer.Start()
		} else {
			logger.Debugf("Max connected peers reached. Not adding incoming peer.")
		}
	}
}

func (s *Server) ConnectToPeer(addr string) error {
	if s.peers.Len() < s.MaxPeers {
		var alreadyConnected bool

		ahost, _, _ := net.SplitHostPort(addr)
		var chost string

		ips, err := net.LookupIP(ahost)

		if err != nil {
			return err
		} else {
			// If more then one ip is available try stripping away the ipv6 ones
			if len(ips) > 1 {
				var ipsv4 []net.IP
				// For now remove the ipv6 addresses
				for _, ip := range ips {
					if strings.Contains(ip.String(), "::") {
						continue
					} else {
						ipsv4 = append(ipsv4, ip)
					}
				}
				if len(ipsv4) == 0 {
					return fmt.Errorf("[SERV] No IPV4 addresses available for hostname")
				}

				// Pick a random ipv4 address, simulating round-robin DNS.
				rand.Seed(time.Now().UTC().UnixNano())
				i := rand.Intn(len(ipsv4))
				chost = ipsv4[i].String()
			} else {
				if len(ips) == 0 {
					return fmt.Errorf("[SERV] No IPs resolved for the given hostname")
					return nil
				}
				chost = ips[0].String()
			}
		}

		eachPeer(s.peers, func(p *Peer, v *list.Element) {
			if p.conn == nil {
				return
			}
			phost, _, _ := net.SplitHostPort(p.conn.RemoteAddr().String())

			if phost == chost {
				alreadyConnected = true
				//ethlogger.Debugf("Peer %s already added.\n", chost)
				return
			}
		})

		if alreadyConnected {
			return nil
		}

		NewOutboundPeer(addr, s)
	}

	return nil
}

func (s *Server) removePeerElement(e *list.Element) {
	s.peerMut.Lock()
	defer s.peerMut.Unlock()

	s.peers.Remove(e)

	//s.reactor.Post("peerList", s.peers)
}
