# Faz 8: Son inceleme (güvenlik ve eşzamanlılık)

Bu fazda kod tabanının tamamı, özellikle güvenlik ve eşzamanlılık açısından,
baştan sona yeniden okundu. Yöntem: satır satır tarama, kaldırılan davranışlar,
çağıranlar ve çağrılanlar, dile özgü tuzaklar, sarmalayıcılar, yeniden kullanım,
sadeleştirme, verimlilik, kök neden ve proje kuralları olmak üzere on ayrı açı,
ardından gözden kaçanlar için bir tarama daha. **16 bulgu** çıktı:

- **14'ü düzeltildi.** Her biri önce bir testle gösterildi, sonra düzeltildi,
  ardından mutasyonla testin gerçekten koruduğu kanıtlandı.
- **2'si değişiklik gerektirmedi**, gerekçesi aşağıda.

## Bulgular ve düzeltmeler

| # | Bulgu | Düzeltme | Kanıt |
| --- | --- | --- | --- |
| 1 | **İç ağ bilgisi kiracıya sızıyordu.** Go'nun ağ hata metinleri, çözülen IP'yi, worker'ın kendi adresini ve DNS sunucusunu içeriyor. Bu metin deneme geçmişine yazılıp `GET /v1/deliveries/{id}` ile kiracıya dönüyordu. Kiracı iç ad kaydederek iç DNS'i haritalayabiliyordu. | `describe()` engel, DNS ve `OpError` hatalarını adres içermeyen metne çeviriyor (`dns: no such host`, `dial: connect: connection refused`, `blocked: ... (private network)`). Tam hata yalnızca worker loguna (`detail`) yazılıyor. | Birim testi beş adres/ad için sızıntı arıyor; 4+1 mutasyon yakalandı. Docker'da kiracı `dns: no such host` görüyor, log ise `127.0.0.11:53`'ü gösteriyor. |
| 2 | **`X-Request-Id` istemciden olduğu gibi alınıyordu.** Uzunluk ve karakter sınırı yoktu. Değer yanıtta ve her log satırında yer alıyordu. Başka bir isteğin kimliği taklit edilebiliyordu. chi'nin ürettiği kimlikler konteyner adını içeriyordu. | Kimliği her zaman sunucu üretiyor (`crypto/rand.Text`, 26 karakter). İstemcininki yok sayılıyor. Biçim OpenAPI'de `pattern` olarak tanımlı. | Sözleşme testleri artık her hata yanıtında biçimi denetliyor; 2 mutasyon (istemci kimliğine güvenmek, biçimi değiştirmek) yakalandı. |
| 3 | **IPv6 denetimi bir yasak listesiydi.** `IsGlobalUnicast`, `::a00:5` (IPv4 uyumlu), `::ffff:0:a00:5` (SIIT) ve `fec0::/10` (site-local) adreslerini kabul ediyordu. | IPv6 için **izin listesi**: yalnızca `2000::/3`. Ayrıca `2001::/23` (IETF) ve `3fff::/20` (belgeleme) yasak listesine eklendi. | 7 mutasyon yakalandı. "Aralık çok dar" mutasyonu **ilk denemede hayatta kaldı**: testte `3000::/4` içinden izinli bir adres yoktu. `3000::1` sınır testi eklenerek kapatıldı. |
| 4 | **Worker'ın kendi kestiği deneme, endpoint hatası sayılıyordu.** Drain süresi dolunca iptal edilen deneme endpoint'in hata sayacını artırıyor ve teslimatın bütçesinden düşüyordu. Son denemesindeki bir teslimat bir deploy yüzünden `dead` olabiliyordu. | Sender denemeyi `interrupted` olarak işaretliyor. Deneme geçmişe yazılıyor, ama endpoint sayacına dokunulmuyor ve `round_base` bir kaydırılıyor (bütçe değişmez). Teslimat hemen tekrar sıraya giriyor. Yalnızca teslimat satırı kilitlendiği için endpoint silmeyle kilitlenme (deadlock) olmuyor. | Hedefli test (tek denemelik bütçe, ardından ikinci worker teslim ediyor); 6 mutasyon yakalandı (biri üretilen sqlc koduna uygulandı, bkz. ders 1). Kaos testine yeni değişmez eklendi, aşağıda. |
| 5 | **Docker 10 sn'de SIGKILL atıyordu**, oysa drain 15 sn'ydi. | `stop_grace_period: 30s` (api ve worker). | Docker'da 14 sn'lik deneme sürerken `docker compose stop worker`: 14 sn bekledi, çıkış kodu 0, teslimat `succeeded`. |
| 6 | **`SHUTDOWN_TIMEOUT` < `DELIVERY_TIMEOUT` kabul ediliyordu.** Varsayılanlarda ikisi eşitti. | Varsayılan 20s. `SHUTDOWN_TIMEOUT` < `DELIVERY_TIMEOUT` + 5s ise başlangıçta hata veriliyor (kira kuralıyla aynı biçimde). | 3 mutasyon yakalandı. |
| 7 | **Replay sırasında endpoint silinirse 500 dönüyordu**, bir sonraki aralıkta ise yanıltıcı bir 409 dönüyordu. | İki aralıkta da 404. | Yarış, sqlc'nin `DBTX`'ini saran bir test yardımcısıyla tam o noktada **deterministik** olarak üretildi; 2 mutasyon eski iki hatayı aynen gösterdi. |
| 9 | **Panik yanıtı gövdesiz bir 500'dü.** Belgedeki hata zarfına uymuyordu ve istemciye `request_id` dönmüyordu. | Kendi recoverer'ımız: belgeli zarfı `request_id` ile yazıyor ve paniği yığın iziyle slog'a kaydediyor. Yanıt başlamışsa üstüne yazmıyor. `http.ErrAbortHandler`'ı geçiriyor. | Gerçek router üzerinden test edildi (veritabanısız bir `Service` nil dereference ile panik yapıyor); 5 mutasyon yakalandı. |
| 10 | **Azure WireServer (`168.63.129.16`)** engellenmiyordu. Adres genel görünüyor ama yalnızca VM içinden erişiliyor. | `/32` olarak eklendi. | `::ffff:` biçimi ve komşu adresler (izinli) test edildi. |
| 11 | **Veritabanı oturum sınırları yoktu.** HTTP isteklerinin bağlamında süre sınırı yok; kilit bekleyen bir sorgu bağlantısını süresiz tutabiliyordu. | `statement_timeout=30s`, `lock_timeout=10s`, `idle_in_transaction_session_timeout=60s`. Bağlantı dizesindeki değer önceliklidir. Migration'lar ayrı bağlantı kullandığı için etkilenmez. | Test gerçek bir kilit beklemesinin `55P03` ile bittiğini gösteriyor; 2 mutasyon yakalandı. |
| 12 | **`Verify` imza sayısını sınırlamıyordu.** Her imzayı her anahtar için yeniden çözüyordu. | İmzalar bir kez çözülüyor. 16'nın üstü `ErrTooManySignatures` veriyor. | Sınır ve bir fazlası test edildi; 2 mutasyon yakalandı. |
| 13 | **Yanıt başlığı sınırı varsayılan 1 MB'tı.** | 64 KB. | 200 KB'lık başlığın reddedildiği test edildi. |
| 15 | **Alıcının `truncate`'i bir UTF-8 karakterini ortasından kesebiliyordu.** | Kesme noktası karakter başına geri çekiliyor. | 2, 3 ve 4 baytlık karakterlerle test edildi; 2 mutasyon yakalandı. |
| 16 | **Demo alıcısı servis imajının içindeydi.** | Ayrı `receiver` hedefi ve imajı. Servis imajı yalnızca `admin`, `api`, `migrate` ve `worker` içeriyor. | Docker'da iki imajın içeriği listelendi. |

## Değişiklik gerektirmeyen iki bulgu

- **8. Replay'de endpoint durumu UPDATE'in dışında denetleniyor.** Kontrolden
  hemen sonra endpoint kapatılırsa sonuç, "replay, kapatmadan hemen önce oldu"
  sıralamasıyla birebir aynı olur: teslimat `pending` olur, worker onu "endpoint
  kapalı" gerekçesiyle bırakır. İki işlem zamanda örtüştüğü için bu geçerli bir
  sıralama; istemciye yanlış bir şey söylenmiyor.
- **14. Replay `last_error`'u temizlemiyor.** `last_status_code` ve
  `last_error` "en son denemenin sonucu" demek. Yeniden deneme bekleyen bir
  teslimat da son hatayı aynı şekilde gösteriyor; silmek bilgi kaybı olurdu.
  Bu anlam artık OpenAPI'de açıkça yazıyor.

## Kaos testi ne öğretti

Kesilen denemeler kaos testinde **hiç oluşmuyordu**: drain süresi (200 ms)
istek süresinden (150 ms) uzundu, yani her deneme drain bitmeden kendiliğinden
sona eriyordu. Drain 20 ms'ye indirildi. Artık her koşuda 4 ile 16 arasında
kesilen deneme oluşuyor ve sıfır olması "test iddia ettiğini sınamadı" hatası
sayılıyor.

Eklenen değişmezin ilk hali **yanlıştı, kod değil**. "Kesilen denemenin
numarası `round_base`'den büyük olamaz" diye yazmıştım. Kaos bunu 3 koşuda
"çiğnedi". Geçmiş incelendiğinde: #1 kayıp kira (sayılır, `round_base` 0
kalır), #2 kesildi (`round_base` 0'dan 1'e çıkar), #3 başarılı. Kesilen deneme
gerçekten sayılmamıştı; yanlış olan benim formülümdü. Doğru değişmez şu: **hiç
replay edilmemiş bir teslimatta `round_base`, kesilen deneme sayısına eşittir**
(replay'ler takip ediliyor). Bu değişmez, eski davranışı geri getiren iki
mutasyonu da yakalıyor.

## Mutasyon özeti

37 geçerli mutasyonun **hepsi sonunda yakalandı**. Biri ilk denemede hayatta
kaldı ve eksik test eklenerek kapatıldı (bulgu 3). Ayrıca 6 mutasyon
geçersizdi; hepsi benim hatamdı ve sayılmadı:

- 2'si `.sql` dosyasına uygulandı. Go kodu sqlc'nin **ürettiği** sorgu metnini
  kullandığı için davranış değişmedi; üretilen dosyaya uygulanarak
  tekrarlandı (ders 1).
- 1'i satır sonu (CRLF/LF) uyuşmadığı için uygulanmadı.
- 1'i eşdeğerdi (`+1 -1 +1`).
- 1'i hiçbir şeyi değiştirmiyordu.
- 1'i derlenmedi.

## Dersler

1. sqlc kullanan bir projede SQL mutasyonu **üretilen koda** uygulanmalı ya da
   ardından `sqlc generate` çalıştırılmalı. Aksi halde "hayatta kaldı" sonucu
   yanıltır.
2. Bir değişmez yazmadan önce, kodun o alanı nasıl değiştirdiği tam olarak
   modellenmeli (`round_base = attempt_no` değil, `round_base + 1`).
3. Bir test bir yolu "sınıyor" görünebilir ama hiç tetiklemiyor olabilir. Kaos
   testindeki "hiç oluşmadıysa hata" listesi bu yüzden var; kesilen denemeler
   de oraya eklendi.

## Bilinçli sınırlar

- Kiracı bir adın çözülüp çözülmediğini ve çözülen adresin sınıfını
  ("private network" gibi) hâlâ öğrenebilir; **adresin kendisini öğrenemez**.
  Bu, kiracının kendi hatasını anlaması için bırakıldı.
- Kesilen denemeler bütçeden düşmediği için, sürekli deneme ortasında yeniden
  başlatılan bir worker bir teslimatı sınırsız deneyebilir. Bu yalnızca
  operatörün yaptığı yeniden başlatmalarda olur; çökme ve `kill -9` hâlâ kayıp
  kira olarak sayılır.
- Oturum sınırları sabit değerler; ortam değişkeniyle değil, bağlantı dizesiyle
  değiştirilebilir.
- `MaxSignatures` (16) standardın koymadığı bir sınır.
- HTTP isteklerinin genel bir süre sınırı yok; sınırlanan yalnızca veritabanı
  işi.
- CI hâlâ hiç çalışmadı (depo henüz gönderilmedi).
