# TESTING

## (a) Demo hesapları

Hepsinin parolası **`Demo1234!`**. Seed her `make reset-demo` sonrası aynı veriyi üretir (`SEED=42`).

| Rol | E-posta | Arayüzde açılan ekran |
|---|---|---|
| Müşteri | `musteri@sahnebilet.test` | Seanslar, koltuk seçimi, Biletlerim |
| Gişe görevlisi | `gise@sahnebilet.test` | Gişe satışı, Siparişler, Hediye çeki |
| Salon amiri | `amir@sahnebilet.test` | Seanslar (kural istisnası), Salon düzeni, Giriş çakışmaları, Denetim kaydı |
| Programcı | `programci@sahnebilet.test` | Seans programı, Filmler/oyunlar, Kuponlar |
| Muhasebe | `muhasebe@sahnebilet.test` | Hasılat + dağıtımcı payı, Kuponlar, Siparişler, Denetim kaydı |
| Giriş kontrol | `kapi@sahnebilet.test` | Giriş tarayıcı (çevrimdışı mod), Çakışmalar |

Ek olarak yük testi ve simülatör için `yuk001@sahnebilet.test` … `yuk200@sahnebilet.test` müşteri hesapları vardır (parola aynı). Seed'deki kupon ve hediye çekleri:
- Kuponlar: `HOSGELDIN10` (%10), `SINEMA50` (50 TL, en az 300 TL sepet), `TEKKULLANIM` (%20, tek kullanımlık)
- Hediye çekleri: `HC-DEMO-500` (500 TL), `HC-DEMO-100` (100 TL)

## Tek komutla doğrulama

```bash
cp .env.example .env
make verify          # = docker compose up -d --wait db api web && docker compose --profile verify run --rm verify
```

Çıktının sonunda her kimlik için PASS/FAIL tablosu basılır; herhangi bir kontrol başarısızsa komut sıfırdan farklı kodla çıkar. Doğrulama tekrarlanabilir: her kontrol kendine ait, kimsenin dokunmadığı bir seans seçer ve başlangıçta önceki koşulardan kalan kendi sepetlerini bırakır. Simülatör çalışırken art arda 3 koşuda da 23/23 geçer. Tamamen temiz bir başlangıç için önce `make reset-demo` çalıştırılabilir.

```
+--------+------+----------+----------------------------------------------------------------------------+
| ID     | RES  | TIME     | CHECK                                                                      |
+--------+------+----------+----------------------------------------------------------------------------+
| HEALTH | PASS |    0.00s | GET /health reports database ok                                            |
| AUTH-1 | PASS |    0.05s | wrong password is rejected with 401                                        |
| AUTH-2 | PASS |    0.00s | missing / invalid token is rejected with 401                               |
| AUTH-3 | PASS |    0.00s | role violations are rejected with 403 on the server                        |
| AUTH-4 | PASS |    0.05s | refresh token rotates and cannot be reused                                 |
| F-1    | PASS |    0.03s | hall and seat map; hall manager blocks a seat                              |
| F-2    | PASS |    0.01s | movies, programmer adds a film, keyset-paged showtimes                     |
| F-3    | PASS |    0.03s | online sale and box office sale                                            |
| F-4    | PASS |    0.02s | hold + cart: get, extend (max 2), release                                  |
| F-5    | PASS |    0.02s | price, coupon, gift card in a quote and an order                           |
| F-6    | PASS |    0.01s | signed QR, gate admits once, rejects reuse and forgery                     |
| F-7    | PASS |    0.02s | refund and revenue report                                                  |
| T-1    | PASS |    0.47s | 200 concurrent holds on the same seats: exactly one wins; expiry; no doubl |
| T-2    | PASS |    0.02s | single-seat rule with manager override; group and accessible suggestions   |
| T-3    | PASS |    0.01s | 10 halls x 2 weeks schedule generated and validated; overlaps impossible   |
| T-4    | PASS |    0.03s | dynamic price, coupon limits, partial gift card, refund tax split, distrib |
| T-5    | PASS |    0.02s | offline scanners: duplicate entry and refunded ticket reconciled on sync   |
| IDEM   | PASS |    0.01s | Idempotency-Key replays the same response, rejects a different body        |
| AUDIT  | PASS |    0.00s | audit log records critical actions and is append-only                      |
| OPS    | PASS |    0.00s | metrics, OpenAPI, uniform error body                                       |
| LIVE   | PASS |    0.01s | seat map changes are pushed over SSE (bonus)                               |
| AB     | PASS |    0.01s | campaign A/B: stable assignment, offer in cart, z-test report (bonus)      |
| WEB    | PASS |    0.00s | frontend is served and proxies the API                                     |
+--------+------+----------+----------------------------------------------------------------------------+
23 checks, 23 passed, 0 failed
```

## (b) Doğrulama kimlikleri: nasıl denenir, beklenen sonuç

Kontrollerin kodu: `backend/cmd/verify/main.go`. Uçların elle denenmesi için: [requests.http](requests.http).

| Kimlik | Nasıl denenir (API) | Beklenen sonuç |
|---|---|---|
| HEALTH | `GET /health` | `200 {"status":"ok","database":"ok"}`; DB kapalıyken `503` |
| AUTH-1 | `POST /api/auth/login` yanlış parola / bilinmeyen e-posta | `401 INVALID_CREDENTIALS` (bilinmeyen e-postada da aynı süre ve aynı yanıt) |
| AUTH-2 | `GET /api/me` token olmadan veya bozuk token ile | `401 UNAUTHENTICATED` |
| AUTH-3 | Müşteri → `GET /api/reports/revenue`, `POST /api/gate/scan`, `PATCH /api/seats/1`; giriş görevlisi → `POST /api/movies`, `POST /api/holds`; gişe → `POST /api/schedule/generate`; muhasebe → `POST /api/gate/sync`; programcı → `GET /api/audit` | hepsi `403 FORBIDDEN` |
| AUTH-4 | `POST /api/auth/refresh` aynı refresh token ile iki kez | birincisi `200`, ikincisi `401 INVALID_REFRESH_TOKEN` |
| F-1 | `GET /api/halls`, `GET /api/halls/{id}/seats`; amir `PATCH /api/seats/{id} {"kind":"blocked"}` sonra `standard` | 10 salon; haritada VIP, engelli ve kapalı (koridor) koltuklar; amir değişikliği `200`, denetim kaydında `seat.kind` |
| F-2 | `GET /api/movies`; programcı `POST /api/movies`; `GET /api/showtimes?limit=5` + `cursor` | ≥8 film; geçerli film `201`, geçersiz film `422`; 2. sayfa 1. sayfanın devamı, kesişim yok |
| F-3 | Müşteri hold + order; gişe hold + order (bir koltuk öğrenci) | müşteri siparişi `channel:"online"`, gişe siparişi `channel:"box_office"`; haritada 4 koltuk `sold` |
| F-4 | `POST /api/holds` → `GET /api/holds/{id}` → `POST …/extend` ×3 → `DELETE /api/holds/{id}` | ~10 dk süre; başkasının sepeti `404`; 3. uzatma `409 EXTENSION_LIMIT`; bırakılan koltuk `free` |
| F-5 | `POST /api/quote` (1 menü + `HOSGELDIN10`); gişe `POST /api/gift-cards`; o çekle `POST /api/orders` | indirim = biletlerin %10'u (menü indirimsiz); sipariş toplamı teklifle aynı; 100 TL çek tamamen kullanılır, bakiye 0 |
| F-6 | `GET /api/tickets/{id}/qr` → `POST /api/gate/scan` iki farklı kapıda; imzası bozulmuş QR; başkasının bileti | `ACCEPTED`, sonra `REJECTED_ALREADY_USED`; sahte imza `REJECTED_SIGNATURE`; başkasının QR'ı `404` |
| F-7 | 2 bilet + menü al, tek bileti iade et (`ticket_ids`); başkasının siparişini iade etmeyi dene; muhasebe `GET /api/reports/revenue` | `partially_refunded`, iade tutarı = o biletin ödenen tutarı; başkası `404`; raporda iade sayısı ≥1 ve `net + eğlence + KDV = tahsilat` |
| T-1 | 200 farklı müşteri aynı 2 koltuğa aynı anda `POST /api/holds`; çakışan 2 koltuklu sepet; kazananın süresini DB'de geçmişe çek; 300 alıcılı mini yük; `GET /api/showtimes/{id}/integrity`; doğrudan SQL ile ikinci bilet ekle | tam 1 × `201`, 199 × `409`; çakışan sepetin diğer koltuğu boş kalır; süresi dolan koltuk `free`, o tutma ile ödeme `409`; `integrity.ok = true`, `duplicate_seats = 0`; SQL eklemesi `23505` ile reddedilir |
| T-2 | Bir sıranın 2-3. koltuklarını tut; müşteri `override_single_seat`; amir override; `GET …/suggest?n=4`; `…?n=2&accessible=true` | `422 SINGLE_SEAT_RULE` (`details`'te boş kalacak 1. koltuk); müşteri override `403`; amir `201` ve denetim kaydında `hold.single_seat_override`; öneriler 4'lü, engelli koltuk içermez ve ilk öneri doğrudan satılabilir; engelli önerisi en az bir engelli koltuk içerir |
| T-3 | Programcı `GET /api/schedule/validate?from=<seed günü>&days=14`; `POST /api/schedule/generate {"from":"+40 gün","days":14,"save":false}`; dolu döneme `save:true`; SQL ile çakışan seans ekle | seed programı ≥560 seans, 0 ihlal; üretilen plan ≥560 seans, 0 ihlal; dolu döneme kayıt `409 PERIOD_NOT_EMPTY`; SQL eklemesi `23P01` (exclusion) ile reddedilir |
| T-4 | Seat map fiyatları; tek kullanımlık kupon (muhasebe oluşturur) → 2 sipariş → tam iade → tekrar; 500 TL hediye çekiyle sipariş + tek bilet iadesi; `GET /api/reports/distributor` | öğrenci < tam; menü 120 TL'de kalır, menüde eğlence vergisi 0; 2. sipariş `409 COUPON_EXHAUSTED`; tam iade `restore_coupon:true`, vergi toplamı = iade tutarı; sonra kupon yeniden kullanılabilir; çek bakiyesi = 500 − harcanan + iade edilen satırın çek payı; dağıtımcı payı = net × oran |
| T-5 | Aynı biletin 2 QR'ı; `GET /api/gate/state` ile açık anahtarı al; Go `gate.Scanner` ile 2 çevrimdışı kapı; A'yı iki kapıda kabul et; B'yi gişede iade et ve çevrimdışı kapıda kabul et; `POST /api/gate/sync` ×2, aynı yüklemeyi tekrar gönder | aynı kapıda ikinci okutma `REJECTED_ALREADY_USED`; sahte QR `REJECTED_SIGNATURE`; 2. kapının senkronu `DUPLICATE_ENTRY` + `ADMITTED_AFTER_REFUND`; tekrar yükleme yeni çakışma üretmez; senkron sonrası iki kapı da A'yı kullanılmış, B'yi iade edilmiş olarak reddeder; çevrimiçi B taraması `REJECTED_REFUNDED` |
| IDEM | Aynı `Idempotency-Key` ve gövdeyle `POST /api/holds` iki kez; aynı anahtar, farklı gövde; anahtarsız | aynı gövde + `Idempotent-Replayed: true`; farklı gövde `422 IDEMPOTENCY_KEY_REUSED`; anahtarsız `400` |
| AUDIT | Muhasebe `GET /api/audit`; SQL ile `UPDATE audit_log` | `order.create`, `order.refund`, `seat.kind`, `hold.single_seat_override`, `gate.sync_conflicts` kayıtları var; UPDATE `audit_log is append-only` hatasıyla reddedilir |
| OPS | `GET /metrics`, `GET /api/openapi.yaml`, bilinmeyen route, `/api/showtimes/abc/seats` | metrikler var; spec `/api/holds`'u içerir; `404` ve `400`, hepsi `{"error":{"code":…}}` biçiminde |
| LIVE (bonus) | `GET /api/showtimes/{id}/events` akışını aç, başka bir müşteriyle o seansta koltuk tut; token olmadan akış | `event: ready`, ardından 3 sn içinde `event: seats`; token olmadan `401` |
| AB (bonus) | Müşteri `GET /api/offers/me`; muhasebe `GET /api/reports/ab?from=&to=`; müşteri aynı rapor | teklif, `experiment.Assign`'ın öngördüğü kol; raporda 2 kol, `ödenen sepet ≤ sepet`, `0 ≤ p ≤ 1`; müşteri `403` |
| WEB | `GET http://localhost:3000/`, `GET http://localhost:3000/api/me` | arayüz açılır, `/api` nginx üzerinden API'ye iletilir |

## (c) Arayüzde elle deneme senaryoları (http://localhost:3000)

1. **Koltuk satın alma (müşteri):** `musteri@` ile giriş → Seanslar → bir seans → "Kişi sayısı 3" → **En iyi koltukları öner** → bir öneriye tıkla → **Koltukları 10 dk tut**. Sağ üstte geri sayım başlar. Bir koltuğu Öğrenci yap, menü 1, kupon `HOSGELDIN10`, çek `HC-DEMO-100` → fiyat kırılımı güncellenir → **Ödemeyi tamamla** → QR'lı bilet sayfası açılır.
2. **Tekli koltuk kuralı:** bir sıranın 2. ve 3. koltuğunu elle seç → tutmaya çalış → "Bu seçim sırada tek boş koltuk bırakıyor… Boş kalacak koltuk: X-1" mesajı çıkar. `amir@` ile aynı seçim, "kuralı aş" kutusu işaretlenince kabul edilir.
3. **Yarış:** iki ayrı tarayıcıda (biri gizli pencere) iki müşteriyle aynı koltukları seç ve neredeyse aynı anda tut. Biri sepete alır; diğeri "az önce başka biri tarafından alındı" mesajını görür ve harita yenilenir. Koltuk haritası 4 saniyede bir kendiliğinden güncellenir.
4. **Kısmi iade:** Biletlerim → sipariş → tek bileti iade et → iade tutarı, karta / çeke dağılımı ve KDV / eğlence vergisi kırılımı gösterilir; QR kaybolur.
5. **Çevrimdışı giriş:** müşteri bilet sayfasındaki QR'ın metnini almak için `GET /api/tickets/{id}/qr` kullan (ya da `requests.http`). `kapi@` ile giriş → Giriş tarayıcı → **Çevrimdışı mod** işaretle → QR metnini yapıştır → "GİRİŞ KABUL (çevrimdışı)" görünür; tekrar okutunca "BİLET KULLANILMIŞ". Kapı adını `KAPI-2` yapıp (temiz bir cihazı simüle etmek için tarayıcı depolamasını sil) aynı bileti tekrar kabul et → **Senkronize et** → "1 çakışma salon amirine bildirildi: DUPLICATE_ENTRY". `amir@` → Giriş çakışmaları ekranında görünür.
6. **Seans programı:** `programci@` → Seans programı → **Kayıtlı programı doğrula** (0 ihlal) → başlangıcı 2 ay sonrası yapıp **Yeni program üret (önizleme)** → ilk günün salon bazlı planı ve ihlal listesi.
7. **Salon düzeni:** `amir@` → Salon düzeni → bir koltuğa tıklayarak türünü değiştir (standart → VIP → engelli → kapalı). Satılmış bileti olan bir koltuğu kapatmaya çalışınca `SEAT_IN_USE` hatası gelir.
8. **Hasılat:** `muhasebe@` → Hasılat → tarih aralığı → film ve kanal bazında brüt, indirim, tahsilat, net, eğlence vergisi, KDV ve iade; altında dağıtımcı payı (vizyon haftası ve oran).
9. **Sistem canlı:** üst bardaki yeşil nokta ve "Son 1 dk: N sepet · N satış · N giriş" sayacı simülatör trafiğini gösterir.
10. **Canlı doluluk + cüzdan (bonus):**
    - Aynı seansı iki pencerede aç; harita üstündeki rozet "Canlı" yazar. Bir pencerede koltuk tutunca diğerinde sayfa yenilemeden turuncuya döner.
    - Ödeme sonrası bilet sayfasında "Cüzdan bileti" → tam ekran bilet. "Takvime ekle" bir .ics indirir. Ağı kesip (DevTools → Offline) "Cüzdanım"ı açınca bilet yine görünür.
11. **A/B (bonus):** müşteri sepetinde "Size özel: KOD" önerisi çıkar, Uygula'ya basınca fiyat güncellenir. `muhasebe@` → Hasılat ekranının altında "Kampanya A/B testi" tablosu ve karar kutusu.
12. **Program iyileştirme:** `programci@` → Seans programı → önizleme; sonuç kutusunda "İyileştirme turu: N film yer değiştirdi; kapasite × talep skoru X → Y" satırı.

## Kanıt komutları (görev kimliğiyle)

| Kimlik | Komut | Kanıtladığı |
|---|---|---|
| T-1 | `make verify` (T-1 satırı) | 200'lük yarışta tek kazanan, atomik sepet, süre dolumu, bütünlük, DB'nin ikinci bileti reddetmesi |
| T-1 | `cd backend && TEST_DATABASE_URL=… go test -run 'Concurrent\|Atomic\|Expiry\|Extend' -v ./internal/service/` | aynı şeyler servis katmanında |
| T-1 | `make load` (k6, 2000 VU) | yük altında 5xx yok, `integrity.ok`, gecikme yüzdelikleri |
| T-2 | `cd backend && go test -v ./internal/domain/seating/` | kural ve öneri algoritmasının 13 senaryosu |
| T-2 | `… go test -run SingleSeat -v ./internal/service/` | eşzamanlı iki sepet tek boşluk yaratamaz; amir istisnası denetimde |
| T-3 | `cd backend && go test -v ./internal/domain/schedule/` | 10 salon × 14 gün üretim + doğrulayıcının çakışma, saat, süre, asgari ve prime ihlallerini yakalaması |
| T-3 | `make verify` (T-3 satırı) | seed programının ve yeni planın doğrulanması, `EXCLUDE` kısıtı |
| T-4 | `cd backend && go test -v ./internal/domain/pricing/` | fiyat kuralları tablosu, vergi toplamı, kupon / çek dağıtımı, iade, dağıtımcı payı |
| T-4 | `… go test -run 'Coupon\|GiftCard\|Refund' -v ./internal/service/` | eşzamanlı kupon ve çek kullanımı, iade sonrası bakiye |
| T-5 | `cd backend && go test -v ./internal/domain/ticket/ ./internal/domain/gate/` | imza, sahtecilik, zaman penceresi, çevrimdışı tek kullanım, uzlaştırma kuralları |
| T-5 | `… go test -run Gate -v ./internal/service/` | 10 kapı aynı anda: tek kabul; senkron çakışmaları; tekrar yükleme no-op |
| T-3 | `cd backend && go test -run Improve -v ./internal/domain/schedule/` | iyileştirme skoru artırır, kısıtları bozmaz, sonraki seansa ve kapanışa uyar |
| LIVE | `… go test -run LiveSeatEvents -v ./internal/service/` | tutma ve bırakmada bildirim gelir, başka seansın abonesine gelmez |
| AB | `cd backend && go test -v ./internal/domain/experiment/` | deterministik ve dengeli atama, deneyler arası bağımsızlık, z-testi |
| Kaos | `make chaos` | DB dururken `/health` 503 ve hızlı hata; DB geri gelince yeniden başlatmadan 200 |
| Kapsama | `make cover` | domain paketleri satır kapsaması (%97,7) |

Entegrasyon testleri gerçek bir PostgreSQL ister; veritabanını silip yeniden seed ederler, bu yüzden **ayrı bir veritabanı** kullanın:

```bash
docker run -d --name sbtest -e POSTGRES_PASSWORD=t -p 55432:5432 postgres:16-alpine
cd backend && TEST_DATABASE_URL="postgres://postgres:t@localhost:55432/postgres?sslmode=disable" go test -count=1 ./...
```
