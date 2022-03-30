package blocks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"strings"
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
	pins   map[cid.Cid]time.Time
	pinsLk sync.Mutex

	pruneLk sync.Mutex
}

// The datastore that was used to create the blockstore is a required parameter
// used for calculating remaining disk space - the Blockstore interface itself
// does not provide this information
func NewRandomPruner(
	ctx context.Context,
	inner blockstore.Blockstore,
	datastore *flatfs.Datastore,
	cfg RandomPrunerConfig,
) (*RandomPruner, error) {
	if cfg.Threshold == 0 {
		log.Warnf("Zero is not a valid prune threshold - do not initialize RandomPruner when it is not intended to be used")
	}

	if cfg.PruneBytes == 0 {
		cfg.PruneBytes = cfg.Threshold / 2
	}

	if cfg.Threshold > cfg.PruneBytes {
		cfg.PruneBytes = cfg.Threshold
	}

	size, err := datastore.DiskUsage(ctx)
	if err != nil {
		return nil, err
	}

	log.Infof("Initialized pruner's tracked size as %s", humanize.IBytes(size))

	return &RandomPruner{
		Blockstore:  inner,
		datastore:   datastore,
		threshold:   cfg.Threshold,
		pruneBytes:  cfg.PruneBytes,
		pinDuration: cfg.PinDuration,
		size:        size,

		pins: make(map[cid.Cid]time.Time),
	}, nil
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
		if pruner.pruneLk.TryLock() {
			go func() {
				defer pruner.pruneLk.Unlock()

				if err := pruner.prune(ctx, pruner.threshold*pruner.pruneBytes); err != nil {
					log.Errorf("Random pruner errored during prune: %v", err)
				}
			}()
		}
	}
}

// This is a potentially long-running operation, start it as a goroutine!
//
// TODO: definitely want to add a span here
func (pruner *RandomPruner) prune(ctx context.Context, bytesToPrune uint64) error {

	log.Infof("Starting prune operation with original datastore size of %s", humanize.IBytes(pruner.size))
	start := time.Now()

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
	const cidPadLength = 64
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

		paddedCidStr := fmt.Sprintf("%-*s", cidPadLength, cid.String())
		if _, err := writer.WriteString(paddedCidStr + "\n"); err != nil {
			return fmt.Errorf("failed to write cid to tmp file: %w", err)
		}
		cidCount++
	}
	writer.Flush()

	// Choose random blocks and remove them until the requested amount of bytes
	// have been pruned
	bytesPruned := uint64(0)
	// notFoundCount is used to avoid infinitely spinning through here if no
	// blocks appear to be left to remove - concretely detecting that all blocks
	// have been covered actually seems to be a difficult problem
	notFoundCount := 0

	defer func() {
		duration := time.Since(start)
		log.Infof("Prune operation finished after %s with new datastore size of %s (removed %v)", duration, humanize.IBytes(pruner.size), humanize.IBytes(bytesPruned))
	}()

	for bytesPruned < bytesToPrune {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Seek to the line with the CID, read it and parse
		cidIndex := rand.Int() % cidCount
		if _, err := tmpFile.Seek((cidPadLength+1)*int64(cidIndex), io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek back to start of tmp file: %w", err)
		}
		scanner := bufio.NewScanner(tmpFile)
		scanner.Scan()
		cidStr := scanner.Text()
		cid, err := cid.Parse(strings.TrimSpace(cidStr))
		if err != nil {
			log.Errorf("Failed to parse cid for removal (%s): %v", cidStr, err)
			continue
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
	}

	return nil
}

func (pruner *RandomPruner) updatePin(cid cid.Cid) {
	pruner.pinsLk.Lock()
	pruner.pins[cid] = time.Now()
	pruner.pinsLk.Unlock()
}

// Remove pins that have passed the pin duration
func (pruner *RandomPruner) cleanPins(ctx context.Context) error {
	pruner.pinsLk.Lock()
	defer pruner.pinsLk.Unlock()

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
	pruner.pinsLk.Lock()
	defer pruner.pinsLk.Unlock()

	lastUse, ok := pruner.pins[cid]
	if !ok {
		return false
	}

	return time.Since(lastUse) < pruner.pinDuration
}
