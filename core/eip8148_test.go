// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TestSetSweepThresholdRequests checks the EIP-8148 set sweep threshold queue:
// requests are rejected while the pre-fork inhibitor is active, accepted once
// the first system call cleared it, and surfaced as type 0x05 EIP-7685 requests
// with a little-endian threshold.
func TestSetSweepThresholdRequests(t *testing.T) {
	var (
		env       = newBALTestEnv(nil)
		engine    = beacon.New(ethash.NewFaker())
		pubkey    = common.FromHex("b917cfdc0d25b72d55cf94db328e1629b7f4fde2c30cdacf873b664416f76a0c7f7cc50c9f72a3cb84be88144cde9125")
		threshold = uint64(33_000_000_000) // 33 ETH, in Gwei
	)
	// Start from the post-constructor state, where the excess slot holds the
	// inhibitor until the first system call.
	queue := env.gspec.Alloc[params.SetSweepThresholdQueueAddress]
	queue.Storage = map[common.Hash]common.Hash{{}: common.MaxHash}
	env.gspec.Alloc[params.SetSweepThresholdQueueAddress] = queue
	env.gspec.GasLimit = 60_000_000

	input := binary.BigEndian.AppendUint64(bytes.Clone(pubkey), threshold)

	nonce := uint64(0)
	requestTx := func(b *BlockGen, data []byte, value int64) {
		b.AddTx(types.MustSignNewTx(env.key, env.signer, &types.DynamicFeeTx{
			ChainID:   env.cfg.ChainID,
			Nonce:     nonce,
			To:        &params.SetSweepThresholdQueueAddress,
			Gas:       1_000_000, // ~576k under EIP-8037 state gas pricing
			GasFeeCap: newGwei(5),
			GasTipCap: big.NewInt(2),
			Value:     big.NewInt(value),
			Data:      data,
		}))
		nonce++
	}

	_, blocks, receipts := GenerateChainWithGenesis(env.gspec, engine, 3, func(i int, b *BlockGen) {
		switch i {
		case 0:
			// The inhibitor is only cleared by the end-of-block system call.
			requestTx(b, input, 1)
		case 1:
			requestTx(b, input, 1)                // valid, the fee is 1 wei
			requestTx(b, input[:len(input)-1], 1) // wrong input length
			requestTx(b, input, 0)                // fee not paid
			requestTx(b, append(input, 0x00), 1)  // wrong input length
		}
	})

	wantStatus := [][]uint64{
		{types.ReceiptStatusFailed},
		{types.ReceiptStatusSuccessful, types.ReceiptStatusFailed, types.ReceiptStatusFailed, types.ReceiptStatusFailed},
		{},
	}
	for i, want := range wantStatus {
		if len(receipts[i]) != len(want) {
			t.Fatalf("block %d: got %d receipts, want %d", i+1, len(receipts[i]), len(want))
		}
		for j, r := range receipts[i] {
			if r.Status != want[j] {
				t.Errorf("block %d tx %d: got status %d, want %d", i+1, j, r.Status, want[j])
			}
		}
	}

	// source_address (20) ++ validator_pubkey (48) ++ threshold (8, little-endian)
	request := []byte{0x05}
	request = append(request, env.from.Bytes()...)
	request = append(request, pubkey...)
	request = binary.LittleEndian.AppendUint64(request, threshold)

	wantRequests := [][][]byte{nil, {request}, nil}
	for i, want := range wantRequests {
		rh := blocks[i].RequestsHash()
		if rh == nil {
			t.Fatalf("block %d has nil requests hash", i+1)
		}
		if exp := types.CalcRequestsHash(want); *rh != exp {
			t.Errorf("block %d: got requests hash %x, want %x", i+1, *rh, exp)
		}
	}

	// Insert the blocks to check validation.
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), env.gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
}

// TestSetSweepThresholdQueueMissing checks that an Amsterdam block is invalid
// when the EIP-8148 system contract is not deployed.
func TestSetSweepThresholdQueueMissing(t *testing.T) {
	env := newBALTestEnv(nil)
	delete(env.gspec.Alloc, params.SetSweepThresholdQueueAddress)

	defer func() {
		if recover() == nil {
			t.Fatal("expected block generation to fail without the EIP-8148 system contract")
		}
	}()
	GenerateChainWithGenesis(env.gspec, beacon.New(ethash.NewFaker()), 1, func(int, *BlockGen) {})
}
