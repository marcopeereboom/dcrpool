package pool

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	"decred.org/dcrwallet/rpc/walletrpc"
	txrules "decred.org/dcrwallet/wallet/txrules"
	"decred.org/dcrwallet/wallet/txsizes"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrutil/v3"
	chainjson "github.com/decred/dcrd/rpc/jsonrpc/types/v2"
	"github.com/decred/dcrd/wire"
	"google.golang.org/grpc"

	errs "github.com/decred/dcrpool/errors"
)

const (
	// PPS represents the pay per share payment method.
	PPS = "pps"

	// PPLNS represents the pay per last n shares payment method.
	PPLNS = "pplns"

	// maxRoundingDiff is the maximum amount of atoms the total
	// output value of a transaction is allowed to be short of the
	// provided input due to rounding errors.
	maxRoundingDiff = dcrutil.Amount(500)
)

// TxCreator defines the functionality needed by a transaction creator for the
// pool.
type TxCreator interface {
	// GetTxOut fetches the output referenced by the provided txHash and index.
	GetTxOut(context.Context, *chainhash.Hash, uint32, bool) (*chainjson.GetTxOutResult, error)
	// CreateRawTransaction generates a transaction from the provided
	// inputs and payouts.
	CreateRawTransaction(context.Context, []chainjson.TransactionInput, map[dcrutil.Address]dcrutil.Amount, *int64, *int64) (*wire.MsgTx, error)
	// GetBlock fetches the block associated with the provided block hash.
	GetBlock(ctx context.Context, blockHash *chainhash.Hash) (*wire.MsgBlock, error)
}

// TxBroadcaster defines the functionality needed by a transaction broadcaster
// for the pool.
type TxBroadcaster interface {
	// SignTransaction signs transaction inputs, unlocking them for use.
	SignTransaction(context.Context, *walletrpc.SignTransactionRequest, ...grpc.CallOption) (*walletrpc.SignTransactionResponse, error)
	// PublishTransaction broadcasts the transaction unto the network.
	PublishTransaction(context.Context, *walletrpc.PublishTransactionRequest, ...grpc.CallOption) (*walletrpc.PublishTransactionResponse, error)
}

// confNotifMsg represents a tx confirmation notification message.
type confNotifMsg struct {
	resp *walletrpc.ConfirmationNotificationsResponse
	err  error
}

// PaymentMgrConfig contains all of the configuration values which should be
// provided when creating a new instance of PaymentMgr.
type PaymentMgrConfig struct {
	// db represents the pool database.
	db Database
	// ActiveNet represents the network being mined on.
	ActiveNet *chaincfg.Params
	// PoolFee represents the fee charged to participating accounts of the pool.
	PoolFee float64
	// LastNPeriod represents the period to source shares from when using the
	// PPLNS payment scheme.
	LastNPeriod time.Duration
	// SoloPool represents the solo pool mining mode.
	SoloPool bool
	// PaymentMethod represents the payment scheme of the pool.
	PaymentMethod string
	// PoolFeeAddrs represents the pool fee addresses of the pool.
	PoolFeeAddrs []dcrutil.Address
	// WalletAccount represents the wallet account to process payments from.
	WalletAccount uint32
	// WalletPass represents the passphrase to unlock the wallet with.
	WalletPass string
	// GetBlockConfirmations returns the number of block confirmations for the
	// provided block hash.
	GetBlockConfirmations func(context.Context, *chainhash.Hash) (int64, error)
	// GetTxConfNotifications streams transaction confirmation notifications on
	// the provided hashes.
	GetTxConfNotifications func([]*chainhash.Hash, int32) (func() (*walletrpc.ConfirmationNotificationsResponse, error), error)
	// FetchTxCreator returns a transaction creator that allows coinbase lookups
	// and payment transaction creation.
	FetchTxCreator func() TxCreator
	// FetchTxBroadcaster returns a transaction broadcaster that allows signing
	// and publishing of transactions.
	FetchTxBroadcaster func() TxBroadcaster
	// CoinbaseConfTimeout is the duration to wait for coinbase confirmations
	// when generating a payout transaction.
	CoinbaseConfTimeout time.Duration
}

// PaymentMgr handles generating shares and paying out dividends to
// participating accounts.
type PaymentMgr struct {
	cfg *PaymentMgrConfig
}

// NewPaymentMgr creates a new payment manager.
func NewPaymentMgr(pCfg *PaymentMgrConfig) (*PaymentMgr, error) {
	pm := &PaymentMgr{
		cfg: pCfg,
	}
	rand.Seed(time.Now().UnixNano())

	// Initialize last payment info (height and paid-on).
	_, _, err := pm.cfg.db.loadLastPaymentInfo()
	if err != nil {
		if errors.Is(err, errs.ValueNotFound) {
			// Initialize with zeros.
			err = pm.cfg.db.persistLastPaymentInfo(0, 0)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Initialize last payment created-on.
	_, err = pm.cfg.db.loadLastPaymentCreatedOn()
	if err != nil {
		if errors.Is(err, errs.ValueNotFound) {
			// Initialize with zero.
			err = pm.cfg.db.persistLastPaymentCreatedOn(0)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	return pm, nil
}

// sharePercentages calculates the percentages due each participating account
// according to their weighted shares.
func (pm *PaymentMgr) sharePercentages(shares []*Share) (map[string]*big.Rat, error) {
	totalShares := new(big.Rat)
	tally := make(map[string]*big.Rat)
	percentages := make(map[string]*big.Rat)

	// Tally all share weights for each participating account.
	for _, share := range shares {
		totalShares = totalShares.Add(totalShares, share.Weight)
		if _, ok := tally[share.Account]; ok {
			tally[share.Account] = tally[share.Account].
				Add(tally[share.Account], share.Weight)
			continue
		}
		tally[share.Account] = share.Weight
	}

	// Calculate each participating account percentage to be claimed.
	for account, shareCount := range tally {
		if tally[account].Cmp(ZeroRat) == 0 {
			return nil, errs.PoolError(errs.DivideByZero, "division by zero")
		}
		accPercent := new(big.Rat).Quo(shareCount, totalShares)
		percentages[account] = accPercent
	}
	return percentages, nil
}

// PPSSharePercentages calculates the current mining reward percentages
// due participating pool accounts based on work performed measured by
// the PPS payment scheme.
func (pm *PaymentMgr) PPSSharePercentages(workCreatedOn int64) (map[string]*big.Rat, error) {
	shares, err := pm.cfg.db.ppsEligibleShares(workCreatedOn)
	if err != nil {
		return nil, err
	}
	if len(shares) == 0 {
		return make(map[string]*big.Rat), nil
	}
	percentages, err := pm.sharePercentages(shares)
	if err != nil {
		return nil, err
	}
	return percentages, nil
}

// PPLNSSharePercentages calculates the current mining reward percentages due pool
// accounts based on work performed measured by the PPLNS payment scheme.
func (pm *PaymentMgr) PPLNSSharePercentages() (map[string]*big.Rat, error) {
	now := time.Now()
	min := now.Add(-pm.cfg.LastNPeriod)
	shares, err := pm.cfg.db.pplnsEligibleShares(min.UnixNano())
	if err != nil {
		return nil, err
	}
	if len(shares) == 0 {
		return make(map[string]*big.Rat), nil
	}

	// Deduct pool fees and calculate the payment due each participating
	// account.
	percentages, err := pm.sharePercentages(shares)
	if err != nil {
		return nil, err
	}
	return percentages, nil
}

// calculatePayments creates the payments due participating accounts.
func (pm *PaymentMgr) calculatePayments(ratios map[string]*big.Rat, source *PaymentSource,
	total dcrutil.Amount, poolFee float64, height uint32, estMaturity uint32) ([]*Payment, int64, error) {
	funcName := "calculatePayments"
	if len(ratios) == 0 {
		desc := fmt.Sprintf("%s: valid share ratios required to "+
			"generate payments", funcName)
		return nil, 0, errs.PoolError(errs.ShareRatio, desc)
	}

	// Deduct pool fee from the amount to be shared.
	fee := total.MulF64(poolFee)
	amtSansFees := total - fee
	sansFees := new(big.Rat).SetInt64(int64(amtSansFees))
	paymentTotal := dcrutil.Amount(0)
	dustAmts := make([]dcrutil.Amount, 0)

	// Calculate each participating account's portion of the amount after fees.
	payments := make([]*Payment, 0)
	for account, ratio := range ratios {
		amtRat := new(big.Rat).Mul(sansFees, ratio)
		amtI, accuracy := new(big.Float).SetRat(amtRat).Int64()
		amt := dcrutil.Amount(amtI)

		// Reduce the amount by an atom if float conversion accuracy was
		// above the actual value.
		if accuracy > 0 {
			amt -= dcrutil.Amount(1)
		}

		paymentTotal += amt

		// The script size of the output is assumed to be the worst possible,
		// which is txsizes.P2PKHOutputSize, to avoid a lower size estimation.
		if txrules.IsDustAmount(amt, txsizes.P2PKHOutputSize,
			txrules.DefaultRelayFeePerKb) {
			// Since dust payments will cause the payout transaction to error
			// and are also most likely to be generated by participating
			// accounts contributing sporadic work to pool they will be
			// forfeited by their corresponding accounts and be added to
			// the pool fee payout. This is intended to serve as a deterrent
			// for contributing intermittent, sporadic work to the pool.
			dustAmts = append(dustAmts, amt)
		} else {
			payments = append(payments, NewPayment(account, source, amt, height,
				estMaturity))
		}
	}

	if amtSansFees < paymentTotal {
		diff := paymentTotal - amtSansFees
		desc := fmt.Sprintf("%s: total payments (%s) is greater than "+
			"the remaining coinbase amount after fees (%s). Difference is %s",
			funcName, paymentTotal, amtSansFees, diff)
		return nil, 0, errs.PoolError(errs.PaymentSource, desc)
	}

	// Add a payout entry for pool fees, which includes any dust payments
	// collected.
	var dustTotal dcrutil.Amount
	for _, amt := range dustAmts {
		dustTotal += amt
	}

	feePayment := NewPayment(PoolFeesK, source, fee+dustTotal, height, estMaturity)
	payments = append(payments, feePayment)

	return payments, feePayment.CreatedOn, nil
}

// PayPerShare generates a payment bundle comprised of payments to all
// participating accounts. Payments are calculated based on work contributed
// to the pool since the last payment batch.
func (pm *PaymentMgr) payPerShare(source *PaymentSource, amt dcrutil.Amount, height uint32, workCreatedOn int64) error {
	percentages, err := pm.PPSSharePercentages(workCreatedOn)
	if err != nil {
		return err
	}
	estMaturity := height + uint32(pm.cfg.ActiveNet.CoinbaseMaturity)
	payments, lastPmtCreatedOn, err := pm.calculatePayments(percentages,
		source, amt, pm.cfg.PoolFee, height, estMaturity)
	if err != nil {
		return err
	}
	for _, payment := range payments {
		err := pm.cfg.db.PersistPayment(payment)
		if err != nil {
			return err
		}
	}
	// Update the last payment created on time and prune invalidated shares.
	err = pm.cfg.db.persistLastPaymentCreatedOn(lastPmtCreatedOn)
	if err != nil {
		return err
	}
	return pm.cfg.db.pruneShares(workCreatedOn)
}

// payPerLastNShares generates a payment bundle comprised of payments to all
// participating accounts within the lastNPeriod of the pool.
func (pm *PaymentMgr) payPerLastNShares(source *PaymentSource, amt dcrutil.Amount, height uint32) error {
	percentages, err := pm.PPLNSSharePercentages()
	if err != nil {
		return err
	}
	estMaturity := height + uint32(pm.cfg.ActiveNet.CoinbaseMaturity)
	payments, lastPmtCreatedOn, err := pm.calculatePayments(percentages,
		source, amt, pm.cfg.PoolFee, height, estMaturity)
	if err != nil {
		return err
	}
	for _, payment := range payments {
		err := pm.cfg.db.PersistPayment(payment)
		if err != nil {
			return err
		}
	}
	// Update the last payment created on time and prune invalidated shares.
	err = pm.cfg.db.persistLastPaymentCreatedOn(lastPmtCreatedOn)
	if err != nil {
		return err
	}
	minNano := time.Now().Add(-pm.cfg.LastNPeriod).UnixNano()
	return pm.cfg.db.pruneShares(minNano)
}

// generatePayments creates payments for participating accounts. This should
// only be called when a block is confirmed mined, in pool mining mode.
func (pm *PaymentMgr) generatePayments(height uint32, source *PaymentSource, amt dcrutil.Amount, workCreatedOn int64) error {
	switch pm.cfg.PaymentMethod {
	case PPS:
		return pm.payPerShare(source, amt, height, workCreatedOn)

	case PPLNS:
		return pm.payPerLastNShares(source, amt, height)

	default:
		return fmt.Errorf("unknown payment method provided %v", pm.cfg.PaymentMethod)
	}
}

// pruneOrphanedPayments removes all orphaned payments from the provided payments.
func (pm *PaymentMgr) pruneOrphanedPayments(ctx context.Context, pmts map[string][]*Payment) (map[string][]*Payment, error) {
	toDelete := make([]string, 0, len(pmts))
	for key := range pmts {
		blockHash, err := chainhash.NewHashFromStr(key)
		if err != nil {
			desc := fmt.Sprintf("unable to generate hash: %v", err)
			return nil, errs.PoolError(errs.CreateHash, desc)
		}

		confs, err := pm.cfg.GetBlockConfirmations(ctx, blockHash)
		if err != nil {
			return nil, err
		}

		// If the block has no confirmations for the current chain
		// state it is an orphan. Remove payments associated with it.
		if confs <= 0 {
			toDelete = append(toDelete, key)
		}
	}

	// Delete payments sourced from orphaned blocks.
	for _, k := range toDelete {
		delete(pmts, k)
	}
	return pmts, nil
}

// applyTxFees determines the transaction fees needed for the payout transaction
// and deducts portions of the fee from outputs of participating accounts
// being paid to.
//
// The deducted portions are calculated as the percentage of fees based on
// the ratio of the amount being paid to the total transaction output minus
// pool fees.
func (pm *PaymentMgr) applyTxFees(inputs []chainjson.TransactionInput, outputs map[string]dcrutil.Amount,
	tOut dcrutil.Amount, feeAddr dcrutil.Address) (dcrutil.Amount, dcrutil.Amount, error) {
	funcName := "applyTxFees"
	if len(inputs) == 0 {
		desc := fmt.Sprint("%s: cannot create a payout transaction "+
			"without a tx input", funcName)
		return 0, 0, errs.PoolError(errs.TxIn, desc)
	}
	if len(outputs) == 0 {
		desc := fmt.Sprint("%s:cannot create a payout transaction "+
			"without a tx output", funcName)
		return 0, 0, errs.PoolError(errs.TxOut, desc)
	}
	inSizes := make([]int, len(inputs))
	for range inputs {
		inSizes = append(inSizes, txsizes.RedeemP2PKHSigScriptSize)
	}
	outSizes := make([]int, len(outputs))
	for range outputs {
		outSizes = append(outSizes, txsizes.P2PKHOutputSize)
	}
	changeScriptSize := 0
	estSize := txsizes.EstimateSerializeSizeFromScriptSizes(inSizes, outSizes,
		changeScriptSize)
	estFee := txrules.FeeForSerializeSize(txrules.DefaultRelayFeePerKb, estSize)
	sansFees := tOut - estFee

	for addr, v := range outputs {
		// Pool fee payments are excluded from tx fee deductions.
		if addr == feeAddr.String() {
			continue
		}

		ratio := float64(int64(sansFees)) / float64(int64(v))
		outFee := estFee.MulF64(ratio)
		outputs[addr] -= outFee
	}

	return sansFees, estFee, nil
}

// fetchTxConfNotifications is a helper function for fetching tx confirmation
// notifications. It will return when either a notification or error is
// received from the provided notification source, or when the provided
// context is cancelled.
func fetchTxConfNotifications(ctx context.Context, notifSource func() (*walletrpc.ConfirmationNotificationsResponse, error)) (*walletrpc.ConfirmationNotificationsResponse, error) {
	funcName := "fetchTxConfNotifications"
	notifCh := make(chan confNotifMsg)
	go func(ch chan confNotifMsg) {
		resp, err := notifSource()
		ch <- confNotifMsg{
			resp: resp,
			err:  err,
		}
	}(notifCh)

	select {
	case <-ctx.Done():
		log.Tracef("%s: unable to fetch tx confirmation notifications",
			funcName)
		return nil, errs.ContextCancelled
	case notif := <-notifCh:
		close(notifCh)
		if notif.err != nil {
			desc := fmt.Sprintf("%s: unable to fetch tx confirmation "+
				"notifications, %s", funcName, notif.err)
			return nil, errs.PoolError(errs.TxConf, desc)
		}
		return notif.resp, nil
	}
}

// confirmCoinbases ensures the coinbases referenced by the provided
// transaction hashes are spendable by the expected maximum spendable height.
//
// The context passed to this function must have a corresponding
// cancellation to allow for a clean shutdown process
func (pm *PaymentMgr) confirmCoinbases(ctx context.Context, txHashes map[string]*chainhash.Hash, spendableHeight uint32) error {
	funcName := "confirmCoinbases"
	hashes := make([]*chainhash.Hash, 0, len(txHashes))
	for _, hash := range txHashes {
		hashes = append(hashes, hash)
	}

	notifSource, err := pm.cfg.GetTxConfNotifications(hashes,
		int32(spendableHeight))
	if err != nil {
		return err
	}

	// Wait for coinbase tx confirmations from the wallet.
	maxSpendableConfs := int32(pm.cfg.ActiveNet.CoinbaseMaturity) + 1

	for {
		resp, err := fetchTxConfNotifications(ctx, notifSource)
		if err != nil {
			if errors.Is(err, errs.ContextCancelled) {
				desc := fmt.Sprintf("%s: cancelled confirming %d coinbase "+
					"transaction(s)", funcName, len(txHashes))
				return errs.PoolError(errs.ContextCancelled, desc)
			}
			return err
		}

		// Ensure all coinbases being spent are spendable.
		for _, coinbase := range resp.Confirmations {
			if coinbase.Confirmations >= maxSpendableConfs {
				hash, err := chainhash.NewHash(coinbase.TxHash)
				if err != nil {
					desc := fmt.Sprintf("%s: unable to create tx hash: %v",
						funcName, err)
					return errs.PoolError(errs.CreateHash, desc)
				}

				// Remove spendable coinbase from the tx hash set. All
				// coinbases are spendable when the tx hash set is empty.
				delete(txHashes, hash.String())
			}
		}

		if len(txHashes) == 0 {
			return nil
		}
	}
}

// generatePayoutTxDetails creates the payout transaction inputs and outputs
// from the provided payments
func (pm *PaymentMgr) generatePayoutTxDetails(ctx context.Context, txC TxCreator, feeAddr dcrutil.Address, payments map[string][]*Payment, treasuryActive bool) ([]chainjson.TransactionInput,
	map[string]*chainhash.Hash, map[string]dcrutil.Amount, dcrutil.Amount, error) {
	funcName := "generatePayoutTxDetails"

	// The coinbase output prior to
	// [DCP0006](https://github.com/decred/dcps/pull/17)
	// activation is at the third index position and at
	// the second index position once DCP0006 is activated.
	coinbaseIndex := uint32(1)
	if !treasuryActive {
		coinbaseIndex = 2
	}

	var tIn, tOut dcrutil.Amount
	inputs := make([]chainjson.TransactionInput, 0)
	inputTxHashes := make(map[string]*chainhash.Hash)
	outputs := make(map[string]dcrutil.Amount)
	for _, pmtSet := range payments {
		coinbaseTx := pmtSet[0].Source.Coinbase
		txHash, err := chainhash.NewHashFromStr(coinbaseTx)
		if err != nil {
			desc := fmt.Sprintf("%s: unable to create tx hash: %v",
				funcName, err)
			return nil, nil, nil, 0, errs.PoolError(errs.CreateHash, desc)
		}

		// Ensure the referenced prevout to be spent is spendable at
		// the current height.
		txOutResult, err := txC.GetTxOut(ctx, txHash, coinbaseIndex, false)
		if err != nil {
			desc := fmt.Sprintf("%s: unable to find tx output: %v",
				funcName, err)
			return nil, nil, nil, 0, errs.PoolError(errs.TxOut, desc)
		}
		if txOutResult.Confirmations < int64(pm.cfg.ActiveNet.CoinbaseMaturity+1) {
			desc := fmt.Sprintf("%s: referenced coinbase at "+
				"index %d for tx %v is not spendable", funcName,
				coinbaseIndex, txHash.String())
			return nil, nil, nil, 0, errs.PoolError(errs.Coinbase, desc)
		}

		// Create the transaction input using the provided prevOut.
		in := chainjson.TransactionInput{
			Amount: txOutResult.Value,
			Txid:   txHash.String(),
			Vout:   coinbaseIndex,
			Tree:   wire.TxTreeRegular,
		}
		inputs = append(inputs, in)
		inputTxHashes[txHash.String()] = txHash

		prevOutV, err := dcrutil.NewAmount(in.Amount)
		if err != nil {
			desc := fmt.Sprintf("%s: unable create the input amount: %v",
				funcName, err)
			return nil, nil, nil, 0, errs.PoolError(errs.CreateAmount, desc)
		}
		tIn += prevOutV

		// Generate the outputs paying dividends as well as pool fees.
		for _, pmt := range pmtSet {
			if pmt.Account == PoolFeesK {
				_, ok := outputs[feeAddr.String()]
				if !ok {
					outputs[feeAddr.String()] = pmt.Amount
					tOut += pmt.Amount
					continue
				}
				outputs[feeAddr.String()] += pmt.Amount
				tOut += pmt.Amount
				continue
			}

			acc, err := pm.cfg.db.fetchAccount(pmt.Account)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			_, ok := outputs[acc.Address]
			if !ok {
				outputs[acc.Address] = pmt.Amount
				tOut += pmt.Amount
				continue
			}
			outputs[acc.Address] += pmt.Amount
			tOut += pmt.Amount
		}
	}

	// Ensure the transaction outputs do not source more value than possible
	// from the provided inputs and also are consuming all of the input
	// value after rounding errors.
	if tOut > tIn {
		desc := fmt.Sprintf("%s: total output values for the "+
			"transaction (%s) is greater than the provided inputs (%s)",
			funcName, tOut, tIn)
		return nil, nil, nil, 0, errs.PoolError(errs.CreateTx, desc)
	}

	diff := tIn - tOut
	if diff > maxRoundingDiff {
		desc := fmt.Sprintf("%s: difference between total output "+
			"values and the provided inputs (%s) exceeds the maximum "+
			"allowed for rounding errors (%s)", funcName, diff, maxRoundingDiff)
		return nil, nil, nil, 0, errs.PoolError(errs.CreateTx, desc)
	}

	return inputs, inputTxHashes, outputs, tOut, nil
}

// PayDividends pays mature mining rewards to participating accounts.
func (pm *PaymentMgr) payDividends(ctx context.Context, height uint32, treasuryActive bool) error {
	funcName := "payDividends"
	mPmts, err := pm.cfg.db.maturePendingPayments(height)
	if err != nil {
		return err
	}

	// Nothing to do if there are no mature payments to process.
	if len(mPmts) == 0 {
		return nil
	}

	txC := pm.cfg.FetchTxCreator()
	if txC == nil {
		desc := fmt.Sprintf("%s: tx creator cannot be nil", funcName)
		return errs.PoolError(errs.Disconnected, desc)
	}

	// remove all matured orphaned payments. Since the associated blocks
	// to these payments are not part of the main chain they will not be
	// paid out.
	pmts, err := pm.pruneOrphanedPayments(ctx, mPmts)
	if err != nil {
		return err
	}

	// The fee address is being picked at random from the set of pool fee
	// addresses to make it difficult for third-parties wanting to track
	// pool fees collected by the pool and ultimately determine the
	// cumulative value accrued by pool operators.
	feeAddr := pm.cfg.PoolFeeAddrs[rand.Intn(len(pm.cfg.PoolFeeAddrs))]

	inputs, inputTxHashes, outputs, tOut, err :=
		pm.generatePayoutTxDetails(ctx, txC, feeAddr, pmts, treasuryActive)
	if err != nil {
		return err
	}

	_, estFee, err := pm.applyTxFees(inputs, outputs, tOut, feeAddr)
	if err != nil {
		return err
	}

	// Generate the transaction output set.
	outs := make(map[dcrutil.Address]dcrutil.Amount, len(outputs))
	for sAddr, amt := range outputs {
		addr, err := dcrutil.DecodeAddress(sAddr, pm.cfg.ActiveNet)
		if err != nil {
			desc := fmt.Sprintf("%s: unable to decode payout address: %v",
				funcName, err)
			return errs.PoolError(errs.Decode, desc)
		}
		outs[addr] = amt
	}

	// Ensure the wallet is aware of all the coinbase outputs being
	// spent by the payout transaction.
	var maxSpendableHeight uint32
	for _, pmtSet := range pmts {
		spendableHeight := pmtSet[0].EstimatedMaturity + 1
		if maxSpendableHeight < spendableHeight {
			maxSpendableHeight = spendableHeight
		}
	}
	if maxSpendableHeight < height {
		maxSpendableHeight = height
	}

	tCtx, tCancel := context.WithTimeout(ctx, pm.cfg.CoinbaseConfTimeout)
	defer tCancel()
	err = pm.confirmCoinbases(tCtx, inputTxHashes, maxSpendableHeight)
	if err != nil {
		// Do not error if coinbase spendable confirmation requests are
		// terminated by the context cancellation.
		if !errors.Is(err, errs.ContextCancelled) {
			return err
		}

		return nil
	}

	// Create, sign and publish the payout transaction.
	tx, err := txC.CreateRawTransaction(ctx, inputs, outs, nil, nil)
	if err != nil {
		desc := fmt.Sprintf("%s: unable to create transaction: %v",
			funcName, err)
		return errs.PoolError(errs.CreateTx, desc)
	}
	txBytes, err := tx.Bytes()
	if err != nil {
		return err
	}

	txB := pm.cfg.FetchTxBroadcaster()
	if txB == nil {
		desc := fmt.Sprintf("%s: tx broadcaster cannot be nil", funcName)
		return errs.PoolError(errs.Disconnected, desc)
	}
	signTxReq := &walletrpc.SignTransactionRequest{
		SerializedTransaction: txBytes,
		Passphrase:            []byte(pm.cfg.WalletPass),
	}
	signedTxResp, err := txB.SignTransaction(ctx, signTxReq)
	if err != nil {
		desc := fmt.Sprintf("%s: unable to sign transaction: %v",
			funcName, err)
		return errs.PoolError(errs.SignTx, desc)

	}

	pubTxReq := &walletrpc.PublishTransactionRequest{
		SignedTransaction: signedTxResp.Transaction,
	}
	pubTxResp, err := txB.PublishTransaction(ctx, pubTxReq)
	if err != nil {
		desc := fmt.Sprintf("%s: unable to publish transaction: %v",
			funcName, err)
		return errs.PoolError(errs.PublishTx, desc)
	}

	txid, err := chainhash.NewHash(pubTxResp.TransactionHash)
	if err != nil {
		desc := fmt.Sprintf("unable to create transaction hash: %v", err)
		return errs.PoolError(errs.CreateHash, desc)
	}
	fees := outputs[feeAddr.String()]

	log.Infof("paid a total of %v in tx %s, including %v in pool fees. "+
		"Tx fee: %v", tOut, txid.String(), fees, estFee)

	// Update all associated payments as paid and archive them.
	for _, set := range pmts {
		for _, pmt := range set {
			pmt.PaidOnHeight = height
			pmt.TransactionID = txid.String()
			err := pm.cfg.db.updatePayment(pmt)
			if err != nil {
				desc := fmt.Sprintf("%s: unable to update payment: %v",
					funcName, err)
				return errs.PoolError(errs.PersistEntry, desc)
			}
			err = pm.cfg.db.ArchivePayment(pmt)
			if err != nil {
				desc := fmt.Sprintf("%s: unable to archive payment: %v",
					funcName, err)
				return errs.PoolError(errs.PersistEntry, desc)
			}
		}
	}

	// Update payments metadata.
	err = pm.cfg.db.persistLastPaymentInfo(height, time.Now().UnixNano())
	if err != nil {
		return err
	}

	return nil
}
