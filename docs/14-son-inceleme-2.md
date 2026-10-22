# Faz 14: İkinci son inceleme, CI provası, ekran görüntüleri

Faz 8'deki son incelemeden (eaefba7) sonra beş faz daha eklendi: saklama
süresi, sır şifreleme, yük ölçümü, hız sınırı ve web konsolu. Toplam yaklaşık
4.100 satır Go/SQL ve 3.300 satır ön yüz kodu hiç ikinci bir gözden
geçmemişti. Bu faz onları inceledi, bulunanları düzeltti, CI'ı GitHub'a
göndermeden olabildiğince gerçeğe yakın koşturdu ve README için ekran
görüntüleri aldı.

## Yöntem

En riskli yerden başlayarak, satır satır: şifreleme ve anahtar değişimi,
eşzamanlılık (claim sorgusu), veri silme (saklama süresi), sonra yeni HTTP
yüzeyi ve ön yüz. Her bulgu düzeltilmeden önce **kanıtlandı** (`EXPLAIN`
çıktısı, tarayıcıda ölçüm), her düzeltme bir testle korundu ve testin gerçekten
koruduğu mutasyonla gösterildi.

**Temiz çıkanlar:** `secretbox` (rastgele 96 bit nonce, uç noktaya bağlı AAD,
anahtar ya da değer sızdırmayan hatalar), yeniden şifreleme (yalnızca satır hâlâ
okunan değeri tutuyorsa yazıyor, yarışta bir tur daha dönüyor), hız sınırlayıcı
(kova taşmıyor, sıfıra bölme yok, `Retry-After` yukarı yuvarlanıyor), claim
sorgusu (worker boş dizi gönderiyor, `NULL` değil: `<> ALL(NULL)` hiçbir satır
almazdı), saklama süresi (bitmemiş teslimata hiç dokunmuyor, olayı ancak hiçbir
teslimat ona bağlı değilken siliyor), yapılandırma doğrulaması, yük aracı
(anahtarı yazdırmıyor), ön yüzün enjeksiyon yüzeyi (`innerHTML`, `eval`, dış
bağlantı yok), `usePolling` (istekler üst üste binmiyor, eski yanıt atılıyor).

## Bulgular

| # | Önem | Bulgu | Kanıt |
| --- | --- | --- | --- |
| 1 | Orta | README'deki anahtar değişimi adımları, örnekler tek tek yeniden başlatılırken teslimatları **öldürüyordu**. | Kod okuması + iki yeni test |
| 2 | Orta-düşük | `/v1/stats`'ın birikim sayımı kiracının **bütün geçmişini** tarıyordu. | `EXPLAIN` |
| 3 | Düşük | Teslimatlar'da süzgeç açıkken menüye tıklayınca adres ile görünüm ayrışıyordu; `useQuery` ölü koddu. | Tarayıcı |
| 4 | Düşük | Boru hattındaki paketleri silen zamanlayıcı hiç çalışmıyordu. | Tarayıcıda eski/yeni ölçüm |
| 5 | Düşük | Depolamayı engelleyen bir tarayıcıda konsol boş sayfa açılıyordu. | Kod okuması + test |

### 1. Anahtar değişimi teslimatları öldürüyordu

Adımlar "yeni anahtarı başa koy ve yeniden başlat" diye başlıyordu. Birden çok
süreç tek tek yeniden başlatılırken yeni anahtarı alan bir api, oluşturulan ya
da döndürülen sırları yeni anahtarla şifreliyor; henüz eski anahtarla çalışan
bir worker bunları açamıyor ve teslimatı **ölü** işaretliyordu ("cannot be
decrypted"). Koddaki yorum tam da yanlış varsayımı söylüyordu: "başlangıç
denetimi eksik anahtarı dışarıda bırakır". Başlangıç denetimi yalnızca o ana
kadar yazılmış olanı görüyor; çalışırken yazılanı değil.

Düzeltme iki katmanlı:

- **Doğru sıra (README):** yeni anahtar önce **ikinci** sıraya eklenip her
  süreç yeniden başlatılıyor (artık hepsi açabiliyor, hâlâ eskisiyle
  şifreliyor), sonra başa alınıyor, sonra `migrate`, sonra eski anahtar
  çıkarılıyor. Dört adım.
- **Savunma (worker):** sır, worker'da olmayan bir anahtarla şifrelenmişse
  (`ErrUnknownKey`) ya da şifresizse (`ErrNotSealed`: yükseltme sırasında eski
  bir ikili yazmış olabilir) teslimat artık ölmüyor. Yeni `PostponeDelivery`
  sorgusu onu **deneme yapmadan** kuyruğa geri veriyor, bir dakika sonra yeniden
  alınıyor, worker her seferinde hata logluyor. Deneme satırı yazılmıyor,
  `attempt_count` değişmiyor (değişmez: deneme sayısı = deneme satırı sayısı),
  yeniden deneme hakkı harcanmıyor. Kiracının gördüğü `last_error` yapılandırma
  ayrıntısı içermiyor. Bozuk, kurcalanmış ya da başka uç noktadan kopyalanmış
  değer (`ErrOpen`, `ErrMalformed`) eskisi gibi bırakılıyor: hiçbir yapılandırma
  değişikliği onu açmaz.

Testler: anahtarı olmayan worker teslimatı ertelemeli, istek göndermemeli, bir
dakikadan önce yeniden almamalı; anahtarı olan worker sonra teslim etmeli. Aynısı
şifresiz sır için: ertelenmeli, `ReencryptSecrets` (migrate'in yaptığı) sonrası
teslim edilmeli. **6/6 mutasyon yakalandı** (dalı geri almak, şifresiz durumu
çıkarmak, her açma hatasını ertelemek, gecikmeyi sıfırlamak, logu kaldırmak,
ertelemede deneme saymak).

### 2. Birikim sayımı bütün geçmişi tarıyordu

Sorgudaki yorum "yalnızca bitmemiş teslimatlara bakar" diyordu ama hiçbir
indeks bunu sağlamıyordu. `status IN ('pending', 'in_flight')` kısmi
indekslerin (`WHERE status = 'pending'`, `WHERE status = 'in_flight'`)
koşulunu gerektirmediği için planlayıcı kiracı indeksini kullanıp saklama
süresindeki (90 gün) **bütün** teslimatları okuyordu; açık her konsol sekmesi
bunu 5 saniyede bir yapıyordu. `EXPLAIN` bunu gösterdi (`Index Scan using
deliveries_tenant_idx ... Filter: status = ANY(...)`).

Düzeltme: her durum için ayrı bir sayım; her biri kendi kısmi indeksini
kullanıyor. Bedeli: kısmi indeksler bütün kiracıların bitmemiş teslimatlarını
tutuyor, yani bu kuyruğun kendisi; worker'lar yetiştiği sürece küçük. Yeni
indeks eklemedim: her durum değişikliğinde yazma maliyeti getirirdi ve Faz 11
tavanın commit hızı olduğunu göstermişti.

Test: `TestStatsQueriesUseBoundedIndexes`, üç istatistik sorgusunun sqlc'nin
ürettiği metnini `EXPLAIN (FORMAT JSON)` ile inceliyor: birikim yalnızca iki
kısmi indeksi kullanmalı; 24 saatlik sayımlar teslimatlara ancak `created_at`
sınırlı bir indeks koşuluyla ulaşmalı. **3/3 mutasyon yakalandı** (eski sorgu,
iki sorgudan 24 saat sınırını kaldırmak).

### 3. Teslimatlar sayfasında adres ile görünüm ayrışıyordu

Süzgeç sayfanın kendi durumunda tutuluyor, adres `replaceState` ile
güncelleniyordu. "Ölü" süzgeci açıkken menüden "Deliveries"e tıklanınca adres
`/deliveries`'e dönüyor, görünüm "Dead"te kalıyordu; kopyalanan bağlantı
ekrandakini göstermiyordu. Sayfa artık `popstate`'i dinleyip süzgeci adresten
yeniden okuyor. Tarayıcıda doğrulandı: menü "All"a döndürüyor, Geri "Dead"e
götürüyor ve görünüm izliyor. Kullanılmayan `useQuery` silindi (yalnızca yol
değişince yeniden çizildiği için zaten yanlış çalışırdı).

### 4. Paket zamanlayıcıları hiç çalışmıyordu

Her paket grubunu 6,5 saniye sonra silen zamanlayıcı efektin temizleme
fonksiyonunda iptal ediliyordu. React temizlemeyi efektin her yeniden
çalışmasından önce çağırır; veri 2 saniyede bir geldiği için zamanlayıcı hiç
dolmuyor, paketler yalnızca şerit başına 6 sınırıyla atılıyordu. Görünür bir
hata yoktu (CSS animasyonu paketi saydamlaştırıyor) ama DOM'da 36'ya kadar ölü
düğüm kalıyordu. İşin ilginci Faz 13'teki tarayıcı testinde "en çok 12 paket"
ölçmüştüm (2 şerit × 6) ve bunu başarı saymıştım: sayı hatanın kendisiydi.

Zamanlayıcılar artık bir kümede tutuluyor, yalnızca sahne kaldırılınca iptal
ediliyor. Kanıt (aynı ölçüm, 3 olay, sonra 9 saniye sessizlik): **eski kodla 6
paket kaldı, yenisiyle 0**.

### 5. Depolama engellenince boş sayfa

Bazı tarayıcılarda (çerezler/site verisi engellenmişse) `window.localStorage`'ı
okumak bile hata fırlatıyor; modül yüklenirken fırlattığı için konsol hiç
açılmıyordu. `storageOrMemory` depolamayı bir yazma/silme denemesiyle sınıyor,
olmazsa bellekte tutuyor (anahtar sayfa yenilenene kadar yaşar). Test: engelli
ve dolu depolamada anahtar deposu çalışmalı, çalışan depolama olduğu gibi
kullanılmalı ve deneme iz bırakmamalı. **3/3 mutasyon yakalandı.**

Ön yüzün toplam mutasyon sonucu artık **17/17**.

## CI provası

İş akışı hiç çalışmamıştı ve GitHub'a göndermek kullanıcının onayına bağlı.
Bu yüzden adımları aynı imajlarla yerelde koşturdum:

- Kaynak: yalnızca git'in göreceği dosyalar (`git ls-files --cached --others
  --exclude-standard`), yani `.env`, `node_modules` ve derleme çıktısı yok;
  temiz bir checkout gibi.
- **go işi:** `golang:1.27` + `postgres:16-alpine` servisi: gofmt, vet, build,
  `sqlc diff`, `go test -race ./...`: **19 paketin hepsi geçti.**
- **web işi:** `node:24`: `npm ci`, typecheck, 29 test, build: **geçti.**
- **image işi:** Dockerfile bu fazda defalarca derlendi.
- `actionlint 1.7.7`: **0 hata.**

Kanıtlanmayan: GitHub'ın kendine özgü kısımları (`setup-go`/`setup-node`
önbelleği, servis konteynerine `127.0.0.1` port eşlemesiyle erişim; provada
ağ adıyla eriştim).

**GitHub'daki ilk koşu (2026-10-22):** depo private olarak gönderildi; ilk
çalışmada (run 39829101092) go, web ve image işlerinin üçü de ilk denemede
geçti. Yerel prova, GitHub'a özgü kısımlar dışında iş akışını doğru
öngörmüştü.

## Ekran görüntüleri

`docs/screenshots/` altında beş ekran (1440×900, 1,5 kat). Bilgisayarda kurulu
Edge'i başsız modda, DevTools protokolüyle (Node'un yerleşik WebSocket'i)
sürdüm; hiçbir şey indirilmedi ve kullanıcının Edge profiline dokunulmadı.
Veriler bu iş için açılan "Acme Inc" kiracısından, herkese açık API üzerinden
(dört endpoint, biri bilerek bozuk); kullanılan anahtar sonunda iptal edildi.
Görüntülerde anahtarın yalnızca açık ön eki (`hm_b64e4175…`) görünüyor.

## Yolda yaptığım hatalar

- Faz 13'te "sınırlı" diye yorum yazdım ama planı denetlemedim. Ders: bir yorum
  indeks kullanımı iddia ediyorsa bir plan testi de olmalı.
- Paket sayısını başarı sandım (bkz. 4).
- Mutasyon betiğine Python'dan `\n` yazarken gerçek satır sonu oluştu; bir
  mutasyon metni dosyada iki kez geçtiği için ilk denemede geçersiz sayıldı,
  daha özgül bir metinle yeniden koşturdum.

## Bilinçli olarak düzeltilmeyenler

- Konsol yollarına `HEAD` isteği `405` dönüyor (chi GET'i HEAD'e açmıyor);
  sağlık denetimleri `/healthz` kullanıyor.
- Oturum kapalıyken açılan derin bağlantı (`/deliveries?status=dead`) girişten
  sonra genel bakışa gidiyor, bağlantıya değil.
- Sekme bir süre gizli kaldıktan sonra açılınca o arada olanlar paket olarak
  bir kerede akıyor (şerit başına en çok 6).
- `/v1/stats` endpoint'leri sayfalamadan döndürüyor (README'de yazıyor).

## Kalan

Depo private olarak gönderildi ve CI yeşil. Deponun herkese açık olup
olmayacağı kullanıcının kararı.
