package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// очередь в базе: PDFBOT_TEST_DB=postgres://... go test -run Queue
func TestQueue(t *testing.T) {
	dsn := os.Getenv("PDFBOT_TEST_DB")
	if dsn == "" {
		t.Skip("нет PDFBOT_TEST_DB")
	}
	ctx := context.Background()
	var err error
	if db, err = pgxpool.New(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "drop table if exists queue;"+SCHEMA); err != nil {
		t.Fatal(err)
	}
	count := func(where string) (n int) {
		db.QueryRow(ctx, "select count(*) from queue where chat_id = 7 and "+where).Scan(&n)
		return
	}

	// телеграм тут не нужен: send падает и пишет в лог, очередь от этого не зависит
	API = "http://127.0.0.1:1"
	for i := range MAX_QUEUE + 5 {
		enqueue(ctx, 7, "f", int64(i), "Фото")
	}
	if n := count("true"); n != MAX_QUEUE {
		t.Fatalf("в очереди %d, ждали %d", n, MAX_QUEUE)
	}
	// сборка падает (телеграма нет) - страницы возвращаются в очередь, а не теряются
	makePDF(ctx, 7, "x.pdf")
	if n := count("taken is null"); n != MAX_QUEUE {
		t.Fatalf("после неудачной сборки в очереди %d", n)
	}
	// рестарт возвращает брошенные сборки
	db.Exec(ctx, "update queue set taken = now()")
	db.Exec(ctx, SCHEMA)
	if n := count("taken is null"); n != MAX_QUEUE {
		t.Fatalf("после рестарта свободных %d", n)
	}
	handle(ctx, &update{Message: msg(7, "/cancel")})
	if n := count("true"); n != 0 {
		t.Fatalf("после /cancel осталось %d", n)
	}
}

func msg(chat int64, text string) *message {
	m := &message{Text: text}
	m.Chat.ID = chat
	return m
}
