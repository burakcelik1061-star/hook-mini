# Faz 0: Zemin

Bu fazın amacı, domain koduna geçmeden önce sağlam bir iskelet kurmaktı. Burada
hiçbir webhook mantığı yok; yalnızca sonraki fazların güvenle üzerine kurulacağı
temel var.

## Ne var

| Parça | Dosya | Neden |
| --- | --- | --- |
| Yapılandırma | `internal/config` | Ortam değişkenlerini okur, **tüm** hataları birlikte raporlar. |
| Log | `internal/logging` | `slog`, JSON (üretim) veya text (terminal); her satırda `service` alanı. |
| Veritabanı | `internal/database` | Bağlantı havuzu, Postgres hazır olana kadar geri çekilmeli yeniden deneme, goose migration. |
| HTTP | `internal/httpapi` | `/healthz`, `/readyz`, istek kimliği, istek logu, panic kurtarma. |
| Yaşam döngüsü | `internal/lifecycle` | SIGTERM/SIGINT ile zarif kapanış; devam eden istekleri bekler. |
| Binary'ler | `cmd/api`, `cmd/worker`, `cmd/migrate` | Kısa ve okunur birleştirme noktaları. |
| Migration | `db/migrations` | Binary'ye gömülü düz SQL; `set_updated_at()` tabanı. |

## Alınan kararlar

- **Liveness ile readiness ayrı.** `/healthz` veritabanına dokunmaz; yoksa bir
  veritabanı kesintisi sağlam süreçleri gereksiz yere yeniden başlatır.
- **Readiness hataları dışarı sızmaz.** Sürücü hata mesajı kullanıcı veya host
  adı içerebilir; ayrıntı yalnızca loga gider.
- **Tek altyapı bileşeni: PostgreSQL.** Kuyruk ileride `SKIP LOCKED` ile
  yapılacak, Redis yok.
- **`-race` yalnızca CI'da.** Windows'ta cgo ve C derleyicisi gerekir; yerelde
  `go test ./...`, CI'da `go test -race ./...`.

## Doğrulama durumu

- `go build`, `go vet`, `gofmt` ve birim testleri (config, httpapi, lifecycle)
  yerelde geçti.
- `internal/database` entegrasyon testi gerçek Postgres 16'ya karşı geçti:
  migration uygulanıyor ve ikinci çalıştırmada hiçbir şey yapmıyor.
- `docker compose up --build` temiz bir veritabanından denendi: migrate çıkış
  kodu 0 ile bitiyor, api ve worker ayağa kalkıyor, üç servis tek imajı
  (`hook-mini:dev`) paylaşıyor.
- Veritabanı durdurulunca `/readyz` 503, `/healthz` 200 veriyor; veritabanı
  geri gelince `/readyz` toparlanıyor. Hata nedeni yalnızca loga yazılıyor.
- `docker compose stop` ile api ve worker 1 saniyenin altında, çıkış kodu 0
  ile kapanıyor.
- Doğrulanmayan: `go test -race` (yalnızca CI'da çalışır).

## Kod incelemesinde bulunan ve düzeltilenler

- `http.Server.BaseContext` sinyal bağlamına bağlıydı; kapanışta devam eden
  isteklerin bağlamı iptal oluyor, zarif kapanışın amacı bozuluyordu.
  Kaldırıldı.
- `/readyz` başarısız olunca hata nedenini loga yazmıyordu (yanıtta
  gizliyordu ama kayıt da tutmuyordu). Artık `WARN` olarak loglanıyor.
- Compose üç ayrı imaj derliyordu; tek imaja indirildi.
