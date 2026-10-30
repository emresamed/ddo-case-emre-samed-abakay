# SahneBilet — Sinema ve Tiyatro Bilet, Koltuk ve Seans Yönetimi

**Geliştiren:** Emre Samed Abakay · DDO Yazılım Ekibi alım projesi

Çok salonlu bir sinema/tiyatro zinciri için seans programı, koltuk satışı (online + gişe), kampanya/kupon/hediye çeki, QR ile giriş kontrolü, iade ve hasılat raporlarını yöneten sistem. Go API + PostgreSQL 16 + bağımlılıksız (vanilla JS) arayüz, hepsi Docker Compose ile tek komutla ayağa kalkıyor.

## Kurulum

```bash
git clone https://github.com/emresamed/ddo-case-emre-samed-abakay && cd ddo-case-emre-samed-abakay
cp .env.example .env
docker compose up --build
```

| Servis | Adres | Not |
|---|---|---|
| Arayüz | http://localhost:3000 | nginx (root olmayan imaj), `/api` isteklerini API'ye iletir |
| API | http://localhost:8080 | `GET /health`, `GET /metrics`, `GET /api/openapi.yaml` |
| PostgreSQL 16 | yalnızca compose ağında | host'a port açılmaz |
| Simülatör | ayrı servis | tohumlu trafik ve 10 dakikada bir "vizyon sabahı" yığılması |

Migrasyonlar ve seed API açılırken otomatik çalışır; elle yapılacak bir adım yoktur. Bütün servislerde `healthcheck` tanımlıdır ve `depends_on` ile `service_healthy` koşulu kullanılır.

| Komut | Ne yapar |
|---|---|
| `make verify` | Ayaktaki sistemde F-1…F-7, T-1…T-5, health, auth ve yetki senaryolarını uçtan uca dener, PASS/FAIL tablosu basar, hata varsa sıfırdan farklı kodla çıkar (kontrollerin kendisi birkaç saniye sürer) |
| `make load` | k6 ile 2000 sanal müşterinin aynı seansa aynı anda koltuk saldırısı |
| `make reset-demo` | Veritabanını silip aynı tohumla yeniden seed eder |
| `make chaos` | PostgreSQL'i durdurur, API'nin bozulduğunu (503) ve DB geri gelince yeniden başlatma gerekmeden toparlandığını gösterir |
| `make test` | Birim ve entegrasyon testleri (entegrasyon testleri için `TEST_DATABASE_URL` gerekir) |

**CI kanıtı:** GitHub Actions'taki `e2e` işi her push'ta temiz bir Linux makinede `docker compose up --build` + `make verify` (21/21 PASS) + `make chaos` (PASS) çalıştırır; `test` işi lint, race detector'lı testler ve kapsama kapısını (domain ≥ %70, şu an %97,7) koşturur.

Demo hesapların (her rol için bir hesap, parola `Demo1234!`) listesi ve elle deneme senaryoları için: [TESTING.md](TESTING.md).

## Mimari

```
frontend/ (nginx)  ──/api──▶  backend/cmd/api (Go, net/http)
                                 │
                 httpapi ── middleware: request-id, JSON log, metrik, timeout, auth/RBAC, Idempotency-Key
                    │
                 service ── transaction + SQL (pgx), denetim kaydı
                    │
                 domain/ ── saf iş kuralları: I/O yok, saat yok, %97,7 test kapsamı
                   ├─ seating   tekli koltuk kuralı, grup önerisi        (T-2)
                   ├─ pricing   dinamik fiyat, kupon, hediye çeki, vergi, iade, dağıtımcı payı (T-4)
                   ├─ schedule  çizelgeleyici + bağımsız kısıt doğrulayıcı  (T-3)
                   ├─ ticket    Ed25519 imzalı QR                          (T-5)
                   └─ gate      çevrimdışı tarayıcı + senkron uzlaştırma    (T-5)
                    │
              PostgreSQL 16 ── FK/UNIQUE/CHECK, EXCLUDE (seans çakışması), kısmi UNIQUE (çift satış),
                               append-only denetim tetikleyicisi
```

- **`backend/cmd/simulator`**: ayrı bir compose servisi. Aynı `SEED` her zaman aynı seçim dizisini üretir. Arayüzün üst barındaki "Son 1 dk: … sepet · … satış · … giriş" göstergesi bu trafiği canlı olarak gösterir.
- **`backend/cmd/verify`**: `make verify`'ın çalıştırdığı uçtan uca doğrulayıcı.
- **Frontend tercihi:** build adımı olmayan vanilla JS + elle yazılmış CSS. Gerekçeleri: (1) giriş tarayıcısı ağ yokken de çalışmalı; harici bir framework veya CDN'e bağlı olmayan sayfa bir kez yüklendikten sonra çevrimdışı kalabilir. (2) İmaj küçük ve derlemesi saniyeler sürüyor. (3) Değerlendirmenin ağırlığı backend'de. Bedeli şu: ~1000 satırlık tek bir `app.js`. Daha büyük bir ekip için React + TypeScript'e geçmek mantıklı olur.

### Rol ve yetki matrisi (sunucu tarafında her uçta kontrol edilir)

| Uç nokta | müşteri | gişe | salon amiri | programcı | muhasebe | giriş |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| Salon / film / seans / koltuk haritası okuma | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Koltuk tutma, sepet, sipariş, fiyat teklifi | ✓ | ✓ | ✓ | | | |
| Tekli koltuk kuralını aşma | | | ✓ | | | |
| İade | kendi siparişi, seanstan 60 dk öncesine kadar | ✓ | ✓ | | | |
| Bilet QR'ı | kendi bileti | ✓ | | | | |
| Koltuk türü değiştirme (arızalı/engelli/VIP) | | | ✓ | | | |
| Film ekleme, seans programı üretme | | | | ✓ | | |
| Program doğrulama | | | ✓ | ✓ | | |
| Kupon oluşturma / listeleme | | | | listeleme | ✓ | |
| Hediye çeki satma / bakiye sorgulama | bakiye | ✓ | | | | |
| Hasılat ve dağıtımcı raporu | | | | | ✓ | |
| QR tarama, çevrimdışı senkron | | | | | | ✓ |
| Giriş çakışmaları | | | ✓ | | | ✓ |
| Denetim kaydı | | | ✓ | | ✓ | |

Token yoksa veya geçersizse **401**, token geçerli ama rol yetkisizse **403** döner. Bu davranış `make verify` içindeki AUTH-2 ve AUTH-3 ile doğrulanır.

## Zorlu görevler: nasıl çözüldü

### T-1 Koltuk satışı yarışı ve geçici tutma
- Envanter `show_seats` tablosunda tutulur: her seans için koltuk başına bir satır, eşzamanlılık kontrolünün birimi bu satırdır.
- `POST /api/holds` tek transaction içinde şunları yapar:
  1. Seçilen koltukların **bulunduğu sıralardaki tüm koltukları** `seat_id` sırasıyla `SELECT … FOR UPDATE` ile kilitler. Sabit sıra deadlock'u önler.
  2. Uygunluk ve tekli koltuk kuralını kontrol eder.
  3. `UPDATE` ile koltukları tutar. Koltuklardan biri bile alınamazsa hiçbiri alınmaz.
- `SET LOCAL lock_timeout = '3s'`: sıcak bir sırada bekleyen istek askıda kalmaz, `409 BUSY` alır.
- **Tutma süresi:** 10 dakika (`HOLD_TTL`), en fazla 2 kez uzatılabilir. Okumalar süresi dolmuş tutmayı zaten boş sayar. Arka plandaki süpürücü 15 saniyede bir `FOR UPDATE SKIP LOCKED` ile süresi dolmuş satırları temizler; birden fazla API kopyası çalışsa da birbirini beklemez.
- **Son savunma hattı:** `order_lines (showtime_id, seat_id) WHERE kind='ticket' AND status<>'refunded'` üzerinde kısmi `UNIQUE` dizin. Uygulamada bir hata olsa bile veritabanı ikinci bileti reddeder; `make verify` bunu doğrudan SQL ile dener.

### T-2 Tekli koltuk kuralı ve grup önerisi
- Kural saf bir fonksiyondur: `seating.CheckSingleSeat`. Bir sırada iki dolu koltuk ya da dolu koltukla duvar/koridor arasında tek boş koltuk bırakan seçim reddedilir (`422 SINGLE_SEAT_RULE`, `details` alanında boş kalacak koltuklar). Seçimden önce zaten var olan tek boşluklar yeni seçime yüklenmez.
- Salon amiri `override_single_seat` ile kuralı aşabilir; bu işlem denetim kaydına yazılır.
- Kilit tüm sırayı kapsadığı için iki farklı sepet aynı sıraya aynı anda kuralı ihlal edecek şekilde yerleşemez. Bu, entegrasyon testiyle kanıtlanmıştır.
- **Grup önerisi:** bitişik N boş koltuk; skor = |sıra − ideal sıra (salonun %60'ı)| + 0,5·|blok merkezi − sıra merkezi|. Tekli koltuk bırakan bloklar elenir; böylece her öneri kural aşılmadan satılabilir. Engelli koltukları normal gruba hiç önerilmez; `accessible=true` istenirse blok en az bir engelli koltuğu içermek zorundadır.

### T-3 Seans çizelgeleme
- `schedule.Generate`: her salonu açılıştan kapanışa kadar doldurur (film süresi + 15 dk reklam + 20 dk temizlik).
  - Prime-time aralığında (18:00–22:00) prime payının en çok gerisinde kalan film seçilir.
  - Diğer saatlerde haftalık asgarinin en çok gerisinde kalan film seçilir; asgariler dolduktan sonra prime payını en az bozacak film seçilir.
  - Salonlar lobi yığılmasın diye 10'ar dakika kaydırılarak başlar.
- `schedule.Validate`: üreticiden bağımsızdır; çakışma (temizlik dahil), açılış saatleri, gösterim süresi, haftalık asgari ve prime payını kontrol eder.
- Veritabanında `EXCLUDE USING gist (hall_id WITH =, tstzrange(starts_at, busy_until) WITH &&)` vardır; çakışan seans fiziksel olarak eklenemez.
- Seed 10 salon × 14 günlük programı bu üreticiyle oluşturur (≈ 750 seans) ve doğrulamadan geçirir.

### T-4 Fiyat, kampanya ve hasılat payı
- **Fiyat sırası:** gün (çarşamba halk günü −%20, cuma–pazar +%10) → saat (13:00 öncesi matine −%15, 18:00–23:00 prime +%10) → doluluk (≥%50 +%5, ≥%80 +%15) → öğrenci (−%25; çarşamba indirimiyle birleşmez, büyük olan uygulanır). Sonuç 50 kuruşa yuvarlanır.
- **Kupon:** yalnızca biletlere uygulanır, menüye uygulanmaz. İndirim satırlara "en büyük kalan" yöntemiyle dağıtılır, toplam kuruşu kuruşuna tutar. Kullanım hakkı satır kilidiyle korunur; 8 eşzamanlı sipariş testinde tek kullanımlık kupon bir kez kullanıldı.
- **Hediye çeki:** kısmen kullanılabilir; bakiye `CHECK (balance >= 0)` ile korunur. Kullanılan tutar satırlara dağıtılıp saklanır; iadede her satırın hediye çeki payı çeke, kalanı karta döner.
- **Vergi:** satır bazında `net + eğlence vergisi + KDV = ödenen`. Bu eşitlik DB'de `CHECK` kısıtıdır. Eğlence vergisi (%5) yalnızca bilete uygulanır, KDV (%10) ikisine de. Oranlar yasa değil yapılandırmadır (`pricing.DefaultRates`).
- **İade:** saklanan satır tutarları aynen geri alınır, yani yeniden hesaplama yapılmaz. Sipariş tamamen iade edildiğinde kupon hakkı geri verilir; kısmi iadede verilmez. Menü, son biletle birlikte iade olur.
- **Dağıtımcı payı:** net bilet hasılatı (KDV ve eğlence vergisi hariç, menü hariç) × vizyon haftasına göre kayan oran (1. hafta / 2. hafta / sonrası).

### T-5 Çevrimdışı çalışabilen giriş kontrol
- QR içeriği `SB1.<base64url JSON>.<Ed25519 imzası>` biçimindedir. Tarayıcı yalnızca **açık anahtara** ihtiyaç duyar; ele geçirilen bir cihazla bilet üretilemez. HMAC'te bu mümkün olurdu.
- Bilet seanstan 24 saat önce geçerli olmaya başlar ve seans bitiminde geçerliliğini kaybeder.
- **Çevrimiçi tarama:** `UPDATE … WHERE status='valid'` koşullu güncellemesi. 10 kapı aynı bileti aynı anda tarasa bile yalnızca biri kabul edilir.
- **Çevrimdışı tarama** (`gate.Scanner`; arayüzde WebCrypto Ed25519 ile aynı mantık): imza ve zaman cihazda doğrulanır, cihazda yerel bir kullanılmış listesi tutulur, kabul edilen girişler kuyruğa alınır.
- **Senkron** (`gate.Reconcile`): olaylar tarama zamanına göre sıralanır ve en erken tarama kazanır. Aynı bileti ikinci kez içeri alan kapı `DUPLICATE_ENTRY`, sonradan iade edilmiş bileti içeri alan kapı `ADMITTED_AFTER_REFUND` çakışması üretir. Çakışmalar saklanır, denetim kaydına yazılır ve salon amirinin ekranında görünür.
- Aynı yüklemenin tekrarı (zaman aşımı sonrası yeniden deneme) yeni bir çakışma üretmez. Senkron yanıtı cihaza güncel kullanılmış ve iade listelerini geri gönderir.

## Ortak teknik gereksinimler

| Gereksinim | Uygulama |
|---|---|
| Parola, oturum | bcrypt; 15 dk HS256 access token + DB'de özeti tutulan, her kullanımda dönen (rotation) refresh token |
| Migrasyon | goose, SQL dosyaları binary'ye gömülü (`backend/migrations`) |
| Para | `bigint` kuruş; float hiçbir yerde yok |
| Eşzamanlılık | `FOR UPDATE` (sıra kilidi), `SKIP LOCKED` (süpürücü), koşullu `UPDATE` (giriş), kısmi `UNIQUE` + `EXCLUDE` |
| İdempotans | `POST /holds`, `/orders`, `/orders/{id}/refund`, `/gift-cards` için `Idempotency-Key` zorunlu. Aynı anahtar ve aynı gövde saklanan yanıtı döndürür (`Idempotent-Replayed: true`); aynı anahtar ve farklı gövde 422 döner; 5xx yanıtlar saklanmaz |
| Denetim | `audit_log`; UPDATE/DELETE'i reddeden tetikleyiciyle eklemeli-yalnız |
| Hata biçimi | her yerde `{"error":{"code","message","details?"}}` |
| Sayfalama | keyset: seanslar `(starts_at,id)`, siparişler `(created_at,id)`, denetim kaydı `id` |
| Dayanıklılık | istek başına `context` zaman aşımı, `lock_timeout`, graceful shutdown, JSON log (gövde ve sorgu dizesi loglanmaz), tüm yapılandırma ortam değişkenlerinden |
| Gözlemlenebilirlik | `/metrics` (Prometheus): HTTP sayaç ve histogramları, tutma/satış/iade/tarama sayaçları |
| Konteyner | çok aşamalı derleme; API `distroless:nonroot`, web `nginx-unprivileged` |

## Ölçümler

### k6: vizyon sabahı (`make load`, 2000 eşzamanlı sanal müşteri, tek seans)

Her sanal kullanıcı aynı anda merkeze yakın 1–4 koltuk seçer, tutar ve satın alır. Teardown aşamasında bütünlük ucu çağrılır.

| Metrik | Değer |
|---|---|
| Tutma isteği p50 / p95 / p99 / max | 47,5 ms / 228 ms / 275 ms / 345 ms |
| Tüm istekler p50 / p95 / p99 | 45,7 ms / 219 ms / 274 ms |
| Başarılı tutma → sipariş | 57 → 57 (satılan 112 koltuk = 112 geçerli bilet) |
| 409/422 (koltuk alınmış, kural, yoğunluk) | 1665 |
| API 5xx / beklenmeyen kod | **0** |
| Çift satış (`/integrity`) | **0** (`ok: true`) |
| TCP seviyesinde başarısız istek | 278 (%13,9), aşağıdaki nota bakın |

> **Ölçüm ortamı ve dürüst not:** Bu sonuçlar Windows 11 dizüstü bilgisayarda, API ve PostgreSQL 16 aynı makinede yerel süreç olarak çalışırken alındı (k6 ölçümü için; Docker kurulumu ve uçtan uca `make verify` ise CI'da Linux üzerinde doğrulandı). 278 istek sunucuya hiç ulaşmadı: Windows'un listen backlog'u, 2000 bağlantının aynı milisaniyede açılmasını kaldırmadı. API logunda bu istekler görünmüyor; API yalnızca 201/409/422 döndürdü. Linux'ta (Docker) backlog çok daha büyük olduğu için bu kayıp beklenmez. k6 betiği ağ hatalarını (`hold_network_error`) API hatalarından (`hold_other`, eşik 0) ayrı sayar.

### Dizinler: `EXPLAIN (ANALYZE)`, ham çıktı [docs/explain-analyze.txt](docs/explain-analyze.txt)

Ölçüm seed verisi üzerinde alındı: 744 seans, 113.859 `show_seats` satırı.

| Sorgu | Dizin | Dizinli | Dizinsiz (aynı transaction'da `DROP INDEX`, sonra `ROLLBACK`) |
|---|---|---|---|
| Süresi dolan tutmaları süpüren sorgu (`status='held' AND held_until <= now() … FOR UPDATE SKIP LOCKED`) | `show_seats_expiry_idx` kısmi: `(held_until) WHERE status='held'` | 0,014 ms, Index Scan, 1 buffer | 6,3 ms, Seq Scan + Sort, 740 buffer |
| Seans listesi keyset sayfası (`(starts_at,id) > ($c) ORDER BY starts_at,id LIMIT 51`) | `showtimes_starts_idx (starts_at, id)` | 0,045 ms, Index Only Scan, 3 buffer | 0,227 ms, Seq Scan + top-N sort |

Diğer kritik sorgular birincil anahtarla çözülür: koltuk haritası `show_seats_pkey (showtime_id, seat_id)` ile 0,07 ms.

## Doğrulama Soruları

### 1. En zorlandığın görev hangisiydi? Hangi alternatifleri düşündün, neden eledin?

**T-1 ile T-2'nin kesiştiği yer.** Tek başına "aynı koltuk iki kişiye satılmasın" sorunu koltuk satırını kilitleyerek kolayca çözülüyor. Asıl zorluk, tekli koltuk kuralının **komşu koltuklara** bağlı olması. A 1-2'yi, B 4-5'i aynı anda isterse ikisi de kendi başına geçerli; ama ikisi birlikte 3 numarayı yalnız bırakıyor. Yalnızca istenen koltuklar kilitlenirse iki transaction da kuralı geçer.

Düşündüğüm alternatifler:
- **`SERIALIZABLE` + yeniden deneme:** doğru sonuç verir, ama yoğun sırada serileştirme hataları ve yeniden denemeler patlar; gecikmeyi öngörmek zorlaşır. Elendi.
- **Seans başına advisory lock:** basit, ama 2000 kişiyi tek bir kuyruğa sokar; 10 salonun 20 sırası birbirini gereksiz yere bekler. Elendi.
- **Redis `SETNX` ile koltuk kilidi:** yeni bir bağımlılık getirir ve kuralı yine DB durumu üzerinde kontrol etmek gerekir; iki kaynağın tutarlılığını sağlamak gerekir. Elendi.
- **Seçilen çözüm:** etkilenen **sıraların** tüm koltuklarını `seat_id` sırasıyla `FOR UPDATE` ile kilitlemek. Kural tek bir sıra içinde tanımlı olduğu için bu kilit kapsamı tam yetiyor; farklı sıralar paralel ilerliyor. Sabit kilit sırası deadlock'u önlüyor. `TestSingleSeatRule_ConcurrentCartsCannotCreateOrphan` 5 turda tam olarak bir sepetin geçtiğini doğruluyor.

### 2. Çözümün kırıldığı ya da yetersiz kaldığı bir senaryo (bilinen sınırlamalar)

- **Sıcak sıra:** kilit sıra bazında olduğu için 2000 kişinin aynı sıraya yüklendiği anda o sıradaki istekler sıraya girer. `lock_timeout 3s` sayesinde askıda kalmazlar ama `409 BUSY` alabilirler. Dürüst bir "tekrar deneyin" yanıtı veriyoruz, ama kuyruk adaleti yok: önce gelen önce alır garantisi verilmiyor.
- **Tekli koltuk kuralı yalnızca tutma anında uygulanıyor.** Kuralı sağlayan bir sepetin komşusu sonradan iade edilir ya da tutması süresi dolarsa tek boşluk oluşabilir. Bu, kuralın "yeni seçim yeni boşluk yaratmasın" tanımına uygun, ama salonun genel durumu her zaman kurala uygun değil.
- **Çevrimdışı giriş çakışmayı engelleyemez, yalnızca sonradan tespit eder.** Ağ kesikken iki kapı aynı bileti kabul edebilir; bu senkronda `DUPLICATE_ENTRY` olarak raporlanır. Cihaz saatleri kaymışsa "en erken tarama kazanır" kuralı yanlış kapıyı birinci sayabilir.
- **Çizelgeleyici açgözlü (greedy):** kısıtları sağlıyor ve doğrulayıcıdan geçiyor, ama optimal değil; örneğin salon kapasitesini filmin talebiyle eşleştirmiyor. Ağır kısıtlarda (çok yüksek prime payı isteyen çok sayıda film) çözüm bulamayabilir; doğrulayıcı bunu raporlar, kaydetme reddedilir.
- **Vergi oranları basitleştirilmiş yapılandırmadır** (KDV %10, eğlence vergisi %5); belediyeye göre değişen oranlar modellenmedi.
- **Ödeme simüledir:** gerçek bir ödeme sağlayıcısı entegrasyonu yok; sipariş, ödeme alınmış kabul edilir.

### 3. Eklediğin iki dizin: hangi sorgu için, dizinli/dizinsiz fark?

Yukarıdaki "Dizinler" tablosu.
- **`show_seats_expiry_idx`** kısmi bir dizin: yalnızca `status='held'` satırlarını içerdiği için 114 bin satırlık tabloda bile birkaç yüz satırla sınırlı kalır. 15 saniyede bir çalışan süpürücü 6,3 ms'lik tam tablo taraması yerine 0,014 ms'de biter (≈ 450×). Tablo büyüdükçe dizinsiz maliyet doğrusal artar, dizinli maliyet sabit kalır.
- **`showtimes_starts_idx (starts_at, id)`** keyset sayfalamanın sıralama anahtarıyla birebir aynı; Index Only Scan ile yalnızca 3 buffer okunur, sıralama yapılmaz. 744 seansta fark küçük (0,045 ms'ye karşı 0,227 ms). Ama dizinsiz plan her sayfada tüm tabloyu tarayıp sıralar, aylarca birikmiş seansta sayfa maliyeti büyür; dizinli plan sabit kalır.

### 4. Yapay zekâ kullandıysan, reddettiğin ya da düzelttiğin bir çıktı (önce/sonra)

Bu projede Claude (Anthropic) ile çalıştım. Uçtan uca `make verify`'ı ilk koşturduğumda F-7 ve T-4 kırmızı yandı: iki biletli bir siparişin **tek** biletini iade etmek **bütün siparişi** iade ediyordu. Üretilen kod şuydu:

```go
// ÖNCE
for _, l := range lines {
    if len(want) == 0 || want[l.TicketID] {   // want: iade istenen bilet id'leri
        pick = append(pick, l)
        delete(want, l.TicketID)              // listeden düşür
    }
}
```

Hata şu: istenen tek bilet seçilip haritadan silindiği anda `len(want) == 0` koşulu "liste boş = her şeyi iade et" anlamına gelmeye başlıyor; döngünün geri kalanı siparişteki diğer biletleri de seçiyor. Birim testlerde yakalanmadı, çünkü bu mantık domain'de değil servis katmanındaydı ve o yol tek biletli siparişle test edilmişti.

```go
// SONRA
all := len(ticketIDs) == 0        // niyet döngüden önce, bir kez belirlenir
for _, l := range lines {
    if all || want[l.TicketID] {
        ...
```

Düzeltmeyle birlikte regresyon testi `TestRefund_PartialKeepsOtherTickets` eklendi (commit `aaba08d`). İkinci bir düzeltme: senkron yanıtındaki `conflicts` alanı `omitempty` olduğu için çakışma yoksa JSON'da hiç bulunmuyordu ve arayüz `undefined.length` hatası veriyordu. Bu hatayı tarayıcıda elle denerken buldum; alan artık her zaman dizi olarak dönüyor.

### 5. T-1 çözümünün doğru çalıştığını nasıl kanıtladın? Komut ve çıktı

Üç bağımsız kanıt var:

**(a) Uçtan uca: `make verify` (T-1 satırı).** 200 farklı müşteri aynı 2 koltuğa aynı anda istek atar. Tam olarak 1 kazanan ve 199 adet 409 beklenir. Ardından:
- çakışan bir sepetin atomik olarak tamamen reddedildiği kontrol edilir,
- kazananın tutma süresi veritabanında geçmişe çekilip koltuğun boşa düştüğü ve süresi dolmuş tutmayla ödeme yapılamadığı (`409`) kontrol edilir,
- 300 alıcılık bir mini yük sonrası `/integrity` çağrılır,
- veritabanına doğrudan ikinci bilet eklenmeye çalışılır ve `23505` hatası beklenir.

```
$ make verify
...
| T-1    | PASS |    0.47s | 200 concurrent holds on the same seats: exactly one wins; expiry; no doubl |
...
21 checks, 21 passed, 0 failed
```

**(b) Entegrasyon testi:**

```
$ cd backend && TEST_DATABASE_URL=postgres://… go test -run 'Concurrent|Atomic|Expiry' -v ./internal/service/
--- PASS: TestConcurrentHolds_ExactlyOneWinner (0.30s)
--- PASS: TestHold_MultiSeatIsAtomic (0.01s)
--- PASS: TestHold_ExpiryFreesSeatsAndBlocksCheckout (0.01s)
--- PASS: TestSingleSeatRule_ConcurrentCartsCannotCreateOrphan (0.01s)
```

**(c) Yük testi: `make load`.** Teardown çıktısı:

```
integrity: {"showtime_id":12,"sold_seats":112,"valid_tickets":112,"duplicate_seats":0,"sold_without_ticket":0,"ok":true}
✓ no double sale
hold_other.....................: 0
```

## Dokümanlar

- [TESTING.md](TESTING.md): demo hesapları, her doğrulama kimliği için nasıl denenir / beklenen sonuç, arayüzde elle deneme senaryoları, kanıt komutları
- [DECISIONS.md](DECISIONS.md): mimari karar kayıtları
- [requests.http](requests.http): bütün uçlar için VS Code REST Client koleksiyonu
- `GET /api/openapi.yaml`: OpenAPI 3 dokümanı (`backend/internal/httpapi/openapi.yaml`)
