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

	"github.com/ipfs/go-cid"
	flatfs "github.com/ipfs/go-ds-flatfs"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
)

type RandomPrunerConfig struct {
	// The amount of bytes at which the blockstore will run a prune operation -
	// cannot be zero
	Threshold uint64

	// Min bytes to remove during each prune operation - a value higher than
	// MaxCapacityBytes shall prune all applicable blocks; a value of 0 shall
	// default to half of MaxCapacityBytes
	PruneBytes uint64
}

// RandomPruner is a blockstore wrapper which removes blocks at random when disk
// space is exhausted
type RandomPruner struct {
	blockstore.Blockstore
	datastore  *flatfs.Datastore
	threshold  uint64
	pruneBytes uint64
	lk         sync.Mutex
}

// The datastore that was used to create the blockstore is a required parameter
// used for calculating remaining disk space
func NewPruner(inner blockstore.Blockstore, datastore *flatfs.Datastore, cfg RandomPrunerConfig) *RandomPruner {
	if cfg.Threshold == 0 {
		// Always, immediately fail
		panic("prune threshold cannot be 0")
	}

	if cfg.PruneBytes == 0 {
		cfg.PruneBytes = cfg.Threshold / 2
	}

	if cfg.Threshold > cfg.PruneBytes {
		cfg.Threshold = cfg.PruneBytes
	}

	return &RandomPruner{
		Blockstore: inner,
		datastore:  datastore,
		threshold:  cfg.Threshold,
		pruneBytes: cfg.PruneBytes,
	}
}

// Checks remaining blockstore capacity and prunes if maximum capacity is hit
func (pruner *RandomPruner) Poll(ctx context.Context) {
	size, err := pruner.datastore.DiskUsage(ctx)
	if err != nil {
		log.Errorf("Failed to poll random pruner: could not get disk usage: %v", err)
	}

	if size >= pruner.threshold {
		go func() {
			if err := pruner.prune(ctx, pruner.threshold*pruner.pruneBytes); err != nil {
				log.Errorf("Random pruner failed to prune: %v", err)
			}
		}()
	}
}

// This is a potentially long-running operation, start it as a goroutine!
//
// TODO: definitely want to add a span here
func (pruner *RandomPruner) prune(ctx context.Context, bytesToPrune uint64) error {
	if !pruner.lk.TryLock() {
		return fmt.Errorf("already pruning")
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

	// Write every block on a separate line to the temporary file
	writer := bufio.NewWriter(tmpFile)
	cidCount := 0
	for cid := range allCids {
		if ctx.Err() != nil {
			return ctx.Err()
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
	// blocks appear to be left to remove
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
			// If the block was already removed, ignore it up to a few times
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

func (pruner *RandomPruner) Put(ctx context.Context, block Block) {
	pruner.Blockstore.Put(ctx, block)
	pruner.Poll(ctx)
}

func (pruner *RandomPruner) PutMany(ctx context.Context, blocks []Block) {
	pruner.Blockstore.PutMany(ctx, blocks)
	pruner.Poll(ctx)
}
