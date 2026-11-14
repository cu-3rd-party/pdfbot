# pdfbot

Телеграм-бот: шлёшь фотки (или картинки файлами), `/makepdf` собирает из них PDF, `/cancel` чистит очередь.
Переписан на Go с [pxc1984/pdfbot](https://github.com/pxc1984/pdfbot), работает только через вебхук.

- Очередь в Postgres (только `file_id` и размер), переживает рестарты; страницы удаляются после отправки PDF,
  при неудачной сборке возвращаются в очередь.
- PDF не лежит ни в памяти, ни на диске: JPEG из ответа телеграма стримится прямо в тело `sendDocument`.
  100 одновременных сборок держат кучу около 30 МБ (`TestHundredBuilds`). PNG/GIF приходится декодировать,
  такие страницы обрабатываются по одной на процесс, до 16 Мп.
- Лимиты телеграма: файл до 20 МБ на скачивание, PDF до 50 МБ, в очереди до 100 страниц.

```sh
TELEGRAM_BOT_TOKEN=... WEBHOOK_URL=https://pdfbot.cu3rd.ru/webhook DATABASE_URL=postgres://... go run .
PDFBOT_TEST_DB=postgres://... go test -race ./...
```

| env | что |
|---|---|
| `TELEGRAM_BOT_TOKEN` | токен от BotFather |
| `WEBHOOK_URL` | публичный https-адрес `/webhook`, ставится через setWebhook при старте |
| `DATABASE_URL` | Postgres, таблица `queue` создаётся сама |
| `HTTP_ADDR` | по умолчанию `:8080`; `/healthz` для проб |
