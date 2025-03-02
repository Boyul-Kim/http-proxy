package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
)

//TODO:  gzip, content caching

func runHttpProxy() {
	listener, err := net.Listen("tcp", "127.0.0.1:8000")
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
	defer listener.Close()

	log.Println("Proxy server listening on port 8000")

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting connection: %v", err)
			continue
		}
		log.Printf("Accepted new connection")

		go handleProxyConnection(clientConn)
	}
}

func handleProxyConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Create persistent connection to the upstream
	upstreamConn, err := net.Dial("tcp", "127.0.0.1:9000")
	if err != nil {
		log.Printf("Error connecting to upstream: %v", err)
		return
	}
	// Close upstreamConn when the client or proxy is done
	defer upstreamConn.Close()

	buffer := make([]byte, 4096)
	for {
		// Read one request from the client
		n, err := clientConn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from client: %v", err)
			}
			return
		}
		if n == 0 {
			// client closed the connection
			return
		}

		request := string(buffer[:n])
		version, connectionHeader := parseHTTPHeaders(request)

		// Forward the request to our persistent upstream connection
		_, err = upstreamConn.Write(buffer[:n])
		if err != nil {
			log.Printf("Error writing to upstream: %v", err)
			return
		}

		// For true streaming, we can spin up two goroutines to copy data
		// in both directions. But that means we need to know
		// when one request/response ends and the next begins.
		// This code just demonstrates a naive approach: it does
		// a bidirectional copy for this 'request' cycle.

		done1 := make(chan struct{})
		done2 := make(chan struct{})

		// Upstream -> Client
		go func() {
			proxyData(upstreamConn, clientConn, "UPSTREAM -> CLIENT")
			close(done1)
		}()

		// Client -> Upstream
		go func() {
			proxyData(clientConn, upstreamConn, "CLIENT -> UPSTREAM")
			close(done2)
		}()

		// Wait until both directions have finished this request cycle
		<-done1
		<-done2

		// DO NOT close upstreamConn here — we want to reuse it
		// Instead, check if the client wants to keep the connection open
		if shouldKeepAlive(version, connectionHeader) {
			log.Println("Connection kept alive for next request.")
			continue
		} else {
			log.Println("Connection closing (not keep-alive).")
			break
		}
	}
}

// proxyData just copies data from srcConn to destConn until EOF or error
func proxyData(srcConn, destConn net.Conn, proxyDir string) {

	//simple header modification when sending back to client
	//TODO need to make this more dynamic
	if proxyDir == "UPSTREAM -> CLIENT" {
		proxyResponseWithHeaderInjection(srcConn, destConn)
		return
	}

	buf := make([]byte, 4096)
	for {
		n, err := srcConn.Read(buf)
		if n > 0 {
			_, writeErr := destConn.Write(buf[:n])
			if writeErr != nil {
				log.Printf("Error writing data: %v", writeErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading data: %v", err)
			}
			return
		}
	}
}

// parseHTTPHeaders extracts the HTTP version and Connection header
func parseHTTPHeaders(request string) (version, connection string) {
	if idx := strings.Index(request, "HTTP/"); idx != -1 && len(request) > idx+8 {
		version = request[idx+5 : idx+8]
	}
	lines := bytes.Split([]byte(request), []byte("\r\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("Connection:")) {
			connection = strings.TrimSpace(string(line[len("Connection: "):]))
			break
		}
	}
	return version, connection
}

// Checks if the request indicates a keep-alive
func shouldKeepAlive(version, connHeader string) bool {
	ver := strings.TrimSpace(version)
	ch := strings.ToLower(strings.TrimSpace(connHeader))

	// For HTTP/1.0: keep-alive must be explicit
	if ver == "1.0" && ch == "keep-alive" {
		return true
	}
	// For HTTP/1.1: close must be explicit, otherwise keep-alive
	if ver == "1.1" && ch != "close" {
		return true
	}
	return false
}

func proxyResponseWithHeaderInjection(upstream net.Conn, client net.Conn) error {
	// Need to avoid modifying the body and only modify the headers
	var headerBuf bytes.Buffer
	// We read one byte at a time and stop once it reaches "\r\n\r\n" in the headerBuf.
	tmp := make([]byte, 1)
	for {
		n, err := upstream.Read(tmp)
		if err != nil {
			return fmt.Errorf("error reading response header: %w", err)
		}
		if n > 0 {
			headerBuf.Write(tmp[:n])
		}
		// Check if we’ve reached the end of headers
		if bytes.Contains(headerBuf.Bytes(), []byte("\r\n\r\n")) {
			break
		}
	}

	rawHeaders := headerBuf.Bytes()
	parts := bytes.SplitN(rawHeaders, []byte("\r\n\r\n"), 2)
	if len(parts) < 2 {
		return fmt.Errorf("malformed HTTP response headers")
	}
	headerSection := parts[0]
	leftover := parts[1]

	lines := bytes.Split(headerSection, []byte("\r\n"))
	lines = append(lines, []byte("Foo: Bar"))

	// Reassemble the headers
	modifiedHeader := bytes.Join(lines, []byte("\r\n"))
	modifiedHeader = append(modifiedHeader, []byte("\r\n\r\n")...)

	// Write modified headers to the client
	if _, err := client.Write(modifiedHeader); err != nil {
		return fmt.Errorf("error writing modified headers to client: %w", err)
	}

	// Write any leftover (the beginning of the body) to the client
	if len(leftover) > 0 {
		if _, err := client.Write(leftover); err != nil {
			return fmt.Errorf("error writing leftover body data to client: %w", err)
		}
	}

	// NOTE: We immediately forward any leftover body bytes we already read (leftoverBody) to the client
	// That chunk might be large or small, or even zero bytes if we happened to read exactly up to the end of the headers and no further
	// This is because even though we read up unitl \r\n\r\n, the reading process can "overshoot" and it may read some bytes that are actually the body because they arrived in the same TCP packet
	// So, we send the leftovers first and then io.Copy the rest or just copyData

	// Copy the remainder of the body as-is, including chunked or not
	// We do NOT modify the body any further
	err := copyData(client, upstream)
	if err != nil {
		// handle or log error
	}

	// copyData is essentially io.Copy - just doing it myself
	// _, err := io.Copy(client, upstream)
	return err
}

func copyData(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write error: %w", writeErr)
			}
			if written < n {
				offset := written
				for offset < n {
					w, werr := dst.Write(buf[offset:n])
					if werr != nil {
						return fmt.Errorf("write error: %w", werr)
					}
					offset += w
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				// End of data
				return nil
			}
			return fmt.Errorf("read error: %w", readErr)
		}
	}
}
