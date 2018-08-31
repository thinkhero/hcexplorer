// Copyright (c) 2017, The Dcrdata developers
// See LICENSE for details.

package explorer

import (
	"github.com/HcashOrg/hcd/hcjson"
	"github.com/HcashOrg/hcd/hcutil"
	"github.com/HcashOrg/hcexplorer/db/dbtypes"
	"github.com/HcashOrg/hcexplorer/txhelpers"
)

// BlockBasic models data for the explorer's explorer page
type BlockBasic struct {
	Height         int64  `json:"height"`
	Size           int32  `json:"size"`
	Valid          bool   `json:"valid"`
	Voters         uint16 `json:"votes"`
	Transactions   int    `json:"tx"`
	FreshStake     uint8  `json:"tickets"`
	Revocations    uint32 `json:"revocations"`
	BlockTime      int64  `json:"time"`
	FormattedTime  string `json:"formatted_time"`
	FormattedBytes string
}

//RichList models data for the RichList's page
type RichList struct {
	Address string  `json:"address"`
	Value   float64 `json:"value"`
}

// TxBasic models data for transactions on the block page
type TxBasic struct {
	TxID          string
	FormattedSize string
	Total         float64
	Fee           hcutil.Amount
	FeeRate       hcutil.Amount
	VoteInfo      *VoteInfo
	Coinbase      bool
}

//AddressTx models data for transactions on the address page
type AddressTx struct {
	TxID          string
	FormattedSize string
	Total         float64
	Confirmations uint64
	Time          int64
	FormattedTime string
	RecievedTotal float64
	SentTotal     float64
}

// TxInfo models data needed for display on the tx page
type TxInfo struct {
	*TxBasic
	SpendingTxns    []TxInID
	Type            string
	Vin             []Vin
	Vout            []Vout
	BlockHeight     int64
	BlockIndex      uint32
	Confirmations   int64
	Time            int64
	FormattedTime   string
	Mature          string
	VoteFundsLocked string
	TicketMaturity  int64
}

// TxInID models the identity of a spending transaction input
type TxInID struct {
	Hash  string
	Index uint32
}

// VoteInfo models data about a SSGen transaction (vote)
type VoteInfo struct {
	Validation BlockValidation         `json:"block_validation"`
	Version    uint32                  `json:"vote_version"`
	Bits       uint16                  `json:"vote_bits"`
	Choices    []*txhelpers.VoteChoice `json:"vote_choices"`
}

// BlockValidation models data about a vote's decision on a block
type BlockValidation struct {
	Hash     string `json:"hash"`
	Height   int64  `json:"height"`
	Validity bool   `json:"validity"`
}

// Vin models basic data about a tx input for display
type Vin struct {
	*hcjson.Vin
	Addresses       []string
	FormattedAmount string
}

// Vout models basic data about a tx output for display
type Vout struct {
	Addresses       []string
	Amount          float64
	FormattedAmount string
	Type            string
	Spent           bool
	OP_RETURN       string
}

// BlockInfo models data for display on the block page
type BlockInfo struct {
	*BlockBasic
	Hash                  string
	Version               int32
	Confirmations         int64
	StakeRoot             string
	MerkleRoot            string
	Tx                    []*TxBasic
	Tickets               []*TxBasic
	Revs                  []*TxBasic
	Votes                 []*TxBasic
	Nonce                 uint32
	VoteBits              uint16
	FinalState            string
	PoolSize              uint32
	Bits                  string
	SBits                 float64
	Difficulty            float64
	ExtraData             string
	StakeVersion          uint32
	PreviousHash          string
	NextHash              string
	TotalSent             float64
	MiningFee             hcutil.Amount
	StakeValidationHeight int64
}

// AddressInfo models data for display on the address page
type AddressInfo struct {
	Address          string
	Limit            int64
	Offset           int64
	Transactions     []*AddressTx
	NumFundingTxns   int64
	NumSpendingTxns  int64
	KnownFundingTxns int64
	NumUnconfirmed   int64
	TotalReceived    hcutil.Amount
	TotalSent        hcutil.Amount
	Unspent          hcutil.Amount
	Balance          *AddressBalance
	Path             string
}

// AddressBalance represents the number and value of spent and unspent outputs
// for an address.
type AddressBalance struct {
	Address      string
	NumSpent     int64
	NumUnspent   int64
	TotalSpent   int64
	TotalUnspent int64
}

// ReduceAddressHistory generates a template AddressInfo from a slice of
// dbtypes.AddressRow. All fields except NumUnconfirmed and Transactions are set
// completely. Transactions is partially set, with each transaction having only
// the TxID and ReceivedTotal set. The rest of the data should be filled in by
// other means, such as RPC calls or database queries.
func ReduceAddressHistory(addrHist []*dbtypes.AddressRow) *AddressInfo {
	if len(addrHist) == 0 {
		return nil
	}

	var received, sent int64
	var numFundingTxns, numSpendingTxns int64
	var transactions []*AddressTx
	for _, addrOut := range addrHist {
		numFundingTxns++
		coin := hcutil.Amount(addrOut.Value).ToCoin()

		// Funding transaction
		received += int64(addrOut.Value)
		tx := AddressTx{
			TxID:          addrOut.FundingTxHash,
			RecievedTotal: coin,
		}
		transactions = append(transactions, &tx)

		// Is the outpoint spent?
		if addrOut.SpendingTxHash == "" {
			continue
		}

		// Spending transaction
		numSpendingTxns++
		sent += int64(addrOut.Value)

		spendingTx := AddressTx{
			TxID:      addrOut.SpendingTxHash,
			SentTotal: coin,
		}
		transactions = append(transactions, &spendingTx)
	}

	return &AddressInfo{
		Address:         addrHist[0].Address,
		Transactions:    transactions,
		NumFundingTxns:  numFundingTxns,
		NumSpendingTxns: numSpendingTxns,
		TotalReceived:   hcutil.Amount(received),
		TotalSent:       hcutil.Amount(sent),
		Unspent:         hcutil.Amount(received - sent),
	}
}

// WebsocketBlock wraps the new block info for use in the websocket
type WebsocketBlock struct {
	Block BlockBasic `json:"block"`
}
