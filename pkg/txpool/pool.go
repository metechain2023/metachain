package txpool

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	_ "metechain/pkg/crypto/sigs/ed25519"
	_ "metechain/pkg/crypto/sigs/secp"
	"metechain/pkg/transaction"

	"github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"
)

const (
	PendingLimit = 100
	poolCap      = 10000
	timesub      = 100
)

type IBlockchain interface {
	GetNonce(*common.Address) (uint64, error)
	GetAvailableBalance(*common.Address) (*big.Int, error)

	GetBindingmeteAddress(ethAddr string) (*common.Address, error)
}

// Pool is a temporary storage pool for unchained transactions.
// Transactions are sorted by GasCap and Nonce in the pool and waiting to be
// uploaded to the chain.
type Pool struct {
	qlock sync.Mutex
	q     *orderlyQueue

	bc         IBlockchain
	logger     *zap.Logger
	pendingBuf []transaction.SignedTransaction
}

// NewPool Create transaction pool
func NewPool(cfg Config) (*Pool, error) {
	if cfg.BlockChain == nil {
		return nil, fmt.Errorf("bc cannot be empty")
	}

	p := &Pool{
		bc:         cfg.BlockChain,
		q:          newQueue(),
		pendingBuf: make([]transaction.SignedTransaction, PendingLimit, PendingLimit),
	}

	if cfg.Logger != nil {
		p.logger = cfg.Logger
	} else {
		p.logger = zap.NewNop()
	}

	return p, nil
}

// Add to add a new transaction to the pool, outdated transactions
// will return an error
func (p *Pool) Add(st *transaction.SignedTransaction) error {
	if st.Type == transaction.TransferTransaction {
		if len(st.Input) > 0 {
			return fmt.Errorf("Unsupported Token transaction currently,input: %v", string(st.Input))
		}
	}
	// if st.GasLimit*st.GasPrice < blockchain.MINGASLIMIT || st.GasLimit*st.GasPrice > blockchain.MAXGASLIMIT {
	// 	return fmt.Errorf("gas is too small or too big,gas limit:%d gas price:%d", st.GasLimit, st.GasPrice)
	// }

	if err := st.VerifySign(); err != nil {
		return err
	}

	p.qlock.Lock()
	defer p.qlock.Unlock()
	p.logger.Debug("add tx", zap.String("transaction", st.String()))

	return p.add(st)
}

func (p *Pool) AddList(stList []transaction.SignedTransaction) []error {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	var eList []error
	for i := 0; i < len(stList); i++ {
		if err := stList[i].VerifySign(); err != nil {
			err = fmt.Errorf("transaction:%s,error:%v", stList[i].String(), err)
			eList = append(eList, err)
			continue
		}

		if err := p.add(&stList[i]); err != nil {
			err = fmt.Errorf("transaction:%s,error:%v", stList[i].String(), err)
			eList = append(eList, err)
		}
	}

	return eList
}

func (p *Pool) add(st *transaction.SignedTransaction) error {
	if p.q.len() >= poolCap {
		return fmt.Errorf("pool is full,please try again later")
	}

	// Check if the nonce of the transaction is required
	if err := p.geCallerNonce(st.Caller(), st.GetNonce()); err != nil {
		return err
	}

	p.q.push(*st)
	//p.logger.Debug("success add", zap.String("transaction", st.String()))
	return nil
}

func (p *Pool) geCallerNonce(caller *common.Address, nonce uint64) error {
	lastNonce, _ := p.bc.GetNonce(caller)
	if nonce < lastNonce {
		return fmt.Errorf("transaction.nonce must be greater than the chain nonce : transaction nonce(%d) chain nonce (%d)", nonce, lastNonce)
	}
	return nil
}

type traderInfo struct {
	availableBalance *big.Int
	//lockBalance      *big.Int
	nextNonce uint64
	//sigleLockBalance *big.Int
	sigleExist bool
}

// String
func (t *traderInfo) String() string {
	// return fmt.Sprintf("avalible balance %d,lock balance %d,next nonce:%d",
	// 	t.availableBalance, t.lockBalance, t.nextNonce)
	return fmt.Sprintf("avalible balance %d,next nonce:%d",
		t.availableBalance, t.nextNonce)
}

// Pending returns the transaction that can be packaged
func (p *Pool) Pending() ([]transaction.SignedTransaction, error) {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	traderBuffer := make(map[string]traderInfo)
	//unreadyBuffer := make([]transaction.SignedTransaction, 0)
	i, l := 0, p.q.len()

	var badTxIdxs []int
	a := 0
	for ; a < PendingLimit && i < l; i++ {
		idx := l - 1 - i
		st := p.q.stList[idx]
		if n := p.compareNonce(traderBuffer, st.Caller(), st.GetNonce()); n < 0 {
			traderInfo, err := p.getTraderInfo(traderBuffer, st.Caller(), nil)
			if err != nil {
				p.logger.Error("getTraderInfo", zap.String("address", st.Caller().String()), zap.Error(err))
				continue
			}
			p.logger.Error("compare nonce", zap.String("trader", traderInfo.String()),
				zap.String("transaction", st.Transaction.String()))
			badTxIdxs = append(badTxIdxs, idx)
			continue
		} else if n > 0 {
			//unreadyBuffer = append(unreadyBuffer, st)
			traderInfo, err := p.getTraderInfo(traderBuffer, st.Caller(), nil)
			if err != nil {
				p.logger.Error("getTraderInfo", zap.String("address", st.Caller().String()), zap.Error(err))
				continue
			}
			p.logger.Error("compare nonce", zap.String("trader", traderInfo.String()),
				zap.String("transaction", st.Transaction.String()))
			continue
		}

		if err := p.transactionPreCalculated(traderBuffer, st.Transaction); err != nil {
			p.logger.Error("pre-calculated", zap.Error(err), zap.String("transaction", st.Transaction.String()))
			badTxIdxs = append(badTxIdxs, idx)
			continue
		}
		p.pendingBuf[a] = st
		a++
		p.logger.Debug("success pending", zap.String("transaction", st.Transaction.String()))
	}

	for _, idx := range badTxIdxs {
		p.q.remove(idx)
	}

	return p.pendingBuf[:a], nil
}

func (p *Pool) transactionPreCalculated(traderBuf map[string]traderInfo, tx transaction.Transaction) error {
	switch tx.Type {
	case transaction.TransferTransaction:
		return p.transferTransactionPreCalculated(traderBuf, tx)

	case transaction.EvmContractTransaction, transaction.EvmmeteTransaction:
		return p.pledgeTrasnactionPreCalculated(traderBuf, tx)

	case transaction.WithdrawToEthTransaction:
		return p.withdrawTrasnactionPreCalculated(traderBuf, tx)
	default:
		return fmt.Errorf("unknown transaction type:%d", tx.Type)
	}
}

func (p *Pool) transferTransactionPreCalculated(traderBuf map[string]traderInfo, tx transaction.Transaction) error {
	caller, err := p.getTraderInfo(traderBuf, tx.Caller(), nil)
	if err != nil {
		return err
	}

	receiver, err := p.getTraderInfo(traderBuf, tx.Receiver(), nil)
	if err != nil {
		return err
	}

	amountAndGas := new(big.Int).Add(tx.Amount, tx.GasFeeCap)

	if caller.availableBalance.Cmp(amountAndGas) == -1 {
		return fmt.Errorf(err.Error()+":caller avalible balance(%s) - amount and gas(%s)",
			caller.availableBalance, amountAndGas)
	}

	caller.nextNonce++
	caller.availableBalance = new(big.Int).Sub(caller.availableBalance, amountAndGas)
	receiver.availableBalance = new(big.Int).Add(tx.Amount, receiver.availableBalance)

	traderBuf[tx.Caller().String()] = caller
	traderBuf[tx.Receiver().String()] = receiver
	return nil
}

func (p *Pool) pledgeTrasnactionPreCalculated(traderBuf map[string]traderInfo, tx transaction.Transaction) error {
	caller, err := p.getTraderInfo(traderBuf, tx.Caller(), nil)
	if err != nil {
		return err
	}

	if caller.availableBalance.Cmp(tx.Amount) == -1 {
		return fmt.Errorf("available balance(%d) < lock amount(%d)", caller.availableBalance, tx.Amount)
	}

	avlBalance := new(big.Int).Sub(caller.availableBalance, tx.Amount)

	if avlBalance.Cmp(tx.GasCap()) == -1 {
		return fmt.Errorf(err.Error()+": availbale balance(%s) - gas fee(%s)", avlBalance, tx.GasCap())
	}

	avlBalance = avlBalance.Sub(avlBalance, tx.GasCap())

	caller.nextNonce++
	caller.availableBalance = avlBalance
	traderBuf[tx.Caller().String()] = caller

	return nil
}

func (p *Pool) getTraderInfo(traderBuf map[string]traderInfo, trader, receiver *common.Address) (traderInfo, error) {
	traderInfo, ok := traderBuf[trader.String()]
	if ok && receiver == nil {
		return traderInfo, nil
	}

	var err error
	if !ok {
		traderInfo.availableBalance, err = p.bc.GetAvailableBalance(trader)
		if err != nil {
			return traderInfo, err
		}

		traderInfo.nextNonce, err = p.bc.GetNonce(trader)
		if err != nil {
			return traderInfo, err
		}
	}

	if receiver != nil && !traderInfo.sigleExist {

		traderInfo.sigleExist = true
	}

	traderBuf[trader.String()] = traderInfo

	return traderInfo, nil
}

// if nonce > nextnonce return 1
//    nonce = nextnonce return 0
//    nonce < nextnonce return -1
func (p *Pool) compareNonce(traderBuf map[string]traderInfo, trader *common.Address, nonce uint64) int {
	traderInfo, err := p.getTraderInfo(traderBuf, trader, nil)
	if err != nil {
		p.logger.Error("compareNonce", zap.Error(err))
	}

	if nonce < traderInfo.nextNonce {
		return -1
	} else if nonce > traderInfo.nextNonce {
		return 1
	}

	return 0
}

// FilterTransaction filter incoming transactions
func (p *Pool) FilterTransaction(stList []transaction.SignedTransaction) {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	for _, st := range stList {
		rstList, ok := p.q.rstBuffer[st.Caller().String()]
		if !ok {
			continue
		}

		for _, rst := range rstList {
			if st.GetNonce() == rst.nonce {
				p.q.remove(rst.idx)
			}
		}
	}

	p.cacheOutSignedTransaction()
}

// TransctionCacheOut  Eliminate transactions in the transaction pool
func (p *Pool) cacheOutSignedTransaction() error {
	for k, v := range p.q.timeBuffer {
		if v > time.Now().Unix()-3*timesub {
			continue
		}

		delete(p.q.timeBuffer, k)

		caller, nonce, err := parseKey(k)
		if err != nil {
			p.logger.Error("CacheOutSignedTransaction,parseKey", zap.Error(err))
			continue
		}

		// addr, err := address.NewAddrFromString(caller)
		addr := common.HexToAddress(caller)
		if err != nil {
			p.logger.Error("CacheOutSignedTransaction,NewAddrFromString", zap.Error(err))
			continue
		}

		idx, ok := p.findSignedTransactionIdx(addr, nonce)
		if !ok {
			continue
		}

		p.q.remove(idx)
		p.logger.Debug("remove tx", zap.String("addr", addr.String()), zap.Uint64("nonce", nonce))
	}

	for p.q.len() > poolCap/3*2 {
		p.logger.Debug("remove tx", zap.String("addr", p.q.stList[0].Caller().String()), zap.Uint64("nonce", p.q.stList[0].GetNonce()))
		p.q.remove(0)
	}

	return nil
}

func (p *Pool) findSignedTransactionIdx(addr common.Address, nonce uint64) (int, bool) {
	list, ok := p.q.rstBuffer[addr.String()]
	if !ok {
		return -1, false
	}

	for _, rst := range list {
		if rst.nonce == nonce {
			if p.q.len() <= rst.idx {
				break
			}

			return rst.idx, true
		}
	}
	return -1, false
}

func (p *Pool) FindSignedTransaction(addr *common.Address, nonce uint64) (*transaction.SignedTransaction, bool) {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	list, ok := p.q.rstBuffer[addr.String()]
	if !ok {
		return nil, false
	}

	for _, rst := range list {
		if rst.nonce == nonce {
			if p.q.len() <= rst.idx {
				break
			}

			return &p.q.stList[rst.idx], true
		}
	}

	return nil, false
}

func (p *Pool) CopySignedTransactions() []transaction.SignedTransaction {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	stc := make([]transaction.SignedTransaction, p.q.len())

	copy(stc, p.q.stList)
	return stc
}

func (p *Pool) GetTxByHash(hash string) (*transaction.SignedTransaction, error) {
	p.qlock.Lock()
	defer p.qlock.Unlock()

	addrStr, nonce, err := p.q.getAddrAndNonceByHash(hash)
	if err != nil {
		return nil, err
	}
	// addr, err := address.NewAddrFromString(addrStr)
	// if err != nil {
	// 	return nil, err
	// }
	addr := common.HexToAddress(addrStr)

	idx, ok := p.findSignedTransactionIdx(addr, nonce)
	if !ok {
		return nil, fmt.Errorf("not exist")
	}

	if idx > len(p.q.stList) || p.q.stList[idx].HashToString() != hash {
		return nil, fmt.Errorf("not exist")
	}

	return &p.q.stList[idx], nil
}

func (p *Pool) getwithdrawTraderInfo(traderBuf map[string]traderInfo, trader *common.Address) (traderInfo, error) {
	var trdinfo traderInfo
	var err error

	// cmaddr, err := trader.NewCommonAddr()
	cmaddr := trader
	if err != nil {
		return trdinfo, err
	}

	kaddr, err := p.bc.GetBindingmeteAddress(cmaddr.String())
	if err != nil {
		return trdinfo, err
	}

	trdinfo.availableBalance, err = p.bc.GetAvailableBalance(kaddr)
	if err != nil {
		return trdinfo, err
	}

	trdinfo.nextNonce, err = p.bc.GetNonce(trader)
	if err != nil {
		return trdinfo, err
	}
	traderBuf[trader.String()] = trdinfo

	return trdinfo, nil
}

func (p *Pool) withdrawTrasnactionPreCalculated(traderBuf map[string]traderInfo, tx transaction.Transaction) error {
	caller, err := p.getwithdrawTraderInfo(traderBuf, tx.Caller())
	if err != nil {
		return err
	}

	amountAndGas := new(big.Int).Add(tx.Amount, tx.GasCap())

	if caller.availableBalance.Cmp(amountAndGas) == -1 {
		return fmt.Errorf(err.Error()+":caller avalible balance(%s) - amount and gas(%s)",
			caller.availableBalance, amountAndGas)
	}

	callerBal := new(big.Int).Sub(caller.availableBalance, amountAndGas)

	receiver, err := p.getTraderInfo(traderBuf, tx.To, nil)
	if err != nil {
		return err
	}

	receiverBal := new(big.Int).Add(receiver.availableBalance, tx.Amount)

	caller.nextNonce++
	caller.availableBalance = callerBal
	receiver.availableBalance = receiverBal

	traderBuf[tx.Caller().String()] = caller
	traderBuf[tx.Receiver().String()] = receiver
	return nil
}
