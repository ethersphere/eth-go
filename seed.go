package eth

import (
	"net"

	"github.com/ethereum/eth-go/ethutil"
)

const (
	// seedTextFileUri   string = "http://www.ethereum.org/servers.poc3.txt"
	seedNodeAddress   = "poc-6.ethdev.com:30303"
	goSeedNodeAddress = "seed.bysh.me"
)

func WritePeers(path string, addresses []string) {
	if len(addresses) > 0 {
		data, _ := json.MarshalIndent(addresses, "", "    ")
		ethutil.WriteFile(path, data)
	}
}

func ReadPeers(path string) (ips []string, err error) {
	var data []byte
	data, err = ethutil.ReadAllFile(path)
	if err != nil {
		json.Unmarshal([]byte(data), &ips)
	}
	return
}

func Seed(path string, bootstrap bool, peerCallback func(net.Addr)) {
	var addr net.Addr
	i := 0
	ethlogger.Infoln("seeding peers")
	if len(path) > 0 {
		ips, err := ReadPeers(path)
		if err != nil && len(ips) > 0 {
			ethlogger.Debugln("known peers")
			for _, ip := range ips {
				addr, err = network.ParseAddress(ip)
				if err == nil {
					i++
					ethlogger.Debugln("known peer ", ip)
					peerCallback(addr)
				} else {
					ethlogger.Debugln("couldn't parse %v: %v", ip, err)
				}
			}
		}
	}
	if !bootstrap && i > 0 {
		return
	}
	// Eth-Go Bootstrapping
	ips, err = net.LookupIP(goSeedNodeAddress)
	if err == nil {
		ethlogger.Debugln("eth go seed node ", goSeedNodeAddress)
		for _, ip := range ips {
			addr, err = network.NewAddr(ip, 30303)
			if err == nil {
				ethlogger.Debugf("DNS Go peer: %v:30303", ip)
				peerCallback(addr)
			} else {
				ethlogger.Debugln("couldn't resolve %v: %v", ip, err)
			}
		}
	} else {
		ethlogger.Debugln("couldn't resolve %v: %v", ip, err)
	}

	// Official DNS Bootstrapping
	var nodes *net.SRV
	_, nodes, err = net.LookupSRV("eth", "tcp", "ethereum.org")
	if err == nil {
		ethlogger.Debugln("eth SRV seed node ethereum.org")
		for _, node := range nodes {
			addr, err = network.NewAddr(node.Target, int(node.Port))
			if err == nil {
				ethlogger.Debugln("DNS eth Peer:", addr)
				peerCallback(addr)
			} else {
				ethlogger.Debugln("couldn't resolve %v: %v", node.Target, err)
			}
		}
	} else {
		ethlogger.Debugln("couldn't look up eth srv: %v", err)
	}

	addr, err = network.ParseAddress(seedNodeAddress)
	if err == nil {
		ethlogger.Debugln("eth seed node ", seedNodeAddress)
		peerCallback(addr)
	} else {
		ethlogger.Debugln("couldn't parse %v: %v", seedNodeAddress, err)
	}
}
