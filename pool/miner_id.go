// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package pool

import (
	"fmt"

	errs "github.com/decred/dcrpool/errors"
)

var (
	// These miner ids represent the expected identifications returned by
	// supported miners in their mining.subscribe requests.

	CPUID  = "cpuminer/1.0.0"
	DCR1ID = "cgminer/4.10.0"
	D9ID   = "sgminer/4.4.2"
	DR3ID  = "cgminer/4.9.0"
	D1ID   = "whatsminer/d1-v1.0"
	NHID   = "NiceHash/1.0.0"
)

// minerIDPair represents miner subscription identification pairing
// between the id and the miners that identify as.
type minerIDPair struct {
	id     string
	miners map[int]string
}

// newMinerIDPair creates a new miner ID pair.
func newMinerIDPair(id string, miners ...string) *minerIDPair {
	set := make(map[int]string, len(miners))
	for id, entry := range miners {
		set[id] = entry
	}
	sub := &minerIDPair{
		id:     id,
		miners: set,
	}
	return sub
}

// generateMinerIDs creates the miner id pairings for all supported miners.
func generateMinerIDs() map[string]*minerIDPair {
	ids := make(map[string]*minerIDPair)
	cpu := newMinerIDPair(CPUID, CPU)
	obelisk := newMinerIDPair(DCR1ID, ObeliskDCR1)
	innosilicon := newMinerIDPair(D9ID, InnosiliconD9)
	antminer := newMinerIDPair(DR3ID, AntminerDR3, AntminerDR5)
	whatsminer := newMinerIDPair(D1ID, WhatsminerD1)
	nicehash := newMinerIDPair(NHID, NiceHashValidator)

	ids[cpu.id] = cpu
	ids[obelisk.id] = obelisk
	ids[innosilicon.id] = innosilicon
	ids[antminer.id] = antminer
	ids[whatsminer.id] = whatsminer
	ids[nicehash.id] = nicehash
	return ids
}

var (
	// minerIDs represents the minder id pairings for all supported miners.
	minerIDs = generateMinerIDs()
)

// identifyMiner determines if the provided miner id is supported by the pool.
func identifyMiner(id string) (*minerIDPair, error) {
	mID, ok := minerIDs[id]
	if !ok {
		msg := fmt.Sprintf("connected miner with id %s is unsupported", id)
		return nil, errs.PoolError(errs.MinerUnknown, msg)
	}
	return mID, nil
}
