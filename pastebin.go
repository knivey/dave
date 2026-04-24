package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

const termbinAddr = "termbin.com:9999"

func uploadToTermbin(text string) (string, error) {
	conn, err := net.DialTimeout("tcp", termbinAddr, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("connecting to termbin: %w", err)
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fmt.Fprint(conn, text)
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			conn.Close()
		}
	}()

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		<-done
		return strings.TrimSpace(scanner.Text()), nil
	}
	<-done
	return "", fmt.Errorf("no response from termbin")
}
