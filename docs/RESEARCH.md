# Riset Agent & Rancangan Agentium (v2 · data per 22 Sep 2026)

**Tujuan:** agent yang **paling simpel, paling cepat, dan to the point**, dengan **login/provider luas**, **ingatan terbaik**, dan **keputusan terbaik**. Targetnya mengungguli semua agent yang ada di setiap dimensi itu, dan harus **dibuktikan dengan benchmark**, bukan klaim.

> Label sumber: **[V]** = dibaca langsung dari sumber primer. **[S]** = dari snippet/sumber sekunder, belum diverifikasi. Angka vendor ditandai *(klaim vendor)*.

---

## 1. Peta agent terbaru

| Agent | Versi terbaru | Runtime | Overhead prompt | Memori | Kelemahan utama (2026) |
|---|---|---|---|---|---|
| **Claude Code** | v2.1.280 (22 Sep) | Binary native (Bun), ~100 MB | **~33k token, 27 tool**. Dengan config nyata 75–85k [V] | CLAUDE.md/AGENTS.md + auto-memory (`MEMORY.md`) | ~3/4 token input adalah overhead, kuota cepat habis |
| **Codex CLI** | v0.155.1 (18 Sep) | Rust | ? | *memories* + pruning 30 hari + konsolidasi latar; compaction di server [S] | Terkunci ke ekosistem OpenAI |
| **OpenCode** | v1.18.32 (21 Sep) | TS/Bun → pindah ke Node | ~7k token, 10 tool [V] | AGENTS.md, compaction | Memori proses bisa 10–15 GB, CPU tinggi, startup lambat (issue #11399, #12513) |
| **Pi** (Mario Zechner) | v0.86.1 (22 Sep) | TS/Node | **< 1k token, 4 tool** [V] | AGENTS.md, sesi bercabang (`/tree`), compaction | Tidak ada memori lintas sesi, tidak ada sandbox |
| **Gemini CLI → Antigravity CLI** | v0.59 / `agy` | Node → Go (closed) | ? | GEMINI.md | Gemini CLI dipensiunkan untuk user gratis/Pro sejak 18 Jun 2026 [S] |
| **Crush** (Charm) | v0.93.1 | Go | ? | Tidak ada auto-memory | Lisensi FSL |
| **Goose** (AAIF/Linux Foundation) | v1.50.1 | Rust + TS | ? | Recipes | Berat karena 70+ ekstensi MCP |
| **Amp** | rolling | ? | ? | Thread | Fitur terbaik ada di layanan berbayar (orbs) |
| **Factory Droid** | v0.177 | ? | ? | Spec Mode | Closed source |
| **Hermes Agent** | v0.21.4 (21 Sep) | Python + Node | ? | MEMORY.md 2.200 char + USER.md 1.375 char (snapshot beku), FTS5 `session_search`, skill otomatis, 8 plugin memori [V] | Startup pernah 2+ menit (Honcho blocking), state.db korup, CVE RCE di pemindai memori (CVE-2026-10223) [S] |
| **OpenClaw** | v2026.9.5 (19 Sep) | Node 24+, install > 1 GB [S] | ? | MEMORY/USER/daily/DREAMS.md, hybrid search, *dreaming*, flush sebelum compaction [V] | ClawBleed RCE (CVSS 8.8), privesc CVSS 9.9, 800+ skill berbahaya di ClawHub [S]. Recall 1 fakta 19,6 s vs 113 ms di Hermes [S, 1 run] |

Sumber: bagian **Sumber** di akhir dokumen.

---

## 2. Temuan penting dari data 2026 (yang mengubah rancangan)

1. **Harness minimal menang, atau minimal seimbang.**
   - Pi (4 tool, prompt ~150 kata) menempati peringkat 2 Terminal-Bench, dan pass rate-nya tertinggi di Opus 4.8 dengan **konteks ~3× lebih kecil** daripada Claude Code/Codex [S].
   - mini-swe-agent (~100 baris, hanya bash) mencetak > 74% di SWE-bench Verified [V].
   - Studi 176 setting (arXiv 2609.20804): model kuat dengan **hanya bash** mendapat 69,4% vs 65,8% dengan tool lengkap, dengan **biaya separuhnya** ($1,11 vs $2,33) [V].
2. **Tapi rekayasa harness tetap berpengaruh sampai ±5 poin.**
   - Model yang sama (GPT-5.5): Codex CLI 83,4% vs Terminus 2 78,2% di Terminal-Bench 2.1.
   - Penyebabnya: **sandbox OS**, **verifikasi otomatis setelah tiap tool call**, tool filesystem langsung (bukan MCP), dan **~4× lebih sedikit token** [V].
3. **Tool memori yang harus dipanggil model = untung-untungan.** Tool recall menang di 15 dan kalah di 14 dari 32 perbandingan, dan **di 36 dari 64 setting model tidak pernah memanggilnya** [V]. ⇒ Memori harus **otomatis**, bukan menunggu model ingat untuk memanggil.
4. **Teknik memori yang terbukti paling efektif (LongMemEval):**
   - **Observation log + prefix stabil** (Mastra 94,87%, biaya ~10× lebih murah) *(klaim vendor)*
   - **Hybrid retrieval + rank fusion** (Hindsight 91,4%)
   - **Fakta bertemporal** (valid-dari/digantikan; Zep +18,5%)
   - **Penalaran latar atas memori** (Honcho 90,4% dengan 5% konteks) *(klaim vendor)*
   - **Konsolidasi** fakta basi (Mem0 naik dari 57,5% ke 73,8% pada uji independen)
5. **Manajemen konteks:**
   - Paling berpengaruh saat konteksnya sempit: dengan budget 32k, skor naik dari 6% ke 58%.
   - Cara termurah: **elision** (membuang output tool lama) dulu, baru **ringkasan bertahap** [V].
6. **Planning hanya membantu model lemah.** Model 30B turun dari 25% ke 13,6% tanpa plan. Model frontier tidak butuh [V]. ⇒ Harness harus **adaptif per model**.
7. **Prompt caching** menghemat biaya 77–81% (GPT-5.2), tapi manfaatnya hilang kalau prefix berubah [S].
8. **Kecepatan inferensi yang tersedia:**
   - Claude fast mode sampai 2,5× token/s [V].
   - GPT-5.3-Codex-Spark di Cerebras > 1.000 tok/s [S].
   - Cerebras ~2.100 tok/s, Groq TTFT < 100 ms [S].
9. **Kebijakan login langganan (penting untuk "login luas"):**
   - ✅ **ChatGPT (OpenAI)**: dipakai OpenCode, Pi, dan OpenClaw. OpenAI terbuka soal ini [S].
   - ✅ **GitHub Copilot**: resmi mendukung agent pihak ketiga sejak 16 Jan 2026 [S].
   - ❌ **Anthropic Pro/Max OAuth**: dilarang untuk tool pihak ketiga (ToS Feb 2026, ditegakkan Apr 2026, kebijakan berubah-ubah) [S]. ⇒ pakai **API key / Bedrock / Vertex**.
   - ❌ **Google Gemini/Antigravity OAuth**: dilarang, ditegakkan sejak 25 Mar 2026 [S]. ⇒ pakai **AI Studio / Vertex key**.
   - **models.dev** aktif dan menyediakan `https://models.dev/api.json` [V].
10. **Keamanan adalah titik lemah agent "pintar".** Hermes dan OpenClaw sama-sama kena CVE RCE, dan marketplace skill OpenClaw disusupi malware. ⇒ **Tidak ada marketplace skill jarak jauh** dan **sandbox dinyalakan sejak awal**.

---

## 3. Target Agentium vs yang terbaik saat ini

| Dimensi | Terbaik saat ini | Target Agentium | Cara mencapainya |
|---|---|---|---|
| Overhead prompt | Pi < 1k token | **≤ 800 token** (prompt + skema tool) | 5 tool, skema ringkas, tanpa contoh panjang |
| Startup | Claude Code native (≈ms, 100 MB) | **< 30 ms, binary < 15 MB** | Go statis, lazy-load, tanpa runtime |
| RAM | OpenCode bisa GB | **< 30 MB idle, < 150 MB sesi panjang** | Streaming, output tool dipangkas, tanpa Electron/Node |
| Token per tugas | Codex (~4× lebih hemat dari Claude Code) | **≤ Pi & Codex** | Elision, prefix ter-cache, output ringkas |
| Skor tugas (Terminal-Bench 2.1) | Codex CLI + GPT-5.5 83,4% | **≥ harness bawaan untuk model yang sama** | Verifikasi otomatis + sandbox + adaptif per model |
| Recall memori | Hermes 113 ms, tapi tergantung model memanggil tool | **< 20 ms, otomatis setiap giliran** | FTS5 lokal, injeksi otomatis |
| Kualitas memori | Snapshot beku (Hermes), dreaming (OpenClaw) | **Gabungan 5 teknik terbukti, tanpa server/embedding wajib** | Lihat 4.3 |
| Keputusan | Plan mode, Spec mode | **Keputusan tercatat & dicek otomatis, plan hanya untuk model lemah** | Lihat 4.4 |
| Provider | Hermes 70+, OpenClaw 71, OpenCode 75+ | **Semua di models.dev (75+)** + lokal | 3 protokol + models.dev |
| Login | Hermes / OpenClaw | **API key semua provider + OAuth ChatGPT & Copilot + cloud (Bedrock/Vertex/Azure)**, hanya jalur yang sah | Lihat 4.2 |
| Keamanan | Codex (sandbox OS) | **Sandbox default + tanpa marketplace + memori dipindai sebelum masuk prompt** | Lihat 4.5 |

> Jujur: "mengalahkan semua agent" hanya sah kalau terukur. Karena itu **benchmark suite** (Terminal-Bench 2.1 subset + tes memori + ukur startup/RAM/token) jadi bagian v0.1, bukan belakangan.

---

## 4. Rancangan Agentium v2

### 4.0 Tiga aturan
1. **Jalur utama harus secepat mungkin.** Semua yang "pintar" (menulis memori, konsolidasi, indeks) berjalan di latar belakang atau dengan model `fast`.
2. **Hal penting jangan diserahkan ke "ingatan" model.** Verifikasi, recall memori, gerbang risiko, dan elision dijalankan **deterministik oleh kode**, bukan menunggu model memanggil tool.
3. **Output to the point.** Tanpa pembuka, tanpa rekap, tanpa saran yang tidak diminta. Mode ringkas adalah default, dan penjelasan panjang hanya lewat `/explain`.

### 4.1 Tool: 5 saja
| Tool | Catatan |
|---|---|
| `read` | Banyak file dan rentang baris dalam satu panggilan |
| `edit` | Buat, ganti, atau banyak edit sekaligus. Format *search-replace* (bisa diganti model *fast-apply* kelak) |
| `bash` | Timeout, output dipangkas (kepala + ekor), jalan di sandbox |
| `search` | Glob + ripgrep bawaan |
| `fetch` | URL → markdown, konten ditandai *tidak tepercaya* |

**Memori tidak dijadikan tool**, karena data menunjukkan model sering lupa memanggilnya. Memori ditangani otomatis (4.3). Model hanya bisa menulis memori lewat baris khusus di jawabannya, misalnya `@remember …` atau `@decide …`, yang diparse oleh harness. Ini nol skema tool dan nol giliran ekstra.

Semua tool dijalankan **paralel** kalau model memintanya dalam satu giliran.

### 4.2 Provider & login
- **3 klien protokol:**
  - OpenAI-compatible (Chat + Responses)
  - Anthropic Messages
  - Gemini
- Ketiganya ditambah daftar model **models.dev** (di-cache lokal) sudah mencakup 75+ provider plus Ollama, LM Studio, dan vLLM.
- **Login:**
  - API key untuk semua provider
  - OAuth **ChatGPT** (device code/PKCE)
  - OAuth **GitHub Copilot**
  - OAuth **OpenRouter**
  - Kredensial cloud: **Bedrock** (SigV4), **Vertex** (ADC), **Azure**
- **Tidak didukung:** OAuth langganan Anthropic dan Google, karena melanggar ToS. Keduanya tetap bisa dipakai lewat API key atau jalur cloud.
- **Dua slot model:**
  - `main` untuk pekerjaan utama
  - `fast` untuk ringkasan, konsolidasi, dan klasifikasi. Contohnya Haiku, GPT-mini, Gemini Flash, model di Cerebras/Groq, atau model lokal.
- Mendukung **fast mode** provider (mis. Claude `speed: "fast"`), tapi tidak boleh bergonta-ganti di tengah sesi karena akan merusak cache.
- **Fallback otomatis** kalau terkena rate limit atau error.

### 4.3 Memori: 5 teknik terbukti, versi ringan

| Lapisan | Isi | Teknik (bukti) | Masuk prompt? |
|---|---|---|---|
| `USER.md` (≤ 1 KB) | Preferensi user | Snapshot beku (Hermes) | Ya, di prefix ter-cache |
| `MEMORY.md` (≤ 2 KB) | Fakta, pelajaran, keputusan aktif | Snapshot beku + konsolidasi (Mem0/OpenClaw) | Ya, di prefix ter-cache |
| `observations.md` | Log observasi bertanggal, dirangkum model `fast` di latar | Observational memory (Mastra) | Ya, bagian terbaru setelah prefix. Dipangkas otomatis |
| `facts` di SQLite | Fakta + `valid_from` / `superseded_by` / `sumber` | Temporal (Zep) | Via auto-recall |
| `agentium.db` FTS5 | Semua pesan, tool call, keputusan | Hybrid search: BM25 + recency + (opsional) vektor, digabung dengan RRF (Hindsight) | Via auto-recall |

**Auto-recall:**
- Sebelum setiap giliran, harness menjalankan FTS5 atas prompt user dan file yang disentuh. Waktunya < 20 ms dan tanpa panggilan LLM.
- Maksimal ±300 token hasil yang relevan disisipkan sebagai blok `<recall>`. Blok ini diletakkan **setelah** prefix yang di-cache, jadi cache tidak rusak.

**Konsolidasi latar:** dijalankan setelah sesi atau lewat `agentium tidy`. Model `fast` bertugas:
- Menggabungkan observasi.
- Menandai fakta yang digantikan (bukan menghapusnya).
- Mempromosikan fakta ke `MEMORY.md` dalam batas ukurannya.

Hasilnya berupa diff yang bisa di-review user.

**Tidak pernah memblokir startup.** Indeks dibangun secara lazy. Ini pelajaran dari startup Hermes yang pernah 2+ menit.

**Keamanan memori:** konten dipindai dengan aturan deterministik sederhana (tanpa eval) sebelum masuk prompt, dan secret disamarkan. Pelajaran dari CVE-2026-10223.

### 4.4 Keputusan lebih baik
1. **Verifikasi otomatis setelah edit** (penyebab keunggulan Codex).
   - Harness mendeteksi perintah cek proyek: `go vet`, `tsc --noEmit`, `ruff`, `cargo check`, tes terkait, dan sebagainya.
   - Setelah setiap `edit`, perintah itu dijalankan untuk file yang berubah.
   - Error dikirim ke model di giliran berikutnya. Kalau tidak ada error, tidak ada tambahan token.
2. **Decision log bertemporal.**
   - `@decide` mencatat satu baris: keputusan, alasan, dan status.
   - Keputusan yang bertentangan dengan keputusan aktif memicu peringatan (via auto-recall) sebelum model melanjutkan.
   - Keputusan lama ditandai `superseded`, bukan dihapus.
3. **Lessons.** Kalau verifikasi gagal dengan pola yang sama berulang kali, atau user mengoreksi, satu baris pelajaran otomatis diusulkan ke `MEMORY.md`.
4. **Harness adaptif per model** (dari studi harness 2026).
   - Model frontier: tanpa plan, tool minimal, konteks penuh.
   - Model kecil/lokal: plan singkat wajib, elision lebih agresif, contoh tool.
   - Profil dipilih otomatis dari metadata models.dev (ukuran konteks, harga) dan bisa di-override.
5. **Manajemen konteks.**
   - Elision output tool lama dulu (tanpa LLM).
   - Baru ringkasan bertahap oleh model `fast`.
   - Sebelum itu ada *flush* memori (OpenClaw).
6. **"Selesai" = terverifikasi.** Kalau tidak bisa diverifikasi, agent mengatakannya dalam satu baris.

### 4.5 Keamanan (ringan tapi default aktif)
- **Sandbox OS:**
  - Linux: Landlock + seccomp
  - macOS: `sandbox-exec`
- Mode sandbox:
  - `workspace-write` (default): tulis hanya di workspace, jaringan off untuk `bash` kecuali diizinkan.
  - `read-only`
  - `full`
- **Gerbang risiko deterministik:** `rm -rf`, `git push --force`, dan akses ke luar workspace butuh persetujuan.
- **Tanpa marketplace skill jarak jauh.** Skill hanya berupa file lokal yang ditulis user atau agent.
- Input dari web dan file dianggap **tidak tepercaya**. Tidak ada port jaringan yang dibuka (tidak ada gateway di v1).

### 4.6 Stack
**Go** (seperti Crush dan Antigravity CLI):
- Satu binary statis, startup beberapa milidetik, RAM kecil.
- Konkurensi mudah untuk tool paralel, streaming, dan tugas latar.
- SQLite FTS5 tanpa CGO (`modernc.org/sqlite`).
- Tidak mengulang masalah OpenCode, yang sedang meninggalkan Bun karena memori dan crash.

```
cmd/agentium/        main: agentium, login, tidy, bench
internal/loop/       loop, streaming, tool paralel, elision/compaction, profil model
internal/provider/   openai, anthropic, gemini, modelsdev, auth (key, oauth, cloud)
internal/tool/       read, edit, bash, search, fetch
internal/memory/     md files, sqlite fts5, facts temporal, auto-recall, konsolidasi
internal/verify/     deteksi & jalankan cek proyek
internal/sandbox/    landlock/seccomp, sandbox-exec, gerbang risiko
internal/bench/      ukur startup, RAM, token, skor
```

### 4.7 Roadmap
1. **v0.1 ✅ selesai:** loop + 5 tool paralel + klien OpenAI-compatible & Anthropic (API key, Ollama, 15 provider bawaan + custom) + output ringkas + gerbang risiko dasar + sesi `-c` + **`agentium bench`**.
   - **Hasil terukur:** binary 6,6 MB, startup ~2,5 ms, RSS ~7 MB, overhead prompt+tool ~600 token.
   - Target di bagian 3 sudah terlampaui untuk startup (< 30 ms), binary (< 15 MB), RAM (< 30 MB), dan overhead (≤ 800 token).
   - Belum diuji dengan API model sungguhan karena tidak ada API key di lingkungan build. Semua tes memakai server model palsu untuk kedua protokol.
2. **v0.2:** memori (USER/MEMORY snapshot, FTS5, auto-recall, `@remember`/`@decide`) + elision.
3. **v0.3:** verifikasi otomatis + sandbox + gerbang risiko + profil adaptif per model.
4. **v0.4:** klien Gemini, models.dev penuh, OAuth ChatGPT/Copilot/OpenRouter, Bedrock/Vertex/Azure, fallback, fast mode.
5. **v0.5:** observational memory + fakta temporal + konsolidasi `tidy`, lalu uji dengan LongMemEval subset.
6. **v0.6:** jalankan Terminal-Bench 2.1 dan bandingkan dengan Pi, Codex, Claude Code, dan OpenCode pada model yang sama. Publikasikan hasilnya.

---

## Sumber
**Agent & versi**
- Claude Code: https://www.gradually.ai/en/changelogs/claude-code/ · https://code.claude.com/docs/en/memory · https://www.contextstudios.ai/blog/claude-code-goes-native-binary-shift-for-ai-dev-tooling
- Overhead token Claude Code/OpenCode/Pi: https://www.developersdigest.tech/blog/claude-code-token-overhead-opencode-comparison
- Codex: https://developers.openai.com/codex/changelog · https://codex.danielvaughan.com/2026/04/08/codex-cli-memory-internals/
- OpenCode: https://github.com/anomalyco/opencode/releases · https://github.com/anomalyco/opencode/issues/11399
- Pi: https://pi.dev/ · https://github.com/earendil-works/pi/releases · https://mariozechner.at/posts/2025-11-30-pi-coding-agent/
- Gemini → Antigravity: https://developers.googleblog.com/an-important-update-transitioning-gemini-cli-to-antigravity-cli/
- Crush: https://github.com/charmbracelet/crush · Goose: https://goose-docs.ai/blog/2026/04/07/goose-moves-to-aaif/ · Amp: https://ampcode.com/chronicle · Droid: https://docs.factory.ai/changelog/release-notes
- Hermes: https://github.com/NousResearch/hermes-agent/releases · https://hermes-agent.nousresearch.com/docs/user-guide/features/memory · https://hermes-agent.nousresearch.com/docs/integrations/providers · https://github.com/NousResearch/hermes-agent/issues/5726 · https://www.sentinelone.com/vulnerability-database/cve-2026-10223/
- OpenClaw: https://github.com/openclaw/openclaw/releases · https://docs.openclaw.ai/concepts/memory · https://docs.openclaw.ai/providers · https://conscia.com/blog/the-openclaw-security-crisis/ · https://regolo.ai/how-to-benchmark-memory-usage-between-hermes-agent-and-openclaw/

**Benchmark & desain harness**
- Terminal-Bench 2.0: https://llm-stats.com/benchmarks/terminal-bench-2 · 2.1: https://codex.danielvaughan.com/2026/06/11/terminal-bench-2-1-june-2026-benchmark-landscape-codex-cli-harness-engineering-model-scores/
- SWE-bench Pro: https://labs.scale.com/leaderboard/swe_bench_pro · mini-swe-agent: https://github.com/SWE-agent/mini-swe-agent
- Pi di Terminal-Bench: https://www.zenml.io/llmops-database/building-pi-a-minimal-extensible-coding-agent-framework
- Studi desain harness (arXiv 2609.20804): https://github.com/jjakimoto/research-issues/issues/1640
- Prompt caching: https://arxiv.org/abs/2601.06007 · Claude fast mode: https://platform.claude.com/docs/en/build-with-claude/fast-mode
- Codex-Spark: https://openai.com/index/introducing-gpt-5-3-codex-spark/ · Cerebras vs Groq: https://www.spheron.network/blog/groq-lpu-vs-cerebras-wse-cheapest-inference-2026/

**Memori**
- Mastra OM: https://mastra.ai/research/observational-memory · Hindsight: https://arxiv.org/abs/2512.12818 · Zep: https://arxiv.org/abs/2501.13956 · Honcho: https://plasticlabs.ai/blog/research/Benchmarking-Honcho · Audit klaim vs observasi: https://www.maximem.ai/blog/state-of-ai-memory-2026-claimed-vs-observed · LongMemEval: https://arxiv.org/abs/2410.10813

**Login & registry**
- models.dev: https://github.com/sst/models.dev
- Anthropic OAuth: https://www.betterclaw.io/blog/openclaw-anthropic-subscription-ban
- ChatGPT OAuth: https://manifest.build/blog/chatgpt-plus-tokens-third-party-harnesses/
- Copilot: https://github.blog/changelog/2026-01-16-github-copilot-now-supports-opencode/
- Google: https://docs.openclaw.ai/providers/google
