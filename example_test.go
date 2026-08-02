package proxator_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	proxator "github.com/cpouldev/go-proxator"
)

func ExampleClient_Get() {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxy.Close()

	client, err := proxator.New(proxator.Config{
		SessionFactory: proxator.HTTPFactory{},
		Pools: []proxator.PoolConfig{{
			Name:      "main",
			Endpoints: []string{proxy.URL},
		}},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	resp, err := client.Get(context.Background(), "", "http://target.example/items")
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d %s\n", resp.StatusCode, resp.Body)
	// Output: 200 proxied
}

func ExampleStickyEndpoints() {
	endpoints, err := proxator.StickyEndpoints(proxator.StickyOptions{
		Scheme:   "socks5",
		Username: "user",
		Password: "pass",
		Host:     "gate.example.net",
		Port:     "1080",
		Count:    2,
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(endpoints[0])
	fmt.Println(endpoints[1])
	// Output:
	// socks5://user-1:pass@gate.example.net:1080
	// socks5://user-2:pass@gate.example.net:1080
}

func ExampleClient_multiplePools() {
	euProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("eu"))
	}))
	defer euProxy.Close()
	usProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("us"))
	}))
	defer usProxy.Close()

	client, err := proxator.New(proxator.Config{
		SessionFactory: proxator.HTTPFactory{},
		Pools: []proxator.PoolConfig{
			{Name: "eu", Endpoints: []string{euProxy.URL}},
			{Name: "us", Endpoints: []string{usProxy.URL}},
		},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()
	euResp, err := client.Get(ctx, "eu", "http://target.example/items")
	if err != nil {
		panic(err)
	}
	usResp, err := client.Get(ctx, "us", "http://target.example/items")
	if err != nil {
		panic(err)
	}

	fmt.Println(string(euResp.Body), string(usResp.Body))
	// Output: eu us
}

func ExampleClient_Do() {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "%s %s", r.Method, r.Header.Get("X-Trace"))
	}))
	defer proxy.Close()

	client, err := proxator.New(proxator.Config{
		SessionFactory: proxator.HTTPFactory{},
		Pools: []proxator.PoolConfig{{
			Name:            "main",
			Endpoints:       []string{proxy.URL},
			SessionPoolSize: 1,
		}},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()
	resp, err := client.Do(ctx, "", func(session proxator.Session) (*proxator.Response, error) {
		return session.Do(ctx, proxator.Request{
			Method:  http.MethodPost,
			URL:     "http://target.example/jobs",
			Headers: proxator.OrderedHeaders{{"X-Trace", "example"}},
		})
	})
	if err != nil {
		panic(err)
	}

	fmt.Printf("%d %s\n", resp.StatusCode, resp.Body)
	// Output:
	// 201 POST example
}

func ExampleClient_Stats() {
	client, err := proxator.New(proxator.Config{
		Pools: []proxator.PoolConfig{{
			Name:            "main",
			Endpoints:       []string{"http://proxy.example:8080"},
			SessionPoolSize: 1,
		}},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	stats := client.Stats()["main"]
	fmt.Printf("%s: %d alive, %d total\n", stats.Name, stats.Alive, stats.Total)
	// Output: main: 1 alive, 1 total
}

func ExampleIsTransient() {
	err := errors.New("proxy tunnel failed: connection reset")
	fmt.Println(proxator.IsTransient(err))
	// Output: true
}
