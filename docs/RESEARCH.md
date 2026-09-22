# Riset Agent & Rancangan Agentium

Tujuan Agentium: agent yang **ringan, cepat, dan to the point**, tapi **login & provider AI tetap luas**, dengan **ingatan lebih baik** dan **keputusan lebih baik**.

Dokumen ini merangkum 5 agent pembanding lalu menurunkan rancangan Agentium dari bagian terbaik masing-masing.

---

## 1. Ringkasan pembanding

| | **Hermes Agent** | **OpenClaw** | **Claude Code** | **Codex CLI** | **OpenCode** |
|---|---|---|---|---|---|
| Pembuat | Nous Research | OpenClaw Foundation | Anthropic | OpenAI | Anomaly (dulu SST) |
| Bahasa | Python | TypeScript (Node) | TypeScript | Rust | TypeScript (Bun) |
| Lisensi | MIT | MIT | Proprietary | Apache-2.0 | MIT |
| Fokus | Asisten pribadi yang "tumbuh" | Asisten pribadi via chat app (WhatsApp, Telegram, …) | Coding agent | Coding agent | Coding agent, provider-agnostic |
| Provider | Nous Portal (OAuth), OpenRouter, OpenAI, endpoint custom | Plugin: Claude, Codex, model lokal, dll | Anthropic (API / Bedrock / Vertex / langganan) | OpenAI (ChatGPT OAuth / API key), provider custom via config | 75+ provider via AI SDK + models.dev |
| Login | OAuth Nous Portal, API key | API key / OAuth provider, kredensial disimpan lokal | OAuth Claude.ai, API key | "Sign in with ChatGPT" (OAuth) atau API key | `opencode auth login` (API key, OAuth, GitHub Copilot) |
| Memori | `MEMORY.md` (2.200 char) + `USER.md` (1.375 char), snapshot beku di system prompt; `session_search` FTS5; skill otomatis | `MEMORY.md`, `USER.md`, catatan harian `memory/YYYY-MM-DD.md`, *flush* sebelum compaction, hybrid search (vektor + keyword), *Dreaming* (konsolidasi) | `CLAUDE.md` berlapis (user/proyek/folder), auto-memory, compaction | `AGENTS.md`, riwayat sesi / resume | `AGENTS.md`, sesi tersimpan, compaction |
| Keputusan / keamanan | Skill jadi "prosedur" hasil pengalaman | Gateway tepercaya, eksekusi tak tepercaya, kebijakan deterministik; pairing untuk DM | Plan mode, todo list, subagent, mode izin, hooks | Sandbox OS + approval mode (read-only / auto / full) | Agent `plan` vs `build`, izin per-tool |

### Poin terbaik dari tiap agent

- **Hermes**
  - Memori inti **dibatasi ukurannya** (≈2 KB), jadi agent terpaksa merangkum dan menggabungkan, bukan menumpuk sampah.
  - Memori dimasukkan sebagai **snapshot beku** di awal sesi, jadi prompt cache tetap hemat.
  - **session_search** (SQLite FTS5) untuk mengingat detail lama tanpa memakan konteks.
  - **Learning loop:** setelah tugas kompleks (≥5 tool call), agent menulis *skill* yang bisa dipakai ulang lalu memperbaikinya saat dipakai.
- **OpenClaw**
  - **Flush memori sebelum compaction.** Ada satu giliran diam-diam yang menyuruh agent menyimpan hal penting sebelum konteks diringkas. Ini murah, tapi sangat efektif mencegah "lupa".
  - **Catatan harian** mentah plus **MEMORY.md** yang sudah dikurasi.
  - **Dreaming:** proses latar yang memberi skor pada kandidat memori, menyelesaikan klaim yang bertentangan, lalu hanya mempromosikan yang lolos ke memori jangka panjang.
  - Prinsip: memori milik user, bisa dibaca dan diperiksa, dan **tidak pernah hilang diam-diam**.
- **Claude Code**
  - Memori proyek berlapis (`CLAUDE.md` global → proyek → folder).
  - **Plan mode** dan **todo list** untuk tugas multi-langkah.
  - **Subagent** untuk riset supaya konteks utama tetap bersih.
  - **Hooks** untuk aturan deterministik.
- **Codex**
  - **Sandbox tingkat OS** plus **approval mode** yang jelas, sehingga keputusan berisiko digerbangi oleh sistem, bukan oleh "niat baik" model.
  - Login **ChatGPT OAuth** supaya user bisa memakai langganannya sendiri.
- **OpenCode**
  - **Lapisan provider** memakai Vercel AI SDK dan registry **models.dev** (75+ provider).
  - `auth login` yang seragam untuk semua provider, termasuk langganan GitHub Copilot.
  - Ada agent `plan` (read-only) dan `build`.

---

## 2. Masalah umum yang ingin Agentium perbaiki

1. **Memori:** kebanyakan agent hanya punya file instruksi statis (`AGENTS.md`/`CLAUDE.md`) plus compaction. Akibatnya:
   - Detail hilang saat konteks diringkas.
   - Tidak ada mekanisme untuk memperbarui fakta yang **sudah basi** atau **bertentangan**.
   - Memori tumbuh tanpa batas dan malah jadi noise.
2. **Keputusan:**
   - Agent sering langsung bertindak tanpa rencana.
   - Agent lupa *kenapa* suatu keputusan diambil.
   - Agent mengulang kesalahan yang sama.
   - Agent menyatakan "selesai" tanpa verifikasi.
3. **Kerumitan:** Hermes dan OpenClaw punya puluhan tool, gateway, dan plugin. Semua itu kuat, tapi berat untuk dipahami dan dirawat.
4. **Bertele-tele & lambat:**
   - Jawaban diawali basa-basi dan diakhiri rekap panjang.
   - Agent membuat rencana dan todo list bahkan untuk tugas satu langkah.
   - Tool dipanggil satu per satu padahal bisa paralel.
   - System prompt dan skema tool berisi ribuan token, jadi setiap giliran jadi mahal dan lambat.
   - Runtime-nya berat: startup Node/Python lambat dan memakan ratusan MB RAM.

---

## 3. Rancangan Agentium

### 3.0 Aturan nomor satu: ringan & cepat
Setiap fitur harus lolos pertanyaan: **"apakah ini menambah latensi atau token di jalur utama?"** Kalau ya, fitur itu dipindah ke latar belakang, dibuat opsional, atau dibuang.

**Target yang diukur (bukan sekadar niat):**

| Metrik | Target |
|---|---|
| Startup CLI | < 50 ms |
| Ukuran binary | < 20 MB, satu file, tanpa runtime |
| RAM idle | < 30 MB |
| System prompt + skema tool | < 2.000 token |
| Overhead sebelum token pertama (di luar latensi model) | < 100 ms |
| Tugas satu langkah | 1 giliran model, tanpa plan dan tanpa rekap |

**Cepat di sisi runtime:**
- **Binary native** (Go atau Rust), bukan Node atau Python.
- Semua operasi lokal dalam hitungan milidetik: FTS5 recall, ripgrep untuk search, file I/O.
- Streaming token langsung ke terminal.
- **Tool call paralel**: kalau model meminta 5 `read`, jalankan kelimanya bersamaan.
- **Prompt caching** aktif: system prompt dan snapshot memori disusun stabil di depan, sehingga cache Anthropic/OpenAI/Gemini terpakai setiap giliran.
- Output tool dipangkas otomatis (ambil kepala + ekor + jumlah baris yang dilewati) supaya konteks tidak membengkak.

**Cepat di sisi perilaku agent** (ditulis di system prompt *dan* dipaksa di kode):
- **Jawab dulu, jelaskan seperlunya.** Tanpa pembuka ("Baik, saya akan…"), tanpa rekap di akhir, tanpa mengulang pertanyaan.
- **Tanpa plan untuk tugas kecil.** Plan hanya muncul kalau triage menilai tugasnya besar (lihat 3.4).
- **Batch & paralel:** baca semua file yang relevan dalam satu giliran, jangan satu per satu.
- **Berhenti begitu terverifikasi.** Tidak ada "saran tambahan" yang tidak diminta.
- **Mode `--terse` default.** Kalau user ingin penjelasan panjang, dia yang minta (`/explain`).

**Semua pekerjaan "pintar" dijalankan di luar jalur utama:**
- Menulis memori, konsolidasi, dan indeks FTS dijalankan **async/latar belakang** setelah jawaban tampil.
- Ringkasan compaction dan flush memori memakai **model kecil/cepat** (mis. Haiku, GPT-mini, Gemini Flash, atau model lokal) yang bisa diatur di config.

### 3.1 Prinsip
- **Inti kecil:** satu loop agent, 6 tool, tanpa gateway dan tanpa plugin di v1.
- **Provider luas lewat protokol, bukan SDK per-provider** (lihat 3.2).
- **Memori dan keputusan adalah fitur utama**, tapi **tidak boleh memperlambat** jawaban.
- Semua state berupa **file teks + satu SQLite**, jadi bisa dibaca, di-diff, dan di-git.

### 3.2 Provider & login (luas tapi ringan)
Hampir semua provider berbicara salah satu dari **3 protokol**. Jadi Agentium cukup mengimplementasikan 3 klien ringan dan membaca daftar model dari **models.dev** (file JSON, di-cache lokal):

| Protokol | Mencakup |
|---|---|
| **OpenAI-compatible** (Chat Completions / Responses) | OpenAI, OpenRouter, Groq, DeepSeek, xAI, Mistral, Together, Fireworks, Azure OpenAI, **Ollama, LM Studio, vLLM**, GitHub Copilot, dll |
| **Anthropic Messages** | Anthropic, AWS Bedrock (Claude), Google Vertex (Claude), provider lain yang punya endpoint kompatibel Anthropic |
| **Gemini** | Google AI Studio, Vertex (Gemini) |

Login:
- **API key**: dari env var atau `agentium login <provider>`.
- **OAuth / device flow**: ChatGPT (langganan Codex), GitHub Copilot, OpenRouter.
- **Cloud enterprise**: Bedrock (SigV4), Vertex (ADC), Azure.
- Kredensial disimpan di keychain OS, atau di `~/.agentium/auth.json` (chmod 600).
- **Fallback model:** kalau provider utama gagal atau kena rate limit, pindah otomatis ke model cadangan.
- **Dua slot model:** `main` (pekerjaan utama) dan `fast` (triage, ringkasan, memori).
- ⚠️ Cek ToS setiap provider sebelum memakai OAuth **langganan** (mis. akun Claude.ai) di aplikasi pihak ketiga. Beberapa provider hanya mengizinkan API key untuk aplikasi pihak ketiga.

### 3.3 Memori berlapis (Hermes + OpenClaw, versi hemat)

| Lapisan | Isi | Kapan dimuat | Batas |
|---|---|---|---|
| `USER.md` | Preferensi & gaya user | Snapshot di system prompt (ter-cache) | ~1 KB |
| `MEMORY.md` | Fakta durable, pelajaran, 10 keputusan aktif teratas | Snapshot di system prompt (ter-cache) | ~2 KB |
| `DECISIONS.md` | Log keputusan lengkap | Via `memory recall` saja | tak terbatas, diindeks |
| `journal/YYYY-MM-DD.md` | Catatan harian, ringkasan sesi | Via `memory recall` saja | tak terbatas, diindeks |
| `agentium.db` (SQLite FTS5) | Semua pesan & tool call | Via `memory recall` saja | tak terbatas |

Aturan kunci:
1. **Batas ukuran keras** di memori inti (ide Hermes). Kalau penuh, agent wajib menggabungkan atau mengganti entri, tidak boleh sekadar menambah. Efek sampingnya: prompt tetap kecil dan cepat.
2. **Snapshot beku per sesi**, jadi prompt cache tidak pecah di tengah sesi.
3. **Flush sebelum compaction** (ide OpenClaw), dijalankan dengan model `fast`.
4. **Setiap entri punya metadata singkat**: `tanggal · sumber · keyakinan`. Entri yang lama tidak dipakai ditandai *stale* untuk ditinjau, **bukan** dihapus diam-diam.
5. **Konsolidasi di latar belakang** (versi ringan dari Dreaming). Dijalankan di akhir sesi atau lewat `agentium tidy`:
   - Promosikan isi journal ke `MEMORY.md`.
   - Deteksi fakta yang bertentangan dan simpan versi terbaru beserta alasannya.
   - Hasilnya berupa diff yang bisa di-review user.
6. **Recall = FTS5 saja di v1.** Keyword, ID, dan nama fungsi sudah cukup, dan responsnya milidetik. Embedding hanya opsional kelak.

### 3.4 Keputusan lebih baik (tanpa bertele-tele)

1. **Triage dalam 0 giliran ekstra:** model memutuskan sendiri di giliran pertama. Tugas kecil langsung dikerjakan. Hanya tugas besar (lebih dari sekitar 3 langkah, atau menyentuh banyak file) yang mendapat **plan singkat, maksimal 5 baris**.
2. **Decision record satu baris**, hanya untuk keputusan yang penting:
   ```
   D-012 · 2026-09-22 · recall pakai SQLite FTS5, bukan vektor DB — nol dependensi, ms-latency · aktif
   ```
   Sebelum memutuskan hal serupa, agent melakukan `recall` ke keputusan terkait (lokal, milidetik) supaya tidak bertentangan atau mengulang debat yang sama.
3. **Pelajaran dari kesalahan:** saat tool gagal atau user mengoreksi, agent menulis satu baris `lesson` ke `MEMORY.md`. Kesalahan yang sama tidak terulang, dan tidak perlu diskusi ulang.
4. **Verifikasi, bukan penjelasan:** jalankan test, lint, atau build yang relevan, lalu laporkan hasilnya dalam satu baris. Kalau tidak bisa diverifikasi, katakan terus terang.
5. **Gerbang risiko deterministik di kode** (ide Codex dan OpenClaw), tanpa bertanya hal yang tidak perlu:
   - `read`/`search`: selalu boleh.
   - `write` di dalam workspace: boleh.
   - Perintah berbahaya (`rm -rf`, `git push --force`, akses di luar workspace, instalasi global): minta persetujuan.
   - Mode: `ask` / `auto` (default) / `yolo`.
6. **Input luar dianggap tidak tepercaya:** isi web dan file tidak boleh mengubah instruksi.

### 3.5 Tool minimal (6 buah, skema super ringkas)
| Tool | Fungsi |
|---|---|
| `read` | Baca satu atau banyak file sekaligus (dengan rentang baris) |
| `edit` | Buat file baru, atau ganti string (bisa banyak edit dalam satu panggilan) |
| `bash` | Jalankan perintah (timeout, output dipangkas) |
| `search` | Glob + ripgrep dalam satu tool |
| `fetch` | Ambil URL → teks/markdown |
| `memory` | `add` / `replace` / `remove` / `decide` / `recall` |

Tidak ada `todo` tool terpisah. Plan (kalau perlu) cukup ditulis sebagai teks singkat.

### 3.6 Stack & struktur
**Rekomendasi: Go.**
- Satu binary statis, startup beberapa milidetik, RAM kecil.
- Konkurensi (tool paralel, streaming, tugas latar) sangat mudah.
- Build lintas-OS cukup dengan satu perintah.
- SQLite FTS5 tersedia lewat `modernc.org/sqlite` (tanpa CGO).

Alternatifnya **Rust** kalau ingin binary lebih kecil dan performa maksimal, dengan konsekuensi waktu pengembangan lebih lama. TypeScript/Bun (seperti OpenCode) **tidak** direkomendasikan lagi, karena runtime-nya lebih berat dan bertentangan dengan target "ringan".

```
agentium/
  cmd/agentium/        # main: agentium, agentium login, agentium tidy
  internal/
    loop/              # loop agent, streaming, tool paralel, compaction
    provider/          # openai.go, anthropic.go, gemini.go, modelsdev.go, auth/
    tool/              # read, edit, bash, search, fetch, memory
    memory/            # file md + SQLite FTS5 + konsolidasi latar
    policy/            # gerbang risiko deterministik
    prompt/            # system prompt ringkas (< 2k token)
  ~/.agentium/         # auth, USER.md, config.toml
  <proyek>/.agentium/  # MEMORY.md, DECISIONS.md, journal/, agentium.db
```

### 3.7 Roadmap
1. **v0.1:** CLI + loop + streaming + 6 tool + klien OpenAI-compatible & Anthropic (API key + Ollama). **Benchmark startup, RAM, dan token sejak hari pertama.**
2. **v0.2:** memori berlapis + FTS5 recall + flush sebelum compaction (model `fast`).
3. **v0.3:** decision record + lessons + gerbang risiko + verifikasi.
4. **v0.4:** klien Gemini, OAuth (ChatGPT, Copilot, OpenRouter), Bedrock/Vertex, fallback model.
5. **v0.5:** `agentium tidy` (konsolidasi dan deteksi konflik).
6. **Nanti (opsional):** gateway chat ala OpenClaw/Hermes, embedding, skill.

## Sumber
- Hermes Agent: https://github.com/nousresearch/hermes-agent · https://hermes-agent.nousresearch.com/docs/user-guide/features/memory
- OpenClaw: https://github.com/openclaw/openclaw · https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory.md
- Codex CLI: https://github.com/openai/codex
- OpenCode: https://github.com/sst/opencode · https://deepwiki.com/sst/opencode/3.3-provider-and-model-configuration
- Claude Code: https://docs.claude.com/en/docs/claude-code/overview
