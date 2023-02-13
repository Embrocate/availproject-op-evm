package avail

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/0xPolygon/polygon-edge/blockchain"
	"github.com/0xPolygon/polygon-edge/consensus"
	"github.com/0xPolygon/polygon-edge/state"
	"github.com/0xPolygon/polygon-edge/txpool"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	avail_types "github.com/centrifuge/go-substrate-rpc-client/v4/types"
	stypes "github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/hashicorp/go-hclog"
	"github.com/maticnetwork/avail-settlement/consensus/avail/validator"
	"github.com/maticnetwork/avail-settlement/pkg/avail"
	"github.com/maticnetwork/avail-settlement/pkg/block"
	"github.com/maticnetwork/avail-settlement/pkg/staking"
)

type transitionInterface interface {
	Write(txn *types.Transaction) error
}

type SequencerWorker struct {
	logger       hclog.Logger
	blockchain   *blockchain.Blockchain
	executor     *state.Executor
	validator    validator.Validator
	txpool       *txpool.TxPool
	apq          staking.ActiveParticipants
	availAppID   avail_types.U32
	availClient  avail.Client
	availAccount signature.KeyringPair
	nodeSignKey  *ecdsa.PrivateKey
	nodeAddr     types.Address
	nodeType     MechanismType
	stakingNode  staking.Node
	availSender  avail.Sender
	closeCh      chan struct{}
	blockTime    time.Duration // Minimum block generation time in seconds
}

func (sw *SequencerWorker) Run(account accounts.Account, key *keystore.Key) error {
	t := new(atomic.Int64)
	activeSequencersQuerier := staking.NewRandomizedActiveSequencersQuerier(t.Load, sw.apq)
	validator := validator.New(sw.blockchain, sw.executor, sw.nodeAddr)
	fraudResolver := NewFraudResolver(sw.logger, sw.blockchain, sw.executor, validator, sw.nodeAddr, sw.nodeSignKey, sw.availSender)

	accBalance, err := avail.GetBalance(sw.availClient, sw.availAccount)
	if err != nil {
		return fmt.Errorf("failed to discover account balance: %s", err)
	}

	sw.logger.Info("Current avail account", "balance", accBalance.Int64())

	callIdx, err := avail.FindCallIndex(sw.availClient)
	if err != nil {
		return fmt.Errorf("failed to discover avail call index: %s", err)
	}

	// Will wait until contract is updated and there's a staking transaction written
	sw.waitForStakedSequencer(activeSequencersQuerier, sw.nodeAddr)

	// BlockStream watcher must be started after the staking is done. Otherwise
	// the stream is out-of-sync.
	availBlockStream := avail.NewBlockStream(sw.availClient, sw.logger, 0)
	defer availBlockStream.Close()

	sw.logger.Info("Block stream successfully started.", "node_type", sw.nodeType)

	for {
		select {
		case blk := <-availBlockStream.Chan():
			// Time `t` is [mostly] monotonic clock, backed by Avail. It's used for all
			// time sensitive logic in sequencer, such as block generation timeouts.
			t.Store(int64(blk.Block.Header.Number))

			// So this is the situation...
			// Here we are not looking for if current node should be producing or not producing the block.
			// What we are interested, prior to fraud resolver, if block is containing fraud check request.
			// That and only that we're looking at this stage...

			edgeBlks, err := block.FromAvail(blk, sw.availAppID, callIdx, sw.logger)
			if len(edgeBlks) == 0 && err != nil {
				sw.logger.Error("cannot extract Edge block from Avail block", "block_number", blk.Block.Header.Number, "error", err)
				// It is expected that each edge block should contain avail block, however,
				// this can block the entire production of the blocks later on.
				// Continue if the decompiling block resulted in any error other than not found.
				if err != block.ErrNoExtrinsicFound {
					continue
				}
			}

			for _, blockk := range edgeBlks {
				sw.blockchain.WriteBlock(blockk, sw.nodeType.String())
			}

			// Go through the blocks from avail and make sure to set fraud block in case it was discovered...
			fraudResolver.CheckAndSetFraudBlock(edgeBlks)

			// Will check the block for fraudlent behaviour and slash parties accordingly.
			// WARN: Continue will hard stop whole network from producing blocks until dispute is resolved.
			// We can change this in the future but with it, understand that series of issues are going to
			// happen with syncing and publishing of the blocks that will need to be fixed.
			if _, err := fraudResolver.CheckAndSlash(); err != nil {
				continue
			}

			// Periodically verify that we are staked, before proceeding with sequencer
			// logic. In the unexpected case of being slashed and dropping below the
			// required sequencer staking threshold, we must stop processing, because
			// otherwise we just get slashed more.
			sequencerStaked, sequencerError := activeSequencersQuerier.Contains(sw.nodeAddr)
			if sequencerError != nil {
				sw.logger.Error("failed to check if my account is among active staked sequencers; cannot continue", "err", sequencerError)
				continue
			}

			if !sequencerStaked {
				sw.logger.Error("my account is not among active staked sequencers; cannot continue", "address", sw.nodeAddr.String())
				continue
			}

			sequencers, err := activeSequencersQuerier.Get()
			if err != nil {
				sw.logger.Error("querying staked sequencers failed; quitting", "error", err)
				continue
			}

			if len(sequencers) == 0 {
				// This is something that should **never** happen.
				panic("no staked sequencers")
			}

			// Is it my turn to generate next block?
			if bytes.Equal(sequencers[0].Bytes(), sw.nodeAddr.Bytes()) {
				header := sw.blockchain.Header()
				sw.logger.Info("it's my turn; producing a block", "t", blk.Block.Header.Number)
				if err := sw.writeBlock(account, key, header); err != nil {
					sw.logger.Error("failed to mine block", "err", err)
				}
				continue
			} else {
				sw.logger.Info("it's not my turn; skippin' a round", "t", blk.Block.Header.Number)
			}

		case <-sw.closeCh:
			if err := sw.stakingNode.UnStake(sw.nodeSignKey); err != nil {
				sw.logger.Error("failed to unstake the node", "error", err)
				return err
			}
			return nil
		}
	}
}

// writeNewBLock generates a new block based on transactions from the pool,
// and writes them to the blockchain
func (sw *SequencerWorker) writeBlock(myAccount accounts.Account, signKey *keystore.Key, parent *types.Header) error {
	header := &types.Header{
		ParentHash: parent.Hash,
		Number:     parent.Number + 1,
		Miner:      myAccount.Address.Bytes(),
		Nonce:      types.Nonce{},
		GasLimit:   parent.GasLimit, // Inherit from parent for now, will need to adjust dynamically later.
		Timestamp:  uint64(time.Now().Unix()),
	}

	// calculate gas limit based on parent header
	gasLimit, err := sw.blockchain.CalculateGasLimit(header.Number)
	if err != nil {
		return err
	}

	header.GasLimit = gasLimit

	// set the timestamp
	parentTime := time.Unix(int64(parent.Timestamp), 0)
	headerTime := parentTime.Add(sw.blockTime)

	if headerTime.Before(time.Now()) {
		headerTime = time.Now()
	}

	header.Timestamp = uint64(headerTime.Unix())

	// we need to include in the extra field the current set of validators
	err = block.AssignExtraValidators(header, ValidatorSet{types.StringToAddress(myAccount.Address.Hex())})
	if err != nil {
		return err
	}

	transition, err := sw.executor.BeginTxn(parent.StateRoot, header, types.StringToAddress(myAccount.Address.Hex()))
	if err != nil {
		return err
	}

	txns := sw.writeTransactions(gasLimit, transition)

	// Commit the changes
	_, root := transition.Commit()

	// Update the header
	header.StateRoot = root
	header.GasUsed = transition.TotalGas()

	// Build the actual block
	// The header hash is computed inside buildBlock
	blk := consensus.BuildBlock(consensus.BuildBlockParams{
		Header:   header,
		Txns:     txns,
		Receipts: transition.Receipts(),
	})

	// write the seal of the block after all the fields are completed
	header, err = block.WriteSeal(signKey.PrivateKey, blk.Header)
	if err != nil {
		return err
	}

	//if header.Number == 5 {
	//	header.ExtraData = []byte{1, 2, 3}
	//}

	// Corrupt miner -> fraud check.
	//header.Miner = types.ZeroAddress.Bytes()

	blk.Header = header

	// compute the hash, this is only a provisional hash since the final one
	// is sealed after all the committed seals
	blk.Header.ComputeHash()

	sw.logger.Info("sending block to avail")

	err = sw.availSender.SendAndWaitForStatus(blk, stypes.ExtrinsicStatus{IsInBlock: true})
	if err != nil {
		sw.logger.Error("Error while submitting data to avail", "error", err)
		return err
	}

	sw.logger.Info("sent block to avail")
	sw.logger.Info("writing block to blockchain")

	// Write the block to the blockchain
	if err := sw.blockchain.WriteBlock(blk, sw.nodeType.String()); err != nil {
		return err
	}

	sw.logger.Info("Successfully wrote block to blockchain", "number", blk.Number(), "hash", blk.Hash(), "parent_hash", blk.ParentHash())

	// after the block has been written we reset the txpool so that
	// the old transactions are removed
	sw.txpool.ResetWithHeaders(blk.Header)

	return nil
}

func (sw *SequencerWorker) writeTransactions(gasLimit uint64, transition transitionInterface) []*types.Transaction {
	var successful []*types.Transaction

	sw.txpool.Prepare()

	for {
		tx := sw.txpool.Peek()
		if tx == nil {
			break
		}

		sw.logger.Debug("found transaction from txpool", "hash", tx.Hash.String())

		if tx.ExceedsBlockGasLimit(gasLimit) {
			sw.txpool.Drop(tx)
			continue
		}

		if err := transition.Write(tx); err != nil {
			if _, ok := err.(*state.GasLimitReachedTransitionApplicationError); ok { // nolint:errorlint
				sw.logger.Warn("transaction reached gas limit during excution", "hash", tx.Hash.String())
				break
			} else if appErr, ok := err.(*state.TransitionApplicationError); ok && appErr.IsRecoverable { // nolint:errorlint
				sw.logger.Warn("transaction caused application error", "hash", tx.Hash.String())
				sw.txpool.Demote(tx)
			} else {
				sw.logger.Error("transaction caused unknown error", "error", err)
				sw.txpool.Drop(tx)
			}

			continue
		}

		// no errors, pop the tx from the pool
		sw.txpool.Pop(tx)

		successful = append(successful, tx)
	}

	return successful
}

func NewSequencer(
	logger hclog.Logger, b *blockchain.Blockchain, e *state.Executor, txp *txpool.TxPool, v validator.Validator, availClient avail.Client,
	availAccount signature.KeyringPair, availAppID avail_types.U32,
	nodeSignKey *ecdsa.PrivateKey, nodeAddr types.Address, nodeType MechanismType,
	apq staking.ActiveParticipants, stakingNode staking.Node, availSender avail.Sender, closeCh <-chan struct{}, blockTime time.Duration) (*SequencerWorker, error) {
	return &SequencerWorker{
		logger:       logger,
		blockchain:   b,
		executor:     e,
		validator:    v,
		txpool:       txp,
		apq:          apq,
		availAppID:   availAppID,
		availClient:  availClient,
		availAccount: availAccount,
		nodeSignKey:  nodeSignKey,
		nodeAddr:     nodeAddr,
		nodeType:     nodeType,
		stakingNode:  stakingNode,
		availSender:  availSender,
		blockTime:    blockTime,
	}, nil
}
