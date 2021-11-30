package pandora

import (
	"github.com/ethereum/go-ethereum/log"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pkg/errors"
	"golang.org/x/crypto/sha3"
)

var (
	// Difficulty is not used anymore in pandora vanguard symbiotic relation flow
	calcDifficulty = func() *big.Int { return big.NewInt(1) }
)

func (pan *Pandora) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (p *Pandora) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	extraData := header.Extra
	extraDataLen := len(extraData)

	// Bls signature is 96 bytes long and will be inserted at the bottom of the extraData field
	if extraDataLen > signatureSize {
		//extraData = extraData[:extraDataLen-signatureSize]
		pandoraExtraData := new(ExtraDataSealed)
		pandoraExtraData.FromHeader(header)
		headerExtra := new(ExtraData)
		headerExtra.Epoch = pandoraExtraData.Epoch
		headerExtra.Turn = pandoraExtraData.Turn
		headerExtra.Slot = pandoraExtraData.Slot
		extraData, _ = rlp.EncodeToBytes(headerExtra)
	}
	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		extraData,
	})
	hasher.Sum(hash[:0])
	return hash
}

func (p *Pandora) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return p.verifyHeader(chain, header, parent)
}

func (p *Pandora) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			var parent *types.Header

			if i == 0 {
				parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
				if !isAscendingSlot(p.chain.CurrentBlock().Header(), headers[0]) {
					log.Error("slot numbers are not in ascending order", "canonical chain head blockNumber", p.chain.CurrentBlock().NumberU64(), "received header blockNumber", headers[0].Number.Uint64())
					results <- consensus.ErrInvalidSlotSequence
				}
			} else if headers[i-1].Hash() == headers[i].ParentHash {
				parent = headers[i-1]
			}

			if parent == nil {
				results <- consensus.ErrUnknownAncestor
				continue
			}

			err := p.verifyHeader(chain, header, parent)

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()

	return abort, results
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (p *Pandora) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

func (p *Pandora) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = p.CalcDifficulty(chain, header.Time, parent)
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards,
// setting the final state on the header
func (p *Pandora) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// No block rewards in PoA, so the state remains as is and uncles are dropped
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
func (p *Pandora) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// Finalize block
	p.Finalize(chain, header, state, txs, uncles)

	// Header seems complete, assemble into a block and return
	return types.NewBlock(header, txs, nil, receipts, trie.NewStackTrie(nil)), nil
}

func (p *Pandora) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return calcDifficulty()
}
