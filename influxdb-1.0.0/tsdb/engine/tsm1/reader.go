package tsm1

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync"
)

// 单个 tsm 文件的读取对象
type TSMReader struct {
	mu sync.RWMutex

	// accessor provides access and decoding of blocks for the reader
	accessor blockAccessor

	// index is the index of all blocks.
	// 单个 tsm 文件的所有 block 的索引信息
	index TSMIndex

	// tombstoner ensures tombstoned keys are not available by the index.
	// 记录哪些数据是需要被删除的
	tombstoner *Tombstoner

	// size is the size of the file on disk.
	// 此 tsm 文件的大小
	size int64

	// lastModified is the last time this file was modified on disk
	// 文件最后修改时间
	lastModified int64
}

// TSMIndex represent the index section of a TSM file.  The index records all
// blocks, their locations, sizes, min and max times.
// 单个 tsm 文件的索引信息的操作接口
type TSMIndex interface {

	// Delete removes the given keys from the index.
	Delete(keys []string)

	// DeleteRange removes the given keys with data between minTime and maxTime from the index.
	DeleteRange(keys []string, minTime, maxTime int64)

	// Contains return true if the given key exists in the index.
	Contains(key string) bool

	// ContainsValue returns true if key and time might exists in this file.  This function could
	// return true even though the actual point does not exists.  For example, the key may
	// exists in this file, but not have point exactly at time t.
	ContainsValue(key string, timestamp int64) bool

	// Entries returns all index entries for a key.
	Entries(key string) []IndexEntry

	// ReadEntries reads the index entries for key into entries.
	ReadEntries(key string, entries *[]IndexEntry)

	// Entry returns the index entry for the specified key and timestamp.  If no entry
	// matches the key and timestamp, nil is returned.
	Entry(key string, timestamp int64) *IndexEntry

	// Key returns the key in the index at the given postion.
	Key(index int) (string, []IndexEntry)

	// KeyAt returns the key in the index at the given postion.
	// 返回指定位置的 key 的信息
	KeyAt(index int) (string, byte)

	// KeyCount returns the count of unique keys in the index.
	KeyCount() int

	// Size returns the size of a the current index in bytes
	Size() uint32

	// TimeRange returns the min and max time across all keys in the file.
	TimeRange() (int64, int64)

	// TombstoneRange returns ranges of time that are deleted for the given key.
	TombstoneRange(key string) []TimeRange

	// KeyRange returns the min and max keys in the file.
	KeyRange() (string, string)

	// Type returns the block type of the values stored for the key.  Returns one of
	// BlockFloat64, BlockInt64, BlockBool, BlockString.  If key does not exist,
	// an error is returned.
	Type(key string) (byte, error)

	// UnmarshalBinary populates an index from an encoded byte slice
	// representation of an index.
	UnmarshalBinary(b []byte) error
}

// BlockIterator allows iterating over each block in a TSM file in order.  It provides
// raw access to the block bytes without decoding them.
// block 迭代器
// 一个 block 里存的是一个 key 在一个指定时间段内的所有数据
type BlockIterator struct {
	r *TSMReader

	// i is the current key index
	i int

	// n is the total number of keys
	n int

	key     string
	entries []IndexEntry
	err     error
}

func (b *BlockIterator) PeekNext() string {
	if len(b.entries) > 1 {
		return b.key
	} else if b.n-b.i > 1 {
		key, _ := b.r.KeyAt(b.i + 1)
		return key
	}
	return ""
}

func (b *BlockIterator) Next() bool {
	if b.n-b.i == 0 && len(b.entries) == 0 {
		return false
	}

	if len(b.entries) > 0 {
		b.entries = b.entries[1:]
		if len(b.entries) > 0 {
			return true
		}
	}

	if b.n-b.i > 0 {
		b.key, b.entries = b.r.Key(b.i)
		b.i++

		if len(b.entries) > 0 {
			return true
		}
	}

	return false
}

func (b *BlockIterator) Read() (string, int64, int64, uint32, []byte, error) {
	if b.err != nil {
		return "", 0, 0, 0, nil, b.err
	}
	checksum, buf, err := b.r.readBytes(&b.entries[0], nil)
	if err != nil {
		return "", 0, 0, 0, nil, err
	}
	return b.key, b.entries[0].MinTime, b.entries[0].MaxTime, checksum, buf, err
}

// blockAccessor abstracts a method of accessing blocks from a
// TSM file.
type blockAccessor interface {
	init() (*indirectIndex, error)
	read(key string, timestamp int64) ([]Value, error)
	readAll(key string) ([]Value, error)
	readBlock(entry *IndexEntry, values []Value) ([]Value, error)
	readFloatBlock(entry *IndexEntry, tdec *TimeDecoder, fdec *FloatDecoder, values *[]FloatValue) ([]FloatValue, error)
	readIntegerBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *IntegerDecoder, values *[]IntegerValue) ([]IntegerValue, error)
	readStringBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *StringDecoder, values *[]StringValue) ([]StringValue, error)
	readBooleanBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *BooleanDecoder, values *[]BooleanValue) ([]BooleanValue, error)
	readBytes(entry *IndexEntry, buf []byte) (uint32, []byte, error)
	path() string
	close() error
}

// 创建一个 tsm 文件的读取对象
func NewTSMReader(f *os.File) (*TSMReader, error) {
	t := &TSMReader{}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	t.size = stat.Size()
	t.lastModified = stat.ModTime().UnixNano()
	// mmap 操作的管理对象
	t.accessor = &mmapAccessor{
		f: f,
	}

	index, err := t.accessor.init()
	if err != nil {
		return nil, err
	}

	t.index = index
	t.tombstoner = &Tombstoner{Path: t.Path()}

	if err := t.applyTombstones(); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *TSMReader) applyTombstones() error {
	// Read any tombstone entries if the exist
	tombstones, err := t.tombstoner.ReadAll()
	if err != nil {
		return fmt.Errorf("init: read tombstones: %v", err)
	}

	if len(tombstones) == 0 {
		return nil
	}

	var cur, prev Tombstone
	cur = tombstones[0]
	batch := []string{cur.Key}
	for i := 1; i < len(tombstones); i++ {
		cur = tombstones[i]
		prev = tombstones[i-1]
		if prev.Min != cur.Min || prev.Max != cur.Max {
			t.index.DeleteRange(batch, prev.Min, prev.Max)
			batch = batch[:0]
		}
		batch = append(batch, cur.Key)
	}

	if len(batch) > 0 {
		t.index.DeleteRange(batch, cur.Min, cur.Max)
	}
	return nil
}

func (t *TSMReader) Path() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.accessor.path()
}

func (t *TSMReader) Key(index int) (string, []IndexEntry) {
	return t.index.Key(index)
}

// KeyAt returns the key and key type at position idx in the index.
func (t *TSMReader) KeyAt(idx int) (string, byte) {
	return t.index.KeyAt(idx)
}

func (t *TSMReader) ReadAt(entry *IndexEntry, vals []Value) ([]Value, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.accessor.readBlock(entry, vals)
}

// 根据内存中 block 的索引数据，获取整个 block 的数据
func (t *TSMReader) ReadFloatBlockAt(entry *IndexEntry, tdec *TimeDecoder, vdec *FloatDecoder, vals *[]FloatValue) ([]FloatValue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.accessor.readFloatBlock(entry, tdec, vdec, vals)
}

func (t *TSMReader) ReadIntegerBlockAt(entry *IndexEntry, tdec *TimeDecoder, vdec *IntegerDecoder, vals *[]IntegerValue) ([]IntegerValue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.accessor.readIntegerBlock(entry, tdec, vdec, vals)
}

func (t *TSMReader) ReadStringBlockAt(entry *IndexEntry, tdec *TimeDecoder, vdec *StringDecoder, vals *[]StringValue) ([]StringValue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.accessor.readStringBlock(entry, tdec, vdec, vals)
}

func (t *TSMReader) ReadBooleanBlockAt(entry *IndexEntry, tdec *TimeDecoder, vdec *BooleanDecoder, vals *[]BooleanValue) ([]BooleanValue, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.accessor.readBooleanBlock(entry, tdec, vdec, vals)
}

func (t *TSMReader) Read(key string, timestamp int64) ([]Value, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.accessor.read(key, timestamp)
}

// ReadAll returns all values for a key in all blocks.
// 读取这个文件中 key 对应的所有数据
func (t *TSMReader) ReadAll(key string) ([]Value, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.accessor.readAll(key)
}

func (t *TSMReader) readBytes(e *IndexEntry, b []byte) (uint32, []byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.accessor.readBytes(e, b)
}

func (t *TSMReader) Type(key string) (byte, error) {
	return t.index.Type(key)
}

func (t *TSMReader) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.accessor.close()
}

// Remove removes any underlying files stored on disk for this reader.
func (t *TSMReader) Remove() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	path := t.accessor.path()
	if path != "" {
		os.RemoveAll(path)
	}

	if err := t.tombstoner.Delete(); err != nil {
		return err
	}
	return nil
}

func (t *TSMReader) Contains(key string) bool {
	return t.index.Contains(key)
}

// ContainsValue returns true if key and time might exists in this file.  This function could
// return true even though the actual point does not exists.  For example, the key may
// exists in this file, but not have point exactly at time t.
func (t *TSMReader) ContainsValue(key string, ts int64) bool {
	return t.index.ContainsValue(key, ts)
}

// DeleteRange removes the given points for keys between minTime and maxTime
// 删除 tsm 文件中的指定数据，文件中并不实际删除，而是用 ts文件的方式，内存索引中删除
func (t *TSMReader) DeleteRange(keys []string, minTime, maxTime int64) error {
	// 在 tombstone 中加入要删除的数据信息
	if err := t.tombstoner.AddRange(keys, minTime, maxTime); err != nil {
		return err
	}

	// 在 tsm 文件内存索引中删除指定的数据信息
	t.index.DeleteRange(keys, minTime, maxTime)
	return nil
}

// 在索引中删除指定的 key
func (t *TSMReader) Delete(keys []string) error {
	if err := t.tombstoner.Add(keys); err != nil {
		return err
	}

	t.index.Delete(keys)
	return nil
}

// TimeRange returns the min and max time across all keys in the file.
func (t *TSMReader) TimeRange() (int64, int64) {
	return t.index.TimeRange()
}

// KeyRange returns the min and max key across all keys in the file.
func (t *TSMReader) KeyRange() (string, string) {
	return t.index.KeyRange()
}

func (t *TSMReader) KeyCount() int {
	return t.index.KeyCount()
}

func (t *TSMReader) Entries(key string) []IndexEntry {
	return t.index.Entries(key)
}

func (t *TSMReader) ReadEntries(key string, entries *[]IndexEntry) {
	t.index.ReadEntries(key, entries)
}

func (t *TSMReader) IndexSize() uint32 {
	return t.index.Size()
}

func (t *TSMReader) Size() uint32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return uint32(t.size)
}

func (t *TSMReader) LastModified() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastModified
}

// HasTombstones return true if there are any tombstone entries recorded.
func (t *TSMReader) HasTombstones() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tombstoner.HasTombstones()
}

// TombstoneFiles returns any tombstone files associated with this TSM file.
func (t *TSMReader) TombstoneFiles() []FileStat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tombstoner.TombstoneFiles()
}

// TombstoneRange returns ranges of time that are deleted for the given key.
func (t *TSMReader) TombstoneRange(key string) []TimeRange {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.index.TombstoneRange(key)
}

func (t *TSMReader) Stats() FileStat {
	minTime, maxTime := t.index.TimeRange()
	minKey, maxKey := t.index.KeyRange()
	return FileStat{
		Path:         t.Path(),
		Size:         t.Size(),
		LastModified: t.LastModified(),
		MinTime:      minTime,
		MaxTime:      maxTime,
		MinKey:       minKey,
		MaxKey:       maxKey,
		HasTombstone: t.tombstoner.HasTombstones(),
	}
}

func (t *TSMReader) BlockIterator() *BlockIterator {
	return &BlockIterator{
		r: t,
		n: t.index.KeyCount(),
	}
}

// indirectIndex is a TSMIndex that uses a raw byte slice representation of an index.  This
// implementation can be used for indexes that may be MMAPed into memory.
// 间接索引，只存放每一个 key 在下一层详细索引中的偏移量的信息
type indirectIndex struct {
	mu sync.RWMutex

	// indirectIndex works a follows.  Assuming we have an index structure in memory as
	// the diagram below:
	//
	// ┌────────────────────────────────────────────────────────────────────┐
	// │                               Index                                │
	// ├─┬──────────────────────┬──┬───────────────────────┬───┬────────────┘
	// │0│                      │62│                       │145│
	// ├─┴───────┬─────────┬────┼──┴──────┬─────────┬──────┼───┴─────┬──────┐
	// │Key 1 Len│   Key   │... │Key 2 Len│  Key 2  │ ...  │  Key 3  │ ...  │
	// │ 2 bytes │ N bytes │    │ 2 bytes │ N bytes │      │ 2 bytes │      │
	// └─────────┴─────────┴────┴─────────┴─────────┴──────┴─────────┴──────┘

	// We would build an `offsets` slices where each element pointers to the byte location
	// for the first key in the index slice.

	// ┌────────────────────────────────────────────────────────────────────┐
	// │                              Offsets                               │
	// ├────┬────┬────┬─────────────────────────────────────────────────────┘
	// │ 0  │ 62 │145 │
	// └────┴────┴────┘

	// Using this offset slice we can find `Key 2` by doing a binary search
	// over the offsets slice.  Instead of comparing the value in the offsets
	// (e.g. `62`), we use that as an index into the underlying index to
	// retrieve the key at postion `62` and perform our comparisons with that.

	// When we have identified the correct position in the index for a given
	// key, we could perform another binary search or a linear scan.  This
	// should be fast as well since each index entry is 28 bytes and all
	// contiguous in memory.  The current implementation uses a linear scan since the
	// number of block entries is expected to be < 100 per key.

	// b is the underlying index byte slice.  This could be a copy on the heap or an MMAP
	// slice reference
	// 下层详细索引的字节流
	b []byte

	// offsets contains the positions in b for each key.  It points to the 2 byte length of
	// key.
	// 偏移量数组，记录了一个 key 在 b 中的偏移量
	offsets []int32

	// minKey, maxKey are the minium and maximum (lexicographically sorted) contained in the
	// file
	minKey, maxKey string

	// minTime, maxTime are the minimum and maximum times contained in the file across all
	// series.
	// 此文件中的最小时间和最大时间，根据这个可以快速判断要查询的数据在此文件中是否存在，是否有必要读取这个文件
	minTime, maxTime int64

	// tombstones contains only the tombstoned keys with subset of time values deleted.  An
	// entry would exist here if a subset of the points for a key were deleted and the file
	// had not be re-compacted to remove the points on disk.
	// 用于记录哪些 key 在指定范围内的数据是已经被删除的
	tombstones map[string][]TimeRange
}

// 时间范围
type TimeRange struct {
	Min, Max int64
}

// 创建一个间接索引
func NewIndirectIndex() *indirectIndex {
	return &indirectIndex{
		tombstones: make(map[string][]TimeRange),
	}
}

// search returns the index of i in offsets for where key is located.  If key is not
// in the index, len(index) is returned.
func (d *indirectIndex) search(key []byte) int {
	// We use a binary search across our indirect offsets (pointers to all the keys
	// in the index slice).
	i := sort.Search(len(d.offsets), func(i int) bool {
		// i is the position in offsets we are at so get offset it points to
		offset := d.offsets[i]

		// It's pointing to the start of the key which is a 2 byte length
		keyLen := int32(binary.BigEndian.Uint16(d.b[offset : offset+2]))

		// See if it matches
		return bytes.Compare(d.b[offset+2:offset+2+keyLen], key) >= 0
	})

	// See if we might have found the right index
	if i < len(d.offsets) {
		ofs := d.offsets[i]
		_, k, err := readKey(d.b[ofs:])
		if err != nil {
			panic(fmt.Sprintf("error reading key: %v", err))
		}

		// The search may have returned an i == 0 which could indicated that the value
		// searched should be inserted at postion 0.  Make sure the key in the index
		// matches the search value.
		if !bytes.Equal(key, k) {
			return len(d.b)
		}

		return int(ofs)
	}

	// The key is not in the index.  i is the index where it would be inserted so return
	// a value outside our offset range.
	return len(d.b)
}

// Entries returns all index entries for a key.
func (d *indirectIndex) Entries(key string) []IndexEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()

	kb := []byte(key)

	ofs := d.search(kb)
	if ofs < len(d.b) {
		n, k, err := readKey(d.b[ofs:])
		if err != nil {
			panic(fmt.Sprintf("error reading key: %v", err))
		}

		// The search may have returned an i == 0 which could indicated that the value
		// searched should be inserted at postion 0.  Make sure the key in the index
		// matches the search value.
		if !bytes.Equal(kb, k) {
			return nil
		}

		// Read and return all the entries
		ofs += n
		var entries indexEntries
		if _, err := readEntries(d.b[ofs:], &entries); err != nil {
			panic(fmt.Sprintf("error reading entries: %v", err))
		}
		return entries.entries
	}

	// The key is not in the index.  i is the index where it would be inserted.
	return nil
}

// ReadEntries returns all index entries for a key.
func (d *indirectIndex) ReadEntries(key string, entries *[]IndexEntry) {
	*entries = d.Entries(key)
}

// Entry returns the index entry for the specified key and timestamp.  If no entry
// matches the key an timestamp, nil is returned.
func (d *indirectIndex) Entry(key string, timestamp int64) *IndexEntry {
	entries := d.Entries(key)
	for _, entry := range entries {
		if entry.Contains(timestamp) {
			return &entry
		}
	}
	return nil
}

func (d *indirectIndex) Key(idx int) (string, []IndexEntry) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if idx < 0 || idx >= len(d.offsets) {
		return "", nil
	}
	n, key, _ := readKey(d.b[d.offsets[idx]:])

	var entries indexEntries
	readEntries(d.b[int(d.offsets[idx])+n:], &entries)
	return string(key), entries.entries
}

// 返回指定位置的 key 的名字以及类型
func (d *indirectIndex) KeyAt(idx int) (string, byte) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if idx < 0 || idx >= len(d.offsets) {
		return "", 0
	}
	// 反序列化 key 的信息，通过间接索引快速定位到指定序号的 key 在详细索引中的位置，之后获取 key 的值以及类型信息
	// 2字节 keySize + N字节 key
	n, key, _ := readKey(d.b[d.offsets[idx]:])
	return string(key), d.b[d.offsets[idx]+int32(n)]
}

func (d *indirectIndex) KeyCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return len(d.offsets)
}

// 删除指定的 key 的信息，这里仅仅是在 offset 数组里将 key 的位置信息删除，这样就不会再查到
func (d *indirectIndex) Delete(keys []string) {
	if len(keys) == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	lookup := map[string]struct{}{}
	for _, k := range keys {
		lookup[k] = struct{}{}
	}

	var offsets []int32
	// 遍历索引中的每一个 key，去除掉要删除的 key
	for _, offset := range d.offsets {
		_, indexKey, _ := readKey(d.b[offset:])

		if _, ok := lookup[string(indexKey)]; ok {
			continue
		}
		offsets = append(offsets, int32(offset))
	}
	d.offsets = offsets
}

// 在内存索引中删除指定的数据信息
func (d *indirectIndex) DeleteRange(keys []string, minTime, maxTime int64) {
	// No keys, nothing to do
	if len(keys) == 0 {
		return
	}

	// If we're deleting the max time range, just use tombstoning to remove the
	// key from the offsets slice
	// 删除所有数据
	if minTime == math.MinInt64 && maxTime == math.MaxInt64 {
		d.Delete(keys)
		return
	}

	// Is the range passed in outside of the time range for the file?
	// 检查要删除的数据是否在此文件中存在
	min, max := d.TimeRange()
	if minTime > max || maxTime < min {
		return
	}

	tombstones := map[string][]TimeRange{}
	for _, k := range keys {

		// Is the range passed in outside the time range for this key?
		// 获取指定的 key 在这个 tsm 文件中的 block 块的索引信息
		// entries 包括了每个 block 中数据的 MinTime 和 MaxTime
		entries := d.Entries(k)

		// If multiple tombstones are saved for the same key
		if len(entries) == 0 {
			continue
		}

		// 检查时间范围是否匹配，没有数据的直接跳过
		min, max := entries[0].MinTime, entries[len(entries)-1].MaxTime
		if minTime > max || maxTime < min {
			continue
		}

		// Is the range passed in cover every value for the key?
		// 如果已经包含了要删除的所有数据，直接删除这个 key
		if minTime <= min && maxTime >= max {
			d.Delete(keys)
			continue
		}

		// 记录下要删除的 key 的时间范围
		tombstones[k] = append(tombstones[k], TimeRange{minTime, maxTime})
	}

	if len(tombstones) == 0 {
		return
	}

	d.mu.Lock()
	for k, v := range tombstones {
		d.tombstones[k] = append(d.tombstones[k], v...)
	}
	d.mu.Unlock()

}

func (d *indirectIndex) TombstoneRange(key string) []TimeRange {
	d.mu.RLock()
	r := d.tombstones[key]
	d.mu.RUnlock()
	return r
}

func (d *indirectIndex) Contains(key string) bool {
	return len(d.Entries(key)) > 0
}

func (d *indirectIndex) ContainsValue(key string, timestamp int64) bool {
	entry := d.Entry(key, timestamp)
	if entry == nil {
		return false
	}

	d.mu.RLock()
	tombstones := d.tombstones[key]
	d.mu.RUnlock()

	for _, t := range tombstones {
		if t.Min <= timestamp && t.Max >= timestamp {
			return false
		}
	}
	return true
}

func (d *indirectIndex) Type(key string) (byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	kb := []byte(key)
	ofs := d.search(kb)
	if ofs < len(d.b) {
		n, _, err := readKey(d.b[ofs:])
		if err != nil {
			panic(fmt.Sprintf("error reading key: %v", err))
		}

		ofs += n
		return d.b[ofs], nil
	}
	return 0, fmt.Errorf("key does not exist: %v", key)
}

func (d *indirectIndex) KeyRange() (string, string) {
	return d.minKey, d.maxKey
}

func (d *indirectIndex) TimeRange() (int64, int64) {
	return d.minTime, d.maxTime
}

// MarshalBinary returns a byte slice encoded version of the index.
func (d *indirectIndex) MarshalBinary() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return d.b, nil
}

// UnmarshalBinary populates an index from an encoded byte slice
// representation of an index.
func (d *indirectIndex) UnmarshalBinary(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Keep a reference to the actual index bytes
	d.b = b

	//var minKey, maxKey []byte
	var minTime, maxTime int64 = math.MaxInt64, 0

	// To create our "indirect" index, we need to find the location of all the keys in
	// the raw byte slice.  The keys are listed once each (in sorted order).  Following
	// each key is a time ordered list of index entry blocks for that key.  The loop below
	// basically skips across the slice keeping track of the counter when we are at a key
	// field.
	var i int32
	for i < int32(len(b)) {
		d.offsets = append(d.offsets, i)

		// Skip to the start of the values
		// key length value (2) + type (1) + length of key
		i += 3 + int32(binary.BigEndian.Uint16(b[i:i+2]))

		// count of index entries
		count := int32(binary.BigEndian.Uint16(b[i : i+indexCountSize]))
		i += indexCountSize

		// Find the min time for the block
		minT := int64(binary.BigEndian.Uint64(b[i : i+8]))
		if minT < minTime {
			minTime = minT
		}

		i += (count - 1) * indexEntrySize

		// Find the max time for the block
		maxT := int64(binary.BigEndian.Uint64(b[i+8 : i+16]))
		if maxT > maxTime {
			maxTime = maxT
		}

		i += indexEntrySize
	}

	firstOfs := d.offsets[0]
	_, key, err := readKey(b[firstOfs:])
	if err != nil {
		return err
	}
	d.minKey = string(key)

	lastOfs := d.offsets[len(d.offsets)-1]
	_, key, err = readKey(b[lastOfs:])
	if err != nil {
		return err
	}
	d.maxKey = string(key)

	d.minTime = minTime
	d.maxTime = maxTime

	return nil
}

func (d *indirectIndex) Size() uint32 {
	d.mu.RLock()
	defer d.mu.RUnlock()

	return uint32(len(d.b))
}

// mmapAccess is mmap based block accessor.  It access blocks through an
// MMAP file interface.
// 通过 mmap 访问文件
type mmapAccessor struct {
	mu sync.RWMutex

	f     *os.File
	b     []byte
	index *indirectIndex
}

func (m *mmapAccessor) init() (*indirectIndex, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查 reader 对象对应的是否是一个 tsm 文件流对应的格式，主要是 MagicNumber 和 version 的比对
	if err := verifyVersion(m.f); err != nil {
		return nil, err
	}

	var err error

	// 定位到文件开始
	if _, err := m.f.Seek(0, 0); err != nil {
		return nil, err
	}

	stat, err := m.f.Stat()
	if err != nil {
		return nil, err
	}

	// mmap 整个文件
	m.b, err = mmap(m.f, 0, int(stat.Size()))
	if err != nil {
		return nil, err
	}

	// 获取此 tsm 文件中 Index 部分的偏移量
	indexOfsPos := len(m.b) - 8
	indexStart := binary.BigEndian.Uint64(m.b[indexOfsPos : indexOfsPos+8])

	m.index = NewIndirectIndex()
	if err := m.index.UnmarshalBinary(m.b[indexStart:indexOfsPos]); err != nil {
		return nil, err
	}

	return m.index, nil
}

func (m *mmapAccessor) read(key string, timestamp int64) ([]Value, error) {
	entry := m.index.Entry(key, timestamp)
	if entry == nil {
		return nil, nil
	}

	return m.readBlock(entry, nil)
}

func (m *mmapAccessor) readBlock(entry *IndexEntry, values []Value) ([]Value, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return nil, ErrTSMClosed
	}
	//TODO: Validate checksum
	var err error
	values, err = DecodeBlock(m.b[entry.Offset+4:entry.Offset+int64(entry.Size)], values)
	if err != nil {
		return nil, err
	}

	return values, nil
}

// 通过 mmap，从文件中获取指定 block 的数据，并且反序列化成 FloatValue 数组
func (m *mmapAccessor) readFloatBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *FloatDecoder, values *[]FloatValue) ([]FloatValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return nil, ErrTSMClosed
	}

	//TODO: Validate checksum
	a, err := DecodeFloatBlock(m.b[entry.Offset+4:entry.Offset+int64(entry.Size)], tdec, vdec, values)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func (m *mmapAccessor) readIntegerBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *IntegerDecoder, values *[]IntegerValue) ([]IntegerValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return nil, ErrTSMClosed
	}
	//TODO: Validate checksum
	a, err := DecodeIntegerBlock(m.b[entry.Offset+4:entry.Offset+int64(entry.Size)], tdec, vdec, values)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func (m *mmapAccessor) readStringBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *StringDecoder, values *[]StringValue) ([]StringValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return nil, ErrTSMClosed
	}
	//TODO: Validate checksum
	a, err := DecodeStringBlock(m.b[entry.Offset+4:entry.Offset+int64(entry.Size)], tdec, vdec, values)
	if err != nil {
		return nil, err
	}

	return a, nil
}

func (m *mmapAccessor) readBooleanBlock(entry *IndexEntry, tdec *TimeDecoder, vdec *BooleanDecoder, values *[]BooleanValue) ([]BooleanValue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return nil, ErrTSMClosed
	}
	//TODO: Validate checksum
	a, err := DecodeBooleanBlock(m.b[entry.Offset+4:entry.Offset+int64(entry.Size)], tdec, vdec, values)
	if err != nil {
		return nil, err
	}

	return a, nil
}

// 读取一个 block 的内容，前4字节为 checksum，后面为 data 数据，其长度为索引中的 offset 加上 size
func (m *mmapAccessor) readBytes(entry *IndexEntry, b []byte) (uint32, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if int64(len(m.b)) < entry.Offset+int64(entry.Size) {
		return 0, nil, ErrTSMClosed
	}

	// return the bytes after the 4 byte checksum
	return binary.BigEndian.Uint32(m.b[entry.Offset : entry.Offset+4]), m.b[entry.Offset+4 : entry.Offset+int64(entry.Size)], nil
}

// ReadAll returns all values for a key in all blocks.
func (m *mmapAccessor) readAll(key string) ([]Value, error) {
	blocks := m.index.Entries(key)
	if len(blocks) == 0 {
		return nil, nil
	}

	tombstones := m.index.TombstoneRange(key)

	m.mu.RLock()
	defer m.mu.RUnlock()

	var temp []Value
	var err error
	var values []Value
	for _, block := range blocks {
		var skip bool
		for _, t := range tombstones {
			// Should we skip this block because it contains points that have been deleted
			if t.Min <= block.MinTime && t.Max >= block.MaxTime {
				skip = true
				break
			}
		}

		if skip {
			continue
		}
		//TODO: Validate checksum
		temp = temp[:0]
		// The +4 is the 4 byte checksum length
		temp, err = DecodeBlock(m.b[block.Offset+4:block.Offset+int64(block.Size)], temp)
		if err != nil {
			return nil, err
		}

		// Filter out any values that were deleted
		for _, t := range tombstones {
			temp = Values(temp).Exclude(t.Min, t.Max)
		}

		values = append(values, temp...)
	}

	return values, nil
}

func (m *mmapAccessor) path() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.f.Name()
}

func (m *mmapAccessor) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.b == nil {
		return nil
	}

	err := munmap(m.b)
	if err != nil {
		return err
	}

	m.b = nil
	return m.f.Close()
}

type indexEntries struct {
	Type    byte
	entries []IndexEntry
}

func (a *indexEntries) Len() int      { return len(a.entries) }
func (a *indexEntries) Swap(i, j int) { a.entries[i], a.entries[j] = a.entries[j], a.entries[i] }
func (a *indexEntries) Less(i, j int) bool {
	return a.entries[i].MinTime < a.entries[j].MinTime
}

func (a *indexEntries) MarshalBinary() ([]byte, error) {
	buf := make([]byte, len(a.entries)*indexEntrySize)

	for i, entry := range a.entries {
		entry.AppendTo(buf[indexEntrySize*i:])
	}

	return buf, nil
}

func (a *indexEntries) WriteTo(w io.Writer) (total int64, err error) {
	var buf [indexEntrySize]byte
	var n int

	for _, entry := range a.entries {
		entry.AppendTo(buf[:])
		n, err = w.Write(buf[:])
		total += int64(n)
		if err != nil {
			return total, err
		}
	}

	return total, nil
}

// 反序列化 key 的信息
func readKey(b []byte) (n int, key []byte, err error) {
	// 2 byte size of key
	// 2字节 key 的长度
	n, size := 2, int(binary.BigEndian.Uint16(b[:2]))

	// N byte key
	// 获取 key 的内容
	key = b[n : n+size]

	// 共占用的长度
	n += len(key)
	return
}

// 反序列化 block 的信息
func readEntries(b []byte, entries *indexEntries) (n int, err error) {
	// 1 byte block type
	entries.Type = b[n]
	n++

	// 2 byte count of index entries
	count := int(binary.BigEndian.Uint16(b[n : n+indexCountSize]))
	n += indexCountSize

	entries.entries = make([]IndexEntry, count)
	for i := 0; i < count; i++ {
		var ie IndexEntry
		if err := ie.UnmarshalBinary(b[i*indexEntrySize+indexCountSize+indexTypeSize : i*indexEntrySize+indexCountSize+indexEntrySize+indexTypeSize]); err != nil {
			return 0, fmt.Errorf("readEntries: unmarshal error: %v", err)
		}
		entries.entries[i] = ie
		n += indexEntrySize
	}
	return
}
