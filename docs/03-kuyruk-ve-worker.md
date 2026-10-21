# Faz 3: Kuyruk ve worker

Bu fazda kuyruğa alınan teslimatlar gerçekten gönderilmeye başladı. Projenin en
zor kısmı burası: birden fazla worker'ın aynı işi almaması, çöken bir worker'ın
iş kaybettirmemesi ve geç kalan bir sonucun yenisini ezmemesi.

## Bir teslimatın yaşamı

```
pending ──claim──▶ in_flight ──2xx──────────────▶ succeeded
   ▲                  │
   │                  ├──hata, deneme kaldı──▶ pending (next_attempt_at ileri)
   │                  ├──hata, son deneme───▶ dead
   │                  └──kilit süresi doldu─▶ pending / dead (reaper)
   └──────────────────┘
```

Endpoint devre dışıysa teslimat hiç istek atılmadan `dead` olur (deneme sayılmaz).

## Garantiler ve nasıl sağlandıkları

| Garanti | Nasıl |
| --- | --- |
| Bir teslimat aynı anda iki worker'a verilmez | Tek ifade: `FOR UPDATE SKIP LOCKED` seçimi bir kez çalışan `MATERIALIZED` CTE'de, ardından `UPDATE` ve `status = 'pending'` yeniden kontrolü. |
| Çöken worker iş kaybettirmez | Her alış bir kilit süresi (lease) taşır. Süresi dolan teslimatları her worker'daki reaper geri alır. |
| Zehirli mesaj sonsuz döngüye giremez | Kayıp deneme de sayılır; `MaxAttempts`'ta teslimat `dead` olur. |
| Geç gelen sonuç yenisini ezmez | Üç katman: (1) her alışta yeni `lease_token`, yazımlar ona bağlı (fencing); (2) yazımlar yalnızca `in_flight` satırı değiştirir; (3) `UNIQUE (delivery_id, attempt_no)`: reaper kayıp denemeyi eski worker'ın kullanacağı numarayla yazar. |
| Geçmiş tutarlı | Değişmez: `attempt_count` = deneme satırı sayısı, numaralar 1..n. Her entegrasyon testinin sonunda kontrol edilir. |
| Kapanışta yarım iş kalmaz | Sinyalde yeni iş alınmaz; devam eden istekler `DrainTimeout`'a kadar bitirilir ve sonuçları kapanıştan bağımsız bir bağlamla yazılır. Süre aşılırsa istekler iptal edilir, başarısız deneme olarak kaydedilip yeniden denenir. |

**Teslimat en az bir kez (at-least-once).** Uç noktaya ulaşmış ama sonucu
kaybolmuş bir istek yeniden gönderilir. Alıcı `Webhook-Id` başlığıyla tekrarı
ayıklar (olay kimliği; tüm denemelerde aynı).

## Alınan kararlar ve nedenleri

- **Saat her zaman veritabanından (`now()`).** `next_attempt_at` worker'ın
  saatiyle yazılsaydı, saat kayması olan bir worker yeniden denemeleri erken ya
  da geç tetiklerdi.
- **`attempt_count` alışta değil, sonuç yazılırken artar.** Devre dışı endpoint
  gibi istek atılmadan bırakılan teslimatlar deneme saymaz; çöken worker'ın
  denemesini ise reaper sayar.
- **Geri çekilme ×4 ve "equal jitter".** 30 sn, 2 dk, 8 dk, 32 dk, 2 sa 8 dk,
  sonra 6 saat tavan; varsayılan 8 deneme ≈ 15 saat. Bekleme [taban/2, taban]
  aralığından seçilir: aynı anda düşen çok sayıda teslimat endpoint'e aynı
  anda yüklenmez, ama hiçbiri planlanandan çok erken de denenmez.
- **`Retry-After` (429/503) beklemeyi yalnızca uzatabilir**, kısaltamaz; tavan
  yine geçerli.
- **Yönlendirmeler takip edilmez.** 3xx başarısız sayılır. Takip etmek,
  endpoint'in worker'ı başka (iç ağdaki dahil) bir adrese yönlendirmesine izin
  verirdi ve POST'u GET'e çevirirdi.
- **Proxy ortam değişkenleri yok sayılır.** Webhook trafiği worker'ın
  ortamından gelen bir `HTTP_PROXY` üzerinden gitmez.
- **Yanıt gövdesi temizlenir.** Postgres `text` NUL baytı ve geçersiz UTF-8'i
  reddeder; temizlenmeseydi ikili veri döndüren tek bir endpoint, sonucun hiç
  kaydedilememesine ve teslimatın döngüye girmesine yol açardı. En fazla 64 KiB
  okunur, 1024 karakter saklanır.
- **Zaman aşımı yanıtın okunmasını da kapsar.** Başlığı hemen gönderip gövdeyi
  hiç bitirmeyen bir endpoint worker'ı tutamaz.
- **Kilit sırası: önce endpoint, sonra teslimat.** Endpoint silme aynı sırayla
  kilitler (cascade); ters sıra deadlock'a yol açabilirdi.
- **İş alma kapanıştan bağımsız bir bağlamla yapılır.** Alınmış ama işlenmemiş
  satırlar kilit süresi dolunca bir deneme kaybederdi.
- **Worker testleri kendi geçici veritabanında.** Worker veritabanındaki tüm
  teslimatları alır; ortak test veritabanında diğer paketlerin test verisine
  istek atabilirdi.

## Testlerin yakaladığı Postgres planlayıcı tuzağı

İlk sürümde iş alma `UPDATE ... FROM (SELECT ... LIMIT n FOR UPDATE SKIP LOCKED) due`
biçimindeydi. Boş şemada tam test paketi tekrar tekrar çalıştırılırken bir kez
`claim(1)` **iki** satır döndürdü. `EXPLAIN ANALYZE` nedeni gösterdi: planlayıcı
alt sorguyu iç içe döngünün iç tarafına koyup **her dış satır için yeniden
çalıştırıyordu** (`Subquery Scan on due ... loops=3`). Her çalıştırma kendi
kilitlediği satırları atlayıp yenilerini kilitlediği için `LIMIT` aşılıyordu.
Çift teslimat olmuyordu, ama worker kapasitesinden fazla iş alıp slot
hesabını bozuyordu (kilitlenme riski).

Çözüm: kilitleyen seçim `WITH ... AS MATERIALIZED` CTE'sinde; Postgres onu
tam bir kez çalıştırır. Reaper sorgusu da aynı biçime getirildi. Koruma:

- `TestClaimLocksOnce` üretilen gerçek SQL ile `EXPLAIN ANALYZE` çalıştırır ve
  her `LockRows` düğümünün tek kez çalıştığını doğrular; eski biçime geri
  döndürülünce "2 kez çalıştı" diye kırıldığı doğrulandı.
- Worker, sorgu yine de fazla satır döndürürse kapasiteyi aşmak yerine ek
  slot bekler ve hata loglar.

## sqlc'de karşılaşılan sorun

`now() + @delay_ms::bigint * interval '1 millisecond'` sqlc'yi bozdu: Postgres'te
`@` aynı zamanda mutlak değer operatörü, sqlc kısaltmayı operatör sanıp metni
kaydırdı ("locked_until" → "d_until"). Sistematik daraltmayla bulundu;
`worker.sql`'de `sqlc.arg(...)` biçimi kullanılıyor ve nedeni dosyada yazılı.

## Bilinçli sınırlar

- **İmza ve SSRF koruması yok (Faz 4).** İstekler şu an imzasız; iç ağ
  adreslerine gönderim henüz engellenmiyor. Gönderici, Faz 4'ün güvenli
  bağlantı katmanının takılacağı şekilde `Transport` alır.
- **Bekleme sorgulama (polling), `LISTEN/NOTIFY` değil.** Gecikme en fazla
  `WORKER_POLL_INTERVAL` (500 ms). Basit ve sağlam; ileride bildirime çevrilebilir.
- **Tenant/endpoint başına adalet yok.** Çok olay gönderen bir tenant kuyruğun
  önünü doldurabilir; endpoint başına eşzamanlılık sınırı yok.
- **Kayıp denemenin `started_at` değeri** kilidin geri alındığı an; gerçek
  başlangıç bilinmiyor.
- Endpoint'i hatalar yüzünden otomatik devre dışı bırakma Faz 5'te; sayaç
  (`consecutive_failures`) şimdiden tutuluyor.

## Doğrulama

- Birim: geri çekilme (tavan, jitter aralığı, taşma), `Retry-After` (saniye,
  HTTP tarihi, geçmiş, aşırı değer), metin temizleme, gönderici (istek biçimi,
  3xx, zaman aşımı, bitmeyen gövde, reddedilen bağlantı, proxy).
- Entegrasyon, gerçek Postgres + gerçek HTTP sunucuları: başarı, yeniden deneme,
  vazgeçme, `Retry-After`, zaman aşımı, kötü gövde, devre dışı endpoint, 4 worker
  × 120 teslimatta her birinin **tam bir kez** ulaşması, kilitli satırı atlama,
  sıra, çöken worker, fencing, son denemede reaper, kapanış (bekleme ve iptal),
  uçuştayken silinen endpoint.
- `-race` ile tüm paketler, boş şemada 3 ardışık koşu; worker paketi ayrıca 5 kez.
- Docker uçtan uca (demo alıcı, 1 sn geri çekilme, 3 deneme): başarılı endpoint
  tek denemede `succeeded`; 503 dönen ve çözülemeyen host 3 denemede `dead`
  (denemeler arası 1,0 sn ve 2,5 sn); 3 worker ile 30 olayın 30'u tam bir kez
  ulaştı, iş üç worker'a dağıldı; `docker compose stop` ile üç worker da çıkış
  kodu 0; loglarda anahtar veya sır yok.
- Mutasyon denemeleri (her biri ilgili testi kırdı): `SKIP LOCKED` yok,
  `SKIP LOCKED` ve durum kontrolü yok (çift teslimat yakalandı), fencing yok,
  reaper denemeyi saymıyor, drain yok, gövde temizlenmiyor, yönlendirme takip
  ediliyor, `Retry-After` yok sayılıyor, devre dışı endpoint'e istek atılıyor.
