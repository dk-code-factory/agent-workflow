package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	HTTP *http.Client
	Dir  string
}

func NewClient(dir string) *Client {
	return &Client{Dir: dir, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		socket, err := socketAddress(dir)
		if err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}
}
func (c *Client) Call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://controller"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var result struct {
			Error string `json:"error"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		return fmt.Errorf("controller: %s", result.Error)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(output)
}
