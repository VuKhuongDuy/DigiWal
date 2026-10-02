package sweep

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"digiwal/services/sweeper/internal/chain"
	"digiwal/services/sweeper/internal/ledger"
	"digiwal/services/sweeper/internal/wallet"
)

// A Gas Top-Up is a Hot Wallet tx like a withdrawal's: if it is stuck underpriced it holds a Hot Wallet
// nonce, and every withdrawal, Gas Top-Up and Treasury Sweep queues behind it. So it is Fee Bumped the
// same way withdrawal-service does it (withdrawal-flow.md invariant #21; deposit-flow.md invariant #18,
// task 2.8): re-signed by wallet-service at the same nonce with higher fees, never abandoned.

// hotVersion is one signed form of the top-up: the original or a Fee Bump (or an operator's filler).
type hotVersion struct {
	hash   common.Hash
	raw    []byte
	cancel bool
}

// hashOf is the tx hash of a signed typed transaction: keccak256 of its binary encoding.
func hashOf(raw []byte) common.Hash { return crypto.Keccak256Hash(raw) }

func (s *Sweeper) bumpAfter() int64 {
	if s.Config.FeeBumpAfterBlocks > 0 {
		return s.Config.FeeBumpAfterBlocks
	}
	return 20
}

func (s *Sweeper) maxBumps() int {
	if s.Config.MaxFeeBumps > 0 {
		return s.Config.MaxFeeBumps
	}
	return 5
}

// underpriced is a node's "your max fee is too low", in the wordings geth and Anvil use.
func underpriced(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "underpriced") || strings.Contains(m, "fee too low") || strings.Contains(m, "less than block base fee")
}

func (s *Sweeper) alert(msg string) {
	if s.Alert != nil {
		s.Alert(msg)
		return
	}
	log.Print("ALERT: " + msg)
}

// sendHotTx broadcasts the Gas Top-Up signed under key and waits until a version of it is mined, Fee
// Bumping it while it is stuck: when the node refuses it as underpriced, or when it has not been mined
// FeeBumpAfterBlocks blocks after it was last sent and is underpriced against the current base fee; at
// most MaxFeeBumps, then an ALERT (the 50 gwei ceiling is wallet-service's, answered as a refusal).
//
// It first asks wallet-service for the key's existing versions, so a retry — after a crash or a timeout —
// carries on from the latest replacement and watches every hash instead of re-sending the original: any
// version may be the one that is mined, never more than one. The mined version's hash and receipt are
// returned, and are what its gas is reported for.
func (s *Sweeper) sendHotTx(ctx context.Context, key string, raw []byte) (common.Hash, *chain.Receipt, error) {
	const what = "gas top-up"
	hk, err := s.Wallet.LookupHotKey(ctx, key)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("looking up %s key: %w", what, err)
	}
	versions := []hotVersion{{hash: hashOf(raw), raw: raw}}
	bumps := 0
	if hk != nil {
		versions[0] = hotVersion{hash: hashOf(hk.Raw), raw: hk.Raw}
		for _, r := range hk.Replacements {
			versions = append(versions, hotVersion{hash: hashOf(r.Raw), raw: r.Raw, cancel: r.Kind == "cancel"})
			if r.Kind != "cancel" {
				bumps++
			}
		}
	}
	if versions[len(versions)-1].cancel {
		s.alert(fmt.Sprintf("%s %s was cancelled by an operator's Nonce Filler: it will never land", what, key))
	}

	head, err := s.Chain.Head(ctx)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("reading the chain head: %w", err)
	}
	sentHead := head
	bumpingOff := false // wallet-service refused: stop asking, keep waiting
	bump := func() error {
		if bumpingOff {
			return nil
		}
		if bumps >= s.maxBumps() {
			s.alert(fmt.Sprintf("%s %s is still not mined after %d Fee Bumps (Hot Wallet nonce held); not bumping again", what, key, bumps))
			bumpingOff = true
			return nil
		}
		rawN, hashN, err := s.Wallet.ReplaceHotWallet(ctx, key, bumps+1)
		switch {
		case errors.Is(err, wallet.ErrPermanent): // the fee ceiling, a Nonce Filler, an unknown key
			s.alert(fmt.Sprintf("wallet-service refused Fee Bump %d of %s %s: %v", bumps+1, what, key, err))
			bumpingOff = true
			return nil
		case err != nil:
			log.Printf("Fee Bump %d of %s %s: %v (will try again)", bumps+1, what, key, err)
			return nil
		}
		bumps++
		versions = append(versions, hotVersion{hash: common.HexToHash(hashN), raw: rawN})
		sentHead = head
		if _, err := s.Chain.Broadcast(ctx, rawN); err != nil {
			log.Printf("sending Fee Bump %d of %s %s: %v", bumps, what, key, err)
		}
		return nil
	}

	if _, err := s.Chain.Broadcast(ctx, versions[len(versions)-1].raw); err != nil {
		if !underpriced(err) {
			return versions[len(versions)-1].hash, nil, fmt.Errorf("broadcasting %s: %w", what, err)
		}
		if err := bump(); err != nil {
			return common.Hash{}, nil, err
		}
	}

	deadline := time.Now().Add(s.Config.InclusionTimeout)
	for {
		for _, v := range versions {
			r, err := s.Chain.Receipt(ctx, v.hash)
			if err != nil {
				return v.hash, nil, fmt.Errorf("reading %s receipt: %w", what, err)
			}
			if r == nil {
				continue
			}
			switch {
			case v.cancel:
				return v.hash, r, fmt.Errorf("%s %s was cancelled by an operator's Nonce Filler (mined in block %d)", what, key, r.Block)
			case !r.Success:
				return v.hash, r, fmt.Errorf("%s %s reverted in block %d", what, v.hash, r.Block)
			}
			return v.hash, r, nil
		}
		if time.Now().After(deadline) {
			return versions[len(versions)-1].hash, nil, fmt.Errorf("%s %s not mined within %s", what, versions[len(versions)-1].hash, s.Config.InclusionTimeout)
		}
		if head, err = s.Chain.Head(ctx); err != nil {
			return common.Hash{}, nil, fmt.Errorf("reading the chain head: %w", err)
		}
		if latest := versions[len(versions)-1]; !latest.cancel && head-sentHead >= s.bumpAfter() {
			if under, err := s.Chain.Underpriced(ctx, latest.raw); err == nil && under {
				if err := bump(); err != nil {
					return common.Hash{}, nil, err
				}
			}
		}
		if err := sleep(ctx, s.Config.PollInterval); err != nil {
			return common.Hash{}, nil, err
		}
	}
}

// postGas reports the gas a mined tx of ours spent (ledger.md invariant #12; purpose gas_topup or sweep).
// Best effort: a few attempts, then an ALERT. It never fails the Sweep — a redelivery would find nothing
// left to move and never retry it — which is the accepted limitation of a stateless sweeper (ledger.md
// Open Questions #6, Decided). ledger-service dedupes on the tx hash, so a retry cannot double-post.
func (s *Sweeper) postGas(ctx context.Context, hash common.Hash, rc *chain.Receipt, purpose string) {
	if s.Ledger == nil || rc == nil {
		return
	}
	price := rc.EffectiveGasPrice
	if price == nil {
		price = new(big.Int)
	}
	g := ledger.GasFee{ChainID: s.Config.ChainID, TxHash: strings.ToLower(hash.Hex()), GasUsed: rc.GasUsed, GasPrice: price, Purpose: purpose}
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = s.Ledger.PostGasFee(ctx, g); err == nil {
			return
		}
		if errors.Is(err, ledger.ErrRefused) {
			s.alert(fmt.Sprintf("ledger-service refused the %s Gas Fee of tx %s: %v", purpose, g.TxHash, err))
			return
		}
		if sleep(ctx, time.Duration(attempt)*s.Config.PollInterval) != nil {
			return
		}
	}
	s.alert(fmt.Sprintf("could not record the %s Gas Fee of tx %s after 3 attempts (not retried later: sweeper keeps no state): %v", purpose, g.TxHash, err))
}
