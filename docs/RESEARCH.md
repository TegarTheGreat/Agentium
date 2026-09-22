# Riset Agent & Rancangan Agentium

Tujuan Agentium: agent yang **simpel dan to the point**, tapi **login & provider AI tetap luas**, dengan **ingatan lebih baik** dan **keputusan lebih baik**.

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

---

## 3. Rancangan Agentium

### 3.1 Prinsip
- **Inti kecil:** satu loop agent, sekitar 8 tool, tanpa gateway dan tanpa plugin di v1.
- **Provider luas lewat satu adapter.** Luasnya dukungan provider datang dari registry, bukan dari kode per-provider.
- **Memori dan keputusan adalah fitur utama**, bukan tempelan.
- Semua state berupa **file teks + satu SQLite**, jadi bisa dibaca, di-diff, dan di-git.

### 3.2 Provider & login (meniru OpenCode + Codex)
- Adapter tunggal memakai **Vercel AI SDK**, dengan daftar model dan harga dari **models.dev**.
- Metode login:
  - **API key**: Anthropic, OpenAI, Google, Mistral, DeepSeek, xAI, Groq, dll. Dibaca dari env var atau `agentium auth login`.
  - **OpenAI-compatible endpoint**: OpenRouter, Together, Fireworks, vLLM, **Ollama / LM Studio** (lokal).
  - **OAuth / device flow**: ChatGPT (Codex), GitHub Copilot, OpenRouter.
  - **Cloud enterprise**: AWS Bedrock, Google Vertex, Azure OpenAI.
- Kredensial disimpan di `~/.agentium/auth.json` (chmod 600) atau di keychain OS.
- **Fallback model:** kalau provider utama gagal atau kena rate limit, pindah ke model cadangan dari config.
- ⚠️ Cek ToS setiap provider sebelum memakai OAuth **langganan** (mis. akun Claude.ai) di aplikasi pihak ketiga. Beberapa provider hanya mengizinkan API key untuk aplikasi pihak ketiga.

### 3.3 Memori berlapis (gabungan Hermes + OpenClaw + tambahan)

| Lapisan | Isi | Kapan dimuat | Batas |
|---|---|---|---|
| `USER.md` | Preferensi & gaya user | Snapshot di system prompt | ~1,5 KB |
| `MEMORY.md` | Fakta durable proyek/lingkungan | Snapshot di system prompt | ~2,5 KB |
| `DECISIONS.md` | Log keputusan (lihat 3.4) | Ringkasan 10 teratas di prompt, sisanya via search | tak terbatas, diindeks |
| `journal/YYYY-MM-DD.md` | Catatan harian mentah, ringkasan sesi | Tidak dimuat, via search | tak terbatas |
| `sessions.db` (SQLite FTS5) | Semua pesan & tool call | Via tool `recall` | tak terbatas |

Aturan kunci:
1. **Batas ukuran keras** pada memori inti (ide Hermes). Kalau penuh, agent **wajib** menggabungkan atau mengganti entri, tidak boleh sekadar menambah.
2. **Flush sebelum compaction** (ide OpenClaw). Sebelum konteks diringkas, jalankan satu giliran: "simpan fakta, keputusan, dan pelajaran penting sekarang".
3. **Setiap entri memori punya metadata**: `sumber`, `tanggal`, `keyakinan`, `terakhir-dipakai`. Entri yang lama tak dipakai ditandai *stale*, lalu ditinjau, **bukan** dihapus diam-diam.
4. **Konsolidasi** (ide Dreaming, versi ringan). Dijalankan di akhir sesi atau lewat `agentium tidy`:
   - Baca journal hari ini.
   - Usulkan promosi atau revisi ke `MEMORY.md`.
   - Deteksi fakta yang bertentangan dan simpan versi terbaru beserta alasannya.
   - Hasilnya berupa diff yang bisa di-review user.
5. **Recall hybrid:** FTS5 (keyword/ID/nama fungsi) dulu. Embedding hanya opsional kalau provider embedding tersedia, supaya v1 tetap simpel.

### 3.4 Keputusan lebih baik

1. **Triage otomatis:** tugas kecil langsung dikerjakan. Tugas yang lebih besar (lebih dari sekitar 3 langkah, atau menyentuh banyak file) masuk ke **rencana singkat** dulu, berupa todo list.
2. **Decision record:** setiap keputusan non-trivial ditulis ke `DECISIONS.md`:
   ```
   ## D-012 · 2026-09-22 · Pakai SQLite FTS5 untuk recall
   Konteks: butuh pencarian riwayat tanpa server tambahan
   Opsi: FTS5 | vektor DB | grep file
   Dipilih: FTS5 — nol dependensi, cepat, cukup untuk keyword
   Status: aktif   (aktif | diganti oleh D-0xx | dibatalkan)
   ```
   Sebelum membuat keputusan baru, agent **mencari keputusan terkait** supaya tidak bertentangan atau mengulang debat yang sama.
3. **Pelajaran dari kesalahan:** saat tool gagal atau user mengoreksi, agent menulis entri `lesson` singkat ke `MEMORY.md`, misalnya "jangan pakai X, pakai Y karena Z". Ini versi ringan dari *skill* Hermes.
4. **Verifikasi sebelum "selesai":** jalankan test, lint, atau build yang relevan. Kalau tidak bisa diverifikasi, katakan terus terang.
5. **Gerbang risiko deterministik** (ide Codex dan OpenClaw). Kebijakan di kode, bukan di prompt:
   - `read`: selalu boleh
   - `write` di dalam workspace: boleh
   - `bash` dengan perintah berbahaya (`rm -rf`, `git push --force`, jaringan, di luar workspace): minta persetujuan
   - Mode: `ask` / `auto` / `yolo`
6. **Input luar dianggap tidak tepercaya:** isi web dan file tidak boleh mengubah instruksi.

### 3.5 Tool minimal (v1)
`read`, `write`, `edit`, `bash`, `search` (glob + grep), `web_fetch`, `memory` (add / replace / remove / decide), `recall` (FTS5 atas sesi + journal), `todo`.

### 3.6 Struktur yang diusulkan
```
agentium/
  src/
    cli.ts            # entry: agentium, agentium auth login, agentium tidy
    loop.ts           # loop agent: model ↔ tool, compaction, flush memori
    providers/        # adapter AI SDK + models.dev + auth (key/OAuth)
    tools/            # 9 tool di atas
    memory/           # USER/MEMORY/DECISIONS/journal + SQLite FTS5 + konsolidasi
    policy.ts         # gerbang risiko deterministik
  ~/.agentium/        # auth.json, USER.md, config.toml
  <proyek>/.agentium/ # MEMORY.md, DECISIONS.md, journal/, sessions.db
```
Rekomendasi stack: **TypeScript + Bun** (seperti OpenCode), supaya bisa langsung memakai AI SDK dan models.dev. Alternatifnya **Rust** (seperti Codex) kalau yang diutamakan adalah performa dan binary tunggal.

### 3.7 Roadmap
1. **v0.1:** CLI chat + loop + 9 tool + provider via AI SDK (API key + Ollama).
2. **v0.2:** memori berlapis + FTS5 recall + flush sebelum compaction.
3. **v0.3:** decision record + triage/plan + gerbang risiko + verifikasi.
4. **v0.4:** OAuth (ChatGPT, Copilot, OpenRouter) + fallback model.
5. **v0.5:** `agentium tidy` (konsolidasi dan deteksi konflik) + skill ringan.
6. **Nanti:** gateway chat (Telegram/WhatsApp) ala OpenClaw/Hermes, dan embedding opsional.

---

## Sumber
- Hermes Agent: https://github.com/nousresearch/hermes-agent · https://hermes-agent.nousresearch.com/docs/user-guide/features/memory
- OpenClaw: https://github.com/openclaw/openclaw · https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory.md
- Codex CLI: https://github.com/openai/codex
- OpenCode: https://github.com/sst/opencode · https://deepwiki.com/sst/opencode/3.3-provider-and-model-configuration
- Claude Code: https://docs.claude.com/en/docs/claude-code/overview
