package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// DiskManager はデータファイルへの「ページ単位の」読み書きだけを担当する。
//
// この層の責務はあえて狭く保つ。ページ番号をバイト位置に変換して pread/pwrite する、
// それだけ。キャッシュも並べ替えもしない。それは上の BufferPool の仕事。
//
// ReadCount / WriteCount が本プロジェクトの主役の 1 つ。
// ここを数えることで「この SQL は物理 I/O を何回起こしたか」が測れるようになり、
// インデックスの有無で実行計画がどう変わるかを数字で確認できる。
type DiskManager struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	numPages PageID

	ReadCount  int64 // 実際にファイルから読んだページ数
	WriteCount int64 // 実際にファイルへ書いたページ数
}

func OpenDiskManager(path string) (*DiskManager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("storage: ファイルを開けない %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := st.Size()
	if size%PageSize != 0 {
		f.Close()
		return nil, fmt.Errorf("storage: ファイルサイズ %d が PageSize %d の倍数でない (破損の可能性)", size, PageSize)
	}
	return &DiskManager{f: f, path: path, numPages: PageID(size / PageSize)}, nil
}

// ReadPage は id 番のページをまるごと p に読み込む。
func (d *DiskManager) ReadPage(id PageID, p Page) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if id >= d.numPages {
		return fmt.Errorf("storage: ページ %d は未割り当て (numPages=%d)", id, d.numPages)
	}
	off := int64(id) * PageSize
	// ReadAt は「短く読めた」場合に io.ErrUnexpectedEOF を返してくれるので、
	// 部分読み込みを取りこぼさない。
	if _, err := d.f.ReadAt(p[:PageSize], off); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("storage: ページ %d の読み込み失敗: %w", id, err)
	}
	d.ReadCount++
	return nil
}

func (d *DiskManager) WritePage(id PageID, p Page) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	off := int64(id) * PageSize
	if _, err := d.f.WriteAt(p[:PageSize], off); err != nil {
		return fmt.Errorf("storage: ページ %d の書き込み失敗: %w", id, err)
	}
	if id >= d.numPages {
		d.numPages = id + 1
	}
	d.WriteCount++
	return nil
}

// AllocatePage はファイル末尾に 1 ページ分の領域を確保し、その PageID を返す。
//
// 空きページの再利用 (フリーリスト) は実装していない。
// 本物の RDBMS は削除で空いたページを FSM (PostgreSQL の free space map) 等で
// 管理して再利用するが、ここでは常に末尾追記にして単純さを優先する。
func (d *DiskManager) AllocatePage() (PageID, error) {
	d.mu.Lock()
	id := d.numPages
	d.numPages++
	d.mu.Unlock()

	// ファイルを実際に伸ばしておく。こうしないと、書き込み前に読もうとしたときに
	// 「未割り当て」判定とファイル実サイズがずれる。
	zero := make(Page, PageSize)
	if err := d.WritePage(id, zero); err != nil {
		return 0, err
	}
	return id, nil
}

func (d *DiskManager) NumPages() PageID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.numPages
}

// Sync は OS のページキャッシュ上の内容を実際のディスクまで押し込む。
// 耐久性 (Durability) を保証したい場面ではこれが必要になる。
func (d *DiskManager) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.f.Sync()
}

func (d *DiskManager) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.f.Close()
}

// ResetStats は I/O カウンタを 0 に戻す。
// EXPLAIN ANALYZE で「このクエリだけの I/O」を測るときに、実行直前に呼ぶ。
func (d *DiskManager) ResetStats() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ReadCount, d.WriteCount = 0, 0
}

func (d *DiskManager) Path() string { return d.path }
