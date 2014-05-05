package ethpub

import (
	"github.com/ethereum/eth-go/ethchain"
	"github.com/ethereum/eth-go/ethutil"
)

type PEthereum struct {
	stateManager *ethchain.StateManager
	blockChain   *ethchain.BlockChain
	txPool       *ethchain.TxPool
}

func NewPEthereum(sm *ethchain.StateManager, bc *ethchain.BlockChain, txp *ethchain.TxPool) *PEthereum {
	return &PEthereum{
		sm,
		bc,
		txp,
	}
}

func (lib *PEthereum) GetBlock(hexHash string) *PBlock {
	hash := ethutil.FromHex(hexHash)

	block := lib.blockChain.GetBlock(hash)

	var blockInfo *PBlock

	if block != nil {
		blockInfo = &PBlock{Number: int(block.BlockInfo().Number), Hash: ethutil.Hex(block.Hash())}
	} else {
		blockInfo = &PBlock{Number: -1, Hash: ""}
	}

	return blockInfo
}

func (lib *PEthereum) GetKey() *PKey {
	keyPair, err := ethchain.NewKeyPairFromSec(ethutil.Config.Db.GetKeys()[0].PrivateKey)
	if err != nil {
		return nil
	}

	return NewPKey(keyPair)
}

func (lib *PEthereum) GetStateObject(address string) *PStateObject {
	stateObject := lib.stateManager.ProcState().GetContract(ethutil.FromHex(address))
	if stateObject != nil {
		return NewPStateObject(stateObject)
	}

	// See GetStorage for explanation on "nil"
	return NewPStateObject(nil)
}

func (lib *PEthereum) GetStorage(address, storageAddress string) string {
	return lib.GetStateObject(address).GetStorage(storageAddress)
}

func (lib *PEthereum) GetTxCount(address string) int {
	return lib.GetStateObject(address).Nonce()
}

func (lib *PEthereum) IsContract(address string) bool {
	return lib.GetStateObject(address).IsContract()
}

func (lib *PEthereum) SecretToAddress(key string) string {
	pair, err := ethchain.NewKeyPairFromSec(ethutil.FromHex(key))
	if err != nil {
		return ""
	}

	return ethutil.Hex(pair.Address())
}

func (lib *PEthereum) Transact(key, recipient, valueStr, gasStr, gasPriceStr, dataStr string) (*PReceipt, error) {
	return lib.createTx(key, recipient, valueStr, gasStr, gasPriceStr, dataStr, "")
}

func (lib *PEthereum) Create(key, valueStr, gasStr, gasPriceStr, initStr, bodyStr string) (*PReceipt, error) {
	return lib.createTx(key, "", valueStr, gasStr, gasPriceStr, initStr, bodyStr)
}

func (lib *PEthereum) createTx(key, recipient, valueStr, gasStr, gasPriceStr, initStr, scriptStr string) (*PReceipt, error) {
	var hash []byte
	var contractCreation bool
	if len(recipient) == 0 {
		contractCreation = true
	} else {
		hash = ethutil.FromHex(recipient)
	}

	keyPair, err := ethchain.NewKeyPairFromSec([]byte(ethutil.FromHex(key)))
	if err != nil {
		return nil, err
	}

	value := ethutil.Big(valueStr)
	gas := ethutil.Big(gasStr)
	gasPrice := ethutil.Big(gasPriceStr)
	var tx *ethchain.Transaction
	// Compile and assemble the given data
	if contractCreation {
		var initScript, mainScript []byte
		var err error
		if ethutil.IsHex(initStr) {
			initScript = ethutil.FromHex(initStr[2:])
		} else {
			initScript, err = ethutil.Compile(initStr)
			if err != nil {
				return nil, err
			}
		}

		if ethutil.IsHex(scriptStr) {
			mainScript = ethutil.FromHex(scriptStr[2:])
		} else {
			mainScript, err = ethutil.Compile(scriptStr)
			if err != nil {
				return nil, err
			}
		}

		tx = ethchain.NewContractCreationTx(value, gas, gasPrice, mainScript, initScript)
	} else {
		// Just in case it was submitted as a 0x prefixed string
		if len(initStr) > 0 && initStr[0:2] == "0x" {
			initStr = initStr[2:len(initStr)]
		}
		tx = ethchain.NewTransactionMessage(hash, value, gas, gasPrice, ethutil.FromHex(initStr))
	}

	acc := lib.stateManager.GetAddrState(keyPair.Address())
	tx.Nonce = acc.Nonce
	tx.Sign(keyPair.PrivateKey)
	lib.txPool.QueueTransaction(tx)

	if contractCreation {
		ethutil.Config.Log.Infof("Contract addr %x", tx.CreationAddress())
	} else {
		ethutil.Config.Log.Infof("Tx hash %x", tx.Hash())
	}

	return NewPReciept(contractCreation, tx.CreationAddress(), tx.Hash(), keyPair.Address()), nil
}