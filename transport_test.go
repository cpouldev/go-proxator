package proxator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/Noooste/azuretls-client"
)

type recordingSession struct {
	mu       sync.Mutex
	requests []Request
	response *Response
	err      error
	closed   bool
}

func (s *recordingSession) Do(_ context.Context, req Request) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return s.response, s.err
}

func (s *recordingSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

type recordingFactory struct {
	mu       sync.Mutex
	urls     []string
	sessions []*recordingSession
	response *Response
	err      error
}

func (f *recordingFactory) New(proxyURL string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	session := &recordingSession{response: f.response}
	f.urls = append(f.urls, proxyURL)
	f.sessions = append(f.sessions, session)
	return session, nil
}

func TestClient_Get_UsesConfiguredSessionFactory(t *testing.T) {
	t.Parallel()

	factory := &recordingFactory{
		response: &Response{
			StatusCode: http.StatusOK,
			Body:       []byte("ok"),
		},
	}
	client, err := New(Config{
		SessionFactory: factory,
		Pools: []PoolConfig{{
			Name:            "main",
			Endpoints:       []string{testProxyURL},
			SessionPoolSize: 1,
		}},
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	t.Cleanup(client.Close)

	resp, err := client.Get(context.Background(), "main", "https://example.com/items")
	if err != nil {
		t.Fatalf("Get returned an error: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if len(factory.urls) != 1 || factory.urls[0] != testProxyURL {
		t.Fatalf("factory URLs = %q, want [%q]", factory.urls, testProxyURL)
	}
	requests := factory.sessions[0].requests
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if got := requests[0]; got.Method != http.MethodGet || got.URL != "https://example.com/items" {
		t.Fatalf("request = %+v, want GET request for target URL", got)
	}
}

func TestHTTPFactory_SendsThroughProxy(t *testing.T) {
	t.Parallel()

	requests := make(chan *http.Request, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if string(body) != "payload" {
			t.Errorf("body = %q, want payload", body)
		}
		w.Header().Set("X-Proxy-Test", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	t.Cleanup(proxy.Close)

	session, err := (HTTPFactory{}).New(proxy.URL)
	if err != nil {
		t.Fatalf("creating HTTP session: %v", err)
	}
	t.Cleanup(session.Close)

	resp, err := session.Do(context.Background(), Request{
		Method: http.MethodPost,
		URL:    "http://target.example/items",
		Body:   "payload",
		Headers: OrderedHeaders{
			{"X-Test", "one", "two"},
		},
	})
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "created" {
		t.Fatalf("response = %+v, want 201 with created body", resp)
	}
	if resp.Header.Get("X-Proxy-Test") != "yes" {
		t.Fatalf("response headers = %v, want X-Proxy-Test", resp.Header)
	}

	req := <-requests
	if req.URL.String() != "http://target.example/items" {
		t.Fatalf("target URL = %q, want absolute proxy URL", req.URL)
	}
	if got := req.Header.Values("X-Test"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("X-Test headers = %q, want [one two]", got)
	}
}

func TestHTTPFactory_EncodesJSONBody(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		body        string
		contentType string
	}
	captured := make(chan capturedRequest, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		captured <- capturedRequest{
			body:        string(body),
			contentType: r.Header.Get("Content-Type"),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)

	session, err := (HTTPFactory{}).New(proxy.URL)
	if err != nil {
		t.Fatalf("creating HTTP session: %v", err)
	}
	t.Cleanup(session.Close)

	_, err = session.Do(context.Background(), Request{
		Method: http.MethodPost,
		URL:    "http://target.example/items",
		Body:   map[string]string{"name": "proxator"},
	})
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}

	got := <-captured
	if got.body != `{"name":"proxator"}` {
		t.Fatalf("body = %q, want JSON object", got.body)
	}
	if got.contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got.contentType)
	}
}

func TestClient_GetWithHeaders_MapsRequest(t *testing.T) {
	t.Parallel()

	client, factory := newRecordingClient(t, &Response{StatusCode: http.StatusOK})
	headers := OrderedHeaders{{"Accept", "application/json"}}
	if _, err := client.GetWithHeaders(
		context.Background(), "main", "https://example.com/items", headers,
	); err != nil {
		t.Fatalf("GetWithHeaders returned an error: %v", err)
	}

	got := factory.sessions[0].requests[0]
	want := Request{
		Method:  http.MethodGet,
		URL:     "https://example.com/items",
		Headers: headers,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
}

func TestClient_Post_MapsRequest(t *testing.T) {
	t.Parallel()

	client, factory := newRecordingClient(t, &Response{StatusCode: http.StatusOK})
	body := map[string]string{"name": "proxator"}
	if _, err := client.Post(
		context.Background(), "main", "https://example.com/items", body,
	); err != nil {
		t.Fatalf("Post returned an error: %v", err)
	}

	got := factory.sessions[0].requests[0]
	if got.Method != http.MethodPost || got.URL != "https://example.com/items" {
		t.Fatalf("request = %+v, want POST request for target URL", got)
	}
	if !reflect.DeepEqual(got.Body, body) {
		t.Fatalf("body = %#v, want %#v", got.Body, body)
	}
}

func TestClient_PostWithHeaders_MapsRequest(t *testing.T) {
	t.Parallel()

	client, factory := newRecordingClient(t, &Response{StatusCode: http.StatusOK})
	headers := OrderedHeaders{{"Content-Type", "application/json"}}
	if _, err := client.PostWithHeaders(
		context.Background(), "main", "https://example.com/items", "{}", headers,
	); err != nil {
		t.Fatalf("PostWithHeaders returned an error: %v", err)
	}

	got := factory.sessions[0].requests[0]
	want := Request{
		Method:  http.MethodPost,
		URL:     "https://example.com/items",
		Body:    "{}",
		Headers: headers,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
}

func TestClient_Ping_UsesTransport(t *testing.T) {
	t.Parallel()

	client, factory := newRecordingClient(t, &Response{StatusCode: http.StatusNoContent})
	if err := client.Ping(context.Background(), "main", "https://example.com/health"); err != nil {
		t.Fatalf("Ping returned an error: %v", err)
	}

	got := factory.sessions[0].requests[0]
	if got.Method != http.MethodGet || got.URL != "https://example.com/health" {
		t.Fatalf("request = %+v, want GET request for probe URL", got)
	}
}

func TestAzureTLSSessionFactory_AcceptsSOCKS5Proxy(t *testing.T) {
	t.Parallel()

	session, err := defaultSessionFactory().New("socks5://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("creating SOCKS5 session: %v", err)
	}
	session.Close()
}

func TestAzureTLSSessionFactory_VerifiesCertificates(t *testing.T) {
	t.Parallel()

	session, err := defaultSessionFactory().New("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("creating default session: %v", err)
	}
	defer session.Close()

	azureSession := session.(*azureTLSSession)
	if azureSession.session.InsecureSkipVerify {
		t.Fatal("default azuretls session disables certificate verification")
	}
}

func TestAzureTLSSession_NormalizesResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "value" {
			t.Errorf("X-Test = %q, want value", r.Header.Get("X-Test"))
		}
		w.Header().Set("X-Response", "yes")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	}))
	t.Cleanup(server.Close)

	session := &azureTLSSession{session: azuretls.NewSession()}
	t.Cleanup(session.Close)
	resp, err := session.Do(context.Background(), Request{
		Method:  http.MethodGet,
		URL:     server.URL,
		Headers: OrderedHeaders{{"X-Test", "value"}},
	})
	if err != nil {
		t.Fatalf("Do returned an error: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted || string(resp.Body) != "accepted" {
		t.Fatalf("response = %+v, want normalized 202 response", resp)
	}
	if resp.Header.Get("X-Response") != "yes" {
		t.Fatalf("response headers = %v, want X-Response", resp.Header)
	}
}

func newRecordingClient(t *testing.T, response *Response) (*Client, *recordingFactory) {
	t.Helper()

	factory := &recordingFactory{response: response}
	client, err := New(Config{
		SessionFactory: factory,
		Pools: []PoolConfig{{
			Name:            "main",
			Endpoints:       []string{testProxyURL},
			SessionPoolSize: 1,
		}},
		Retry:  fastRetry(),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	t.Cleanup(client.Close)
	return client, factory
}
