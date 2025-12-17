package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inconshreveable/go-vhost"
	_ "github.com/mattn/go-sqlite3"
)

type RealitySettins struct {
	Target string `json:"target"`
}

type StreamSettings struct {
	RealitySettings RealitySettins `json:"realitySettings"`
}
type RouteTable map[string]int

var (
	routes atomic.Value
)

func getInbounds(conn *sql.DB) (RouteTable, error) {
	rows, err := conn.Query("SELECT port, stream_settings FROM inbounds")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	newRoutes := make(RouteTable)

	for rows.Next() {
		var port int
		var settingsRaw string
		if err := rows.Scan(&port, &settingsRaw); err != nil {
			return nil, err
		}

		var settings StreamSettings
		if err := json.Unmarshal([]byte(settingsRaw), &settings); err != nil {
			log.Printf("[WARN] skipping inbound with bad json: %v", err)
			continue
		}

		// parse host & port from json
		host, _, err := net.SplitHostPort(settings.RealitySettings.Target)
		if err != nil {
			log.Printf("[WARN] error splitting host and port; trying to use the field as the host")
			host = settings.RealitySettings.Target
		}

		if host != "" {
			newRoutes[host] = port
		}
	}
	return newRoutes, nil
}

func reloadInbounds(conn *sql.DB) error {
	newRoutes, err := getInbounds(conn)
	if err != nil {
		return err
	}

	routes.Store(newRoutes)

	log.Printf("[INFO] route table updated: %d entries", len(newRoutes))
	return nil
}

func getPort(sni string) (int, bool) {
	val := routes.Load()
	if val == nil {
		return 0, false
	}
	rt := val.(RouteTable)
	port, ok := rt[sni]
	return port, ok
}

type writeCloser interface {
	CloseWrite() error
}

func proxyConn(src, dst net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(writeCloser); ok {
			cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(src, dst)
		if cw, ok := src.(writeCloser); ok {
			cw.CloseWrite()
		}
	}()

	wg.Wait()
}

func handleConn(c net.Conn) {
	defer c.Close()

	// handshake timeout
	c.SetReadDeadline(time.Now().Add(5 * time.Second))

	vhostConn, err := vhost.TLS(c)
	if err != nil {
		log.Printf("[DEBUG] not a TLS connection from %s: %v", c.RemoteAddr(), err)
		return
	}

	// resets deadline after sni parsed
	c.SetDeadline(time.Time{})

	sni := vhostConn.Host()
	if sni == "" {
		log.Printf("[WARN] %s: no SNI found", c.RemoteAddr())
		return
	}

	backendPort, found := getPort(sni)
	if !found {
		log.Printf("[WARN] %s: no route for SNI %s", c.RemoteAddr(), sni)
		return
	}

	// init connection to local vless-reality server
	backendAddr := fmt.Sprintf("127.0.0.1:%d", backendPort)
	bc, err := net.DialTimeout("tcp", backendAddr, 3*time.Second)
	if err != nil {
		log.Printf("[ERROR] failed to connect to backend %s: %v", backendAddr, err)
		return
	}
	defer bc.Close()

	log.Printf("[INFO] %s -> SNI:%s -> %s", c.RemoteAddr(), sni, backendAddr)

	proxyConn(vhostConn, bc)
}

func main() {
	dbPath := flag.String("db_path", "/etc/x-ui/x-ui.db", "path to x-ui database")
	flag.Parse()

	conn, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// read & parse x-ui inbounds to memory cached table
	if err := reloadInbounds(conn); err != nil {
		log.Fatalf("Initial DB load failed: %v", err)
	}

	// ticker for auto-renew cache
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := reloadInbounds(conn); err != nil {
				log.Printf("[ERROR] reload failed: %v", err)
			}
		}
	}()

	listener, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Server started on :443 (SNI Proxy)")

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			log.Printf("[ERROR] accept error: %v", err)
			continue
		}

		// handle incoming connection
		go handleConn(conn)
	}
}
