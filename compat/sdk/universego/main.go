//go:build compat

// Command universego probes the Go Pub/Sub client with no endpoint
// override (#243): it prints "pubsub-no-override OK <topic>" or
// "pubsub-no-override ERR <error>". The compat suite runs it behind the
// Escape guard, which names the host the client dialed.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
)

func main() {
	project := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name, err := getTopic(ctx, project)
	if err != nil {
		fmt.Println("pubsub-no-override ERR", strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	fmt.Println("pubsub-no-override OK", name)
}

func getTopic(ctx context.Context, project string) (string, error) {
	// The probe is about where the client dials, not who it is.
	c, err := pubsub.NewClient(ctx, project, option.WithoutAuthentication())
	if err != nil {
		return "", err
	}
	defer c.Close()
	t, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/go-universe"})
	if err != nil {
		return "", err
	}
	return t.Name, nil
}
