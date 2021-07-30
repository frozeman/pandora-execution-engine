package pandora

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pkg/errors"
)

var (
	reConPeriod             = 15 * time.Second
	DefaultGenesisStartTime = uint64(time.Now().Unix())
	DefaultSlotsPerEpoch    = uint64(32)
	DefaultSlotTimeDuration = 6 * time.Second

	errInvalidValidatorSize = errors.New("invalid length of validator list")
	errInvalidEpochInfo     = errors.New("invalid epoch info")
	errEmptyOrchestratorUrl = errors.New("orchestrator url is empty")
	errNoShardingBlock      = errors.New("no pandora sharding header available yet")
	errInvalidParentHash    = errors.New("invalid parent hash")
	errInvalidBlockNumber   = errors.New("invalid block number")
	errOlderBlockTime       = errors.New("timestamp older than parent")
	errSigFailedToVerify    = errors.New("signature did not verify")
)

// DialRPCFn dials to the given endpoint
type DialRPCFn func(endpoint string) (*rpc.Client, error)

// Pandora
type Pandora struct {
	isRunning      bool
	processingLock sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	runError       error

	chain             consensus.ChainHeaderReader
	config            *params.PandoraConfig // Consensus engine configuration parameters
	epochInfoCache    *EpochInfoCache
	currentEpoch      uint64
	currentEpochInfo  *EpochInfo
	currentBlock      *types.Block
	dialRPC           DialRPCFn
	endpoint          string
	connected         bool
	rpcClient         *rpc.Client
	namespace         string
	subscription      *rpc.ClientSubscription
	subscriptionErrCh chan error

	results chan<- *types.Block
	works   map[common.Hash]*types.Block

	fetchShardingInfoCh  chan *shardingInfoReq // Channel used for remote sealer to fetch mining work
	submitShardingInfoCh chan *shardingResult
	newSealRequestCh     chan *sealTask
}

func New(
	ctx context.Context,
	cfg *params.PandoraConfig,
	urls []string,
	dialRPCFn DialRPCFn,
) *Pandora {

	ctx, cancel := context.WithCancel(ctx)
	_ = cancel // govet fix for lost cancel. Cancel is handled in service.Stop()
	if cfg.SlotsPerEpoch == 0 {
		cfg.SlotsPerEpoch = DefaultSlotsPerEpoch
	}
	if cfg.GenesisStartTime == 0 {
		cfg.GenesisStartTime = DefaultGenesisStartTime
	}
	if cfg.SlotTimeDuration == 0 {
		cfg.SlotTimeDuration = DefaultSlotTimeDuration
	}

	return &Pandora{
		ctx:            ctx,
		cancel:         cancel,
		config:         cfg,
		epochInfoCache: NewEpochInfoCache(),
		dialRPC:        dialRPCFn,
		endpoint:       urls[0],
		namespace:      "orc",

		fetchShardingInfoCh:  make(chan *shardingInfoReq),
		submitShardingInfoCh: make(chan *shardingResult),
		newSealRequestCh:     make(chan *sealTask),
		subscriptionErrCh:    make(chan error),
		works:                make(map[common.Hash]*types.Block),
	}
}

func (p *Pandora) Start(chain consensus.ChainHeaderReader) {
	// Exit early if pandora endpoint is not set.
	if p.endpoint == "" {
		log.Error("Orchestrator endpoint is empty")
		return
	}
	p.isRunning = true
	p.chain = chain
	go func() {
		p.waitForConnection()
		if p.ctx.Err() != nil {
			log.Info("Context closed, exiting pandora goroutine")
			return
		}
		p.run(p.ctx.Done())
	}()
}

func (p *Pandora) updateBlockHeader(currentBlock *types.Block, slotNumber uint64, epoch uint64) [4]string {
	currentHeader := currentBlock.Header()

	// modify the header with slot, epoch and turn
	extraData := new(ExtraData)
	extraData.Slot = slotNumber
	extraData.Epoch = epoch

	// calculate turn
	startSlot, err := p.StartSlot(epoch)
	if err != nil {
		log.Error("error while calculating start slot from epoch", "error", err, "epoch", epoch)
	}
	extraData.Turn = slotNumber - startSlot

	extraDataInBytes, err := rlp.EncodeToBytes(extraData)
	if err != nil {
		log.Error("error while encoding extra data to bytes", "error", err)
	}

	currentHeader.Extra = extraDataInBytes

	// get the updated block
	updatedBlock := currentBlock.WithSeal(currentHeader)
	// update the current block with this newly created block
	//p.currentBlock = updatedBlock

	rlpHeader, _ := rlp.EncodeToBytes(updatedBlock.Header())

	hash := p.SealHash(updatedBlock.Header())

	var retVal [4]string
	retVal[0] = hash.Hex()
	retVal[1] = updatedBlock.Header().ReceiptHash.Hex()
	retVal[2] = hexutil.Encode(rlpHeader)
	retVal[3] = hexutil.Encode(updatedBlock.Header().Number.Bytes())

	p.works[hash] = updatedBlock

	return retVal
}

// run subscribes to all the services for the ETH1.0 chain.
func (p *Pandora) run(done <-chan struct{}) {
	log.Debug("Pandora chain service is starting")
	p.runError = nil

	// ticker is needed to clean up the map
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// the loop waits for any error which comes from consensus info subscription
	// if any subscription error happens, it will try to reconnect and re-subscribe with pandora chain again.
	for {
		select {

		case sealRequest := <-p.newSealRequestCh:
			log.Debug("new seal request in pandora engine", "block number", sealRequest.block.Number())
			// first save it to result channel. so that we can send worker about the info
			p.results = sealRequest.results
			// then simply save the block into current block. We will use it again
			p.currentBlock = sealRequest.block

		case shardingInfoReq := <-p.fetchShardingInfoCh:
			// Get sharding work API is called and we got slot number from vanguard
			if p.currentBlock == nil {
				// no block is available. worker has not submit any block to seal. So something went wrong. send error
				shardingInfoReq.errc <- errNoShardingBlock
			} else {
				// current block available. now put that info into header extra data and generate seal hash
				// before that check if current block is valid and compatible with the request
				currentBlockHeader := p.currentBlock.Header()
				currentBlock := p.currentBlock.WithSeal(currentBlockHeader)
				if shardingInfoReq.blockNumber > 1 {
					// When producing block #1, validator does not know about hash of block #0
					// so do not check the parent hash and block number 1
					if currentBlockHeader.ParentHash != shardingInfoReq.parentHash {
						log.Error("Mis-match in parentHash",
							"blockNumber", currentBlockHeader.Number.Uint64(),
							"remoteParentHash", currentBlockHeader.ParentHash, "receivedParentHash", shardingInfoReq.parentHash)
						shardingInfoReq.errc <- errInvalidParentHash
						// error found. so don't do anything
						continue
					}
					if currentBlockHeader.Number.Uint64() != shardingInfoReq.blockNumber {
						log.Error("Mis-match in block number",
							"remoteBlockNumber", currentBlockHeader.Number.Uint64(), "receivedBlockNumber", shardingInfoReq.blockNumber)
						shardingInfoReq.errc <- errInvalidBlockNumber
						// error found. so don't do anything
						continue
					}
				}
				// now modify the current block header and generate seal hash
				log.Debug("for GetShardingWork updating block header extra data", "slot", shardingInfoReq.slot, "epoch", shardingInfoReq.epoch)
				shardingInfoReq.res <- p.updateBlockHeader(currentBlock, shardingInfoReq.slot, shardingInfoReq.epoch)
			}

		case submitSignatureData := <-p.submitShardingInfoCh:
			if p.submitWork(submitSignatureData.nonce, submitSignatureData.hash, submitSignatureData.blsSeal) {
				log.Debug("submitWork is successful", "nonce", submitSignatureData.nonce, "hash", submitSignatureData.hash)
				submitSignatureData.errc <- nil
			} else {
				log.Debug("submitWork is failed", "nonce", submitSignatureData.nonce, "hash", submitSignatureData.hash, "signature", submitSignatureData.blsSeal,
					"current block number", p.currentBlock.NumberU64())
				submitSignatureData.errc <- errors.New("invalid submit work request")
			}

		case <-ticker.C:
			// Clear stale pending blocks
			if p.currentBlock != nil {
				for hash, block := range p.works {
					if block.NumberU64()+staleThreshold <= p.currentBlock.NumberU64() {
						delete(p.works, hash)
					}
				}
			}

		case err := <-p.subscriptionErrCh:
			log.Debug("Got subscription error", "err", err)
			log.Debug("Starting retry to connect and subscribe to orchestrator chain")
			// TODO- We need a fall-back support to connect with other orchestrator node for verifying incoming blocks when own orchestrator is down
			// Try to check the connection and retry to establish the connection
			p.retryToConnectAndSubscribe(err)
			continue
		case <-done:
			p.isRunning = false
			p.runError = nil
			log.Debug("Context closed, exiting goroutine", "ctx", "pandora-consensus")
			return
		}
	}
}

// Close closes the exit channel to notify all backend threads exiting.
func (p *Pandora) Close() error {
	if p.cancel != nil {
		defer p.cancel()
	}
	return nil
}

func (p *Pandora) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	// In order to ensure backward compatibility, we exposes ethash RPC APIs
	// to both eth and ethash namespaces.
	return []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   &API{p},
			Public:    true,
		},
	}
}
