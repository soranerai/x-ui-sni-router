package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inconshreveable/go-vhost"
	_ "github.com/mattn/go-sqlite3"
)

// Обновленная структура JSON конфига
type ConfigRoute struct {
	SNI       string `json:"sni"`
	LocalPort int    `json:"local_port"`
}

type JSONConfig struct {
	Routes                   []ConfigRoute `json:"routes"`
	DefaultFallbackLocalPort int           `json:"default_fallback_local_port"`
	DisableDB                bool          `json:"disable_db"` // Новый флаг
}

// Вспомогательные структуры для x-ui DB
type RealitySettings struct {
	Target string `json:"target"`
}

type StreamSettings struct {
	RealitySettings RealitySettings `json:"realitySettings"`
}

type RouteTable map[string]int

type RouterState struct {
	Routes       RouteTable
	FallbackPort int
}

var (
	routerState atomic.Value
)

func getInbounds(conn *sql.DB, configPath string) (*RouterState, error) {
	newRoutes := make(RouteTable)
	var fallbackPort int
	shouldReadDB := true

	// 1. Сначала читаем JSON конфиг, чтобы узнать настройки
	jsonData, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[INFO] custom JSON config not found: %v. Proceeding with DB only.", err)
	} else {
		var cfg JSONConfig
		if err := json.Unmarshal(jsonData, &cfg); err != nil {
			log.Printf("[ERROR] failed to parse JSON config: %v", err)
		} else {
			// Устанавливаем флаг считывания БД
			if cfg.DisableDB {
				shouldReadDB = false
				log.Println("[DEBUG] Database reading is disabled via config")
			}

			// Заполняем роуты из JSON
			for _, r := range cfg.Routes {
				newRoutes[r.SNI] = r.LocalPort
			}
			fallbackPort = cfg.DefaultFallbackLocalPort
		}
	}

	// 2. Читаем из базы x-ui, только если это не запрещено в конфиге
	if shouldReadDB && conn != nil {
		rows, err := conn.Query("SELECT port, stream_settings FROM inbounds")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var port int
				var settingsRaw string
				if err := rows.Scan(&port, &settingsRaw); err != nil {
					continue
				}

				var settings StreamSettings
				if err := json.Unmarshal([]byte(settingsRaw), &settings); err == nil {
					host, _, err := net.SplitHostPort(settings.RealitySettings.Target)
					if err != nil {
						host = settings.RealitySettings.Target
					}
					// JSON имеет приоритет, поэтому пишем только если ключа еще нет
					if host != "" {
						if _, exists := newRoutes[host]; !exists {
							newRoutes[host] = port
						}
					}
				}
			}
		} else {
			log.Printf("[WARN] could not query sqlite: %v", err)
		}
	}

	return &RouterState{
		Routes:       newRoutes,
		FallbackPort: fallbackPort,
	}, nil
}

func reloadInbounds(conn *sql.DB, configPath string) error {
	state, err := getInbounds(conn, configPath)
	if err != nil {
		return err
	}
	routerState.Store(state)
	log.Printf("[INFO] reload success: %d routes, fallback: %d", len(state.Routes), state.FallbackPort)
	return nil
}

func getPort(sni string) (int, bool) {
	val := routerState.Load()
	if val == nil {
		return 0, false
	}
	state := val.(*RouterState)
	if port, ok := state.Routes[sni]; ok {
		return port, true
	}
	if state.FallbackPort > 0 {
		return state.FallbackPort, true
	}
	return 0, false
}

func proxyConn(src, dst net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(src, dst)
		if cw, ok := src.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	wg.Wait()
}

func handleConn(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	vhostConn, err := vhost.TLS(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})

	sni := vhostConn.Host()
	backendPort, found := getPort(sni)
	if !found {
		log.Printf("[WARN] %s: no route for %s", c.RemoteAddr(), sni)
		return
	}

	backendAddr := fmt.Sprintf("127.0.0.1:%d", backendPort)
	bc, err := net.DialTimeout("tcp", backendAddr, 3*time.Second)
	if err != nil {
		log.Printf("[ERROR] backend %s unreachable: %v", backendAddr, err)
		return
	}
	defer bc.Close()
	proxyConn(vhostConn, bc)
}

func main() {
	dbPath := flag.String("db_path", "/etc/x-ui/x-ui.db", "path to x-ui database")
	configPath := flag.String("config_path", "/usr/local/bin/x-ui-sni-router/config.json", "path to JSON config")
	flag.Parse()

	conn, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Printf("[WARN] database connection failed: %v", err)
	} else {
		defer conn.Close()
	}

	if err := reloadInbounds(conn, *configPath); err != nil {
		log.Fatalf("Init failed: %v", err)
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			reloadInbounds(conn, *configPath)
		}
	}()

	listener, err := net.Listen("tcp", ":443")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("SNI Router is active on :443")

	for {
		conn, err := listener.Accept()
		if err == nil {
			go handleConn(conn)
		}
	}
}
