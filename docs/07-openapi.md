# Faz 7: OpenAPI ve belgeler

Bu fazda API'nin yayımlanan sözleşmesi (`api/openapi.yaml`, OpenAPI 3.1)
yazıldı ve, asıl zor kısım, **koddan ayrışamaz** hale getirildi.

## Neden yalnızca bir YAML dosyası yeterli değil

Elle yazılan bir spesifikasyon zamanla yalanlaşır: biri yanıta bir alan ekler,
bir durum kodunu değiştirir, belge yerinde kalır. Okuyan kişi de belgeye
güvenir. Bu yüzden belge ile sunucu birbirini **iki yönde** denetliyor.

| Denetim | Neyi yakalar |
| --- | --- |
| Rota listesi ↔ belge (`chi.Walk`) | Belgede olmayan bir rota sunuluyor, ya da belgedeki bir işlem sunulmuyor. |
| Her gerçek yanıt → bildirilen şema | Belgelenmemiş alan, yanlış tip, yanlış `enum` değeri, eksik zorunlu alan. |
| Her gerçek durum kodu → bildirilen kodlar | Belgelenmemiş durum kodu (ör. 202 yerine 200). |
| **Belgelenmiş ama hiç üretilmeyen** yanıt | Belge hiç gerçekleşmeyen bir duruma söz veriyor. (Her işlem için bildirilen her durum kodu en az bir testte görülmek zorunda.) |
| Giden webhook (başlıklar + gövde) → `webhooks` bölümü | Worker'ın gönderdiği ile belgelenen ayrışıyor. |
| Belgedeki `example`'lar → kendi şemaları | Yanlış örnek. |
| Belgedeki **istek** örnekleri → sunucu | Okuyucuya "bunu gönder" denen şeyi sunucu reddediyor. |

`500` her işlem için ayrı sınanıyor: veritabanı kapalıyken kimlik doğrulama
ilk veritabanı erişimi olduğundan her `/v1` işlemi `500` veriyor ve her biri
bildirilen şemaya karşı doğrulanıyor.

## Kendi küçük doğrulayıcımız (`internal/openapicheck`)

Hazır bir OpenAPI kütüphanesi yerine belgenin kullandığı JSON Schema alt
kümesini uygulayan ~400 satırlık bir doğrulayıcı yazıldı. Nedenleri:

- Bağımlılık ve yüzey küçük; yalnızca `yaml.v3` eklendi.
- **Bilmediği bir anahtarla karşılaşırsa sessizce geçmez, hata verir**
  (`unsupported keyword`). Bilinmeyen bir anahtarı yok sayan doğrulayıcı her
  şeyi onaylar ve bütün sözleşme testlerini anlamsız kılar. Belge yeni bir
  anahtar (`oneOf` gibi) kullanırsa doğrulayıcının da genişletilmesi gerekir.
- Doğrulayıcının kendisi iki yönde test edildi: geçerli değeri kabul eder, 15
  bozuk durumu (eksik/fazla alan, yanlış tip, `enum`, `pattern`, uzunluk,
  sınır, tarih, UUID, `null`…) nedeniyle reddeder.

## Mutasyon sonuçları

Hem **belge** hem **kod** bilerek bozuldu; sözleşme testleri her seferinde
kırıldı (13/13; bir mutasyon ilk denemede derlenmediği için geçersizdi ve düzgün
haliyle tekrarlandı):

| Bozulan | Yakalayan |
| --- | --- |
| Belge: dönen bir alan belgelenmemiş | yanıt doğrulaması ("field ... is not in the document") |
| Belge: 409 bildirilmemiş | "answered 409, which the document does not declare" |
| Belge: hiç olmayan bir 418 bildirilmiş | "documented but never seen" |
| Belge: `enum`'dan gerçek bir değer (`dead`) çıkarılmış | yanıt doğrulaması |
| Belge: sunulan bir işlem eksik / adı yanlış | rota karşılaştırması |
| Belge: webhook gövdesinde alan adı, zorunlu başlık adı yanlış | webhook doğrulaması |
| Belge: bir alanın tipi yanlış (`attempt_no` string) | yanıt doğrulaması |
| Kod: her endpoint yanıtına belgesiz bir alan | yanıt doğrulaması |
| Kod: replay 202 yerine 200 dönüyor | durum kodu denetimi |
| Kod: belgede olmayan bir rota sunuluyor | rota karşılaştırması |
| Kod: gönderici `data` alanını `payload` diye adlandırıyor, bir başlığı düşürüyor | webhook doğrulaması |

## Sunum

`GET /openapi.yaml`, belgenin kendisini (derlemeye gömülü, `go:embed`) kimlik
istemeden sunar; test, sunulanın gömülü olanla aynı olduğunu doğrular. Belge
sunulan sürümle birlikte değişir, ayrı bir dosya olarak sapamaz.

## README

Baştan düzenlendi: içindekiler, hızlı başlangıç en başta, tek bir yapılandırma
tablosu, tasarım kararları, belge dizini ve açık bir **bilinen sınırlar**
bölümü (düz metin imza anahtarları, hız sınırlaması yok, saklama süresi yok,
toplu replay yok, polling, port kısıtı yok, yük testi yok, **CI henüz
çalışmadı**).

## Bilinçli sınırlar

- Doğrulayıcı JSON Schema'nın tamamını değil, belgenin kullandığı alt kümeyi
  destekler.
- **İstek gövdelerinin sunucuda belgeye göre doğrulanması yok**: sunucu kendi
  doğrulamasını yapar; belge ile ayrışmayı yalnızca örnekler ve elle yazılmış
  sınır testleri (`maxLength`, boyut sınırları) yakalar. Belgedeki
  `maxLength`/`pattern` değerleri sunucudaki sınırlarla sistematik karşılaştırılmaz.
- Açıklama metinleri (`description`) doğrulanamaz; yalnızca yapı denetlenir.
- Belge elle yazılır (önce-kod, sonra-belge); belgeden sunucu kodu üretilmez.
