# Faz 11: Yük testi ve ölçümler

Amaç: README'ye "bu makinede şu kadar" diyebileceğimiz dürüst, tekrarlanabilir
sayılar koymak. Bu bir test değil bir ölçüm: hiçbir sayı CI'da eşik olarak
kullanılmıyor. Bu fazda **hiçbir uygulama kodu değişmedi**; yalnızca ölçüm
aracı (`cmd/loadtest`) eklendi.

## Ortam (sonuçların geçerli olduğu yer)

| | |
| --- | --- |
| Makine | AMD Ryzen 5 5600X (6 çekirdek, 12 mantıksal), Windows 11 |
| Docker | Docker Desktop (Engine 29.9.2), VM'e verilen 12 CPU ve 7,7 GiB bellek |
| Veritabanı | PostgreSQL 16 (alpine), **varsayılan ayarlar**, Docker VM içinde adlandırılmış birim |
| Servisler | `api` ve `worker` konteynerleri, ayrı bir yük veritabanında (`hookmini_load`); geliştirme verisine dokunulmadı |
| Yük aracı | Aynı makinede, ana bilgisayarda (alıcı da orada: `host.docker.internal`) |
| Ayar | `ALLOW_PRIVATE_DESTINATIONS=true` (alıcı özel adreste olduğu için; yalnızca geliştirme), `DELIVERY_MAX_ATTEMPTS=3`, `DELIVERY_BACKOFF_INITIAL=1s` |

Yani: **tek makine, yük aracı ve sistem aynı CPU'yu paylaşıyor, yerel ağ, Docker
Desktop disk katmanı.** Üretim donanımında sayılar çok farklı olur, çoğunlukla
daha iyi, ama bunu iddia etmiyorum.

## Araç

`cmd/loadtest` gerçek yığına dışarıdan HTTP ile saldırır:

1. Endpoint'ler kaydeder (alıcısı aracın kendi içindeki bir HTTP sunucusu).
2. Olayları `POST /v1/events` ile gönderir (eşzamanlı istemciler, isteğe bağlı sabit hız).
3. Alıcıda her teslimatın ne zaman geldiğini ve imzanın doğru olduğunu kaydeder.
4. **Sonunda veritabanıyla tutarlılık denetimi yapar:** sağlıklı endpoint'lere olay sayısı kadar teslimat oluşmuş ve hepsi `succeeded` mi, alıcıya gelen sayı beklenenle eşit mi, geçersiz imza ya da kopya var mı. Tutmuyorsa sonuç `valid: false` olur.

Senaryolar: `ingest` (yalnızca alım), `delivery` (uçtan uca), `mixed`
(endpoint'lerin %10'u yavaş = 500 ms, %10'u hep 500 döner).

## Sonuçlar

Her ölçüm 3 kez tekrarlandı (bazıları 2); tablolar **ortanca (en kötü–en iyi)**
verir. Ham JSON'lar bu çalışmada tutuldu, sayılar oradan aktarıldı.

### Alım (worker durdurulmuş, 32 istemci, 5000 olay)

| Fan-out (endpoint/olay) | Olay/sn | p99 gecikme | Eklenen teslimat satırı/sn |
| --- | --- | --- | --- |
| 1 | **1802** (1798–1847) | 25 ms | 1,8 bin |
| 5 | **1485** (1471–1496) | 31 ms | 7,4 bin |
| 20 | **833** (816–874) | 71 ms | 16,7 bin |

Bir olay, tek bir işlemde olay satırı + her endpoint için bir teslimat satırı
yazıyor. Fan-out arttıkça olay/sn düşüyor, ama yazılan satır/sn artıyor.

### Teslimat gecikmesi (5 endpoint, 1 worker, **varsayılan ayarlar**, sabit hızda)

| Yük | Teslimat/sn | "gönderildi → ulaştı" p50 | p99 |
| --- | --- | --- | --- |
| 50 olay/sn | 250 | 262 ms | **509 ms** |
| 100 olay/sn | 500 | 266 ms | **505 ms** (3 koşunun 1'inde bkz. "Takılmalar") |

**Gecikmenin tabanı yoklama aralığı:** `WORKER_POLL_INTERVAL` varsayılanı 500 ms
ve p99 tam olarak onun kadar. Kuyruk boşalınca worker 500 ms uyuyor; yeni
teslimat ortalama yarım aralık (≈250 ms) bekliyor. Bu zaten bilinen sınırdı
(README: "Workers poll"); artık sayısı var. Aralığı düşürmek bunu azaltır
(veritabanına daha çok sorgu pahasına); `LISTEN/NOTIFY` ile tamamen kalkardı.

### Teslimat verimi (patlama: 5000 olay bir anda = 25 bin teslimat)

Alım ve teslimat aynı anda çalıştığı için buradaki "gecikme" sıra bekleme
süresi, anlamsız; yalnızca verim anlamlı:

| Yapılandırma | Teslimat/sn |
| --- | --- |
| 1 worker, eşzamanlılık 10 (varsayılan) | **707** (692–716) |
| 1 worker, eşzamanlılık 50 | **1057** (789–1447) |
| 3 worker, eşzamanlılık 10 | **548** (547–1049) |

**Worker eklemek bu ortamda verimi artırmadı.** Bunun sebebi uygulama değil
(aşağıda).

### Karışık yük (20 endpoint: 2 yavaş = 500 ms, 2 hep 500 döner)

| Yük | Worker eşzamanlılığı | Teslimat/sn | Sağlıklı teslimat gecikmesi p50 / p99 |
| --- | --- | --- | --- |
| 5 olay/sn (100 teslimat/sn) | 10 | 89 | 289 ms / 519 ms |
| 20 olay/sn (400 teslimat/sn) | 10 | 136 | **21,6 sn / 48,8 sn** |
| 20 olay/sn (400 teslimat/sn) | 50 | 356 | 292 ms / 524 ms |

**En önemli bulgu:** eşzamanlılık sınırı **bütün endpoint'ler için ortak**.
Yavaş bir endpoint, sağlıklı olanlarla aynı yuvaları işgal ediyor: 10 yuvada
teslimatların %10'u 500 ms sürerse kapasite yaklaşık 180/sn'ye düşüyor ve 400/sn
gelince kuyruk birikiyor, **sağlıklı endpoint'lerin gecikmesi de saniyelerle
ölçülüyor.** Eşzamanlılığı 50'ye çıkarmak sorunu bu yükte çözdü. Gerçek çözüm
(endpoint başına eşzamanlılık sınırı) yapılmadı; README'de zaten "adalet yok"
sınırı olarak yazıyordu, bu onun ölçülmüş hali.

## Darboğaz: Postgres'in yazma (WAL fsync) hızı

- Verim sırasında Postgres konteyneri CPU'nun %110–190'ını kullanırken worker
  %20–55'te kaldı: **sınırlayan worker değil, veritabanı**.
- Etkin bağlantıların çoğu `WALWrite`/`WALSync` bekliyordu: yani commit'ler
  diske yazılmayı bekliyordu.
- Birimin ham hızı (`pg_test_fsync`): **yaklaşık 500 fsync/sn, ≈2 ms** her biri.
  Eşzamanlı commit'ler gruplanarak 1000–1500 commit/sn'ye çıkabiliyor, ama
  tavan buradan geliyor.
- Kanıt için **yalnızca teşhis amacıyla** `synchronous_commit = off` denendi
  (üretimde önerilmez, gönderilmiyor): 3 koşuda 1985, 2288 ve 399 teslimat/sn.
  İlk ikisi ~2 kat; yani disk bekleyişi gerçekten tavan.
- Çözülen şey **her teslimat denemesinin bir işlem olması**: claim (gruplu),
  sonra sonucu yazan işlem (teslimat + deneme satırı + endpoint sayacı).
  Güvenlik açısından bilinçli bir seçim, ama verimin üst sınırı bu yüzden disk.

### Elediğim hipotezler (neden yanlış çıktıklarıyla)

- **"Aynı endpoint satırında kilit çekişmesi var"** (her başarı endpoint'in
  sayacını güncelliyor). Doğru olsaydı tek endpoint'e giden yük çok daha yavaş
  olurdu. Aynı 25 bin teslimat 1, 5, 20 ve 100 endpoint'e dağıtıldı: **1 endpoint
  878/sn**, dağıtılınca 1066–1283/sn, yani fark gürültü düzeyinde.
- **"Claim sorgusu ölü indeks girdilerini tarıyor"** (kuyruk tablosu şişmesi).
  Plan, koşu sırasında `EXPLAIN (ANALYZE, BUFFERS)` ile alındı: 30–50 tampon,
  0,2–1,2 ms. Ayrıca `deliveries` için agresif autovacuum ayarı da takılmaları
  engellemedi.
- **"Hata/yeniden deneme var"**: bütün koşularda 0 başarısız deneme, 0 yeniden
  deneme, en uzun deneme <500 ms.

## Takılmalar (açıklayamadığım)

Aynı yapılandırmada ardışık koşular yaklaşık beşte birinde 3–5 kat yavaşladı
(ör. 342 ve 320 teslimat/sn'ye karşı 1400+). Böyle bir koşuda saniye başına
teslimat ~230'a iniyor, yaklaşık bir dakika boyunca yavaşça artıyor ve sonra
bir anda 2000+'ye sıçrıyor (autovacuum'un o anda bitmesiyle denk geldi, ama
agresif autovacuum ayarı takılmayı engellemedi). Yavaş fazda Postgres tek bir
işlemde CPU yakıyor, bekleyişler `WALWrite`.

**Sebebini kanıtlayamadım.** Uygulama tarafı temiz görünüyor (yukarıdaki
elemeler), en olası sebep ana bilgisayardaki Docker Desktop disk/VM
davranışı. Bunu doğrulamak için Linux'ta, gerçek bir diskte ve ayarlanmış bir
Postgres ile aynı testi koşmak gerek; yapılmadı. Bu yüzden her tabloda **en
kötü sayı da** yazılı, ve hiçbir yerde "en iyi" sayı öne çıkarılmadı.

## Aracın kendi doğruluğu

- İlk karışık yük koşuları **geçersiz** çıktı: araç "olay × endpoint kadar teslimat" bekliyordu, oysa hep hata veren iki endpoint 100 başarısız denemeden sonra servis tarafından devre dışı bırakılıyor ve devre dışı endpoint'e yeni teslimat oluşmuyor. Bu bir hata değil, Faz 5'in özelliği; araç denetimi "sağlıklı endpoint'lerde tam, hatalılarda en fazla" olarak düzeltildi ve koşular yeniden yapıldı. Geçersiz sonuçlar tablolara girmedi.
- Yardımcı fonksiyonlar (`percentiles`, `round`) testli; 3 mutasyonun 3'ü yakalandı.
- Çalıştırmalar arasında kuyruk temizlendi: önceki koşudan kalan bekleyen teslimatlar sonrakini kirletmesin.

## Nasıl tekrarlanır

```bash
# yük veritabanı yığını, alıcı ana bilgisayarda olduğu için özel adreslere izin
ALLOW_PRIVATE_DESTINATIONS=true docker compose up -d --build
KEY=$(docker compose run --rm api admin bootstrap "Load test" | grep -o 'hm_[A-Za-z0-9_]*')
LOADTEST_KEY=$KEY LOADTEST_DB='postgres://hookmini:hookmini@127.0.0.1:5432/hookmini?sslmode=disable' \
  go run ./cmd/loadtest -scenario delivery -endpoints 5 -events 2000 -rate 100
```

`go run ./cmd/loadtest -h` bütün bayrakları listeler. Windows güvenlik duvarı
ilk çalıştırmada alıcının portu (9100) için izin isteyebilir. Alım ölçümü için
`docker compose stop worker` yapın; sonra bekleyen teslimatları silin.

## Bilinçli sınırlar

- Tek makine, ortak CPU, Docker Desktop disk katmanı: sayılar **bu ortam** için.
- Üç tekrar küçük bir örnek; takılmalar yüzünden aralıklar geniş.
- Yük aracı sağlıklı bir alıcı simüle ediyor; gerçek ağ gecikmesi yok.
- Uçtan uca senaryolar yalnızca `POST /v1/events` ve teslimatı ölçüyor; listeleme, replay vb. ölçülmedi.
- Uzun süreli (saatlerce) dayanıklılık testi yapılmadı.
- Bulunan üç şey (yoklama tabanı, ortak eşzamanlılık, fsync sınırı) **düzeltilmedi**; ayrı kararlar.
