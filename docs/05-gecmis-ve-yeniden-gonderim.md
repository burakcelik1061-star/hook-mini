# Faz 5: Geçmiş, yeniden gönderim ve bozuk endpoint'ler

Bu fazda teslimatların ne olduğunu görmek, başarısız olanları tekrar göndermek
ve sürekli hata veren bir endpoint'in kendini kapatması eklendi.

## Yeni uçlar

| Uç | Ne yapar |
| --- | --- |
| `GET /v1/deliveries` | Teslimat listesi; `endpoint_id`, `event_id`, `status` filtreleri, sayfalama. |
| `GET /v1/deliveries/{id}` | Tek teslimat, **tüm denemeleriyle** (durum kodu, süre, yanıt özeti, hata). |
| `POST /v1/deliveries/{id}/replay` | Biten (`dead` veya `succeeded`) teslimatı yeniden gönderir. `202`. |
| `POST /v1/endpoints/{id}/disable` | Endpoint'i elle kapatır (idempotent). |
| `POST /v1/endpoints/{id}/enable` | Açar ve hata sayacını sıfırlar. |

Listelerde denemeler gömülü gelmez (N+1 ve büyük yanıt olmasın diye); yalnızca
tek teslimat yanıtında vardır. `next_attempt_at` yalnızca `pending` iken görünür.

## Yeniden gönderim nasıl çalışır

Sorun: ölü bir teslimat 8 deneme yapmış olabilir. Aynı satırı `pending`
yapıp denemeyi sürdürürsek ilk başarısızlıkta hemen yine ölür (9 ≥ 8). Ama
`attempt_count`'u sıfırlarsak geçmişle (deneme satırı sayısı) tutarsızlaşır.

Çözüm: **`round_base`** sütunu. `attempt_count` hep artar ve deneme satırı
sayısına eşit kalır (değişmez bozulmaz); yeniden denemelerin bütçesi
`attempt_count - round_base` ile **tur başına** sayılır. Yeniden gönderim
`round_base = attempt_count` yapar. Worker ve reaper aynı kuralı kullanır;
reaper'ın bunu bilmemesi, yeniden gönderilmiş bir teslimatı erken öldürürdü
(testle ve mutasyonla doğrulandı).

- **Eşzamanlı çift tıklama güvenli.** Koruma okumada değil, `UPDATE`'in içindeki
  `status IN ('dead', 'succeeded')` koşulunda: 16 istemci aynı anda
  bastığında tam 1'i kabul edilir, 15'i `409 delivery_not_replayable` alır.
- **Henüz bitmemiş teslimat** (`pending`, `in_flight`) yeniden gönderilemez
  (`409 delivery_not_replayable`).
- **Kapalı endpoint'e yeniden gönderim reddedilir** (`409 endpoint_disabled`):
  worker kapalı endpoint'in teslimatını zaten deneme yapmadan terk eder; sessizce
  başarısız olmak yerine ne yapılacağını söyleyen bir hata dönülür.
- **`webhook-id` aynı kalır**; alıcı tekrarı ayıklayabilir. İmza ve zaman
  damgası yeni.
- **Ardışık iki replay'in ikincisi `202` dönebilir:** ilk replay hızla teslim
  edilip `succeeded` olduysa o da yeniden gönderilebilir bir durumdur. Bu hata
  değil; deneme geçmişinde iki başarılı deneme görünür.

## Otomatik devre dışı bırakma

`ENDPOINT_AUTO_DISABLE_AFTER` (varsayılan 100, `0` = kapalı): bir endpoint'e art
arda bu kadar **deneme** başarısız olursa (herhangi bir başarı sayacı sıfırlar)
worker onu aynı transaction içinde kapatır. Neden bu şekilde:

- **Sayaç ve kapatma aynı transaction'da, tek koşullu `UPDATE`.** İlk neden
  korunur, ikinci worker aynı anda eşiği aşsa bile tekrar kapatmaz.
- **Kapanan endpoint'in bekleyen teslimatları `dead` olur** (kayıp ve sessiz
  değil): `last_error` "endpoint is disabled..." der. Endpoint düzeltilip
  açıldıktan sonra tek tek `replay` ile yeniden gönderilebilirler.
- **Eşik "teslimat" değil "deneme" sayar.** Basit ve veritabanında zaten tutulan
  `consecutive_failures` ile uyumlu. Zamana dayalı bir kural ("5 gün boyunca
  hata") ayrı bir ilk-hata zaman damgası gerektirirdi; bilinçli olarak yapılmadı.

## Test boşluğu ve kırılgan test

- Tenant kapsamının SQL'deki kopyası, servisin önceden yaptığı tenant'a bağlı
  okuma yüzünden hiçbir testle **tek başına** sınanmıyordu; mutasyon hayatta
  kaldı. `ReplayDelivery` sorgusu doğrudan test edildi: başka tenant `ErrNoRows`,
  `pending` teslimat `ErrNoRows`.
- Faz 3'ten kalan bir zaman aşımı testi (`DurationMs < 300`, 300 ms zaman
  aşımı) bu fazda ilk kez kırıldı: süre 299 ms ölçüldü (milisaniye kesiliyor,
  zamanlayıcı biraz erken ateşleyebilir). Ürün hatası değildi; eşik 250 ms'ye
  çekildi ve worker paketi `-race` ile 8 ardışık koşuda doğrulandı.

## Bilinçli sınırlar

- Toplu yeniden gönderim yok (tek teslimat). Bir endpoint'in tüm ölü
  teslimatlarını tek çağrıda gönderen bir uç sonraya kaldı.
- Teslimat silme veya saklama süresi yok; geçmiş sınırsız büyür.
- Kapanma/açılma için bildirim (e-posta, webhook) yok; durum yalnızca
  `GET /v1/endpoints` içinde (`status`, `disabled_reason`) görünür.

## Doğrulama

- Worker: yeniden gönderimde taze bütçe (6 deneme, numaralar 1..6 kesintisiz),
  başarılı teslimatın yeniden gönderilmesi (aynı `webhook-id`), otomatik
  kapanma (tam 3 istek), başarı sayacı sıfırlıyor (2 hata + başarı + 2 hata
  kapatmıyor), kapalı (`0`) iken kapanmıyor, reaper tura göre sayıyor.
- Servis (gerçek Postgres): filtreler ve doğrulama, deneme sıralaması, tüm
  yeniden gönderim kuralları, **16 eşzamanlı replay → tam 1**, devre
  dışı/açma idempotentliği ve tenant izolasyonu.
- HTTP uçtan uca: durum kodları (202/409/404/405/400), filtreler, detayda
  denemeler listede yok, başka tenant 404.
- Mutasyon: 11 mutasyon denendi; ilk turda 1'i hayatta kaldı (yukarıda), düzeltildi,
  hepsi yakalanıyor.
- Docker: gerçek hata veren bir endpoint tam 4 istek aldıktan sonra kapandı,
  bekleyen teslimatları `dead` oldu; kapalıyken `replay` → `409 endpoint_disabled`;
  URL düzeltilip açıldıktan sonra replay başarılı; deneme numaraları 1..4
  kesintisiz, değişmez bozulmadı.
