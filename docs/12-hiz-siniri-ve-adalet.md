# Faz 12: Hız sınırı ve adalet

İki ayrı "gürültülü komşu" sorunu vardı ve ikisi birlikte ele alındı:

1. **API'de sınır yoktu:** bir kiracı sınırsız istek atıp kuyruğu doldurabiliyordu.
2. **Teslimatta yuvalar ortaktı** (Faz 11'in bulgusu): yavaş bir endpoint worker'ın bütün yuvalarını alıp sağlıklı endpoint'lerin gecikmesini saniyelere çıkarabiliyordu.

## 1. Kiracı başına hız sınırı

| Konu | Karar |
| --- | --- |
| Algoritma | Kiracı başına token kovası (`internal/ratelimit`, ~60 satır, yeni bağımlılık yok, saat enjekte edilebilir). |
| Anahtar | API anahtarı değil **kiracı**: aynı kiracının bütün anahtarları tek bütçeyi paylaşıyor; 10 anahtar açmak sınırı aşmıyor. |
| Kapsam | Bütün `/v1` istekleri, kimlik doğrulamadan **sonra**. Doğrulaması başarısız istekler bütçe harcamıyor (yoksa bir saldırgan başkasının bütçesini tüketemez zaten, ama sıra yanlış olsaydı geçersiz anahtarla gelen istek de sayılırdı). `/healthz`, `/readyz`, `/openapi.yaml` dışarıda. |
| Yanıt | `429 rate_limited`, belgeli hata zarfı, `Retry-After` (tam saniye, en az 1). Her yanıtta `RateLimit-Limit` ve `RateLimit-Remaining`. |
| Varsayılan | `RATE_LIMIT_RPS=50`, `RATE_LIMIT_BURST=100`; `0` = kapalı (başlangıçta uyarı loglanır). |
| Nerede | api sürecinin belleği. Faz 11 tavanın Postgres commit hızı olduğunu gösterdi; her isteğe bir veritabanı yazması o tavanı düşürürdü. Bedeli: N api kopyasıyla sınır N katı; yeniden başlatma bütçeleri doldurur. |
| Sözleşme | OpenAPI'de 14 `/v1` işleminin hepsine `429` eklendi. "Belgelenmiş ama hiç görülmemiş" denetimi sayesinde sözleşme testi her işlem için `429`'u gerçekten üretiyor ve `Retry-After`/`RateLimit-*` başlıklarını doğruluyor. |

**Docker'da:** 300 eşzamanlı istek, 162 ms içinde: **106 tane 200** (100'lük ani yük + o sürede dolan ~6–8), **194 tane 429**, hepsinde `Retry-After: 1`.

## 2. Endpoint başına eşzamanlılık sınırı

`WORKER_MAX_PER_ENDPOINT` (varsayılan 3, `0` = sınırsız): bir worker'ın tek bir
endpoint'e aynı anda en fazla kaç denemesi olabileceği.

- Worker, endpoint başına uçuştaki denemeleri sayıyor; sınırdaki endpoint'leri claim sorgusuna "meşgul" olarak veriyor, onlar atlanıyor.
- Tek bir claim de bir endpoint'ten en fazla o kadar alıyor. İlk tasarımımda bu yoktu ve **bir hataydı:** kuyruk zamana göre sıralı olduğu için ilk claim bütün partiyi yavaş endpoint'e verebiliyordu; sınır partinin tamamı kadar aşılırdı. Postgres, satır kilitleyen bir sorguda pencere fonksiyonuna izin vermediği için claim iki adımlı: önce adaylar kilitlenip endpoint başına numaralanıyor, sonra alınacaklar seçiliyor.
- Böylece bir endpoint'in uçuştaki denemesi en fazla `2*sınır-1` olabiliyor (sınırın altındayken bir claim tam pay alabilir), hiçbir zaman partinin tamamı değil.

### Bu fazda bulunan iki hata (ikisi de benim yeni kodumda)

**a) Eşzamanlı claim'ler birbirini aç bırakıyordu.** İlk sürümde aday satırlar kilitlenmiyordu. Her worker aynı ilk N adayı seçiyor, ilki alıyor, diğerleri onları kilitli bulup **boş** dönüyordu. Doğruluk bozulmadı (her satır yine tek kez verildi), ama birden çok worker ve sıcak bir endpoint varsa verim düşerdi.
- Mevcut çekişme testi bunu yakaladı ("yalnızca 1 claim'ci iş aldı").
- Düzeltme: adaylar da `FOR UPDATE SKIP LOCKED` ile alınıyor; claim'ciler farklı satırlara dağılıyor. Bir satır kilidi WAL'a yazıldığı için aday sayısı küçük tutuldu (parti × 4, en az 32).
- **Ama:** düzeltmeyi geri alan mutasyon, çekişme testinde **hayatta kaldı**. O test yalnızca sorgular gerçekten üst üste binerse yakalıyor, yani zamanlamaya bağlı. **Deterministik bir test eklendi:** bir işlem ilk 5 satırı tutuyor (başka bir claim'in ortasındaymış gibi), claim arkadaki 5'i almalı. Mutasyonla birlikte "0 tane aldı" diye yakalıyor.

**b) Sınır açıkken verim çöküyordu (357 → 80 teslimat/sn).** Yük ölçümü buldu: eşzamanlılık 50, sınır 0 iken 357/sn ve p99 520 ms; sınır 3 iken 80/sn.
- Sebebi: worker, istediğinden az satır dönen bir claim'i "kuyruk boş" sayıp yoklama aralığı kadar (500 ms) uyuyordu. Sınır açıkken az dönmenin anlamı ise "iş var, ama endpoint'ler sınırda": bir deneme 20–50 ms'de bitip yer açıyor, worker 500 ms bekliyordu.
- Düzeltme: claim kısa dönerse ve sınırda endpoint varsa, worker **bir deneme bitene kadar** bekliyor (en fazla yoklama aralığı).
- Regresyon testi: tek endpoint, sınır 1, yoklama 2 sn, 20 teslimat. Düzeltmeyle 0,29 sn; mutasyonla 10 sn'de 20'nin 5'i.

### Ölçüm: sınır neyi çözüyor, neyi çözmüyor

Faz 11'in karışık senaryosu: 20 endpoint, 2'si yavaş (500 ms), 2'si hep hata, 20 olay/sn (400 teslimat/sn). Hata düzeltildikten sonra:

| Yapılandırma | Sağlıklı p50 / p99 | Yavaş endpoint'ler p50 |
| --- | --- | --- |
| Eşzamanlılık 50, sınır 0 | 273 ms / 520 ms | 272 ms |
| Eşzamanlılık 50, sınır 3 | 74–190 ms / 389–1069 ms | **36 sn** |
| Eşzamanlılık 10, sınır 0 | 21–49 sn / 41–69 sn | 21–49 sn |
| Eşzamanlılık 10, sınır 3 | 4–5 sn / 8–35 sn (3 koşunun 2'si; 1'inde 58 sn) | 35–37 sn |

Dürüst okuma:
- **Tasarımda "eşzamanlılık 10'da sağlıklı p99 ~500 ms'ye inecek" demiştim; inmedi.** 10 yuva bu yük için yetersiz: 360 sağlıklı teslimat/sn × deneme başına ~35 ms ≈ 13 yuva gerekiyor. Sınır yeni kapasite yaratmıyor, yalnızca yavaşların sağlıklıları ezmesini engelliyor. Sağlıklıların p50'si 21–49 sn'den 4–5 sn'ye indi (3 koşunun 2'sinde).
- **Sınır işi korumuyor (work-conserving değil):** kapasite bolken (eşzamanlılık 50) bile yavaş endpoint'ler kendi 3 yuvalarıyla sınırlı kalıp 36 sn bekledi, boş yuvalar varken. Sınır 0'da onlar da 272 ms'deydi. Bu, statik bir sınırın bedeli.
- **Varsayılanı yine de 3 bıraktım.** En güçlü gerekçe **asılı kalan endpoint**: deneme başına 15 sn zaman aşımıyla, sınır yokken bir worker'ın bütün yuvaları 15 sn boyunca kilitlenebilir. Testte bu durumda sağlıklı endpoint'e hiç teslimat gitmiyordu; sınırla hepsi gidiyor.
- Sınır 1, eşzamanlılık 10: araç "geçersiz" dedi. Teslimat kaybolmadı; yavaş endpoint'ler 1 yuvayla 2 dakikalık bekleme süresine sığmadı. Tablolara girmedi.

Daha iyi çözüm (yapılmadı): boş yuva varken sınırı aşmaya izin veren, ama sağlıklılara her zaman bir pay ayıran (work-conserving) bir zamanlayıcı.

## Testler ve mutasyon

Geçerli 21 mutasyonun **20'si sonunda yakalandı**, 1'i hayatta kaldı:

| Bozulan | Sonuç |
| --- | --- |
| Kova: dolum tavanı yok, son zaman güncellenmiyor, `>0` hatası, boş başlıyor, kilit yok | 5/5 yakalandı |
| Kova: saat geri gidince token silinmesi | **ilk denemede hayatta kaldı**: test yalnızca "token üretmiyor"a bakıyordu; mutasyon 36.000 token silip kiracıyı bir saat kilitliyordu. "Sonra normal doluyor" denetimi eklendi; yakalandı. |
| Ara katman: API anahtarına göre sayma, kimlik doğrulamadan önce sayma, `Retry-After` yok, reddedilen istek yine işleniyor; config | 5/5 yakalandı |
| Worker: sınır yok sayılıyor, meşgul listesi boş, biten deneme sayılmıyor, negatif kabul; config | 5/5 yakalandı (ilk üçü yalnızca 10 dakikalık zaman aşımıyla yakalanmıştı, çünkü testim başarısızlıkta asılı kalıyordu; test hızlı başarısız olacak şekilde düzeltildi, artık 28 sn'de) |
| SQL: meşgul endpoint'ler dışlanmıyor, claim başına sınır yok, adaylar kilitlenmiyor (deterministik testle) | 3/3 yakalandı |
| Kısa claim'den sonra yine tam yoklama aralığı uyunuyor | yakalandı |
| SQL: kilitli satırda vade zamanının yeniden denetimi | **hayatta kaldı.** Bu, aynı ifade içinde aday seçimi ile kilit arasında başka bir işlemin teslimatı ileri tarihe ertelemesine karşı bir koruma; bu iç içe geçme bir testten deterministik olarak üretilemiyor. Bilinçli olarak testsiz koruma olarak bırakıldı. |

Ek: kaos testi artık endpoint başına sınırla çalışıyor (meşgul/sınır yolları kaos altında da işliyor); 4 ek kaos koşusu ve çekişme testi geçti; 18 paket `-race` ile yeşil.

## Yolda yaptığım hatalar

- Python ile düzenlediğim dosyalar Windows'ta CRLF satır sonuyla yazıldı (Faz 11'deki README uyarısının sebebi de buydu); gofmt yakaladı, hepsi LF'ye çevrildi, betikler artık `newline='\n'` ile yazıyor.
- sqlc, CTE takma adındaki bir sütunla karşılaştırılan parametrenin tipini çıkaramadı; açık tip dönüşümü (`::bigint`) eklendi.
- Store testinde beklenen sayıyı yanlış hesapladım (ikinci endpoint eklendikten sonraki olaylar ikisine de gidiyor: 5 değil 7 teslimat).
- İlk Docker denemesinde 130 isteği Git Bash'te ayrı `curl` süreçleriyle attım; süreçler o kadar yavaş başladı ki kova doldu ve hiç 429 çıkmadı. Tek süreçli eşzamanlı bir araçla tekrarlandı.

## Bilinçli sınırlar

- Hız sınırı api örneği başına ve bellekte (yukarıda).
- Kimlik doğrulaması başarısız istekler sınırlanmıyor (IP başına sınır yok).
- Endpoint sınırı statik, worker başına ve work-conserving değil.
- Kuyrukta kiracılar arası adil sıra yok; bir kiracının kuyruğu doldurmasını API sınırı engelliyor.
