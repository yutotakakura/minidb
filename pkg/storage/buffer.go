package storage

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrNoFreeFrame = errors.New("storage: 空きフレームが無い (全ページが pin されている)")
	ErrNotPinned   = errors.New("storage: pin されていないページを unpin しようとした")
)

// frame はバッファプール内の 1 枠。ディスク上の 1 ページを保持する。
type frame struct {
	pageID   PageID
	data     Page
	pinCount int  // 0 より大きい間は追い出し禁止 (誰かが使用中)
	dirty    bool // メモリ上の内容がディスクより新しい
}

// BufferPool はディスクとメモリの境界そのもの。
//
// 上位層 (ヒープ, B+Tree, 実行器) は DiskManager を直接触らず、必ずここを通す。
// これにより「同じページを 2 回読んでも物理 I/O は 1 回」という状況が生まれる。
// インデックス走査が速い理由の一部はまさにこれで、B+Tree の上位ノードは
// 何度もアクセスされるので、ほぼ常にバッファに載っている。
//
// pin / unpin の規約:
//
//	page, err := bp.FetchPage(id)   // pin される。以後 page は追い出されない
//	defer bp.UnpinPage(id, dirty)   // 使い終わったら必ず返す
//
// unpin を忘れるとそのフレームは永久に居座り、いずれ ErrNoFreeFrame になる。
// これは本物の RDBMS でも実際に起きるバグ (buffer leak)。
type BufferPool struct {
	mu     sync.Mutex
	disk   *DiskManager
	frames []*frame
	table  map[PageID]int // pageID -> frames のインデックス
	free   []int          // 未使用フレームのインデックス

	// LRU: pinCount==0 のフレームだけが並ぶ。先頭ほど古い = 次に追い出される。
	lru     *list.List
	lruElem map[int]*list.Element

	Hits      int64 // バッファに載っていて物理 I/O を回避できた回数
	Misses    int64 // 載っておらずディスクから読んだ回数
	Evictions int64 // 追い出した回数
}

func NewBufferPool(disk *DiskManager, numFrames int) *BufferPool {
	if numFrames <= 0 {
		panic("storage: numFrames は 1 以上が必要")
	}
	bp := &BufferPool{
		disk:    disk,
		frames:  make([]*frame, numFrames),
		table:   make(map[PageID]int, numFrames),
		free:    make([]int, 0, numFrames),
		lru:     list.New(),
		lruElem: make(map[int]*list.Element, numFrames),
	}
	for i := range bp.frames {
		bp.frames[i] = &frame{pageID: InvalidPageID, data: NewPage()}
		bp.free = append(bp.free, i)
	}
	return bp
}

// FetchPage は指定ページをバッファ上に用意し、pin して返す。
func (bp *BufferPool) FetchPage(id PageID) (Page, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	// --- ヒット: すでにメモリ上にある。物理 I/O ゼロ ---
	if idx, ok := bp.table[id]; ok {
		bp.Hits++
		f := bp.frames[idx]
		if f.pinCount == 0 {
			bp.removeFromLRU(idx) // 使用中になるので追い出し候補から外す
		}
		f.pinCount++
		return f.data, nil
	}

	// --- ミス: ディスクから読む ---
	bp.Misses++
	idx, err := bp.allocFrame()
	if err != nil {
		return nil, err
	}
	f := bp.frames[idx]
	if err := bp.disk.ReadPage(id, f.data); err != nil {
		bp.free = append(bp.free, idx) // 読めなかったフレームは返却する
		return nil, err
	}
	f.pageID = id
	f.pinCount = 1
	f.dirty = false
	bp.table[id] = idx
	return f.data, nil
}

// NewPage は新しいページを確保し、初期化して pin した状態で返す。
func (bp *BufferPool) NewPage(t PageType) (PageID, Page, error) {
	id, err := bp.disk.AllocatePage()
	if err != nil {
		return 0, nil, err
	}

	bp.mu.Lock()
	defer bp.mu.Unlock()

	idx, err := bp.allocFrame()
	if err != nil {
		return 0, nil, err
	}
	f := bp.frames[idx]
	f.data.Init(t)
	f.pageID = id
	f.pinCount = 1
	f.dirty = true // 中身はまだディスクに無いので必ず書き出す必要がある
	bp.table[id] = idx
	return id, f.data, nil
}

// UnpinPage はページの使用を終える。
// dirty には「このページの内容を書き換えたか」を渡す。
// 一度でも true で unpin されたページは、追い出し時にディスクへ書き戻される。
func (bp *BufferPool) UnpinPage(id PageID, dirty bool) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	idx, ok := bp.table[id]
	if !ok {
		return fmt.Errorf("%w: page=%d", ErrNotPinned, id)
	}
	f := bp.frames[idx]
	if f.pinCount == 0 {
		return fmt.Errorf("%w: page=%d", ErrNotPinned, id)
	}
	if dirty {
		f.dirty = true
	}
	f.pinCount--
	if f.pinCount == 0 {
		bp.pushToLRU(idx) // 誰も使っていないので追い出し候補に戻る
	}
	return nil
}

// WithPage は pin/unpin を自動で対にするヘルパ。
// 実際のコードではこちらを使い、unpin 漏れを構造的に防ぐ。
// fn が返した dirty がそのまま UnpinPage に渡る。
func (bp *BufferPool) WithPage(id PageID, fn func(p Page) (dirty bool, err error)) error {
	p, err := bp.FetchPage(id)
	if err != nil {
		return err
	}
	dirty, err := fn(p)
	if uerr := bp.UnpinPage(id, dirty); uerr != nil && err == nil {
		err = uerr
	}
	return err
}

// allocFrame は空きフレームを 1 つ返す。無ければ LRU で追い出して作る。
// 呼び出し側で bp.mu を保持していること。
func (bp *BufferPool) allocFrame() (int, error) {
	if n := len(bp.free); n > 0 {
		idx := bp.free[n-1]
		bp.free = bp.free[:n-1]
		return idx, nil
	}

	// LRU の先頭 = 最も長く使われていない、かつ pin されていないフレーム。
	e := bp.lru.Front()
	if e == nil {
		// 全フレームが pin 済み。unpin 漏れか、プールが小さすぎる。
		return 0, ErrNoFreeFrame
	}
	idx := e.Value.(int)
	bp.removeFromLRU(idx)

	f := bp.frames[idx]
	// 追い出す前に、書き換えられていればディスクへ書き戻す。
	// この「必要になるまで書かない」のが遅延書き込みで、
	// 同じページへの複数回の更新を 1 回の物理 I/O にまとめてくれる。
	if f.dirty {
		if err := bp.disk.WritePage(f.pageID, f.data); err != nil {
			return 0, err
		}
		f.dirty = false
	}
	delete(bp.table, f.pageID)
	bp.Evictions++
	f.pageID = InvalidPageID
	return idx, nil
}

func (bp *BufferPool) pushToLRU(idx int) {
	if _, ok := bp.lruElem[idx]; ok {
		return
	}
	bp.lruElem[idx] = bp.lru.PushBack(idx) // 末尾 = 最も新しい
}

func (bp *BufferPool) removeFromLRU(idx int) {
	if e, ok := bp.lruElem[idx]; ok {
		bp.lru.Remove(e)
		delete(bp.lruElem, idx)
	}
}

// FlushPage は 1 ページをディスクへ書き戻す (バッファからは追い出さない)。
func (bp *BufferPool) FlushPage(id PageID) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	idx, ok := bp.table[id]
	if !ok {
		return nil
	}
	f := bp.frames[idx]
	if !f.dirty {
		return nil
	}
	if err := bp.disk.WritePage(f.pageID, f.data); err != nil {
		return err
	}
	f.dirty = false
	return nil
}

// FlushAll は dirty なページを全てディスクへ書き戻す。
// DB を閉じるときや、テストで「ディスクの状態」を確定させたいときに使う。
func (bp *BufferPool) FlushAll() error {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for _, f := range bp.frames {
		if f.pageID == InvalidPageID || !f.dirty {
			continue
		}
		if err := bp.disk.WritePage(f.pageID, f.data); err != nil {
			return err
		}
		f.dirty = false
	}
	return nil
}

// PinnedCount は現在 pin されたままのページ数。
// テストの最後にこれが 0 でなければ unpin 漏れがある。
func (bp *BufferPool) PinnedCount() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	n := 0
	for _, f := range bp.frames {
		if f.pinCount > 0 {
			n++
		}
	}
	return n
}

type Stats struct {
	Hits, Misses, Evictions int64
	DiskReads, DiskWrites   int64
}

// HitRate はバッファヒット率。実運用でまず見る指標。
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

func (s Stats) String() string {
	return fmt.Sprintf("buffer: hit=%d miss=%d (hit率 %.1f%%) evict=%d / disk: read=%d write=%d",
		s.Hits, s.Misses, s.HitRate()*100, s.Evictions, s.DiskReads, s.DiskWrites)
}

func (bp *BufferPool) Stats() Stats {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return Stats{
		Hits: bp.Hits, Misses: bp.Misses, Evictions: bp.Evictions,
		DiskReads: bp.disk.ReadCount, DiskWrites: bp.disk.WriteCount,
	}
}

// ResetStats はバッファとディスク双方のカウンタを 0 に戻す。
// EXPLAIN ANALYZE がクエリ単位の I/O を測るために使う。
func (bp *BufferPool) ResetStats() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.Hits, bp.Misses, bp.Evictions = 0, 0, 0
	bp.disk.ResetStats()
}

func (bp *BufferPool) Disk() *DiskManager { return bp.disk }
