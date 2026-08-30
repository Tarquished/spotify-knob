# spotify-knob

Bikin rotary knob di keyboard (LEOBOG Hi75C Pro) ngatur **volume Spotify doang**, bukan master volume Windows.

| Aksi | Hasil |
| --- | --- |
| Puter knob | Volume Spotify naik/turun (step 5%) |
| Pencet knob | Next track, langsung tanpa jeda |
| **Alt + pencet knob** | Previous track |
| **Alt + puter knob** | Buka antrean — lihat 4 lagu berikutnya, sekaligus pilih |
| Pencet selagi antrean terbuka | Putar lagu yang dipilih |
| **Ctrl + pencet knob** | Buka/tutup panel lirik yang ngikutin detik lagu |
| Tarik baris progress di panel lirik | Lompat ke detik itu |
| Tarik slider di header panel lirik | Atur transparansi panelnya |
| Klik dua kali baris lirik | Lompat ke detik baris itu |
| Klik ikon panah di header panel lirik | Buka lagu itu di aplikasi/web Spotify |
| Klik kartunya pakai mouse | Kartunya langsung hilang |
| **Shift + puter knob** | Lolos ke Windows, jadi master volume biasa (escape hatch) |

Tiap aksi munculin kartu di layar: volume berapa persen sekarang, atau lagu apa yang baru dipindah — lengkap sama album art dan warna aksen yang diambil dari cover-nya. Kartunya nggak muncul kalau ada app yang lagi fullscreen. Ctrl + pencet buka panel lirik terpisah yang baris-barisnya nyala ngikutin detik lagu.

Semuanya lewat **Spotify Web API** — nggak ada simulasi keystroke ke Spotify sama sekali, jadi fokus window dan throttling Chromium nggak ngaruh.

---

## Kenapa nggak pakai AutoHotkey

Brief awal mendesain ini sebagai dua proses: script AHK nangkep media key lalu POST ke daemon Go. Di sini media key ditangkep langsung di dalam daemon pakai **low-level keyboard hook Windows (`WH_KEYBOARD_LL`)** — persis mekanisme yang dipakai AHK di balik layar, cuma tanpa proses kedua.

Untungnya:

- Satu binary, satu proses. Nggak perlu install AHK, nggak perlu dua entry di startup.
- Nggak ada hop HTTP di jalur knob. Putaran langsung jadi update state di memori.
- Deteksi double-press pakai timer Go, bukan `KeyWait`. Bug "previous terus ke-next lagi" nggak mungkin kejadian karena nggak ada thread buffer yang bisa nge-replay pencetan kedua.

Endpoint HTTP-nya tetep ada (buat debugging, Stream Deck, atau apa pun), dan script AHK-nya tetep disertain di [`ahk/spotify-knob.ahk`](ahk/spotify-knob.ahk) sebagai fallback opsional.

---

## Setup

### 1. Spotify Developer Dashboard

Di https://developer.spotify.com/dashboard, buka app lo dan pastiin **Redirect URI** ini kedaftar **persis**:

```
http://127.0.0.1:8888/callback
```

Harus `127.0.0.1`, bukan `localhost` — sejak April 2025 Spotify nolak `localhost` dan cuma nerima literal loopback IP buat redirect URI non-HTTPS.

Kalau nanti pas auth muncul `INVALID_CLIENT: Invalid redirect URI`, ini penyebabnya.

### 2. Build

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1
```

Menghasilkan dua binary di `bin\`:

- `spotify-knob.exe` — ada console window. Pakai ini buat auth dan debugging.
- `spotify-knobw.exe` — tanpa console window. Pakai ini buat autostart.

### 3. Config

Config ada di `%APPDATA%\spotify-knob\config.json` dan **udah keisi client ID + secret lo**. Kalau mau bikin ulang, contohnya di [`config.example.json`](config.example.json).

**Diedit langsung kepakai, nggak perlu restart.** Daemon ngecek file-nya tiap 2 detik dan nerapin perubahannya hidup-hidup — termasuk `osd.scale` dan `osd.position`, yang bikin window overlay-nya dibangun ulang di tempat. Yang butuh restart cuma `port`, kredensial, dan `hotkeys`; kalau lo ubah itu, log-nya bakal bilang. Kalau file-nya kepotong atau JSON-nya rusak, setting yang lagi jalan dipertahankan dan alasannya dicatat, bukan daemon-nya mati. BOM dari Notepad/PowerShell juga ditoleransi.

| Field | Default | Fungsi |
| --- | --- | --- |
| `client_id` / `client_secret` | — | Dari Spotify Developer Dashboard |
| `port` | `8888` | Port loopback. Kalau diganti, redirect URI di dashboard ikut ganti |
| `volume_step` | `5` | Persen per klik knob |
| `debounce_ms` | `100` | Jendela coalescing putaran (klik pertama tetep dikirim langsung) |
| `resync_seconds` | `10` | Interval baca ulang volume asli dari Spotify |
| `track_guard_ms` | `150` | Jarak minimum antar perintah next/previous |
| `double_press_ms` | `0` | `0` = pencet langsung skip, previous lewat Alt+pencet. Isi `250` kalau mau double-press balik (lihat catatan di bawah) |
| `long_press_ms` | `450` | Lama tahan sebelum antrean kebuka (mode `hold` doang) |
| `peek_gesture` | `alt-turn` | Juga: `hold`, `off` |
| `peek_linger_ms` | `1200` | Antrean tetap tampil segini setelah knob dilepas |
| `peek_browse_ms` | `1800` | Tiap putaran manjangin jendela browsing jadi segini |
| `hotkeys` | `true` | `false` = matiin keyboard hook, HTTP doang (mode AHK) |
| `osd.enabled` | `true` | Matiin kartu di layar |
| `osd.scale` | `1.0` | Perbesar/perkecil kartu (`1.25` kalau kekecilan) |
| `osd.position` | `bottom-center` | Juga: `top-center`, `bottom-right`, `top-right` |
| `osd.hide_when_fullscreen` | `true` | Sembunyiin kartu pas ada app fullscreen |
| `osd.dismiss_on_click` | `true` | Klik **di kartunya** bikin dia langsung hilang |
| `osd.volume_hold_ms` | `1500` | Lama kartu volume nempel di layar |
| `osd.track_hold_ms` | `3000` | Lama kartu ganti lagu nempel di layar |
| `lyrics.enabled` | `true` | Matiin panel lirik sekalian |
| `lyrics.opacity` | `0.94` | Transparansi awal panel, 0.4–1. Slider di panelnya nimpa ini dan disimpan terpisah |
| `lyrics.scale` | `0` | `0` = ikut DPI sistem |
| `lyrics.fps` | `0` | `0` = ikut refresh rate monitor |
| `osd.fps` | `0` | `0` = ikut refresh rate monitor. Isi angka buat maksa (misal `60`) |

### 4. Authorize (sekali doang)

```bash
.\bin\spotify-knob.exe -auth
```

Browser kebuka ke halaman consent Spotify. Klik **Agree**. Refresh token disimpen ke `%APPDATA%\spotify-knob\token.json` (permission owner-only) dan access token di-refresh otomatis 60 detik sebelum expired — nggak nunggu request gagal dulu.

Jangan buka URL consent-nya manual di browser tanpa `-auth` jalan. Listener di port 8888 baru nyala pas `-auth` dipanggil; kalau belum, browser bakal kena `ERR_CONNECTION_REFUSED` pas Spotify redirect balik, dan `code`-nya kebuang.

Kalau terlanjur kejadian, code-nya masih nyangkut di address bar dan masih valid ~10 menit. Copy nilai `code=` dari URL itu (sampai sebelum `&state=`, kalau ada) terus:

```bash
.\bin\spotify-knob.exe -code AQATi5ILMiQ...
```

### 5. Jalanin

```bash
.\bin\spotify-knob.exe -verbose
```

Puter Spotify dulu (API butuh active device), terus puter knob-nya.

### 6. Autostart pas Windows nyala

Dari PowerShell **as Administrator**:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\install-startup.ps1
```

Ini bikin scheduled task yang jalanin `spotify-knobw.exe` pas logon — binary tanpa console window, jadi nggak ada terminal nongol sama sekali.

Kalau dijalanin **as Administrator**, task-nya didaftarin dengan RunLevel `Highest`. Itu disengaja: low-level keyboard hook dari proses non-elevated nggak nerima key selama window elevated lagi fokus (beberapa game dan tool jalan as admin), jadi knob bakal mati diem-diem di situ. Task Scheduler bisa jalanin elevated pas logon tanpa prompt UAC.

Tanpa admin script-nya tetep jalan, cuma didaftarin RunLevel `Limited` — semuanya normal kecuali di dalam window elevated. Jalanin ulang as admin kapan aja buat upgrade.

Kalau ada daemon lama yang masih megang port 8888 dan nggak bisa dimatiin (misal lo jalanin manual dari terminal admin), script-nya bakal bilang dan **nggak** nyoba start, biar nggak rebutan port. Tutup console-nya dulu, terus:

```powershell
Start-ScheduledTask -TaskName spotify-knob
```

Buat nyabut lagi:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\uninstall-startup.ps1
```

---

## Tampilan di layar

Kartunya digambar sendiri, bukan pakai window toolkit. Prosesnya bikin satu **layered window** (`WS_EX_LAYERED`) yang isinya dikirim utuh ke compositor lewat `UpdateLayeredWindow`, jadi alpha-nya per-pixel: sudut membulat beneran, drop shadow lembut, nggak ada kotak hitam di belakang.

Window-nya `WS_EX_TRANSPARENT` + `WS_EX_NOACTIVATE`, artinya klik tembus ke bawahnya dan fokus **nggak pernah** pindah ke dia — penting pas lagi main game. Dan `WS_EX_TOOLWINDOW` bikin dia nggak nongol di Alt+Tab.

Lihat contohnya tanpa nyentuh Spotify sama sekali:

```bash
.\bin\spotify-knob.exe -preview
```

### Kenapa previous pindah ke Alt+pencet

Double-press buat previous kelihatannya wajar, tapi dia bertabrakan langsung dengan spam next. Kalau lo pencet next lima kali dalam sedetik, jaraknya ~200ms — persis di dalam jendela double-press, jadi sebagian pencetan kebaca sebagai "previous" dan lo lompat ke belakang. Ini kejadian beneran dan keukur: lima pencetan cuma menghasilkan empat perintah, salah satunya `cmd=previous`.

Selain itu, jendela double-press **menunda setiap skip** selama jendelanya, karena satu pencetan belum bisa disebut single sebelum jendelanya lewat.

Dua-duanya hilang kalau previous punya gesture sendiri. Sekarang pencet = next seketika, Alt+pencet = previous seketika, nol ambiguitas dan nol jeda. Diukur ulang setelahnya: lima pencetan = lima `cmd=next` berurutan, dan lagu terakhir persis sama dengan yang disebut kartu terakhir.

Kalau lo lebih suka double-press, isi `double_press_ms: 250`. Hot reload aktif, jadi langsung kepakai — tapi jeda dan tabrakan spam-next itu ikut balik.

### Antrean: Alt + puter, pilih, putar

Tahan Alt lalu puter knob, dan kartunya berubah jadi daftar 4 lagu berikutnya lengkap sama cover. Putaran pertama membuka daftar di baris teratas; putaran berikutnya mindahin pilihan. Arah puterannya sama dengan arah volume: muter ke arah “naik” artinya maju — volume naik, atau pilihan maju ke lagu berikutnya (turun di layar). Daftarnya digambar urut waktu dari atas ke bawah, jadi knob dicocokin ke urutan antrean, bukan ke sumbu layar. Jendelanya diperpanjang tiap kali lo muter, jadi lo nggak dikejar waktu. Pencet knob buat mainin yang kepilih.

**Kenapa bukan tahan knob.** Desain awalnya memang tahan-knob, tapi di keyboard ini tahan lama diklaim duluan sama firmware keyboard-nya sendiri buat menu lighting. Itu terjadi di dalam keyboard — Windows nggak pernah lihat event-nya, jadi nggak ada software di sisi PC yang bisa mencegatnya, hook sekelas apa pun. Satu-satunya jalan adalah nggak memakai gesture itu. Gesture tahan masih tersedia lewat `peek_gesture: "hold"` buat keyboard yang nggak mengklaimnya.

Lompat ke tengah antrean nggak punya endpoint khusus di Web API. Mainin lagu lewat URI doang bisa, tapi itu ngebuang playlist yang lagi lo dengerin. Jadi kalau Spotify ngelaporin ada context (playlist/album), daemon minta context itu dengan offset ke lagu yang dipilih — playlist-nya utuh, antrean setelahnya tetep jalan. Buat lagu yang nggak punya context (misal hasil "Add to queue" manual), nggak ada yang perlu dijaga, jadi URI polos yang dipakai. Baris pertama antrean cukup pakai `next` biasa.

Semua timing gesture-nya satu state machine di [`cmd/spotify-knob/knob.go`](cmd/spotify-knob/knob.go), jalan di satu goroutine dengan timer yang lapor balik lewat channel — nggak ada state yang dipakai bareng, jadi nggak ada yang bisa race. Klik nggak bisa diputuskan sebelum jendela double-click tutup, dan nggak bisa dibedain dari awal tahan sebelum timer tahan-nya bunyi atau dibatalin sama pelepasan; itu sebabnya ini state machine beneran, bukan tumpukan timer.

### Volume vs progress: dua bentuk yang beda

Volume digambar sebagai **meter bersegmen** — satu segmen per klik knob, dua puluh segmen penuh, dan cuma selebar kolom teks. Progress lagu bentuknya beda jauh: **rel menerus** selebar kartu, punya jam di kiri-kanan, dan punya kepala putih di posisi pemutaran. Versi paling awal dua-duanya bar horizontal berujung bulat dan gampang ketuker; sekarang bahkan panjang, posisi, dan isinya beda.

### Progress lagu

Satu baris sendiri di bawah konten kartu: waktu berjalan di kiri, rel setebal 4px di tengah, durasi total di kanan, plus kepala putih kecil di posisi pemutaran. Kepala itu satu-satunya putih murni di kartu, jadi mata nemu posisinya duluan sebelum baca angkanya.

Sebelumnya ini cuma garis rambut 2px yang nempel di tepi bawah kartu. Akurat, tapi kekecilan sampai gampang kelewat — makanya kartunya ditinggiin dari 100 ke 116px supaya baris ini punya ruang sendiri, bukan nebeng di tepi.

Kolom jamnya dikunci selebar `0:00` minimal, jadi ujung relnya nggak geser-geser tiap detik pas digitnya ganti, dan nggak lompat pas durasi lagunya baru dateng. Kalau Spotify belum ngasih tahu panjang lagunya (misal pas kartu masih nunggu detail lagu baru), yang digambar cuma rel kosong tanpa jam — jujur, dan kartunya nggak jadi bolong.

Lagu yang baru dipindah selalu mulai dari nol — riwayat lagu nyimpen posisi terakhirnya, dan menampilkan itu bakal bikin lagu yang baru mulai kelihatan seperti nyambung di tengah. Posisinya nggak ditanyain ke Spotify tiap frame — daemon nyimpen bacaan terakhir plus waktunya, terus kartunya ekstrapolasi sendiri. Di-quantize ke pixel dan ke detik, jadi lagu 3 menit cuma bikin frame digambar ulang sekitar dua kali per detik.

## Panel lirik

Ctrl + pencet knob, dan muncul panel melayang yang nampilin lirik lagu yang lagi jalan, **baris yang lagi dinyanyiin di-highlight** ngikutin detiknya. Panelnya always-on-top, bisa digeser (tarik bagian header), bisa diresize (tarik pojok kanan-bawah), **transparansinya diatur pakai slider di header**, dan **baris progress di bawahnya bisa ditarik buat lompat ke detik mana pun**. Posisi, ukuran, dan transparansi terakhirnya diinget. Pencet Ctrl + knob lagi, atau klik tombol × di pojok, buat nutup.

Kalau lagunya nggak punya lirik, panelnya **nggak jadi kebuka** — yang muncul cuma kartu kecil di bawah yang bilang "No lyrics for this track".

### Liriknya dari mana

Ini bagian yang perlu dijelasin jujur: **Spotify Web API nggak nyediain lirik sama sekali.** Lirik yang lo lihat di aplikasi Spotify itu dari Musixmatch, lewat endpoint internal yang diotorisasi pakai cookie sesi web player — bukan pakai token OAuth yang daemon ini pegang. Nggak ada scope yang bisa diminta buat itu. Jadi opsinya cuma dua: nyolong cookie sesi Spotify lo, atau pakai sumber lain.

Gw pakai **[LRCLIB](https://lrclib.net)**: database lirik komunitas, gratis, tanpa API key, dan yang paling penting — nyediain format **LRC**, satu timestamp per baris. Itu persis bentuk data yang dibutuhin biar highlight-nya bisa ngikutin playhead.

Konsekuensinya juga harus jelas: liriknya diisi komunitas, jadi kadang ada typo, dan lagu yang kurang populer bisa aja belum ada. Badge kecil di header panel nulis `LRCLIB · SYNCED` (ada timestamp) atau `LRCLIB · TEXT` (cuma teks, nggak ada highlight), jadi lo selalu tahu lagi lihat yang mana.

### Milih rekaman yang benar

Nyari lirik pakai judul doang bakal dapet cover version, versi live, dan upload "- Topic" dari lagu yang sama. Jadi urutannya: endpoint `/api/get` dulu, yang nyocokin judul + artis + album + **durasi**; itu yang bikin hasilnya rekaman yang bener, bukan lagu berjudul sama.

Kalau meleset, baru jatuh ke `/api/search` dan hasilnya diskor sendiri: durasi paling ngaruh (beda panjang = rekaman beda, langsung dihukum berat), judul persis dapet nilai penuh, lirik ber-timestamp dinilai lebih tinggi dari teks polos. Kalau skor terbaiknya masih negatif, hasilnya dibuang dan dianggap nggak ada liriknya — **nampilin lirik lagu yang salah itu lebih buruk daripada nggak nampilin apa-apa**.

Spotify juga nulis semua featured artist dalam satu string ("A, B, C") sementara LRCLIB biasanya nyimpen di bawah artis utamanya doang, jadi kalau pencarian pakai artis nggak dapet apa-apa, diulang pakai judul doang.

### Buka atau nolak

Satu keputusan kecil yang bikin fiturnya nggak nyebelin: pencet knob itu harus **kebuka bawa lirik** atau **bilang nggak ada** — nggak boleh kebuka kosong terus ketutup lagi sendiri.

Jadi urutannya: cek cache dulu (termasuk cache "nggak ada", yang disimpan 7 hari). Kalau belum pernah dicari, tunggu hasilnya maksimal 450ms. Kebanyakan lookup selesai di bawah itu, jadi hampir selalu langsung ketahuan mau buka panel atau munculin kartu. Cuma kalau jaringannya lagi lambat panelnya kebuka duluan dalam keadaan loading, karena diem aja lebih buruk daripada nunjukin kalau lagi nyari.

Bedanya pas panelnya udah kebuka terus lagunya ganti: di situ lagu tanpa lirik **nggak nutup panelnya**, tapi nulis pesannya di dalam. Lo yang minta panel itu kebuka; nutup sendiri gara-gara satu lagu itu ngelawan maunya lo.

Dua aturan lain yang lahir dari bug beneran waktu ngerjain ini, ketahuan dari log bukan dari nebak:

- **Hasil lookup yang telat nggak boleh buka ulang panel yang udah lo tutup.** Kalau lookup-nya lambat, panelnya kebuka duluan dalam keadaan loading; kalau lo tutup sebelum kata-katanya nyampe, hasilnya dibuang. Ini persis bug yang sama kayak kartu OSD yang sempat hidup lagi setelah fade out. Cuma satu pengiriman yang boleh mengubah panelnya kebuka atau nggak: jawaban dari pencetan itu sendiri.
- **Pencetan kedua selagi lookup pertama masih jalan itu artinya batal, bukan buka dua kali.** Ditandai pakai generation counter; lookup yang generasinya udah basi diam-diam dibuang.
- **Nutup panel nggak boleh langsung kegambar balik.** Ini yang paling bikin bingung: daemon-nya jelas manggil hide, tapi window-nya kadang tetap kelihatan. Penyebabnya di loop render panelnya sendiri — event penutup diproses, terus iterasi yang sama lanjut render dan present, dan `UpdateLayeredWindow` itu yang sekalian nampilin window-nya. Jadi panelnya kegambar balik ke layar yang barusan lo tutup. Sekarang loop-nya ngecek ulang setelah ngeproses event: kalau udah nggak visible, iterasi itu dihentikan sebelum menyentuh present. Diukur: 11 dari 13 benar sebelum diperbaiki, 28 dari 28 sesudahnya (dua ronde, jeda acak 0,9–4 detik).

Satu lagi: pencet Ctrl + knob beberapa detik setelah PC nyala dulunya jawab "Nothing is playing" padahal Spotify jelas lagi main — polling pertama daemon belum mendarat. Sekarang kalau daemon belum tau apa-apa, dia maksa satu kali baca ke Spotify dulu sebelum menyimpulkan.

Hasilnya di-cache dua lapis — di memori, dan satu file JSON per lagu di `%APPDATA%\spotify-knob\lyrics\` — jadi lagu yang udah pernah dibuka tampil instan, bahkan setelah daemon-nya direstart.

### Gambar panelnya

Bahasa visualnya sengaja disamain sama kartu OSD: gradien grafit yang sama, border rambut yang sama, aksen dari cover album yang sama, dan baris progress yang sama persis. Panelnya harus kebaca sebagai produk yang sama dilihat dari lebih dekat, bukan aplikasi kedua.

Beberapa keputusan yang mungkin nggak kelihatan:

- **Nggak ada drop shadow.** Panelnya bisa diresize, dan shadow beneran berarti nge-blur ulang mask 900×1100 tiap frame pas lo narik pojoknya. Yang dipakai tiga garis luar bertumpuk dengan alpha menurun — nol biaya, dan tetep kebaca ngambang di atas background terang maupun gelap.
- **Bitmap-nya dialokasi sekali di ukuran maksimum.** Resize nggak bikin DIB baru; `UpdateLayeredWindow` cuma disuruh baca area yang lebih kecil dari bitmap yang sama. Itu yang bikin narik pojoknya mulus, bukan tersendat-sendat rebuild.
- **Posisi window diubah lewat present, bukan `SetWindowPos`.** Gerakan dan gambarnya jadi mendarat di update compositor yang sama, jadi panelnya nggak pernah "sobek" ngelawan isinya sendiri sewaktu digeser.
- **Baris yang lewat lebih redup dari baris yang belum.** Yang udah lewat 26% opacity, yang aktif 97%, yang belum 50%. Ditambah gradasi lembut di tepi atas dan bawah body, jadi baris yang keluar layar meleleh bukan kepotong.
- **Jeda instrumental digambar sebagai tiga titik berdenyut**, bukan dikosongin. Break 20 detik tanpa itu bakal kelihatan kayak panelnya nge-hang.
- **Warna cuma di satu tempat**: batang aksen kecil di sebelah kiri baris yang aktif. Kata-katanya sendiri tetep putih — nge-warnain teksnya bikin susah dibaca tanpa nambah informasi.

### Seek: tarik baris progress-nya

Baris progress di panel itu bukan cuma indikator — dia kontrol. Tarik handle-nya buat lompat ke detik mana pun; pencet di titik mana pun langsung lompat ke situ, karena klik itu cuma drag sepanjang nol.

Tiga hal yang bikin ini kerasa solid bukan lemot:

- **Selagi handle-nya ditahan, panelnya nampilin posisi handle, bukan posisi musiknya.** Jadi baris lirik yang di-highlight ikut gerak sambil lo nyeret. Lo bisa baca dulu sampai nemu baris yang lo mau, baru dilepas. Jam di kiri juga ikut nunjukin target, bukan posisi sekarang.
- **Posisi yang dilepas langsung dipakai lokal, sebelum request-nya dikirim.** Panelnya menggambar dari bacaan itu sampai 144fps; nunggu roundtrip API bakal keliatan sebagai rail yang mental balik selama satu panggilan API.
- **Polling yang lagi jalan pas lo seek itu bawa posisi sebelum seek.** Controller dan panel sama-sama nahan bacaannya sendiri 2,5 detik sesudah seek, kalau nggak rail-nya keliatan lompat balik ke bawah kursor. Penahannya diikat ke lagunya, jadi ganti lagu langsung ngebatalin.

Rail-nya juga membesar pas disentuh kursor dan kursornya berubah jadi tangan — itu satu-satunya petunjuk visual kalau baris itu bisa ditarik.

### Slider transparansi

Dulu transparansi cuma angka di config yang harus diedit terus di-reload. Sekarang ada slider-nya di header, di bawah tombol ×. Ditarik, frame berikutnya langsung dikomposisi di nilai baru — itu satu-satunya cara jujur milih transparansi, karena yang lo lihat itu ya benda yang lagi lo atur.

- **Batas bawahnya 0,40, bukan nol.** Di bawah itu tulisannya berhenti kebaca di atas background terang, jadi slider maupun config nggak nawarin ke situ.
- **Badge sumbernya ganti jadi angka persen selagi ditarik**, dan nempel 1,5 detik setelah dilepas — konfirmasi kalau nilai yang lo lepas itu nilai yang nyangkut.
- Nilainya disimpan di `lyrics-window.json` bareng posisi dan ukuran, **bukan** ditulis balik ke `config.json`. Itu nilai-nilai yang lo set dengan cara nyeret; nulisnya ke file yang lo edit tangan bakal bikin tiap seretan kelihatan kayak perubahan config buat hot reload-nya. Edit `config.json` langsung tetap menang, karena itu orang menyatakan maksud, bukan mindahin window.

Dua kontrol ini sama-sama pakai satu disiplin: `headerMetrics` dan `footerMetrics` menghitung posisi kontrolnya sekali, dan penggambar maupun hit-test-nya baca dari situ. Jadi yang kelihatan dan yang bisa diklik nggak akan pernah beda — kelas bug yang gampang banget kejadian kalau dua-duanya menghitung sendiri-sendiri.

### Klik dua kali baris lirik = lompat ke situ

Sama kayak tarik rail, cuma lebih presisi: klik dua kali baris mana pun dan lagu lompat ke detik baris itu persis. Klik pertama diinget bareng baris mana yang kena; klik kedua di baris **yang sama** dalam 400ms dihitung pasangan dan langsung seek — klik ketiga yang nempel di belakangnya nggak ngulang seek-nya lagi (pasangannya udah "dipakai").

Klik di baris yang beda dari klik sebelumnya, atau klik keduanya kelewat lambat, dianggap dua klik tunggal biasa — sama sekali nggak ganggu drag-scroll yang udah ada. Baris yang liriknya nggak ber-timestamp (`docState` bukan `docReady` yang synced) nggak bisa di-double-click sama sekali, karena nggak ada detik yang bisa dituju. Lagu yang durasinya nggak diketahui juga nggak bisa — gerbang yang sama yang nyembunyiin rail progress.

### Ikon "buka di Spotify"

Ditekan, daemon nyoba buka `spotify:track:...` URI-nya lewat `ShellExecuteW` — persis kayak lo dobel klik shortcut ke situ: kalau app Spotify-nya kepasang, dia yang kebuka dan langsung nunjukin lagunya; kalau nggak, fallback ke `https://open.spotify.com/track/...` di browser default. Ikonnya cuma muncul kalau ada lagu yang lagi diketahui (ada URI-nya) — bukan kontrol yang keliatan tapi nggak ngapa-ngapain kalau diklik.

**Versi pertamanya jelek**, dan itu ditunjukin langsung. Dulu dia pil berlabel teks "Open in Spotify" yang nangkring sendirian di bawah nama artis — satu-satunya elemen di panel yang kelihatan kayak kontrol UI yang ditempel belakangan, bukan bagian dari desainnya. Semua kontrol lain di panel ini (tombol ×, slider transparansi, rail progress) itu ikon kecil yang cuma bereaksi ke hover, tanpa label yang nempel terus. Pil teks itu satu-satunya yang keluar dari bahasa itu.

Sekarang jadi **ikon panah kecil sebaris sama tombol ×**, ukuran dan gaya hover-nya sama persis (lingkaran yang keisi pas disentuh kursor). Panahnya digambar pakai primitif vektor yang sama kayak silang tombol ×, bukan glyph font — percobaan pertama makai karakter panah Unicode di teksnya, dan itu nggak kerender di font yang dipakai, jadinya kotak kosong (tofu box). Bentuk vektor nggak punya kegagalan kayak gitu.

Discoverability-nya nggak dikorbankan: pas kursor nempel di ikonnya, badge sumber lirik di header (yang biasanya nulis "LRCLIB · SYNCED") ganti sebentar jadi "OPEN IN SPOTIFY" — slot yang sama juga dipakai buat nunjukin persentase pas slider transparansi ditarik. Satu slot, tiga peran, tergantung apa yang lagi disentuh.

`headerMetrics()` yang nentuin posisi ikon ini juga yang dipakai `hitZone` buat nge-tes klik — disiplin yang sama kayak rail progress dan slider: apa yang kelihatan dan apa yang bisa diklik nggak akan pernah beda tempat.

Terbukti live: diklik pas lagu lagi main, `hitZone` kekonfirmasi resolve ke ikon yang benar lewat log, dan mekanisme `ShellExecuteW`-nya sendiri (nggak berubah dari versi pil) udah dibuktikan sebelumnya buka window Spotify langsung ke halaman lagunya (ditangkep pakai `PrintWindow` biar kebaca walau ketutup app lain).

### Kenapa panelnya nggak ngerebut fokus

`WS_EX_NOACTIVATE`. Panelnya nerima klik, drag, dan resize, tapi nggak pernah ngambil fokus keyboard — jadi lo bisa nggeser panel liriknya di tengah game tanpa game-nya kehilangan input. Hit-testing-nya gratis dari per-pixel alpha: klik di pixel yang transparan (sudut membulatnya, misalnya) diteruskan ke window di bawahnya.

Kartu OSD dan panel ini kebetulan sama-sama layered window, tapi postur input-nya berlawanan total: kartu itu `WS_EX_TRANSPARENT` dan nggak boleh nerima apa-apa; panel ini harus nerima semuanya. Itu sebabnya dua window terpisah, bukan satu jenis kartu baru.

### Kenapa bisa 144fps

Versi pertama cuma jalan ~10fps. Gw profil, dan 70% waktu frame ternyata ada di jalur generik `image/draw`: gradien dikasih ke rasterizer sebagai `image.Image`, jadi tiap pixel manggil `At()` yang nge-box `color.Color` ke interface — satu alokasi heap per pixel, sekitar 120 ribu alokasi per frame.

Tiga perubahan:

1. **Semua fill non-flat sekarang loop komposit langsung.** Bentuknya dirasterisasi ke coverage mask, terus di-composite pakai loop ketat tanpa interface dan tanpa alokasi. Fill warna rata tetep lewat rasterizer, yang punya fast path buat warna uniform di atas RGBA.
2. **Rasterizer diukur seukuran bentuknya, bukan seukuran kanvas.** Bar volume setinggi 6px nggak lagi bayar clear satu kanvas penuh.
3. **Slide dan fade dipindah ke waktu present.** Kartunya dikomposisi sekali dalam posisi diam lalu di-cache; frame yang cuma gerak animasinya nggak digambar ulang sama sekali, cuma satu pass di atas buffer — pass yang emang tetep harus jalan buat tukar RGBA ke BGRA. Geseran vertikalnya diinterpolasi antar baris, jadi sub-pixel, bukan lompat per pixel.

Hasilnya **34ms → 1,66ms per frame** (2,13ms pas animasi jalan). Budget 144fps itu 6,9ms, jadi masih longgar. Pas kartunya cuma diem nunggu, nggak ada render dan nggak ada present sama sekali — CPU-nya nol.

Frame rate-nya ngikutin refresh rate monitor lewat `GetDeviceCaps(VREFRESH)`. Kalau mau lihat angka aslinya:

```bash
.in\spotify-knob.exe -preview -verbose
```

terus cek `osd frame rate` di `%APPDATA%\spotify-knob\daemon.log`.

Bar volume-nya pakai exponential smoothing yang berbasis waktu, bukan per frame — jadi waktu settle-nya sama persis di 60Hz maupun 144Hz.

### Warna aksen dari album art

Bar volume, halo di belakang cover, dan chip NEXT/PREVIOUS semuanya ngambil warna dari album art yang lagi diputer, jadi kartunya berubah nuansa ngikutin lagunya.

Ngambil rata-rata semua pixel hasilnya abu-abu, dan ngambil warna paling sering biasanya kena background hitam. Jadi tiap pixel "milih" dengan bobot berdasarkan seberapa berwarna dan seberapa mid-tone dia, hue-nya dirata-ratain melingkar di roda warna (biar merah nggak saling meniadakan sama magenta), terus hasilnya dipaksa masuk rentang yang tetep kebaca di atas kartu gelap. Cover yang murni hitam-putih jatuh ke hijau Spotify.

### Klik buat nutup

Klik mouse **di atas kartunya** bikin dia langsung fade out, nggak usah nunggu timer-nya habis. Yang dicek posisi kursornya, bukan sekadar ada klik — kalau klik di mana saja dihitung, kartunya bakal ketendang seketika tiap kali lo main game, karena mouse diklik terus-terusan.

### Kenapa frame-nya dikirim ulang berkala

Layered window yang ditampilkan sebelum punya isi digambar Windows sebagai **kotak hitam pekat**. Versi sebelumnya memanggil `ShowWindow` lebih dulu baru `UpdateLayeredWindow`, dan lebih parah lagi menandai frame sebagai "sudah tampil" walaupun update-nya gagal — jadi satu kegagalan sesaat meninggalkan kotak hitam yang nggak pernah diperbaiki. Itu yang muncul pas Alt+Tab.

Empat perbaikan: pixel-nya dikirim **sebelum** window ditampilkan; kegagalan update nggak lagi dianggap sukses sehingga dicoba lagi di frame berikutnya; frame-nya dikirim ulang tiap 250ms walaupun nggak ada yang berubah, karena compositor bisa membuang isi layered window tanpa memberi tahu siapa pun; dan `UpdateLayeredWindow` nggak lagi dioper DC layar yang dipegang seumur hidup window. DC dari `GetDC(0)` yang disimpan lama itu jadi basi saat komposisi desktop berubah — persis yang dipicu Alt+Tab dari aplikasi ber-akselerasi seperti Spotify — dan DC tujuan yang basi bisa mengomposisi frame tanpa alpha-nya.

Satu hal yang **sengaja tidak** dilakukan: menyembunyikan lalu menampilkan ulang window saat pindah fokus. Itu sempat dicoba sebagai perbaikan, dan justru jadi penyebab: membangun ulang surface di tengah transisi Alt+Tab membuatnya dikomposisi tanpa alpha. Terukur — 2 gagal dari 8 perpindahan dengan cara itu, 0 dari 25 tanpanya.

### Kenapa hook-nya dipasang ulang berkala

Windows mencopot low-level keyboard hook yang callback-nya sekali saja telat melewati `LowLevelHooksTimeout` (~300ms). Pencopotan itu senyap: nggak ada notifikasi, dan nggak ada API buat nanya hook-nya masih hidup atau nggak. Di mesin yang lagi sibuk main game, ini kejadian — knob-nya mati diam-diam sampai daemon di-restart.

Dua penangkalnya: thread hook-nya dinaikin prioritasnya supaya nggak kalah rebutan CPU, dan hook-nya didaftarin ulang tiap 20 detik. Diukur di mesin ini: sebelumnya mati dalam ~45 detik, sesudahnya masih hidup di menit ke-3,5.

### Kenapa nggak muncul pas fullscreen

Dua pengecekan, karena satu doang nggak cukup:

- `SHQueryUserNotificationState` — cara Windows sendiri nentuin boleh nampilin notifikasi atau nggak. Ini nangkep D3D exclusive fullscreen dan presentation mode.
- Perbandingan geometri: window yang lagi fokus nutupin seluruh monitor. Ini nangkep game borderless-windowed yang sama shell masih dianggap window biasa.

Kalau lo masuk fullscreen selagi kartunya lagi tampil, dia langsung fade out, nggak nunggu waktunya habis.

### Ganti lagu langsung kelihatan judulnya

Endpoint skip Spotify balik duluan sebelum player ngelaporin lagu barunya, jadi nunggu konfirmasi berarti nunggu 200-400ms sebelum judulnya nongol. Daemon nggak nunggu — dia udah tau duluan:

- **Next** diambil dari antrean Spotify sendiri (`GET /me/player/queue`), disimpan sepuluh lagu ke depan walaupun kartunya cuma nampilin empat — burst pencetan menghabiskan satu entri tiap kali, dan kehabisan di tengah burst persis yang bikin kartu nyebut lagu salah. Antreannya dijaga tetep fresh: di-refresh pas start, tiap resync 10 detik, dan tiap kali ganti lagu kekonfirmasi.
- **Previous** diambil dari riwayat lagu yang daemon lihat main. Ini bukan sekadar list: ada kursornya, jadi mencet previous dua kali jalan mundur beneran, bukan nunjuk lagu yang barusan ditinggal.

Jadi begitu knob dipencet, kartunya langsung muncul lengkap sama judul, artis, dan cover. Watcher di belakang tetep verifikasi ke API dan ngebenerin kartunya kalau tebakannya meleset (misal shuffle ganti antrean). Kalau lo mencet berkali-kali, watcher lama dibatalin sama yang baru, jadi request-nya nggak numpuk.

Watcher yang memverifikasi cuma **mengoreksi kartu yang masih tampil**, nggak pernah memunculkan yang baru. Kalau kartunya sudah selesai fade out sebelum lagunya benar-benar mulai, koreksinya dibuang — kartu yang bangkit lagi buat menyebut lagu sebagai "NEXT" padahal lagunya sedang diputar itu lebih buruk daripada tidak mengoreksi sama sekali.

Kartunya juga dimunculin **sebelum** request-nya dikirim, bukan sesudah balasannya dateng. Lagunya udah ketebak, jadi nunggu konfirmasi Spotify cuma nambahin roundtrip-nya ke latency yang kerasa. Kalau ternyata request-nya gagal, watcher yang punya kata terakhir.

Kalau antreannya belum kebaca (misal knob dipencet sedetik setelah daemon nyala), kartunya balik ke placeholder seperti sebelumnya.

### Nyoba panel lirik tanpa nunggu lagunya

```bash
.in\spotify-knob.exe -preview-lyrics "Daniel Caesar - Best Part"
```

Buka panelnya di lagu yang lo sebut, playhead-nya jalan sendiri dari nol, tanpa nyentuh Spotify sama sekali. Ini yang dipakai buat ngerjain desain panelnya — nggak perlu nunggu lagu yang bener muncul di player.

---

## Cara testing manual

**Daemon hidup?**

```bash
.\bin\spotify-knob.exe -status
```

```json
{
  "volume": 65,
  "target": 65,
  "device": "DESKTOP-XYZ",
  "supports_volume": true,
  "playing": true,
  "track": "Radiohead - Weird Fishes",
  "step": 5,
  "debounce_ms": 100
}
```

**Tanpa nyentuh knob** (buat misahin masalah hook vs masalah API):

```bash
curl -X POST http://127.0.0.1:8888/volume/up
```

```bash
curl -X POST http://127.0.0.1:8888/next
```

**Cek coalescing beneran jalan:** jalanin dengan `-verbose`, puter knob cepet-cepet 6 klik. Di log harus muncul enam baris `volume target` tapi cuma **dua** baris `volume set` (satu langsung, satu susulan ke nilai final).

**Cek slider di app ikut gerak:** ini yang gagal di pendekatan SoundVolumeView. Web API nyentuh volume control Spotify yang sama dengan slider di app, jadi slider-nya harus ikut geser.

**Log:** `%APPDATA%\spotify-knob\daemon.log`

---

## Test otomatis

```bash
go test ./...
```

Yang dicover:

- **Coalescing** — 6 klik cepat = maksimal 2 API call, mendarat di nilai final (`internal/controller`)
- **Leading edge** — putaran pertama nembak tanpa nunggu window debounce
- **Clamping** 0–100, **no active device** nggak bikin crash, **resync** ngambil perubahan volume dari app
- **Keyboard hook** beneran nangkep `Volume_Up/Down/Mute` (test-nya nyuntik key sintetis lewat `keybd_event` — key-nya ditelen hook, jadi volume mesin nggak keganggu)
- **Double-press** — dua pencetan rapat = 1 previous dan **0** next (regresi "previous terus ke-next lagi")
- **Siklus kartu** — enter, hold, exit, hilang; putaran baru pas lagi fade-out nyambung mulus tanpa kedip
- **Prediksi lagu** — next diambil dari antrean, previous dari riwayat, dan balik ke placeholder kalau dua-duanya kosong
- **Gesture knob** — delapan belas test buat state machine-nya: klik, double klik, Alt+klik, Alt+puter, Ctrl+klik, tahan, puter selagi peek, pilih lalu putar, dan balik ke volume setelah jendelanya tutup
- **Progress** — lagu baru mulai dari nol, dan kartu yang belum tau lagunya nggak mewarisi progress lagu sebelumnya
- **Antrean** — dua pencetan beruntun nyebut dua lagu berbeda, lima pencetan dalam sedetik nyebut lima lagu berurutan tanpa ngulang, pencetan bersamaan nggak pernah ngeklaim lagu yang sama, dan previous ngembaliin lagu yang ditinggal ke depan antrean
- **Klik buat nutup** — cuma klik di dalam kartunya yang ngitung, dan area kartunya ikut membesar pas mode antrean
- **Hook** — didaftarin ulang lalu tombol masih kebaca
- **Parser LRC** — timestamp, tag metadata yang nggak boleh jadi baris lirik di detik nol, satu baris dengan banyak timestamp, baris kosong sebagai jeda instrumental, dan urutan yang dibetulin
- **Milih rekaman** — durasi ngalahin judul yang sama, ber-timestamp ngalahin teks polos, dan lagu yang jelas salah ditolak daripada ditampilin
- **Buka atau nolak** — lagu tanpa lirik nggak pernah buka panel kosong, cache dipakai tanpa nyentuh jaringan, lookup lambat buka panel loading terus keisi, dan lagu tanpa lirik nggak nutup panel yang udah kebuka
- **Panel nggak buka sendiri** — hasil lookup yang mendarat setelah panelnya ditutup dibuang, ganti lagu di balik panel tertutup nggak munculin apa-apa, dan pencetan kedua selagi lookup jalan bikin batal bukan buka dua kali
- **Seek** — pemetaan posisi kursor ke detik lagu termasuk yang keluar ujung rail, highlight ngikutin handle selagi ditarik, bacaan basi dari polling nggak bisa nimpa seek yang baru, penahannya nggak kebawa ke lagu berikutnya, dan seek dilaporin tepat sekali
- **Slider transparansi** — pemetaannya bolak-balik konsisten (nilai → posisi handle → nilai yang sama), nilai di luar batas dijepit, dan opacity yang belum diset nggak kebaca sebagai "tembus pandang"
- **Klik dua kali baris lirik** — dua klik di baris yang sama dalam jendela waktunya nge-seek; dua klik di baris beda, atau kelewat lambat, nggak; klik ketiga nempel di belakang pasangan yang udah jadi nggak nge-seek dua kali; ganti lagu mutusin pasangan klik yang lagi ditunggu; lagu yang durasinya nggak diketahui nggak bisa di-double-click sama sekali
- **Tombol Open in Spotify** — nggak nongol kalau nggak ada lagu, nggak crash kalau callback-nya belum dipasang (mode preview), dan URI yang dikirim ke callback-nya persis URI lagu yang lagi aktif
- **Fallback web Spotify** — `spotify:track:ID` jadi `https://open.spotify.com/track/ID` dan sejenisnya, URI ngaco (kosong, ID kosong, skema yang bukan Spotify) ditolak jadi string kosong, dan urutan yang dicoba (app dulu baru web) selalu app duluan
- **Config** — BOM dari Notepad ditoleransi, UTF-16 ditolak dengan pesan yang jelas, key yang absen tetep pakai default
- **Riwayat lagu** — mundur berkali-kali jalan beneran, cabang baru ngebuang riwayat di depannya, dan ada batas maksimal
- **Warna aksen** — nolak background gelap, jatuh ke hijau Spotify kalau cover-nya monokrom
- **Render kartu** — semua varian digambar tanpa panic; PNG-nya bisa diliat sendiri:

```bash
OSD_OUT=C:	mp\osd go test ./internal/osd -run TestRenderSamples
```

Ada benchmark render-nya juga, buat mastiin nggak ada regresi performa:

```bash
go test ./internal/osd -run XXX -bench . -benchtime 300x
```

---

## Cara kerjanya

```
knob keyboard
     |  media key (VK_VOLUME_UP / DOWN / MUTE)
     v
WH_KEYBOARD_LL hook  --- ditelen, Windows nggak pernah liat key-nya
     |  channel (non-blocking)
     v
dispatch  --- timing single vs double press
     |
     v
controller  --- state volume + coalescing 100ms
     |  1 HTTP call per window
     v
Spotify Web API
```

### Kenapa perlu debounce

Web API cuma punya `PUT /me/player/volume?volume_percent=N` yang **absolut**, nggak ada increment relatif. Knob bisa ngirim 10+ event per detik. Kalau tiap event jadi satu API call, langsung kena rate limit.

Penjadwalannya **leading-edge**: klik pertama setelah knob diem dikirim **langsung**, tanpa nunggu window sama sekali — jadi satu klik kerasa instan. Klik yang dateng selagi request lagi jalan digabung ke target dan dikirim sekali sebagai susulan begitu window 100ms lewat. Cuma boleh ada satu request in-flight.

Puter 6 klik cepet dari 40%: satu call ke ~45 (langsung), satu call ke 70 (susulan). Dua call, bukan enam, dan lo langsung liat responnya di klik pertama.

Kenapa nggak sekadar dikecilin window-nya: trailing debounce murni selalu nambahin delay penuh satu window ke setiap putaran. Leading-edge ngilangin delay itu buat kasus paling sering (satu-dua klik) tanpa ngorbanin proteksi rate limit pas diputer kenceng.

### State volume dan drift

Karena API-nya absolut, daemon harus tau volume sekarang. Dia baca `GET /me/player` pas start dan tiap 10 detik. Resync dilewatin kalau ada write kita sendiri dalam 3 detik terakhir — biar hasil GET yang basi nggak nge-undo volume yang barusan diset.

### Error handling

| Kondisi | Perlakuan |
| --- | --- |
| Nggak ada active device (404 / 204) | State volume di-reset ke unknown, log info, nggak crash. Puteran berikutnya nyoba sync ulang |
| Rate limit (429) | Baca header `Retry-After`, tahan sampai lewat, baru kirim nilai final |
| Token expired (401) | Auto-refresh, retry sekali |
| Device nggak support volume (403) | Ditandain, knob berhenti nembak API sampai device ganti |
| Daemon mati | Knob balik jadi master volume Windows (hook-nya ikut mati) |

---

## Yang perlu diketahui

**Latency.** Roundtrip ke `api.spotify.com` diukur dari mesin ini: ~120–190ms buat koneksi baru, **~70ms** kalau koneksi di-reuse. Resync tiap 10 detik sekalian jadi keep-alive, jadi knob hampir selalu kena jalur ~70ms yang cepet.

Sisa delay yang kerasa datang dari Spotify sendiri: setelah API balikin OK, app desktop perlu waktu buat nerima perubahan itu lewat koneksinya sendiri sebelum slider-nya gerak. Itu di luar kendali tool ini.

**Spotify Connect.** Kalau lo lagi mutar di device lain (HP, speaker), knob ngontrol device itu, bukan desktop. API selalu ngikut *active device*. Ini fitur, bukan bug — tapi bisa ngagetin.

**Harus ada yang lagi diputer.** Kalau Spotify idle, nggak ada active device dan API balikin 404. Puter lagu dulu.

**Nggak semua device support volume.** Beberapa target Spotify Connect nolak volume control lewat API (`supports_volume: false`). Cek lewat `-status`.

**Window elevated.** Keyboard hook non-elevated nggak keliatan key-nya selama window as-admin lagi fokus. Makanya `install-startup.ps1` pakai privilege tertinggi.

**Premium.** Endpoint playback control butuh akun Premium.

---

## Struktur project

```
cmd/spotify-knob/main.go        wiring, flag, dispatch single/double press
internal/config                 config.json di %APPDATA%
internal/auth                   OAuth Authorization Code + auto-refresh token
internal/spotify                client Web API, error yang bertipe
internal/controller             state volume, debounce, resync
internal/hotkey                 low-level keyboard hook Windows
internal/lyrics                 lookup LRCLIB, parser LRC, cache dua lapis
internal/openurl                buka spotify:/https: URI lewat ShellExecuteW
internal/osd                    kartu di layar + panel lirik (layered window, renderer sendiri)
internal/server                 HTTP loopback (127.0.0.1 doang)
ahk/spotify-knob.ahk            fallback opsional, cuma perlu kalau hotkeys:false
scripts/                        build, install/uninstall autostart
```

Dependency cuma `golang.org/x/image` (rasterizer vektor + rasterisasi font), sisanya stdlib. Tetep satu binary tanpa runtime.

---

## Troubleshooting

| Gejala | Penyebab |
| --- | --- |
| `INVALID_CLIENT: Invalid redirect URI` | `http://127.0.0.1:8888/callback` belum kedaftar di dashboard |
| `listen ... address already in use` | Daemon udah jalan (cek `-status`), atau port 8888 kepake app lain |
| `ERR_CONNECTION_REFUSED` di `127.0.0.1:8888/callback` | URL consent dibuka manual tanpa daemon nangkep. Copy `code=` dari address bar, jalanin `-code <code>`, atau ulang pakai `-auth` |
| Knob nggak ngaruh, master volume ikut gerak | Daemon mati, atau `hotkeys: false` di config |
| Knob mati di satu game/app doang | App-nya elevated. Jalanin daemon as admin (`install-startup.ps1`) |
| `no active Spotify device` | Spotify lagi nggak mutar apa-apa |
| Kartu nggak muncul sama sekali | Cek `osd.enabled`, terus coba `-preview`. Kalau preview jalan tapi knob nggak, berarti masalahnya di sisi Spotify, bukan di kartunya |
| Kartu nggak muncul pas main game | Emang begitu desainnya. Set `osd.hide_when_fullscreen: false` kalau mau tetep muncul |
| Kartu kekecilan / kegedean | Ubah `osd.scale` |
| Animasi kerasa patah-patah | Cek `osd frame rate` di log pakai `-verbose`. Kalau jauh di bawah refresh rate monitor, kunci manual lewat `osd.fps` |
| Lirik bilang "No lyrics" padahal Spotify punya | Spotify pakai Musixmatch, panel ini pakai LRCLIB — koleksinya beda. Cek langsung di lrclib.net |
| Liriknya ada typo-nya | Isinya dari kontributor komunitas LRCLIB, bukan dari label |
| Liriknya meleset beberapa detik | Timestamp-nya ikut versi rekaman yang dipilih. Kalau lagunya punya beberapa upload di LRCLIB, yang durasinya paling dekat yang menang |
| Panel liriknya kegeser ke luar layar | Posisinya diinget di `%APPDATA%\spotify-knob\lyrics-window.json` — hapus file itu buat balik ke default |
| Panel liriknya kelewat transparan | Tarik slider di header ke kanan, atau hapus `lyrics-window.json` buat balik ke nilai config |
| Rail progress ditarik tapi lagunya nggak lompat | Cek log buat baris `seeked`; kalau nggak ada, kemungkinan Spotify nggak punya device aktif |
| Daemon kayak jalan tapi kartunya nggak pernah muncul | Kemungkinan port 8888 udah dipegang daemon lain. Sekarang ini kecatat sebagai `http listener failed` di log |
| Judul lagu next sempat salah lalu berubah | Antreannya udah basi (biasanya gara-gara shuffle). Watcher-nya emang sengaja ngebenerin sendiri |
| Album art nggak nongol | Cover diambil dari `i.scdn.co`; kalau diblokir, kartunya pakai placeholder piringan hitam |
| Edit config nggak ngefek | Cek baris `config reloaded` di log. Kalau nggak ada, lihat `config_path` di `-status` — daemon mungkin baca file lain dari yang lo edit |
| Antrean kosong pas dibuka | Spotify belum ngasih lookahead (biasanya di awal, atau lagi radio/autoplay) |
| Tahan knob malah buka menu lighting keyboard | Itu firmware keyboardnya, di luar jangkauan software PC. Pakai `peek_gesture: "alt-turn"` (default) |
| Knob mati sendiri setelah beberapa menit | Harusnya nggak lagi — hook-nya didaftarin ulang tiap 20 detik. Kalau masih, laporin isi log-nya |
| Kartu ilang secepat muncul pas main game | Klik di dalam kartunya yang bikin hilang. Matiin lewat `osd.dismiss_on_click: false` |
| Kotak hitam gede pas Alt+Tab | Harusnya nggak lagi. Kalau muncul, cek `overlay update failed` di log |
| Spam next malah mundur | `double_press_ms` nggak nol; pencetan cepat kebaca double-press. Isi `0` |
| Volume gerak tapi slider di app nggak | Laporin — harusnya nggak mungkin, keduanya nyentuh volume control yang sama |
