package filtermaps

import (
	"errors"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	startLvPointer       = valuesPerMap << 31
	removedPointer       = math.MaxUint64
	revertPointFrequency = 256
	cachedRevertPoints   = 64
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

func (f *FilterMaps) tryUpdateHead(newHead *types.Header) {
	var newHeaders []*types.Header
	chainPtr := newHead
	for {
		if chainPtr.Hash() == f.headBlockHash {
			break // no need to revert
		}
		rp, stop, err := f.getRevertPoint(chainPtr.Number.Uint64())
		if err != nil {
			log.Error("Error fetching revert point", "block number", chainPtr.Number.Uint64(), "error", err)
			return
		}
		if rp != nil && rp.BlockHash == chainPtr.Hash() {
			if err := f.revertTo(rp); err != nil {
				log.Error("Error applying revert point", "block number", chainPtr.Number.Uint64(), "error", err)
				return
			}
			break
		}
		if stop {
			log.Warn("No suitable revert point exists; re-initializing log index", "block number", newHead.Number.Uint64())
			f.reset()
			f.tryInit(newHead)
			return
		}
		newHeaders = append(newHeaders, chainPtr)
		chainPtr = f.chain.GetHeader(chainPtr.ParentHash, chainPtr.Number.Uint64()-1)
		if chainPtr == nil {
			log.Error("Canonical header not found", "number", chainPtr.Number.Uint64()-1, "hash", chainPtr.ParentHash)
			return
		}
	}

	if newHeaders == nil {
		return
	}
	update := f.newUpdateBatch()
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
	revertPoints           map[uint64]*revertPoint
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
		revertPoints:    make(map[uint64]*revertPoint),
	}
}

func (f *FilterMaps) applyUpdateBatch(u *updateBatch) {
	f.lock.Lock()
	defer f.lock.Unlock()

	batch := f.db.NewBatch()
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
	if u.headBlockNumber < f.headBlockNumber {
		for b := u.headBlockNumber + 1; b <= f.headBlockNumber; b++ {
			delete(f.revertPoints, b)
			if b%revertPointFrequency == 0 {
				rawdb.DeleteRevertPoint(batch, b)
			}
		}
	}
	if u.headBlockNumber > f.headBlockNumber {
		for b := f.headBlockNumber + 1; b <= u.headBlockNumber; b++ {
			delete(f.revertPoints, b-cachedRevertPoints)
		}
	}
	for b, rp := range u.revertPoints {
		if b+cachedRevertPoints > u.headBlockNumber {
			f.revertPoints[b] = rp
		}
		if b%revertPointFrequency == 0 {
			enc, err := rlp.EncodeToBytes(rp)
			if err != nil {
				log.Crit("Failed to encode revert point", "err", err)
			}
			rawdb.WriteRevertPoint(batch, b, enc)
		}
	}
	f.setRange(batch, u.filterMapsRange)
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
	if (u.headBlockNumber-cachedRevertPoints)%revertPointFrequency != 0 {
		delete(u.revertPoints, u.headBlockNumber-cachedRevertPoints)
	}
	if rp, err := u.makeRevertPoint(); err != nil {
		return err
	} else if rp != nil {
		u.revertPoints[u.headBlockNumber] = rp
	}
	return nil
}

/*func (u *updateBatch) removeValueFromHead(logValue common.Hash) error {
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
}*/

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

type revertPoint struct {
	blockNumber uint64
	BlockHash   common.Hash
	MapIndex    uint32
	RowLength   [mapHeight]uint
}

func (u *updateBatch) makeRevertPoint() (*revertPoint, error) {
	rp := &revertPoint{
		blockNumber: u.headBlockNumber,
		BlockHash:   u.headBlockHash,
		MapIndex:    uint32(u.headLvPointer >> logValuesPerMap),
	}
	if u.tailLvPointer > uint64(rp.MapIndex)<<logValuesPerMap {
		return nil, nil
	}
	for i := range rp.RowLength[:] {
		var row FilterRow
		if m := u.maps[rp.MapIndex]; m != nil {
			row = (*m)[i]
		}
		if row == nil {
			var err error
			row, err = u.getFilterMapRow(rp.MapIndex, uint32(i))
			if err != nil {
				return nil, err
			}
		}
		rp.RowLength[i] = uint(len(row))
	}
	return rp, nil
}

func (f *FilterMaps) getRevertPoint(blockNumber uint64) (*revertPoint, bool, error) {
	if blockNumber > f.headBlockNumber {
		return nil, false, nil
	}
	if rp := f.revertPoints[blockNumber]; rp != nil {
		return rp, false, nil
	}
	if blockNumber%revertPointFrequency != 0 {
		return nil, false, nil
	}
	enc, err := rawdb.ReadRevertPoint(f.db, blockNumber)
	if err != nil {
		return nil, false, err
	}
	if enc == nil {
		return nil, true, nil
	}
	rp := &revertPoint{blockNumber: blockNumber}
	return rp, false, rlp.DecodeBytes(enc, rp)
}

func (f *FilterMaps) revertTo(rp *revertPoint) error {
	batch := f.db.NewBatch()
	afterLastMap := uint32((f.headLvPointer + valuesPerMap - 1) >> logValuesPerMap)
	if rp.MapIndex >= afterLastMap {
		return errors.New("cannot revert (head map behind revert point)")
	}
	lvPointer := uint64(rp.MapIndex) << logValuesPerMap
	for rowIndex, rowLen := range rp.RowLength[:] {
		rowIndex := uint32(rowIndex)
		row, err := f.getFilterMapRow(rp.MapIndex, rowIndex)
		if err != nil {
			return err
		}
		if uint(len(row)) < rowLen {
			return errors.New("cannot revert (row too short)")
		}
		if uint(len(row)) > rowLen {
			f.storeFilterMapRow(batch, rp.MapIndex, rowIndex, row[:rowLen])
		}
		for mapIndex := rp.MapIndex + 1; mapIndex < afterLastMap; mapIndex++ {
			f.storeFilterMapRow(batch, mapIndex, rowIndex, nil)
		}
		lvPointer += uint64(rowLen)
	}
	for mapIndex := rp.MapIndex + 1; mapIndex < afterLastMap; mapIndex++ {
		f.deleteMapBlockPtr(batch, mapIndex)
	}
	for blockNumber := rp.blockNumber + 1; blockNumber <= f.headBlockNumber; blockNumber++ {
		f.deleteBlockLvPointer(batch, blockNumber)
	}
	newRange := f.filterMapsRange
	newRange.headLvPointer = lvPointer
	newRange.headBlockNumber = rp.blockNumber
	newRange.headBlockHash = rp.BlockHash
	f.setRange(batch, newRange)
	if err := batch.Write(); err != nil {
		log.Crit("Could not write update batch", "error", err)
	}
	return nil
}
