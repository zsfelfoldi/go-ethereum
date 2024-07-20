package filtermaps

import (
	"errors"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	startLvPointer = valuesPerMap << 31
	removedPointer = math.MaxUint64
)

func (f *FilterMaps) updateLoop() {
	headEventCh := make(chan core.ChainHeadEvent)
	sub := f.chain.SubscribeChainHeadEvent(headEventCh)
	defer sub.Unsubscribe()
	head := f.chain.CurrentHeader()
	if head == nil {
		select {
		case ev := <-headEventCh:
			head = ev.Block.Header()
		case ch := <-f.closeCh:
			close(ch)
			return
		}
	}

	for {
		if !f.getRange().initialized {
			f.tryInit(head)
		}
		if fmr := f.getRange(); fmr.initialized && fmr.headBlockHash != head.Hash() {
			f.tryUpdateHead(head)
		}
		if fmr := f.getRange(); fmr.initialized && fmr.headBlockHash == head.Hash() {
			var closed bool
			f.tryExtendTail(func() bool {
				select {
				case ev := <-headEventCh:
					head = ev.Block.Header()
					return true
				case ch := <-f.closeCh:
					close(ch)
					closed = true
					return true
				default:
					return false
				}
			})
			if closed {
				return
			}
		}
		select {
		case ev := <-headEventCh:
			head = ev.Block.Header()
		case ch := <-f.closeCh:
			close(ch)
			return
		}
	}
}

func (f *FilterMaps) getRange() filterMapsRange {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.filterMapsRange
}

func (f *FilterMaps) tryInit(head *types.Header) {
	receipts := f.chain.GetReceiptsByHash(head.Hash())
	if receipts == nil {
		log.Error("Could not retrieve block receipts for init block", "number", head.Number, "hash", head.Hash())
		return
	}
	update := f.newUpdateBatch()
	if err := update.initWithBlock(head, receipts); err != nil {
		log.Error("Could not initialize filter maps", "error", err)
	}
	f.applyUpdateBatch(update)
}

func (f *FilterMaps) tryUpdateHead(head *types.Header) {
	fmRange := f.getRange()
	fmPtr := f.chain.GetHeader(fmRange.headBlockHash, fmRange.headBlockNumber)
	if fmPtr == nil {
		log.Error("Head header not found", "number", fmRange.headBlockNumber, "hash", fmRange.headBlockHash)
		return
	}
	chainPtr := head
	update := f.newUpdateBatch()

	var newHeaders []*types.Header
	for fmPtr.Hash() != chainPtr.Hash() {
		if fmPtr.Number.Uint64() >= chainPtr.Number.Uint64() {
			receipts := f.chain.GetReceiptsByHash(fmPtr.Hash())
			if receipts == nil {
				log.Error("Could not retrieve block receipts for reverted block", "number", fmPtr.Number, "hash", fmPtr.Hash())
				newHeaders = nil
				break
			}
			if err := update.removeBlockFromHead(fmPtr, receipts); err == nil {
				if update.updatedRangeLength() >= mapsPerEpoch {
					f.applyUpdateBatch(update)
					update = f.newUpdateBatch()
				}
				fmPtr = f.chain.GetHeader(fmPtr.ParentHash, fmPtr.Number.Uint64()-1)
				if fmPtr == nil {
					log.Error("Header of old path not found", "number", fmPtr.Number.Uint64()-1, "hash", fmPtr.ParentHash)
					return
				}
			} else {
				log.Error("Error reverting block", "number", fmPtr.Number, "hash", fmPtr.Hash(), "error", err)
				newHeaders = nil
				break
			}
		}
		if fmPtr.Number.Uint64() < chainPtr.Number.Uint64() {
			newHeaders = append(newHeaders, chainPtr)
			chainPtr = f.chain.GetHeader(chainPtr.ParentHash, chainPtr.Number.Uint64()-1)
			if chainPtr == nil {
				log.Error("Header of new path not found", "number", chainPtr.Number.Uint64()-1, "hash", chainPtr.ParentHash)
				return
			}
		}
	}

	if newHeaders != nil {
		for i := len(newHeaders) - 1; i >= 0; i-- {
			newHeader := newHeaders[i]
			receipts := f.chain.GetReceiptsByHash(newHeader.Hash())
			if receipts == nil {
				log.Error("Could not retrieve block receipts for new block", "number", newHeader.Number, "hash", newHeader.Hash())
				break
			}
			if err := update.addBlockToHead(newHeader, receipts); err != nil {
				log.Error("Error adding new block", "number", newHeader.Number, "hash", newHeader.Hash(), "error", err)
				break
			}
			if update.updatedRangeLength() >= mapsPerEpoch {
				f.applyUpdateBatch(update)
				update = f.newUpdateBatch()
			}
		}
	}
	f.applyUpdateBatch(update)
}

func (f *FilterMaps) tryExtendTail(stopFn func() bool) {
	fmr := f.getRange()
	number, parentHash := fmr.tailBlockNumber, fmr.tailParentHash
	if number == 0 {
		return
	}
	update := f.newUpdateBatch()
	lastTailEpoch := update.tailEpoch()
	for number > 0 && !stopFn() {
		if tailEpoch := update.tailEpoch(); tailEpoch < lastTailEpoch {
			f.applyUpdateBatch(update)
			update = f.newUpdateBatch()
			lastTailEpoch = tailEpoch
		}
		newTail := f.chain.GetHeader(parentHash, number-1)
		if newTail == nil {
			log.Error("Tail header not found", "number", number-1, "hash", parentHash)
			break
		}
		receipts := f.chain.GetReceiptsByHash(newTail.Hash())
		if receipts == nil {
			log.Error("Could not retrieve block receipts for tail block", "number", newTail.Number, "hash", newTail.Hash())
			//TODO no error needed?
			break
		}
		if err := update.addBlockToTail(newTail, receipts); err != nil {
			log.Error("Error adding tail block", "number", newTail.Number, "hash", newTail.Hash(), "error", err)
			break
		}
		number, parentHash = newTail.Number.Uint64(), newTail.ParentHash
	}
	f.applyUpdateBatch(update)
}

type updateBatch struct {
	filterMapsRange
	maps                   map[uint32]*filterMap
	getFilterMapRow        func(mapIndex, rowIndex uint32) (FilterRow, error)
	blockLvPointer         map[uint64]uint64
	mapBlockPtr            map[uint32]uint64
	firstMap, afterLastMap uint32
}

func (f *FilterMaps) newUpdateBatch() *updateBatch {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return &updateBatch{
		filterMapsRange: f.filterMapsRange,
		maps:            make(map[uint32]*filterMap),
		getFilterMapRow: f.getFilterMapRow,
		blockLvPointer:  make(map[uint64]uint64),
		mapBlockPtr:     make(map[uint32]uint64),
	}
}

func (f *FilterMaps) applyUpdateBatch(u *updateBatch) {
	f.lock.Lock()
	defer f.lock.Unlock()

	batch := f.db.NewBatch()
	f.setRange(batch, u.filterMapsRange)
	for blockNumber, lvPointer := range u.blockLvPointer {
		if lvPointer != removedPointer {
			f.storeBlockLvPointer(batch, blockNumber, lvPointer)
		} else {
			f.deleteBlockLvPointer(batch, blockNumber)
		}
	}
	for mapIndex, blockNumber := range u.mapBlockPtr {
		if blockNumber != removedPointer {
			f.storeMapBlockPtr(batch, mapIndex, blockNumber)
		} else {
			f.deleteMapBlockPtr(batch, mapIndex)
		}
	}
	for rowIndex := uint32(0); rowIndex < mapHeight; rowIndex++ {
		for mapIndex := u.firstMap; mapIndex < u.afterLastMap; mapIndex++ {
			if fm := u.maps[mapIndex]; fm != nil {
				if row := (*fm)[rowIndex]; row != nil {
					f.storeFilterMapRow(batch, mapIndex, rowIndex, row)
				}
			}
		}
	}
	if err := batch.Write(); err != nil {
		log.Crit("Could not write update batch", "error", err)
	}
	log.Info("Filter maps block range updated", "tail", u.tailBlockNumber, "head", u.headBlockNumber)
}

func (u *updateBatch) updatedRangeLength() uint32 {
	return u.afterLastMap - u.firstMap
}

func (u *updateBatch) tailEpoch() uint32 {
	return uint32(u.tailLvPointer >> (logValuesPerMap + logMapsPerEpoch))
}

func (u *updateBatch) getRowPtr(mapIndex, rowIndex uint32) (*FilterRow, error) {
	fm := u.maps[mapIndex]
	if fm == nil {
		fm = new(filterMap)
		u.maps[mapIndex] = fm
		if mapIndex < u.firstMap || u.afterLastMap == 0 {
			u.firstMap = mapIndex
		}
		if mapIndex >= u.afterLastMap {
			u.afterLastMap = mapIndex + 1
		}
	}
	rowPtr := &(*fm)[rowIndex]
	if *rowPtr == nil {
		if filterRow, err := u.getFilterMapRow(mapIndex, rowIndex); err == nil {
			*rowPtr = filterRow
		} else {
			return nil, err
		}
	}
	return rowPtr, nil
}

func (u *updateBatch) initWithBlock(header *types.Header, receipts types.Receipts) error {
	if u.initialized {
		return errors.New("already initialized")
	}
	u.initialized = true
	u.headLvPointer, u.tailLvPointer = startLvPointer, startLvPointer
	u.headBlockNumber, u.tailBlockNumber = header.Number.Uint64()-1, header.Number.Uint64() //TODO genesis?
	u.headBlockHash, u.tailParentHash = header.ParentHash, header.ParentHash
	u.addBlockToHead(header, receipts)
	return nil
}

func (u *updateBatch) addValueToHead(logValue common.Hash) error {
	mapIndex := uint32(u.headLvPointer >> logValuesPerMap)
	rowPtr, err := u.getRowPtr(mapIndex, rowIndex(mapIndex>>logMapsPerEpoch, logValue))
	if err != nil {
		return err
	}
	column := columnIndex(u.headLvPointer, logValue)
	*rowPtr = append(*rowPtr, column)
	u.headLvPointer++
	return nil
}

func (u *updateBatch) addBlockToHead(header *types.Header, receipts types.Receipts) error {
	if !u.initialized {
		return errors.New("not initialized")
	}
	if header.ParentHash != u.headBlockHash {
		return errors.New("addBlockToHead parent mismatch")
	}
	number := header.Number.Uint64()
	u.blockLvPointer[number] = u.headLvPointer
	startMap := uint32((u.headLvPointer + valuesPerMap - 1) >> logValuesPerMap)
	if err := iterateReceipts(receipts, u.addValueToHead); err != nil {
		return err
	}
	stopMap := uint32((u.headLvPointer + valuesPerMap - 1) >> logValuesPerMap)
	for m := startMap; m < stopMap; m++ {
		u.mapBlockPtr[m] = number
	}
	u.headBlockNumber, u.headBlockHash = number, header.Hash()
	return nil
}

func (u *updateBatch) removeValueFromHead(logValue common.Hash) error {
	u.headLvPointer--
	mapIndex := uint32(u.headLvPointer >> logValuesPerMap)
	rowPtr, err := u.getRowPtr(mapIndex, rowIndex(mapIndex>>logMapsPerEpoch, logValue))
	if err != nil {
		return err
	}
	column := columnIndex(u.headLvPointer, logValue)
	l := len(*rowPtr) - 1
	if l < 0 || (*rowPtr)[l] != column {
		return errors.New("removed value not found at end of row")
	}
	*rowPtr = (*rowPtr)[:l]
	return nil
}

func (u *updateBatch) removeBlockFromHead(header *types.Header, receipts types.Receipts) error {
	if !u.initialized {
		return errors.New("not initialized")
	}
	if header.Hash() != u.headBlockHash {
		return errors.New("removeBlockFromHead head mismatch")
	}
	number := header.Number.Uint64()
	u.blockLvPointer[number] = removedPointer
	stopMap := uint32(u.headLvPointer >> logValuesPerMap)
	if err := iterateReceiptsReverse(receipts, u.removeValueFromHead); err != nil {
		return err
	}
	startMap := uint32(u.headLvPointer >> logValuesPerMap)
	for m := startMap; m < stopMap; m++ {
		u.mapBlockPtr[m] = removedPointer
	}
	u.headBlockNumber, u.headBlockHash = number-1, header.ParentHash
	return nil
}

func (u *updateBatch) addValueToTail(logValue common.Hash) error {
	if u.tailLvPointer == 0 {
		return errors.New("tail log value pointer underflow")
	}
	u.tailLvPointer--
	mapIndex := uint32(u.tailLvPointer >> logValuesPerMap)
	rowPtr, err := u.getRowPtr(mapIndex, rowIndex(mapIndex>>logMapsPerEpoch, logValue))
	if err != nil {
		return err
	}
	column := columnIndex(u.tailLvPointer, logValue)
	*rowPtr = append(*rowPtr, 0)
	copy((*rowPtr)[1:], (*rowPtr)[:len(*rowPtr)-1])
	(*rowPtr)[0] = column
	return nil
}

func (u *updateBatch) addBlockToTail(header *types.Header, receipts types.Receipts) error {
	if !u.initialized {
		return errors.New("not initialized")
	}
	if header.Hash() != u.tailParentHash {
		return errors.New("addBlockToTail parent mismatch")
	}
	number := header.Number.Uint64()
	stopMap := uint32((u.tailLvPointer + valuesPerMap - 1) >> logValuesPerMap)
	var cnt int
	if err := iterateReceiptsReverse(receipts, func(lv common.Hash) error {
		cnt++
		return u.addValueToTail(lv)
	}); err != nil {
		return err
	}
	startMap := uint32(u.tailLvPointer >> logValuesPerMap)
	for m := startMap; m < stopMap; m++ {
		u.mapBlockPtr[m] = number
	}
	u.blockLvPointer[number] = u.tailLvPointer
	u.tailBlockNumber, u.tailParentHash = number, header.ParentHash
	return nil
}

func iterateReceipts(receipts types.Receipts, valueCb func(common.Hash) error) error {
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if err := valueCb(addressValue(log.Address)); err != nil {
				return err
			}
			for _, topic := range log.Topics {
				if err := valueCb(topicValue(topic)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func iterateReceiptsReverse(receipts types.Receipts, valueCb func(common.Hash) error) error {
	for i := len(receipts) - 1; i >= 0; i-- {
		logs := receipts[i].Logs
		for j := len(logs) - 1; j >= 0; j-- {
			log := logs[j]
			for k := len(log.Topics) - 1; k >= 0; k-- {
				if err := valueCb(topicValue(log.Topics[k])); err != nil {
					return err
				}
			}
			if err := valueCb(addressValue(log.Address)); err != nil {
				return err
			}
		}
	}
	return nil
}
