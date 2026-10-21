# Faz 4: İmza ve SSRF koruması

Bu fazda iki güvenlik açığı kapandı: alıcı, isteğin gerçekten Hook Mini'den
geldiğini doğrulayamıyordu (imza yoktu) ve bir tenant, endpoint URL'sini iç ağa
çevirerek worker'ı kendi altyapımıza istek atmaya kullanabiliyordu (SSRF).

## İmza: Standard Webhooks

Biçim yayımlanmış bir standart ([standardwebhooks.com](https://www.standardwebhooks.com)),
yani alıcılar her dilde resmi kütüphanelerle doğrulayabilir.

| Başlık | İçerik |
| --- | --- |
| `webhook-id` | Olay kimliği; her yeniden denemede aynı (tekrar ayıklama için). |
| `webhook-timestamp` | Bu denemenin imzalandığı an (Unix saniye). |
| `webhook-signature` | `v1,<base64 HMAC-SHA256>`; döndürme sırasında boşlukla ayrılmış iki imza. |

İmzalanan içerik: `<id>.<timestamp>.<ham gövde>`. Anahtar: `whsec_` + standart
base64 (32 bayt).

- **Uygulamanın doğruluğu standarda karşı kanıtlandı:** spesifikasyonun örnek
  vektörü (anahtar, id, zaman, gövde → beklenen imza) önce Go'dan bağımsız
  olarak Python `hmac` ile yeniden hesaplandı, sonra teste kondu. Go
  uygulaması aynı imzayı üretiyor.
- **Her deneme yeniden imzalanır.** Saatler sonraki bir yeniden deneme, alıcının
  5 dakikalık tekrar saldırısı penceresine takılmaz; `webhook-id` ise sabit kalır.
- **Alıcı kütüphanesi:** `pkg/webhook` (içe aktarılabilir). `Verify`, zaman
  penceresini (±5 dk) ve imzayı sabit zamanlı (`hmac.Equal`) kontrol eder;
  bilinmeyen sürümleri (`v2,...`) standart gereği yok sayar.
- **Anahtar biçimi değişti:** önceki fazlarda URL-güvenli base64 kullanılıyordu;
  resmi kütüphaneler standart base64 bekliyor. Yeni anahtarlar standart
  biçimde; çözümleyici eski biçimi de kabul eder.

### Anahtar döndürme

`POST /v1/endpoints/{id}/rotate-secret` yeni anahtarı **bir kez** döndürür. Eski
anahtar 24 saat daha geçerli kalır; bu sürede her istek iki imza taşır, alıcı
hazır olduğunda yeni anahtara geçer ve hiçbir teslimat reddedilmez. Sürenin
bitip bitmediğine veritabanı saati karar verir. Eski anahtarın kendisi hiçbir
yanıtta yer almaz (`json:"-"` + DTO; testle ve mutasyonla doğrulandı); yalnızca
`previous_secret_expires_at` görünür.

## SSRF koruması (`internal/safedial`)

**Kontrol bağlantı anında yapılır**, kayıt anında değil. `net.Dialer.Control`
kancası DNS çözümlemesinden sonra, bağlanılacak gerçek IP ile her bağlantı
denemesinde çalışır. Bu yüzden:

- Bugün genel bir IP'ye çözülüp yarın `127.0.0.1`'e çözülen bir ad (DNS
  rebinding) işe yaramaz.
- Birden çok IP döndüren adlarda her biri ayrı denetlenir.

Engellenenler: loopback, özel ağlar (RFC 1918, `fc00::/7`), link-local (bulut
metadata `169.254.169.254` dahil), belirtilmemiş, çok noktaya yayın, broadcast,
CGNAT (`100.64/10`), dokümantasyon/benchmark/ayrılmış aralıklar ve **IPv4'ü
IPv6 içinde taşıyan biçimler**: `::ffff:` (eşlenmiş), NAT64, 6to4, Teredo.

Ek kararlar:

- **Kayıt anında erken kontrol:** DNS gerektirmeyen durumlar (`127.0.0.1`,
  `[::1]`, `10.0.0.5`, `localhost`, `*.localhost`) `400` ile ve nedeniyle
  reddedilir. Adlar ise bağlantı anında yargılanır; kayıt sırasında DNS
  sorgusu yapılmaz.
- **Proxy yok sayılır.** Ortamdaki `HTTP_PROXY` bağlantıyı bizim adımıza açardı
  ve denetimin dışında kalırdı.
- **Yönlendirme takip edilmez** (Faz 3): bir endpoint worker'ı iç adrese
  yönlendiremez.
- **Güvenli varsayılan:** `ALLOW_PRIVATE_DESTINATIONS=false`. Yalnızca yerel
  geliştirme için açılır; açıksa api ve worker başlangıçta uyarı loglar.
  `delivery.Config` sıfır değeriyle de korumalıdır.

### Mutasyonun yakaladığı test boşluğu

`Unmap()` satırını silen mutasyon ilk denemede **hayatta kaldı**. Neden: Go'nun
`netip` yardımcıları (`IsLoopback`, `IsPrivate`…) `::ffff:` biçimini kendi
içinde açıyor, testteki tüm eşlenmiş örnekler de bu yardımcılara düşüyordu.
Ama önek listesi (CGNAT gibi) ve broadcast karşılaştırması açmıyor:
`Unmap()` olmasaydı `::ffff:100.64.0.1` **geçerdi**. Kod doğruydu, test bunu
kanıtlamıyordu; `::ffff:100.64.0.1`, `::ffff:198.18.0.1`, `::ffff:255.255.255.255`
eklendi ve mutasyon artık yakalanıyor.

## Bilinçli sınırlar

- Port kısıtlaması yok: genel bir IP'nin herhangi bir portuna istek atılabilir.
- Alıcının imzayı doğrulayıp doğrulamadığını bilemeyiz; belge ve kütüphane
  sunuyoruz.
- Anahtarlar veritabanında düz metin (HMAC için gerekli); diskte şifreleme yok.
- Aynı 24 saat içinde iki kez döndürmek en eski anahtarı hemen geçersiz kılar.

## Doğrulama

- Birim: SSRF tablosu (40+ adres, nedenleriyle), `CheckHost`, gerçek bağlantıda
  loopback reddi, adın değil çözülen IP'nin denetlenmesi (`localhost`), ham TCP
  bağlantısı, proxy'nin yok sayılması; imza (spesifikasyon vektörü, kurcalama,
  zaman penceresi, döndürme, anahtar çözümleme).
- Entegrasyon: teslim edilen her istek endpoint anahtarıyla doğrulanıyor;
  döndürmede iki imza, süre bitince tek; varsayılan worker loopback'e bağlanmıyor;
  servis ve HTTP kayıtta iç adresleri reddediyor.
- Mutasyon: 13 güvenlik özelliğinin her biri kaldırılınca ilgili test kırıldı.
- Docker uçtan uca:
  - Güvenli modda `127.0.0.1`, `localhost`, `169.254.169.254`, `[::1]`,
    `10.0.0.5` kayıtta `400` (nedenleriyle); `postgres:5432` ve
    `receiver:9000` kabul edildi ama çözülen `172.18.0.x` bağlantı anında
    engellendi, alıcıya **0** istek gitti.
  - Demo modda gerçek alıcı imzayı doğruladı; döndürmeden sonra eski ve yeni
    anahtar ayrı ayrı kabul edildi (2 imza); ilgisiz anahtar `401`, teslimat
    `dead`. Loglarda anahtar yok.
