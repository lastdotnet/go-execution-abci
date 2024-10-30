package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmbytes "github.com/cometbft/cometbft/libs/bytes"
	cmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/cometbft/cometbft/proxy"
	cmtypes "github.com/cometbft/cometbft/types"
	execution "github.com/rollkit/go-execution"
	proxyJsonrpc "github.com/rollkit/go-execution/proxy/jsonrpc"
	"github.com/rollkit/rollkit/mempool"
	"github.com/rollkit/rollkit/state"
	"github.com/rollkit/rollkit/store"
	"github.com/rollkit/rollkit/third_party/log"
	"github.com/rollkit/rollkit/types"
	abciconv "github.com/rollkit/rollkit/types/abci"
)

// ErrEmptyValSetGenerated is returned when applying the validator changes would result in empty set.
var ErrEmptyValSetGenerated = errors.New("applying the validator changes would result in empty set")

// ErrAddingValidatorToBased is returned when trying to add a validator to an empty validator set.
var ErrAddingValidatorToBased = errors.New("cannot add validators to empty validator set")

// Ensure ProxyClient implements Execute interface
var _ execution.Execute = (*ProxyClient)(nil)

// ProxyClient implements the Execute interface in go-execution
type ProxyClient struct {
	client *proxyJsonrpc.Client
	// abci specific
	proxyApp        proxy.AppConnConsensus
	eventBus        *cmtypes.EventBus
	genesis         *cmtypes.GenesisDoc
	maxBytes        uint64
	proposerAddress []byte
	chainID         string

	// rollkit specific
	mempool       mempool.Mempool
	mempoolReaper *mempool.CListMempoolReaper
	logger        log.Logger
	metrics       *state.Metrics
	state         *types.State
	store         store.Store
}

func NewClient(client *proxyJsonrpc.Client, proxyApp proxy.AppConnConsensus, eventBus *cmtypes.EventBus, genesis *cmtypes.GenesisDoc, maxBytes uint64, proposerAddress []byte, chainID string, mempool mempool.Mempool, mempoolReaper *mempool.CListMempoolReaper, logger log.Logger, metrics *state.Metrics, state *types.State, store store.Store) *ProxyClient {
	return &ProxyClient{
		client:          client,
		proxyApp:        proxyApp,
		eventBus:        eventBus,
		genesis:         genesis,
		maxBytes:        maxBytes,
		proposerAddress: proposerAddress,
		chainID:         chainID,
		mempool:         mempool,
		mempoolReaper:   mempoolReaper,
		logger:          logger,
		metrics:         metrics,
		state:           state,
		store:           store,
	}
}

// InitChain initializes the blockchain with genesis information.
func (c *ProxyClient) InitChain(
	genesisTime time.Time,
	initialHeight uint64,
	chainID string,
) (types.Hash, uint64, error) {
	genesis := &cmtypes.GenesisDoc{
		GenesisTime:     genesisTime,
		ChainID:         chainID,
		ConsensusParams: c.genesis.ConsensusParams,
		Validators:      c.genesis.Validators,
		AppState:        c.genesis.AppState,
		InitialHeight:   int64(initialHeight),
	}

	response, err := c.initChain(genesis)
	if err != nil {
		return types.Hash{}, 0, err
	}

	stateRoot := types.Hash(response.AppHash)
	maxBytes := response.ConsensusParams.Block.MaxBytes

	return stateRoot, uint64(maxBytes), nil
}

// initChain calls InitChainSync using consensus connection to app.
func (c *ProxyClient) initChain(genesis *cmtypes.GenesisDoc) (*abci.ResponseInitChain, error) {
	params := genesis.ConsensusParams

	validators := make([]*cmtypes.Validator, len(genesis.Validators))
	for i, v := range genesis.Validators {
		validators[i] = cmtypes.NewValidator(v.PubKey, v.Power)
	}

	return c.proxyApp.InitChain(context.Background(), &abci.RequestInitChain{
		Time:    genesis.GenesisTime,
		ChainId: genesis.ChainID,
		ConsensusParams: &cmproto.ConsensusParams{
			Block: &cmproto.BlockParams{
				MaxBytes: params.Block.MaxBytes,
				MaxGas:   params.Block.MaxGas,
			},
			Evidence: &cmproto.EvidenceParams{
				MaxAgeNumBlocks: params.Evidence.MaxAgeNumBlocks,
				MaxAgeDuration:  params.Evidence.MaxAgeDuration,
				MaxBytes:        params.Evidence.MaxBytes,
			},
			Validator: &cmproto.ValidatorParams{
				PubKeyTypes: params.Validator.PubKeyTypes,
			},
			Version: &cmproto.VersionParams{
				App: params.Version.App,
			},
			Abci: &cmproto.ABCIParams{
				VoteExtensionsEnableHeight: params.ABCI.VoteExtensionsEnableHeight,
			},
		},
		Validators:    cmtypes.TM2PB.ValidatorUpdates(cmtypes.NewValidatorSet(validators)),
		AppStateBytes: genesis.AppState,
		InitialHeight: genesis.InitialHeight,
	})
}

// GetTxs retrieves all available transactions from the mempool.
func (c *ProxyClient) GetTxs() ([]types.Tx, error) {
	state, err := c.store.GetState(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get current state: %w", err)
	}

	maxBytes := state.ConsensusParams.Block.MaxBytes
	if maxBytes == -1 {
		maxBytes = int64(cmtypes.MaxBlockSizeBytes)
	}
	if maxBytes > int64(c.maxBytes) {
		c.logger.Debug("limiting maxBytes to", "maxBytes", c.maxBytes)
		maxBytes = int64(c.maxBytes)
	}

	cmTxs := c.mempool.ReapMaxTxs(int(maxBytes))

	rollkitTxs := make([]types.Tx, len(cmTxs))
	for i, tx := range cmTxs {
		rollkitTxs[i] = types.Tx(tx)
	}

	return rollkitTxs, nil
}

// ExecuteTxs executes a set of transactions to produce a new block.
func (c *ProxyClient) ExecuteTxs(
	txs []types.Tx,
	blockHeight uint64,
	timestamp time.Time,
	prevStateRoot types.Hash,
) (types.Hash, uint64, error) {
	ctx := context.Background()

	state, err := c.store.GetState(ctx)
	if err != nil {
		return types.Hash{}, 0, fmt.Errorf("failed to get current state: %w", err)
	}

	cmTxs := fromRollkitTxs(txs)

	var lastSignature *types.Signature
	var lastHeaderHash types.Hash
	var lastExtendedCommit abci.ExtendedCommitInfo

	if blockHeight == uint64(c.genesis.InitialHeight) {
		lastSignature = &types.Signature{}
		lastHeaderHash = types.Hash{}
		lastExtendedCommit = abci.ExtendedCommitInfo{}
	} else {
		lastSignature, err = c.store.GetSignature(ctx, blockHeight-1)
		if err != nil {
			return types.Hash{}, 0, fmt.Errorf("error while loading last commit: %w", err)
		}

		lastHeader, _, err := c.store.GetBlockData(ctx, blockHeight-1)
		if err != nil {
			return types.Hash{}, 0, fmt.Errorf("error while loading last block: %w", err)
		}
		lastHeaderHash = lastHeader.Hash()

		extCommit, err := c.store.GetExtendedCommit(ctx, blockHeight-1)
		if err != nil {
			return types.Hash{}, 0, fmt.Errorf("failed to load extended commit for height %d: %w", blockHeight-1, err)
		}
		if extCommit != nil {
			lastExtendedCommit = *extCommit
		}
	}

	header, data, err := c.CreateBlock(
		blockHeight,
		lastSignature,
		lastExtendedCommit,
		lastHeaderHash,
		state,
		cmTxs,
		timestamp,
	)
	if err != nil {
		return types.Hash{}, 0, fmt.Errorf("failed to create block: %w", err)
	}

	isValid, err := c.ProcessProposal(header, data, state)
	if err != nil {
		return types.Hash{}, 0, fmt.Errorf("failed to process proposal: %w", err)
	}
	if !isValid {
		return types.Hash{}, 0, fmt.Errorf("proposal was not valid")
	}

	newState, resp, err := c.ApplyBlock(ctx, state, header, data)
	if err != nil {
		return types.Hash{}, 0, fmt.Errorf("failed to apply block: %w", err)
	}

	appHash, _, err := c.Commit(ctx, newState, header, data, resp)
	if err != nil {
		return types.Hash{}, 0, fmt.Errorf("failed to commit: %w", err)
	}

	return types.Hash(appHash), uint64(newState.ConsensusParams.Block.MaxBytes), nil
}

// SetFinal marks a block at the given height as final.
func (c *ProxyClient) SetFinal(blockHeight uint64) error {
	ctx := context.Background()

	header, data, err := c.store.GetBlockData(ctx, blockHeight)
	if err != nil {
		return fmt.Errorf("failed to get block data for height %d: %w", blockHeight, err)
	}

	state, err := c.store.GetState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current state: %w", err)
	}

	resp, err := c.proxyApp.FinalizeBlock(ctx, &abci.RequestFinalizeBlock{
		Hash:               header.Hash(),
		Height:             int64(blockHeight),
		Time:               header.Time(),
		Txs:                data.Txs.ToSliceOfBytes(),
		ProposerAddress:    header.ProposerAddress,
		NextValidatorsHash: state.Validators.Hash(),
	})
	if err != nil {
		return fmt.Errorf("failed to finalize block at height %d: %w", blockHeight, err)
	}

	state.AppHash = resp.AppHash
	if err := c.store.UpdateState(ctx, state); err != nil {
		return fmt.Errorf("failed to update state after finalizing block %d: %w", blockHeight, err)
	}

	c.logger.Info("Block finalized", "height", blockHeight, "hash", header.Hash())

	return nil
}

// CreateBlock reaps transactions from mempool and builds a block.
func (c *ProxyClient) CreateBlock(height uint64, lastSignature *types.Signature, lastExtendedCommit abci.ExtendedCommitInfo, lastHeaderHash types.Hash, state types.State, txs cmtypes.Txs, timestamp time.Time) (*types.SignedHeader, *types.Data, error) {
	maxBytes := state.ConsensusParams.Block.MaxBytes
	emptyMaxBytes := maxBytes == -1
	if emptyMaxBytes {
		maxBytes = int64(cmtypes.MaxBlockSizeBytes)
	}
	if maxBytes > int64(c.maxBytes) { //nolint:gosec
		c.logger.Debug("limiting maxBytes to", "e.maxBytes=%d", c.maxBytes)
		maxBytes = int64(c.maxBytes) //nolint:gosec
	}

	header := &types.SignedHeader{
		Header: types.Header{
			Version: types.Version{
				Block: state.Version.Consensus.Block,
				App:   state.Version.Consensus.App,
			},
			BaseHeader: types.BaseHeader{
				ChainID: c.chainID,
				Height:  height,
				Time:    uint64(timestamp.UnixNano()), //nolint:gosec
			},
			DataHash:        make(types.Hash, 32),
			ConsensusHash:   make(types.Hash, 32),
			AppHash:         state.AppHash,
			LastResultsHash: state.LastResultsHash,
			ProposerAddress: c.proposerAddress,
		},
		Signature: *lastSignature,
	}
	data := &types.Data{
		Txs: toRollkitTxs(txs),
		// IntermediateStateRoots: types.IntermediateStateRoots{RawRootsList: nil},
		// Note: Temporarily remove Evidence #896
		// Evidence:               types.EvidenceData{Evidence: nil},
	}

	rpp, err := c.proxyApp.PrepareProposal(
		context.TODO(),
		&abci.RequestPrepareProposal{
			MaxTxBytes:         maxBytes,
			Txs:                txs.ToSliceOfBytes(),
			LocalLastCommit:    lastExtendedCommit,
			Misbehavior:        []abci.Misbehavior{},
			Height:             int64(header.Height()), //nolint:gosec
			Time:               header.Time(),          //TODO: replace with sequencer timestamp
			NextValidatorsHash: state.Validators.Hash(),
			ProposerAddress:    c.proposerAddress,
		},
	)
	if err != nil {
		// The App MUST ensure that only valid (and hence 'processable') transactions
		// enter the mempool. Hence, at this point, we can't have any non-processable
		// transaction causing an error.
		//
		// Also, the App can simply skip any transaction that could cause any kind of trouble.
		// Either way, we cannot recover in a meaningful way, unless we skip proposing
		// this block, repair what caused the error and try again. Hence, we return an
		// error for now (the production code calling this function is expected to panic).
		return nil, nil, err
	}

	txl := cmtypes.ToTxs(rpp.Txs)
	if err := txl.Validate(maxBytes); err != nil {
		return nil, nil, err
	}

	data.Txs = toRollkitTxs(txl)
	// Note: This is hash of an ABCI type commit equivalent of the last signature in the signed header.
	header.LastCommitHash = lastSignature.GetCommitHash(&header.Header, c.proposerAddress)
	header.LastHeaderHash = lastHeaderHash

	return header, data, nil
}

// ProcessProposal calls the corresponding ABCI method on the app.
func (c *ProxyClient) ProcessProposal(
	header *types.SignedHeader,
	data *types.Data,
	state types.State,
) (bool, error) {
	resp, err := c.proxyApp.ProcessProposal(context.TODO(), &abci.RequestProcessProposal{
		Hash:   header.Hash(),
		Height: int64(header.Height()), //nolint:gosec
		Time:   header.Time(),
		Txs:    data.Txs.ToSliceOfBytes(),
		ProposedLastCommit: abci.CommitInfo{
			Round: 0,
			Votes: []abci.VoteInfo{
				{
					Validator: abci.Validator{
						Address: header.Validators.GetProposer().Address,
						Power:   header.Validators.GetProposer().VotingPower,
					},
					BlockIdFlag: cmproto.BlockIDFlagCommit,
				},
			},
		},
		Misbehavior:        []abci.Misbehavior{},
		ProposerAddress:    c.proposerAddress,
		NextValidatorsHash: state.Validators.Hash(),
	})
	if err != nil {
		return false, err
	}
	if resp.IsStatusUnknown() {
		panic(fmt.Sprintf("ProcessProposal responded with status %s", resp.Status.String()))
	}

	return resp.IsAccepted(), nil
}

// ApplyBlock validates and executes the block.
func (c *ProxyClient) ApplyBlock(ctx context.Context, state types.State, header *types.SignedHeader, data *types.Data) (types.State, *abci.ResponseFinalizeBlock, error) {
	isAppValid, err := c.ProcessProposal(header, data, state)
	if err != nil {
		return types.State{}, nil, err
	}
	if !isAppValid {
		return types.State{}, nil, fmt.Errorf("proposal processing resulted in an invalid application state")
	}

	err = c.Validate(state, header, data)
	if err != nil {
		return types.State{}, nil, err
	}
	// This makes calls to the AppClient
	resp, err := c.execute(ctx, state, header, data)
	if err != nil {
		return types.State{}, nil, err
	}
	abciValUpdates := resp.ValidatorUpdates

	validatorUpdates, err := cmtypes.PB2TM.ValidatorUpdates(abciValUpdates)
	if err != nil {
		return state, nil, err
	}

	if resp.ConsensusParamUpdates != nil {
		c.metrics.ConsensusParamUpdates.Add(1)
	}

	state, err = c.updateState(state, header, data, resp, validatorUpdates)
	if err != nil {
		return types.State{}, nil, err
	}

	if state.ConsensusParams.Block.MaxBytes <= 0 {
		c.logger.Error("maxBytes<=0", "state.ConsensusParams.Block", state.ConsensusParams.Block, "header", header)
	}

	return state, resp, nil
}

// ExtendVote calls the ExtendVote ABCI method on the proxy app.
func (c *ProxyClient) ExtendVote(ctx context.Context, header *types.SignedHeader, data *types.Data) ([]byte, error) {
	resp, err := c.proxyApp.ExtendVote(ctx, &abci.RequestExtendVote{
		Hash:   header.Hash(),
		Height: int64(header.Height()), //nolint:gosec
		Time:   header.Time(),
		Txs:    data.Txs.ToSliceOfBytes(),
		ProposedLastCommit: abci.CommitInfo{
			Votes: []abci.VoteInfo{{
				Validator: abci.Validator{
					Address: header.Validators.GetProposer().Address,
					Power:   header.Validators.GetProposer().VotingPower,
				},
				BlockIdFlag: cmproto.BlockIDFlagCommit,
			}},
		},
		Misbehavior:        nil,
		NextValidatorsHash: header.ValidatorHash,
		ProposerAddress:    header.ProposerAddress,
	})
	if err != nil {
		return nil, err
	}
	return resp.VoteExtension, nil
}

// Commit commits the block
func (c *ProxyClient) Commit(ctx context.Context, state types.State, header *types.SignedHeader, data *types.Data, resp *abci.ResponseFinalizeBlock) ([]byte, uint64, error) {
	appHash, retainHeight, err := c.commit(ctx, state, header, data, resp)
	if err != nil {
		return []byte{}, 0, err
	}

	state.AppHash = appHash

	c.publishEvents(resp, header, data, state)

	return appHash, retainHeight, nil
}

// updateConsensusParams updates the consensus parameters based on the provided updates.
func (c *ProxyClient) updateConsensusParams(height uint64, params cmtypes.ConsensusParams, consensusParamUpdates *cmproto.ConsensusParams) (cmproto.ConsensusParams, uint64, error) {
	nextParams := params.Update(consensusParamUpdates)
	if err := types.ConsensusParamsValidateBasic(nextParams); err != nil {
		return cmproto.ConsensusParams{}, 0, fmt.Errorf("validating new consensus params: %w", err)
	}
	if err := nextParams.ValidateUpdate(consensusParamUpdates, int64(height)); err != nil { //nolint:gosec
		return cmproto.ConsensusParams{}, 0, fmt.Errorf("updating consensus params: %w", err)
	}
	return nextParams.ToProto(), nextParams.Version.App, nil
}

func (c *ProxyClient) updateState(state types.State, header *types.SignedHeader, data *types.Data, finalizeBlockResponse *abci.ResponseFinalizeBlock, validatorUpdates []*cmtypes.Validator) (types.State, error) {
	height := header.Height()
	if finalizeBlockResponse.ConsensusParamUpdates != nil {
		nextParamsProto, appVersion, err := c.updateConsensusParams(height, types.ConsensusParamsFromProto(state.ConsensusParams), finalizeBlockResponse.ConsensusParamUpdates)
		if err != nil {
			return types.State{}, err
		}
		// Change results from this height but only applies to the next height.
		state.LastHeightConsensusParamsChanged = height + 1
		state.Version.Consensus.App = appVersion
		state.ConsensusParams = nextParamsProto
	}

	nValSet := state.NextValidators.Copy()
	lastHeightValSetChanged := state.LastHeightValidatorsChanged

	if len(nValSet.Validators) > 0 {
		err := nValSet.UpdateWithChangeSet(validatorUpdates)
		if err != nil {
			if err.Error() != ErrEmptyValSetGenerated.Error() {
				return state, err
			}
			nValSet = &cmtypes.ValidatorSet{
				Validators: make([]*cmtypes.Validator, 0),
				Proposer:   nil,
			}
		}
		// Change results from this height but only applies to the next next height.
		lastHeightValSetChanged = int64(header.Header.Height() + 1 + 1) //nolint:gosec

		if len(nValSet.Validators) > 0 {
			nValSet.IncrementProposerPriority(1)
		}
	}

	s := types.State{
		Version:         state.Version,
		ChainID:         state.ChainID,
		InitialHeight:   state.InitialHeight,
		LastBlockHeight: height,
		LastBlockTime:   header.Time(),
		LastBlockID: cmtypes.BlockID{
			Hash: cmbytes.HexBytes(header.Hash()),
			// for now, we don't care about part set headers
		},
		ConsensusParams:                  state.ConsensusParams,
		LastHeightConsensusParamsChanged: state.LastHeightConsensusParamsChanged,
		AppHash:                          finalizeBlockResponse.AppHash,
		Validators:                       state.NextValidators.Copy(),
		NextValidators:                   nValSet,
		LastHeightValidatorsChanged:      lastHeightValSetChanged,
		LastValidators:                   state.Validators.Copy(),
	}
	copy(s.LastResultsHash[:], cmtypes.NewResults(finalizeBlockResponse.TxResults).Hash())

	return s, nil
}

func (c *ProxyClient) commit(ctx context.Context, state types.State, header *types.SignedHeader, data *types.Data, resp *abci.ResponseFinalizeBlock) ([]byte, uint64, error) {
	c.mempool.Lock()
	defer c.mempool.Unlock()

	err := c.mempool.FlushAppConn()
	if err != nil {
		return nil, 0, err
	}

	commitResp, err := c.proxyApp.Commit(ctx)
	if err != nil {
		return nil, 0, err
	}

	maxBytes := state.ConsensusParams.Block.MaxBytes
	maxGas := state.ConsensusParams.Block.MaxGas
	cTxs := fromRollkitTxs(data.Txs)
	c.mempoolReaper.UpdateCommitedTxs(cTxs)
	err = c.mempool.Update(header.Height(), cTxs, resp.TxResults, mempool.PreCheckMaxBytes(maxBytes), mempool.PostCheckMaxGas(maxGas))
	if err != nil {
		return nil, 0, err
	}

	return resp.AppHash, uint64(commitResp.RetainHeight), err //nolint:gosec
}

// Validate validates the state and the block for the executor
func (c *ProxyClient) Validate(state types.State, header *types.SignedHeader, data *types.Data) error {
	if err := header.ValidateBasic(); err != nil {
		return err
	}
	if err := data.ValidateBasic(); err != nil {
		return err
	}
	if err := types.Validate(header, data); err != nil {
		return err
	}
	if header.Version.App != state.Version.Consensus.App ||
		header.Version.Block != state.Version.Consensus.Block {
		return errors.New("block version mismatch")
	}
	if state.LastBlockHeight <= 0 && header.Height() != state.InitialHeight {
		return errors.New("initial block height mismatch")
	}
	if state.LastBlockHeight > 0 && header.Height() != state.LastBlockHeight+1 {
		return errors.New("block height mismatch")
	}
	if !bytes.Equal(header.AppHash[:], state.AppHash[:]) {
		return errors.New("AppHash mismatch")
	}

	if !bytes.Equal(header.LastResultsHash[:], state.LastResultsHash[:]) {
		return errors.New("LastResultsHash mismatch")
	}

	return nil
}

func (c *ProxyClient) execute(ctx context.Context, state types.State, header *types.SignedHeader, data *types.Data) (*abci.ResponseFinalizeBlock, error) {
	// Only execute if the node hasn't already shut down
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	abciHeader, err := abciconv.ToABCIHeaderPB(&header.Header)
	if err != nil {
		return nil, err
	}
	abciHeader.ChainID = c.chainID
	abciBlock, err := abciconv.ToABCIBlock(header, data)
	if err != nil {
		return nil, err
	}

	startTime := time.Now().UnixNano()
	finalizeBlockResponse, err := c.proxyApp.FinalizeBlock(ctx, &abci.RequestFinalizeBlock{
		Hash:               header.Hash(),
		NextValidatorsHash: state.Validators.Hash(),
		ProposerAddress:    abciHeader.ProposerAddress,
		Height:             abciHeader.Height,
		Time:               abciHeader.Time,
		DecidedLastCommit: abci.CommitInfo{
			Round: 0,
			Votes: nil,
		},
		Misbehavior: abciBlock.Evidence.Evidence.ToABCI(),
		Txs:         abciBlock.Txs.ToSliceOfBytes(),
	})
	endTime := time.Now().UnixNano()
	c.metrics.BlockProcessingTime.Observe(float64(endTime-startTime) / 1000000)
	if err != nil {
		c.logger.Error("error in proxyAppConn.FinalizeBlock", "err", err)
		return nil, err
	}

	c.logger.Info(
		"finalized block",
		"height", abciBlock.Height,
		"num_txs_res", len(finalizeBlockResponse.TxResults),
		"num_val_updates", len(finalizeBlockResponse.ValidatorUpdates),
		"block_app_hash", fmt.Sprintf("%X", finalizeBlockResponse.AppHash),
	)

	// Assert that the application correctly returned tx results for each of the transactions provided in the block
	if len(abciBlock.Data.Txs) != len(finalizeBlockResponse.TxResults) {
		return nil, fmt.Errorf("expected tx results length to match size of transactions in block. Expected %d, got %d", len(data.Txs), len(finalizeBlockResponse.TxResults))
	}

	c.logger.Info("executed block", "height", abciHeader.Height, "app_hash", fmt.Sprintf("%X", finalizeBlockResponse.AppHash))

	return finalizeBlockResponse, nil
}

func (e *ProxyClient) publishEvents(resp *abci.ResponseFinalizeBlock, header *types.SignedHeader, data *types.Data, state types.State) {
	if e.eventBus == nil {
		return
	}

	abciBlock, err := abciconv.ToABCIBlock(header, data)
	if err != nil {
		return
	}

	if err := e.eventBus.PublishEventNewBlock(cmtypes.EventDataNewBlock{
		Block: abciBlock,
		BlockID: cmtypes.BlockID{
			Hash: cmbytes.HexBytes(header.Hash()),
			// for now, we don't care about part set headers
		},
		ResultFinalizeBlock: *resp,
	}); err != nil {
		e.logger.Error("failed publishing new block", "err", err)
	}

	if err := e.eventBus.PublishEventNewBlockHeader(cmtypes.EventDataNewBlockHeader{
		Header: abciBlock.Header,
	}); err != nil {
		e.logger.Error("failed publishing new block header", "err", err)
	}

	if err := e.eventBus.PublishEventNewBlockEvents(cmtypes.EventDataNewBlockEvents{
		Height: abciBlock.Height,
		Events: resp.Events,
		NumTxs: int64(len(abciBlock.Txs)),
	}); err != nil {
		e.logger.Error("failed publishing new block events", "err", err)
	}

	if len(abciBlock.Evidence.Evidence) != 0 {
		for _, ev := range abciBlock.Evidence.Evidence {
			if err := e.eventBus.PublishEventNewEvidence(cmtypes.EventDataNewEvidence{
				Evidence: ev,
				Height:   int64(header.Header.Height()), //nolint:gosec
			}); err != nil {
				e.logger.Error("failed publishing new evidence", "err", err)
			}
		}
	}

	for i, tx := range abciBlock.Data.Txs {
		err := e.eventBus.PublishEventTx(cmtypes.EventDataTx{
			TxResult: abci.TxResult{
				Height: abciBlock.Height,
				Index:  uint32(i), //nolint:gosec
				Tx:     tx,
				Result: *(resp.TxResults[i]),
			},
		})
		if err != nil {
			e.logger.Error("failed publishing event TX", "err", err)
		}
	}
}

func toRollkitTxs(txs cmtypes.Txs) types.Txs {
	rollkitTxs := make(types.Txs, len(txs))
	for i := range txs {
		rollkitTxs[i] = []byte(txs[i])
	}
	return rollkitTxs
}

func fromRollkitTxs(rollkitTxs types.Txs) cmtypes.Txs {
	txs := make(cmtypes.Txs, len(rollkitTxs))
	for i := range rollkitTxs {
		txs[i] = []byte(rollkitTxs[i])
	}
	return txs
}