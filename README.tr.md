# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)

**[English](README.md)** | **Türkçe**

Microsoft 365 Copilot'un WebSocket arayüzünü OpenAI/Anthropic uyumlu HTTP API'sine dönüştüren bir Go uygulamasıdır.

## Mimari

Uygulamanız -> M365Bridge -> substrate.office.com (SignalR) -> M365 Copilot Backend

## Ön Koşullar

- **Go 1.22+** kurulu ([indir](https://go.dev/dl/))
- Bu repoyu klonlamak için **git**
- **Microsoft 365 Copilot lisansı** (iş veya kurumsal hesap, Copilot erişimi olan) test edilmiş copilot chat (temel) hesabı
- [https://m365.cloud.microsoft](https://m365.cloud.microsoft) adresine giriş yapmış bir tarayıcı (kurulum sihirbazı token çıkarımı için)

## Özellikler

- Akışlı/akışsız çıktı ile metin sohbeti
- Çok modlu görsel girdi (OpenAI `image_url` ve Anthropic `image` içerik blokları; PNG, JPEG, GIF, WebP)
- ConversationId takibi ile çok turlu sohbet desteği
- Oturum izolasyonu (oturum başına ayrı M365 sohbetleri)
- Düşünme/akıl yürütme içeriği çıkarımı (OpenAI için `reasoning_content`, Anthropic için `thinking` blokları)
- Simüle edilmiş tool calling (istemci tanımlı araçlar hem OpenAI hem Anthropic uç noktalarında, streaming ve non-streaming modlarda çalışır)
- OpenAI uyumlu API uç noktaları
- Anthropic uyumlu API uç noktaları (özel SSE işleyiciler)
- API anahtarı kimlik doğrulama (`M365_API_KEYS` / `M365_API_KEY`)
- Tüm uç noktalarda max_tokens uygulaması (tiktoken BPE)
- Etkileşimli kullanım için CLI arayüzü
- Alt komut yönlendirmeli tek binary

## Kurulum

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge
cd M365Bridge
go mod download
go build -o bin/m365-bridge ./cmd/cli
```

### Hazır Binary'ler

Platformunuz için en son binary'yi [GitHub Releases](https://github.com/KilimcininKorOglu/M365Bridge/releases) sayfasından indirin:

| Platform                    | Dosya                           |
|-----------------------------|---------------------------------|
| Linux amd64                 | `m365-bridge-linux-amd64`       |
| Linux arm64                 | `m365-bridge-linux-arm64`       |
| macOS amd64 (Intel)         | `m365-bridge-darwin-amd64`      |
| macOS arm64 (Apple Silicon) | `m365-bridge-darwin-arm64`      |
| Windows amd64               | `m365-bridge-windows-amd64.exe` |
| Windows arm64               | `m365-bridge-windows-arm64.exe` |

```bash
# Örnek: Linux amd64
wget https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge-linux-amd64
./m365-bridge-linux-amd64 serve --port 8000
```

### Docker

M365Bridge'i çalıştırmanın en kolay yolu Docker'dır. Hazır imaj GitHub Container Registry'de mevcuttur.

#### Adım 1: docker-compose.yml oluşturun

Proje dizininizde bir `docker-compose.yml` dosyası oluşturun:

```yaml
services:
  m365bridge:
    image: ghcr.io/kilimcininkoroglu/m365bridge:latest
    container_name: m365bridge
    ports:
      - "8230:8000"
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

#### Adım 2: Container'ı başlatın

```bash
docker compose up -d
```

API `http://localhost:8230` adresinde erişilebilir olacaktır.

#### Adım 3: Tarayıcıdan kimlik doğrulama token'ını alın

Sunucunun, Microsoft 365 Copilot oturumunuzdan bir refresh token'a ihtiyacı var. Şu şekilde çıkarın:

1. Tarayıcınızda [https://m365.cloud.microsoft](https://m365.cloud.microsoft) adresini açın ve giriş yapın
2. DevTools'u açmak için **F12**'ye basın, **Console** sekmesine geçin
3. Aşağıdaki JavaScript kodunu yapıştırıp çalıştırın:

<details>
<summary>JavaScript interceptor kod parçacığını genişletmek için tıklayın</summary>

```javascript
(() => {
const k = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
if (!k) return 'NOT_FOUND';
const p = k.split('|')[1].split('.');
const oid = p[0], tenant = p[1];

const origFetch = window.fetch;
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url) || '';
  if (url.includes('login.microsoftonline.com') && url.includes('oauth2/v2.0/token')) {
    try {
      const clone = resp.clone();
      const data = await clone.json();
      if (data.refresh_token) {
        console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
        console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
      }
    } catch(e) {}
  }
  return resp;
};

const origXHROpen = XMLHttpRequest.prototype.open;
const origXHRSend = XMLHttpRequest.prototype.send;
XMLHttpRequest.prototype.open = function(method, url) {
  this._url = url;
  return origXHROpen.apply(this, arguments);
};
XMLHttpRequest.prototype.send = function(body) {
  this.addEventListener('load', function() {
    if (this._url && this._url.includes('oauth2/v2.0/token')) {
      try {
        const data = JSON.parse(this.responseText);
        if (data.refresh_token) {
          console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
          console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
        }
      } catch(e) {}
    }
  });
  return origXHRSend.apply(this, arguments);
};

const keys = Object.keys(localStorage);
let cleared = 0;
for (const key of keys) {
  if (key.includes('accesstoken') || key.includes('idtoken')) {
    localStorage.removeItem(key);
    cleared++;
  }
}

window.dispatchEvent(new Event('load'));
if (window.msal) {
  try {
    const accounts = window.msal.getAllAccounts();
    if (accounts.length > 0) {
      window.msal.acquireTokenSilent({
        account: accounts[0],
        scopes: ['https://substrate.office.com/sydney/.default']
      }).catch(() => {});
    }
  } catch(e) {}
}

return 'Interceptors installed and ' + cleared + ' access tokens cleared. MSAL should refresh automatically. Watch the console for the JSON output.';
})()
```

</details>

4. Konsolda şu mesajı bekleyin: `===== COPY THE COMPLETE JSON LINE BELOW =====`
5. JSON çıktısını kopyalayın. Şu formatta olur:

```json
{"oid":"sizin-oid","tenant":"sizin-tenant","refresh_token":"sizin-refresh-token"}
```

#### Adım 4 (Opsiyonel): Otomatik yenileme için SSO cookie'leri alın

Microsoft SPA refresh token'ları **24 saat** sonra süresi dolar. SSO cookie'leri olmadan, 24 saatte bir Adım 3'ü tekrarlamanız gerekir. SSO cookie'leri otomatik yenilemeyi sağlar ve haftalarca/aylarca dayanır.

SSO cookie'lerini yakalamak için:

1. Tarayıcınızda [https://login.microsoftonline.com](https://login.microsoftonline.com) adresini açın (cookie'ler burada bulunur, m365.cloud.microsoft'ta değil)
2. DevTools'u açmak için **F12**'ye basın, **Application** > **Cookies** > `https://login.microsoftonline.com` bölümüne gidin
3. Şu iki cookie'nin değerlerini bulun ve kopyalayın:
   - `ESTSAUTH`
   - `ESTSAUTHPERSISTENT`

#### Adım 5: setup.json oluşturun

Adım 3'teki JSON ile `data/setup.json` dosyası oluşturun. Adım 4'te SSO cookie'leri yakaladıysanız, `sso_cookies` dizisine ekleyin:

**SSO cookie'leri olmadan (24 saatte bir setup tekrar gerekir):**

```json
{"oid":"sizin-oid","tenant":"sizin-tenant","refresh_token":"sizin-refresh-token"}
```

**SSO cookie'leri ile (otomatik yenileme, önerilir):**

```json
{
  "oid": "sizin-oid",
  "tenant": "sizin-tenant",
  "refresh_token": "sizin-refresh-token",
  "sso_cookies": [
    {"name": "ESTSAUTH", "value": "estsauth-degerini-buraya-yapistirin"},
    {"name": "ESTSAUTHPERSISTENT", "value": "estsauthpersistent-degerini-buraya-yapistirin"}
  ]
}
```

#### Adım 6: Kurulum sihirbazını çalıştırın

Kimlik bilgilerinizi şifreleyip kaydetmek için container içinde kurulum sihirbazını çalıştırın:

```bash
docker exec -it m365bridge ./bin/m365-bridge setup-wizard
```

Sihirbaz şunları yapar:
- `data/setup.json` dosyasını okur
- Refresh token ve SSO cookie'lerini AES-256-GCM ile şifreler
- Ortam değişkenlerini `data/.env` dosyasına kaydeder
- Token'ı access token ile değiştirerek doğrular

Başarı durumunda sunucu hazırdır. API `http://localhost:8230` adresinde kullanılabilir.

> **Not:** SSO cookie'leri yakalamadıysanız, refresh token 24 saat sonra süresi dolar ve sunucu çalışmayı durdurur. Yeni token almak için Adım 3, 5 ve 6'yı tekrarlayın. SSO cookie'leri ile sunucu, token süresi dolduğunda otomatik olarak yeniler.

#### Alternatif: docker run

Docker Compose yerine `docker run` kullanmayı tercih ederseniz:

```bash
docker run -d \
  --name m365bridge \
  -p 8230:8000 \
  -v $(pwd)/data:/app/data \
  --restart unless-stopped \
  ghcr.io/kilimcininkoroglu/m365bridge:latest
```

Sonra yukarıdaki Adım 3-6'yı izleyin.

#### Notlar

- `data/` dizini token'ları, önbelleği ve yapılandırmayı saklar. İlk çalıştırmada otomatik oluşturulur.
- Port `8230` (host) ile `8000` (container) arasında eşleştirilir. Host portunu `docker-compose.yml` veya `-p` parametresinden değiştirebilirsiniz.
- Container varsayılan olarak `serve --port 8000` ile başlar.
- Hazır imaj yerine kendiniz derlemek isterseniz: `docker compose up --build -d`

## Kullanım

### CLI Bayrakları

| Bayrak          | Tip    | Varsayılan | Açıklama                                                                       |
|-----------------|--------|------------|--------------------------------------------------------------------------------|
| `-i`            | bool   | false      | Etkileşimli mod (çok turlu sohbet)                                             |
| `--model`       | string | `auto`     | Kullanılacak model: `auto`, `quick`, `reasoning`, `gpt5.5`, `gpt5.5-reasoning` |
| `--reasoning`   | bool   | false      | Akıl yürütme modunu kullan                                                     |
| `--no-stream`   | bool   | false      | Akışı devre dışı bırak, tam yanıtı tek seferde yazdır                          |
| `--list-models` | bool   | false      | Tüm kullanılabilir modelleri listele ve çık                                    |
| `--version`     | bool   | false      | Sürümü göster ve çık                                                           |

Konumsal argüman (hiçbir bayrak tüketmezse): tek sorgu modu için sorgu metni.

### Alt komut: serve

HTTP API sunucusunu başlatır.

| Bayrak      | Tip  | Varsayılan | Açıklama             |
|-------------|------|------------|----------------------|
| `--port`    | int  | 8000       | Dinlenecek port      |
| `--version` | bool | false      | Sürümü göster ve çık |

### Alt komut: setup-wizard

Tarayıcı tabanlı kurulum sihirbazını çalıştırır. `oid`, `tenant` ve `refresh_token` içeren JSON dosyasını okur.

| Bayrak   | Tip    | Varsayılan        | Açıklama                     |
|----------|--------|-------------------|------------------------------|
| `--file` | string | `data/setup.json` | Kurulum JSON dosyasının yolu |

### Örnekler

```bash
# Tek sorgu
./bin/m365-bridge "soru metniniz"

# Etkileşimli mod
./bin/m365-bridge -i

# Akıl yürütme ile model belirtme
./bin/m365-bridge --model gpt5.5-reasoning "soru metniniz"

# Akışsız
./bin/m365-bridge --no-stream "soru metniniz"

# Modelleri listele
./bin/m365-bridge --list-models

# API sunucusunu başlat
./bin/m365-bridge serve --port 8000

# Özel dosya ile kurulum sihirbazını çalıştır
./bin/m365-bridge setup-wizard --file /path/to/setup.json
```

### API Sunucusu

```bash
# 8000 portunda API sunucusunu başlat
./bin/m365-bridge serve --port 8000

# curl ile test (kimlik doğrulamasız)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Merhaba"}]}'

# curl ile test (API anahtarı ile)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{"model":"gpt5.5","messages":[{"role":"user","content":"Merhaba"}]}'

# Oturum izolasyonu ile akış
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -H "X-Session-Id: my-session-1" \
  -d '{"model":"gpt5.5","stream":true,"messages":[{"role":"user","content":"Merhaba"}]}'
```

### İlk Çalıştırma

Sunucuyu ilk kez başlattığınızda:

1. Sunucu geçerli çalışma dizininden `data/.env` dosyasını okur
2. `data/tokens/rt_90day.txt` dosyasından şifrelenmiş refresh token'ı yükler
3. Token yenileme gerçekleştirir (refresh token'ı access token'a değiştirir). Bu 1-2 saniye sürer
4. Başarı durumunda şunu görürsünüz: `Starting API server on port 8000`
5. İlk istek, `substrate.office.com`'a WebSocket bağlantısı açtığı için biraz daha uzun sürebilir

Refresh token eksik veya süresi dolmuşsa, sunucu `data/tokens/sso_cookies.json` dosyası mevcutsa SSO cookie ile yeniden kimlik doğrulamayı dener. SSO cookie'leri de yoksa veya süresi dolmuşsa, sunucu token yenileme hatası ile başlayamaz. Taze token ve cookie çıkarmak için `./bin/m365-bridge setup-wizard` komutunu tekrar çalıştırın.

### Oturum İzolasyonu

Her oturum benzersiz bir M365 sohbetine eşlenir. Oturum ID'si öncelik sırasına göre çözümlenir:

1. İstek gövdesinde `session_id` alanı
2. İstek gövdesinde `user` alanı
3. `X-Session-Id` başlığı
4. `hash(api_key + ilk_kullanıcı_mesajı)` (kimlik doğrulama açıkken) veya `hash(ilk_kullanıcı_mesajı)` (kimlik doğrulama kapalıyken)

Hash yedeği, özel başlık gönderemeyen standart OpenAI istemcilerinin (Claude Code gibi) ilk kullanıcı mesajları farklı olduğu sürece otomatik olarak ayrı sohbetlere sahip olmasını sağlar.

### Python İstemcisi (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # M365_API_KEYS ayarlıysa zorunlu
)
resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{"role": "user", "content": "Merhaba"}]
)
print(resp.choices[0].message.content)
```

### Python İstemcisi (Anthropic SDK)

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # M365_API_KEYS ayarlıysa zorunlu
)
resp = client.messages.create(
    model="gpt5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Merhaba"}]
)
print(resp.content[0].text)
```

### Görsel Girdi Örneği

```python
from openai import OpenAI
import base64

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",
)

with open("image.png", "rb") as f:
    img_b64 = base64.b64encode(f.read()).decode()

resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "Bu görselde ne var?"},
            {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img_b64}"}},
        ],
    }],
)
print(resp.choices[0].message.content)
```

## API Uç Noktaları

| Uç Nokta                    | Açıklama                                          |
|-----------------------------|---------------------------------------------------|
| `POST /v1/chat/completions` | OpenAI Chat Completions (akışlı + akışsız)        |
| `POST /v1/completions`      | OpenAI metin tamamlama (akışlı + akışsız)         |
| `POST /v1/responses`        | OpenAI Responses API (akışlı + akışsız)           |
| `POST /v1/messages`         | Anthropic Messages formatı (özel SSE işleyiciler) |
| `POST /v1/complete`         | Anthropic Complete (FIM)                          |
| `GET /v1/models`            | Model listesi                                     |
| `GET /health`               | Sağlık kontrolü (kimlik doğrulama gerektirmez)    |

## Modeller

Tüm model seçimi, M365 backend'ine gönderilen `tone` alanı ile yapılır. Tüm modeller için `Override` alanı boştur. GPT-5.x modelleri GPT-5 backend'ine; Claude modelleri gerçek Anthropic Claude modellerine yönlendirilir (tone testi ile doğrulanmıştır).

| Anahtar                    | Tone              | OpenAI ID         | Düşünme? | Backend |
|----------------------------|-------------------|-------------------|----------|---------|
| `auto`                     | Magic             | gpt-4-auto        | Hayır    | GPT-5   |
| `quick`                    | Chat              | gpt-4-quick       | Hayır    | GPT-5   |
| `reasoning`                | Magic             | gpt-4-reasoning   | Hayır    | GPT-5   |
| `gpt5.5`                   | Gpt_5_5_Chat      | gpt-5.5           | Hayır    | GPT-5   |
| `gpt5.5-reasoning`         | Gpt_5_5_Reasoning | gpt-5.5-reasoning | Evet     | GPT-5   |
| `claude`                   | Claude_Sonnet     | claude-sonnet-4.6 | Hayır    | Claude  |
| `claude-sonnet`            | Claude_Sonnet     | claude-sonnet-4.6 | Hayır    | Claude  |
| `claude-opus`              | Claude_Opus       | claude-opus-4.6   | Hayır    | Claude  |
| `claude-sonnet-4-20250514` | Claude_Sonnet     | claude-sonnet-4.6 | Hayır    | Claude  |

### Hangi modeli kullanmalıyım?

| Kullanım senaryosu                            | Model              |
|-----------------------------------------------|--------------------|
| Genel amaçlı, backend karar versin            | `auto`             |
| Hızlı yanıtlar, basit sorular                 | `quick`            |
| Karmaşık akıl yürütme, çok adımlı problemler  | `reasoning`        |
| GPT-5.5 sohbet (en son sohbet modeli)         | `gpt5.5`           |
| GPT-5.5 derin düşünme (akıl yürütme gösterir) | `gpt5.5-reasoning` |
| Claude Sonnet 4.6 (Anthropic)                 | `claude-sonnet`    |
| Claude Opus 4.6 (Anthropic, en yetenekli)     | `claude-opus`      |

`gpt5.5-reasoning`, modelin düşünme sürecini içeren `reasoning_content` çıktısı üretir. OpenAI endpoint'leri bunu `reasoning_content` olarak; Anthropic endpoint'leri `text` bloğundan önce bir `thinking` içerik bloğu olarak gösterir. Claude modelleri düşünme içeriği üretmez.

### Model Adında Session ID

Model adında `:` ayırıcısı ile session ID gömebilirsiniz. Bu, özel header gönderemeyen istemciler (Claude Code, Codex gibi) için kullanışlıdır:

```
model: "gpt5.5-reasoning:my-session-001"
```

Bu, `X-Session-Id: my-session-001` header'ı veya istek gövdesinde `session_id: "my-session-001"` ayarlamakla eşdeğerdir. Model anahtarı `:`'den önce, session ID'si sonra çıkarılır.

### Harici Model Adları

Kayıtta olmayan model adları gönderen istemciler (ör. `claude-sonnet-4-20250514`, `gpt-4o`, `o1`) `auto` modeline düşer. Proxy herhangi bir model dizesini kabul eder — bilinmeyen adlar hata vermez, sadece varsayılan modeli kullanır.

## Tool Calling (Araç Çağırma)

M365Bridge **simüle edilmiş tool calling** destekler — istemci tanımlı araçlar (Claude Code'un Read/Bash/Write'i, Codex araçları, vb.) M365 backend'inin bunları doğal olarak desteklemesi olmadan çalışır.

### Nasıl Çalışır?

1. İstemci, `tools` dizisi ile bir istek gönderir (OpenAI function tanımları veya Anthropic tool şemaları)
2. M365Bridge, tüm istek JSON'unu M365 Copilot'a gönderilen prompt'a gömer
3. M365 Copilot, ```` ```json ```` bloğunda tam yanıt JSON'u döndürür
4. M365Bridge, yanıtı ayrıştırır ve tool call'ları OpenAI `tool_calls` veya Anthropic `tool_use` içerik bloklarına çıkarır
5. İstemci aracı çalıştırır ve sonucu bir sonraki mesajda geri gönderir

Bu, hem OpenAI (`/v1/chat/completions`) hem de Anthropic (`/v1/messages`) endpoint'lerinde, hem streaming hem de non-streaming modlarda çalışır.

### Örnek (OpenAI)

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "bash",
        "description": "Run a shell command",
        "parameters": {
          "type": "object",
          "properties": {"command": {"type": "string"}},
          "required": ["command"]
        }
      }
    }],
    "tool_choice": "required"
  }'
```

Yanıt:

```json
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "call_001",
        "type": "function",
        "function": {
          "name": "bash",
          "arguments": "{\"command\": \"echo hello\"}"
        }
      }]
    }
  }]
}
```

### Örnek (Anthropic)

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "name": "bash",
      "description": "Run a shell command",
      "input_schema": {
        "type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"]
      }
    }],
    "tool_choice": {"type": "any"}
  }'
```

Yanıt:

```json
{
  "content": [{
    "type": "tool_use",
    "id": "toolu_001",
    "name": "bash",
    "input": {"command": "echo hello"}
  }],
  "stop_reason": "tool_use"
}
```

### Notlar

- Tool calling her zaman etkindir — yapılandırma gerekmez. `tools` olmayan istekler etkilenmez.
- M365 Copilot kendi sunucu tarafı araçlarını çalıştırdığında (web araması, code interpreter) ve simüle JSON yerine düz metin döndürdüğünde, yanıt normal bir metin tamamlaması olarak `finish_reason: "stop"` ile döndürülür.
- Konuşma geçmişindeki `tool_result` mesajları (OpenAI) ve `tool_use`/`tool_result` içerik blokları (Anthropic), M365 backend'i tool rollerini anlamadığı için M365'ye gönderilmeden önce düz metne dönüştürülür.
- Streaming endpoint'leri, tool call'ları ayrıştırmadan önce tam yanıtı tampona alır (tool call JSON'u birden çok chunk'a yayılabilir).

## Responses API

`/v1/responses` uç noktası, OpenAI Responses API formatını uygular. `input` (string veya tipli öğe dizisi), `instructions`, `max_output_tokens`, `tools` ve konuşma sürekliliği için `previous_response_id` kabul eder.

### Örnek (akışsız)

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5",
    "input": "2+2 kaçtır?",
    "session_id": "my-session"
  }'
```

Yanıt:

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1234567890,
  "status": "completed",
  "model": "gpt-5.5",
  "output": [{
    "id": "msg_...",
    "type": "message",
    "status": "completed",
    "role": "assistant",
    "content": [{"type": "output_text", "text": "2+2 eşittir 4.", "annotations": []}]
  }],
  "output_text": "2+2 eşittir 4.",
  "usage": {"input_tokens": 5, "output_tokens": 8, "total_tokens": 13}
}
```

### Örnek (instructions ve input öğeleri ile)

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "instructions": "Kısa ve öz bir asistansın.",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Rekürsiyonu açıkla"}]}],
    "stream": true
  }'
```

### Streaming Olayları

Streaming uç noktası tipli SSE olayları yayınlar:

| Olay | Açıklama |
|------|----------|
| `response.created` | Response nesnesi oluşturuldu (status: in_progress) |
| `response.in_progress` | Response üretiliyor |
| `response.output_item.added` | Yeni output öğesi eklendi (message, reasoning veya function_call) |
| `response.content_part.added` | İçerik parçası message öğesine eklendi |
| `response.output_text.delta` | Metin deltası |
| `response.output_text.done` | Metin tamamlandı |
| `response.content_part.done` | İçerik parçası tamamlandı |
| `response.output_item.done` | Output öğesi tamamlandı |
| `response.reasoning_summary_text.delta` | Reasoning/düşünme deltası |
| `response.reasoning_summary_text.done` | Reasoning tamamlandı |
| `response.function_call_arguments.delta` | Tool call argüman deltası |
| `response.function_call_arguments.done` | Tool call argümanları tamamlandı |
| `response.completed` | Tam response nesnesi (status: completed) |
| `response.failed` | Hata oluştu (status: failed) |

## Proje Yapısı

```
cmd/cli/main.go          # Tek giriş noktası, alt komut yönlendirici
pkg/
  auth/auth.go           # TokenManager, token yenileme, AES şifreli refresh token depolama
  auth/sso.go            # SSO cookie tabanlı yeniden kimlik doğrulama (24h token süresi için yedek)
  client/client.go       # M365Client, WebSocket (SignalR) iletişimi
  crypto/crypto.go       # Refresh token'lar için AES-256-GCM şifreleme
  models/models.go       # Version, ModelRegistry, Config, LoadConfig, LookupModel
  payload/payload.go     # İstek payload oluşturucuları, URL oluşturucu, locale/timezone yardımcıları
  servers/
    api.go               # HTTP API sunucusu, tüm uç noktalar, max_tokens, token sayımı, oturum izolasyonu
    cli.go               # CLI sunucusu, etkileşimli mod
  setup/wizard.go        # Tarayıcı tabanlı kurulum sihirbazı (JS kod parçacığı, token doğrulama, data/.env kaydı)
go.mod                   # Modül: github.com/KilimcininKorOglu/M365Bridge, Go 1.22
data/                    # Çalışma zamanı verisi (gitignore'lı): tokens/, setup.json, cache/
```

## Bağımlılıklar

| Bağımlılık                      | Amaç                                                                  |
|---------------------------------|-----------------------------------------------------------------------|
| `github.com/google/uuid`        | SID'ler ve istek ID'leri için UUID oluşturma                          |
| `github.com/gorilla/websocket`  | SignalR için WebSocket istemcisi                                      |
| `github.com/pkoukk/tiktoken-go` | Kullanım ve max_tokens uygulaması için BPE token sayımı (cl100k_base) |
| `golang.org/x/net`              | SSO cookie jar için publicsuffix listesi                              |

## Güvenlik

- Refresh token'lar depolamadan önce AES-256-GCM ile şifrelenir
- SSO cookie'ler depolamadan önce AES-256-GCM ile şifrelenir (`data/tokens/sso_cookies.json`)
- Şifreleme anahtarı `data/tokens/encryption.key` dosyasında saklanır
- Access token'lar `data/tokens/token_cache.json` dosyasında önbelleğe alınır (disk'te saklanır, ~1 saat geçerli, 60 saniye buffer ile)
- Arka plan token yenileyici, `serve` modunda her 30 dakikada bir access token'ı proaktif olarak yeniler
- SSO cookie otomatik yenileme, refresh token süresi dolduğunda (24h SPA limiti) sessizce yeniden kimlik doğrular
- Kod veya repoda kimlik bilgisi saklanmaz
- `data/` dizini gitignore'lıdır (token, önbellek, setup.json içerir)
- API anahtarı kimlik doğrulaması, yapılandırıldığında tüm `/v1/*` uç noktalarını korur

## Görsel Girdi Desteği

Proxy, OpenAI ve Anthropic API formatları ile çok modlu görsel girdiyi destekler:

- **OpenAI**: `{"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}` blokları içeren `content` dizisi
- **Anthropic**: `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}` blokları içeren `content` dizisi

Görseller, `POST https://substrate.office.com/m365Copilot/UploadFile` üzerinden M365 backend'ine yüklenir ve WebSocket mesajına `messageAnnotations` olarak eklenir. Desteklenen formatlar: PNG, JPEG, GIF, WebP.

## Uygulanmayan Özellikler

- Dosya yükleme
- Kod yorumlayıcı

## Sorumluluk Reddi

Bu proje yalnızca öğrenim ve araştırma amaçlıdır. Genel ağ iletişim protokollerini araştırır.

Bu projeyi kullanarak şunları onaylarsınız:
- Meşru Microsoft 365 Copilot yetkiniz olduğunu
- Kişisel öğrenim ve araştırma için olduğunu, ticari kullanım olmadığını
- Resmi olmayan arayüzler kullanmanın risklerini anladığınızı
- Tüm sonuçları kabul ettiğinizi

Bu proje şunları yapmaz:
- Şifreleme kırmaz veya kimlik doğrulamayı atlatmaz
- Başkalarının verisine erişmez veya sızdırmaz
- Microsoft servislerine müdahale etmez
- Microsoft Corporation ile hiçbir ilişkisi yoktur

## Lisans

Yalnızca Araştırma
