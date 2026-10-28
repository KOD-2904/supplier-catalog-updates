# Supplier Catalog Updates (Go)

Hệ thống đồng bộ danh mục sản phẩm từ đối tác cung cấp (Supplier) cho nhân viên cửa hàng bán lẻ, được xây dựng bằng Go với kiến trúc phân tầng sạch, đảm bảo tính bền vững (ACID), khả năng phục hồi sau sự cố (Resumability), và tính lũy đẳng tuyệt đối (Idempotency).

---

## 1. Các Giả Định Thiết Kế (Assumptions)

Dự án được xây dựng dựa trên các giả định nghiệp vụ và kỹ thuật cụ thể sau:

1. **Định danh duy nhất:** Mỗi sản phẩm có một định danh ổn định không đổi do nhà cung cấp cấp phát: `supplier_id`.
2. **Version là tín hiệu chân lý duy nhất:** Mỗi bản ghi sản phẩm có trường `version` là số nguyên dương tăng dần theo từng sản phẩm. `version` là cơ sở duy nhất để xác định dữ liệu mới hay cũ (không sử dụng thời gian nhận của máy tính để tránh lỗi do mạng chậm hay sai lệch đồng hồ).
3. **Phân trang dạng Cursor:** Supplier trả dữ liệu theo từng trang (batch) kèm con trỏ `next_cursor`, và có cờ `done = true` khi đã gửi hết toàn bộ danh mục.
4. **Sự vắng mặt không phải là ngừng bán:** Một sản phẩm không xuất hiện trong feed **KHÔNG** có nghĩa là đã bị ngừng bán (bởi feed có thể phân trang hoặc gửi dạng delta). Chỉ bản ghi có `status = "withdrawn"` mới đánh dấu sản phẩm ngừng bán. Không bao giờ xóa sản phẩm dựa trên sự vắng mặt.
5. **Cơ chế Relist (Bán lại):** Bản ghi `active` có `version` lớn hơn version lúc `withdrawn` sẽ được phép kích hoạt bán lại. Các bản ghi có version nhỏ hơn hoặc bằng version lúc withdraw đều bị chặn.
6. **Bản ghi Withdrawn tối giản:** Bản ghi có trạng thái `withdrawn` chỉ bắt buộc cần `supplier_id` và `version`. Các thông tin khác (tên, giá) không bắt buộc và chỉ mang tính tham khảo.
7. **Đơn tiến trình vận hành:** Một nhân viên vận hành một lần chạy tại một thời điểm. Hệ thống sử dụng *Partial Unique Index* ở tầng database để ngăn ngừa hai tiến trình cùng chạy đồng thời.
8. **Định dạng tiền tệ:** Giá sản phẩm được lưu trữ dưới dạng số nguyên cents (`price_cents`) cùng mã tiền tệ chuẩn (`currency`, ví dụ "USD") để tránh sai số dấu phẩy động.
9. **Phân loại lỗi mạng:**
   - Lỗi tạm thời (*Transient*): Mã HTTP `5xx`, `429`, hoặc đứt kết nối mạng $\rightarrow$ tự động retry với exponential backoff.
   - Lỗi vĩnh viễn (*Permanent*): Mã HTTP `4xx` khác (400, 401, 404), JSON hỏng, hoặc con trỏ `cursor` bị kẹt không tiến $\rightarrow$ dừng run ngay với trạng thái `failed`, không thể resume.

---

## 2. Hướng Dẫn Chạy Các Bài Kiểm Tra (Running Tests)

Toàn bộ hệ thống được kiểm thử tự động với độ bao phủ cao (Unit Tests cho logic Merge thuần túy, Integration Tests cho SQLite và Runner điều phối).

### Yêu cầu môi trường:
- Đã cài đặt **Go 1.22** trở lên (`go version`).
- Không cần cài đặt GCC hay CGO (SQLite sử dụng driver thuần Go `modernc.org/sqlite`).

### Chạy toàn bộ Test Suite:
```bash
go test -v ./...
```

### Chạy kiểm thử kèm đo lường độ bao phủ (Coverage):
```bash
go test -cover ./...
```

### Các nhóm test chính trong dự án:
- `internal/syncer/merge_test.go`: Kiểm thử kiểm tra đầu vào (`Validate`) và toàn bộ 9 trường hợp của bảng quyết định `Merge`, kiểm chứng tính lũy đẳng và ngăn chặn hồi sinh sản phẩm cũ.
- `internal/store/store_test.go`: Kiểm thử tính nguyên tử của Transaction (`CommitBatch`), kiểm thử SQLite WAL, kiểm thử ràng buộc chỉ 1 run chạy tại một thời điểm, và lớp bảo vệ chống hạ version ở tầng SQL.
- `internal/supplier/supplier_test.go`: Kiểm thử HTTP client với mock server, phân loại lỗi `TransientError` vs `PermanentError`.
- `internal/syncer/runner_test.go`: Kiểm thử toàn bộ vòng đời (start, resume từ cursor dở dang sau khi phục hồi outage, xử lý dữ liệu lỗi, bảo vệ chống kẹt cursor).

---

## 3. Hướng Dẫn Chạy Ứng Dụng (Running Application)

Người kiểm tra có thể chọn **chạy tự động bằng 1 lệnh script** (khuyên dùng) hoặc **chạy thủ công từng lệnh**.

### Cách 1: Chạy Demo Tự Động Toàn Trình (1 Lệnh duy nhất)


Script demo sẽ tự động: biên dịch mã nguồn $\rightarrow$ bật mock supplier $\rightarrow$ đồng bộ lần 1 $\rightarrow$ kiểm tra tính lũy đẳng lần 2 $\rightarrow$ giả lập sự cố sập server 503 $\rightarrow$ phục hồi $\rightarrow$ resume hoàn tất $\rightarrow$ dọn dẹp tiến trình.

- **Trên Windows (PowerShell):**
  ```powershell
  powershell -ExecutionPolicy Bypass -File .\scripts\demo.ps1
  ```

- **Trên Linux / macOS (Bash):**
  ```bash
  chmod +x ./scripts/demo.sh
  ./scripts/demo.sh
  ```

---

### Cách 2: Chạy Thủ Công Từng Bước Bằng CLI

#### Bước 1: Biên dịch các file thực thi

- **Trên Windows (PowerShell / CMD):**
  ```powershell
  go build -o bin/catalogsync.exe ./cmd/catalogsync
  go build -o bin/mocksupplier.exe ./cmd/mocksupplier
  ```

- **Trên Linux / macOS:**
  ```bash
  go build -o bin/catalogsync ./cmd/catalogsync
  go build -o bin/mocksupplier ./cmd/mocksupplier
  ```

> [!NOTE]
> Trên hệ điều hành Windows, các file chạy bắt buộc phải có đuôi `.exe` để Windows nhận diện là chương trình thực thi (tránh bị hệ thống hỏi "mở bằng ứng dụng gì").

---

#### Bước 2: Khởi động Mock Supplier (Cửa sổ Terminal 1)

Mở **Terminal thứ nhất** và chạy server giả lập cung cấp dữ liệu:

- **Windows:**
  ```powershell
  .\bin\mocksupplier.exe -port 8080 -scenario normal
  ```
- **Linux / macOS:**
  ```bash
  ./bin/mocksupplier -port 8080 -scenario normal
  ```
*Server sẽ chạy và lắng nghe tại `http://localhost:8080` (giữ nguyên cửa sổ terminal này).*

---

#### Bước 3: Thao tác bằng công cụ `catalogsync` (Cửa sổ Terminal 2)

Mở **Terminal thứ hai** tại thư mục dự án để chạy các lệnh quản lý danh mục:

**1. Bắt đầu một lần đồng bộ mới:**
- Windows:
  ```powershell
  .\bin\catalogsync.exe -db catalog.db -supplier http://localhost:8080 start
  ```
- Linux / macOS:
  ```bash
  ./bin/catalogsync -db catalog.db -supplier http://localhost:8080 start
  ```
*Kết quả sẽ hiển thị bảng tóm tắt: Created: 120, Updated: 24 (sản phẩm v2), Withdrawn: 7 (sản phẩm v3).*

**2. Kiểm tra tính lũy đẳng (chạy lại feed cũ lần 2):**
- Windows: `.\bin\catalogsync.exe -db catalog.db start`
- Linux / macOS: `./bin/catalogsync -db catalog.db start`
*Kết quả: Created: 0, Updated: 0, Unchanged: 120 $\rightarrow$ chứng minh không sinh bản ghi trùng lặp.*

**3. Xem tình trạng lần chạy gần nhất:**
- Windows: `.\bin\catalogsync.exe -db catalog.db status` 
- Linux / macOS: `./bin/catalogsync -db catalog.db status`

**4. Xem báo cáo chi tiết (kèm log thay đổi và danh sách problems):**
- Windows: `.\bin\catalogsync.exe -db catalog.db report`
- Linux / macOS: `./bin/catalogsync -db catalog.db report`

**5. Tra cứu danh sách sản phẩm trong catalog:**
- Windows:
  ```powershell
  # Xem toàn bộ
  .\bin\catalogsync.exe -db catalog.db products

  # Chỉ xem sản phẩm đang bán
  .\bin\catalogsync.exe -db catalog.db products -status active

  # Chỉ xem sản phẩm đã ngừng bán (tombstone)
  .\bin\catalogsync.exe -db catalog.db products -status withdrawn
  ```
- Linux / macOS:
  ```bash
  # Xem toàn bộ
  ./bin/catalogsync -db catalog.db products

  # Chỉ xem sản phẩm đang bán
  ./bin/catalogsync -db catalog.db products -status active

  # Chỉ xem sản phẩm đã ngừng bán (tombstone)
  ./bin/catalogsync -db catalog.db products -status withdrawn
  ```

---

### Cách 3: Thử Nghiệm Sự Cố Mạng (Outage) & Khả Năng Tiếp Tục (Resume)

1. Khởi động Mock Supplier giả lập sự cố (trả lỗi 503 sau 2 batch):
   - Windows:
     ```powershell
     .\bin\mocksupplier.exe -port 8080 -scenario normal -outage-after 2 -outage-for 5m
     ```
   - Linux / macOS:
     ```bash
     ./bin/mocksupplier -port 8080 -scenario normal -outage-after 2 -outage-for 5m
     ```

2. Bắt đầu đồng bộ:
   - Windows:
     ```powershell
     .\bin\catalogsync.exe -db outage.db -retries 2 -backoff 100ms start
     ```
   - Linux / macOS:
     ```bash
     ./bin/catalogsync -db outage.db -retries 2 -backoff 100ms start
     ```
   *Ứng dụng xử lý xong 2 batch đầu, gặp lỗi 503, tự động retry rồi dừng lại với trạng thái `interrupted`. Dữ liệu 2 batch trước vẫn an toàn 100%.*

3. Phục hồi Supplier:
   - Windows:
     ```powershell
     Invoke-RestMethod -Uri "http://localhost:8080/admin/recover" -Method Post
     ```
   - Linux / macOS:
     ```bash
     curl -X POST http://localhost:8080/admin/recover
     ```

4. Tiếp tục lần chạy bị gián đoạn:
   - Windows:
     ```powershell
     .\bin\catalogsync.exe -db outage.db resume
     ```
   - Linux / macOS:
     ```bash
     ./bin/catalogsync -db outage.db resume
     ```
   *Ứng dụng tiếp tục chính xác từ cursor của batch 2 và hoàn tất toàn bộ danh mục.*

---

### Cách 4: Thử Nghiệm Xử Lý Dữ Liệu Lỗi & Giải Quyết Sự Cố

1. Khởi động supplier với dữ liệu lỗi và xung đột version:
   - Windows:
     ```powershell
     .\bin\mocksupplier.exe -port 8080 -scenario "invalid,conflict"
     ```
   - Linux / macOS:
     ```bash
     ./bin/mocksupplier -port 8080 -scenario "invalid,conflict"
     ```

2. Chạy đồng bộ:
   - Windows:
     ```powershell
     .\bin\catalogsync.exe -db dirty.db start
     ```
   - Linux / macOS:
     ```bash
     ./bin/catalogsync -db dirty.db start
     ```
   *Ứng dụng bỏ qua các item lỗi, lưu các item hợp lệ, và kết thúc với trạng thái `completed_with_problems`.*

3. Xem danh sách sự cố cần con người xử lý:
   - Windows:
     ```powershell
     .\bin\catalogsync.exe -db dirty.db problems
     ```
   - Linux / macOS:
     ```bash
     ./bin/catalogsync -db dirty.db problems
     ```

4. Đánh dấu đã xử lý một sự cố:
   - Windows:
     ```powershell
     .\bin\catalogsync.exe -db dirty.db resolve <problem-id>
     ```
   - Linux / macOS:
     ```bash
     ./bin/catalogsync -db dirty.db resolve <problem-id>
     ```

---

## 4. Bảng Tra Cứu Lệnh CLI & Tham Số

```
catalogsync [-db catalog.db] [-supplier URL] [-retries 3] [-backoff 200ms] [-batch-size 20] [-v] <command>
```

| Lệnh (Command) | Tham số tùy chọn | Ý nghĩa |
|---|---|---|
| `start` (hoặc `sync start`) | | Bắt đầu một run đồng bộ mới |
| `resume` (hoặc `sync resume`) | `[run-id]` | Tiếp tục run bị gián đoạn (mặc định lấy run mới nhất) |
| `status` | `[run-id]` | Xem trạng thái, tiến độ, số đếm của run |
| `report` | `[run-id]` | Báo cáo đầy đủ: status + chi tiết thay đổi + problems |
| `products` | `-status active\|withdrawn` | Liệt kê danh sách sản phẩm trong kho |
| `problems` | `-run [run-id]` `-all` | Liệt kê các sự cố (mặc định chỉ hiện sự cố chưa giải quyết) |
| `resolve` | `<problem-id>` | Đánh dấu sự cố đã được con người xử lý |

---

## 5. Kiến Trúc & Các Bất Biến Cốt Lõi (Architecture & Invariants)

### Sơ đồ luồng dữ liệu phân tầng:
```
CLI (cmd/catalogsync)
      |
      +--> internal/syncer (Runner điều phối + Merge hàm thuần)
              |        |
              |        +--> internal/store (SQLite WAL, ACID Transaction)
              |
              +--> internal/supplier (HTTP Client có Retry Backoff)
                        |
                        +--> cmd/mocksupplier (Giả lập supplier kiểm thử)
```

### 6 Bất Biến Cốt Lõi Đã Được Đảm Bảo:
1. **Idempotency (Tính lũy đẳng):** Nhập cùng một dữ liệu bao nhiêu lần cũng không tạo sản phẩm trùng lặp hay làm sai lệch số liệu.
2. **Monotonic Versioning:** Dữ liệu có version thấp hơn version đang lưu trong database không bao giờ được phép ghi đè lên dữ liệu mới hơn (bảo vệ 2 lớp: ở hàm `Merge` và ở câu lệnh SQL `WHERE excluded.version >= products.version`).
3. **Tombstone Protection:** Sản phẩm `withdrawn` được lưu dạng tombstone; dữ liệu `active` cũ đến muộn không thể làm sống lại sản phẩm đã ngừng bán.
4. **Crash & Fault Durability:** Toàn bộ sản phẩm, lịch sử thay đổi, sự cố, bộ đếm và con trỏ `next_cursor` của 1 batch được gom trong **MỘT transaction duy nhất**. Crash giữa chừng không bao giờ để lại dữ liệu dở dang.
5. **Resumability:** Run bị gián đoạn (`interrupted`) được tiếp tục mượt mà từ cursor đã commit gần nhất.
6. **Strict Completion State:** Trạng thái `completed` (hoặc `completed_with_problems`) được ghi **ngay trong cùng transaction của batch cuối cùng**. Run chưa tải xong toàn bộ không bao giờ được báo là hoàn tất.

---

## 6. Sử Dụng Công Cụ AI (AI Tools Usage)

Xem chi tiết tại tệp **[AI-USAGE.md](AI-USAGE.md)**.

> **Tóm tắt:** Trong dự án này, ứng viên sử dụng Claude và Trợ lý lập trình AI như một cộng sự (pair programmer) để tăng tốc độ hiện thực, trong khi ứng viên giữ vai trò Kiến trúc sư trưởng (Lead Architect): tự tay thiết lập 6 bất biến cốt lõi, xác định mô hình dữ liệu, phân loại ngoại lệ mạng, kiểm toán chất lượng code, trực tiếp sửa các lỗi phát sinh (Windows `.exe`, trùng cờ CLI), và xây dựng các kịch bản kiểm thử tự động toàn trình.



