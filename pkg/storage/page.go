package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PageSize はディスク上の 1 ページのバイト数。
// 実在の RDBMS も 4KB〜16KB を使う (PostgreSQL: 8KB, MySQL/InnoDB: 16KB, SQLite: 4KB)。
// OS のページサイズやファイルシステムのブロックサイズと揃えると、
// 1 ページの読み書きが 1 回の物理 I/O に対応しやすくなる。
const PageSize = 4096

// PageID はデータファイル先頭からのページ番号。ファイル上のバイト位置は PageID * PageSize。
type PageID uint32

// InvalidPageID は「ページが存在しない」ことを表す番兵値。
// 連結リストの終端 (next が無い) などに使う。
const InvalidPageID PageID = 0xFFFFFFFF

type PageType uint8

const (
	PageTypeInvalid PageType = iota
	PageTypeMeta
	PageTypeHeap          // テーブルの実データ (タプル置き場)
	PageTypeBTreeInternal // B+Tree の内部ノード
	PageTypeBTreeLeaf     // B+Tree の葉ノード
)

func (t PageType) String() string {
	switch t {
	case PageTypeMeta:
		return "Meta"
	case PageTypeHeap:
		return "Heap"
	case PageTypeBTreeInternal:
		return "BTreeInternal"
	case PageTypeBTreeLeaf:
		return "BTreeLeaf"
	default:
		return "Invalid"
	}
}

// ページヘッダ内の各フィールドのバイトオフセット。
// 「どのバイトが何を意味するか」を自分で決めるのがストレージ設計の出発点。
const (
	offPageType   = 0  // u8  ページ種別
	offLevel      = 1  // u8  B+Tree の階層 (葉 = 0)。ヒープページでは未使用
	offNumSlots   = 2  // u16 スロット数
	offFreeStart  = 4  // u16 空き領域の先頭 = スロット配列の終端
	offFreeEnd    = 6  // u16 空き領域の終端 = タプル領域の先頭
	offNextPageID = 8  // u32 次ページ (ヒープの連結リスト / 葉ノードの右兄弟)
	offLSN        = 12 // u32 将来の WAL 用。現状は常に 0

	PageHeaderSize = 16
	SlotSize       = 4 // offset(u16) + length(u16)
)

var (
	ErrPageFull      = errors.New("storage: ページに空き領域が無い")
	ErrSlotNotFound  = errors.New("storage: スロットが存在しない")
	ErrSlotDeleted   = errors.New("storage: スロットは削除済み")
	ErrTupleTooLarge = errors.New("storage: タプルが 1 ページに収まらない")
)

// Page は 4KB の生バイト列そのもの。
// 構造体にマッピングせず []byte のまま扱うことで、
// 「ディスク上のレイアウト = メモリ上のレイアウト」を保ち、
// 書き出しが単なる memcpy で済むようにしている。
//
// スロット式ページ (slotted page) のレイアウト:
//
//	+--------------------------------+ 0
//	| PageHeader (16B)               |
//	+--------------------------------+ 16
//	| Slot[0] | Slot[1] | Slot[2] ...|  ← 前から伸びる
//	+--------------------------------+ freeStart
//	|                                |
//	|          空き領域               |
//	|                                |
//	+--------------------------------+ freeEnd
//	| ... | Tuple[1] | Tuple[0]      |  ← 後ろから伸びる
//	+--------------------------------+ PageSize
//
// スロットを前から、タプルを後ろから詰めるのは、
// 「どちらがどれだけ増えるか事前に決めなくていい」ため。
// 両者が出会った時点でそのページは満杯。
//
// タプルの物理位置がスロット経由の間接参照になっているので、
// ページ内でタプルを移動 (デフラグ) しても、外部が持つ (PageID, SlotID)
// という識別子は変わらない。これが RID (Record ID) の安定性を支えている。
type Page []byte

func NewPage() Page {
	return make(Page, PageSize)
}

// Init はページをまっさらな状態に初期化する。
func (p Page) Init(t PageType) {
	for i := range p {
		p[i] = 0
	}
	p.SetType(t)
	p.setNumSlots(0)
	p.setFreeStart(PageHeaderSize)
	p.setFreeEnd(PageSize)
	p.SetNextPageID(InvalidPageID)
}

// ---- ヘッダのアクセサ ----

func (p Page) Type() PageType     { return PageType(p[offPageType]) }
func (p Page) SetType(t PageType) { p[offPageType] = byte(t) }

func (p Page) Level() uint8     { return p[offLevel] }
func (p Page) SetLevel(l uint8) { p[offLevel] = l }

func (p Page) NumSlots() int      { return int(binary.LittleEndian.Uint16(p[offNumSlots:])) }
func (p Page) setNumSlots(n int)  { binary.LittleEndian.PutUint16(p[offNumSlots:], uint16(n)) }
func (p Page) freeStart() int     { return int(binary.LittleEndian.Uint16(p[offFreeStart:])) }
func (p Page) setFreeStart(v int) { binary.LittleEndian.PutUint16(p[offFreeStart:], uint16(v)) }
func (p Page) freeEnd() int       { return int(binary.LittleEndian.Uint16(p[offFreeEnd:])) }
func (p Page) setFreeEnd(v int)   { binary.LittleEndian.PutUint16(p[offFreeEnd:], uint16(v)) }

func (p Page) NextPageID() PageID {
	return PageID(binary.LittleEndian.Uint32(p[offNextPageID:]))
}
func (p Page) SetNextPageID(id PageID) {
	binary.LittleEndian.PutUint32(p[offNextPageID:], uint32(id))
}

// FreeSpace はいま挿入に使える生の空きバイト数。
// 実際に 1 件挿入するには、さらにスロット 1 個分 (SlotSize) が必要。
func (p Page) FreeSpace() int { return p.freeEnd() - p.freeStart() }

// MaxTupleSize は 1 ページに入りうるタプルの上限。
// これを超えるタプルは、本物の RDBMS では TOAST (PostgreSQL) や
// オーバーフローページ (SQLite) に追い出される。minidb では単にエラーにする。
const MaxTupleSize = PageSize - PageHeaderSize - SlotSize

// ---- スロットのアクセサ ----

func (p Page) slotPos(i int) int { return PageHeaderSize + i*SlotSize }

func (p Page) slot(i int) (offset, length int) {
	pos := p.slotPos(i)
	return int(binary.LittleEndian.Uint16(p[pos:])), int(binary.LittleEndian.Uint16(p[pos+2:]))
}

func (p Page) setSlot(i, offset, length int) {
	pos := p.slotPos(i)
	binary.LittleEndian.PutUint16(p[pos:], uint16(offset))
	binary.LittleEndian.PutUint16(p[pos+2:], uint16(length))
}

// Insert はタプルを 1 件追加し、割り当てられたスロット番号を返す。
// 空きが足りなければ ErrPageFull。呼び出し側は次のページを用意する。
func (p Page) Insert(data []byte) (int, error) {
	if len(data) > MaxTupleSize {
		return 0, fmt.Errorf("%w: %d bytes (最大 %d)", ErrTupleTooLarge, len(data), MaxTupleSize)
	}
	// スロット 1 個 + データ本体が空き領域に収まるか。
	if p.FreeSpace() < len(data)+SlotSize {
		return 0, ErrPageFull
	}

	newFreeEnd := p.freeEnd() - len(data)
	copy(p[newFreeEnd:], data)
	p.setFreeEnd(newFreeEnd)

	slotID := p.NumSlots()
	p.setSlot(slotID, newFreeEnd, len(data))
	p.setNumSlots(slotID + 1)
	p.setFreeStart(p.slotPos(slotID + 1))

	return slotID, nil
}

// Get はスロット番号でタプルを取り出す。
// 返るのはページ内部を指すスライスなので、ページを unpin した後は触ってはいけない。
// 保持したい場合は Clone を使う。
func (p Page) Get(slotID int) ([]byte, error) {
	if slotID < 0 || slotID >= p.NumSlots() {
		return nil, fmt.Errorf("%w: slot=%d numSlots=%d", ErrSlotNotFound, slotID, p.NumSlots())
	}
	offset, length := p.slot(slotID)
	if length == 0 {
		return nil, ErrSlotDeleted
	}
	return p[offset : offset+length], nil
}

// Clone はページに依存しないコピーを返す。
func (p Page) Clone(slotID int) ([]byte, error) {
	b, err := p.Get(slotID)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// Delete はスロットを墓標 (tombstone) にする。
// length=0 が「削除済み」の印。データ本体の領域はここでは回収しない。
//
// 本物の RDBMS もこうする。削除のたびにページを詰め直すのは高価すぎるので、
// 空間の回収は VACUUM (PostgreSQL) やページ分割時のデフラグにまとめて任せる。
// 「DELETE してもファイルが小さくならない」のはこの設計が理由。
func (p Page) Delete(slotID int) error {
	if slotID < 0 || slotID >= p.NumSlots() {
		return fmt.Errorf("%w: slot=%d", ErrSlotNotFound, slotID)
	}
	if _, length := p.slot(slotID); length == 0 {
		return ErrSlotDeleted
	}
	p.setSlot(slotID, 0, 0)
	return nil
}

// LiveTuples は削除済みを除いた実際のタプル数。
func (p Page) LiveTuples() int {
	n := 0
	for i := 0; i < p.NumSlots(); i++ {
		if _, length := p.slot(i); length > 0 {
			n++
		}
	}
	return n
}

func (p Page) String() string {
	return fmt.Sprintf("Page{type=%s slots=%d live=%d free=%dB next=%d}",
		p.Type(), p.NumSlots(), p.LiveTuples(), p.FreeSpace(), p.NextPageID())
}
