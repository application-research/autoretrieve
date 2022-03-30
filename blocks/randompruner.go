package blocks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/ipfs/go-cid"
	flatfs "github.com/ipfs/go-ds-flatfs"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
)

type RandomPrunerConfig struct {
	// The amount of bytes at which the blockstore will run a prune operation -
	// cannot be zero
	Threshold uint64

	// Min bytes to remove during each prune operation - a value greater than or
	// equal to MaxCapacityBytes shall prune all applicable blocks; a value of 0
	// shall default to half of MaxCapacityBytes
	PruneBytes uint64

	// How long a block should remain pinned after it is used, before it is
	// considered for pruning; defaults to one day
	PinDuration time.Duration
}

// RandomPruner is a blockstore wrapper which removes blocks at random when disk
// space is exhausted
type RandomPruner struct {
	blockstore.Blockstore
	datastore      *flatfs.Datastore
	threshold      uint64
	pruneBytes     uint64
	pinDuration    time.Duration
	size           uint64
	lastSizeUpdate time.Time

	// A list of "hot" CIDs which should not be deleted, and when they were last
	// used
	pins map[cid.Cid]time.Time

	lk sync.Mutex
}

// The datastore that was used to create the blockstore is a required parameter
// used for calculating remaining disk space - the Blockstore interface itself
// does not provide this information
func NewRandomPruner(inner blockstore.Blockstore, datastore *flatfs.Datastore, cfg RandomPrunerConfig) *RandomPruner {
	if cfg.Threshold == 0 {
		log.Warnf("zero is not a valid prune threshold - do not initialize RandomPruner when it is not intended to be used")
	}

	if cfg.PruneBytes == 0 {
		cfg.PruneBytes = cfg.Threshold / 2
	}

	if cfg.Threshold > cfg.PruneBytes {
		cfg.PruneBytes = cfg.Threshold
	}

	if cfg.PinDuration == 0 {
		cfg.PinDuration = time.Hour * 24
	}

	return &RandomPruner{
		Blockstore:  inner,
		datastore:   datastore,
		threshold:   cfg.Threshold,
		pruneBytes:  cfg.PruneBytes,
		pinDuration: cfg.PinDuration,
	}
}

func (pruner *RandomPruner) DeleteBlock(ctx context.Context, cid cid.Cid) error {
	blockSize, err := pruner.Blockstore.GetSize(ctx, cid)
	if err != nil {
		return err
	}

	if err := pruner.Blockstore.DeleteBlock(ctx, cid); err != nil {
		return err
	}

	pruner.size -= uint64(blockSize)

	return nil
}

func (pruner *RandomPruner) Put(ctx context.Context, block Block) error {
	pruner.updatePin(block.Cid())
	pruner.Poll(ctx)
	if err := pruner.Blockstore.Put(ctx, block); err != nil {
		return err
	}

	pruner.size += uint64(len(block.RawData()))

	return nil
}

func (pruner *RandomPruner) PutMany(ctx context.Context, blocks []Block) error {
	blocksSize := 0
	for _, block := range blocks {
		pruner.updatePin(block.Cid())
		blocksSize += len(block.RawData())
	}
	pruner.Poll(ctx)
	if err := pruner.Blockstore.PutMany(ctx, blocks); err != nil {
		return err
	}

	pruner.size += uint64(blocksSize)

	return nil
}

// Checks remaining blockstore capacity and prunes if maximum capacity is hit
func (pruner *RandomPruner) Poll(ctx context.Context) {
	pruner.cleanPins(ctx)

	if time.Since(pruner.lastSizeUpdate) > time.Minute {
		size, err := pruner.datastore.DiskUsage(ctx)
		if err != nil {
			log.Errorf("Pruner could not get blockstore disk usage: %v", err)
		}

		var diff uint64
		if size > pruner.size {
			diff = size - pruner.size
		} else {
			diff = pruner.size - size
		}
		if diff > 1<<30 {
			log.Warnf("Large mismatch between pruner's tracked size (%v) and datastore's reported disk usage (%v)", pruner.size, size)
		}

		pruner.size = size
	}

	if pruner.size >= pruner.threshold {
		go func() {
			log.Infof("Starting prune operation with original datastore size of %s", humanize.IBytes(pruner.size))
			start := time.Now()

			if err := pruner.prune(ctx, pruner.threshold*pruner.pruneBytes); err != nil {
				log.Errorf("Random pruner errored during prune: %v", err)
			}

			duration := time.Since(start)
			log.Infof("Prune operation finished after %s with new datastore size of %s", duration, humanize.IBytes(pruner.size))
		}()

	}
}

// This is a potentially long-running operation, start it as a goroutine!
//
// TODO: definitely want to add a span here
func (pruner *RandomPruner) prune(ctx context.Context, bytesToPrune uint64) error {
	if !pruner.lk.TryLock() {
		return fmt.Errorf("already pruning - this could happen if the blockstore is growing faster than prune processes can run")
	}
	defer pruner.lk.Unlock()

	// Get all the keys in the blockstore
	allCids, err := pruner.AllKeysChan(ctx)
	if err != nil {
		return err
	}

	tmpFile, err := os.Create(path.Join(os.TempDir(), "autoretrieve-prune.txt"))
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	// Write every non-pinned block on a separate line to the temporary file
	writer := bufio.NewWriter(tmpFile)
	cidCount := 0
	for cid := range allCids {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Don't consider pinned blocks for deletion
		if pruner.isPinned(cid) {
			continue
		}

		if _, err := writer.WriteString(cid.String() + "\n"); err != nil {
			return err
		}
		cidCount++
	}

	// Choose random blocks and remove them until the requested amount of bytes
	// have been pruned
	reader := bufio.NewReader(tmpFile)
	bytesPruned := uint64(0)
	// notFoundCount is used to avoid infinitely spinning through here if no
	// blocks appear to be left to remove - concretely detecting that all blocks
	// have been covered actually seems to be a difficult problem
	notFoundCount := 0
	for bytesPruned < bytesToPrune {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Read up to the chosen line and parse the CID
		cidIndex := rand.Int() % cidCount
		var cidStr string
		for i := 0; i < cidIndex; i++ {
			cidStr, err = reader.ReadString('\n')
			if err != nil {
				return err
			}
		}
		cid, err := cid.Parse(cidStr)
		if err != nil {
			return err
		}

		blockSize, err := pruner.GetSize(ctx, cid)
		if err != nil {
			// If the block was already removed, ignore it up to a few times in
			// a row; this strategy might not work super effectively when
			// pruneBytes is close to the threshold, and could possibly be
			// remedied by deleting CIDs from the file after reading them
			if errors.Is(err, blockstore.ErrNotFound) {
				notFoundCount++
				if notFoundCount > 10 {
					break
				}
				continue
			}
			return err
		}

		if err := pruner.DeleteBlock(ctx, cid); err != nil {
			return err
		}

		bytesPruned += uint64(blockSize)
		notFoundCount = 0

		// Return reader to start
		reader.Reset(tmpFile)
	}

	return nil
}

func (pruner *RandomPruner) updatePin(cid cid.Cid) {
	pruner.pins[cid] = time.Now()
}

// Remove pins that have passed the pin duration
func (pruner *RandomPruner) cleanPins(ctx context.Context) error {
	now := time.Now()

	for cid, lastUse := range pruner.pins {
		if err := ctx.Err(); err != nil {
			return err
		}

		if now.Sub(lastUse) > pruner.pinDuration {
			delete(pruner.pins, cid)
		}
	}

	return nil
}

func (pruner *RandomPruner) isPinned(cid cid.Cid) bool {
	lastUse, ok := pruner.pins[cid]
	if !ok {
		return false
	}

	return time.Since(lastUse) < pruner.pinDuration
}
