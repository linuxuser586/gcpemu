// Command gcsprobe is a Build step's Go client: it prints the account the
// metadata server answers as, then reads the Cloud Storage object
// gs://BUCKET/OBJECT (its arguments) with Application Default Credentials
// and no Endpoint override, printing it or the error.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/storage"
)

func main() {
	ctx := context.Background()
	email, err := metadata.EmailWithContext(ctx, "default")
	if err != nil {
		fmt.Println("metadata:", err)
		os.Exit(1)
	}
	fmt.Println("account:", email)
	c, err := storage.NewClient(ctx)
	if err != nil {
		fmt.Println("client:", err)
		os.Exit(1)
	}
	r, err := c.Bucket(os.Args[1]).Object(os.Args[2]).NewReader(ctx)
	if err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		fmt.Println("read:", err)
		os.Exit(1)
	}
	fmt.Printf("object: %s\n", b)
}
