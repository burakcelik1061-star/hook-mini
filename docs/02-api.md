# Faz 2: API

Bu fazda dış dünyaya açılan yüzey yazıldı: kimlik doğrulama, endpoint yönetimi
ve olay alma. Olaylar kalıcı olarak kuyruğa alınır ama **henüz hiçbir şey
gönderilmez**; gönderim motoru Faz 3 ve 4.

## Katmanlar

```
HTTP (internal/httpapi)  →  Servis (internal/service)  →  Store (internal/store, sqlc)
```

- **httpapi** yalnızca istek/yanıt çevirir: JSON çözme, durum kodları, başlıklar.
- **service** iş kurallarını taşır: doğrulama, idempotency, fan-out, yetkilendirme.
  HTTP'siz test edilebilir.
- **store** yalnızca SQL çalıştırır.
- **credential**: API anahtarı ve imza gizli anahtarı üretimi/doğrulaması.
- **cmd/admin**: tenant ve anahtar işlemleri (HTTP'de yok).

## Alınan kararlar ve nedenleri

- **Tenant oluşturma HTTP'de yok.** Kimliksiz bir "hesap aç" ucu, kötüye
  kullanım yüzeyi demektir. Tenant ve ilk anahtar `admin bootstrap` ile
  operatör tarafından oluşturulur; ham anahtar yalnızca bir kez yazdırılır.
- **API anahtarı biçimi `hm_<8 hex>_<64 hex>`.** İlk kısım genel önek (arama için),
  ikincisi 256 bit rastgele sır. Veritabanında yalnızca SHA-256 hash'i durur ve
  sabit zamanlı karşılaştırılır. Biçimi bozuk girdi veritabanına hiç gitmez.
- **Kimlik doğrulama hataları birbirinden ayırt edilemez.** Yok, iptal edilmiş,
  yanlış sır, bozuk biçim: hepsi aynı `401` gövdesi. Testle doğrulandı: yanıtlar
  (istek kimliği hariç) birebir aynı.
- **Veritabanı hatası `401` değil `500`.** Veritabanı çökünce kullanıcıya
  "anahtarın geçersiz" dememek için; ayrı test var.
- **Başka tenant'ın kaynağı `404`.** `403` olsaydı kaynağın var olduğunu
  söylemiş olurduk. Bozuk UUID de `404`.
- **Yanıtlar store yapılarından değil, ayrı DTO'lardan üretilir.** Yeni bir
  veritabanı sütunu yanlışlıkla API'ye çıkmaz. Üstüne sqlc'de `json:"-"`
  etiketi var: iki ayrı savunma.
- **İmza gizli anahtarı yalnızca oluşturma yanıtında.** Sonraki hiçbir yanıtta
  (get, list, patch) yok; mutasyon testiyle doğrulandı. Yanıtlarda
  `Cache-Control: no-store`.
- **Olay + teslimatlar tek transaction'da.** Kabul edilmiş bir olayın
  teslimatları kaybolamaz.
- **Idempotency:** aynı anahtar + aynı içerik → `200` ve orijinal olay, yeni
  kayıt yok. İçerik karşılaştırması Postgres'te normalize `jsonb` üzerinde
  yapılır; anahtar sırası ve boşluk fark etmez. Aynı anahtar + farklı içerik →
  `422`. Eşzamanlı 24 istek aynı anahtarla gelince tam olarak bir olay oluşur
  (`-race` ile test edildi; anahtarı yok sayan ve uyuşmazlık kontrolünü
  kaldıran mutasyonlar testleri kırdı).
- **Sıkı JSON çözme:** yanlış `Content-Type` (`415`), boş/bozuk gövde, bilinmeyen
  alan (yazım hatası sessizce yutulmaz), tek değerden fazlası (`400`), boyut
  sınırı (`413`: endpoint 16 KiB, olay 256 KiB).
- **Hata zarfı tek biçim:** `{"error": {code, message, field?, request_id}}`.
  `code` kararlı, `message` insanlar için. 500'de ayrıntı yalnızca loga yazılır.
- **Sayfalama keyset, opak imleç.** Bozuk imleç ve uç değerler `400` (500 değil).
- **Her yanıtta `X-Request-Id`.** İstemci destek isterken kullanabilir.

## Bilinçli sınırlar

- **URL doğrulaması yalnızca sözdizimi.** `http(s)`, host var, kimlik bilgisi yok,
  boşluk/parça yok. Özel ağlara, loopback'e ve bulut metadata adreslerine
  gönderimin engellenmesi (SSRF) **Faz 4'te, bağlantı kurulurken** yapılacak:
  bir host adının IP'si doğrulamadan sonra değişebilir.
- Hız sınırlama (rate limit) yok.
- Endpoint'i devre dışı bırakma/açma ucu yok (Faz 5).
- Silinen endpoint'in teslimat geçmişi de silinir.
- Anahtar listeleme ucu yok; `admin` yalnızca oluşturur ve iptal eder.

## Doğrulama

- Birim: kimlik bilgileri, doğrulama tabloları (30'a yakın kötü girdi), imleç.
- Servis, gerçek Postgres'te: kimlik doğrulama, izolasyon, fan-out, idempotency,
  eşzamanlılık, sayfalama.
- HTTP uçtan uca, gerçek sunucu + Postgres: kimlik doğrulama, izolasyon, 30
  doğrulama senaryosu, idempotency durum kodları, büyük sayı ve Unicode'un
  bozulmadan saklanması, 500'de sızıntı olmaması.
- Mutasyon denemeleri (idempotency anahtarını yok sayma, uyuşmazlık kontrolünü
  kaldırma, sırrı her yanıta koyma, 500'de ham hata): hepsi ilgili testleri kırdı.
