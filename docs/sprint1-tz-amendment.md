# Sprint 1 — ТЗ: Поправки по результатам реверс-инжиниринга

> **Дата анализа:** 2026-05-19 (ревизия v3 — live curl подтверждение + закрытые вопросы)
> **Источники:**
> - `ljm.adba296eae66f486.js` (762 KB, lib-jitsi-meet форк Kontur)
> - DevTools — вкладки **Headers / Response / Preview** запроса `GET /api/rooms/cb140blkff7i` (200 OK)
> - DevTools — вкладка **Cookies** (`ngtoken`, `kontur_ngtoken`)
> - DevTools — запрос `POST /api/metrics/connection` (200 OK, повторяется раз в ~60 с)
> - **curl к живой комнате `https://ilte0310.ktalk.ru/cb140blkff7i`** (2026-05-19)
> - upstream `jitsi/lib-jitsi-meet` (для сравнения)

---

## 1. REST API: реальный endpoint и формат

### 1.1 Исправление endpoint'а

| Параметр | Было в ТЗ | Исправление |
|---|---|---|
| URL | `GET /api/<room-id>` | `GET /api/rooms/<room-id>` |
| Auth header | Bearer JWT | `Session <token>` |
| Пример | — | `GET https://ilte0310.ktalk.ru/api/rooms/cb140blkff7i` |

**Аргументация:** DevTools-скриншот однозначно показывает `:path: /api/rooms/cb140blkff7i` и `Authorization: Session 6JFg3aXfMjwNmwKbN1kM`.

### 1.2 Двойной вызов — ServiceWorker PWA pattern

Запрос `GET /api/rooms/<room-id>` приходит **дважды** при открытии комнаты:
- Первый — из ServiceWorker (SW intercept, может вернуть кэшированный ответ).
- Второй — прямой fetch из основного потока.

**Implikации для headless-клиента:**
- SW при реконнекте может отдать устаревший ответ с кэшированным wsUrl.
- Наш Go-клиент не использует SW, поэтому всегда получает свежий ответ — это **лучше**, чем браузерный клиент.
- При отладке через браузер: если видите два одинаковых запроса — это нормально.

### 1.3 Ожидаемые поля JSON-ответа `/api/rooms/<room-id>`

JS-код использует следующие поля из ответа rooms API:

```jsonc
{
  // Поля, подтверждённые в JS (по использованию в коде):
  "wsUrl": "...",          // WebSocket URL (fallback, если не собирается из шаблона)
  "token": "...",          // Session token для Authorization header
  "roomName": "...",       // Имя комнаты (отображается в UI)
  "owner": "...",          // Владелец комнаты (JID или username)
  "sessionId": "...",      // ID сессии (используется в Jicofo IQ)

  // Поля, видимые в Baggage заголовке DevTools (Sentry telemetry):
  // sentry-transaction=%2F%3Aroom%2F  → роут /room/:roomId
  // tariffType=free                   → тариф владельца
  // sentry-release=talk-web@master-50ba49a8
}
```

**⚠️ Важно:** Полная структура ответа `/api/rooms/<room-id>` не видна в JS-бандле —  
она обрабатывается в другом webpack chunk (rooms API client). Это значит:

> **TODO (команда):** Открыть DevTools → Network → cb140blkff7i → вкладка **Response**  
> и зафиксировать полный JSON. Предположительно там: `{wsUrl, roomName, hosts, token, ...}`.

### 1.4 Конфигурация Jitsi из JS (URL-шаблон)

Из JS однозначно извлечена функция сборки конфигурации:

```javascript
// Переменные: v = domain, i = subdomain, s = roomName
{
  conferenceRequestUrl: `https://${v}/${i}/conference-request/v1?room=${s}`,
  hosts: {
    domain: v,                          // например: ilte0310.ktalk.ru
    muc: `conference.${i}.${v}`         // например: conference.cb140blkff7i.ilte0310.ktalk.ru
  },
  serviceUrl: `wss://${v}/${i}/xmpp-websocket?room=${s}`,
  websocketKeepAliveUrl: `https://${v}/${i}/_unlock?room=${s}`
}
```

**Вывод для имплементации:**
- WebSocket: `wss://<subdomain>.ktalk.ru/<room-name-prefix>/xmpp-websocket?room=<roomName>`
- MUC: `conference.<room-prefix>.<subdomain>.ktalk.ru`
- `_unlock` — keepalive endpoint, опрашивается каждые **~60 секунд** (см. п.3).

---

## 2. XMPP/SASL: подтверждённые механизмы и расширения

### 2.1 SASL-механизмы: приоритеты

| Механизм | Приоритет | Условие активации |
|---|---|---|
| `SCRAM-SHA-1` | 60 | Всегда |
| `PLAIN` | 50 | Всегда (клиент первый) |
| `OAUTHBEARER` | 40 | OAuth Bearer token |
| `X-OAUTH2` | 30 | `pass != null` (см. ниже) |
| `ANONYMOUS` | 20 | — (fallback) |
| `EXTERNAL` | 10 | — |

**X-OAUTH2 vs ANONYMOUS:**  
- `X-OAUTH2.test(u) → u.pass !== null` — используется только если передан пароль.  
- `ANONYMOUS.test()` — всегда `true` (fallback).  
- Сервер предлагает механизмы через `<mechanisms>` в stream features.  
- Для анонимного входа сервер предложит только `ANONYMOUS` → клиент выберет его.  
- `X-OAUTH2` — для **аутентифицированных** пользователей (владельцы комнат, модераторы).

**Implikации для реализации:**
```go
// Наш XMPP-клиент: только ANONYMOUS для headless-входа
// Не нужно реализовывать X-OAUTH2 пока не нужен модераторский доступ
saslMechanism = "ANONYMOUS"
```

### 2.2 jabber:iq:profile — это НЕ Kontur custom

Пространство имён `jabber:iq:profile` определено в **Strophe.js v1.5.0** (стандартная namespace map):

```javascript
// Из кода Strophe — стандартный NS block:
D.NS = {
  PROFILE: "jabber:iq:profile",   // ← часть Strophe.NS, не Kontur
  ROSTER:  "jabber:iq:roster",
  AUTH:    "jabber:iq:auth",
  // ...
}
```

Strophe версия в бандле: `VERSION: "1.5.0"` — это стандартная библиотека.

**Вывод:** `jabber:iq:profile` **не является Kontur-кастомным расширением**. Это стандартный Strophe namespace, унаследованный от jitsi/lib-jitsi-meet. Наш клиент может его **игнорировать** — сервер не требует profile IQ для анонимного входа.

### 2.3 Подтверждённые XEP для реализации

| XEP | Namespace | Статус |
|---|---|---|
| XEP-0166 Jingle | `urn:xmpp:jingle:1` | ✅ Реализован |
| XEP-0167 Jingle RTP | `urn:xmpp:jingle:apps:rtp:1` | ✅ Реализован |
| XEP-0176 ICE-UDP | `urn:xmpp:jingle:transports:ice-udp:1` | ✅ Реализован |
| XEP-0198 Stream Management | `urn:xmpp:sm:3` | ✅ |
| XEP-0215 ExtDisco | `urn:xmpp:extdisco:1` + `:2` | ✅ |
| XEP-0199 Ping | `urn:xmpp:ping` | ✅ |
| XEP-0320 DTLS | `urn:xmpp:jingle:apps:dtls:0` | ✅ |
| MUC | `http://jabber.org/protocol/muc` | ✅ |
| Entity Caps | `http://jabber.org/protocol/caps` | ✅ |
| **XEP-0444 Reactions** | `urn:xmpp:reactions:0` | ⚠️ Chat-only |

**Jitsi-specific (нужны для caps/disco):**

```
http://jitsi.org/jitmeet                      — base namespace
http://jitsi.org/protocol/focus               — Jicofo IQ
http://jitsi.org/visitors-1                   — режим наблюдателя
http://jitsi.org/ssrc-rewriting-1             — SSRC rewriting (bridge)
http://jitsi.org/json-encoded-sources         — JSON source descriptions
http://jitsi.org/source-name                  — per-source naming
http://jitsi.org/jitmeet/start-muted          — start-muted feature
http://jitsi.org/start-muted-room-metadata    — room metadata
http://jitsi.org/protocols/colibri            — Colibri bridge
http://jitsi.org/protocol/jibri               — recording
http://jitsi.org/protocol/jigasi              — SIP gateway
http://jabber.org/protocol/nick               — nickname
```

### 2.4 Conference Request: HTTP vs XMPP mode

Jicofo (Focus) можно достичь двумя способами — JS выбирает по наличию `conferenceRequestUrl`:

```javascript
if ("xmpp" === this.mode) {
  // Отправляем IQ напрямую по XMPP (классический Jicofo путь)
  this.connection.sendIQ(this._createConferenceIq(roomJid), ...)
} else {
  // HTTP POST на /conference-request/v1?room=<name>
  // Authorization: Bearer <token>
  fetch(this.targetUrl, { body: JSON.stringify(request), ... })
}
```

**Для нашего клиента:** используем XMPP-режим (прямой IQ к focus@<subdomain>.ktalk.ru).

---

## 2.5 Anonymous Join Flow — headless (подтверждено curl 2026-05-19)

> **Статус:** ✅ ПОЛНОСТЬЮ ПОДТВЕРЖДЕНО И РЕАЛИЗОВАНО

### Схема anonymous join (headless, без браузера)

```
1. GET https://ilte0310.ktalk.ru/api/rooms/cb140blkff7i
   Request: User-Agent: ktalk-core/0.1
   
   Response: 200 OK
   Set-Cookie: ngtoken=WakQAWoM28svHVkmCOfwAg==; Domain=.ktalk.ru; HttpOnly; SameSite=None
   Set-Cookie: kontur_ngtoken=...; Domain=ilte0310.ktalk.ru; SameSite=Strict
   Body: {"roomName":"cb140blkff7i","conferenceId":"cb140blkff7i_3074b...","allowAnonymous":true,...}

2. WebSocket upgrade (куки автоматически из jar):
   GET wss://ilte0310.ktalk.ru/cb140blkff7i/xmpp-websocket?room=cb140blkff7i_3074b...
   Cookie: ngtoken=WakQAWoM28svHVkmCOfwAg==; kontur_ngtoken=...
   Sec-WebSocket-Protocol: xmpp
   
3. Keepalive #1 (каждые 55 с):
   POST https://ilte0310.ktalk.ru/api/metrics/connection
   Cookie: ngtoken=...; kontur_ngtoken=...
   Body: {"connectionType":"datachannel"}
   → 200 OK

4. Keepalive #2 (каждые 55 с):
   GET https://ilte0310.ktalk.ru/cb140blkff7i/_unlock?room=cb140blkff7i_3074b...
   Cookie: ngtoken=...; kontur_ngtoken=...
   → 200 OK, Content-Type: text/html (3701 байт)
   → x-jitsi-shard: НЕ ВОЗВРАЩАЕТСЯ в этой инсталляции
```

### Ключевые выводы из curl

| Вопрос | Ответ |
|---|---|
| Нужна ли регистрация для anonymous join? | **Нет** — GET `/api/rooms/` без авторизации |
| Откуда берётся ngtoken? | **Set-Cookie на GET `/api/rooms/`** — автоматически |
| Принимает ли metrics endpoint `{}`? | **Да** — 200 OK без тела |
| Есть ли `x-jitsi-shard` в `_unlock`? | **Нет** — только HTML, одношардовая инсталляция |
| Лимит времени free-тарифа? | **Нет** — работает 24/7, 3+ месяца без проблем |
| TTL комнаты? | **Нет** — комната постоянная |

### Реализация в Go (carrier.go)

```go
// prepareRoom() автоматически вызывается из Connect() перед созданием XMPP-клиента:
func (c *Carrier) prepareRoom(ctx context.Context) (config.RoomConfig, http.CookieJar, error) {
    if room.ConferenceID != "" {
        return room, nil, nil // уже известен — пропускаем fetch
    }
    raClient, _ := roomapi.NewClient(room.HTTPBase())
    info, err := raClient.FetchRoom(ctx, room.RoomID)
    // После этого raClient.CookieJar() содержит ngtoken + kontur_ngtoken
    room.ConferenceID = info.ConferenceID
    return room, raClient.CookieJar(), nil
}
```

---

## 3. Keepalive — два раздельных механизма

> **Обновлено:** подтверждено DevTools 2026-05-19. Исходное ТЗ описывало только `_unlock`.
> Реальная система использует **два keepalive канала** с разными целями.

### 3.1 `/api/metrics/connection` — основной keepalive (раз в ~60 с)

> **Статус:** ✅ ПОДТВЕРЖДЁН — DevTools Network, множественные записи `connection` с интервалом ~60с

```
POST https://ilte0310.ktalk.ru/api/metrics/connection
Content-Type: application/json
Content-Length: 1871
Cookie: ngtoken=WakQAWIjw6YXKmMVBCr3Ag==; kontur_ngtoken=LHOEWIOi22+X7UWAwM3Ag==
→ 200 OK, Content-Length: 0
```

**Что это делает:**
- Отправляет телеметрию/метрики соединения на бэкенд (1871 байт JSON — статистика WebRTC, ICE, битрейты)
- Служит keepalive для серверной сессии (бэкенд знает, что клиент ещё активен)
- Периодичность: строго ~60 секунд (видно в DevTools — строки `connection` повторяются каждую минуту)
- Одна неудачная отправка не критична, но несколько подряд → сервер может инвалидировать сессию

**Для Go-клиента (минимальная реализация):**
```go
// Отправляем пустую или минимальную метрику раз в 60 с
func metricsKeepalive(ctx context.Context, client *http.Client, baseURL, roomName string) {
    ticker := time.NewTicker(55 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            payload := buildConnectionMetrics(roomName) // JSON, ~1-2 KB
            req, _ := http.NewRequestWithContext(ctx, "POST",
                fmt.Sprintf("%s/api/metrics/connection", baseURL),
                bytes.NewReader(payload))
            req.Header.Set("Content-Type", "application/json")
            resp, err := client.Do(req) // куки уже в jar
            if err == nil {
                resp.Body.Close()
            }
        case <-ctx.Done():
            return
        }
    }
}
```

> **TODO:** Перехватить реальный payload `/api/metrics/connection` (1871 байт) из DevTools → Payload tab  
> чтобы точно знать структуру метрик. Минимальный вариант — пустой JSON `{}` или  
> `{"roomName": "...", "connectionType": "datachannel"}` — достаточен для keepalive.

### 3.2 `_unlock` — shard detection (раз в ~60 с, параллельно с 3.1)

JS автоматически опрашивает `websocketKeepAliveUrl` для **обнаружения смены шарда**:

```javascript
// Из кода:
websocketKeepAlive: void 0 === i ? 6e4 : Number(i)  // default = 60000ms = 60s
// + random jitter: interval + 60 * Math.random() * 1000
```

**Что делает `_unlock`:**
```
GET https://<domain>/<roomName>/_unlock?room=<conferenceId>
→ Ответ: заголовок x-jitsi-shard
```
JS проверяет, не изменился ли shard (геобалансировка). Если shard изменился → `CONN_SHARD_CHANGED` → переподключение.

**Назначение:**
- Shard detection (geo failover) — первичная цель
- Вторичный keepalive WebSocket соединения
- НЕ является заменой `/api/metrics/connection`

```go
// В watchdog / shard-detection goroutine:
func shardWatcher(ctx context.Context, client *http.Client, unlockURL string, currentShard *string, onShardChange func()) {
    ticker := time.NewTicker(55 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            req, _ := http.NewRequestWithContext(ctx, "GET", unlockURL, nil)
            resp, err := client.Do(req)
            if err != nil {
                continue
            }
            resp.Body.Close()
            shard := resp.Header.Get("x-jitsi-shard")
            if shard != "" && shard != *currentShard {
                *currentShard = shard
                onShardChange() // trigger reconnect
            }
        case <-ctx.Done():
            return
        }
    }
}
```

**Итоговая картина keepalive для Go-клиента:**

| Горутина | Endpoint | Интервал | Цель |
|---|---|---|---|
| `metricsKeepalive` | `POST /api/metrics/connection` | 55 с | Серверная сессия |
| `shardWatcher` | `GET /<roomName>/_unlock?room=<conferenceId>` | 55 с | Shard detection / WS keepalive |
| XMPP Ping | `urn:xmpp:ping` (IQ) | 30 с | XMPP stream keepalive |

### 3.3 ICE restart логика (из JS)

JS поддерживает **3 попытки ICE restart** с экспоненциальным backoff перед полным reloadом:

```javascript
this._iceRestarts < 3 → setTimeout(restart, 2^n * 1000)
this._iceRestarts >= 3 → CONFERENCE_FAILED → full reload
```

Наша реализация (5 попыток, 2s backoff) — **более агрессивная, чем браузерный клиент**. Можно оставить как есть.

---

## 4. Лимиты бесплатного тарифа (tariffType=free)

### 4.1 Что найдено в JS

**В lib-jitsi-meet (ljm.adba296eae66f486.js) НЕТ:**
- Таймера на 40 минут.
- Кода с числами `2400` (40 мин × 60 с) или `40*60`.
- Поля `tariffType` / `callLimit` / `freeMinutes`.
- Обработки события "звонок истекает".

**Это означает:** лимит длительности (если он есть) **реализован на стороне сервера** (Jicofo или бэкенд), а не в JS. Клиент не знает о лимите заранее — получает `session-terminate` или `conference.kicked`/`conference.focusLeft` когда сервер решает завершить.

### 4.2 Найденные события завершения конференции (server-initiated)

| Событие | Причина | Триггер |
|---|---|---|
| `conference.focusLeft` | Jicofo отключился | Сервер завершил/перезапустил Jicofo |
| `conference.kicked` | Явный kick | Модератор или сервер |
| `conference.destroyed` | Комната уничтожена | Все участники вышли или принудительно |
| `conference.max_users` | Лимит участников | tariff restriction (server-side) |
| `conference.connectionError.accessDenied` | Запрет доступа | Смена политики комнаты |
| `conference.gracefulShutdown` | Плановое обслуживание | Джовидный рестарт сервера |
| `xmpp.room_max_users_error` | Лимит MUC | tariff restriction |

**Ключевой сценарий** — если на free-тарифе есть лимит времени, сервер пришлёт:
```xml
<iq type="set">
  <jingle action="session-terminate">
    <reason><expired/></reason>  <!-- или <gone/> или обычный <success/> -->
  </jingle>
</iq>
```
или Jicofo уйдёт из MUC → `conference.focusLeft`.

### 4.3 Обязательная эмпирическая проверка

> **🚨 ACTION ITEM (критично перед написанием watchdog):**
>
> Запустить тестовый звонок через ktalk.ru с `tariffType=free` и засечь время до принудительного завершения.
>
> **Алгоритм проверки:**
> 1. Открыть DevTools → Console
> 2. Подключиться к room через JS SDK (`window.JitsiMeetJS`)
> 3. `conference.on('conference.focusLeft', () => console.log('FOCUS LEFT', Date.now()))`
> 4. `conference.on('conference.kicked', () => console.log('KICKED', Date.now()))`
> 5. Засечь время от join до terminate-события
>
> **Ожидаемые результаты:**
> - Нет лимита → конференция работает >2 часов (unlikely для free)
> - Лимит 40 мин → `focusLeft` в ~2400 с после старта → **нужен watchdog с реконнектом каждые 38 мин**
> - Лимит 60 мин → аналогично, реконнект каждые 58 мин
> - Лимит по участникам (напр. 5 чел) → `conference.max_users` при попытке 6-го входа

### 4.4 Рекомендация по watchdog (до получения результатов проверки)

```go
// Консервативный watchdog: реконнект каждые 35 минут
// (покрывает Zoom Free 40 мин + Google Meet Free 60 мин)
const TunnelSessionMaxAge = 35 * time.Minute

// В supervisor:
// - запускаем keepalive каждые 55 секунд (_unlock)  
// - при conference.focusLeft или conference.kicked → немедленный реконнект
// - превентивный реконнект каждые 35 минут (на случай если сервер не уведомляет)
```

---

## 5. Anonymous Room Access TTL

### 5.1 enableAnonymousRoomAccessExpiresReminder

Флаг `enableAnonymousRoomAccessExpiresReminder: true` виден в Sentry Baggage DevTools-запроса, но **НЕ найден в ljm.adba296eae66f486.js**.

**Вывод:** флаг обрабатывается в **другом бандле** (основное приложение ktalk.ru, не lib-jitsi-meet). Это пользовательский UI-reminder, показываемый за N минут до истечения сессии.

### 5.2 Что это означает

1. **Anonymous room access имеет TTL** — подтверждается наличием "expires reminder".
2. TTL установлен **на стороне сервера** и связан с cookie-токеном (`ngtoken`, expires 2027-02-15).
3. `ngtoken` живёт ~9 месяцев (выдан при первом входе, expires 2027-02-15 по DevTools) — это **долгосрочный** токен.
4. `kontur_ngtoken` — сессионный (expires при закрытии браузера), привязан к конкретному шарду.
5. После истечения токена сервер может:
   - Вернуть `401 Unauthorized` на `/api/rooms/<room-id>`
   - Закрыть XMPP-сессию с `<not-authorized/>`
   - Отозвать MUC membership

### 5.3 Риск для нашей модели "создал один раз"

| Сценарий | Вероятность | Mitigation |
|---|---|---|
| TTL session token истекает за N часов | **Высокая** | Переполучать токен при 401 |
| Комната сама по себе имеет TTL (требует renewal владельцем) | **Средняя** | Мониторить XMPP `<gone/>` |
| Анонимный гость не может зайти после X минут | **Низкая** (если join prematurely) | Проверить эмпирически |

### 5.4 Рекомендация по имплементации

```go
// В xmpp/client.go — обработка 401 при GET /api/rooms/
// и reconnect с re-auth

func (c *Client) Connect(ctx context.Context) error {
    room, err := c.fetchRoom(ctx)  // GET /api/rooms/<id>
    if errors.Is(err, ErrUnauthorized) {
        // session token истёк — нужно как-то получить новый
        // (либо через refresh, либо через повторный anonymous JOIN)
        return ErrSessionExpired
    }
    // ...
}
```

---

## 6. Shard Detection и Geo-балансировка

### 6.1 Механизм

```javascript
// При keepalive _unlock:
const shard = response.headers.get("x-jitsi-shard")
if (shard !== currentShard) {
    this.eventEmitter.emit(Events.CONN_SHARD_CHANGED)
    // → full reconnect
}
```

### 6.2 Implikации

- Kontur использует несколько shard'ов (инфраструктура: `ktalk.host`, `kontur.host`).
- При failover (перебалансировке) наш клиент получит новый shard.
- Наш клиент **должен обрабатывать смену shard** как сигнал к реконнекту.
- Подтверждённые поддомены из DevTools: `ilte0310.ktalk.ru` (конкретная нода).

---

## 7. Frontend версионирование

```
sentry-release=talk-web@master-50ba49a8
```

- Continuous deployment от `master` (без релизных тегов).
- Fingerprint'ы (User-Agent, capabilities) нужно обновлять при смене версии.
- Рекомендация: проверять раз в месяц на новые capabilities/namespace'ы.

---

## 8. Итог: что изменилось в ТЗ Sprint 1

> **Ревизия v2** — обновлено 2026-05-19 по результатам анализа DevTools (Response + Cookies + Network)

### Обязательные изменения (breaking):

1. **REST endpoint:** `GET /api/rooms/<room-id>` (не `/api/<room-id>`)  
   — статус: ✅ было исправлено в v1 ТЗ

2. **Аутентификация через cookie, не заголовок:**  
   — `ngtoken` cookie (domain `.ktalk.ru`, expires 2027-02-15, HttpOnly, SameSite=None)  
   — `kontur_ngtoken` cookie (domain `<shard>.ktalk.ru`, Session, SameSite=Strict)  
   — Go-клиент: `http.Client{Jar: cookiejar.New(nil)}`  
   — Заголовок `Authorization: Session ...` из первых скриншотов — это Sentry session ID, **не auth token**

3. **WebSocket URL:** `wss://<domain>/<roomName>/xmpp-websocket?room=<conferenceId>`  
   — **`room=` = поле `conferenceId` из JSON-ответа**, НЕ `roomName`  
   — Пример: `?room=cb140blkff7i_3074b65d29905f8e4418e2113a329f487fcadc8e4ed58df7b108624d199a4110`

4. **MUC JID:** `conference.<roomName>.<domain>` (не изменилось)

5. **Keepalive #1 (основной):** `POST /api/metrics/connection` каждые ~60 с  
   — JSON body ~1871 байт (метрики соединения)  
   — без этого сервер может инвалидировать сессию

6. **Keepalive #2 (shard):** `GET /<roomName>/_unlock?room=<conferenceId>` каждые ~60 с  
   — для обнаружения смены шарда (`x-jitsi-shard` header)

### Поля, которых НЕТ в JSON-ответе `/api/rooms/` (вопреки ТЗ v1):

| Поле | Статус | Реальный источник |
|---|---|---|
| `wsUrl` | ❌ Отсутствует | URL строится из шаблона + `conferenceId` |
| `token` | ❌ Отсутствует | Берётся из cookie `ngtoken` |
| `owner` | ❌ Отсутствует | Нет в ответе |
| `sessionId` | ❌ Отсутствует | Нет в ответе |

### Поля, которые ЕСТЬ в ответе (подтверждены):

| Поле | Значение | Использование |
|---|---|---|
| `roomName` | `"cb140blkff7i"` | path в WebSocket URL |
| `conferenceId` | `"cb140blkff7i_3074b..."` | `room=` param в WebSocket URL |
| `allowAnonymous` | `true` | Подтверждает SASL ANONYMOUS |
| `audioPolicy` | `"none"` | Нет медиа-ограничений |
| `videoPolicy` | `"none"` | Нет медиа-ограничений |
| `usersOnline` | `1` | Информационное |
| `onlineUsers[]` | `[{anonymousName, anonymousId, isAnonymous}]` | Список участников |

### Новые требования к watchdog:

7. **Metrics keepalive:** `POST /api/metrics/connection` каждые 55 с
8. **Shard monitoring:** читать `x-jitsi-shard` из `_unlock`, реконнект при смене
9. **Cookie refresh:** при 401 на `/api/rooms/` — повторный anonymous join для получения новых кук
10. **Превентивный реконнект:** каждые 35 минут (до эмпирической проверки лимита free-тарифа)
11. **Focus events watchdog:** `conference.focusLeft` → немедленный реконнект (не через 2s backoff)

### Не требует изменений:

- SASL ANONYMOUS — подтверждён (`allowAnonymous: true` в ответе)
- XEP набор — полностью совпадает с ожидаемым
- Jingle state machine — соответствует JS реализации
- ICE restart (3 попытки в JS, наши 5 — допустимо)
- `jabber:iq:profile` — НЕ Kontur custom, наш клиент может игнорировать
- `audioPolicy/videoPolicy = "none"` — никаких медиа-ограничений для DataChannel-туннеля

### Закрытые вопросы (подтверждено empirically 2026-05-19):

- [x] ~~**Процедура anonymous join headless**~~ — **ЗАКРЫТО**: `GET /api/rooms/<roomName>` выдаёт `ngtoken` cookie напрямую через `Set-Cookie`, без браузерной сессии. Реализовано в `roomapi.Client.FetchRoom()`.
- [x] ~~**Payload `/api/metrics/connection`**~~ — **ЗАКРЫТО**: сервер принимает `{}` (200 OK). Минимальный JSON `{"connectionType":"datachannel"}` достаточен для keepalive.
- [x] ~~**Лимит длительности звонка**~~ — **ЗАКРЫТО**: free-тариф = **нет лимита по времени**. Комната работает 24/7, звонок не прерывается. Подтверждено пользователем (3+ месяца одна и та же комната).
- [x] ~~**TTL комнаты**~~ — **ЗАКРЫТО**: комната **постоянная**, не требует renewal. Доступна по ссылке всегда, независимо от того есть ли участники.
- [x] ~~**Полная структура JSON** ответа `/api/rooms/<room-id>`~~ — **ЗАКРЫТО** (см. п. 1.3)
- [x] ~~**TTL `ngtoken`**~~ — **ЗАКРЫТО**: expires 2027-02-15 (~9 месяцев), долгосрочный.
- [x] ~~**`x-jitsi-shard` в `_unlock`**~~ — **ЗАКРЫТО**: в данной инсталляции `_unlock` **не возвращает** `x-jitsi-shard`. Ответ: `200 HTML 3701 байт`. Shard detection не нужен — инсталляция одношардовая.

### Оставшиеся открытые вопросы (низкий приоритет):

- [ ] **Полный payload `/api/metrics/connection`** (1871 байт из браузера) — нужен DevTools → Payload вкладка для документации. На функциональность не влияет (сервер принимает `{}`).
- [ ] **Процедура refresh `ngtoken` при истечении** (2027-02-15) — нужен ли повторный GET `/api/rooms/` или отдельный refresh endpoint?

---

## Приложение A: Подтверждённые namespace'ы (полный список)

```
# Стандартные XMPP/Jingle XEP
urn:xmpp:jingle:1
urn:xmpp:jingle:apps:rtp:1
urn:xmpp:jingle:apps:rtp:audio
urn:xmpp:jingle:apps:rtp:video
urn:xmpp:jingle:apps:dtls:0
urn:xmpp:jingle:transports:ice-udp:1
urn:xmpp:jingle:transports:dtls-sctp:1
urn:xmpp:sm:3
urn:xmpp:extdisco:1
urn:xmpp:extdisco:2
urn:xmpp:ping
urn:xmpp:reactions:0

# XMPP core
jabber:client
jabber:iq:auth
jabber:iq:roster
jabber:iq:profile       ← стандартный Strophe NS, не Kontur custom
urn:ietf:params:xml:ns:xmpp-sasl
urn:ietf:params:xml:ns:xmpp-bind
urn:ietf:params:xml:ns:xmpp-session
urn:ietf:params:xml:ns:xmpp-stanzas
urn:ietf:params:xml:ns:xmpp-framing

# MUC + discovery
http://jabber.org/protocol/muc
http://jabber.org/protocol/muc#user
http://jabber.org/protocol/disco#info
http://jabber.org/protocol/disco#items
http://jabber.org/protocol/caps
http://jabber.org/protocol/nick

# Jitsi-specific (нужны в Entity Capabilities)
http://jitsi.org/jitmeet
http://jitsi.org/protocol/focus
http://jitsi.org/visitors-1
http://jitsi.org/ssrc-rewriting-1
http://jitsi.org/json-encoded-sources
http://jitsi.org/source-name
http://jitsi.org/jitmeet/start-muted
http://jitsi.org/start-muted-room-metadata
http://jitsi.org/protocols/colibri
http://jitsi.org/protocol/jibri
http://jitsi.org/protocol/jigasi
http://jitsi.org/protocol/transcriber
```

## Приложение B: SASL приоритеты (Strophe.js v1.5.0)

| Механизм | Priority | Условие |
|---|---|---|
| SCRAM-SHA-1 | 60 | Всегда |
| PLAIN | 50 | Client-first |
| OAUTHBEARER | 40 | OAuth2 token |
| X-OAUTH2 | 30 | pass != null |
| **ANONYMOUS** | **20** | **Наш случай (headless)** |
| EXTERNAL | 10 | TLS client cert |

> Сервер предлагает механизмы через stream features. Для анонимных гостей сервер  
> предложит только `ANONYMOUS`. Клиент выберет наибольший приоритет из пересечения.
