# Faz 6: Zorlayıcı testler

Önceki fazlar her özelliği tek tek test etti. Bu faz, parçaların **birlikte** ve
**kötü koşullarda** çalıştığını sınar: rastgele girdi, yoğun çekişme, her
şeyin aynı anda ters gittiği bir kaos senaryosu ve gerçek süreç öldürme. Dört
gerçek hata bulundu (aşağıda); hepsi önceki testlerden geçmişti.

## Bulunan hatalar

| # | Hata | Nasıl bulundu | Etkisi |
| --- | --- | --- | --- |
| 1 | **Doğrulayıcılar ile Postgres uyuşmuyordu.** URL'de em space (`U+2003`), açıklamada `\u0000`, olay yükünde `"\u0000"`, tek başına vekil (`"\ud800"`), `1e999999999` ve geçersiz UTF-8 | Fuzz testlerinin **tohum girdileri** (fuzzing başlamadan) | Geçerli görünen ama saklanamayan girdi `400` yerine **`500`** dönüyordu. |
| 2 | **Fan-out ile eşzamanlı endpoint silme yarışı.** Fan-out `SELECT` endpoint'i görüyor, silme commit oluyor, `INSERT`'in yabancı anahtar kontrolü patlıyor | **Kaos testi** (ilk koşuda) | Geçerli bir olay, aynı anda bir endpoint silindi diye `500` ile reddediliyordu. |
| 3 | **Uçuştaki *başarısız* bir deneme sırasında endpoint silinirse** sonuç yazılamıyor ve iç hata olarak loglanıyordu | Kaos testi (3 koşuda 2'sinde) | Teslimat zaten silinmişti (cascade) ama worker hata logluyordu. Başarı yolu doğru çalışıyordu, bu yüzden Faz 3 testi yakalamamıştı. |

| 4 | **`Retry-After` tarihi sınırsız.** Çok ileri bir tarih ~292 yıllık süre döndürüyordu | Fuzzing (`FuzzParseRetryAfter`) | Zararsız (bekleme zaten `MaxBackoff` ile sınırlı) ama fonksiyon taşmaya açıktı. |

### Düzeltmeler

1. **İki katmanlı:** doğrulayıcılar sıkılaştırıldı (Unicode boşluk ve kontrol
   karakterleri, geçersiz UTF-8, NUL) ve Postgres'in veri hatası sınıfı
   (`SQLSTATE 22xxx`) arka koruma olarak `400`'e çevrildi. İkincisi önemli:
   Postgres'in "geçerli JSON/metin" tanımını (kaçış dizileri, sayı aralığı,
   yerel ayara bağlı boşluk) elle yeniden yazmak her zaman geride kalır; Postgres'in
   kendi kararı hep senkron. `23514` (CHECK ihlali) de `400` olur ama
   doğrulayıcı ile kısıt ayrıştığı için **uyarı olarak loglanır**.
2. **`FOR KEY SHARE OF e`** fan-out `SELECT`'inde. Silme `FOR UPDATE` ister, bu
   kilitle çakışır: silme ya fan-out bitene kadar bekler (yeni teslimatları
   cascade siler) ya da zaten bitmiştir (satır artık dönmez).
3. `RecordEndpointFailure`'ın `ErrNoRows` sonucu "endpoint silindi, sonuç atıldı"
   olarak ele alınıyor.
4. `Retry-After` hem saniye hem tarih biçiminde 366 güne kırpılıyor.

Her biri, düzeltmeden **önce kırılan** deterministik bir testle sabitlendi:
`TestIngestDuringEndpointDeletion` (silme commit edilene kadar bekleyen
transaction ile), `TestEndpointDeletedWhileAFailingAttemptIsInFlight`, fuzz
tohumları.

## Fuzz testleri

Düz `go test` yalnızca tohum girdilerini çalıştırır; gerçek fuzzing için
`-fuzz` kullanılır. Servis hedefleri gerçek Postgres'e yazar ve tek bir özelliği
sınar: **istemci hatası asla `500` olmamalı**.

| Hedef | Özellik |
| --- | --- |
| `FuzzCreateEndpoint`, `FuzzUpdateEndpoint` | URL ve açıklama: `nil` ya da `ValidationError`, asla veritabanı hatası. |
| `FuzzIngestEvent` | Tip, yük, idempotency anahtarı: `nil`, `ValidationError` ya da `ErrIdempotencyMismatch`. |
| `FuzzDecodePage` | İmleç ve sınır: `ValidationError`; kabul edilen imleç Postgres'in reddedeceği bir zaman içermez. |
| `FuzzCheckConsistency` | SSRF: bir adres yazılış biçiminden bağımsız yargılanır; içinde engelli IPv4 taşıyan IPv6 (`::ffff:`, NAT64, 6to4) asla geçmez. |
| `FuzzCheckIPv4`, `FuzzCheckHost` | Loopback/özel/link-local hiçbir biçimde geçmez; `*.localhost` adları engellenir; IP literali `Check` ile aynı kararı alır. |
| `FuzzVerifyNeverPanics` | `Verify` hiçbir başlık/gövde ile panik yapmaz; kabul ederse imza gerçekten doğrudur. |
| `FuzzSignVerifyRoundTrip` | İmzaladığımız her istek doğrulanır; gövdede tek bit değişirse doğrulanmaz. |
| `FuzzDecodeSecret` | Boş anahtar kabul edilmez, hata ile anahtar birlikte dönmez. |
| `FuzzClean`, `FuzzParseRetryAfter`, `FuzzBackoff` | Saklanabilir metin (geçerli UTF-8, NUL yok, sınırlı, idempotent); `Retry-After` taşmaz ve bekleme tavanı aşılmaz; geri çekilme aralıkta kalır. |

### Fuzz sonuçları (hedef başına 30 sn, Windows, tek makine)

| Hedef | Çalıştırılan girdi | Sonuç |
| --- | --- | --- |
| `FuzzCreateEndpoint` | 126 bin | hata yok (düzeltmelerden sonra) |
| `FuzzUpdateEndpoint` | 210 bin | hata yok |
| `FuzzIngestEvent` | 588 bin | hata yok |
| `FuzzDecodePage` | 731 bin | hata yok |
| `FuzzCheckConsistency` | 2,30 milyon | hata yok |
| `FuzzCheckIPv4` | 2,30 milyon | hata yok |
| `FuzzCheckHost` | 1,75 milyon | hata yok |
| `FuzzVerifyNeverPanics` | 1,24 milyon | hata yok |
| `FuzzSignVerifyRoundTrip` | 3,63 milyon | hata yok |
| `FuzzDecodeSecret` | 4,54 milyon | hata yok |
| `FuzzClean` | 1,18 milyon | hata yok |
| `FuzzBackoff` | 1,03 milyon | hata yok |
| `FuzzParseRetryAfter` | | **1 hata bulundu** (aşağıda), düzeltildi, 20 sn daha (360 bin girdi) temiz |

**`FuzzParseRetryAfter`:** `Retry-After: Wed, 01 Oct 7000 ...` gibi çok ileri bir
tarih ~292 yıllık bir süre döndürüyordu (`time.Duration` doygunluğu); saniye
biçimi ise bir yıla kırpılıyordu. Zararsızdı (`nextDelay` zaten `MaxBackoff` ile
sınırlar) ama fonksiyonun kendisi sınırsız değer döndürüyordu, yarın zaman
aritmetiğine girerse taşardı. İki biçim de aynı tavana (366 gün) kırpılıyor;
girdi `testdata/fuzz/FuzzParseRetryAfter/` altında kalıcı, her `go test`'te
çalışır. Tavanı kaldıran mutasyon hem birim testi hem bu girdiyi kırıyor.

Bulgu sayısı notu: ilk üç hata fuzzing **başlamadan**, tohum girdilerle ve kaos
testiyle bulundu; asıl fuzzing yalnızca bir hata daha buldu. Toplam 17 milyondan
fazla girdi; bu, "hata yok" demek değil, 30 saniyelik koşuların kapsadığı
kadarını gösterir.

## Çekişme testleri

- **`TestClaimUnderHeavyContention`:** 16 eşzamanlı alıcı, 400 teslimat. Her
  teslimat **tam bir kez** verilir, her biri kendi kilit belirtecini taşır,
  veritabanındaki durum alıcıların gördüğüyle birebir aynıdır, iş birden çok
  alıcıya gerçekten dağılır.
- **`TestConcurrentReapersReclaimEachLeaseOnce`:** 120 süresi dolmuş kilit, 8
  eşzamanlı reaper: toplam geri alma sayısı tam 120, her teslimat tam bir (kayıp)
  deneme ile `pending`.

## Kaos testi

`TestChaos` 6 saniye boyunca şunları **aynı anda** yapar:

- 3 worker, rastgele aralıklarla zarifçe durdurulup yenisi başlatılır;
- bir "hayalet" worker teslimat alıp hiç raporlamaz (çöken süreç);
- 8 uç nokta, iki tenant: iyi, %35 hata veren, yavaş, takılan (%12), hız
  sınırı koyan (`429` + `Retry-After`) ve her zaman hata veren;
- 3 üretici olay gönderir, bir kısmı tekrar eden idempotency anahtarlarıyla;
- bir operatör rastgele teslimatları yeniden gönderir, bir uç noktayı
  açıp kapatır, bir anahtarı iki kez döndürür ve bir uç noktayı siler.

Sonra kuyruk boşalana kadar beklenir ve şunlar doğrulanır:

1. her teslimat son durumda (`succeeded`/`dead`), hiçbirinde kilit kalmamış;
2. `attempt_count` deneme satırı sayısına eşit, numaralar 1..n boşluksuz ve tekrarsız;
3. son durumlar tutarlı (`succeeded` ⇒ 2xx kodu, `dead` ⇒ hata metni);
4. idempotency tam beklenen sayıda olay üretti;
5. **hiçbir istek yanlış tenant'ın ya da yanlış uç noktanın olayını taşımadı**;
6. `succeeded` olan her teslimat **gerçekten** uç noktaya ulaştı (en az bir kez);
7. alıcı her istekte imzayı uç noktanın o anki (güncel ve önceki) anahtarlarıyla
   doğruladı: sıfır geçersiz imza;
8. hiçbir worker iç hata, kilitlenme ya da kapasite aşımı loglamadı; hepsi
   temiz kapandı.

Testin **dişi olduğunu** da doğrular: çöken talep, kaybolan kilit, yeniden deneme
sonrası başarı, ölü teslimat, yeniden başlatma ve idempotent tekrar sayılarından
biri sıfırsa test kendisi başarısız olur. Rastgele tohum başarısızlıkta
yazdırılır, aynı tohumla tekrarlanabilir.

Tipik bir koşu: ~3000 teslimat (≈3000 başarılı, ≈90 ölü), ~130 çöken talep ve
o kadar kaybolan kilidin geri alınması, ~500 yeniden denemeyle başarılan, ~20
worker yeniden başlatması, ~270 idempotent tekrar, ~3800 alınan istek.

### Kaos testi gerçekten ne yakalar? (mutasyonlar)

Kaos testi her şeyi yakalamaz; bunu ölçtük (her mutasyon kaos testinin 3 farklı
tohumuyla ve varsa hedefli testle denendi):

| Mutasyon | Kaos testi | Hedefli test |
| --- | --- | --- |
| Reaper kayıp denemenin satırını yazmıyor | 3/3 yakaladı | yakaladı |
| Deneme numarası yeniden kullanılıyor | 3/3 yakaladı | n/a |
| Kapalı/silinen endpoint iç hata sayılıyor | 3/3 yakaladı | yakaladı |
| Fan-out'ta endpoint kilidi yok | **2/3** yakaladı | yakaladı (deterministik) |
| Başarı yazımı kilit belirteciyle (fencing) korunmuyor | **0/3, yakalamadı** | yakaladı |

İki sonuç: **fencing'i kaos testi tek başına görmez**: eski worker'ın geç sonucu
yenisini ezdiğinde toplu değişmezler (satır sayıları, son durumlar) yine tutar;
bunu yalnızca zamanlaması elle kurulan hedefli test yakalar. Fan-out yarışı da
kaosta zamanlamaya bağlı (2/3); güvenilir olan hedefli test. Yani iki katman
birbirinin yerini tutmuyor, birbirini tamamlıyor. (Bir mutasyonum ilk
denemede geçersizdi, derlenmedi; düzeltilip tekrar denendi.)

## Gerçek süreç öldürme (Docker, `kill -9`)

2 worker, 9 sn kilit süresi, 3 sn süren bir alıcı, 8 olay. Uçuşta 6 teslimat
varken **iki worker da `docker kill --signal=KILL` ile öldürüldü**: zarif kapanış
yok, sonuç yazılmadı. Sonuç:

- Öldürülme anında 3 teslimat hâlâ `in_flight` ve kilitli (5'i bitmişti).
- Yeni bir worker başlatıldı; 3 süresi dolmuş kilit geri alındı (log: 3 satır),
  teslimatlar yeniden denendi.
- 8'i de `succeeded`; deneme geçmişinde 8 `delivered` + 3 `lost to the kill`
  (kaybolan denemeler sayıldı ve kayıtlı). `attempt_count` = satır sayısı
  değişmezi tuttu.
- Alıcı 8 olayın 8'ini de aldı. Öldürülen denemelerin istekleri alıcıda
  yarım kaldığı (bağlantı koptu) için bu koşuda yinelenen istek olmadı; istek
  ulaşıp sonucu kaybolsaydı yeniden gönderilir ve alıcı `webhook-id` ile ayıklardı
  (en az bir kez garantisi).

## Bilinçli sınırlar

- Kaos testi tek makinede ve tek Postgres'te çalışır; ağ bölünmesi, yavaş disk
  ya da Postgres yeniden başlatması simüle edilmiyor.
- Fuzz çalıştırmaları süreyle sınırlıdır (hedef başına 30 sn); bulunmayan hata
  yok demek değildir. Bulunan girdiler `testdata/fuzz/` altında kalıcıdır.
- Throughput/yük ölçümü yok; performans iddiası yapılmıyor.
