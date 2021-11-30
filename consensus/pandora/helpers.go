package pandora

import (
	"math/big"
	"math/bits"

	"github.com/status-im/keycard-go/hexutils"

	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/pkg/errors"
	bls_common "github.com/prysmaticlabs/prysm/shared/bls/common"
	"github.com/prysmaticlabs/prysm/shared/bls/herumi"
)

func Mul64(a, b uint64) (uint64, error) {
	overflows, val := bits.Mul64(a, b)
	if overflows > 0 {
		return 0, errors.New("multiplication overflows")
	}
	return val, nil
}

func (p *Pandora) StartSlot(epoch uint64) (uint64, error) {
	slot, err := Mul64(p.config.SlotsPerEpoch, epoch)
	if err != nil {
		return slot, errors.Errorf("start slot calculation overflows: %v", err)
	}
	return slot, nil
}

func (pandoraExtraDataSealed *ExtraDataSealed) FromHeader(header *types.Header) {
	err := rlp.DecodeBytes(header.Extra, pandoraExtraDataSealed)

	if nil != err {
		panic(err.Error())
	}
}

func (pandoraExtraDataSealed *ExtraDataSealed) FromExtraDataAndSignature(
	pandoraExtraData ExtraData,
	signature bls_common.Signature,
) {
	var blsSignatureBytes BlsSignatureBytes
	signatureBytes := signature.Marshal()

	if len(signatureBytes) != signatureSize {
		panic("Incompatible bls mode detected")
	}

	copy(blsSignatureBytes[:], signatureBytes[:])
	pandoraExtraDataSealed.ExtraData = pandoraExtraData
	pandoraExtraDataSealed.BlsSignatureBytes = &blsSignatureBytes
}

func (p *Pandora) verifyHeaderWorker(chain consensus.ChainHeaderReader, headers []*types.Header, index int) error {
	var parent *types.Header
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash {
		parent = headers[index-1]
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return p.verifyHeader(chain, headers[index], parent)
}

func (p *Pandora) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header) error {
	if header.Time <= parent.Time {
		return errOlderBlockTime
	}
	if !isAscendingSlot(parent, header) {
		log.Error("slot numbers are not in ascending order", "prev head blockNumber", parent.Number.Uint64(), "current header blockNumber", header.Number.Uint64())
		return consensus.ErrInvalidSlotSequence
	}
	// Verify the block's difficulty based on its timestamp and parent's difficulty
	expected := p.CalcDifficulty(chain, header.Time, parent)
	if expected.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, expected)
	}
	// Verify that the gas limit is <= 2^63-1
	cap := uint64(0x7fffffffffffffff)
	if header.GasLimit > cap {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, cap)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	// Verify that the gas limit remains within allowed bounds
	diff := int64(parent.GasLimit) - int64(header.GasLimit)
	if diff < 0 {
		diff *= -1
	}
	limit := parent.GasLimit / params.GasLimitBoundDivisor

	if uint64(diff) >= limit || header.GasLimit < params.MinGasLimit {
		return fmt.Errorf("invalid gas limit: have %d, want %d += %d", header.GasLimit, parent.GasLimit, limit)
	}
	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}

	//verify bls signature
	if err := p.VerifyBLSSignature(header); err != nil {
		return err
	}

	return nil
}

func isAscendingSlot(parentHeader, currentHeader *types.Header) bool {
	// decode the extraData byte
	getSlotNumber := func(header *types.Header) (uint64, error) {
		extraDataWithBLSSig := new(ExtraDataSealed)
		if err := rlp.DecodeBytes(header.Extra, extraDataWithBLSSig); err != nil {
			log.Error("Failed to decode extraData with signature", "err", err)
			return 0, err
		}
		return extraDataWithBLSSig.Slot, nil
	}

	parentHeaderSlotNumber, err := getSlotNumber(parentHeader)
	if err != nil {
		return false
	}

	currentHeaderSlotNumber, err := getSlotNumber(currentHeader)
	if err != nil {
		return false
	}

	log.Debug("isAscendingSlot", "parentSlotNumber", parentHeaderSlotNumber, "currentSlotNumber", currentHeaderSlotNumber)
	return parentHeaderSlotNumber < currentHeaderSlotNumber
}

func (p *Pandora) VerifyBLSSignature(header *types.Header) error {
	if p.skipBLSValidation {
		// it's running on test mode. no need to do anything. just skip
		return nil
	}
	// decode the extraData byte
	extraDataWithBLSSig := new(ExtraDataSealed)
	if err := rlp.DecodeBytes(header.Extra, extraDataWithBLSSig); err != nil {
		log.Error("Failed to decode extraData with signature", "err", err)
		return err
	}
	// extract the extraData
	extractedSlot := extraDataWithBLSSig.Slot
	extractedEpoch := extraDataWithBLSSig.Epoch
	extractedIndex := extraDataWithBLSSig.Turn

	//curEpochInfo, err := p.epochInfoCache.get(extractedEpoch)
	curEpochInfo := p.getEpochInfo(extractedEpoch)
	if curEpochInfo == nil {
		log.Error("Epoch info not found in cache", "slot", extractedSlot, "epoch", extractedEpoch)
		if extractedEpoch < p.currentEpoch {
			p.requestedEpoch = extractedEpoch
			p.subscriptionErrCh <- consensus.ErrEpochNotFound
		}
		return consensus.ErrEpochNotFound
	}

	blsSignatureBytes := extraDataWithBLSSig.BlsSignatureBytes
	log.Debug("Incoming header's extraData info", "slot", extractedSlot, "epoch",
		extractedEpoch, "turn", extractedIndex, "blsSig", common.Bytes2Hex(blsSignatureBytes[:]))

	signature, err := herumi.SignatureFromBytes(blsSignatureBytes[:])
	if err != nil {
		log.Error("Failed retrieve signature from extraData", "err", err)
		return err
	}
	validatorPubKey := curEpochInfo.ValidatorList[extractedIndex]
	sealHash := p.SealHash(header)
	log.Debug("In verifyBlsSignature", "header extra data", common.Bytes2Hex(header.Extra), "header block Number", header.Number.Uint64(), "sealHash", sealHash, "sealHash (signature msg) in bytes", sealHash[:], "validatorPublicKey", hexutils.BytesToHex(validatorPubKey.Marshal()), "extractedIndex", extractedIndex)

	if !signature.Verify(validatorPubKey, sealHash[:]) {
		log.Error("Failed to verify bls signature", "err", consensus.ErrSigFailedToVerify)
		return consensus.ErrSigFailedToVerify
	}
	return nil
}

// getEpochInfo
func (p *Pandora) getEpochInfo(epoch uint64) *EpochInfo {
	//p.epochInfosMu.RLock()
	//defer p.epochInfosMu.RUnlock()
	info, found := p.epochInfos.Get(epoch)
	if !found {
		log.Error("epoch not found in cache", "epoch", epoch)
		return nil
	}
	return info.(*EpochInfo)
}

// setEpochInfo
func (p *Pandora) setEpochInfo(epoch uint64, epochInfo *EpochInfo) {
	//p.epochInfosMu.Lock()
	//defer p.epochInfosMu.Unlock()

	log.Debug("store new epoch info into map", "epoch", epoch, "epochInfo", fmt.Sprintf("%+v", epochInfo))
	p.epochInfos.Add(epoch, epochInfo)
}
