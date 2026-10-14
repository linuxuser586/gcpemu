//go:build compat

// Command foreigngo probes Foreign credentials (#247) with a Go HTTP
// client, which fetches a token before it checks the universe. It lists
// service accounts through ADC with the IAM endpoint override and prints
// "iam-adc OK <n>" or "iam-adc ERR <error>". The compat suite runs it
// behind the Escape guard, which names the host the token request went to.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

func main() {
	project, endpoint := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := list(ctx, project, endpoint)
	if err != nil {
		fmt.Println("iam-adc ERR", strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	fmt.Println("iam-adc OK", n)
}

func list(ctx context.Context, project, endpoint string) (int, error) {
	s, err := iam.NewService(ctx, option.WithEndpoint(endpoint))
	if err != nil {
		return 0, err
	}
	r, err := s.Projects.ServiceAccounts.List("projects/" + project).Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	return len(r.Accounts), nil
}
