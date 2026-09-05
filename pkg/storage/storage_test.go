package storage

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func tempDisk(t *testing.T) *DiskManager {
	t.Helper()
	d, err := OpenDiskManager(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// --- ページ ---

func TestPageInsertAndGet(t *testing.T) {
	p := NewPage()
	p.Init(PageTypeHeap)

	if p.Type() != PageTypeHeap {
		t.Fatalf("type=%v", p.Type())
	}
	if got, want := p.FreeSpace(), PageSize-PageHeaderSize; got != want {
		t.Fatalf("初期空き容量 got=%d want=%d", got, want)
	}

	inputs := [][]byte{[]byte("alice"), []byte("bob"), []byte("")}
	for i, in := range inputs {
		slot, err := p.Insert(in)
		if err != nil {
			t.Fatal(err)
		}
		if slot != i {
			t.Fatalf("slot got=%d want=%d", slot, i)
		}
	}

	for i, want := range inputs {
		got, err := p.Get(i)
		if err != nil && !errors.Is(err, ErrSlotDeleted) {
			t.Fatal(err)
		}
		// 長さ 0 のタプルは削除済みと区別できない (既知の割り切り)。
		if len(want) == 0 {
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("slot %d got=%q want=%q", i, got, want)
		}
	}
}

func TestPageDeleteIsTombstone(t *testing.T) {
	p := NewPage()
	p.Init(PageTypeHeap)
	p.Insert([]byte("keep"))
	p.Insert([]byte("gone"))

	if err := p.Delete(1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(1); !errors.Is(err, ErrSlotDeleted) {
		t.Fatalf("削除済みスロットの取得 err=%v", err)
	}
	// スロット番号は詰められない。これが RID の安定性を支えている。
	if p.NumSlots() != 2 {
		t.Fatalf("NumSlots got=%d want=2", p.NumSlots())
	}
	if p.LiveTuples() != 1 {
		t.Fatalf("LiveTuples got=%d want=1", p.LiveTuples())
	}
	if got, err := p.Get(0); err != nil || !bytes.Equal(got, []byte("keep")) {
		t.Fatalf("残ったスロットが壊れた got=%q err=%v", got, err)
	}
}

func TestPageFillUntilFull(t *testing.T) {
	p := NewPage()
	p.Init(PageTypeHeap)

	tuple := make([]byte, 100)
	n := 0
	for {
		if _, err := p.Insert(tuple); err != nil {
			if errors.Is(err, ErrPageFull) {
				break
			}
			t.Fatal(err)
		}
		n++
		if n > PageSize {
			t.Fatal("ErrPageFull が返らない (無限ループ)")
		}
	}
	// 1 件あたり 100B + スロット 4B = 104B、使える領域は 4096-16 = 4080B。
	if want := (PageSize - PageHeaderSize) / (100 + SlotSize); n != want {
		t.Fatalf("入った件数 got=%d want=%d", n, want)
	}
	if p.FreeSpace() >= 100+SlotSize {
		t.Fatalf("満杯のはずが空きがある: %d", p.FreeSpace())
	}
}

func TestPageRejectsOversizedTuple(t *testing.T) {
	p := NewPage()
	p.Init(PageTypeHeap)
	if _, err := p.Insert(make([]byte, MaxTupleSize+1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

// --- DiskManager ---

func TestDiskPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	d1, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d1.AllocatePage()
	if err != nil {
		t.Fatal(err)
	}
	p := NewPage()
	p.Init(PageTypeHeap)
	p.Insert([]byte("durable"))
	if err := d1.WritePage(id, p); err != nil {
		t.Fatal(err)
	}
	d1.Sync()
	d1.Close()

	// 開き直して同じ内容が読めること = 永続化できている。
	d2, err := OpenDiskManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.NumPages() != 1 {
		t.Fatalf("NumPages got=%d want=1", d2.NumPages())
	}
	q := NewPage()
	if err := d2.ReadPage(id, q); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("durable")) {
		t.Fatalf("got=%q", got)
	}
}

func TestDiskRejectsUnallocatedRead(t *testing.T) {
	d := tempDisk(t)
	if err := d.ReadPage(42, NewPage()); err == nil {
		t.Fatal("未割り当てページの読み込みがエラーにならない")
	}
}

// --- BufferPool ---

func TestBufferPoolHitAvoidsDiskRead(t *testing.T) {
	d := tempDisk(t)
	bp := NewBufferPool(d, 4)

	id, p, err := bp.NewPage(PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	p.Insert([]byte("x"))
	bp.UnpinPage(id, true)

	bp.ResetStats()
	for i := 0; i < 10; i++ {
		if _, err := bp.FetchPage(id); err != nil {
			t.Fatal(err)
		}
		bp.UnpinPage(id, false)
	}

	s := bp.Stats()
	// 一度メモリに載ったページは、何度触っても物理読み込みは起きない。
	if s.Misses != 0 || s.DiskReads != 0 {
		t.Fatalf("キャッシュが効いていない: %s", s)
	}
	if s.Hits != 10 {
		t.Fatalf("Hits got=%d want=10", s.Hits)
	}
}

func TestBufferPoolEvictsLRUAndWritesBackDirty(t *testing.T) {
	d := tempDisk(t)
	bp := NewBufferPool(d, 2) // わざと 2 枠しかないプール

	ids := make([]PageID, 3)
	for i := range ids {
		id, p, err := bp.NewPage(PageTypeHeap)
		if err != nil {
			t.Fatal(err)
		}
		p.Insert([]byte(fmt.Sprintf("page-%d", i)))
		ids[i] = id
		bp.UnpinPage(id, true) // dirty で返す
	}

	// 3 ページ目を確保した時点で、最も古い 1 ページ目が追い出されているはず。
	if got := bp.Stats().Evictions; got != 1 {
		t.Fatalf("Evictions got=%d want=1", got)
	}

	// 追い出されたページを読み直す。dirty のまま書き戻されていれば内容が残っている。
	p, err := bp.FetchPage(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer bp.UnpinPage(ids[0], false)
	got, err := p.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("page-0")) {
		t.Fatalf("追い出し時に書き戻されていない got=%q", got)
	}
}

func TestBufferPoolPinnedPageIsNotEvicted(t *testing.T) {
	d := tempDisk(t)
	bp := NewBufferPool(d, 1)

	id, _, err := bp.NewPage(PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	// unpin していないので、このページは追い出せない。
	if _, _, err := bp.NewPage(PageTypeHeap); !errors.Is(err, ErrNoFreeFrame) {
		t.Fatalf("pin 中のページが追い出された err=%v", err)
	}
	bp.UnpinPage(id, false)

	// 返した後なら確保できる。
	if _, _, err := bp.NewPage(PageTypeHeap); err != nil {
		t.Fatal(err)
	}
}

func TestBufferPoolWithPageUnpinsOnError(t *testing.T) {
	d := tempDisk(t)
	bp := NewBufferPool(d, 2)
	id, _, _ := bp.NewPage(PageTypeHeap)
	bp.UnpinPage(id, false)

	boom := errors.New("boom")
	if err := bp.WithPage(id, func(p Page) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// エラーで抜けても unpin されていること。
	if n := bp.PinnedCount(); n != 0 {
		t.Fatalf("pin が漏れている: %d", n)
	}
}
