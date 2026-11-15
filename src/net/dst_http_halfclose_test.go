// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net_test

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/simulation"
	"time"
)

func TestDSTNetHTTPEarlyResponseWithUnreadBodyNoRST(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency: 10 * time.Millisecond,
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		srvDone := make(chan struct{})
		var srv *http.Server

		simulation.Host("srv", func() {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port

			srv = &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Reject early without draining the large request body; net/http
					// uses *net.TCPConn.CloseWrite so the client reads the 413 response
					// before any close(2) RST tears down the unread body.
					w.Header().Set("Connection", "close")
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = w.Write([]byte("payload too large"))
				}),
			}
			go func() {
				defer close(srvDone)
				_ = srv.Serve(ln)
			}()
		})

		simulation.Host("cli", func() {
			addr := <-addrCh
			tr := &http.Transport{DisableKeepAlives: true}
			client := &http.Client{Transport: tr}

			body := bytes.Repeat([]byte("x"), 256<<10)
			resp, err := client.Post("http://"+addr+"/upload", "application/octet-stream", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("HTTP POST early 413 failed (missing CloseWrite half-close caused RST): %v", err)
			}
			got, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				t.Fatalf("reading 413 response body: %v", rerr)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge || string(got) != "payload too large" {
				t.Fatalf("HTTP response = (%d, %q), want (413, \"payload too large\")", resp.StatusCode, got)
			}
			_ = srv.Close()
			<-srvDone
		})
	})
}
