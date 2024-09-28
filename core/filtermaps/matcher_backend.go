package filtermaps

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/core/types"
)

// FilterMapsMatcherBackend implements MatcherBackend.
type FilterMapsMatcherBackend struct {
	f                     *FilterMaps
	valid                 bool
	firstValid, lastValid uint64
	syncCh                chan SyncRange
}

// NewMatcherBackend returns a FilterMapsMatcherBackend after registering it in
// the active matcher set.
// Note that Close should always be called when the matcher is no longer used.
func (f *FilterMaps) NewMatcherBackend() *FilterMapsMatcherBackend {
	f.lock.Lock()
	defer f.lock.Unlock()

	fm := &FilterMapsMatcherBackend{
		f:          f,
		valid:      f.initialized,
		firstValid: f.tailBlockNumber,
		lastValid:  f.headBlockNumber,
	}
	f.matchers[fm] = struct{}{}
	return fm
}

// GetParams returns the filtermaps parameters.
// GetParams implements MatcherBackend.
func (fm *FilterMapsMatcherBackend) GetParams() *Params {
	return &fm.f.Params
}

// Close removes the matcher from the set of active matchers and ensures that
// any SyncLogIndex calls are cancelled.
// Close implements MatcherBackend.
func (fm *FilterMapsMatcherBackend) Close() {
	fm.f.lock.Lock()
	defer fm.f.lock.Unlock()

	delete(fm.f.matchers, fm)
}

// GetFilterMapRow returns the given row of the given map. If the row is empty
// then a non-nil zero length row is returned.
// Note that the returned slices should not be modified, they should be copied
// on write.
// GetFilterMapRow implements MatcherBackend.
func (fm *FilterMapsMatcherBackend) GetFilterMapRow(ctx context.Context, mapIndex, rowIndex uint32) (FilterRow, error) {
	return fm.f.getFilterMapRow(mapIndex, rowIndex)
}

// GetBlockLvPointer returns the starting log value index where the log values
// generated by the given block are located. If blockNumber is beyond the current
// head then the first unoccupied log value index is returned.
// GetBlockLvPointer implements MatcherBackend.
func (fm *FilterMapsMatcherBackend) GetBlockLvPointer(ctx context.Context, blockNumber uint64) (uint64, error) {
	fm.f.lock.RLock()
	defer fm.f.lock.RUnlock()

	return fm.f.getBlockLvPointer(blockNumber)
}

// GetLogByLvIndex returns the log at the given log value index.
// Note that this function assumes that the log index structure is consistent
// with the canonical chain at the point where the given log value index points.
// If this is not the case then an invalid result may be returned or certain
// logs might not be returned at all.
// No error is returned though because of an inconsistency between the chain and
// the log index. It is the caller's responsibility to verify this consistency
// using SyncLogIndex and re-process certain blocks if necessary.
// GetLogByLvIndex implements MatcherBackend.
func (fm *FilterMapsMatcherBackend) GetLogByLvIndex(ctx context.Context, lvIndex uint64) (*types.Log, error) {
	fm.f.lock.RLock()
	defer fm.f.lock.RUnlock()

	return fm.f.getLogByLvIndex(lvIndex)
}

// synced signals to the matcher that has triggered a synchronisation that it
// has been finished and the log index is consistent with the chain head passed
// as a parameter.
// Note that if the log index head was far behind the chain head then it might not
// be synced up to the given head in a single step. Still, the latest chain head
// should be passed as a parameter and the existing log index should be consistent
// with that chain.
func (fm *FilterMapsMatcherBackend) synced(head *types.Header) {
	fm.f.lock.Lock()
	defer fm.f.lock.Unlock()

	fm.syncCh <- SyncRange{
		Head:         head,
		Valid:        fm.valid,
		FirstValid:   fm.firstValid,
		LastValid:    fm.lastValid,
		Indexed:      fm.f.initialized,
		FirstIndexed: fm.f.tailBlockNumber,
		LastIndexed:  fm.f.headBlockNumber,
	}
	fm.valid = fm.f.initialized
	fm.firstValid = fm.f.tailBlockNumber
	fm.lastValid = fm.f.headBlockNumber
	fm.syncCh = nil
}

// SyncLogIndex ensures that the log index is consistent with the current state
// of the chain (note that it may or may not be actually synced up to the head).
// It blocks until this state is achieved.
// If successful, it returns a SyncRange that contains the latest chain head,
// the indexed range that is currently consistent with the chain and the valid
// range that has not been changed and has been consistent with all states of the
// chain since the previous SyncLogIndex or the creation of the matcher backend.
func (fm *FilterMapsMatcherBackend) SyncLogIndex(ctx context.Context) (SyncRange, error) {
	// add SyncRange return channel, ensuring that
	syncCh := make(chan SyncRange, 1)
	fm.f.lock.Lock()
	fm.syncCh = syncCh
	fm.f.lock.Unlock()

	select {
	case fm.f.matcherSyncCh <- fm:
	case <-ctx.Done():
		return SyncRange{}, ctx.Err()
	}
	select {
	case vr := <-syncCh:
		if vr.Head == nil {
			return SyncRange{}, errors.New("canonical chain head not available")
		}
		return vr, nil
	case <-ctx.Done():
		return SyncRange{}, ctx.Err()
	}
}

// updateMatchersValidRange iterates through active matchers and limits their
// valid range with the current indexed range. This function should be called
// whenever a part of the log index has been removed, before adding new blocks
// to it.
func (f *FilterMaps) updateMatchersValidRange() {
	for fm := range f.matchers {
		if !f.initialized {
			fm.valid = false
		}
		if !fm.valid {
			continue
		}
		if fm.firstValid < f.tailBlockNumber {
			fm.firstValid = f.tailBlockNumber
		}
		if fm.lastValid > f.headBlockNumber {
			fm.lastValid = f.headBlockNumber
		}
		if fm.firstValid > fm.lastValid {
			fm.valid = false
		}
	}
}
