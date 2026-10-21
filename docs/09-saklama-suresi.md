# Faz 9: Saklama süresi (retention)

`events`, `deliveries` ve `delivery_attempts` tabloları sonsuza kadar
büyüyordu. Bu fazda süresi dolan veriyi silen bir temizleyici eklendi.

## Karar

| Konu | Karar |
| --- | --- |
| Ne silinir | Biten (`succeeded`/`dead`) teslimatlar, bitişi `RETENTION_PERIOD`'dan eskiyse; deneme satırları `ON DELETE CASCADE` ile birlikte gider. Ardından hiçbir teslimatı kalmamış, yeterince eski olaylar. |
| Ne asla silinmez | `pending` ve `in_flight` teslimatlar; en az bir teslimatı duran olay. |
| Varsayılan süre | 90 gün (`2160h`). `0` = sonsuza kadar sakla. `0` dışında en az 24 saat: `1s` gibi bir yazım hatası bütün geçmişi silmesin. |
| Kim çalıştırır | Her worker, başlangıçta ve her `CLEANUP_INTERVAL`'da (varsayılan 1 saat). Ayrı bir servis yok. |
| Nasıl | 1000'lik partilerle; her parti kendi ifadesi, uzun kilit yok. |

## Güvenlik: temizleyici neye karşı korunuyor?

- **Eşzamanlı temizleyiciler:** `FOR UPDATE SKIP LOCKED`. Başkasının kilitlediği satır atlanır, temizleyici hiç beklemez. Testte 4 temizleyici 60 teslimatı birlikte siliyor ve rapor ettikleri toplam tam 60.
- **Replay:** bir teslimat replay ile `pending`e dönmüşse durum süzgecine uymaz ve kalır. Replay satırı kilitliyorsa atlanır. Temizleyici önce silmişse replay bir şey bulamaz ve (Faz 8'de eklenen eşleme sayesinde) `404` alır.
- **Fan-out:** olay eklenirken olay satırında `FOR KEY SHARE` tutulur; temizleyicinin `FOR UPDATE`'i bununla çakışır ve olay atlanır.
- **Yanlış yapılandırma:** 24 saatten kısa süre başlangıçta hata.
- **Sorgular indeks kullanır:** migration 00006 iki indeks ekliyor (`deliveries_finished_idx`, kısmi; `events_created_idx`). Bir test, temizleyicinin gerçekte gönderdiği ifadeleri `EXPLAIN` ile inceliyor ve hiçbir yerde tam tablo taraması olmadığını doğruluyor.

## Bilinçli sonuç: idempotency penceresi

Olay silinince **idempotency anahtarı da** gider. `RETENTION_PERIOD` geçtikten
sonra aynı anahtar yeni bir olay oluşturur. Bu, saklama süresinin doğal
bedeli ve README'nin "bilinen sınırlar" bölümünde yazıyor.

## Mutasyon sonuçları

`internal/store/retention.sql.go` (üretilen kod) ve `retention.go` üzerinde
bilerek bozuldu. **Sonunda her biri yakalandı**, şu istisnalarla:

| Bozulan | Sonuç |
| --- | --- |
| Yaş karşılaştırması ters (teslimat, olay) | yakalandı (2) |
| `SKIP LOCKED` kaldırıldı (teslimat, olay) | yakalandı (2): temizleyici kilitli satırı beklerken zaman aşımı |
| Parti sınırı yok sayıldı (teslimat, olay) | yakalandı (2). **Olay için ilk denemede hayatta kaldı**: olay partileri hiç doğrudan sınanmıyordu; `[3 3 3 1]` doğrulaması eklendi. |
| Olay koruması (`NOT EXISTS`) tek tek kaldırıldı | **hayatta kaldı**, çünkü iki koruma var ve biri diğerini örtüyor (bilinçli savunma katmanı). **İkisi birden** kaldırılınca yakalandı: bekleyen teslimatı olan olay ve teslimatı silindi. |
| Durum süzgeci SELECT'ten kaldırıldı | **ilk denemede hayatta kaldı**: indeks testi, DELETE'in kendi indeks kullanımını görüp yanıldı. Test artık planda hiç `Seq Scan` olmamasını arıyor; yakalandı. |
| Durum süzgeci DELETE'ten kaldırıldı | hayatta kaldı ve **eşdeğer** çıktı: satırı SELECT kilitliyor ve `completed_at` ancak biten durumlarda dolu (CHECK kısıtı). Bu yüzden korumayı **kaldırdım** ve SQL yorumunu düzelttim. |
| Bir parti sonrası dur, `Enabled` sıfır için doğru, olaylar hiç silinmiyor | yakalandı (3) |

## Dersler

1. Bir testin "geçmesi", doğru şeyi sınadığını göstermez: indeks testi önce kendi kopyalarıyla çalışıyordu, sonra gerçek ifadeleri yakalayan bir sarmalayıcıya çevrildi.
2. Eşdeğer bir mutasyon bir uyarıdır: korumayı kaldırdığında hiçbir şey değişmiyorsa, ya o koruma gereksizdir ya da test eksiktir. Burada gereksizdi, kaldırıldı.
3. Gerçek ortamda doğrulandı: Docker'da 200 günlük bir teslimat eklendi, worker yeniden başlatılınca "retention pass finished deleted_deliveries=1 deleted_events=1" yazıp ikisini de sildi.

## Bilinçli sınırlar

- Temizleyici kiracı bazlı farklı süre desteklemiyor; tek global süre.
- Silme kalıcı: arşivleme ya da dışa aktarma yok.
- Çok büyük bir birikim ilk geçişte uzun sürebilir (parti parti ilerler, durdurulabilir, ama ilk geçiş iş yükü yaratır).
