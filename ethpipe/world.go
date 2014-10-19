package ethpipe

import (
	"github.com/ethereum/eth-go/ethstate"
	"github.com/ethereum/eth-go/p2p"
)

type World struct {
	pipe *Pipe
	cfg  *Config
}

func NewWorld(pipe *Pipe) *World {
	world := &World{pipe, nil}
	world.cfg = &Config{pipe}

	return world
}

func (self *Pipe) World() *World {
	return self.world
}

func (self *World) State() *ethstate.State {
	return self.pipe.obj.StateManager().CurrentState()
}

func (self *World) Get(addr []byte) *Object {
	return &Object{self.State().GetStateObject(addr)}
}

func (self *World) SafeGet(addr []byte) *Object {
	return &Object{self.safeGet(addr)}
}

func (self *World) safeGet(addr []byte) *ethstate.StateObject {
	object := self.State().GetStateObject(addr)
	if object == nil {
		object = ethstate.NewStateObject(addr)
	}

	return object
}

func (self *World) Coinbase() *ethstate.StateObject {
	return nil
}

func (self *World) IsMining() bool {
	return self.pipe.obj.IsMining()
}

func (self *World) IsListening() bool {
	return self.pipe.obj.IsListening()
}

func (self *World) Peers() []*p2p.Peer {
	return self.pipe.obj.Peers()
}

func (self *World) Config() *Config {
	return self.cfg
}
