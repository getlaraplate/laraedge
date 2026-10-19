package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// statusTokenEnv holds the bearer token of a status endpoint bound with
	// status_token.
	statusTokenEnv = "LARAPLATE_EDGE_STATUS_TOKEN"
	// statusTimeout bounds the status request.
	statusTimeout = 10 * time.Second
	// maxStatusBody bounds the status answer read.
	maxStatusBody = 4 << 20
)

// fetchStatus asks the status endpoint at listen for /status, with token as
// bearer when it is not empty, and returns the answer indented. The request
// never goes through a proxy and follows no redirect, so the token reaches
// only listen.
func fetchStatus(listen, token string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()

	url := "http://" + listen + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("--listen %s: %w", listen, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read the answer: %w", url, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GET %s: HTTP 401: the endpoint needs its token in %s", url, statusTokenEnv)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(body), "", "  "); err != nil {
		return nil, fmt.Errorf("GET %s: the answer is not JSON", url)
	}
	out.WriteByte('\n')

	return out.Bytes(), nil
}
