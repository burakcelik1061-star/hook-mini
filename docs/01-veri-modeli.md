# Faz 1: Veri modeli

Bu fazın amacı, sonraki fazların (API, worker, imza) üzerine kurulacağı şemayı
ve tip güvenli sorgu katmanını yazmaktı. Hiç HTTP veya worker kodu yok.

## Tablolar

| Tablo | Ne tutar |
| --- | --- |
| `tenants` | Hook Mini'yi kullanan uygulama. |
| `api_keys` | Tenant'ın anahtarları: genel önek + SHA-256 hash. Ham anahtar hiç saklanmaz. |
| `endpoints` | Abone URL'si, imza gizli anahtarı, durum, ardışık hata sayısı. |
| `events` | Gelen olay: tip, JSON yük, isteğe bağlı `idempotency_key`. |
| `deliveries` | Bir olayın bir endpoint'e gönderimi. Worker'ın yeniden denediği iş birimi. |
| `delivery_attempts` | Her HTTP denemesinin değişmez kaydı. |

## Alınan kararlar ve nedenleri

- **Tenant izolasyonu veritabanında zorlanır.** `deliveries`, `events` ve
  `endpoints`'e `(id, tenant_id)` bileşik yabancı anahtarlarıyla bağlanır.
  Bir sorguda hata olsa bile A tenant'ının teslimatı B'nin endpoint'ine
  bağlanamaz. Testle kanıtlandı: kısıtı silince test kırılıyor.
- **Fan-out tek SQL ifadesi** (`INSERT ... SELECT`) ve idempotent
  (`UNIQUE (event_id, endpoint_id)`). Olay ve teslimatları aynı transaction'da
  yazılacağı için "olay kabul edildi ama teslimatı kayboldu" durumu olmaz
  (Faz 2'de uygulanacak).
- **Olay alımı idempotent:** `UNIQUE (tenant_id, idempotency_key)`. `NULL`
  anahtarlar birbirinden farklı sayılır, anahtar göndermeyen istemci etkilenmez.
  `CreateEvent` çakışmada satır döndürmez; çağıran orijinali okuyup yükleri
  karşılaştırabilir (çelişen tekrar isteği reddi Faz 2'de).
- **Teslimat yaşam döngüsü SQL kısıtlarıyla korunur.** `in_flight` ancak kilit
  süresiyle (`locked_until`) mümkün; `succeeded`/`dead` ancak `completed_at`
  ile. Kilidi temizlemeyi unutan bir worker hatası, kuyruğu bozmak yerine
  Postgres tarafından reddedilir.
- **Statüler `text + CHECK`, enum değil.** Yeni durum eklemek tek satırlık
  kısıt değişikliği; `ALTER TYPE` gerekmez.
- **Kısmi indeksler:** `WHERE status = 'pending'` ve `WHERE status =
  'in_flight'`. Biten teslimatlar birikse de indeks küçük kalır. Testte
  `enable_seqscan = off` ile planlayıcının worker sorgusu için gerçekten bu
  indeksi seçtiği doğrulanıyor.
- **API anahtarı için SHA-256, argon2 değil.** Anahtar 256 bit rastgele; kaba
  kuvvetle denenecek bir parola değil. Yavaş hash güvenlik eklemeden maliyet
  ekler. Arama genel önekle yapılır, hash sabit zamanlı karşılaştırılır.
- **`TouchAPIKey` dakikada en fazla bir kez yazar.** Kimlik doğrulama her
  istekte sıcak bir satıra yazmaya dönüşmesin.
- **Sayfalama keyset (`created_at, id`), OFFSET değil.** Eşzamanlı eklemelerde
  kararlı, sayfa başına sabit maliyet. Test, aynı `created_at` değerine sahip
  satırların sayfa sınırına denk geldiği durumda boşluk ya da tekrar olmadığını
  doğruluyor.
- **Sayaç artışı atomik** (`consecutive_failures + 1`), oku-değiştir-yaz değil;
  iki worker aynı anda başarısız olursa iki hata sayılır.
- **Sırlar yanlışlıkla JSON'a çıkmaz.** `endpoints.secret` ve
  `api_keys.key_hash` alanlarına üretilen yapıda `json:"-"` etiketi konur
  (`sqlc.yaml`). Faz 2'de bir handler `store.Endpoint`'i doğrudan
  serileştirse bile imza anahtarı sızmaz; testle doğrulandı.
- **Migration'lar Postgres advisory kilidiyle sıraya girer.** İki kopya aynı
  anda açılırsa aynı tabloyu iki kez yaratmaya çalışmaz. Kilidi bekleyen
  migrator saniyede bir yoklar. Deterministik test: kilidi test tutarken
  `Migrate` bloke oluyor, kilit bırakılınca ilerliyor; kilit kodu kaldırılınca
  test kırılıyor.
- **sqlc, goose migration'larını doğrudan okur.** Şemanın tek kaynağı
  migration'lar; üretilen kod bunlardan ayrışamaz. CI, `sqlc diff` ile fark
  varsa başarısız olur.

## Bilinçli sınırlar

- Endpoint gizli anahtarı **düz metin** saklanır, çünkü HMAC imzası için ham
  değer gerekir. Diskte şifreleme MVP sonrası.
- `ListDeliveries` isteğe bağlı filtreleri `... IS NULL OR ...` ile yapar;
  MVP ölçeğinde yeterli, çok büyük tablolarda filtre başına ayrı sorgu gerekir.
- Saklama süresi (eski olay/teslimat temizliği) yok.
- Bir endpoint silinince onun teslimat geçmişi de `CASCADE` ile silinir. Geçmişi
  korumak istenirse endpoint silmek yerine devre dışı bırakılır (`disabled`).
- `DisableEndpoint`, `RecordEndpointFailure` ve `RecordEndpointSuccess` tenant
  almaz: yalnızca worker çağırır, dışarıdan gelen bir id ile kullanılmamalı.
- Worker sorguları (`SKIP LOCKED` ile alma, tamamlama, yeniden deneme, ölü
  işaretleme, süresi dolmuş kilidi geri alma) **Faz 3'te**, worker ile birlikte
  tasarlanacak.

## Doğrulama

- 14 şema/sorgu testi gerçek Postgres 16'ya karşı geçti (her biri geri alınan
  transaction içinde).
- Mutasyon denemesi: bileşik yabancı anahtarı ve kısmi indeksi bilerek
  bozunca ilgili testler kırıldı; testler gerçekten koruma sağlıyor.
- Migration turu: yukarı, sıfıra kadar aşağı, tekrar yukarı. Aşağı
  yönde hiçbir tablo ya da fonksiyon kalmıyor, ikinci yukarıda şema aynı.
