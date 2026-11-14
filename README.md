# pdfbot

Телеграм-бот: шлёшь фотки (или картинки файлами), `/makepdf` собирает из них PDF, `/cancel` чистит очередь.
Переписан на Go с [pxc1984/pdfbot](https://github.com/pxc1984/pdfbot): без базы и зависимостей, только вебхук.

Очередь живёт в памяти (только `file_id`), после рестарта теряется.

```sh
TELEGRAM_BOT_TOKEN=... WEBHOOK_URL=https://pdfbot.cu3rd.ru/webhook go run .
```

| env | что |
|---|---|
| `TELEGRAM_BOT_TOKEN` | токен от BotFather |
| `WEBHOOK_URL` | публичный https-адрес `/webhook`, ставится через setWebhook при старте |
| `HTTP_ADDR` | по умолчанию `:8080`; `/healthz` для проб |
