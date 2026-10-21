# Faz 10: İmza anahtarlarının şifreli saklanması

Endpoint imza anahtarları (`whsec_...`) veritabanında düz metin duruyordu.
Bir döküm ya da yedek sızarsa, saldırgan her kiracı adına geçerli imzalı
webhook üretebilirdi. API anahtarları gibi hash'lenemezler, çünkü imzalamak
için ham değer gerekir. Bu fazda veritabanında şifreli saklanmaya başladılar.

## Tasarım

| Konu | Karar |
| --- | --- |
| Algoritma | AES-256-GCM (Go standart kütüphanesi, ek bağımlılık yok); her değer için yeni rastgele nonce. |
| Saklanan biçim | `enc:v1:<anahtar kimliği>:<base64url(nonce‖şifreli metin‖etiket)>`. Düz metin (`whsec_`) ile karışamaz. |
| Bağlam (AAD) | Endpoint'in kimliği. Bir endpoint'in şifreli anahtarı başka bir endpoint'e kopyalanırsa açılmaz. Sütuna değil endpoint'e bağlı, çünkü döndürme işlemi (rotation) değeri `secret`'tan `previous_secret`'a SQL içinde taşıyor. |
| Anahtar | `SECRET_ENCRYPTION_KEYS`: virgülle ayrılmış base64 32 baytlık anahtarlar. İlki şifreler, hepsi açar. Anahtar kimliği, anahtarın kısaltılmış SHA-256 özeti (8 hex). |
| Zorunluluk | api, worker ve migrate anahtarsız başlamaz. Düz metne geri dönüş yolu **yok**: anahtarsız bir Service sırla ilgili her işlemde hata verir. admin anahtar istemez (sırlara dokunmaz). |
| Kimlik | Endpoint kimliği artık Go'da üretiliyor (`uuid.New()`): şifreli değer ona bağlı olduğu için satırdan önce bilinmesi gerekiyor. Şema değişmedi, yalnızca `CreateEndpoint` sorgusu. |
| Mevcut veri | `migrate`, SQL migration'lardan sonra düz metin anahtarları şifreliyor ve eski bir anahtarla şifrelenmişleri güncel anahtara taşıyor. |
| Başlangıç denetimi | api ve worker, veritabanında düz metin ya da tanımadıkları bir anahtarla şifrelenmiş değer varsa başlamıyor; neyin, kaç tane olduğunu ve ne yapılacağını yazıyor. |
| Çözülemeyen değer | Worker göndermeden önce çözer. Çözülemezse (bozulmuş ya da kurcalanmış) teslimat gerekçesiyle `dead` olur ve **hiç istek gitmez**. Eksik anahtar zaten başlangıçta yakalandığı için buraya yalnızca bozuk bir değer düşebilir. |
| Test anahtarı | `internal/secretbox/secretboxtest`: yalnızca test dosyaları içe aktarıyor, sabit anahtar üretim ikili dosyasına girmiyor. |

## Yeniden şifreleme neden basit bir döngü değil

`migrate`, api çalışırken de güvenle çalıştırılabilmeli. Tehlikeli senaryo
şu: yeniden şifreleme bir satırı okuyor, tam o sırada kiracı anahtarını
döndürüyor (yeni anahtar şifreli yazılıyor, **eski düz metin** anahtar
`previous_secret`'a taşınıyor), sonra yeniden şifreleme okuduğu eski değeri
geri yazıyor. Bu iki şekilde yanlış gidebilir: kiracının yeni anahtarı silinir
ya da düz metin `previous_secret`'ta kalır.

Çözüm iki parçalı:
- Yazma, satır **hâlâ okunan değerleri tutuyorsa** yapılıyor (`AND secret = eski AND previous_secret IS NOT DISTINCT FROM eski`).
- Değişmiş bir satır, bir sonraki turda yeniden ele alınıyor.

Test bu yarışı, sqlc'nin `DBTX`'ini saran yardımcıyla tam o noktada
**deterministik** olarak üretiyor.

## Docker'da gerçek doğrulama

| Senaryo | Sonuç |
| --- | --- |
| Anahtar yok | migrate açık mesajla durdu, api ve worker hiç başlamadı. Sadece postgres isteyen komutlar (testler için) çalışmaya devam etti. |
| Geliştirme veritabanındaki 7 düz metin anahtar | migrate `rewritten=7`; ardından 0 düz metin, 7 şifreli. |
| Yeni endpoint | Yanıtta `whsec_...` bir kez görünüyor, veritabanında `enc:v1:e55c0a51:...`, GET yanıtında hiçbir sır yok. |
| İmzalı teslimat (alıcı imzayı doğruluyor) | `signature=valid`; anahtar döndürüldükten sonra, alıcı yalnızca yeni anahtarı bilirken de `signature=valid`. |
| Veritabanına düz metin anahtar sokuldu | api başlamayı reddetti ("1 endpoint secrets are stored unencrypted..."); migrate onu şifreledi, api başladı. |
| Şifreleme anahtarı değiştirildi (yeni,eski → migrate → yeni) | `rewritten=9`, bütün değerler yeni anahtar kimliğinde; eski anahtar çıkarılınca her şey çalıştı ve teslimat yine `signature=valid`. |
| Anahtar erken çıkarıldı (yanlış anahtarla başlatıldı) | api reddetti: "...keys missing from SECRET_ENCRYPTION_KEYS: 73fbdef4 (10 secrets)". |

## Mutasyon sonuçları

Geçerli 28 mutasyonun **hepsi sonunda yakalandı**. İkisi ilk denemede
**hayatta kaldı** ve eksik testler eklendi; biri **pratikte eşdeğer** çıktı:

| Bozulan | Sonuç |
| --- | --- |
| secretbox: AAD iki tarafta da yok sayıldı, sabit nonce, düz metin geçirildi, tek anahtar kimliği, uzunluk/çift anahtar denetimi yok, bilinmeyen anahtar birinciyle açılıyor, son anahtarla şifreleniyor | 8/8 yakalandı |
| Service: oluşturma/döndürme düz metin yazıyor, çağırana şifreli değer dönüyor (2), yeniden şifreleme şifreli değeri düz metin sanıyor, yarış tekrar denenmiyor, `previous_secret` atlanıyor, denetim düz metni/bilinmeyen anahtarı görmüyor, SQL'deki yazma koruması kaldırıldı, liste `previous_secret`'a bakmıyor | yakalandı |
| Yeniden şifreleme ilk partiden sonra duruyor | **ilk denemede hayatta kaldı**: parti 500 ve testler bu sınırı hiç geçmiyordu. Parti boyutu değişken yapıldı, 2'lik partilerle 5 satırlık test eklendi; yakalandı. |
| Sayım `previous_secret`'ı düz metin sayıyor | **ilk denemede hayatta kaldı**: denetim testinde şifreli `previous_secret`'ı olan bir endpoint yoktu. Döndürme eklendi; yakalandı. |
| Keyset ilerletilmiyor | hayatta kaldı ve **pratikte eşdeğer**: yeniden yazılan satırlar listenin süzgecine artık uymadığı için sonraki sorgu kalanları zaten getiriyor. Keyset yalnızca sürekli yazılamayan patolojik bir satırda sonsuz döngüyü önler; bilinçli bırakıldı. |
| Worker: saklanan değerle imzalıyor, `previous` çözülmüyor, çözülemeyen teslimat bırakılmıyor, anahtar zorunlu değil | 4/4 yakalandı |
| Config: anahtar hiç zorunlu değil, hatalı anahtar yok sayılıyor | 2/2 yakalandı |

Geçersiz denemeler (benim hatam, sayılmadı): AAD mutasyonunun ilk hali tek
taraflıydı (sadece şifrelemede), bu yüzden önemsiz bir şekilde yakalandı; iki
taraflı haliyle tekrarlandı. Sabit nonce mutasyonunun ilk hali derlenmedi ve
betik o yüzden toplu çalıştırmanın geri kalanını durdurdu. Betik, derleme
hatasını tolere edecek ve iki noktalı mutasyonları destekleyecek şekilde
düzeltildi.

## Sürecin dürüst notları

- **Bir test başarısızlığı kodun değil ortamın hatasıydı.** Faz 9'un Docker denemesinden kalan compose worker'ı test veritabanında çalışıyordu ve testlerin oluşturduğu teslimatları kapıyordu (README'nin uyardığı durum). Konteynerler durduruldu, testler iki kez tekrarlandı ve geçti.
- **Bir testi yanlış yazmıştım:** iki teslimat bekliyordum, ama ikinci endpoint'i olaydan önce kapattığım için yalnızca bir tane oluşuyordu. Davranış doğruydu, beklenti düzeltildi.
- **Docker'da bir teslimat engellendi:** konteyneri yeni bir kabuktan, demo ayarı (`ALLOW_PRIVATE_DESTINATIONS=true`) olmadan yeniden başlattığım için worker özel ağdaki alıcıyı haklı olarak reddetti. Hata metni adres içermiyordu (Faz 8'deki düzeltme). Demo ayarıyla yeniden başlatınca teslimat `signature=valid` ile tamamlandı.
- `docker-compose.yml`'da önce `${SECRET_ENCRYPTION_KEYS:?...}` kullanmıştım. Bu, anahtar yokken **her** compose komutunu, testler için `postgres`'i başlatmayı bile, engelliyordu. Boş varsayılana çevrildi; reddetme işini ikili dosyalar açık mesajla yapıyor.

## Bilinçli sınırlar

- Anahtar ortam değişkeninde, bir KMS/Vault'ta değil. Veritabanı sızıntısına karşı korur; hem veritabanını hem süreçlerin ortamını ele geçiren birine karşı korumaz.
- Anahtar kaybolursa saklanan imza anahtarları kurtarılamaz (her endpoint için `rotate-secret` gerekir). Yedeklemek operatörün işi.
- api ve worker birlikte güncellenmeli: şifrelemeden önceki bir worker şifreli anahtarı okuyamaz.
- Worker'ın belleğinde çözülmüş anahtarlar geçici olarak bulunur; bu kaçınılmaz.
