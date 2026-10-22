# Faz 13: Web konsolu

Hook Mini'nin bir arayüzü yoktu: her şey curl ile yapılıyordu. Bu fazda api'nin
içine gömülü bir web konsolu eklendi (13a'da taslak çizildi ve onaylandı, 13b'de
gerçeği yazıldı). Konsol yalnızca herkese açık `/v1` API'sini, kiracının
anahtarıyla, sıradan bir istemci gibi kullanıyor; arka uca konsol için özel bir
kapı açılmadı. Tek yeni uç nokta `GET /v1/stats`, o da her istemcinin işine
yarayan bir özet.

## Kararlar

| Konu | Karar |
| --- | --- |
| Yığın | React 19 + TypeScript (strict, `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`) + Vite, `web/` altında. Yönlendirici, durum ya da bileşen kütüphanesi yok: History API üzerine ~40 satırlık bir yönlendirici ve bir `usePolling` kancası yetiyor. |
| Dağıtım | `npm run build` çıktıyı `internal/webui/dist`'e yazıyor; api `go:embed` ile ikili dosyanın içine alıyor. Ayrı sunucu, ayrı konteyner yok. Dockerfile'ın ilk aşaması (Node 24) konsolu derliyor, Go aşaması onu kopyalayıp gömüyor. |
| Derlenmemişse | `dist` boşken Go derlemesi yine çalışıyor (yer tutucu `.gitkeep` sayesinde) ve `/` konsolun nasıl derleneceğini anlatan bir sayfa gösteriyor. CI'daki go işi bu yüzden Node'a ihtiyaç duymuyor. |
| Yollar | `/`, `/sign-in`, `/endpoints`, `/deliveries`, `/send` uygulama kabuğunu (index.html), `/assets/*` dosyaları veriyor. Bilinmeyen yollar yine API'nin `404` zarfı; konsol API'yi gölgelemiyor (test var). |
| Anahtar | Varsayılan `sessionStorage` (sekme kapanınca gider). "Remember" işaretlenirse yalnızca `localStorage`; her zaman tek kopya. Yalnızca `Authorization: Bearer` başlığında gidiyor, URL'ye hiç girmiyor. Çerez yok (`credentials: 'omit'`), dolayısıyla CSRF'lik bir oturum da yok. `401` gelince konsol hemen çıkış yapıp anahtarı siliyor. |
| İçerik politikası | Sıkı CSP: `script-src 'self'`, `style-src 'self'`, `font-src 'self'`, `connect-src 'self'`, `frame-ancestors 'none'`, `base-uri 'none'`, `object-src 'none'`; satır içi betik, stil ya da yazı tipi yok. Ek olarak `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`. `localStorage` seçeneğini kabul edilebilir kılan şey bu: sayfada kendi betiğimizden başka betik çalışamıyor. |
| Yazı tipleri | Geist / Geist Mono, `@fontsource` paketleriyle uygulamanın içinde; CDN yok (CSP zaten izin vermezdi, gizlilik açısından da daha iyi). |
| Önbellek | Adı içerik özetini taşıyan dosyalar `max-age=31536000, immutable`; uygulama kabuğu `no-cache` (her yüklemede doğrulanır, yeni sürüm hemen görünür). |
| Görünüm | "Signal Room": koyu zemin, nane yeşili vurgu (`#6ef2c0`), CSS 3D (WebGL yok). `prefers-reduced-motion` bütün animasyonları kapatıyor. Telefon genişliğinde çalışıyor. |

## `GET /v1/stats`

Genel bakış ekranının sayıları için. Tek istekte:

- son 24 saatte **oluşturulan** teslimatların durumlara göre sayısı,
- şu anki birikim (`pending` ve `in_flight`, yaşına bakmadan),
- her endpoint için aynı 24 saatlik sayılar ve art arda başarısızlık sayısı.

Üç sorgu `REPEATABLE READ, READ ONLY` bir işlemde çalışıyor; yani üçü aynı anın
fotoğrafı, toplamlar birbiriyle tutarlı. Sorgular kiracıyla sınırlı.
`TestStats` 24 saat penceresini, pencere dışındaki eski bir `pending`'in yalnızca
birikimde sayıldığını, hiç teslimatı olmayan endpoint'in sıfırlarla geldiğini
ve başka kiracının verisinin karışmadığını doğruluyor; 7 mutasyonun hepsini
yakaladı. OpenAPI'de belgeli, sözleşme testi gerçek yanıtı şemaya karşı denetliyor.

## Ekranlar

| Ekran | İçerik |
| --- | --- |
| Giriş | Anahtarın biçimi önce yerelde denetleniyor, sonra `/v1/stats` ile gerçekten çalıştığı kanıtlanıyor; ancak ondan sonra saklanıyor. |
| Genel bakış | KPI'lar, canlı 3D boru hattı, "dikkat isteyenler" (ölü teslimatlar, kapalı endpoint'ler; tek tıkla replay/enable), endpoint başına başarı oranı. |
| Endpoint'ler | Oluştur, sır döndür, kapat/aç, sil (geçmişiyle birlikte silindiği açıkça soruluyor). İmza sırrı yalnızca oluşturma ve döndürme sonrasında bir kez gösteriliyor. |
| Teslimatlar | Durum ve endpoint süzgeci URL'de (`?status=dead`), deneme zaman çizelgesi, yük, replay. |
| Olay gönder | Tür, JSON yük ve `Idempotency-Key` gönderilmeden denetleniyor; teslimatlar bir dakika boyunca izleniyor, aynı anahtarla ikinci gönderim `200 · Idempotent-Replayed: true` gösteriyor. Aynı isteğin curl karşılığı da yazılıyor. |

İki küçük mantık dikkat istedi:

- **Canlı boru hattı geçmişi yeniden oynatmamalı.** İlk sorgu yalnızca taban
  çizgisi; paket yalnızca iki sorgu arasındaki **geçişlerden** doğuyor
  (başarılı, öldü, yeni bir denemeyle yeniden kuyrukta).
- **Kesilen deneme hata değil.** Worker dururken kesilen deneme sayılmıyor
  (Faz 8); teslimat `in_flight`'tan `pending`'e deneme sayısı artmadan dönüyor.
  Konsol bunu "başarısız, yeniden denenecek" diye çizmiyor; zaman çizelgesinde
  de kesilen deneme ve kaybolan kira, endpoint hatalarından ayrı etiketleniyor.
- **Yüzde asla 100'e yuvarlanmıyor:** 9999 başarı ve 1 ölü teslimat %99.9
  görünüyor, %100 değil.

## Testler ve mutasyon

- **Vitest, 28 test:** API istemcisi (başlıklar, hata zarfı, `Retry-After`
  yalnızca 429'da, istek bütçesi, `401`'de çıkış, ağ hatası), anahtar deposu,
  biçimlendirme, boru hattı geçişleri, fan-out geometrisi.
- **Go:** `internal/webui` (yollar, başlıklar, önbellek, `/assets/` dışına
  tırmanma, derlenmemiş sayfa), router (konsol API'yi gölgelemiyor), `TestStats`,
  sözleşme testi.

Mutasyon sonuçları, ikisi de ilk turda boşluk gösterdi:

| Hedef | İlk tur | Bulunan boşluk | Sonra |
| --- | --- | --- | --- |
| Ön yüz mantığı (14 mutasyon) | 12/14 | Anahtar deposu yalnızca oturum→cihaz yönünde test ediliyordu; "remember" kaldırılınca cihazda kalan kopya yakalanmıyordu. Boru hattında deneme sayısı artmadan `pending`'e dönüş (kesilen deneme) test edilmiyordu. | İki test eklendi, 14/14 |
| Konsol başlıkları (12 mutasyon) | 10/12 | CSP alt dize olarak denetleniyordu: `font-src 'self' data:` içinde `font-src 'self'` geçtiği için test geçiyordu (tarayıcıda yaşadığım hatanın ta kendisi). `path.Clean` kaldırılınca `FileServerFS` yine reddediyordu ama `301` ve bir yıllık önbellek başlığıyla. | CSP artık yönerge yönerge **tam** karşılaştırılıyor; `/assets/../` için tam olarak `404` ve önbelleksiz yanıt isteniyor. 12/12 |

## Tarayıcıda uçtan uca deneme

Gerçek yığına (Docker, demo profili, alıcı ile) karşı, uygulamanın içindeki
tarayıcıda: bu deneme için ayrı bir kiracı ve anahtar açıldı, sonunda anahtar
iptal edildi. Yapılanlar: giriş; üç endpoint (`/ok`, `/delay/3000`,
`/status/503`), her birinin sırrının bir kez görünmesi; geçersiz URL, geçersiz
JSON ve tür hataları; olay gönderme, aynı anahtarla tekrar (`200`, replay), aynı
anahtarla farklı gövde (`422`); boru hattında paketler; ölü teslimatlar
süzgeci, zaman çizelgesi ve replay (yeni denemeler 4'ten numaralanıyor);
sır döndürme, kapat/aç, sil; 375 px genişlik; anahtar iptal edilince konsolun
kendiliğinden çıkış yapıp anahtarı silmesi.

Bulunanlar ve düzeltmeler:

1. **CSP yazı tiplerini engelledi.** Vite küçük yazı tipi alt kümelerini
   `data:` URI olarak CSS'e gömüyordu; `font-src 'self'` bunları reddetti.
   Politikayı gevşetmek yerine gömme kapatıldı (`assetsInlineLimit: 0`).
   Testin bunu neden yakalamadığı yukarıda.
2. **Aynı sunucudaki endpoint'ler ayırt edilemiyordu:** her yerde yalnızca
   `receiver:9000` yazıyordu. Artık sunucu + yol gösteriliyor
   (`receiver:9000/status/503`). Sorgu dizesi bilerek gösterilmiyor: içinde
   belirteç olabilir (test var).
3. **"1 deliveries"** tekil/çoğul hatası.
4. **3D kuyruk yığını dar ekranda alttaki yazının üstüne biniyordu.** İlk
   denemem bir medya sorgusuydu ve işe yaramadı: önemli olan pencere değil
   sahnenin kendi genişliği. Kapsayıcı sorgusuna (`@container`) geçildi;
   yığına her genişlikte yer bırakılıyor, telefonda yığın gizlenip şeritler
   bütün genişliği alıyor.

## Yolda yaptığım hatalar

- `React.ReactNode`'u içe aktarmadan kullandım, `tsc` yakaladı.
- Kapsayıcı sorgusu kurallarını temel `.plane` kuralından **önce** yazdım; aynı
  özgüllükte sonraki kural kazandığı için telefonda hiçbir şey değişmedi.
  Ekran görüntüsünde fark edip sıralamayı düzelttim.
- CSP düzeltmesinden sonra konsolda hâlâ hata görünce düzeltme çalışmadı
  sandım; hatalar yeniden yüklemeden önceki kayıtlardı. Yeni bir sekmede
  konsolun boş olduğu ve sunulan CSS'te `data:font` kalmadığı doğrulandı.

## Bilinçli sınırlar

- Konsol ince bir istemci, onlarca endpoint için tasarlandı: olay türleri ve
  endpoint adları tarayıcıda birleştiriliyor (Teslimatlar ve Olay gönder ilk
  100, Endpoint'ler sayfası 500 endpoint okuyor); canlı görünümler en fazla
  altı endpoint çiziyor; `/v1/stats` kiracının bütün endpoint'lerini
  sayfalamadan döndürüyor.
- Canlı görünümler akış (SSE/WebSocket) değil yoklama kullanıyor (1–30 sn,
  sekme gizliyken duruyor).
- Sayılar sayım; p99 gibi bir gecikme ölçüsü yok. Replay'in "turu"
  gösterilmiyor, yalnızca numaralı denemeler.
- "Remember" anahtarı `localStorage`'da tutuyor; aynı kökende çalışan her betik
  okuyabilir. Kabul edilebilir olmasının tek nedeni sıkı CSP.
- Ekran görüntüleri ve CI'ın gerçekten çalıştırılması Faz 14'te.
