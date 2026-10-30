# Mimari Karar Kayıtları

Her kayıt: bağlam → karar → sonuçlar (bedeli dahil).

---

## ADR-1: Koltuk envanteri seans × koltuk satırı, kilit birimi "sıra"

**Bağlam.** 2000 kişi aynı anda aynı seansa yükleniyor. Çift satış kesinlikle olmamalı, çoklu koltuk atomik tutulmalı ve tekli koltuk kuralı komşu koltuklara bakıyor.

**Karar.** Her seans açılırken salonun her koltuğu için bir `show_seats` satırı oluşturulur (`free/held/sold`, `hold_id`, `held_until`). Tutma transaction'ı seçilen koltukların bulunduğu sıralardaki bütün satırları `seat_id` sırasıyla `SELECT … FOR UPDATE` ile kilitler, kuralları Go'daki saf fonksiyonla kontrol eder, sonra günceller. `lock_timeout = 3s`.

**Sonuçlar.**
- (+) Kural ile kilit kapsamı örtüşür: aynı sıradaki iki sepet sıralanır, farklı sıralar paralel ilerler. Sabit kilit sırası deadlock'u önler.
- (+) Envanter tek tabloda durur; koltuk haritası birincil anahtarla tek sorguda gelir.
- (−) Seans başına salon büyüklüğü kadar satır oluşur (14 gün × 10 salon ≈ 114 bin). PostgreSQL için küçük bir sayı.
- (−) Sıcak sırada bekleme olur; 3 saniyeyi aşarsa istemci `409 BUSY` alır ve yeniden dener.
- Reddedilen alternatifler: `SERIALIZABLE` (yoğun yükte yeniden deneme fırtınası), seans başına advisory lock (gereksiz serileştirme), Redis (ikinci kaynakla tutarlılık sorunu).

## ADR-2: Çift satışa karşı son savunma veritabanında

**Bağlam.** Uygulama katmanındaki her kontrol bir gün bir hatayla delinebilir.

**Karar.** `order_lines` üzerinde `UNIQUE (showtime_id, seat_id) WHERE kind='ticket' AND status<>'refunded'` kısmi dizini. Benzer şekilde seans çakışması için `EXCLUDE USING gist (hall_id WITH =, tstzrange(starts_at, busy_until) WITH &&)`, hediye çeki için `CHECK (balance_kurus >= 0)`, her satır için `CHECK (net + ent_tax + vat = paid)`.

**Sonuçlar.**
- (+) Değişmezler (invariant) kodla değil şemayla garanti altında; `make verify` bunları doğrudan SQL ile ihlal etmeye çalışıp reddedildiğini gösterir.
- (+) İade edilen bilet dizinden düştüğü için aynı koltuk yeniden satılabilir.
- (−) `btree_gist` eklentisi gerekir (PostgreSQL ile birlikte gelir).

## ADR-3: Süresi dolan tutma "okumada boş", temizlik "SKIP LOCKED" süpürücüyle

**Bağlam.** 10 dakikalık tutma süresi dolduğunda koltuk anında satılabilir olmalı. Bunun için zamanlayıcının doğruluğuna güvenilmemeli.

**Karar.** Bütün okumalar ve tutma kontrolü `status='held' AND held_until > now()` koşulunu "dolu" sayar; süresi geçen tutma mantıksal olarak zaten boştur. Arka planda 15 saniyede bir çalışan süpürücü `FOR UPDATE SKIP LOCKED LIMIT 5000` ile satırları fiziksel olarak boşaltır. Uzatma `expires_at = now() + 10 dk` yapar, en fazla 2 kez.

**Sonuçlar.**
- (+) Süpürücü gecikse ya da çökse bile doğruluk bozulmaz.
- (+) Birden fazla API kopyası birbirini beklemeden süpürür; o anda ödemesi süren satırları atlar.
- (+) Kısmi `show_seats_expiry_idx` dizini sorguyu 6,3 ms'den 0,014 ms'ye indirir.
- (−) Ödeme anında tutmanın hâlâ geçerli olduğu ayrıca kontrol edilir (`HOLD_EXPIRED`).

## ADR-4: Bilet QR'ı Ed25519 ile imzalanır (HMAC değil)

**Bağlam.** Kapı tarayıcıları ağ kesikken de bileti doğrulamalı. Cihazlar kaybolabilir veya çalınabilir.

**Karar.** `SB1.<base64url(JSON claims)>.<Ed25519 imzası>`. Gizli anahtar yalnızca API'de bulunur (`TICKET_SIGNING_SEED`); cihazlar `/api/gate/state` üzerinden açık anahtarı alır. Arayüzdeki tarayıcı WebCrypto Ed25519 ile, Go tarafındaki `gate.Scanner` ise standart kütüphaneyle doğrular.

**Sonuçlar.**
- (+) Ele geçirilen bir cihazla sahte bilet üretilemez. HMAC'te paylaşılan sır cihazda olacağı için bu mümkün olurdu.
- (+) İmza 64 bayt; QR küçük kalıyor.
- (−) WebCrypto Ed25519 eski tarayıcılarda yok (Chrome 137+, Firefox 129+, Safari 17+); desteklenmiyorsa ekran bunu açıkça söyler.
- (−) Anahtar döndürme (rotation) için `SB1` sürüm öneki var, ama çoklu anahtar desteği henüz yok.

## ADR-5: Çevrimdışı giriş için "en erken tarama kazanır" ve çakışmayı raporla

**Bağlam.** Ağ kesikken iki kapı aynı bileti kabul edebilir ya da kapılar o arada iade edilmiş bir bileti bilmeyebilir. Bunu önlemek imkânsız, ama görünür kılmak mümkün.

**Karar.** Senkron saf bir fonksiyondur (`gate.Reconcile`):
- Olaylar tarama zamanına göre sıralanır ve en erken tarama bileti kullanır.
- Sonraki taramalar `DUPLICATE_ENTRY`, iade edilmiş biletin kabulü `ADMITTED_AFTER_REFUND` olur.
- Çakışmalar `gate_conflicts` tablosuna ve denetim kaydına yazılır; salon amiri ekranında görünür.
- Sunucu daha önce aldığı `(ticket, gate, scanned_at)` olaylarını atlar; böylece yeniden deneme yeni bir çakışma üretmez.
- Yanıt cihaza güncel kullanılmış ve iade listelerini geri gönderir.

**Sonuçlar.**
- (+) İdempotent senkron; kural birim testlerinde ve uçtan uca doğrulamada kanıtlı.
- (−) Cihaz saatine güvenilir; saat kayması yanlış kapının "ilk" sayılmasına yol açabilir.

## ADR-6: Fiyat ve vergi satır bazında hesaplanıp saklanır, iade yeniden hesaplamaz

**Bağlam.** İadede kupon indirimi, hediye çeki payı ve vergi kırılımı ödenenle kuruşu kuruşuna aynı olmalı. Fiyat kuralları zamanla değişir.

**Karar.** `pricing.BuildQuote` kupon indirimini ve hediye çeki kullanımını satırlara "en büyük kalan" yöntemiyle dağıtır, her satırın vergi kırılımını hesaplar. Bunlar `order_lines` tablosunda saklanır. İade, saklanan değerleri aynen tersine çevirir.

**Sonuçlar.**
- (+) Fiyat kuralı değişse de geçmiş siparişin iadesi doğru kalır; raporlar `SUM` ile alınır.
- (+) Domain paketi saf olduğu için tablo tabanlı testlerle %97,7 kapsama.
- (−) Satır başına birkaç ek sütun.

## ADR-7: Katmanlar: saf domain, işlem sahibi service, ince HTTP

**Bağlam.** Zorlu görevlerin kuralları karmaşık; bu kuralların veritabanı olmadan test edilebilmesi gerekiyor.

**Karar.**
- `internal/domain/*`: saat ve I/O içermez, framework'e bağımlı değildir.
- `internal/service`: transaction'ları ve SQL'i yönetir, kural için domain'i çağırır.
- `internal/httpapi`: yalnızca HTTP eşlemesi, auth ve idempotency işlerini yapar. Router olarak standart kütüphanenin `net/http` (Go 1.22+ desenleri) kullanılır; ORM yoktur.

**Sonuçlar.**
- (+) Domain testleri milisaniyeler içinde koşar; servis testleri gerçek PostgreSQL'e karşı çalışır.
- (+) Bağımlılık listesi kısa: pgx, goose, golang-jwt, x/crypto, prometheus client.
- (−) SQL elle yazılıyor ve tarama (scan) kodu tekrarlı. Yine de `sqlc` eklemek bu boyutta kazandırmıyor.

## ADR-8: Frontend build adımsız vanilla JS

**Bağlam.** Arayüz zorunlu ve gerçek veriyle çalışmalı; giriş tarayıcısı çevrimdışı çalışabilmeli; teslim süresi kısa.

**Karar.** nginx'in sunduğu tek `index.html` + `app.css` + `app.js`; hash tabanlı router, rol bazlı menü. QR çizimi için tek bir CDN kütüphanesi var; yüklenmezse QR metni gösterilir.

**Sonuçlar.**
- (+) Node toolchain yok, imaj küçük, sayfa bir kez yüklendikten sonra tarayıcı ekranı ağ olmadan çalışır.
- (−) ~1000 satırlık tek dosya; tip güvenliği yok. Ekip büyürse React + TypeScript'e geçiş önerilir.

## ADR-9: Canlı doluluk için PostgreSQL LISTEN/NOTIFY + SSE

**Bağlam.** Vizyon sabahında koltuk haritası saniyeler içinde değişiyor; yoklama hem gecikmeli hem de sunucuya gereksiz yük bindiriyor. Birden fazla API kopyası çalışabilir.

**Karar.**
- Koltuk durumunu değiştiren her transaction `pg_notify('seat_changes', showtime_id)` gönderir. Bildirim commit'e bağlıdır: geri alınan değişiklik bildirilmez, olan değişiklik bütün kopyalara ulaşır.
- Her API süreci tek bir `LISTEN` bağlantısıyla aboneleri besler; abone başına 1 elemanlı kanal kullanılır, böylece yavaş istemci birikmez.
- `GET /api/showtimes/{id}/events` yalnızca "değişti" der; istemci haritayı yeniden çeker.
- Tarayıcı akışı `fetch()` ile okur. `EventSource` başlık gönderemediği için token'ı URL'ye koymak gerekirdi, o da loglara düşerdi.

**Sonuçlar.**
- (+) Ek altyapı (Redis, mesaj kuyruğu) gerekmez; mesaj kaybı durumunda bile istemci tam haritayı çektiği için tutarlıdır.
- (−) Her izleyici bir bağlantı tutar. Bağlantılar en fazla 30 dakika yaşar ve kapanışta düzgünce kesilir; çok büyük ölçekte ayrı bir yayın katmanı gerekir.
- (−) "Değişti" sinyali her seferinde tam harita çekmeye yol açar; fark (diff) göndermek daha az veri taşırdı ama kaçırılan mesajda tutarsızlık riski getirirdi.

## ADR-10: A/B testinde deneme birimi sepet, atama SHA-256

**Bağlam.** Kampanya etkisini ölçmek için müşterilerin kalıcı olarak bir kola atanması ve kolların istatistiksel olarak karşılaştırılması gerekiyor.

**Karar.**
- Atama: `SHA-256(deney anahtarı : kullanıcı id) mod kol sayısı`. Durumsuzdur, her kopyada aynı sonucu verir, tablo gerektirmez. Deney anahtarı karışıma girdiği için farklı deneyler bağımsızdır.
- Teklif sepette gösterildiği için deneme birimi **sepettir** (açılan sepet = deneme, ödenen sepet = başarı). Kullanıcı bazlı dönüşüm bilgi amaçlı raporlanır.
- Karar iki oranlı z-testiyle verilir (α = 0,05); her hücrede en az 5 gözlem yoksa "veri yetersiz" denir.

**Sonuçlar.**
- (+) FNV ile denendi; düşük bitlerin anahtara bağlı olmaması iki deneyi aynı bölünmeye zorladı. Bunu test yakaladı, SHA-256 bu sorunu taşımıyor.
- (−) Aynı müşterinin sepetleri birbirinden bağımsız değildir (kümeleme). z-testi bu yüzden biraz iyimserdir; kullanıcı bazlı bootstrap daha doğru olurdu.
- (−) Deney tanımı kodda sabit; çoklu deney yönetimi yok.

## ADR-11: Çizelgede "önce kısıt, sonra kalite"

**Bağlam.** Açgözlü üretici kısıtları sağlıyor ama salon kapasitesini film talebiyle eşleştirmiyor.

**Karar.** Üretimden sonra ayrı bir yerel arama turu (`schedule.Improve`) çalışır. Yalnızca aynı gün ve aynı prime durumundaki seansların filmlerini değiştirir; böylece sözleşme kısıtları yapısal olarak korunur. Zaman çakışması ve kapanış saati her değişimde kontrol edilir. Bağımsız doğrulayıcı her planı yeniden kontrol eder.

**Sonuçlar.**
- (+) Kısıt mantığı ve kalite mantığı ayrık; her biri ayrı test ediliyor.
- (+) Deterministik; aynı girdi aynı planı verir.
- (−) Yerel optimum: seans saatlerini kaydırmaz, film sayısını değiştirmez. Seed'de kazanç %2,9; daha büyük kazanç zaman kaydırma ya da tam sayılı programlama gerektirir.
