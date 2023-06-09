package blockchain

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"

	"metechain/pkg/block"
	"metechain/pkg/logger"
	"metechain/pkg/storage/store"
	"metechain/pkg/transaction"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// setNonce set address nonce
func setNonce(s *state.StateDB, addr common.Address, nonce uint64) error {

	coAddr := addr

	sdbNonce := s.GetNonce(coAddr)
	if sdbNonce == 0 {
		sdbNonce = 1
	}
	if sdbNonce+1 != nonce {
		return fmt.Errorf("error nonce(%v) != dbNonce(%v)", nonce, sdbNonce+1)
	}

	s.SetNonce(coAddr, nonce)
	return nil
}

// setBalance set address balance
func setBalance(s *state.StateDB, addr common.Address, balance *big.Int) error {
	coAddr := addr
	s.SetBalance(coAddr, balance)
	return nil
}

// setAccount set the balance of the corresponding account
func setAccount(bc *Blockchain, tx *transaction.FinishedTransaction, defaultAmount ...uint64) error {
	hash := tx.Hash()

	if tx.Type == transaction.WithdrawToEthTransaction {
		DBTransaction := bc.db.NewTransaction()
		defer DBTransaction.Cancel()

		cmaddr := tx.Transaction.From

		kaddr, err := getBindingmeteAddress(DBTransaction, cmaddr.Hex())
		if err != nil {
			return err
		}

		fromBalance, err := bc.getAvailableBalance(kaddr)
		if err != nil {
			return err
		}

		// if fromBalance < tx.Transaction.Amount+tx.GasUsed {
		// 	return fmt.Errorf("fromBalance insufficient, fromBalance(%d)<amount(%d)+gas(%d) ", fromBalance, tx.Transaction.Amount, tx.GasUsed)
		// }

		if fromBalance.Cmp(new(big.Int).Add(tx.Amount, tx.GasUsed)) == -1 {
			return fmt.Errorf("fromBalance insufficient, fromBalance(%s)<amount(%s)+gas(%s) ", fromBalance.String(), tx.Amount.String(), tx.GasUsed.String())
		}

		toBalance, err := bc.getBalance(*tx.Transaction.To)
		if err != nil {
			return err
		}

		// if fromBalance < tx.Transaction.Amount+tx.GasUsed {
		// 	return fmt.Errorf("not sufficient funds,hash:%s,from balance(%d) < amount(%d) + gas(%d)",
		// 		hex.EncodeToString(hash), fromBalance, tx.Transaction.Amount, tx.GasUsed)
		// }
		// fromBalance -= tx.Transaction.Amount + tx.GasUsed

		fromBalance.Sub(fromBalance, new(big.Int).Add(tx.Amount, tx.GasUsed))

		// if MAXUINT64-toBalance-tx.GasUsed < tx.Transaction.Amount {
		// 	return fmt.Errorf("amount is too large,hash:%s,max int64(%d)-balance(%d)-gas(%d) < amount(%d)", hash, MAXUINT64, toBalance, tx.GasUsed, tx.Transaction.Amount)
		// }
		// toBalance += tx.Transaction.Amount
		toBalance.Add(toBalance, tx.Amount)

		err = setBalance(bc.sdb, *kaddr, fromBalance)
		if err != nil {
			return err
		}
		err = setBalance(bc.sdb, *tx.Transaction.To, toBalance)
		if err != nil {
			return err
		}

		return nil
	}

	from, to := tx.Transaction.From, tx.Transaction.To

	if bytes.Equal(from.Bytes(), to.Bytes()) == true || tx.Transaction.IsEvmContractTransaction() {
		hash := tx.Hash()
		fromBalance, err := bc.getBalance(*from)
		if err != nil {
			return err
		}

		if fromBalance.Cmp(tx.GasUsed) == -1 {
			return fmt.Errorf("not sufficient funds,hash:%s,from balance(%s) < gas(%s)",
				hex.EncodeToString(hash), fromBalance.String(), tx.GasUsed.String())
		}

		// fromBalance -= tx.GasUsed
		fromBalance.Sub(fromBalance, tx.GasUsed)
		err = setBalance(bc.sdb, *from, fromBalance)
		if err != nil {
			return err
		}

	} else {
		fromBalance, err := bc.getBalance(*from)
		if err != nil {
			return err
		}
		toBalance, err := bc.getBalance(*to)
		if err != nil {
			return err
		}

		if fromBalance.Cmp(new(big.Int).Add(tx.Amount, tx.GasUsed)) == -1 {
			return fmt.Errorf("not sufficient funds,hash:%s,from balance(%s) < amount(%s) + gas(%s)",
				hex.EncodeToString(hash), fromBalance, tx.Amount, tx.GasUsed)
		}
		fromBalance.Sub(fromBalance, new(big.Int).Add(tx.Amount, tx.GasUsed))

		toBalance.Add(toBalance, tx.Amount)

		err = setBalance(bc.sdb, *from, fromBalance)
		if err != nil {
			return err
		}
		err = setBalance(bc.sdb, *to, toBalance)
		if err != nil {
			return err
		}
	}
	return nil
}

// setToAccount set the balance of the specified account
func (bc *Blockchain) setToAccount(block *block.Block, tx *transaction.Transaction) error {
	toBalance, err := bc.getBalance(*tx.To)
	if err != nil {
		return err
	}

	return setBalance(bc.sdb, *tx.To, new(big.Int).Add(toBalance, tx.Amount))
}

// setMinerFee set the balance of the miner's account
func setMinerFee(bc *Blockchain, to common.Address, gasAmount *big.Int) error {
	toBalance, err := bc.getBalance(to)
	if err != nil {
		return err
	}

	return setBalance(bc.sdb, to, new(big.Int).Add(toBalance, gasAmount))
}

// GetBalance get the balance of the address
func (bc *Blockchain) GetBalance(address *common.Address) (*big.Int, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.getBalance(*address)
}

// getBalance get the balance of the address
func (bc *Blockchain) getBalance(address common.Address) (*big.Int, error) {
	coAddr := address
	balance := bc.sdb.GetBalance(coAddr)
	return balance, nil
}

// GetNonce get the nonce of the address
func (bc *Blockchain) GetNonce(address *common.Address) (uint64, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	return bc.getNonce(*address)
}

// getNonce get the nonce of the address
func (bc *Blockchain) getNonce(address common.Address) (uint64, error) {
	var initNonce uint64 = 1

	coAddr := address

	n := bc.sdb.GetNonce(coAddr)
	if n == 0 {
		return initNonce, nil
	}

	return n, nil
}

func getSnapRootLock(db store.DB) (common.Hash, error) {
	var mu sync.RWMutex
	mu.RLock()
	defer mu.RUnlock()

	sr, err := db.Get(SnapRootKey)
	if err == store.NotExist {
		return common.Hash{}, nil
	} else if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(sr), nil
}

// getSnapRoot Get the SnapRoot of the DB
func getSnapRoot(db store.DB) (common.Hash, error) {
	sr, err := db.Get(SnapRootKey)
	if err == store.NotExist {
		return common.Hash{}, nil
	} else if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(sr), nil
}

// factCommit writes the state to the underlying in-memory trie database
func factCommit(sdb *state.StateDB, deleteEmptyObjects bool) (common.Hash, error) {
	ha, err := sdb.Commit(deleteEmptyObjects)
	if err != nil {
		logger.Error("stateDB commit error")
		return common.Hash{}, err
	}
	triDB := sdb.Database().TrieDB()
	err = triDB.Commit(ha, true, nil)
	if err != nil {
		logger.Error("triDB commit error")
		return ha, err
	}

	return ha, err
}

// factCommit writes the state to the underlying in-memory trie database
func (bc *Blockchain) FactCommit(deleteEmptyObjects bool) (common.Hash, error) {
	ha, err := bc.sdb.Commit(deleteEmptyObjects)
	if err != nil {
		logger.Error("stateDB commit error")
		return common.Hash{}, err
	}
	triDB := bc.sdb.Database().TrieDB()
	err = triDB.Commit(ha, true, nil)
	if err != nil {
		logger.Error("triDB commit error")
		return ha, err
	}
	return ha, err
}

// factCommit writes the state to the underlying in-memory trie database
func fakeCommit(sdb *state.StateDB, deleteEmptyObjects bool) (common.Hash, error) {
	sdbc := sdb.Copy()
	ha, err := sdbc.Commit(deleteEmptyObjects)
	if err != nil {
		logger.Error("stateDB commit error")
		return common.Hash{}, err
	}
	triDB := sdbc.Database().TrieDB()
	err = triDB.Commit(ha, true, nil)
	if err != nil {
		logger.Error("triDB commit error")
		return ha, err
	}

	return ha, err
}
