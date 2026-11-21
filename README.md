# bgraph

Chọn test bị ảnh hưởng bởi một thay đổi, đa ngôn ngữ, **đóng khi lỗi** (fail-closed): với mọi thay đổi, đầu ra hoặc chứa
mọi test có thể bị ảnh hưởng, hoặc nới phạm vi (dự án, toàn bộ) kèm lý do máy đọc được. Không bao giờ âm thầm bỏ sót.

Trạng thái: hệ sinh thái Node (JS/TS, CJS/ESM, Jest/Vitest/Mocha/node:test/Playwright). Hệ khác chưa hỗ trợ: thay đổi ở
đó nới ra `full`.

## Dùng

```sh
go build -o bgraph ./cmd/bgraph

bgraph affected --base origin/main          # test bị ảnh hưởng bởi cây làm việc so với base
bgraph affected --base A --head B --json    # giữa hai commit, JSON
bgraph command --base origin/main           # lệnh chạy theo runner (jest --runTestsByPath, vitest run ...)
bgraph explain src/a.test.ts                # vì sao test được chọn
bgraph unresolved                           # import chưa phân giải, kèm luật bgraph.toml gợi ý
```

Thư viện Go: `bgraph.Open(ctx, root, bgraph.Options{})` rồi `idx.Select(ctx, bgraph.Change{Base: "origin/main", Worktree: true})`.

Cấu hình tuỳ chọn `bgraph.toml` ở gốc repo: khai phụ thuộc công cụ không suy ra được (`[[rule]]`), đầu ra sinh mã
(`[[generated]]`). Cache nằm trong thư mục cache của người dùng, không ghi vào repo.

## Giấy phép

Apache License 2.0 — xem `LICENSE` và `THIRD_PARTY_LICENSES`.
