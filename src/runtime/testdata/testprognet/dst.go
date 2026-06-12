// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST in-memory deterministic network test fixtures. They live in testprognet,
// not testprog: importing net links cgo into the program (the resolver), and a
// cgo binary disables the runtime's deadlock detection ("all goroutines are
// asleep"), which testprog's crash tests depend on. Driven by dst_test.go's
// harness via a -tags=dst build of this program.

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing/simulation"
	"time"
)

func init() {
	register("DSTNet", DSTNet)
	register("DSTNetSemantics", DSTNetSemantics)
}

// DSTNet exercises the in-memory deterministic network: inside simulation.Run a
// server Listens/Accepts and a client Dials over net's TCP API, exchange a
// request/response over the simulated connection, with the simulated addresses.
// With the real OS network this could not run deterministically (or at all in a
// sandbox); here it is a function of the seed. Prints a summary line; two runs (a
// and b) check same-seed determinism and that the per-run registry reset (b's
// Listen on the same address must not be "address already in use").
func DSTNet() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	exchange := func() string {
		var out string
		simulation.Run(n, func() {
			const addr = "10.0.0.1:9000"
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				out = "listen err: " + err.Error()
				return
			}
			done := make(chan string, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					done <- "accept err: " + err.Error()
					return
				}
				buf := make([]byte, 16)
				nr, _ := c.Read(buf)
				msg := string(buf[:nr])
				c.Write([]byte("echo:" + msg))
				from := c.RemoteAddr().String()
				c.Close()
				done <- "server saw " + msg + " from " + from
			}()
			client, err := net.Dial("tcp", addr)
			if err != nil {
				out = "dial err: " + err.Error()
				return
			}
			client.Write([]byte("ping"))
			buf := make([]byte, 32)
			nr, _ := client.Read(buf)
			resp := string(buf[:nr])
			srv := <-done
			out = "resp=" + resp + " local=" + client.LocalAddr().String() +
				" remote=" + client.RemoteAddr().String() + " | " + srv
		})
		return out
	}
	a := exchange()
	b := exchange()
	if a != b {
		os.Stdout.WriteString("DIVERGED\n a=" + a + "\n b=" + b + "\n")
		return
	}
	os.Stdout.WriteString(a + "\n")
}

// DSTNetSemantics checks that the DST network shim preserves public net semantics
// for the TCP surface it owns, and rejects protocol shapes it does not model.
// Prints booleans for: canceled/deadline DialContext errors, nil DialContext
// panics, unsupported UDP Listen/Dial errors, deterministic :0 listener ports,
// invalid ports, localhost canonicalization, DNS/service-name rejection, and
// tcp4/tcp6 address-family constraints.
func DSTNetSemantics() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var canceled, deadline, nilPanic, udpRejected, udpDialRejected, zeroPorts, invalidPort, localhost, dnsRejected, serviceRejected, familyRejected, wildcardFamilyRejected, tcp6WildcardRejected, tcp6Local, tcp4Unspecified bool
	simulation.Run(n, func() {
		ln, err := net.Listen("tcp", "127.0.0.1:9100")
		if err == nil {
			defer ln.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:9100")
			if c != nil {
				c.Close()
			}
			canceled = errors.Is(err, context.Canceled)

			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			defer cancel()
			c, err = (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:9100")
			if c != nil {
				c.Close()
			}
			deadline = errors.Is(err, context.DeadlineExceeded)

			func() {
				defer func() { nilPanic = recover() != nil }()
				c, _ := (&net.Dialer{}).DialContext(nil, "tcp", "127.0.0.1:9100")
				if c != nil {
					c.Close()
				}
			}()
		}

		if ln, err := net.Listen("udp", "127.0.0.1:9101"); err != nil {
			udpRejected = true
		} else {
			ln.Close()
		}
		if c, err := net.Dial("udp", "127.0.0.1:9100"); err != nil {
			udpDialRejected = true
		} else {
			c.Close()
		}

		l1, err1 := net.Listen("tcp", "127.0.0.1:0")
		l2, err2 := net.Listen("tcp", "127.0.0.1:0")
		if err1 == nil && err2 == nil {
			p1 := l1.Addr().(*net.TCPAddr).Port
			p2 := l2.Addr().(*net.TCPAddr).Port
			zeroPorts = p1 == 10000 && p2 == 10001
		}
		if l1 != nil {
			l1.Close()
		}
		if l2 != nil {
			l2.Close()
		}

		if ln, err := net.Listen("tcp", "127.0.0.1:999999"); err != nil {
			invalidPort = true
		} else {
			ln.Close()
		}

		lnLocal, err := net.Listen("tcp", "127.0.0.1:9102")
		if err == nil {
			defer lnLocal.Close()
			c, err := net.Dial("tcp", "localhost:9102")
			if err == nil {
				localhost = true
				c.Close()
				if srv, err := lnLocal.Accept(); err == nil {
					srv.Close()
				}
			}
		}

		if c, err := net.Dial("tcp", "definitely-not-localhost.invalid:9102"); err != nil {
			dnsRejected = strings.Contains(err.Error(), "DNS lookup unsupported under deterministic simulation")
		} else {
			c.Close()
		}
		if ln, err := net.Listen("tcp", "127.0.0.1:http"); err != nil {
			serviceRejected = true
		} else {
			ln.Close()
		}

		if c, err := net.Dial("tcp6", "127.0.0.1:9102"); err != nil {
			familyRejected = true
		} else {
			c.Close()
		}
		if ln6, err := net.Listen("tcp6", "[::]:9103"); err == nil {
			if c, err := net.Dial("tcp4", "127.0.0.1:9103"); err != nil {
				wildcardFamilyRejected = true
			} else {
				c.Close()
			}
			ln6.Close()
		}
		if ln, err := net.Listen("tcp6", "0.0.0.0:9104"); err != nil {
			tcp6WildcardRejected = true
		} else {
			ln.Close()
		}
		if ln6, err := net.Listen("tcp6", "[::1]:9105"); err == nil {
			c, err := net.Dial("tcp6", "[::1]:9105")
			if err == nil {
				tcp6Local = c.LocalAddr().(*net.TCPAddr).IP.To4() == nil
				c.Close()
				if srv, err := ln6.Accept(); err == nil {
					srv.Close()
				}
			}
			ln6.Close()
		}
		if ln4, err := net.Listen("tcp4", "127.0.0.1:9106"); err == nil {
			c, err := net.Dial("tcp4", "[::]:9106")
			if err == nil {
				tcp4Unspecified = true
				c.Close()
				if srv, err := ln4.Accept(); err == nil {
					srv.Close()
				}
			}
			ln4.Close()
		}
	})
	simulation.Run(n, func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			zeroPorts = zeroPorts && ln.Addr().(*net.TCPAddr).Port == 10000
			ln.Close()
		} else {
			zeroPorts = false
		}
	})
	os.Stdout.WriteString("canceled=" + strconv.FormatBool(canceled) +
		" deadline=" + strconv.FormatBool(deadline) +
		" nilpanic=" + strconv.FormatBool(nilPanic) +
		" udpreject=" + strconv.FormatBool(udpRejected) +
		" udpdialreject=" + strconv.FormatBool(udpDialRejected) +
		" zeroports=" + strconv.FormatBool(zeroPorts) +
		" invalidport=" + strconv.FormatBool(invalidPort) +
		" localhost=" + strconv.FormatBool(localhost) +
		" dnsreject=" + strconv.FormatBool(dnsRejected) +
		" servicereject=" + strconv.FormatBool(serviceRejected) +
		" familyreject=" + strconv.FormatBool(familyRejected) +
		" wildcardfamilyreject=" + strconv.FormatBool(wildcardFamilyRejected) +
		" tcp6wildcardreject=" + strconv.FormatBool(tcp6WildcardRejected) +
		" tcp6local=" + strconv.FormatBool(tcp6Local) +
		" tcp4unspecified=" + strconv.FormatBool(tcp4Unspecified) + "\n")
}
