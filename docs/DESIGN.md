# Desain Agentium: belajar dari otak, alam, dan fisika

Dokumen ini menjelaskan prinsip di balik Agentium. Setiap inspirasi (otak manusia, hukum alam, fisika) diterjemahkan jadi **mekanisme yang benar-benar ada di kode dan diuji**. Yang hanya metafora ditandai sebagai metafora. Kami sengaja **tidak** memakai istilah seperti "kuantum" untuk hal yang bukan kuantum.

Aturan yang dipakai: sebuah ide hanya masuk kalau (1) ada bukti eksternal atau alasan teknik yang kuat, (2) tidak menambah token prompt secara berarti, dan (3) bisa diuji.

---

## 1. Otak manusia

| Prinsip di otak | Mekanisme di Agentium | Letak di kode |
|---|---|---|
| **Memori kerja** (korteks prefrontal: kapasitas kecil, isinya dijaga aktif) | *Ledger*: file dibaca/diubah, 10 perintah terakhir + exit code, error yang belum beres, dan daftar `todo`. Dicatat oleh harness (bukan diingat model) dan dibawa utuh saat konteks diringkas | `internal/agent/ledger.go`, `todo.go`, `context.go` |
| **Hipokampus → neokorteks** (pengalaman harian di-*replay* lalu dikonsolidasi jadi memori jangka panjang) | Jurnal per giliran (deterministik, tanpa LLM). Indeks BM25 atas jurnal, keputusan, dan sesi lama. `agentium tidy` untuk konsolidasi | `internal/memory` |
| **Belajar dari *prediction error*** (dopamin: yang paling diingat adalah kesalahan yang berhasil diperbaiki) | *Pelajaran* error→perbaikan: saat perintah yang gagal lalu lulus, dicatat apa yang gagal dan file apa yang diubah | `ledger.go` (`Lessons`) |
| **Long-term potentiation** (yang berulang menguat) | Pelajaran yang kegagalannya sudah pernah terjadi di giliran sebelumnya dinaikkan ke `MEMORY.md`. Kejadian sekali tetap di jurnal | `memory.go` (`Lessons`) |
| **Kurva lupa Ebbinghaus** (yang lemah dan lama tidak dipakai memudar) | Memori penuh tidak menolak fakta baru. Entri terlemah dilupakan: yang tidak valid dulu, lalu yang tanpa sitasi, lalu yang paling lama tidak dikonfirmasi. Entri itu dipindah ke jurnal, jadi masih bisa dicari | `memory.go` (`upsert`, `weakest`) |
| **Rekonsolidasi** (ingatan yang dipanggil ulang bisa diperbarui) | `@remember` yang mirip ≥ 60% dengan catatan lama **mengganti** catatan itu dan menyegarkan tanggalnya | `entries.go` (`similar`) |
| **Reality monitoring** (membedakan ingatan dari imajinasi) | Setiap catatan menyimpan tanggal + file yang disebut. Catatan yang filenya sudah hilang tidak ditampilkan ke model | `entries.go` (`valid`) |
| **Perhatian selektif** (yang tidak relevan disaring) | Recall hanya disuntik kalau presisi (≥ 2 kata kunci dan ≥ 50% kata kueri cocok), maksimal 2 potong. Selebihnya diambil sendiri lewat `search {memory}` | `cmd/agentium/memory.go` |
| **Sistem 1 / Sistem 2** (Kahneman: cepat dan hemat secara default, lambat dan teliti saat perlu) | Effort berpikir dinaikkan satu tingkat setelah 3 batch gagal berturut-turut atau saat detektor macet memperingatkan (maksimal 2×), lalu kembali normal di giliran berikutnya | `track.go` (`escalate`) |
| **Metakognisi** (tahu kapan belum yakin) | Pengingat verifikasi sebelum selesai, detektor macet (peringatan di 3×, berhenti di 5×) | `agent.go`, `track.go` |
| **Sistem imun** (berlapis, spesifik, diam kalau tidak ada ancaman) | Sandbox OS + gerbang perintah berisiko + penjaga `.git` + env tanpa kredensial + fetch menolak URL berisi rahasia + memori dari web ditahan | `internal/policy`, `sandbox`, `tool/gitguard.go` |
| **Homeostasis** (menjaga kondisi tetap stabil) | Anggaran konteks (elision 55%, ringkasan 85%), batas biaya, batas ukuran `read`, gambar dibuang untuk model tanpa vision | `context.go`, `files.go`, `agent.go` |

## 2. Hukum alam dan fisika

| Hukum | Mekanisme | Letak di kode |
|---|---|---|
| **Kekekalan** (tidak ada yang hilang begitu saja) | Checkpoint shadow-git per giliran (`/undo`). Edit ditulis **atomik**: file sementara → fsync → rename → baca ulang untuk verifikasi. Crash atau disk penuh meninggalkan versi lama atau versi baru, tidak pernah setengah | `internal/checkpoint`, `edit.go` (`writeAtomic`) |
| **Entropi** (kekacauan bertambah kalau dibiarkan) | Membaca ulang file yang tidak berubah hanya mengembalikan penunjuk ke hasil sebelumnya. Output lama di-*mask*. Memori punya kapasitas dan kurva lupa | `files.go`, `context.go` |
| **Prinsip aksi terkecil** (alam memilih jalur paling hemat) | Prompt < 1k token. `--best-of` memilih percobaan lulus dengan diff terkecil. Code map on-demand, bukan disuntik | `bestof.go`, `prompt.go` |
| **Seleksi alam** (variasi + seleksi) | `--best-of N --check`: N percobaan paralel di worktree terpisah, diseleksi oleh tes | `bestof.go` |
| **Gravitasi / PageRank** (massa menarik perhatian) | Peta repo berperingkat: file yang paling banyak "ditarik" oleh file lain (identifier yang dipakai) muncul duluan | `codemap/rank.go` |
| **Pengamatan sebelum tindakan** | Edit menolak menimpa file yang belum dibaca atau yang berubah sejak dibaca | `edit.go`, `tool.go` |
| **Umpan balik negatif** (sistem kendali) | Detektor macet, lint gate, verifikasi, eskalasi effort: sinyal gagal mengubah perilaku | `track.go`, `lint` |

## 3. Keajaiban dunia sebagai prinsip rekayasa (metafora)

- **Piramida: fondasi dulu.** Keamanan dan keandalan dikerjakan sebelum fitur. Setiap fitur punya tes unit/e2e, dan CI menguji Linux dan macOS.
- **Tembok Besar: pertahanan berlapis.** Tidak ada satu lapisan yang dianggap cukup: sandbox, gerbang kebijakan, penjaga `.git`, pembersih env, dan pemeriksa URL saling menutup celah.
- **Akuaduk Romawi: sederhana dan tahan lama.** Satu binary statis, stdlib Go saja, file teks biasa untuk memori (bisa dibaca dan diedit manusia).

## 4. Tentang "kuantum": apa yang tidak kami lakukan

Tidak ada komputasi kuantum di Agentium, dan tidak akan diklaim. Satu-satunya kemiripan yang jujur hanyalah analogi. `--best-of` menjalankan beberapa kemungkinan secara paralel, lalu "runtuh" ke satu hasil saat diukur oleh tes. Itu tetap komputasi klasik biasa, dan kami menyebutnya **seleksi**, bukan kuantum.

## 5. Yang belum terbukti

Semua mekanisme di atas diuji fungsinya (tes otomatis). **Dampaknya terhadap skor tugas nyata belum diukur**, karena butuh API key dan run benchmark (Terminal-Bench 2.1 / 4.0). Itu langkah berikutnya yang paling penting.
