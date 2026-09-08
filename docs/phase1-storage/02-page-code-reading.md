# page.go の読み方

> [01-concepts.md](01-concepts.md) の図が、コードのどの行に対応するかを照合するドキュメント。
> 先に 01 を読んで図を頭に入れてから、こちらを読む。

`page.go` は 238 行あるが、**実質的な処理は `Insert` / `Get` / `Delete` の 3 つだけ**。
残りは定数と、バイト列を数値に変換するだけのアクセサ。

---

## 1. 一番大事な 1 行

[`pkg/storage/page.go:95`](../../pkg/storage/page.go#L95)

```go
type Page []byte
```

ページは **ただのバイト配列**。

普通に設計するなら、こう書きたくなる。

```go
// こうしなかった
type Page struct {
    Type      PageType
    NumSlots  uint16
    FreeStart uint16
    Slots     []Slot
    Tuples    [][]byte
}
```

読みやすいのになぜやらないのか。**この構造体はディスクに書けないから**。

```
  構造体をディスクに書くには…

  Page struct  --[シリアライズ処理]-->  バイト列  --> ディスク
  Page struct  <--[デシリアライズ]---   バイト列  <-- ディスク
                      ^
              ページを読むたびに毎回この変換が走る
```

ページは 1 秒間に何万回も読まれる。そのたびに変換していたら話にならない。

`type Page []byte` にしておけば、**ディスク上のバイト列とメモリ上のバイト列が完全に同じもの**になる。
読み込みは「4096 バイトをそのままコピーする」だけ。変換ゼロ。

> **`page.go` の全ての関数は「このバイト配列の何番目をどう読むか」を書いているだけ。**

### Go 文法メモ: なぜ書き換えられるのか

引っかかるはずのポイント。

```go
func (p Page) SetType(t PageType) { p[offPageType] = byte(t) }
```

`p` はポインタ (`*Page`) ではなく **値** なのに、なぜ書き換えが呼び出し元に反映されるのか。

Go のスライスの正体がこれ。

```
  p := make([]byte, 4096)

  p の中身は「4096 バイトの実データ」ではなく、こういう小さな 3 点セット:

     +----------------------+
     | ptr  -> 実データの先頭 |    <- ここが本体を指している
     | len  = 4096          |
     | cap  = 4096          |
     +----------------------+

  値としてコピーしても、コピーされるのは上の 3 つだけ。
  ptr は同じ場所を指したまま。
  -> p[0] = x は、元の配列そのものを書き換える
```

TypeScript の配列を関数に渡しても中身が書き換わるのと同じ感覚。

---

## 2. 定規: オフセット定数

[`pkg/storage/page.go:49-60`](../../pkg/storage/page.go#L49)

```go
const (
	offPageType   = 0  // u8
	offLevel      = 1  // u8
	offNumSlots   = 2  // u16
	offFreeStart  = 4  // u16
	offFreeEnd    = 6  // u16
	offNextPageID = 8  // u32
	offLSN        = 12 // u32

	PageHeaderSize = 16
)
```

これが **定規**。ヘッダ (先頭 16 バイト) の内訳。

```
  バイト位置  0    1    2    3    4    5    6    7    8 .. 11   12 .. 15
            |----|----|---------|---------|---------|----------|----------|
            type level numSlots  freeStart  freeEnd   nextPageID    LSN
             1B   1B     2B         2B        2B         4B         4B
            <------------------------ 16 バイト ------------------------->
```

`u8` は 1 バイト、`u16` は 2 バイト、`u32` は 4 バイト。
だから `0, 1, 2, 4, 6, 8, 12` と積み上がって合計 16。

**この定数表がなければ、`page.go` の他のコードは全部ただのゴミ。**
バイト列に意味を与えているのはここだけ。

---

## 3. バイト列と数値の変換: アクセサ

[`pkg/storage/page.go:121-126`](../../pkg/storage/page.go#L121)

```go
func (p Page) NumSlots() int     { return int(binary.LittleEndian.Uint16(p[offNumSlots:])) }
func (p Page) setNumSlots(n int) { binary.LittleEndian.PutUint16(p[offNumSlots:], uint16(n)) }
```

やっていることは 1 つだけ。

```
  p[2:] は「2 バイト目から先を見る窓」(Go のスライス式)

  ページ:  [ 02 ][ 00 ][ 03 ][ 00 ][ 18 ][ 00 ] ...
            0     1     2     3     4     5

  p[2:]  ->            [ 03 ][ 00 ][ 18 ][ 00 ] ...

  Uint16(...) は、その窓の先頭 2 バイトだけを読んで数値にする
                       ^^^^^^^^^^^^
                       = 3
```

### LittleEndian とは

2 バイト以上の数値を、**どちらの端から並べるか** の流儀。

```
  数値 300 を 2 バイトで表す。300 = 0x012C

  BigEndian    : [ 01 ][ 2C ]    人間が読む順 (上の桁が先)
  LittleEndian : [ 2C ][ 01 ]    下の桁が先   <- minidb はこっち
```

なぜ LittleEndian か。**x86 と ARM の CPU がそう並べているから**。
CPU のメモリ表現とファイル上の表現を揃えておけば、読み書きで並べ替えが要らない。

### Go 文法メモ: 大文字と小文字

```go
func (p Page) NumSlots() int      // 大文字 -> 他のパッケージから使える (public)
func (p Page) setNumSlots(n int)  // 小文字 -> このパッケージ内だけ (private)
```

Go には `public` / `private` キーワードが無い。**1 文字目の大小がそのままアクセス修飾子**。

`NumSlots()` は読み取りなので公開、`setNumSlots()` は書き込みなので非公開。
**外部からヘッダを直接書き換えられないようにしている。**

---

## 4. 付箋の読み書き

[`pkg/storage/page.go:146-157`](../../pkg/storage/page.go#L146)

```go
func (p Page) slotPos(i int) int { return PageHeaderSize + i*SlotSize }
```

**i 番目の付箋がページのどこにあるか** を計算する。`16 + i*4`。

```
  バイト位置  16      20      24      28
            |-------|-------|-------|-------|
            | Slot0 | Slot1 | Slot2 | Slot3 |
            |-------|-------|-------|-------|

  slotPos(0) = 16 + 0*4 = 16
  slotPos(1) = 16 + 1*4 = 20
  slotPos(2) = 16 + 2*4 = 24

  1 枚の付箋の中身 (4 バイト):
     +--------+--------+
     | offset | length |
     |  u16   |  u16   |
     +--------+--------+
```

**付箋が固定長 (4 バイト) だから、この掛け算だけで場所が分かる。**
これが「スロット番号で O(1) アクセスできる」理由。

---

## 5. Insert: ここが図そのもの

[`pkg/storage/page.go:161-180`](../../pkg/storage/page.go#L161)

空のページに `"alice"` (5 バイト) を入れるところを、**実際の数値で追う**。

**開始状態:** `numSlots=0, freeStart=16, freeEnd=4096`

```go
if p.FreeSpace() < len(data)+SlotSize {     // 166 行
    return 0, ErrPageFull
}
```

```
  FreeSpace() = freeEnd - freeStart = 4096 - 16 = 4080
  必要な量    = 5 (データ) + 4 (付箋) = 9
  4080 >= 9  -> OK
```

> **`+ SlotSize` を忘れないこと。** データだけでなく、付箋の 4 バイトも消費する。

```go
newFreeEnd := p.freeEnd() - len(data)       // 170 行
copy(p[newFreeEnd:], data)                  // 171 行
p.setFreeEnd(newFreeEnd)                    // 172 行
```

```
  newFreeEnd = 4096 - 5 = 4091
  4091 バイト目から "alice" を書く

  0      16                                        4091   4096
  |------|-------------------------------------------|-----|
  |header|                 free                      |alice|
  |------|-------------------------------------------|-----|
                                                     ^
                                              freeEnd = 4091
```

```go
slotID := p.NumSlots()                      // 174 行
p.setSlot(slotID, newFreeEnd, len(data))    // 175 行
p.setNumSlots(slotID + 1)                   // 176 行
p.setFreeStart(p.slotPos(slotID + 1))       // 177 行
```

```
  slotID = 0
  setSlot(0, 4091, 5)  -> 16〜19 バイト目に付箋を書く
  setNumSlots(1)
  setFreeStart(slotPos(1)) = 16 + 1*4 = 20

  0      16    20                                  4091   4096
  |------|-----|-------------------------------------|-----|
  |header|Slot0|              free                   |alice|
  |------|-----|-------------------------------------|-----|
              ^
        freeStart = 20
```

**01-concepts.md §6 の図と 1 バイトも違わない。**
`Insert` がやっているのは、図の矢印を内側に動かすことだけ。

続けて `"bob"` (3 バイト) を入れると:

```
  FreeSpace  = 4091 - 20 = 4071 >= 3+4       OK
  newFreeEnd = 4091 - 3  = 4088              4088〜4090 に bob
  slotID     = 1
  setSlot(1, 4088, 3)                        20〜23 に付箋
  setFreeStart(16 + 2*4) = 24

  0      16    20    24                       4088  4091   4096
  |------|-----|-----|---------------------------|-----|-----|
  |header|Slot0|Slot1|          free             | bob |alice|
  |------|-----|-----|---------------------------|-----|-----|
```

---

## 6. Get と Delete

[`pkg/storage/page.go:185-194`](../../pkg/storage/page.go#L185)

```go
offset, length := p.slot(slotID)     // 付箋を見る
if length == 0 {
    return nil, ErrSlotDeleted       // 墓標だった
}
return p[offset : offset+length], nil
```

**付箋を見て、書いてある場所を切り出すだけ。**
`Slot1 = (4088, 3)` なら `p[4088:4091]` を返す。
01-concepts.md §6 の「付箋を経由する」が、そのままこの 2 行。

ただし [`page.go:183`](../../pkg/storage/page.go#L183) のコメントが重要。

```
  返るのは「ページ内部を指す窓」であって、コピーではない

     戻り値 -----> ページのバイト配列の 4088〜4090

  このページがバッファプールから追い出されると、
  同じ場所が別のページに使い回される -> 中身が静かにすり替わる
```

これが 01-concepts.md §7 の pin/unpin の話と直結する。
だから [`Clone()`](../../pkg/storage/page.go#L197) が別に用意されている。

**Delete** は [`page.go:220`](../../pkg/storage/page.go#L220) の 1 行。

```go
p.setSlot(slotID, 0, 0)
```

付箋を `(0, 0)` にするだけ。データ本体には **一切触らない**。
`"bob"` の 3 バイトは 4088 番地に残り続ける。

---

## 7. 最後に、気づいてほしい粗

コードを読むと、こんな疑問が湧くはず。

> `freeStart` って、わざわざ保存する必要ある？

その通りで、**冗長**。

```
  freeStart = PageHeaderSize + numSlots * SlotSize
            = 16 + numSlots * 4

  numSlots から必ず計算できる。2 バイト無駄にしている。
```

実は **PostgreSQL も同じことをしている** (`pd_lower` と `pd_upper` を両方持つ)。
理由は、ページの種類によってヘッダ以降のレイアウトが変わりうるから。
B+Tree のノードなど、スロット配列以外のものが先頭に来る設計にしたとき、
`freeStart` を明示的に持っていれば計算式を書き換えずに済む。

**2 バイト払って、レイアウトへの依存を消している。**
こういうトレードオフを自分で判断できるようになるのが、自作の一番の収穫。

---

## まとめ

| 節 | コード | 図との対応 |
|---|---|---|
| 1 | `type Page []byte` | ページ = 4KB のバイト配列そのもの |
| 2 | オフセット定数 | ヘッダ 16 バイトの内訳 |
| 3 | アクセサ | バイト列 <-> 数値の変換 |
| 4 | `slotPos` / `slot` / `setSlot` | 付箋の読み書き |
| 5 | `Insert` | 図の矢印を内側に動かす |
| 6 | `Get` / `Delete` | 付箋を経由する / 墓標にする |
| 7 | `freeStart` の冗長さ | 設計上のトレードオフ |
