package transaction

import (
	"context"
	"fmt"
	"github.com/SteinsElite/pickGinS/types"
	"log"
	"math"
	"math/big"
	"sync"

	"github.com/SteinsElite/pickGinS/internal/gateway"
	"github.com/SteinsElite/pickGinS/internal/gateway/pickrouter"
	"github.com/SteinsElite/pickGinS/internal/storage"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	decimal = 18
	// the interval that heco mine new block
	heckBatch = 3
	interval  = heckBatch * 3
	// indicate that the block that the pickRouter contract deploy transaction is included
	geniusBlock = 8121824
	hecoLimit   = 5000
	txColl      = "transaction"
)

var (
	depositSigHash     = crypto.Keccak256Hash([]byte("Deposit(address,address,uint256)"))
	withdrawSigHash    = crypto.Keccak256Hash([]byte("Withdraw(address,address,uint256)"))
	claimProfitSigHash = crypto.Keccak256Hash([]byte("ClaimProfit(address,address,uint256)"))
)

const (
	Deposit     Type = "deposit"
	Withdraw    Type = "withdraw"
	ClaimProfit Type = "claimProfit"
)

type Type string

type BlockSpan struct {
	FromBlock int64
	ToBlock   int64
}

type TxRecord struct {
	TxHash      string  `json:"tx_hash" bson:"tx_hash"`
	BlockNumber uint64  `json:"block_number" bson:"block_number"`
	Timestamp   uint64  `json:"timestamp" bson:"timestamp"`
	TxType      Type    `json:"tx_type" bson:"tx_type"`
	User        string  `json:"user" bson:"user"`
	Token       string  `json:"token" bson:"token"`
	Amount      float64 `json:"amount" bson:"amount"`
}

type TxWatcher struct {
	client       *gateway.RpcClient
	CurrentBlock int64 // indicate the last block that has been processed
}

func newTxWatcher(currentBlock int64) *TxWatcher {
	return &TxWatcher{
		client:       gateway.GetRpcClient(),
		CurrentBlock: currentBlock,
	}
}

// if
func InitTxWatcher() *TxWatcher {
	coll := storage.AccessCollections(txColl)

	// get the tx record with the biggest block number in the database
	findOpt := options.Find()
	findOpt.SetSort(bson.D{{"block_number", -1}})
	findOpt.SetLimit(1)

	cur, err := coll.Find(
		context.TODO(),
		bson.D{},
		findOpt,
	)
	if err != nil {
		log.Println(err)
	}
	defer cur.Close(context.TODO())

	var record TxRecord
	if cur.Next(context.TODO()) {
		err := cur.Decode(&record)
		if err != nil {
			log.Println(err)
		}
		currentBlock := int64(record.BlockNumber) + 1
		return newTxWatcher(currentBlock)
	}
	return GenesisTxWatcher()
}


// GenesisTxWatcher When the first time to start the app, we should query start form the genesis
// contract created block number
func GenesisTxWatcher() *TxWatcher {
	return newTxWatcher(geniusBlock)
}

func (ro *TxWatcher) contractAddr() common.Address {
	return ro.client.ContractAddr
}

func (ro *TxWatcher) rpcClient() *ethclient.Client {
	return ro.client.Client
}

func (ro *TxWatcher) instance() *pickrouter.Pickrouter {
	return ro.client.Instance
}

func (ro *TxWatcher) ObtainTxUntil(toblk int64) (txs []TxRecord) {
	var blockSpans []BlockSpan
	for startblk := ro.CurrentBlock; startblk <= toblk; startblk += hecoLimit {
		endBlk := startblk + hecoLimit - 1
		if endBlk > toblk {
			endBlk = toblk
		}
		blockSpans = append(blockSpans, BlockSpan{startblk, endBlk})
	}

	var wg sync.WaitGroup
	// lock just used for append the slice, so the performance loss doesn't matter
	var m sync.Mutex
	for i := 0; i < len(blockSpans); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recpt, _ := ro.obtainTxRange(blockSpans[i])
			m.Lock()
			txs = append(txs, recpt...)
			m.Unlock()
		}(i)
	}
	wg.Wait()

	ro.CurrentBlock = toblk + 1
	log.Println("finish get the transaction with amount: ", len(txs))
	return
}

// get all the required logs in range [fromblk,toblk],make sure that toblk-fromblk is less that
// 5000 due to heco node limit
func (ro *TxWatcher) obtainTxRange(span BlockSpan) ([]TxRecord, error) {
	var txs []TxRecord

	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(span.FromBlock),
		ToBlock:   big.NewInt(span.ToBlock),
		Addresses: []common.Address{ro.contractAddr()},
	}
	elogs, err := ro.rpcClient().FilterLogs(context.Background(), query)
	if err != nil {
		log.Println("[filterLogs]: ", err)
		return nil, err
	}
	for _, elog := range elogs {
		tx, err := ro.populateTxFromLog(elog)
		if err != nil {
			log.Println("[populateTxFromLog]: ", err)
			return nil, err
		}
		txs = append(txs, tx)

	}
	return txs, nil
}

func (ro *TxWatcher) populateTxFromLog(vlog types.Log) (TxRecord, error) {
	var tx TxRecord

	switch vlog.Topics[0].Hex() {
	case depositSigHash.Hex():
		tx.TxType = Deposit
		deposit, err := ro.instance().ParseDeposit(vlog)
		if err != nil {
			return tx, err
		}
		tx.User = deposit.User.String()
		tx.Token, _ = _type.IdentifyToken(deposit.Token)
		tx.Amount = UnitConvert(deposit.Value)

	case withdrawSigHash.Hex():
		tx.TxType = Withdraw
		withdraw, err := ro.instance().ParseWithdraw(vlog)
		if err != nil {
			return tx, err
		}
		tx.User = withdraw.User.String()
		tx.Token, _ = _type.IdentifyToken(withdraw.Token)
		tx.Amount = UnitConvert(withdraw.Value)

	case claimProfitSigHash.Hex():
		tx.TxType = ClaimProfit
		claimProfit, err := ro.instance().ParseClaimProfit(vlog)
		if err != nil {
			return tx, err
		}
		tx.User = claimProfit.User.String()
		tx.Token, _ = _type.IdentifyToken(claimProfit.Token)
		tx.Amount = UnitConvert(claimProfit.Value)
	default:
		return TxRecord{}, fmt.Errorf("unmatched event")
	}

	tx.BlockNumber = vlog.BlockNumber
	blockHeader, err := ro.rpcClient().HeaderByNumber(
		context.TODO(),
		big.NewInt(int64(tx.BlockNumber)),
	)
	if err != nil {
		return tx, err
	}
	tx.Timestamp = blockHeader.Time
	tx.TxHash = vlog.TxHash.String()
	return tx, nil
}

// persit the transaction record in the database,
// TODO(ERIJ): handle the error when fail to insert the transaction
func persistRecord(txs []TxRecord) {
	coll := storage.AccessCollections(txColl)
	for _, tx := range txs {
		_, err := coll.InsertOne(context.TODO(), tx)
		if err != nil {
			log.Println("Fail to write to database due to: ", err)
		}
	}
}

func UnitConvert(bi *big.Int) float64 {
	fbi, _ := new(big.Float).SetString(bi.String())
	fbi = new(big.Float).Quo(fbi, big.NewFloat(math.Pow10(decimal)))
	famount, _ := fbi.Float64()
	return famount
}