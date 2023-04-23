package main

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaspanet/kaspad/domain/consensus/utils/consensushashing"

	"github.com/kaspanet/kaspad/domain/consensus/model/externalapi"
	"github.com/kaspanet/kaspad/domain/consensus/utils/constants"
	"github.com/kaspanet/kaspad/domain/consensus/utils/subnetworks"
	"github.com/kaspanet/kaspad/domain/consensus/utils/transactionid"
	"github.com/kaspanet/kaspad/domain/consensus/utils/txscript"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"

	"github.com/kaspanet/go-secp256k1"
	"github.com/kaspanet/kaspad/app/appmessage"

	utxopkg "github.com/kaspanet/kaspad/domain/consensus/utils/utxo"
	"github.com/kaspanet/kaspad/util"
	"github.com/pkg/errors"
)

var pendingOutpoints = map[appmessage.RPCOutpoint]time.Time{}

func spendLoop(client *rpcclient.RPCClient, addresses *addressesList,
	blockAddedNtfnChan chan *appmessage.BlockAddedNotificationMessage) <-chan struct{} {

	doneChan := make(chan struct{})

	spawn("spendLoop", func() {
		log.Infof("Populating pending transactions from the mempool")
		err := populatePendingOutpointsFromMempool(client, addresses.myAddress.EncodeAddress())
		if err != nil {
			panic(err)
		}

		log.Infof("Fetching the initial UTXO set")
		utxos, err := fetchSpendableUTXOs(client, addresses.myAddress.EncodeAddress())
		if err != nil {
			panic(err)
		}

		cfg := activeConfig()
		ticker := time.NewTicker(time.Duration(cfg.TransactionInterval) * time.Millisecond)
		for range ticker.C {
			healthChan := make(chan struct{})
			go func() {
				timer := time.NewTimer(5 * time.Minute)
				defer timer.Stop()
				select {
				case <-healthChan:
				case <-timer.C:
					log.Criticalf("HEALTCHECK FAILED")
					fmt.Println("HEALTCHECK FAILED")
					os.Exit(1)
				}
			}()

			hasFunds, err := maybeSendTransaction(client, addresses, utxos)
			if err != nil {
				panic(err)
			}

			//checkTransactions(utxosChangedNotificationChan)

			if !hasFunds {
				log.Infof("No funds. Refetching UTXO set.")
				utxos, err = fetchSpendableUTXOs(client, addresses.myAddress.EncodeAddress())
				if err != nil {
					panic(err)
				}
			}

			if atomic.LoadInt32(&shutdown) != 0 {
				close(doneChan)
				return
			}

			close(healthChan)
		}
	})

	return doneChan
}

func checkBlockAdded(blockAddedNtfnChan <-chan *appmessage.BlockAddedNotificationMessage) {
	select {
	case ntfn := <-blockAddedNtfnChan:
		now := time.Now()
		for _, tx := range ntfn.Block.Transactions {
			for _, input := range tx.Inputs {
				if _, ok := pendingOutpoints[*input.PreviousOutpoint]; ok {
					pendingOutpoints[*input.PreviousOutpoint] = now.Add(5*time.Second - pendingExpiration)
				}
			}
		}
	default:
	}
}

func checkTransactions(utxosChangedNotificationChan <-chan *appmessage.UTXOsChangedNotificationMessage) {
	isDone := false
	for !isDone {
		select {
		case notification := <-utxosChangedNotificationChan:
			for _, removed := range notification.Removed {
				sendTime, ok := pendingOutpoints[*removed.Outpoint]
				if !ok {
					continue // this is coinbase transaction paying to our address or some transaction from an old run
				}

				log.Infof("Output %s:%d accepted. Time since send: %s",
					removed.Outpoint.TransactionID, removed.Outpoint.Index, time.Now().Sub(sendTime))

				delete(pendingOutpoints, *removed.Outpoint)
			}
		default:
			isDone = true
		}
	}

	for pendingOutpoint, txTime := range pendingOutpoints {
		timeSince := time.Now().Sub(txTime)
		if timeSince > 10*time.Minute {
			log.Tracef("Outpoint %s:%d is pending for %s",
				pendingOutpoint.TransactionID, pendingOutpoint.Index, timeSince)
		}
	}
}

const balanceEpsilon = 10_000                // 10,000 sompi = 0.0001 kaspa
const defaultFeeAmount = balanceEpsilon * 10 // use high fee amount, because can have a large number of inputs
const bigTransactionNumOuts = 50
const pendingExpiration = 5 * time.Minute

var stats struct {
	sync.Mutex
	numTxs uint64
	since  time.Time
}

func maybeSendTransaction(client *rpcclient.RPCClient, addresses *addressesList,
	availableUTXOs []*appmessage.UTXOsByAddressesEntry) (hasFunds bool, err error) {

	fee := uint64(defaultFeeAmount)
	sendAmount := randomizeSpendAmount()
	numOuts := 2
	if len(availableUTXOs) < 100_000 {
		fee *= bigTransactionNumOuts
		sendAmount *= bigTransactionNumOuts
		numOuts = bigTransactionNumOuts
	}

	totalSendAmount := sendAmount + fee

	selectedUTXOs, selectedValue, err := selectUTXOs(availableUTXOs, totalSendAmount)
	if err != nil {
		return false, err
	}

	if len(selectedUTXOs) == 0 {
		return false, nil
	}

	if selectedValue < totalSendAmount {
		if selectedValue < fee {
			return false, nil
		}
		sendAmount = selectedValue - fee
	}

	spendAddress := randomizeSpendAddress(addresses)

	rpcTransaction, err := generateTransaction(
		addresses.myPrivateKey, selectedUTXOs, sendAmount, numOuts, spendAddress, addresses.myAddress)
	if err != nil {
		return false, err
	}

	if rpcTransaction.Outputs[0].Amount == 0 {
		log.Warnf("Got transaction with 0 value output")
		return false, nil
	}

	spawn("sendTransaction", func() {
		transactionID, err := sendTransaction(client, rpcTransaction)
		if err != nil {
			errMessage := err.Error()
			if !strings.Contains(errMessage, "orphan transaction") &&
				!strings.Contains(errMessage, "is already in the mempool") &&
				!strings.Contains(errMessage, "is an orphan") &&
				!strings.Contains(errMessage, "already spent by transaction") {
				panic(errors.Wrapf(err, "error sending transaction: %s", err))
			}
			log.Warnf("Double spend error: %s", err)
		} else {
			log.Infof("Sent transaction %s worth %f kaspa with %d inputs and %d outputs", transactionID,
				float64(sendAmount)/constants.SompiPerKaspa, len(rpcTransaction.Inputs), len(rpcTransaction.Outputs))
			func() {
				stats.Lock()
				defer stats.Unlock()

				stats.numTxs++
				timePast := time.Since(stats.since)
				if timePast > 10*time.Second {
					log.Infof("Tx rate: %f/sec", float64(stats.numTxs)/timePast.Seconds())
					stats.numTxs = 0
					stats.since = time.Now()
				}
			}()
		}
	})

	updateState(selectedUTXOs)

	return true, nil
}

func fetchSpendableUTXOs(client *rpcclient.RPCClient, address string) ([]*appmessage.UTXOsByAddressesEntry, error) {
	getUTXOsByAddressesResponse, err := client.GetUTXOsByAddresses([]string{address})
	if err != nil {
		return nil, err
	}
	dagInfo, err := client.GetBlockDAGInfo()
	if err != nil {
		return nil, err
	}

	spendableUTXOs := make([]*appmessage.UTXOsByAddressesEntry, 0, len(getUTXOsByAddressesResponse.Entries))
	for _, entry := range getUTXOsByAddressesResponse.Entries {
		if !isUTXOSpendable(entry, dagInfo.VirtualDAAScore) {
			continue
		}
		spendableUTXOs = append(spendableUTXOs, entry)
	}

	sort.Slice(spendableUTXOs, func(i, j int) bool {
		return spendableUTXOs[i].UTXOEntry.Amount > spendableUTXOs[j].UTXOEntry.Amount
	})

	return spendableUTXOs, nil
}

func populatePendingOutpointsFromMempool(client *rpcclient.RPCClient, address string) error {
	response, err := client.GetMempoolEntriesByAddresses([]string{address}, false, false)
	if err != nil {
		return err
	}
	for _, entry := range response.Entries {
		for _, mempoolEntry := range entry.Sending {
			for _, input := range mempoolEntry.Transaction.Inputs {
				pendingOutpoints[*input.PreviousOutpoint] = time.Now()
			}
		}
	}

	return nil
}

func isUTXOSpendable(entry *appmessage.UTXOsByAddressesEntry, virtualSelectedParentBlueScore uint64) bool {
	blockDAAScore := entry.UTXOEntry.BlockDAAScore
	if !entry.UTXOEntry.IsCoinbase {
		const minConfirmations = 10
		return blockDAAScore+minConfirmations < virtualSelectedParentBlueScore
	}
	coinbaseMaturity := activeConfig().ActiveNetParams.BlockCoinbaseMaturity
	return blockDAAScore+coinbaseMaturity < virtualSelectedParentBlueScore
}

func updateState(
	selectedUTXOs []*appmessage.UTXOsByAddressesEntry) {

	now := time.Now()
	for _, utxo := range selectedUTXOs {
		pendingOutpoints[*utxo.Outpoint] = now
	}

	outpointsToDelete := make([]appmessage.RPCOutpoint, 0)
	for outpoint, sendTime := range pendingOutpoints {
		if now.Sub(sendTime) > pendingExpiration {
			outpointsToDelete = append(outpointsToDelete, outpoint)
		}
	}

	for _, outpoint := range outpointsToDelete {
		delete(pendingOutpoints, outpoint)
	}
}

func filterSpentUTXOsAndCalculateBalance(utxos []*appmessage.UTXOsByAddressesEntry) (
	filteredUTXOs []*appmessage.UTXOsByAddressesEntry, balance uint64) {

	balance = 0
	for _, utxo := range utxos {
		if _, ok := pendingOutpoints[*utxo.Outpoint]; ok {
			continue
		}
		balance += utxo.UTXOEntry.Amount
		filteredUTXOs = append(filteredUTXOs, utxo)
	}
	return filteredUTXOs, balance
}

func randomizeSpendAddress(addresses *addressesList) util.Address {
	spendAddressIndex := rand.Intn(len(addresses.spendAddresses))

	return addresses.spendAddresses[spendAddressIndex]
}

func randomizeSpendAmount() uint64 {
	const maxAmountToSent = 10 * defaultFeeAmount
	amountToSend := rand.Int63n(int64(maxAmountToSent))

	// round to balanceEpsilon
	amountToSend = amountToSend / balanceEpsilon * balanceEpsilon
	if amountToSend < balanceEpsilon {
		amountToSend = balanceEpsilon
	}

	return uint64(amountToSend)
}

func selectUTXOs(utxos []*appmessage.UTXOsByAddressesEntry, amountToSend uint64) (
	selectedUTXOs []*appmessage.UTXOsByAddressesEntry, selectedValue uint64, err error) {

	selectedUTXOs = []*appmessage.UTXOsByAddressesEntry{}
	selectedValue = uint64(0)

	for _, entry := range utxos {
		outpoint := entry.Outpoint
		utxo := entry.UTXOEntry
		if _, ok := pendingOutpoints[*outpoint]; ok {
			continue
		}

		selectedUTXOs = append(selectedUTXOs, entry)
		selectedValue += utxo.Amount

		if selectedValue >= amountToSend {
			break
		}

		const maxInputs = 100
		if len(selectedUTXOs) == maxInputs {
			log.Infof("Selected %d UTXOs so sending the transaction with %d sompis instead "+
				"of %d", maxInputs, selectedValue, amountToSend)
			break
		}
	}

	return selectedUTXOs, selectedValue, nil
}

func generateTransaction(keyPair *secp256k1.SchnorrKeyPair, selectedUTXOs []*appmessage.UTXOsByAddressesEntry,
	sompisToSend uint64, numOuts int, toAddress util.Address,
	fromAddress util.Address) (*appmessage.RPCTransaction, error) {

	inputs := make([]*externalapi.DomainTransactionInput, len(selectedUTXOs))
	for i, utxo := range selectedUTXOs {
		outpointTransactionIDBytes, err := hex.DecodeString(utxo.Outpoint.TransactionID)
		if err != nil {
			return nil, err
		}
		outpointTransactionID, err := transactionid.FromBytes(outpointTransactionIDBytes)
		if err != nil {
			return nil, err
		}
		outpoint := externalapi.DomainOutpoint{
			TransactionID: *outpointTransactionID,
			Index:         utxo.Outpoint.Index,
		}
		utxoScriptPublicKeyScript, err := hex.DecodeString(utxo.UTXOEntry.ScriptPublicKey.Script)
		if err != nil {
			return nil, err
		}

		inputs[i] = &externalapi.DomainTransactionInput{
			PreviousOutpoint: outpoint,
			SigOpCount:       1,
			UTXOEntry: utxopkg.NewUTXOEntry(
				utxo.UTXOEntry.Amount,
				&externalapi.ScriptPublicKey{
					Script:  utxoScriptPublicKeyScript,
					Version: utxo.UTXOEntry.ScriptPublicKey.Version,
				},
				utxo.UTXOEntry.IsCoinbase,
				utxo.UTXOEntry.BlockDAAScore,
			),
		}
	}

	toScript, err := txscript.PayToAddrScript(toAddress)
	if err != nil {
		return nil, err
	}

	outputs := make([]*externalapi.DomainTransactionOutput, 0)
	for i := 0; i < numOuts; i++ {
		outputs = append(outputs, &externalapi.DomainTransactionOutput{
			Value:           sompisToSend / uint64(numOuts),
			ScriptPublicKey: toScript,
		})
	}

	domainTransaction := &externalapi.DomainTransaction{
		Version:      constants.MaxTransactionVersion,
		Inputs:       inputs,
		Outputs:      outputs,
		LockTime:     0,
		SubnetworkID: subnetworks.SubnetworkIDNative,
		Gas:          0,
		Payload:      nil,
	}

	for i, input := range domainTransaction.Inputs {
		signatureScript, err := txscript.SignatureScript(domainTransaction, i, consensushashing.SigHashAll, keyPair,
			&consensushashing.SighashReusedValues{})
		if err != nil {
			return nil, err
		}
		input.SignatureScript = signatureScript
	}

	rpcTransaction := appmessage.DomainTransactionToRPCTransaction(domainTransaction)
	return rpcTransaction, nil
}

func sendTransaction(client *rpcclient.RPCClient, rpcTransaction *appmessage.RPCTransaction) (string, error) {
	submitTransactionResponse, err := client.SubmitTransaction(rpcTransaction, false)
	if err != nil {
		return "", errors.Wrapf(err, "error submitting transaction")
	}
	return submitTransactionResponse.TransactionID, nil
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
