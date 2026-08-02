package proxator

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestAzureTLSSession_SendsTrafficThroughSOCKS5(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("through socks5"))
	}))
	t.Cleanup(target.Close)

	proxyURL, proxyDone := startSOCKS5Proxy(t, "user", "pass")
	session, err := defaultSessionFactory().New(proxyURL)
	if err != nil {
		t.Fatalf("creating SOCKS5 session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := session.Do(ctx, Request{Method: http.MethodGet, URL: target.URL})
	session.Close()
	if err != nil {
		t.Fatalf("sending request through SOCKS5: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted || string(resp.Body) != "through socks5" {
		t.Fatalf("response = %+v, want 202 with SOCKS5 response body", resp)
	}

	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("SOCKS5 proxy: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOCKS5 proxy did not finish")
	}
}

func startSOCKS5Proxy(t *testing.T, username, password string) (string, <-chan error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting SOCKS5 proxy: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		done <- serveSOCKS5Connection(conn, username, password)
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("splitting SOCKS5 address: %v", err)
	}
	endpoints, err := StickyEndpoints(StickyOptions{
		Scheme:         "socks5",
		Username:       username,
		Password:       password,
		Host:           host,
		Port:           port,
		Count:          1,
		UsernameFormat: "%s",
	})
	if err != nil {
		t.Fatalf("building SOCKS5 endpoint: %v", err)
	}
	return endpoints[0], done
}

func serveSOCKS5Connection(client net.Conn, username, password string) error {
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(client, greeting); err != nil {
		return fmt.Errorf("reading greeting: %w", err)
	}
	if greeting[0] != 5 {
		return fmt.Errorf("unexpected SOCKS version %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return fmt.Errorf("reading authentication methods: %w", err)
	}
	method := byte(0)
	if username != "" || password != "" {
		method = 2
	}
	if !hasSOCKS5Method(methods, method) {
		return fmt.Errorf("client did not offer authentication method %d", method)
	}
	if _, err := client.Write([]byte{5, method}); err != nil {
		return fmt.Errorf("writing greeting response: %w", err)
	}
	if method == 2 {
		if err := authenticateSOCKS5(client, username, password); err != nil {
			return err
		}
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		return fmt.Errorf("reading connect request: %w", err)
	}
	if header[0] != 5 || header[1] != 1 {
		return fmt.Errorf("unsupported SOCKS request version=%d command=%d", header[0], header[1])
	}
	host, err := readSOCKS5Host(client, header[3])
	if err != nil {
		return err
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(client, portBytes); err != nil {
		return fmt.Errorf("reading target port: %w", err)
	}
	targetAddress := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))

	target, err := net.DialTimeout("tcp", targetAddress, 5*time.Second)
	if err != nil {
		_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return fmt.Errorf("dialing target %s: %w", targetAddress, err)
	}
	defer target.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return fmt.Errorf("writing connect response: %w", err)
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		return err
	}

	clientToTarget := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(target, client)
		clientToTarget <- copyErr
	}()
	_, targetToClientErr := io.Copy(client, target)
	clientToTargetErr := <-clientToTarget
	if targetToClientErr != nil {
		return fmt.Errorf("copying target response: %w", targetToClientErr)
	}
	if clientToTargetErr != nil {
		return fmt.Errorf("copying client request: %w", clientToTargetErr)
	}
	return nil
}

func hasSOCKS5Method(methods []byte, want byte) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}

func authenticateSOCKS5(conn net.Conn, wantUsername, wantPassword string) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("reading username header: %w", err)
	}
	if header[0] != 1 {
		return fmt.Errorf("unexpected username authentication version %d", header[0])
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, username); err != nil {
		return fmt.Errorf("reading username: %w", err)
	}
	passwordLength := []byte{0}
	if _, err := io.ReadFull(conn, passwordLength); err != nil {
		return fmt.Errorf("reading password length: %w", err)
	}
	password := make([]byte, int(passwordLength[0]))
	if _, err := io.ReadFull(conn, password); err != nil {
		return fmt.Errorf("reading password: %w", err)
	}

	status := byte(0)
	if string(username) != wantUsername || string(password) != wantPassword {
		status = 1
	}
	if _, err := conn.Write([]byte{1, status}); err != nil {
		return fmt.Errorf("writing authentication response: %w", err)
	}
	if status != 0 {
		return fmt.Errorf("unexpected SOCKS5 credentials %q:%q", username, password)
	}
	return nil
}

func readSOCKS5Host(conn net.Conn, addressType byte) (string, error) {
	var size int
	switch addressType {
	case 1:
		size = net.IPv4len
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", fmt.Errorf("reading target hostname length: %w", err)
		}
		size = int(length[0])
	case 4:
		size = net.IPv6len
	default:
		return "", fmt.Errorf("unsupported SOCKS address type %d", addressType)
	}

	address := make([]byte, size)
	if _, err := io.ReadFull(conn, address); err != nil {
		return "", fmt.Errorf("reading target address: %w", err)
	}
	if addressType == 3 {
		return string(address), nil
	}
	return net.IP(address).String(), nil
}
