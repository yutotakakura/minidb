# minidb

[![CI](https://github.com/yutotakakura/minidb/actions/workflows/ci.yml/badge.svg)](https://github.com/yutotakakura/minidb/actions/workflows/ci.yml)

実行計画（EXPLAIN）とインデックスを原理から理解するために、Go で RDBMS を一から作るプロジェクト。

性能を出すためではなく、**内部で何が起きているかを見えるようにするために**作っている。
そのため、普通の DB なら隠す「触ったページ数」を常に数えていて、`EXPLAIN ANALYZE` がそれをそのまま表示する。

## ドキュメント

| | 内容 |
|---|---|
| [docs/00-overview.md](docs/00-overview.md) | 目的・全体アーキテクチャ・Phase ロードマップ・技術選定 |
| [docs/01-storage.md](docs/01-storage.md) | Phase 1: ディスクとページ（前提知識から） |
| [docs/02-page-code-reading.md](docs/02-page-code-reading.md) | `page.go` の読み方（図とコードの照合） |

## 動かし方

```bash
go test ./... -v
```

## 進捗

- [x] Phase 1 — スロット式ページ / DiskManager / バッファプール
- [ ] Phase 2 — スキーマ・タプル符号化 / ヒープファイル / 全表走査
- [ ] Phase 3 — B+Tree インデックス
- [ ] Phase 4 — SQL レキサ / パーサ
- [ ] Phase 5 — 論理プラン → オプティマイザ → 物理プラン
- [ ] Phase 6 — Volcano イテレータ実行器
- [ ] Phase 7 — EXPLAIN / EXPLAIN ANALYZE
- [ ] Phase 8 — REPL
