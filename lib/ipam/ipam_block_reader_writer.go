// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"net"

	log "github.com/sirupsen/logrus"

	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
)

type blockReaderWriter struct {
	client bapi.Client
	pools  PoolAccessorInterface
}

func (rw blockReaderWriter) getAffineBlocks(ctx context.Context, host string, ver ipVersion, pools []cnet.IPNet) ([]cnet.IPNet, error) {
	// Lookup all blocks by providing an empty BlockListOptions
	// to the List operation.
	opts := model.BlockAffinityListOptions{Host: host, IPVersion: ver.Number}
	datastoreObjs, err := rw.client.List(ctx, opts, "")
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
			// The block path does not exist yet.  This is OK - it means
			// there are no affine blocks.
			return []cnet.IPNet{}, nil

		} else {
			log.Errorf("Error getting affine blocks: %v", err)
			return nil, err
		}
	}

	// Iterate through and extract the block CIDRs.
	ids := []cnet.IPNet{}
	for _, o := range datastoreObjs.KVPairs {
		k := o.Key.(model.BlockAffinityKey)

		// Add the block if no IP pools were specified, or if IP pools were specified
		// and the block falls within the given IP pools.
		if len(pools) == 0 {
			ids = append(ids, k.CIDR)
		} else {
			for _, pool := range pools {
				if pool.Contains(k.CIDR.IPNet.IP) {
					ids = append(ids, k.CIDR)
					break
				}
			}
		}
	}
	return ids, nil
}

// claimNewAffineBlock claims a new pending affinity for a currently unclaimed allocation block. The calling code should use the pending affinity
// to attempt to claim the actual allocation block.
func (rw blockReaderWriter) claimNewAffineBlock(ctx context.Context, host string, version ipVersion, requestedPools []cnet.IPNet, config IPAMConfig) (*model.KVPair, error) {
	// If requestedPools is not empty, use it.  Otherwise, default to all configured pools.
	pools := []cnet.IPNet{}

	// Get all the configured pools.
	enabledPools, err := rw.pools.GetEnabledPools(version.Number)
	if err != nil {
		log.Errorf("Error reading configured pools: %v", err)
		return nil, err
	}
	log.Debugf("enabled IPPools: %v", enabledPools)

	if len(requestedPools) > 0 {
		log.Debugf("requested IPPools: %v", requestedPools)
		for _, p := range enabledPools {
			if isPoolInRequestedPools(p, requestedPools) {
				pools = append(pools, p)
			}
		}
	} else {
		pools = enabledPools
	}

	// Build a map so we can lookup existing pools.
	pm := map[string]bool{}
	for _, p := range enabledPools {
		pm[p.String()] = true
	}

	// Make sure each requested pool exists.
	for _, rp := range requestedPools {
		if _, ok := pm[rp.String()]; !ok {
			// The requested pool doesn't exist.
			return nil, fmt.Errorf("the given pool (%s) does not exist, or is not enabled", rp.IPNet.String())
		}
	}

	// If there are no pools, we cannot assign addresses.
	if len(pools) == 0 {
		return nil, errors.New("no configured Calico pools")
	}

	// Iterate through pools to find a new block.
	log.Infof("Claiming a new affine block for host '%s'", host)
	for _, pool := range pools {
		// Use a block generator to iterate through all of the blocks
		// that fall within the pool.
		log.Debugf("Looking for blocks in pool %+v", pool)
		blocks := randomBlockGenerator(pool, host)
		for subnet := blocks(); subnet != nil; subnet = blocks() {
			// Check if a block already exists for this subnet.
			log.Debugf("Getting block: %s", subnet.String())
			key := model.BlockKey{CIDR: *subnet}
			_, err := rw.client.Get(ctx, key, "")
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
					// The block does not yet exist in etcd.  Try to grab it.
					log.Infof("Found free block: %+v", *subnet)
					pa, err := rw.getPendingAffinity(ctx, host, *subnet)
					if err != nil {
						return nil, err
					}
					return pa, nil
				}
				log.Errorf("Error getting block: %v", err)
				return nil, err
			}
			log.Debugf("Block %s already exists", subnet.String())
		}
	}
	return nil, noFreeBlocksError("No Free Blocks")
}

// isPoolInRequestedPools checks if the IP Pool that is passed in belongs to the list of IP Pools
// that should be used for assigning IPs from.
func isPoolInRequestedPools(pool cnet.IPNet, requestedPools []cnet.IPNet) bool {
	if len(requestedPools) == 0 {
		return true
	}
	// Compare the requested pools against the actual pool CIDR.  Note that we don't use deep equals
	// because golang interchangeably seems to use 4-byte and 16-byte representations of IPv4 addresses.
	for _, cidr := range requestedPools {
		if pool.String() == cidr.String() {
			return true
		}
	}
	return false
}

// getPendingAffinity claims a pending affinity for the given host and subnet. The affinity can then
// be used to claim a block. If an affinity already exists, it will attempt to mark the existing affinity
// as pending.
func (rw blockReaderWriter) getPendingAffinity(ctx context.Context, host string, subnet cnet.IPNet) (*model.KVPair, error) {
	log.Infof("Host %s claiming pending affinity for %s", host, subnet)
	obj := model.KVPair{
		Key:   model.BlockAffinityKey{Host: host, CIDR: subnet},
		Value: model.BlockAffinity{State: model.StatePending},
	}
	aff, err := rw.client.Create(ctx, &obj)
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); !ok {
			log.WithError(err).Error("Failed to claim affinity")
			return nil, err
		}
		log.Info("Block affinity already exists")

		// Get the existing affinity and write it as pending.
		aff, err = rw.client.Get(ctx, obj.Key, "")
		if err != nil {
			log.WithError(err).Error("Failed to get existing affinity")
			return nil, err
		}
		aff.Value.(*model.BlockAffinity).State = model.StatePending
		return rw.client.Update(ctx, aff)
	}
	return aff, nil
}

// claimBlockAffinity claims the provided block using the given pending affinity. If successful, it will confirm the affinity. If another host
// steals the block, claimBlockAffinity will attempt to delete the provided pending affinity.
func (rw blockReaderWriter) claimBlockAffinity(ctx context.Context, aff *model.KVPair, config IPAMConfig) (*model.KVPair, error) {
	// Pull out relevant fields.
	subnet := aff.Key.(model.BlockAffinityKey).CIDR
	host := aff.Key.(model.BlockAffinityKey).Host

	// Create the new block.
	affinityKeyStr := "host:" + host
	block := newBlock(subnet)
	block.Affinity = &affinityKeyStr
	block.StrictAffinity = config.StrictAffinity

	// Create the new block in the datastore.
	o := model.KVPair{
		Key:   model.BlockKey{block.CIDR},
		Value: block.AllocationBlock,
	}
	kvp, err := rw.client.Create(ctx, &o)
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); ok {
			// Block already exists, check affinity.
			obj, err := rw.client.Get(ctx, model.BlockKey{subnet}, "")
			if err != nil {
				// We failed to create the block, but the affinity still exists. We don't know
				// if someone else beat us to the block since we can't get it.
				log.WithError(err).Errorf("Error reading block")
				return nil, err
			}

			// Pull out the allocationBlock object.
			b := allocationBlock{obj.Value.(*model.AllocationBlock)}

			if b.Affinity != nil && *b.Affinity == affinityKeyStr {
				// Block has affinity to this host, meaning another
				// process on this host claimed it. Confirm the affinity
				// and return the existing block.
				log.Debugf("Block %s already claimed by us.", subnet)
				if _, err := rw.confirmAffinity(ctx, aff); err != nil {
					// Failed to confirm affinity.
					return nil, err
				}
				return obj, nil
			}

			// Some other host beat us to this block.  Cleanup and return an error.
			_, err = rw.client.Delete(ctx, model.BlockAffinityKey{Host: host, CIDR: b.CIDR}, aff.Revision)
			if err != nil {
				// Failed to clean up our claim to this block.
				log.WithError(err).Errorf("Error cleaning up block affinity after failed claim")
			}
			return nil, errBlockClaimConflict{Block: b}
		}
		log.WithError(err).Warningf("Problem creating block while claiming block")
		return nil, err
	}

	// We've successfully claimed the block - confirm the affinity.
	if _, err = rw.confirmAffinity(ctx, aff); err != nil {
		return nil, err
	}
	return kvp, nil
}

func (rw blockReaderWriter) confirmAffinity(ctx context.Context, aff *model.KVPair) (*model.KVPair, error) {
	host := aff.Key.(model.BlockAffinityKey).Host
	cidr := aff.Key.(model.BlockAffinityKey).CIDR
	log.Debugf("Confirming affinity: %s -> %s", host, cidr)
	aff.Value.(*model.BlockAffinity).State = model.StateConfirmed
	confirmed, err := rw.client.Update(ctx, aff)
	if err != nil {
		log.WithError(err).Error("Failed to confirm block affinity")
		return nil, err
	}
	return confirmed, nil
}

// releaseBlockAffinity releases the host's affinity to the given block, and returns an affinityClaimedError if
// the host does not claim an affinity for the block.
func (rw blockReaderWriter) releaseBlockAffinity(ctx context.Context, host string, blockCIDR cnet.IPNet) error {
	// Make sure hostname is not empty.
	if host == "" {
		log.Errorf("Hostname can't be empty")
		return errors.New("Hostname must be sepcified to release block affinity")
	}

	for i := 0; i < ipamEtcdRetries; i++ {
		// Read the model.KVPair containing the block affinity.
		log.WithFields(log.Fields{"block": blockCIDR.String(), "retry": i}).Debugf("Attempt to release affinity for block")
		aff, err := rw.client.Get(ctx, model.BlockAffinityKey{Host: host, CIDR: blockCIDR}, "")
		if err != nil {
			log.WithError(err).Errorf("Error getting block affinity %s", blockCIDR.String())
			return err
		}

		// Mark it as pending deletion.
		aff.Value.(*model.BlockAffinity).State = model.StatePendingDeletion
		aff, err = rw.client.Update(ctx, aff)
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
				// CASError - continue.
				continue
			}

			log.WithError(err).Errorf("Failed to mark block affinity %s as pending deletion", blockCIDR.String())
			return err
		}

		// Read the model.KVPair containing the block
		// and pull out the allocationBlock object.  We need to hold on to this
		// so that we can pass it back to the datastore on Update.
		obj, err := rw.client.Get(ctx, model.BlockKey{CIDR: blockCIDR}, "")
		if err != nil {
			log.Errorf("Error getting block %s: %v", blockCIDR.String(), err)
			return err
		}
		b := allocationBlock{obj.Value.(*model.AllocationBlock)}

		// Check that the block affinity matches the given affinity.
		if b.Affinity != nil && !hostAffinityMatches(host, b.AllocationBlock) {
			log.Errorf("Mismatched affinity: %s != %s", *b.Affinity, "host:"+host)
			return errBlockClaimConflict{Block: b}
		}

		if b.empty() {
			// If the block is empty, we can delete it.
			log.Debug("Block is empty - delete it")
			_, err := rw.client.Delete(ctx, model.BlockKey{CIDR: b.CIDR}, obj.Revision)
			if err != nil {
				// Return the error unless the block didn't exist.
				if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
					log.Errorf("Error deleting block: %v", err)
					return err
				}
			}
		} else {
			// Otherwise, we need to remove affinity from it.
			// This prevents the host from automatically assigning
			// from this block unless we're allowed to overflow into
			// non-affine blocks.
			log.Debug("Block is not empty - remove the affinity")
			b.Affinity = nil

			// Pass back the original KVPair with the new
			// block information so we can do a CAS.
			obj.Value = b.AllocationBlock
			_, err = rw.client.Update(ctx, obj)
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					// CASError - continue.
					continue
				} else {
					return err
				}
			}
		}

		// We've removed / updated the block, so perform a compare-and-delete on the BlockAffinity.
		_, err = rw.client.Delete(ctx, model.BlockAffinityKey{Host: host, CIDR: b.CIDR}, aff.Revision)
		if err != nil {
			// Return the error unless the affinity didn't exist.
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
				log.Errorf("Error deleting block affinity: %v", err)
				return err
			}
		}
		return nil

	}
	return errors.New("Max retries hit")
}

// withinConfiguredPools returns true if the given IP is within a configured
// Calico pool, and false otherwise.
func (rw blockReaderWriter) withinConfiguredPools(ip cnet.IP) bool {
	enabledPools, _ := rw.pools.GetEnabledPools(ip.Version())
	for _, p := range enabledPools {
		// Compare any enabled pools.
		if p.Contains(ip.IP) {
			return true
		}
	}
	return false
}

// Generator to get list of block CIDRs which
// fall within the given pool. Returns nil when no more
// blocks can be generated.
func blockGenerator(pool cnet.IPNet) func() *cnet.IPNet {
	// Determine the IP type to use.
	version := getIPVersion(cnet.IP{pool.IP})
	ip := cnet.IP{pool.IP}
	return func() *cnet.IPNet {
		returnIP := ip
		if pool.Contains(ip.IP) {
			ipnet := net.IPNet{returnIP.IP, version.BlockPrefixMask}
			cidr := cnet.IPNet{ipnet}
			ip = incrementIP(ip, big.NewInt(blockSize))
			return &cidr
		} else {
			return nil
		}
	}
}

// Returns a generator that, when called, returns a random
// block from the given pool.  When there are no blocks left,
// the it returns nil.
func randomBlockGenerator(pool cnet.IPNet, hostName string) func() *cnet.IPNet {

	// Determine the IP type to use.
	version := getIPVersion(cnet.IP{pool.IP})
	baseIP := cnet.IP{pool.IP}

	// Determine the number of blocks within this pool.
	ones, size := pool.Mask.Size()
	prefixLen := size - ones
	numIP := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(prefixLen)), nil)
	numBlocks := new(big.Int)
	numBlocks.Div(numIP, big.NewInt(blockSize))

	// Create a random number generator seed based on the hostname.
	// This is to avoid assigning multiple blocks when multiple
	// workloads request IPs around the same time.
	hostHash := fnv.New32()
	hostHash.Write([]byte(hostName))
	source := rand.NewSource(int64(hostHash.Sum32()))
	randm := rand.New(source)

	// initialIndex keeps track of the random starting point
	initialIndex := new(big.Int)
	initialIndex.Rand(randm, numBlocks)

	// i keeps track of current index while walking the blocks in a pool
	i := initialIndex

	// numReturned keeps track of number of blocks returned
	numReturned := big.NewInt(0)

	// numDiff = numBlocks - i
	numDiff := new(big.Int)

	return func() *cnet.IPNet {
		// The `big.NewInt(0)` part creates a temp variable and assigns the result of multiplication of `i` and `big.NewInt(blockSize)`
		// Note: we are not using `i.Mul()` because that will assign the result of the multiplication to `i`, which will cause unexpected issues
		ip := incrementIP(baseIP, big.NewInt(0).Mul(i, big.NewInt(blockSize)))
		ipnet := net.IPNet{ip.IP, version.BlockPrefixMask}

		numDiff.Sub(numBlocks, i)

		if numDiff.Cmp(big.NewInt(1)) <= 0 {
			// Index has reached end of the blocks;
			// Loop back to beginning of pool rather than
			// increment, because incrementing would put us outside of the pool.
			i = big.NewInt(0)
		} else {
			// Increment to the next block
			i.Add(i, big.NewInt(1))
		}

		if numReturned.Cmp(numBlocks) >= 0 {
			// Index finished one full circle across the blocks
			// Used all of the blocks in this pool.
			return nil
		}
		numReturned.Add(numReturned, big.NewInt(1))

		// Return the block from this pool that corresponds with the index.
		return &cnet.IPNet{ipnet}
	}
}
