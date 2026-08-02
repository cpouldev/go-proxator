// Command basic sends one request through a configured proxy endpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cpouldev/go-proxator"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	const poolName = "main"

	proxyURL := os.Getenv("PROXY_URL")
	if proxyURL == "" {
		return errors.New("set PROXY_URL to a complete proxy URL")
	}
	targetURL := os.Getenv("TARGET_URL")
	if targetURL == "" {
		targetURL = "https://example.com"
	}

	client, err := proxator.New(proxator.Config{
		Pools: []proxator.PoolConfig{{
			Name:            poolName,
			Endpoints:       []string{proxyURL},
			SessionPoolSize: 1,
		}},
	})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	response, err := client.Get(ctx, poolName, targetURL)
	if err != nil {
		// A blocking response that exhausts the retries is returned alongside
		// the error, so report it when it is available.
		if response != nil {
			return fmt.Errorf("request %s (last status %s): %w", targetURL, response.Status, err)
		}
		return fmt.Errorf("request %s: %w", targetURL, err)
	}
	if _, err := fmt.Printf("%s\n%s\n", response.Status, response.Body); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}
