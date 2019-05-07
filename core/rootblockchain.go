// Modified from go-ethereum under GNU Lesser General Public License

package core

import (
	"errors"
	"fmt"
	"github.com/QuarkChain/goquarkchain/account"
	"io"
	"math/big"
	mrand "math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuarkChain/goquarkchain/cluster/config"
	"github.com/QuarkChain/goquarkchain/consensus"
	"github.com/QuarkChain/goquarkchain/core/rawdb"
	"github.com/QuarkChain/goquarkchain/core/types"
	"github.com/QuarkChain/goquarkchain/serialize"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	lru "github.com/hashicorp/golang-lru"
)

var (
	blockInsertTimer     = metrics.NewRegisteredTimer("chain/inserts", nil)
	blockValidationTimer = metrics.NewRegisteredTimer("chain/validation", nil)
	blockExecutionTimer  = metrics.NewRegisteredTimer("chain/execution", nil)
	blockWriteTimer      = metrics.NewRegisteredTimer("chain/write", nil)

	ErrNoGenesis = errors.New("Genesis not found in chain")
)

const (
	bodyCacheLimit      = 256
	blockCacheLimit     = 256
	receiptsCacheLimit  = 32
	maxFutureBlocks     = 256
	maxTimeFutureBlocks = 30
	badBlockLimit       = 10
	triesInMemory       = 128

	// BlockChainVersion ensures that an incompatible database forces a resync from scratch.
	BlockChainVersion = 3
)

// CacheConfig contains the configuration values for the trie caching/pruning
// that's resident in a blockchain.
type CacheConfig struct {
	Disabled       bool          // Whether to disable trie write caching (archive node)
	TrieCleanLimit int           // Memory allowance (MB) to use for caching trie nodes in memory
	TrieDirtyLimit int           // Memory limit (MB) at which to start flushing dirty trie nodes to disk
	TrieTimeLimit  time.Duration // Time limit after which to flush the current in-memory trie to disk
}

// RootBlockChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The RootBlockChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.
type RootBlockChain struct {
	chainConfig *config.QuarkChainConfig // Chain & network configuration
	cacheConfig *CacheConfig             // Cache configuration for pruning

	db     ethdb.Database // Low level persistent database to store final content in
	triegc *prque.Prque   // Priority queue mapping block numbers to tries to gc
	gcproc time.Duration  // Accumulates canonical block processing for trie dumping

	headerChain          *RootHeaderChain
	validatedMinorBlocks map[common.Hash]*serialize.Uint256
	rmLogsFeed           event.Feed
	chainFeed            event.Feed
	chainSideFeed        event.Feed
	chainHeadFeed        event.Feed
	logsFeed             event.Feed
	scope                event.SubscriptionScope
	genesisBlock         *types.RootBlock

	mu      sync.RWMutex // global mutex for locking chain operations
	chainmu sync.RWMutex // blockchain insertion lock
	procmu  sync.RWMutex // block processor lock

	checkpoint   int          // checkpoint counts towards the new checkpoint
	currentBlock atomic.Value // Current head of the block chain

	blockCache   *lru.Cache // Cache for the most recent entire blocks
	futureBlocks *lru.Cache // future blocks are blocks added for later processing

	quit    chan struct{} // blockchain quit channel
	running int32         // running must be called atomically
	// procInterrupt must be atomically called
	procInterrupt int32          // interrupt signaler for block processing
	wg            sync.WaitGroup // chain processing wait group for shutting down

	engine    consensus.Engine
	validator Validator // block and state validator interface

	badBlocks      *lru.Cache                        // Bad block cache
	shouldPreserve func(block *types.RootBlock) bool // Function used to determine whether should preserve the given block.
}

// NewBlockChain returns a fully initialized block chain using information
// available in the database. It initializes the default Ethereum Validator and
// Processor.
func NewRootBlockChain(db ethdb.Database, cacheConfig *CacheConfig, chainConfig *config.QuarkChainConfig, engine consensus.Engine, shouldPreserve func(block *types.RootBlock) bool) (*RootBlockChain, error) {
	if cacheConfig == nil {
		cacheConfig = &CacheConfig{
			TrieCleanLimit: 256,
			TrieDirtyLimit: 256,
			TrieTimeLimit:  5 * time.Minute,
		}
	}
	blockCache, _ := lru.New(blockCacheLimit)
	futureBlocks, _ := lru.New(maxFutureBlocks)
	badBlocks, _ := lru.New(badBlockLimit)

	bc := &RootBlockChain{
		chainConfig:    chainConfig,
		cacheConfig:    cacheConfig,
		db:             db,
		triegc:         prque.New(nil),
		quit:           make(chan struct{}),
		shouldPreserve: shouldPreserve,
		blockCache:     blockCache,
		futureBlocks:   futureBlocks,
		engine:         engine,
		badBlocks:      badBlocks,
	}
	bc.SetValidator(NewRootBlockValidator(chainConfig, bc, engine))

	var err error
	bc.headerChain, err = NewHeaderChain(db, chainConfig, engine, bc.getProcInterrupt)
	if err != nil {
		return nil, err
	}
	genesisBlock := bc.GetBlockByNumber(0)
	if genesisBlock == nil {
		return nil, ErrNoGenesis
	}
	bc.genesisBlock = genesisBlock.(*types.RootBlock)

	if err := bc.loadLastState(); err != nil {
		return nil, err
	}
	// Take ownership of this particular state
	go bc.update()
	return bc, nil
}

func (bc *RootBlockChain) getProcInterrupt() bool {
	return atomic.LoadInt32(&bc.procInterrupt) == 1
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (bc *RootBlockChain) loadLastState() error {
	// Restore the last known head block
	head := rawdb.ReadHeadBlockHash(bc.db)
	if head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		log.Warn("Empty database, resetting chain")
		return bc.Reset()
	}
	// Make sure the entire head block is available
	currentBlock := bc.GetBlock(head)
	if currentBlock == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head block missing, resetting chain", "hash", head)
		return bc.Reset()
	}
	// Everything seems to be fine, set as the head block
	bc.currentBlock.Store(currentBlock)

	// Restore the last known head header
	currentHeader := currentBlock.IHeader()
	if head := rawdb.ReadHeadHeaderHash(bc.db); head != (common.Hash{}) {
		if header := bc.GetHeader(head); header != nil {
			currentHeader = header
		}
	}
	bc.headerChain.SetCurrentHeader(currentHeader.(*types.RootBlockHeader))

	headerTd := bc.GetTd(currentHeader.Hash())
	blockTd := bc.GetTd(currentBlock.Hash())

	log.Info("Loaded most recent local header", "number", currentHeader.NumberU64(), "hash", currentHeader.Hash(), "td", headerTd, "age", common.PrettyAge(time.Unix(int64(currentHeader.GetTime()), 0)))
	log.Info("Loaded most recent local full block", "number", currentBlock.NumberU64(), "hash", currentBlock.Hash(), "td", blockTd, "age", common.PrettyAge(time.Unix(int64(currentBlock.IHeader().GetTime()), 0)))

	return nil
}

// SetHead rewinds the local chain to a new head. In the case of headers, everything
// above the new head will be deleted and the new one set. In the case of blocks
// though, the head may be further rewound if block bodies are missing (non-archive
// nodes after a fast sync).
func (bc *RootBlockChain) SetHead(head uint64) error {
	log.Warn("Rewinding blockchain", "target", head)

	bc.mu.Lock()
	defer bc.mu.Unlock()

	// Rewind the header chain, deleting all block bodies until then
	delFn := func(db rawdb.DatabaseDeleter, hash common.Hash) {
		rawdb.DeleteBlock(db, hash)
	}
	bc.headerChain.SetHead(head, delFn)
	currentHeader := bc.headerChain.CurrentHeader()

	// Clear out any stale content from the caches
	bc.blockCache.Purge()
	bc.futureBlocks.Purge()

	// Rewind the block chain, ensuring we don't end up with a stateless head block
	if currentBlock := bc.CurrentBlock(); currentBlock != nil && currentHeader.NumberU64() < currentBlock.NumberU64() {
		bc.currentBlock.Store(bc.GetBlock(currentHeader.Hash()))
	}

	// If either blocks reached nil, reset to the genesis state
	if currentBlock := bc.CurrentBlock(); currentBlock == nil {
		bc.currentBlock.Store(bc.genesisBlock)
	}
	currentBlock := bc.CurrentBlock()

	rawdb.WriteHeadBlockHash(bc.db, currentBlock.Hash())

	return bc.loadLastState()
}

// CurrentBlock retrieves the current head block of the canonical chain. The
// block is retrieved from the blockchain's internal cache.
func (bc *RootBlockChain) CurrentBlock() *types.RootBlock {
	return bc.currentBlock.Load().(*types.RootBlock)
}

// SetValidator sets the validator which is used to validate incoming blocks.
func (bc *RootBlockChain) SetValidator(validator Validator) {
	bc.procmu.Lock()
	defer bc.procmu.Unlock()
	bc.validator = validator
}

// Validator returns the current validator.
func (bc *RootBlockChain) Validator() Validator {
	bc.procmu.RLock()
	defer bc.procmu.RUnlock()
	return bc.validator
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (bc *RootBlockChain) Reset() error {
	return bc.ResetWithGenesisBlock(bc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (bc *RootBlockChain) ResetWithGenesisBlock(genesis *types.RootBlock) error {
	// Dump the entire block chain and purge the caches
	if err := bc.SetHead(0); err != nil {
		return err
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	if err := bc.headerChain.WriteTd(genesis.Hash(), genesis.Difficulty()); err != nil {
		log.Crit("Failed to write genesis block TD", "err", err)
	}
	rawdb.WriteRootBlock(bc.db, genesis)

	bc.genesisBlock = genesis
	bc.insert(bc.genesisBlock)
	bc.currentBlock.Store(bc.genesisBlock)
	bc.headerChain.SetGenesis(bc.genesisBlock.Header())
	bc.headerChain.SetCurrentHeader(bc.genesisBlock.Header())

	return nil
}

// repair tries to repair the current blockchain by rolling back the current block
// until one with associated state is found. This is needed to fix incomplete db
// writes caused either by crashes/power outages, or simply non-committed tries.
//
// This method only rolls back the current block. The current header and current
// fast block are left intact.
func (bc *RootBlockChain) repair(head **types.RootBlock) error {
	for {
		block := bc.GetBlock((*head).ParentHash())
		if block == nil {
			return fmt.Errorf("missing block %d [%x]", (*head).NumberU64()-1, (*head).ParentHash())
		}
		(*head) = block.(*types.RootBlock)
	}
}

// Export writes the active chain to the given writer.
func (bc *RootBlockChain) Export(w io.Writer) error {
	return bc.ExportN(w, uint64(0), bc.CurrentBlock().NumberU64())
}

// ExportN writes a subset of the active chain to the given writer.
func (bc *RootBlockChain) ExportN(w io.Writer, first uint64, last uint64) error {
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}
	log.Info("Exporting batch of blocks", "count", last-first+1)

	start, reported := time.Now(), time.Now()
	for nr := first; nr <= last; nr++ {
		block := bc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}
		data, err := serialize.SerializeToBytes(block)
		if err != nil {
			return err
		}
		w.Write(data)
		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}

	return nil
}

// insert injects a new head block into the current block chain. This method
// assumes that the block is indeed a true head. It will also reset the head
// header and the head fast sync block to this very same block if they are older
// or if they are on a different side chain.
//
// Note, this function assumes that the `mu` mutex is held!
func (bc *RootBlockChain) insert(block *types.RootBlock) {
	// If the block is on a side chain or an unknown one, force other heads onto it too
	updateHeads := rawdb.ReadCanonicalHash(bc.db, rawdb.ChainTypeRoot, block.NumberU64()) != block.Hash()

	// Add the block to the canonical chain number scheme and mark as the head
	rawdb.WriteCanonicalHash(bc.db, rawdb.ChainTypeRoot, block.Hash(), block.NumberU64())
	rawdb.WriteHeadBlockHash(bc.db, block.Hash())

	bc.currentBlock.Store(block)

	// If the block is better than our head or is on a different chain, force update heads
	if updateHeads {
		bc.headerChain.SetCurrentHeader(block.Header())
	}
}

// Genesis retrieves the chain's genesis block.
func (bc *RootBlockChain) Genesis() *types.RootBlock {
	return bc.genesisBlock
}

// HasBlock checks if a block is fully present in the database or not.
func (bc *RootBlockChain) HasBlock(hash common.Hash) bool {
	if bc.blockCache.Contains(hash) {
		return true
	}
	return rawdb.HasBlock(bc.db, hash)
}

// GetBlock retrieves a block from the database by hash and number,
// caching it if found.
func (bc *RootBlockChain) GetBlock(hash common.Hash) types.IBlock {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := bc.blockCache.Get(hash); ok {
		return block.(*types.RootBlock)
	}
	block := rawdb.ReadRootBlock(bc.db, hash)
	if block == nil {
		return nil
	}
	// Cache the found block for next time and return
	bc.blockCache.Add(block.Hash(), block)
	return block
}

// GetBlockByNumber retrieves a block from the database by number, caching it
// (associated with its hash) if found.
func (bc *RootBlockChain) GetBlockByNumber(number uint64) types.IBlock {
	hash := rawdb.ReadCanonicalHash(bc.db, rawdb.ChainTypeRoot, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return bc.GetBlock(hash)
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (bc *RootBlockChain) Stop() {
	if !atomic.CompareAndSwapInt32(&bc.running, 0, 1) {
		return
	}
	// Unsubscribe all subscriptions registered from blockchain
	bc.scope.Close()
	close(bc.quit)
	atomic.StoreInt32(&bc.procInterrupt, 1)

	bc.wg.Wait()
	log.Info("Blockchain manager stopped")
}

func (bc *RootBlockChain) procFutureBlocks() {
	blocks := make([]types.IBlock, 0, bc.futureBlocks.Len())
	for _, hash := range bc.futureBlocks.Keys() {
		if block, exist := bc.futureBlocks.Peek(hash); exist {
			blocks = append(blocks, block.(*types.RootBlock))
		}
	}
	if len(blocks) > 0 {
		sort.Slice(blocks, func(i, j int) bool { return blocks[i].NumberU64() < blocks[j].NumberU64() })

		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		for i := range blocks {
			bc.InsertChain(blocks[i : i+1])
		}
	}
}

// WriteStatus status of write
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota
	CanonStatTy
	SideStatTy
)

// Rollback is designed to remove a chain of links from the database that aren't
// certain enough to be valid.
func (bc *RootBlockChain) Rollback(chain []common.Hash) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	for i := len(chain) - 1; i >= 0; i-- {
		hash := chain[i]

		currentHeader := bc.headerChain.CurrentHeader()
		if currentHeader.Hash() == hash {
			bc.headerChain.SetCurrentHeader(bc.GetHeader(currentHeader.GetParentHash()).(*types.RootBlockHeader))
		}
		if currentBlock := bc.CurrentBlock(); currentBlock.Hash() == hash {
			newBlock := bc.GetBlock(currentBlock.ParentHash())
			bc.currentBlock.Store(newBlock)
			rawdb.WriteHeadBlockHash(bc.db, newBlock.Hash())
		}
	}
}

var lastWrite uint64

// WriteBlockWithoutState writes only the block and its metadata to the database,
// but does not write any state. This is used to construct competing side forks
// up to the point where they exceed the canonical total difficulty.
func (bc *RootBlockChain) WriteBlockWithoutState(block types.IBlock, td *big.Int) (err error) {
	bc.wg.Add(1)
	defer bc.wg.Done()

	if err := bc.headerChain.WriteTd(block.Hash(), td); err != nil {
		return err
	}
	rawdb.WriteRootBlock(bc.db, block.(*types.RootBlock))

	return nil
}

//todo
// WriteBlockWithState writes the block and all associated state to the database.
func (bc *RootBlockChain) WriteBlockWithState(block *types.RootBlock) (status WriteStatus, err error) {
	bc.wg.Add(1)
	defer bc.wg.Done()

	// Calculate the total difficulty of the block
	ptd := bc.GetTd(block.ParentHash())
	if ptd == nil {
		return NonStatTy, errors.New("unknown ancestor")
	}
	// Make sure no inconsistent state is leaked during insertion
	bc.mu.Lock()
	defer bc.mu.Unlock()

	currentBlock := bc.CurrentBlock()
	localTd := bc.GetTd(currentBlock.Hash())
	externTd := new(big.Int).Add(block.Difficulty(), ptd)

	// Irrelevant of the canonical status, write the block itself to the database
	if err := bc.headerChain.WriteTd(block.Hash(), externTd); err != nil {
		return NonStatTy, err
	}
	rawdb.WriteRootBlock(bc.db, block)

	// Write other block data using a batch.
	batch := bc.db.NewBatch()

	// If the total difficulty is higher than our known, add it to the canonical chain
	// Second clause in the if statement reduces the vulnerability to selfish mining.
	// Please refer to http://www.cs.cornell.edu/~ie53/publications/btcProcFC.pdf
	reorg := externTd.Cmp(localTd) > 0
	currentBlock = bc.CurrentBlock()
	if !reorg && externTd.Cmp(localTd) == 0 {
		// Split same-difficulty blocks by number, then preferentially select
		// the block generated by the local miner as the canonical block.
		if block.NumberU64() < currentBlock.NumberU64() {
			reorg = true
		} else if block.NumberU64() == currentBlock.NumberU64() {
			var currentPreserve, blockPreserve bool
			if bc.shouldPreserve != nil {
				currentPreserve, blockPreserve = bc.shouldPreserve(currentBlock), bc.shouldPreserve(block)
			}
			reorg = !currentPreserve && (blockPreserve || mrand.Float64() < 0.5)
		}
	}
	if reorg {
		// Reorganise the chain if the parent is not the head block
		if block.ParentHash() != currentBlock.Hash() {
			if err := bc.reorg(currentBlock, block); err != nil {
				return NonStatTy, err
			}
		}
		// Write the positional metadata for transaction/receipt lookups and preimages
		rawdb.WriteBlockContentLookupEntries(batch, block)

		status = CanonStatTy
	} else {
		status = SideStatTy
	}
	if err := batch.Write(); err != nil {
		return NonStatTy, err
	}

	// Set new head.
	if status == CanonStatTy {
		bc.insert(block)
	}
	bc.futureBlocks.Remove(block.Hash())
	return status, nil
}

// addFutureBlock checks if the block is within the max allowed window to get
// accepted for future processing, and returns an error if the block is too far
// ahead and was not added.
func (bc *RootBlockChain) addFutureBlock(block types.IBlock) error {
	max := big.NewInt(time.Now().Unix() + maxTimeFutureBlocks).Uint64()
	if block.IHeader().GetTime() > max {
		return fmt.Errorf("future block timestamp %v > allowed %v", block.IHeader().GetTime(), max)
	}
	bc.futureBlocks.Add(block.Hash(), block)
	return nil
}

// InsertChain attempts to insert the given batch of blocks in to the canonical
// chain or, otherwise, create a fork. If an error is returned it will return
// the index number of the failing block as well an error describing what went
// wrong.
//
// After insertion is done, all accumulated events will be fired.
func (bc *RootBlockChain) InsertChain(chain []types.IBlock) (int, error) {
	for _, v := range chain {
		log.Info("RootBlockChain", "InsertChain-num", v.IHeader().NumberU64())
	}
	defer log.Info("RootBlockChain", "InsertChain", "end")
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}
	// Do a sanity check that the provided chain is actually ordered and linked
	for i := 1; i < len(chain); i++ {
		if chain[i].NumberU64() != chain[i-1].NumberU64()+1 || chain[i].IHeader().GetParentHash() != chain[i-1].Hash() {
			// Chain broke ancestry, log a message (programming error) and skip insertion
			log.Error("Non contiguous block insert", "number", chain[i].NumberU64(), "hash", chain[i].Hash(),
				"parent", chain[i].IHeader().GetParentHash(), "prevnumber", chain[i-1].NumberU64(), "prevhash", chain[i-1].Hash())

			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x…], item %d is #%d [%x…] (parent [%x…])", i-1, chain[i-1].NumberU64(),
				chain[i-1].Hash().Bytes()[:4], i, chain[i].NumberU64(), chain[i].Hash().Bytes()[:4], chain[i].IHeader().GetParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	bc.wg.Add(1)
	bc.chainmu.Lock()
	n, events, err := bc.insertChain(chain, true)
	bc.chainmu.Unlock()
	bc.wg.Done()

	bc.PostChainEvents(events)
	return n, err
}

// insertChain is the internal implementation of insertChain, which assumes that
// 1) chains are contiguous, and 2) The chain mutex is held.
//
// This method is split out so that import batches that require re-injecting
// historical blocks can do so without releasing the lock, which could lead to
// racey behaviour. If a sidechain import is in progress, and the historic state
// is imported, but then new canon-head is added before the actual sidechain
// completes, then the historic state could be pruned again
func (bc *RootBlockChain) insertChain(chain []types.IBlock, verifySeals bool) (int, []interface{}, error) {
	// If the chain is terminating, don't even bother starting u
	if atomic.LoadInt32(&bc.procInterrupt) == 1 {
		return 0, nil, nil
	}

	// A queued approach to delivering events. This is generally
	// faster than direct delivery and requires much less mutex
	// acquiring.
	var (
		stats     = insertStats{startTime: mclock.Now()}
		events    = make([]interface{}, 0, len(chain))
		lastCanon *types.RootBlock
	)
	// Start the parallel header verifier
	headers := make([]types.IHeader, len(chain))
	seals := make([]bool, len(chain))

	for i, block := range chain {
		headers[i] = block.IHeader()
		seals[i] = verifySeals
	}
	abort, results := bc.engine.VerifyHeaders(bc, headers, seals)
	defer close(abort)

	// Peek the error for the first block to decide the directing import logic
	it := newInsertIterator(chain, results, bc.Validator())

	block, err := it.next()
	switch {
	// First block is pruned, insert as sidechain and reorg only if TD grows enough
	case err == ErrPrunedAncestor:
		return bc.insertSidechain(it)

	// First block is future, shove it (and all children) to the future queue (unknown ancestor)
	case err == ErrFutureBlock || (err == ErrUnknownAncestor && bc.futureBlocks.Contains(it.first().IHeader().GetParentHash())):
		for block != nil && (it.index == 0 || err == ErrUnknownAncestor) {
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, events, err
			}
			block, err = it.next()
		}
		stats.queued += it.processed()
		stats.ignored += it.remaining()

		// If there are any still remaining, mark as ignored
		return it.index, events, err

	// First block (and state) is known
	//   1. We did a roll-back, and should now do a re-import
	//   2. The block is stored as a sidechain, and is lying about it's stateroot, and passes a stateroot
	// 	    from the canonical chain, which has not been verified.
	case err == ErrKnownBlock:
		// Skip all known blocks that behind us
		current := bc.CurrentBlock().NumberU64()

		for block != nil && err == ErrKnownBlock && current >= block.NumberU64() {
			stats.ignored++
			block, err = it.next()
		}
		// Falls through to the block import

	// Some other error occurred, abort
	case err != nil:
		stats.ignored += len(it.chain)
		bc.reportBlock(block, err)
		return it.index, events, err
	}
	// No validation errors for the first block (or chain prefix skipped)
	for ; block != nil && err == nil; block, err = it.next() {
		// If the chain is terminating, stop processing blocks
		if atomic.LoadInt32(&bc.procInterrupt) == 1 {
			log.Debug("Premature abort during blocks processing")
			break
		}
		// Retrieve the parent block and it's state to execute on top
		start := time.Now()

		parent := it.previous()
		if parent == nil {
			parent = bc.GetBlock(block.IHeader().GetParentHash())
		}
		t0 := time.Now()
		if err != nil {
			bc.reportBlock(block, err)
			return it.index, events, err
		}
		// Validate the state using the default validator
		if err := bc.Validator().ValidateBlock(block); err != nil {
			bc.reportBlock(block, err)
			return it.index, events, err
		}
		t1 := time.Now()
		proctime := time.Since(start)

		// Write the block to the chain and get the status.
		status, err := bc.WriteBlockWithState(block.(*types.RootBlock))
		t2 := time.Now()
		if err != nil {
			return it.index, events, err
		}
		blockInsertTimer.UpdateSince(start)
		blockValidationTimer.Update(t1.Sub(t0))
		blockValidationTimer.Update(t2.Sub(t1))
		switch status {
		case CanonStatTy:
			log.Debug("Inserted new block", "number", block.NumberU64(), "hash", block.Hash(),
				"minorHeaderd", len(block.Content()), "elapsed", common.PrettyDuration(time.Since(start)))
			lastCanon = block.(*types.RootBlock)
			events = append(events, RootChainEvent{lastCanon, block.Hash()})

			// Only count canonical blocks for GC processing time
			bc.gcproc += proctime

		case SideStatTy:
			log.Debug("Inserted forked block", "number", block.NumberU64(), "hash", block.Hash(),
				"diff", block.IHeader().GetDifficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"headblock", len(block.Content()))
			events = append(events, RootChainSideEvent{block.(*types.RootBlock)})
		}
		blockInsertTimer.UpdateSince(start)
		stats.processed++
		stats.report(chain, it.index)
	}
	// Any blocks remaining here? The only ones we care about are the future ones
	if block != nil && err == ErrFutureBlock {
		if err := bc.addFutureBlock(block); err != nil {
			return it.index, events, err
		}
		block, err = it.next()

		for ; block != nil && err == ErrUnknownAncestor; block, err = it.next() {
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, events, err
			}
			stats.queued++
		}
	}
	stats.ignored += it.remaining()

	// Append a single chain head event if we've progressed the chain
	if lastCanon != nil && bc.CurrentBlock().Hash() == lastCanon.Hash() {
		events = append(events, RootChainHeadEvent{lastCanon})
	}
	return it.index, events, err
}

// insertSidechain is called when an import batch hits upon a pruned ancestor
// error, which happens when a sidechain with a sufficiently old fork-block is
// found.
//
// The method writes all (header-and-body-valid) blocks to disk, then tries to
// switch over to the new chain if the TD exceeded the current chain.
func (bc *RootBlockChain) insertSidechain(it *insertIterator) (int, []interface{}, error) {
	var (
		externTd *big.Int
		current  = bc.CurrentBlock().NumberU64()
	)
	// The first sidechain block error is already verified to be ErrPrunedAncestor.
	// Since we don't import them here, we expect ErrUnknownAncestor for the remaining
	// ones. Any other errors means that the block is invalid, and should not be written
	// to disk.
	block, err := it.current(), ErrPrunedAncestor
	for ; block != nil && (err == ErrPrunedAncestor); block, err = it.next() {
		// Check the canonical state root for that number
		if number := block.NumberU64(); current >= number {
			if canonical := bc.GetBlockByNumber(number); canonical != nil && canonical.Hash() == block.Hash() {
				// This is most likely a shadow-state attack. When a fork is imported into the
				// database, and it eventually reaches a block height which is not pruned, we
				// just found that the state already exist! This means that the sidechain block
				// refers to a state which already exists in our canon chain.
				//
				// If left unchecked, we would now proceed importing the blocks, without actually
				// having verified the state of the previous blocks.
				log.Warn("Sidechain ghost-state attack detected", "number", block.NumberU64(), "sideroot")

				// If someone legitimately side-mines blocks, they would still be imported as usual. However,
				// we cannot risk writing unverified blocks to disk when they obviously target the pruning
				// mechanism.
				return it.index, nil, errors.New("sidechain ghost-state attack")
			}
		}
		if externTd == nil {
			externTd = bc.GetTd(block.IHeader().GetParentHash())
		}
		externTd = new(big.Int).Add(externTd, block.IHeader().GetDifficulty())

		if !bc.HasBlock(block.Hash()) {
			start := time.Now()
			if err := bc.WriteBlockWithoutState(block, externTd); err != nil {
				return it.index, nil, err
			}
			log.Debug("Inserted sidechain block", "number", block.NumberU64(), "hash", block.Hash(),
				"diff", block.IHeader().GetDifficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"headers", len(block.Content()))
		}
	}
	// At this point, we've written all sidechain blocks to database. Loop ended
	// either on some other error or all were processed. If there was some other
	// error, we can ignore the rest of those blocks.
	//
	// If the externTd was larger than our local TD, we now need to reimport the previous
	// blocks to regenerate the required state
	localTd := bc.GetTd(bc.CurrentBlock().Hash())
	if localTd.Cmp(externTd) > 0 {
		log.Info("Sidechain written to disk", "start", it.first().NumberU64(), "end", it.previous().NumberU64(), "sidetd", externTd, "localtd", localTd)
		return it.index, nil, err
	}
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes []common.Hash
	)
	parent := bc.GetHeader(it.previous().Hash())
	for parent != nil {
		hashes = append(hashes, parent.Hash())
		parent = bc.GetHeader(parent.GetParentHash())
	}
	if parent == nil {
		return it.index, nil, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	var (
		blocks []types.IBlock
	)
	for i := len(hashes) - 1; i >= 0; i-- {
		// Append the next block to our batch
		block := bc.GetBlock(hashes[i])

		blocks = append(blocks, block)

		// If memory use grew too large, import and continue. Sadly we need to discard
		// all raised events and logs from notifications since we're too heavy on the
		// memory here.
		if len(blocks) >= 2048 {
			log.Info("Importing heavy sidechain segment", "blocks", len(blocks), "start", blocks[0].NumberU64(), "end", block.NumberU64())
			if _, _, err := bc.insertChain(blocks, false); err != nil {
				return 0, nil, err
			}
			blocks = blocks[:0]

			// If the chain is terminating, stop processing blocks
			if atomic.LoadInt32(&bc.procInterrupt) == 1 {
				log.Debug("Premature abort during blocks processing")
				return 0, nil, nil
			}
		}
	}
	if len(blocks) > 0 {
		log.Info("Importing sidechain segment", "start", blocks[0].NumberU64(), "end", blocks[len(blocks)-1].NumberU64())
		return bc.insertChain(blocks, false)
	}
	return 0, nil, nil
}

// reorgs takes two blocks, an old chain and a new chain and will reconstruct the blocks and inserts them
// to be part of the new canonical chain and accumulates potential missing transactions and post an
// event about them
func (bc *RootBlockChain) reorg(oldBlock, newBlock types.IBlock) error {
	var (
		newChain       []types.IBlock
		oldChain       []types.IBlock
		commonBlock    types.IBlock
		deletedHeaders types.MinorBlockHeaders
	)

	// first reduce whoever is higher bound
	if oldBlock.NumberU64() > newBlock.NumberU64() {
		// reduce old chain
		for ; oldBlock != nil && oldBlock.NumberU64() != newBlock.NumberU64(); oldBlock = bc.GetBlock(oldBlock.IHeader().GetParentHash()) {
			oldChain = append(oldChain, oldBlock)
			deletedHeaders = append(deletedHeaders, oldBlock.(*types.RootBlock).MinorBlockHeaders()...)
		}
	} else {
		// reduce new chain and append new chain blocks for inserting later on
		for ; newBlock != nil && newBlock.NumberU64() != oldBlock.NumberU64(); newBlock = bc.GetBlock(newBlock.IHeader().GetParentHash()) {
			newChain = append(newChain, newBlock)
		}
	}
	if oldBlock == nil {
		return fmt.Errorf("Invalid old chain")
	}
	if newBlock == nil {
		return fmt.Errorf("Invalid new chain")
	}

	for {
		if oldBlock.Hash() == newBlock.Hash() {
			commonBlock = oldBlock
			break
		}

		oldChain = append(oldChain, oldBlock)
		newChain = append(newChain, newBlock)

		oldBlock, newBlock = bc.GetBlock(oldBlock.IHeader().GetParentHash()), bc.GetBlock(newBlock.IHeader().GetParentHash())
		if oldBlock == nil {
			return fmt.Errorf("Invalid old chain")
		}
		if newBlock == nil {
			return fmt.Errorf("Invalid new chain")
		}
	}
	// Ensure the user sees large reorgs
	if len(oldChain) > 0 && len(newChain) > 0 {
		logFn := log.Debug
		if len(oldChain) > 63 {
			logFn = log.Warn
		}
		logFn("Chain split detected", "number", commonBlock.NumberU64(), "hash", commonBlock.Hash(),
			"drop", len(oldChain), "dropfrom", oldChain[0].Hash(), "add", len(newChain), "addfrom", newChain[0].Hash())
	} else {
		log.Error("Impossible reorg, please file an issue", "oldnum", oldBlock.NumberU64(), "oldhash", oldBlock.Hash(), "newnum", newBlock.NumberU64(), "newhash", newBlock.Hash())
	}
	// Insert the new chain, taking care of the proper incremental order
	var addedHeaders types.MinorBlockHeaders
	for i := len(newChain) - 1; i >= 0; i-- {
		// insert the block in the canonical way, re-writing history
		bc.insert(newChain[i].(*types.RootBlock))
		// write lookup entries for hash based transaction/receipt searches
		rawdb.WriteBlockContentLookupEntries(bc.db, newChain[i].(*types.RootBlock))
		addedHeaders = append(addedHeaders, newChain[i].(*types.RootBlock).MinorBlockHeaders()...)
	}
	// calculate the difference between deleted and added transactions
	diff := types.MinorHeaderDifference(deletedHeaders, addedHeaders)
	// When transactions get deleted from the database that means the
	// receipts that were created in the fork must also be deleted
	batch := bc.db.NewBatch()
	for _, item := range diff {
		rawdb.DeleteBlockContentLookupEntry(batch, item.Hash())
	}
	batch.Write()

	if len(oldChain) > 0 {
		go func() {
			for _, block := range oldChain {
				bc.chainSideFeed.Send(RootChainSideEvent{Block: block.(*types.RootBlock)})
			}
		}()
	}

	return nil
}

// PostChainEvents iterates over the events generated by a chain insertion and
// posts them into the event feed.
// TODO: Should not expose PostChainEvents. The chain events should be posted in WriteBlock.
func (bc *RootBlockChain) PostChainEvents(events []interface{}) {
	for _, event := range events {
		switch ev := event.(type) {
		case RootChainEvent:
			bc.chainFeed.Send(ev)

		case RootChainHeadEvent:
			bc.chainHeadFeed.Send(ev)

		case RootChainSideEvent:
			bc.chainSideFeed.Send(ev)
		}
	}
}

func (bc *RootBlockChain) update() {
	futureTimer := time.NewTicker(5 * time.Second)
	defer futureTimer.Stop()
	for {
		select {
		case <-futureTimer.C:
			bc.procFutureBlocks()
		case <-bc.quit:
			return
		}
	}
}

// BadBlocks returns a list of the last 'bad blocks' that the client has seen on the network
func (bc *RootBlockChain) BadBlocks() []*types.RootBlock {
	blocks := make([]*types.RootBlock, 0, bc.badBlocks.Len())
	for _, hash := range bc.badBlocks.Keys() {
		if blk, exist := bc.badBlocks.Peek(hash); exist {
			block := blk.(*types.RootBlock)
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// addBadBlock adds a bad block to the bad-block LRU cache
func (bc *RootBlockChain) addBadBlock(block *types.RootBlock) {
	bc.badBlocks.Add(block.Hash(), block)
}

// reportBlock logs a bad block error.
func (bc *RootBlockChain) reportBlock(block types.IBlock, err error) {
	bc.addBadBlock(block.(*types.RootBlock))

	log.Error(fmt.Sprintf(`
########## BAD BLOCK #########
Chain config: %v

Number: %v
Hash: 0x%x

Error: %v
##############################
`, bc.chainConfig, block.NumberU64(), block.Hash(), err))
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verify nonces, as well as
// because nonces can be verified sparsely, not needing to check each.
func (bc *RootBlockChain) InsertHeaderChain(chain []*types.RootBlockHeader, checkFreq int) (int, error) {
	start := time.Now()
	if i, err := bc.headerChain.ValidateHeaderChain(chain, checkFreq); err != nil {
		return i, err
	}

	// Make sure only one thread manipulates the chain at once
	bc.chainmu.Lock()
	defer bc.chainmu.Unlock()

	bc.wg.Add(1)
	defer bc.wg.Done()

	whFunc := func(header *types.RootBlockHeader) error {
		bc.mu.Lock()
		defer bc.mu.Unlock()

		_, err := bc.headerChain.WriteHeader(header)
		return err
	}

	return bc.headerChain.InsertHeaderChain(chain, whFunc, start)
}

// writeHeader writes a header into the local chain, given that its parent is
// already known. If the total difficulty of the newly inserted header becomes
// greater than the current known TD, the canonical chain is re-routed.
//
// Note: This method is not concurrent-safe with inserting blocks simultaneously
// into the chain, as side effects caused by reorganisations cannot be emulated
// without the real blocks. Hence, writing headers directly should only be done
// in two scenarios: pure-header mode of operation (light clients), or properly
// separated header/block phases (non-archive clients).
func (bc *RootBlockChain) writeHeader(header *types.RootBlockHeader) error {
	bc.wg.Add(1)
	defer bc.wg.Done()

	bc.mu.Lock()
	defer bc.mu.Unlock()

	_, err := bc.headerChain.WriteHeader(header)
	return err
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the RootHeaderChain's internal cache.
func (bc *RootBlockChain) CurrentHeader() types.IHeader {
	return bc.headerChain.CurrentHeader()
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (bc *RootBlockChain) GetTd(hash common.Hash) *big.Int {
	return bc.headerChain.GetTd(hash)
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
func (bc *RootBlockChain) GetHeader(hash common.Hash) types.IHeader {
	return bc.headerChain.GetHeader(hash)
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
func (bc *RootBlockChain) HasHeader(hash common.Hash) bool {
	return bc.headerChain.HasHeader(hash)
}

// GetBlockHashesFromHash retrieves a number of block hashes starting at a given
// hash, fetching towards the genesis block.
func (bc *RootBlockChain) GetBlockHashesFromHash(hash common.Hash, max uint64) []common.Hash {
	return bc.headerChain.GetBlockHashesFromHash(hash, max)
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
func (bc *RootBlockChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	bc.chainmu.Lock()
	defer bc.chainmu.Unlock()

	return bc.headerChain.GetAncestor(hash, number, ancestor, maxNonCanonical)
}

func (bc *RootBlockChain) isSameChain(longerChainHeader, shorterChainHeader *types.RootBlockHeader) bool {
	return false
	//TODO????
	//return bc.headerChain.isSameChain(longerChainHeader, shorterChainHeader)
}

func (bc *RootBlockChain) containMinorBlock(hash common.Hash) bool {
	// TODO fake
	return true
	//_, ok := bc.validatedMinorBlocks[hash]
	//return ok
}

func (bc *RootBlockChain) AddValidatedMinorBlockHeader(header types.IHeader) {
	bc.validatedMinorBlocks[header.Hash()] = header.(*types.MinorBlockHeader).CoinbaseAmount
}

func (bc *RootBlockChain) GetLatestMinorBlockHeaders(hash common.Hash) map[uint32]*types.MinorBlockHeader {
	headerMap := make(map[uint32]*types.MinorBlockHeader)
	headers := rawdb.ReadLatestMinorBlockHeaders(bc.db, hash)
	for _, header := range headers {
		headerMap[header.Branch.GetFullShardID()] = header
	}

	return headerMap
}

func (bc *RootBlockChain) SetLatestMinorBlockHeaders(hash common.Hash, headerMap map[uint32]*types.MinorBlockHeader) {
	headers := make([]*types.MinorBlockHeader, 0, len(headerMap))
	for _, header := range headerMap {
		headers = append(headers, header)
	}

	rawdb.WriteLatestMinorBlockHeaders(bc.db, hash, headers)
}

func CalculateRootBlockCoinbase(config *config.QuarkChainConfig, block *types.RootBlock) *big.Int {
	totalAmount := new(big.Int).SetUint64(0)
	rate := config.RewardTaxRate
	for _, header := range block.MinorBlockHeaders() {
		totalAmount = new(big.Int).Add(totalAmount, header.CoinbaseAmount.Value)
	}
	totalAmount = new(big.Int).Div(new(big.Int).Mul(totalAmount, new(big.Int).Sub(rate.Denom(), rate.Num())), rate.Denom())
	return new(big.Int).Add(config.Root.CoinbaseAmount, totalAmount)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (bc *RootBlockChain) GetHeaderByNumber(number uint64) types.IHeader {
	return bc.headerChain.GetHeaderByNumber(number)
}

// Config retrieves the blockchain's chain configuration.
func (bc *RootBlockChain) Config() *config.QuarkChainConfig { return bc.chainConfig }

// Engine retrieves the blockchain's consensus engine.
func (bc *RootBlockChain) Engine() consensus.Engine { return bc.engine }

// SubscribeRemovedLogsEvent registers a subscription of RemovedLogsEvent.
func (bc *RootBlockChain) SubscribeRemovedLogsEvent(ch chan<- RemovedLogsEvent) event.Subscription {
	return bc.scope.Track(bc.rmLogsFeed.Subscribe(ch))
}

// SubscribeChainEvent registers a subscription of ChainEvent.
func (bc *RootBlockChain) SubscribeChainEvent(ch chan<- RootChainEvent) event.Subscription {
	return bc.scope.Track(bc.chainFeed.Subscribe(ch))
}

// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
func (bc *RootBlockChain) SubscribeChainHeadEvent(ch chan<- RootChainHeadEvent) event.Subscription {
	return bc.scope.Track(bc.chainHeadFeed.Subscribe(ch))
}

// SubscribeChainSideEvent registers a subscription of ChainSideEvent.
func (bc *RootBlockChain) SubscribeChainSideEvent(ch chan<- RootChainSideEvent) event.Subscription {
	return bc.scope.Track(bc.chainSideFeed.Subscribe(ch))
}

func (bc *RootBlockChain) CreateBlockToMine(mHeaderList []*types.MinorBlockHeader, address *account.Address, createTime *uint64) *types.RootBlock {
	if address == nil {
		temp := account.CreatEmptyAddress(0)
		address = &temp
	}
	if createTime == nil {
		temp := uint64(time.Now().Unix())
		if bc.CurrentBlock().Time()+1 > temp {
			temp = bc.CurrentBlock().Time() + 1
		}
		createTime = &temp
	}
	difficulty, err := bc.engine.CalcDifficulty(bc, *createTime, bc.CurrentHeader())
	if err != nil {
		panic(errors.New("sb"))
	}
	block := bc.CurrentBlock().Header().CreateBlockToAppend(createTime, difficulty, address, nil, nil)
	block.ExtendMinorBlockHeaderList(mHeaderList)
	block.Finalize(bc.CalculateRootBlockCoinBase(block), address)
	return block
}

func (bc *RootBlockChain) CalculateRootBlockCoinBase(rootBlock *types.RootBlock) *big.Int {
	coinBaseAmount := bc.Config().Root.CoinbaseAmount
	rewardTaxRate := bc.Config().RewardTaxRate
	value := big.NewRat(1, 1)
	value.Sub(value, rewardTaxRate)
	value.Quo(value, rewardTaxRate)

	minorBlockFee := new(big.Int)
	for _, header := range rootBlock.MinorBlockHeaders() {
		minorBlockFee.Add(minorBlockFee, header.CoinbaseAmount.Value)
	}
	minorBlockFee.Mul(minorBlockFee, value.Denom())
	minorBlockFee.Div(minorBlockFee, value.Num())
	ans := new(big.Int).Add(coinBaseAmount, minorBlockFee)
	return ans
}
func (bc *RootBlockChain) IsMinorBlockValidated(hash common.Hash) bool {
	minorBlock := rawdb.ReadMinorBlock(bc.db, hash)
	return minorBlock != nil
}

func (bc *RootBlockChain) GetNextDifficulty(create *uint64) *big.Int {
	if create == nil {
		temp := uint64(time.Now().Unix())
		if temp < bc.CurrentBlock().Time()+1 {
			temp = bc.CurrentBlock().Time() + 1
		}
		create = &temp
	}
	data, err := bc.engine.CalcDifficulty(bc, *create, bc.CurrentBlock().Header())
	if err != nil {
		panic(errors.New("sb"))
	}
	return data
}

func (bc *RootBlockChain) GetBlockCount() {
	//TODO for json rpc
	//Returns a dict(full_shard_id, dict(miner_recipient, block_count))
}

func (bc *RootBlockChain) WriteCommittingHash(hash common.Hash) {
	rawdb.WriteRbCommittingHash(bc.db, hash)
}

func (bc *RootBlockChain) ClearCommittingHash() {
	rawdb.DeleteRbCommittingHash(bc.db)
}

func (bc *RootBlockChain) GetCommittingBlockHash() common.Hash {
	return rawdb.ReadRbCommittingHash(bc.db)
}
