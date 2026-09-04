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

package logindex

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	ErrTableDeleted = errors.New("table already deleted")
	errFileNotFound = errors.New("file not found")
	errFileLocked   = errors.New("file is locked for writing")
)

const (
	tempFileName = "rename_temp"
	memFileName  = "small_tables"
)

// tableFiles is a layer between the file system and the index table reader/writer
// that provides file io interface for each individual table or partially rendered
// table layer/render state, identified by a file name string. Large tables are
// split up into multiple limited size files while the smallest and shortest
// lived tables are kept in memory and only committed to disk at shutdown in a
// single file. Data integrity can be guaranteed by writing rendered tables under
// a temporary file name and then renaming them to their final name once finished.
type tableFiles struct {
	lock          sync.Mutex
	path          string
	maxFileSize   int64
	maxOpenFiles  int
	accessCounter uint64
	osFiles       map[osFileID]*osFileInfo // finished files only
	tableFiles    map[string]*tableFileInfo
	memFileTotal  int64
}

type osFileID struct {
	tfInfo    *tableFileInfo
	fileIndex int
}

type osFileInfo struct {
	file          *os.File
	writer        *bufio.Writer // only in write mode
	accessCounter uint64
}

type tableFileInfo struct {
	tf                      *tableFiles
	name                    string
	fileCount, maxFileIndex int
	size                    int64
	memData                 []byte
	chunkSize               int64  // size of currently written os file chunk
	locked, deleted         uint32 // atomic
}

func newTableFiles(path string, maxFileSize int64, maxOpenFiles int) (*tableFiles, error) {
	tf := &tableFiles{
		path:         path,
		maxFileSize:  maxFileSize,
		maxOpenFiles: maxOpenFiles,
		osFiles:      make(map[osFileID]*osFileInfo),
		tableFiles:   make(map[string]*tableFileInfo),
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		err = os.Mkdir(path, 0755) //TODO file mode?
	}
	if err != nil {
		return nil, err
	}
	fmt.Println("+++ newTableFiles")
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		fmt.Println(" os file", name)
		if name == tempFileName {
			os.Remove(filepath.Join(path, name))
			continue
		}
		if name == memFileName {
			tf.loadMemTables()
			os.Remove(filepath.Join(path, name))
			continue
		}
		var fileIndex int
		n, err := fmt.Sscanf(name[max(len(name), 5)-5:], ".%04x", &fileIndex)
		if n != 1 || err != nil {
			log.Warn("Unexpected file name in index table directory", "name", name)
		}
		name = name[:max(len(name), 5)-5]
		fi, ok := tf.tableFiles[name]
		if !ok {
			fmt.Println(" new fi", name)
			fi = &tableFileInfo{
				tf:   tf,
				name: name,
			}
			tf.tableFiles[name] = fi
		}
		fi.fileCount++
		fi.maxFileIndex = max(fi.maxFileIndex, fileIndex)
		if fileInfo, err := entry.Info(); err == nil {
			fi.size += fileInfo.Size()
			if fileIndex == fi.maxFileIndex {
				fi.chunkSize = fileInfo.Size()
			}
		} else {
			return nil, err
		}
	}
	for name, fi := range tf.tableFiles {
		if fi.fileCount == 0 {
			continue
		}
		if fi.fileCount != fi.maxFileIndex+1 || fi.size != maxFileSize*int64(fi.fileCount-1)+fi.chunkSize {
			fmt.Println(" remove fi", name)
			log.Warn("Removing incomplete table file", "name", name)
			delete(tf.tableFiles, name)
			for i := range fi.maxFileIndex + 1 {
				os.Remove(tf.osFileName(name, i))
			}
		}
	}
	return tf, nil
}

func (tf *tableFiles) allFiles() []string {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	res := make([]string, 0, len(tf.tableFiles))
	for name := range tf.tableFiles {
		res = append(res, name)
	}
	return res
}

func (tf *tableFiles) getMemFileTotal() uint64 {
	return uint64(atomic.LoadInt64(&tf.memFileTotal))
}

func (tf *tableFiles) close() {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	for name, fi := range tf.tableFiles {
		if fi.isLocked() {
			log.Error("Table file is still locked while shutting down", "name", name)
			if err := fi.Close(); err != nil {
				log.Error("Failed to close locked table file", "name", name)
				continue
			}
			if err := tf.deleteFile(name); err != nil {
				log.Error("Failed to delete incomplete table file", "name", name)
			}
		}
	}
	tf.storeMemTables()
	//TODO no more ops
}

type storedMemTable struct {
	Name string
	Data []byte
}

func (tf *tableFiles) loadMemTables() {
	var memTables []storedMemTable
	fn := filepath.Join(tf.path, memFileName)
	f, err := os.Open(fn)
	if err != nil {
		log.Error("Could not read small index table file", "error", err)
		return
	}
	err = rlp.Decode(f, &memTables)
	if err != nil {
		if err2 := f.Close(); err != nil {
			log.Error("Could not decode small index table file", "error", err2)
			return
		}
		log.Error("Could not decode small index table file", "error", err)
		return
	}
	for _, mt := range memTables {
		tf.tableFiles[mt.Name] = &tableFileInfo{
			tf:      tf,
			name:    mt.Name,
			memData: mt.Data,
			size:    int64(len(mt.Data)),
		}
		tf.memFileTotal += int64(len(mt.Data))
	}
}

func (tf *tableFiles) storeMemTables() {
	fmt.Println("storeMemTables")
	var memTables []storedMemTable
	for name, fi := range tf.tableFiles {
		if fi.fileCount != 0 {
			continue
		}
		memTables = append(memTables, storedMemTable{
			Name: name,
			Data: fi.memData,
		})
	}
	f, err := os.OpenFile(filepath.Join(tf.path, memFileName), os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		err = rlp.Encode(f, memTables)
		err2 := f.Close()
		if err == nil {
			err = err2
		}
	}
	fmt.Println(" error", err)
	if err != nil {
		log.Error("Could not save small index table files", "error", err)
	}
}

func (tf *tableFiles) getReaderAt(name string) (io.ReaderAt, int64, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	//fmt.Println("getReaderAt", name)
	fi, ok := tf.tableFiles[name]
	if !ok {
		//fmt.Println(" does not exist")
		return nil, 0, errors.New("table file does not exist")
	}
	if fi.isLocked() {
		///fmt.Println(" locked")
		return nil, 0, errors.New("table file is currently locked")
	}
	//fmt.Println(" success; size", fi.size)
	return fi, fi.size, nil
}

func (fi *tableFileInfo) isDeleted() bool {
	return atomic.LoadUint32(&fi.deleted) != 0
}

func (fi *tableFileInfo) isLocked() bool {
	return atomic.LoadUint32(&fi.locked) != 0
}

func (tf *tableFiles) getOsFileInfo(fi *tableFileInfo, fileIndex int, write bool) (*osFileInfo, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	if fi.isDeleted() {
		return nil, ErrTableDeleted
	}
	tf.accessCounter++
	id := osFileID{tfInfo: fi, fileIndex: fileIndex}
	if of, ok := tf.osFiles[id]; ok {
		of.accessCounter = tf.accessCounter
		return of, nil
	}
	for len(tf.osFiles) >= tf.maxOpenFiles {
		// close least recently accessed os file
		var closeId osFileID
		lowest := uint64(math.MaxUint64)
		for id, of := range tf.osFiles {
			if of.accessCounter < lowest {
				lowest = of.accessCounter
				closeId = id
			}
		}
		if err := tf.closeOsFileIdLocked(closeId); err != nil {
			log.Error("Could not close index table file", "name", tf.osFileName(closeId.tfInfo.name, closeId.fileIndex), "error", err)
		}
	}
	var (
		of  = &osFileInfo{accessCounter: tf.accessCounter}
		err error
	)
	if write {
		of.file, err = os.OpenFile(tf.osFileName(id.tfInfo.name, id.fileIndex), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		of.file, err = os.Open(tf.osFileName(id.tfInfo.name, id.fileIndex))
	}
	if err != nil {
		return nil, err
	}
	if write {
		of.writer = bufio.NewWriter(of.file)
	}
	/*stats, _ := file.Stat()
	fmt.Println("opened os file", id.tfInfo.name, "size", stats.Size())*/
	tf.osFiles[id] = of
	return of, nil
}

func (tf *tableFiles) closeOsFile(fi *tableFileInfo, fileIndex int) error {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	return tf.closeOsFileIdLocked(osFileID{tfInfo: fi, fileIndex: fileIndex})
}

func (tf *tableFiles) closeOsFileIdLocked(id osFileID) error {
	of := tf.osFiles[id]
	if of == nil {
		return nil
	}
	if of.writer != nil {
		if err := of.writer.Flush(); err != nil {
			return err
		}
	}
	if err := of.file.Close(); err != nil {
		return err
	}
	delete(tf.osFiles, id)
	return nil
}

func (fi *tableFileInfo) ReadAt(p []byte, offset int64) (n int, err error) {
	//fmt.Println("ReadAt", fi.name, fi.size, offset, len(p))
	defer func() {
		//fmt.Println(" ", n, err)
		if err != nil && fi.isDeleted() {
			err = ErrTableDeleted
		}
	}()

	if fi.fileCount == 0 {
		// table file stored in memory
		if offset >= int64(len(fi.memData)) {
			return 0, errors.New("invalid read offset")
		}
		maxLen := int64(len(fi.memData)) - offset
		if int64(len(p)) <= maxLen {
			copy(p, fi.memData[offset:offset+int64(len(p))])
			return len(p), nil
		}
		copy(p[:maxLen], fi.memData[offset:offset+maxLen])
		return int(maxLen), errors.New("end of file reached")
	}
	// table file stored on disk
	fileIndex := int(offset / fi.tf.maxFileSize)
	if fileIndex >= fi.fileCount {
		return 0, errors.New("invalid read offset")
	}
	of, err := fi.tf.getOsFileInfo(fi, fileIndex, false)
	if err != nil {
		return 0, err
	}
	filePos := offset % fi.tf.maxFileSize
	maxLen := fi.tf.maxFileSize - filePos
	if int64(len(p)) <= maxLen {
		return of.file.ReadAt(p, filePos)
	}
	n, err = of.file.ReadAt(p[:maxLen], filePos)
	if err != nil {
		return n, err
	}
	n, err = fi.ReadAt(p[maxLen:], offset+maxLen)
	return int(maxLen) + n, err
}

func (tf *tableFiles) getAppendWriter(name string, memory bool) (io.WriteCloser, error) {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	//fmt.Println("getAppendWriter", name, memory)
	fi, ok := tf.tableFiles[name]
	//fmt.Println(" append", ok)
	if ok {
		//fmt.Println(" size", fi.size)
	}
	if !ok {
		fi = &tableFileInfo{
			tf:   tf,
			name: name,
		}
		if !memory {
			fi.fileCount = 1
		}
		tf.tableFiles[name] = fi
	}
	if fi.isLocked() {
		//fmt.Println(" already locked")
		return nil, errors.New("table file opened for writing is currently locked")
	}
	atomic.StoreUint32(&fi.locked, 1)
	return fi, nil
}

func (fi *tableFileInfo) Write(p []byte) (n int, err error) {
	/*fmt.Println("Write", fi.name, fi.size, len(p))
	defer func() {
		fmt.Println(" ", n, err)
	}()*/

	if fi.fileCount == 0 {
		// table file stored in memory
		fi.memData = append(fi.memData, p...)
		n = len(p)
		fi.size += int64(n)
		atomic.AddInt64(&fi.tf.memFileTotal, int64(n))
		return
	}
	// table file stored on disk
	of, err := fi.tf.getOsFileInfo(fi, fi.fileCount-1, true)
	if err != nil {
		return 0, err
	}
	maxLen := fi.tf.maxFileSize - fi.chunkSize
	if int64(len(p)) <= maxLen {
		n, err = of.writer.Write(p)
		fi.size += int64(n)
		fi.chunkSize += int64(n)
		return
	}
	n, err = of.writer.Write(p[:maxLen])
	fi.size += int64(n)
	if err != nil {
		return n, err
	}
	if err := fi.tf.closeOsFile(fi, fi.fileCount-1); err != nil {
		return n, err
	}
	fi.fileCount++
	fi.chunkSize = 0
	n, err = fi.Write(p[maxLen:])
	return int(maxLen) + n, err
}

func (fi *tableFileInfo) Close() error {
	//fmt.Println("Close", fi.name, "fileCount", fi.fileCount)
	if !fi.isLocked() {
		//fmt.Println(" not open")
		return errors.New("table file was not open for writing")
	}
	if fi.fileCount != 0 {
		if err := fi.tf.closeOsFile(fi, fi.fileCount-1); err != nil {
			return err
		}
	}
	atomic.StoreUint32(&fi.locked, 0)
	//fmt.Println(" success")
	return nil
}

func (tf *tableFiles) renameFile(oldName, newName string) error {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	//fmt.Println("renameFile", oldName, newName)
	if _, ok := tf.tableFiles[newName]; ok {
		fmt.Println(" target name exists")
		return errors.New("cannot rename table file to already existing name")
	}
	fi, ok := tf.tableFiles[oldName]
	if !ok {
		//fmt.Println(" not found")
		return errFileNotFound
	}
	if fi.isLocked() {
		//fmt.Println(" locked")
		return errFileLocked
	}
	delete(tf.tableFiles, oldName)
	switch {
	case fi.fileCount == 1:
		if err := os.Rename(tf.osFileName(oldName, 0), tf.osFileName(newName, 0)); err != nil {
			//fmt.Println(" os.Rename 0 error", err)
			return err
		}
	case fi.fileCount > 1:
		// rename file index 0 to a temporary name first to ensure that file index
		// range is not continuous under either name until the rename fully succeeds.
		if err := os.Rename(tf.osFileName(oldName, 0), filepath.Join(tf.path, tempFileName)); err != nil {
			//fmt.Println(" os.Rename 1 error", err)
			return err
		}
		for i := 1; i < fi.fileCount; i++ {
			if err := os.Rename(tf.osFileName(oldName, i), tf.osFileName(newName, i)); err != nil {
				//fmt.Println(" os.Rename 2 error", i, fi.fileCount, err)
				return err
			}
		}
		if err := os.Rename(filepath.Join(tf.path, tempFileName), tf.osFileName(newName, 0)); err != nil {
			//fmt.Println(" os.Rename 3 error", err)
			return err
		}
	}
	fi.name = newName
	tf.tableFiles[newName] = fi
	//fmt.Println(" success")
	return nil
}

// deleteFile deletes all os file chunks of the table file with the given name
// and removes it from the table registry.
func (tf *tableFiles) deleteFile(name string) error {
	tf.lock.Lock()
	defer tf.lock.Unlock()

	//fmt.Println("deleteFile", name)
	fi, ok := tf.tableFiles[name]
	if !ok {
		fmt.Println(" not found")
		return errFileNotFound
	}
	if fi.isLocked() {
		//fmt.Println(" locked")
		return errFileLocked
	}
	atomic.StoreUint32(&fi.deleted, 1)
	delete(tf.tableFiles, name)
	if fi.fileCount == 0 {
		atomic.AddInt64(&fi.tf.memFileTotal, -fi.size)
	}
	for i := range fi.fileCount {
		id := osFileID{tfInfo: fi, fileIndex: i}
		if of, ok := tf.osFiles[id]; ok {
			if err := of.file.Close(); err != nil {
				log.Error("Could not close index table file", "name", tf.osFileName(fi.name, i), "error", err)
			}
			delete(tf.osFiles, id)
		}
		if err := os.Remove(tf.osFileName(name, i)); err != nil {
			//fmt.Println(" os.Remove error", err)
			return err
		}
	}
	//fmt.Println(" success")
	return nil
}

func (tf *tableFiles) osFileName(tfName string, fileIndex int) string {
	return filepath.Join(tf.path, fmt.Sprintf("%s.%04x", tfName, fileIndex))
}
