package pandora

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	bls_common "github.com/prysmaticlabs/prysm/shared/bls/common"
	"github.com/prysmaticlabs/prysm/shared/bls/herumi"

	"time"
)

// waitForConnection waits for a connection with pandora chain. Until a successful connection and subscription with
// pandora chain, it retries again and again.
func (p *Pandora) waitForConnection() {
	log.Debug("Waiting for the connection with orchestrator client")
	var err error
	if err = p.connectToOrchestrator(); err == nil {
		log.Info("Connected and subscribed to orchestrator client", "endpoint", p.endpoint)
		p.connected = true
		return
	}
	log.Warn("Could not connect or subscribe to orchestrator client", "err", err)
	p.runError = err
	ticker := time.NewTicker(reConPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Debug("Dialing orchestrator node", "endpoint", p.endpoint)
			var errConnect error
			if errConnect = p.connectToOrchestrator(); errConnect != nil {
				log.Warn("Could not connect or subscribe to orchestrator client", "err", errConnect)
				p.runError = errConnect
				continue
			}
			p.connected = true
			p.runError = nil
			log.Info("Connected and subscribed to orchestrator client", "endpoint", p.endpoint)
			return
		case <-p.ctx.Done():
			log.Debug("Received cancelled context,closing existing waiting connection loop")
			return
		}
	}
}

// connectToChain dials to pandora chain and creates rpcClient and subscribe
func (p *Pandora) connectToOrchestrator() error {
	if p.rpcClient == nil {
		panRPCClient, err := p.dialRPC(p.endpoint)
		if err != nil {
			return err
		}
		p.rpcClient = panRPCClient
	}
	cs, err := p.subscribe()
	if err != nil {
		return err
	}
	p.subscription = cs
	return nil
}

func (p *Pandora) subscribe() (*rpc.ClientSubscription, error) {
	var curCanonicalEpoch uint64
	if p.chain != nil {
		curBlock := p.chain.CurrentBlock()
		curHeader := curBlock.Header()

		if curHeader.Number.Uint64() > 0 {
			extraDataWithSig := new(ExtraDataSealed)
			err := rlp.DecodeBytes(curHeader.Extra, extraDataWithSig)
			if err != nil {
				log.Error("Failed to decode extraData of the canonical head", "err", err)
				return nil, err
			}

			log.Debug("Retrieved current header from local chain",
				"blkNumber", curBlock.Number(), "epoch", extraDataWithSig.Epoch, "slot", extraDataWithSig.Slot)

			if extraDataWithSig.Epoch > 0 {
				// subscribing from previous epoch for safety reason
				curCanonicalEpoch = extraDataWithSig.Epoch - 1
				p.currentEpoch = extraDataWithSig.Epoch - 1
			} else {
				curCanonicalEpoch = 0
				p.currentEpoch = 0
			}
		}
	} else {
		log.Debug("Chain is nil. subscription starts from epoch 0")
		// when there is no blockchain in local, subscription starts from 0
		curCanonicalEpoch = 0
		p.currentEpoch = 0
	}
	// connect to pandora subscription
	sub, err := p.SubscribeEpochInfo(p.ctx, curCanonicalEpoch, p.namespace, p.rpcClient)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// retryToConnectAndSubscribe retries to pandora chain in case of any failure.
func (p *Pandora) retryToConnectAndSubscribe(err error) {
	p.runError = err
	p.connected = false
	// Back off for a while before resuming dialing the pandora node.
	time.Sleep(reConPeriod)
	p.waitForConnection()
	// Reset run error in the event of a successful connection.
	p.runError = nil
}

// subscribePendingHeaders subscribes to pandora client from latest saved slot using given rpc client
func (p *Pandora) SubscribeEpochInfo(
	ctx context.Context,
	fromEpoch uint64,
	namespace string,
	client *rpc.Client,
) (*rpc.ClientSubscription, error) {

	ch := make(chan *EpochInfoPayload)
	sub, err := client.Subscribe(ctx, namespace, ch, "minimalConsensusInfo", fromEpoch)
	if nil != err {
		log.Error("Failed to subscribe orchestrator minimalConsensusInfo stream api", "err", err)
		return nil, err
	}
	log.Debug("subscribed to orchestrator for new epoch info", "fromEpoch", fromEpoch)

	// Start up a dispatcher to feed into the callback
	go func() {
		for {
			select {
			case epochInfo := <-ch:
				log.Debug("Received new epoch info", "epochInfo", fmt.Sprintf("%+v", epochInfo))
				// dispatch newPendingHeader to handler
				if err = p.processEpochInfo(epochInfo); err != nil {
					log.Error("Failed to process epoch info", "err", err, "ctx", "pandora-consensus")
					p.subscriptionErrCh <- err
					return
				}
			case err := <-sub.Err():
				if err != nil {
					log.Debug("Got subscription error", "err", err, "ctx", "pandora-consensus")
					p.subscriptionErrCh <- err
					return
				}
			case <-p.ctx.Done():
				log.Debug("Received cancelled context, closing existing epoch info subscription", "ctx", "pandora-consensus")
				return
			}
		}
	}()

	return sub, nil
}

// processEpochInfo
func (p *Pandora) processEpochInfo(info *EpochInfoPayload) error {
	// checking proper length of the validator list.
	if uint64(len(info.ValidatorList)) != p.config.SlotsPerEpoch {
		log.Debug("Mis-match in validator list length", "slotsPerEpoch", p.config.SlotsPerEpoch, "len", len(info.ValidatorList))
		return errInvalidValidatorSize
	}

	epochInfo := new(EpochInfo)
	epochInfo.Epoch = info.Epoch
	epochInfo.SlotTimeDuration = info.SlotTimeDuration
	epochInfo.EpochTimeStart = info.EpochTimeStart
	epochInfo.ValidatorList = [32]bls_common.PublicKey{}

	// convert public key string to publicKey
	for i, pubKeyStr := range info.ValidatorList {
		pubKeyBytes, err := hexutil.Decode(pubKeyStr)
		if err != nil {
			log.Error("Failed to decode validator public key bytes from string", "err", err)
			return err
		}

		if epochInfo.Epoch == 0 && i == 0 {
			continue
		}

		pubKey, err := herumi.PublicKeyFromBytes(pubKeyBytes)
		if err != nil {
			log.Error("Failed to retrieve validator public key from bytes", "err", err)
			return err
		}
		epochInfo.ValidatorList[i] = pubKey
	}

	// store epoch info in in-memeory cache
	//if err := p.epochInfoCache.put(info.Epoch, epochInfo); err != nil {
	//	return err
	//}
	p.setEpochInfo(epochInfo.Epoch, epochInfo)

	return nil
}
