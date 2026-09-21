package main

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestGroqDiag(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	model := os.Getenv("GROQ_MODEL")

	fmt.Printf("MODEL=%s\n", model)
	fmt.Printf("KEY_LENGTH=%d\n", len(key))

	client := NewGroqClient(key, model, 100)

	usage, err := client.Stream(
		context.Background(),
		ChatRequest{
			Message: "Say hello",
			Mode:    "fast",
		},
		func(piece string) error {
			fmt.Printf("CONTENT=%q\n", piece)
			return nil
		},
	)

	fmt.Printf("USAGE=%+v\n", usage)
	fmt.Printf("ERROR=%v\n", err)

	if err != nil {
		t.Fatal(err)
	}
}
